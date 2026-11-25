package filesync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"hash"
	"io"
	gofs "io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/moby/buildkit/util/bklog"
	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil"
	fstypes "github.com/tonistiigi/fsutil/types"
	"google.golang.org/grpc"
)

// Content diff (okteto) compares the files of a local source by content.
//
// fsutil considers that the receiver already has a file when its size and
// modification time match the ones of the saved copy (plus mode, owner and
// link target). Two checkouts of the same tree never share modification
// times, so every file is sent again when a saved copy is reused from a
// different directory.
//
// With content diff the receiver asks the sender, through the request
// metadata, to add the sha256 of the content of every regular file to the
// stat that it sends. When a stat differs from the saved file only in its
// modification time, the receiver hashes its own copy and, if both digests
// match, gives fsutil a stat with the saved modification time, so fsutil does
// not request the content. Once the transfer is done, the modification time
// sent by the client is applied to those files. The saved copy ends in the
// same state as if the content had been transferred.
//
// The modification time of a saved copy that another source synced says
// nothing about the files of this one, also when it is the same. So a file
// with the size and the modification time of the saved one is hashed too. If
// the digests differ, the receiver moves the modification time of the stat by
// a nanosecond, so fsutil requests the content, and applies the real one once
// the transfer is done. This only happens for the files that come with a
// digest: without one the rule is the one of fsutil.
//
// The digest travels as an extra entry of Stat.Xattrs that is removed before
// fsutil sees the stat, so it never reaches the disk or the content hash of
// the cache. A sender that does not know the option ignores the request, and
// a receiver that does not send the request never gets digests.
//
// The receiver only compares against the digest of the bytes that it has on
// disk at that moment, so a sender can't make it keep content that the sender
// does not have. A sender can learn whether the saved copy holds a file with a
// given digest at a given path, which is inherent to skipping content that the
// receiver already has.
//
// A file is only kept when a transfer would leave the same file: same size,
// mode and content, and the same extended attributes. All the attributes that
// fsutil transfers are compared, also the ones that are not part of the
// content hash of the cache, like ACLs. The SELinux label is the exception,
// see isComparedXattr. Owner and link target are compared by fsutil as usual.
//
// The comparison is against the saved file, so the content hash that the
// cache has for a kept file stays the one of its last transfer. It was
// computed from the same content, mode and owner. It can differ from the one
// that a transfer by this client would record in one case: an extended
// attribute that an earlier client sent and the receiver could not store.
// fsutil ignores that error and the hash is computed from the stat as sent,
// so such an attribute is in the hash although it is not on disk.
//
// The paths of the stats come from the client and the saved copy has whatever
// earlier clients sent. The receiver checks the order of the paths with the
// validator of fsutil before it looks at the saved copy, so every path comes
// once and after its parent directories. It only goes into real directories
// (never through a symlink, and it never opens a FIFO or a device) and every
// directory is an os.Root opened from its parent.
//
// Cost. sha256 is the digest because the receiver keeps a file that another
// client sent on the word of this digest, which needs a hash without
// practical collisions, and because the Go implementation uses the SHA
// instructions of amd64 and arm64. Hashing is a small part of the work: most
// of the time goes to opening and reading many small files. So the sender
// hashes several files at once, and the receiver opens every directory of the
// saved copy once instead of resolving the whole path of each file.

const (
	// keyContentDigest is the request metadata key that asks the sender to
	// add content digests to the stats. Its value names the digest and how it
	// is sent: a change in any of them needs another value, so that a sender
	// and a receiver that don't agree fall back to the usual transfer. The
	// plain value "sha256" is not used: test builds of this feature sent the
	// digest under other keys with it.
	keyContentDigest    = "okteto-content-digest"
	contentDigestSHA256 = "sha256-1"

	// contentDigestXattr is the key of the Stat.Xattrs entry that carries the
	// digest. It is sent with every regular file, so it is short. It starts
	// with a NUL byte so that no file can have an extended attribute with
	// this name, on any platform: the system calls take the names as C
	// strings. A key that is only invalid on Linux is not enough, because
	// macOS takes names without a namespace.
	contentDigestXattr = "\x00sha256"

	contentDigestBufferSize = 256 << 10

	// The sender hashes the files of a batch in parallel and then sends their
	// stats in order. A batch ends after contentDigestBatchEntries paths or
	// when the files that wait to be hashed add up to contentDigestBatchBytes.
	contentDigestBatchEntries = 512
	contentDigestBatchBytes   = int64(64 << 20)
	contentDigestMaxWorkers   = 8

	// savedDirsMaxDepth is the number of nested directories of the saved copy
	// that the receiver keeps open. Files that are deeper are not compared.
	savedDirsMaxDepth = 128

	// selinuxXattr is the extended attribute with the SELinux label of a
	// file, see isComparedXattr.
	selinuxXattr = "security.selinux"
)

// wantsContentDigests reports whether the receiver of a transfer asked the
// sender for the content digests, in the way that this sender sends them.
func wantsContentDigests(protocol string, opts map[string][]string) bool {
	return protocol == "diffcopy" && slices.Contains(opts[keyContentDigest], contentDigestSHA256)
}

// useContentDiff reports whether a transfer compares the files by content:
// content diff replaces the modification time check of the metadata differ,
// and only the diffcopy protocol carries the stats that it needs.
func useContentDiff(opt FSSendRequestOpt, protocol string) bool {
	return opt.ContentDiff && protocol == "diffcopy" && opt.Differ == fsutil.DiffMetadata && !opt.MetadataOnly
}

// isContentDiffCandidate reports whether a stat can be compared by content.
// Hardlinks are excluded: fsutil links them to a file that it already has.
func isContentDiffCandidate(st *fstypes.Stat) bool {
	return os.FileMode(st.Mode).IsRegular() && st.Linkname == ""
}

type contentDigester struct {
	h   hash.Hash
	buf []byte
}

var contentDigesters = sync.Pool{
	New: func() any {
		return &contentDigester{
			h:   sha256.New(),
			buf: make([]byte, contentDigestBufferSize),
		}
	},
}

// contentDigest returns the sha256 of everything that can be read from r.
func contentDigest(ctx context.Context, r io.Reader) (dgst [sha256.Size]byte, err error) {
	d := contentDigesters.Get().(*contentDigester)
	defer contentDigesters.Put(d)
	d.h.Reset()

	for {
		if err := context.Cause(ctx); err != nil {
			return dgst, err
		}
		n, err := r.Read(d.buf)
		if n > 0 {
			d.h.Write(d.buf[:n])
		}
		if errors.Is(err, io.EOF) {
			d.h.Sum(dgst[:0])
			return dgst, nil
		}
		if err != nil {
			return dgst, errors.WithStack(err)
		}
	}
}

// withContentDigests returns an FS that adds the sha256 of the content to the
// stat of every regular file. Files that can't be read are sent without a
// digest, the receiver then compares their modification time as usual.
func withContentDigests(fs fsutil.FS) fsutil.FS {
	return &contentDigestFS{
		FS:           fs,
		workers:      min(runtime.GOMAXPROCS(0), contentDigestMaxWorkers),
		batchEntries: contentDigestBatchEntries,
		batchBytes:   contentDigestBatchBytes,
	}
}

type contentDigestFS struct {
	fsutil.FS
	workers      int
	batchEntries int
	batchBytes   int64
}

// Walk calls fn with the same paths, in the same order and with the same
// meaning of its result as the walk of the wrapped FS. The only difference is
// that fn is called in batches, after the files of the batch were hashed.
func (fs *contentDigestFS) Walk(ctx context.Context, target string, fn gofs.WalkDirFunc) error {
	w := &contentDigestWalk{ctx: ctx, fs: fs, fn: fn}
	if err := fs.FS.Walk(ctx, target, w.visit); err != nil {
		return err
	}
	return w.flush()
}

type contentDigestEntry struct {
	path string
	stat *fstypes.Stat
}

// contentDigestWalk is the state of one walk of a contentDigestFS.
type contentDigestWalk struct {
	ctx context.Context
	fs  *contentDigestFS
	fn  gofs.WalkDirFunc

	// batch has the paths that were not given to fn yet, in walk order.
	batch        []contentDigestEntry
	candidates   []int
	pendingBytes int64

	// skip is the prefix of the paths that fn asked to skip with
	// filepath.SkipDir. The wrapped walk may already be inside that
	// directory, so its paths are dropped here. A walk goes through every
	// path under a directory before it leaves it, so one prefix is enough.
	skip string
	// done is set when fn returned filepath.SkipAll.
	done bool
}

// skipped reports whether fn asked to skip p: a path under the directory that
// is skipped, or that directory again (filepath.WalkDir calls back with the
// directory when it can't read it).
func (w *contentDigestWalk) skipped(p string) bool {
	if w.skip == "" {
		return false
	}
	if strings.HasPrefix(p, w.skip) || (len(p)+1 == len(w.skip) && strings.HasPrefix(w.skip, p)) {
		return true
	}
	w.skip = ""
	return false
}

func (w *contentDigestWalk) visit(p string, entry gofs.DirEntry, walkErr error) error {
	if w.done {
		return filepath.SkipAll
	}
	if w.skipped(p) {
		return filepath.SkipDir
	}

	if walkErr != nil || entry == nil {
		// errors keep their place in the order
		if err := w.flush(); err != nil {
			return err
		}
		if w.done {
			return filepath.SkipAll
		}
		if w.skipped(p) {
			return filepath.SkipDir
		}
		return w.fn(p, entry, walkErr)
	}

	fi, err := entry.Info()
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*fstypes.Stat)
	if !ok {
		return errors.WithStack(&os.PathError{Path: p, Err: syscall.EBADMSG, Op: "fileinfo without stat info"})
	}

	if isContentDiffCandidate(st) {
		w.candidates = append(w.candidates, len(w.batch))
		w.pendingBytes += st.Size
	}
	w.batch = append(w.batch, contentDigestEntry{path: p, stat: st})
	if len(w.batch) < w.fs.batchEntries && w.pendingBytes < w.fs.batchBytes {
		return nil
	}

	return w.flush()
}

// flush hashes the files of the batch and gives its paths to fn in order.
func (w *contentDigestWalk) flush() error {
	w.addDigests()

	defer func() {
		clear(w.batch)
		w.batch = w.batch[:0]
		w.candidates = w.candidates[:0]
		w.pendingBytes = 0
	}()

	for _, e := range w.batch {
		if w.done {
			return nil
		}
		if w.skipped(e.path) {
			continue
		}

		// SkipDir and SkipAll are compared as filepath.WalkDir does
		err := w.fn(e.path, &fsutil.DirEntryInfo{Stat: e.stat}, nil)
		if err == nil {
			continue
		}
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			// the walks of fsutil skip a path that is gone
			err = filepath.SkipDir
		}
		if err == filepath.SkipAll { //nolint:errorlint
			w.done = true
			continue
		}
		if err != filepath.SkipDir { //nolint:errorlint
			return err
		}
		dir := e.path
		if !e.stat.IsDir() {
			// the rest of the directory of the file
			dir = filepath.Dir(e.path)
		}
		if dir == "." {
			w.done = true
		} else {
			w.skip = dir + string(filepath.Separator)
		}
	}
	return nil
}

// addDigests adds the digest to the stat of every regular file of the batch.
func (w *contentDigestWalk) addDigests() {
	workers := min(w.fs.workers, len(w.candidates))
	if workers <= 1 {
		for _, i := range w.candidates {
			w.addDigest(w.batch[i])
		}
		return
	}

	var (
		next atomic.Int64
		wg   sync.WaitGroup
	)
	for range workers {
		wg.Go(func() {
			for {
				n := int(next.Add(1)) - 1
				if n >= len(w.candidates) {
					return
				}
				w.addDigest(w.batch[w.candidates[n]])
			}
		})
	}
	wg.Wait()
}

func (w *contentDigestWalk) addDigest(e contentDigestEntry) {
	f, err := w.fs.Open(e.path)
	if err != nil {
		return
	}
	dgst, err := contentDigest(w.ctx, f)
	f.Close()
	if err != nil {
		return
	}
	xattrs := make(map[string][]byte, len(e.stat.Xattrs)+1)
	maps.Copy(xattrs, e.stat.Xattrs)
	xattrs[contentDigestXattr] = dgst[:]
	e.stat.Xattrs = xattrs
}

// savedDirs resolves paths of the saved copy and keeps the directories of the
// last one open. fsutil sends the paths of a directory together, so every
// directory is opened once, where an os.Root of the saved copy would open all
// the parents of each file again.
//
// Every directory is an os.Root opened from its parent, and only if it is a
// real directory: a lookup never leaves the saved copy, follows a symlink or
// opens a file that is not a directory, whatever the saved copy contains.
type savedDirs struct {
	root  *os.Root
	names []string
	// roots[i] is the directory names[0]/.../names[i], nil when it can't be
	// opened.
	roots []*os.Root
}

func newSavedDirs(dest string) (*savedDirs, error) {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to open %s", dest)
	}
	return &savedDirs{root: root}, nil
}

// parent returns the directory of name and the name of the file in it. The
// directory is nil when the saved copy does not have it as a chain of real
// directories, when name is not a clean path inside the saved copy and when
// it is deeper than savedDirsMaxDepth.
func (d *savedDirs) parent(name string) (*os.Root, string) {
	if !filepath.IsLocal(name) || filepath.Clean(name) != name {
		return nil, ""
	}
	dir, base := filepath.Split(name)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	if dir != "" && strings.Count(dir, string(filepath.Separator)) >= savedDirsMaxDepth {
		return nil, ""
	}

	// directories that are already open
	depth := 0
	rest := dir
	for rest != "" && depth < len(d.names) {
		first, tail, _ := strings.Cut(rest, string(filepath.Separator))
		if first != d.names[depth] {
			break
		}
		depth++
		rest = tail
	}
	d.truncate(depth)

	cur := d.root
	if depth > 0 {
		cur = d.roots[depth-1]
	}
	for rest != "" {
		first, tail, _ := strings.Cut(rest, string(filepath.Separator))
		// nil when it is not a real directory: nothing under it can be
		// compared. OpenRoot alone would follow a symlink and would block
		// on a FIFO.
		var next *os.Root
		if cur != nil {
			if fi, err := cur.Lstat(first); err == nil && fi.IsDir() {
				next, _ = cur.OpenRoot(first)
			}
		}
		d.names = append(d.names, first)
		d.roots = append(d.roots, next)
		cur = next
		rest = tail
	}
	return cur, base
}

func (d *savedDirs) truncate(depth int) {
	for i := len(d.roots) - 1; i >= depth; i-- {
		if d.roots[i] != nil {
			d.roots[i].Close()
		}
		d.roots[i] = nil
	}
	d.names = d.names[:depth]
	d.roots = d.roots[:depth]
}

func (d *savedDirs) close() error {
	d.truncate(0)
	return d.root.Close()
}

// contentDiffStream is the receiving side of content diff. It wraps the
// stream that fsutil reads the stats from.
type contentDiffStream struct {
	grpc.ClientStream
	dest string
	dirs *savedDirs

	// order checks the paths as the receiver of fsutil does right after:
	// every path once, in order and after its parent directories. invalid is
	// set at the first path that is not: fsutil fails the transfer then, and
	// nothing else is looked up.
	order   fsutil.Validator
	invalid bool

	// kept has the files whose stat was changed to match the saved copy, in
	// the order of the stats.
	kept []retimedFile
	// requested has the files with the size and the modification time of
	// the saved one and another content or other attributes, whose stat was
	// changed so that fsutil requests the content, in the order of the stats.
	requested []retimedFile

	hashedFiles int
	hashedBytes int64
	// compareTime is the time spent looking at the saved copy: lookups,
	// extended attributes and hashing. fsutil reads the next packet of the
	// stream, stat or file data, only after it.
	compareTime time.Duration
}

// retimedFile is a file whose stat reached fsutil with another modification
// time, with the one that the client sent for it.
type retimedFile struct {
	name    string
	modTime int64
}

func newContentDiffStream(stream grpc.ClientStream, dest string) (*contentDiffStream, error) {
	dirs, err := newSavedDirs(dest)
	if err != nil {
		return nil, err
	}
	return &contentDiffStream{
		ClientStream: stream,
		dest:         dest,
		dirs:         dirs,
	}, nil
}

func (s *contentDiffStream) RecvMsg(m any) error {
	if err := s.ClientStream.RecvMsg(m); err != nil {
		return err
	}
	if p, ok := m.(*fstypes.Packet); ok && p.Type == fstypes.PACKET_STAT && p.Stat != nil {
		s.handleStat(p.Stat)
	}
	return nil
}

// handleStat removes the digest from the stat and compares the file with the
// one of the saved copy. If it is the same file with a different modification
// time, it replaces the modification time with the saved one. If it is
// another file with the same size and modification time, it changes the
// modification time so that it is not the saved one.
func (s *contentDiffStream) handleStat(st *fstypes.Stat) {
	dgst, ok := st.Xattrs[contentDigestXattr]
	if ok {
		delete(st.Xattrs, contentDigestXattr)
		if len(st.Xattrs) == 0 {
			st.Xattrs = nil
		}
	}

	name := filepath.FromSlash(st.Path)
	if s.invalid {
		return
	}
	if err := s.order.HandleChange(fsutil.ChangeKindAdd, name, &fsutil.StatInfo{Stat: st}, nil); err != nil {
		s.invalid = true
		return
	}
	if len(dgst) != sha256.Size || !isContentDiffCandidate(st) {
		return
	}

	start := time.Now()
	defer func() { s.compareTime += time.Since(start) }()

	dir, base := s.dirs.parent(name)
	if dir == nil {
		return
	}
	fi, err := dir.Lstat(base)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != st.Size {
		return
	}
	if runtime.GOOS != "windows" && uint32(fi.Mode()) != st.Mode {
		// fsutil requests the content of a file with another mode, whatever
		// its modification time is
		return
	}
	savedModTime := fi.ModTime().UnixNano()
	same := s.sameSavedFile(dir, base, fi.Size(), st.Xattrs, dgst)
	switch {
	case same && savedModTime != st.ModTime:
		// only the modification time differs: with the saved one fsutil
		// keeps the file
		s.kept = append(s.kept, retimedFile{name: name, modTime: st.ModTime})
		st.ModTime = savedModTime
	case !same && savedModTime == st.ModTime:
		// fsutil would keep the file for its size and modification time:
		// with another time it requests the content
		s.requested = append(s.requested, retimedFile{name: name, modTime: st.ModTime})
		st.ModTime++
	}
}

// sameSavedFile reports whether a regular file of the saved copy has the
// extended attributes and the content that the client sent.
func (s *contentDiffStream) sameSavedFile(dir *os.Root, base string, size int64, xattrs map[string][]byte, dgst []byte) bool {
	f, err := dir.Open(base)
	if err != nil {
		return false
	}
	defer f.Close()

	// fsutil does not compare extended attributes: a file that is kept
	// keeps all the ones of the saved copy, also its ACLs
	saved, err := savedXattrs(f)
	if err != nil || !sameComparedXattrs(xattrs, saved) {
		return false
	}

	s.hashedFiles++
	s.hashedBytes += size
	savedDgst, err := contentDigest(s.Context(), f)
	return err == nil && bytes.Equal(savedDgst[:], dgst)
}

// isComparedXattr reports whether an extended attribute counts when a saved
// file is compared with the file of the client.
//
// Every attribute that fsutil transfers counts, also the ones that are not
// part of the content hash of the cache: ACLs (system.*) and the security
// attributes. A transfer replaces the file and with it all its attributes, so
// a saved file that differs in one of them, or that has one that the client
// did not send, is not the file that a transfer would leave.
//
// The exception is the SELinux label. A node with SELinux gives one to every
// file by its own policy, whether the client sent one or not, and a client
// with SELinux sends the one of its own policy. Compared, no file would ever
// be the same on such a node. A file that is kept keeps the label that it
// has.
func isComparedXattr(name string) bool {
	return name != selinuxXattr
}

// sameComparedXattrs reports whether the extended attributes sent by the
// client and the ones of the saved file (see savedXattrs) are the same, for
// the ones that are compared.
func sameComparedXattrs(sent, saved map[string][]byte) bool {
	n := 0
	for name, value := range sent {
		if !isComparedXattr(name) {
			continue
		}
		n++
		if v, ok := saved[name]; !ok || !bytes.Equal(v, value) {
			return false
		}
	}
	// every compared one of the saved file was sent
	for name := range saved {
		if isComparedXattr(name) {
			n--
		}
	}
	return n == 0
}

// finish applies the modification time sent by the client to the files whose
// stat was changed. It must be called after fsutil has finished writing.
func (s *contentDiffStream) finish() error {
	if len(s.kept) == 0 && len(s.requested) == 0 {
		return nil
	}
	// fsutil may have replaced directories since the stats were read
	dirs, err := newSavedDirs(s.dest)
	if err != nil {
		return err
	}
	defer dirs.close()

	for _, f := range slices.Concat(s.kept, s.requested) {
		t := time.Unix(0, f.modTime)
		dir, base := dirs.parent(f.name)
		if dir == nil {
			// the whole path, to get the error
			dir, base = dirs.root, f.name
		}
		// a file that is not there has no modification time to set: fsutil
		// did not write a path that it filtered out
		if err := dir.Chtimes(base, t, t); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.Wrapf(err, "failed to set modification time of %s", f.name)
		}
	}
	return nil
}

// done ends a transfer that fsutil has finished writing: it sets the
// modification times (see finish) and logs what the comparison did and the
// time that it took.
func (s *contentDiffStream) done(ctx context.Context, name string) error {
	start := time.Now()
	err := s.finish()
	bklog.G(ctx).Debugf("content diff for %s: %d files kept by content, %d files with the saved size and time requested, %d files (%d bytes) hashed, %s comparing with the saved copy, %s setting modification times",
		name, len(s.kept), len(s.requested), s.hashedFiles, s.hashedBytes, s.compareTime.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
	return err
}

func (s *contentDiffStream) close() error {
	return s.dirs.close()
}
