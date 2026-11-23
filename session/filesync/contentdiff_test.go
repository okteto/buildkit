package filesync

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	gofs "io/fs"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tonistiigi/fsutil"
	fstypes "github.com/tonistiigi/fsutil/types"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

var (
	contentDiffTree = map[string]string{
		"Dockerfile":     "FROM scratch\n",
		"empty":          "",
		"src/main.go":    "package main\n",
		"src/lib/lib.go": "package lib\n",
	}
	contentDiffTreeFiles = []string{"Dockerfile", "empty", "src/lib/lib.go", "src/main.go"}

	contentDiffModTimeA = time.Date(2020, 1, 2, 3, 4, 5, 123456789, time.UTC)
	contentDiffModTimeB = time.Date(2021, 6, 7, 8, 9, 10, 987654321, time.UTC)
)

// writeContentDiffTree creates a directory with the given files, all of them
// with the same modification time.
func writeContentDiffTree(t *testing.T, files map[string]string, modTime time.Time) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0700))
		require.NoError(t, os.WriteFile(p, []byte(content), 0600))
		require.NoError(t, os.Chtimes(p, modTime, modTime))
	}
	return dir
}

// recordingUpdater records the regular files that fsutil wrote.
type recordingUpdater struct {
	mu         sync.Mutex
	files      []string
	sawDigests bool
}

func (u *recordingUpdater) MarkSupported(bool) {}

func (u *recordingUpdater) ContentHasher() fsutil.ContentHasher {
	return func(*fstypes.Stat) (hash.Hash, error) {
		return sha256.New(), nil
	}
}

func (u *recordingUpdater) HandleChange(_ fsutil.ChangeKind, p string, fi os.FileInfo, err error) error {
	if err != nil || fi == nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if st, ok := fi.Sys().(*fstypes.Stat); ok {
		if _, ok := st.Xattrs[contentDigestXattr]; ok {
			u.sawDigests = true
		}
	}
	if fi.Mode().IsRegular() {
		u.files = append(u.files, filepath.ToSlash(p))
	}
	return nil
}

func (u *recordingUpdater) writtenFiles() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	files := slices.Clone(u.files)
	slices.Sort(files)
	return files
}

// syncContentDiffDir sends src to dest through a session, as the daemon does
// for a local source, and returns the files that were written.
func syncContentDiffDir(t *testing.T, src, dest string, opt FSSendRequestOpt) *recordingUpdater {
	t.Helper()
	ctx := t.Context()

	srcFS, err := fsutil.NewFS(src)
	require.NoError(t, err)

	s, err := session.NewSession(ctx, "content-diff")
	require.NoError(t, err)
	m, err := session.NewManager()
	require.NoError(t, err)
	s.Allow(NewFSSyncProvider(StaticDirSource{"context": srcFS}))

	updater := &recordingUpdater{}
	opt.Name = "context"
	opt.DestDir = dest
	opt.CacheUpdater = updater

	dialer := session.Dialer(testutil.TestStream(testutil.Handler(m.HandleConn)))
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.Run(ctx, dialer) })
	g.Go(func() (reterr error) {
		defer func() {
			if err := s.Close(); reterr == nil {
				reterr = err
			}
		}()
		c, err := m.Get(ctx, s.ID(), false)
		if err != nil {
			return err
		}
		return FSSync(ctx, c, opt)
	})
	require.NoError(t, g.Wait())
	return updater
}

// requireSameContentDiffTree checks that every regular file of src is in dest
// with the same content, mode and modification time.
func requireSameContentDiffTree(t *testing.T, src, dest string) {
	t.Helper()
	srcFS, destFS := os.DirFS(src), os.DirFS(dest)
	err := gofs.WalkDir(srcFS, ".", func(p string, d gofs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		expected, err := gofs.ReadFile(srcFS, p)
		if err != nil {
			return err
		}
		actual, err := gofs.ReadFile(destFS, p)
		if err != nil {
			return err
		}
		assert.Equal(t, string(expected), string(actual), "content of %s", p)

		srcInfo, err := d.Info()
		if err != nil {
			return err
		}
		destInfo, err := gofs.Stat(destFS, p)
		if err != nil {
			return err
		}
		assert.Equal(t, srcInfo.Mode(), destInfo.Mode(), "mode of %s", p)
		assert.Equal(t, srcInfo.ModTime().UnixNano(), destInfo.ModTime().UnixNano(), "modification time of %s", p)
		return nil
	})
	require.NoError(t, err)
}

func TestContentDiffKeepsFilesWithSameContent(t *testing.T) {
	dirA := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeB)
	dest := t.TempDir()

	first := syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Equal(t, contentDiffTreeFiles, first.writtenFiles())
	assert.False(t, first.sawDigests)
	requireSameContentDiffTree(t, dirA, dest)

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Empty(t, second.writtenFiles())
	assert.False(t, second.sawDigests)
	requireSameContentDiffTree(t, dirB, dest)
}

func TestContentDiffDisabledSendsFilesWithOtherModTime(t *testing.T) {
	dirA := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeB)
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{})

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{})
	assert.Equal(t, contentDiffTreeFiles, second.writtenFiles())
	assert.False(t, second.sawDigests)
	requireSameContentDiffTree(t, dirB, dest)
}

func TestContentDiffSendsFilesWithOtherContent(t *testing.T) {
	changed := map[string]string{
		"Dockerfile":     "FROM alpine3\n",
		"empty":          "",
		"src/main.go":    "package main\n",
		"src/lib/lib.go": "package bil\n",
		"src/new.go":     "package main\n",
	}
	require.Len(t, changed["Dockerfile"], len(contentDiffTree["Dockerfile"]))
	require.Len(t, changed["src/lib/lib.go"], len(contentDiffTree["src/lib/lib.go"]))

	dirA := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, changed, contentDiffModTimeB)
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Equal(t, []string{"Dockerfile", "src/lib/lib.go", "src/new.go"}, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)
}

func TestContentDiffRemovesFilesMissingInTheSource(t *testing.T) {
	dirA := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, map[string]string{"Dockerfile": contentDiffTree["Dockerfile"]}, contentDiffModTimeB)
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Empty(t, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)
	assert.NoDirExists(t, filepath.Join(dest, "src"))
	assert.NoFileExists(t, filepath.Join(dest, "empty"))
}

func TestContentDiffKeepsFilesWithSameModTime(t *testing.T) {
	dir := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	dest := t.TempDir()

	syncContentDiffDir(t, dir, dest, FSSendRequestOpt{ContentDiff: true})

	second := syncContentDiffDir(t, dir, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Empty(t, second.writtenFiles())
	requireSameContentDiffTree(t, dir, dest)
}

func TestUseContentDiff(t *testing.T) {
	tests := []struct {
		name     string
		opt      FSSendRequestOpt
		protocol string
		expected bool
	}{
		{name: "metadata differ", opt: FSSendRequestOpt{ContentDiff: true}, protocol: "diffcopy", expected: true},
		{name: "not asked for", opt: FSSendRequestOpt{}, protocol: "diffcopy"},
		{name: "no differ", opt: FSSendRequestOpt{ContentDiff: true, Differ: fsutil.DiffNone}, protocol: "diffcopy"},
		{name: "content differ", opt: FSSendRequestOpt{ContentDiff: true, Differ: fsutil.DiffContent}, protocol: "diffcopy"},
		{name: "metadata only", opt: FSSendRequestOpt{ContentDiff: true, MetadataOnly: true}, protocol: "diffcopy"},
		{name: "another protocol", opt: FSSendRequestOpt{ContentDiff: true}, protocol: "tarstream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, useContentDiff(tt.opt, tt.protocol))
		})
	}
}

func TestContentDiffOnlyAppliesToMetadataDiffer(t *testing.T) {
	dirA := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeB)
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true, Differ: fsutil.DiffNone})

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true, Differ: fsutil.DiffNone})
	assert.Equal(t, contentDiffTreeFiles, second.writtenFiles())
	assert.False(t, second.sawDigests)
	requireSameContentDiffTree(t, dirB, dest)
}

// contextStream is a grpc.ClientStream that only has a context.
type contextStream struct {
	grpc.ClientStream
	ctx context.Context
}

func (s *contextStream) Context() context.Context {
	return s.ctx
}

const contentDiffSavedContent = "saved content"

// newTestContentDiffStream returns a stream for a destination that has
// "file" (with contentDiffSavedContent and contentDiffModTimeA) and "dir".
func newTestContentDiffStream(t *testing.T) (*contentDiffStream, string) {
	t.Helper()
	dest := writeContentDiffTree(t, map[string]string{"file": contentDiffSavedContent}, contentDiffModTimeA)
	require.NoError(t, os.Mkdir(filepath.Join(dest, "dir"), 0700))

	s, err := newContentDiffStream(&contextStream{ctx: t.Context()}, dest)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, s.close())
	})
	return s, dest
}

func contentDiffDigest(content string) []byte {
	sum := sha256.Sum256([]byte(content))
	return sum[:]
}

// newContentDiffStat is the stat that a client sends for a file with the same
// content as "file" and another modification time.
func newContentDiffStat(path string) *fstypes.Stat {
	return &fstypes.Stat{
		Path:    path,
		Mode:    0600,
		Size:    int64(len(contentDiffSavedContent)),
		ModTime: contentDiffModTimeB.UnixNano(),
		Xattrs:  map[string][]byte{contentDigestXattr: contentDiffDigest(contentDiffSavedContent)},
	}
}

func TestContentDiffStreamKeepsFileWithSameContent(t *testing.T) {
	s, dest := newTestContentDiffStream(t)
	st := newContentDiffStat("file")

	s.handleStat(st)

	assert.Equal(t, contentDiffModTimeA.UnixNano(), st.ModTime)
	assert.Nil(t, st.Xattrs)
	assert.Equal(t, []keptFile{{name: "file", modTime: contentDiffModTimeB.UnixNano()}}, s.kept)
	assert.Equal(t, 1, s.hashedFiles)
	assert.Equal(t, int64(len(contentDiffSavedContent)), s.hashedBytes)

	require.NoError(t, s.finish())
	fi, err := os.Lstat(filepath.Join(dest, "file"))
	require.NoError(t, err)
	assert.Equal(t, contentDiffModTimeB.UnixNano(), fi.ModTime().UnixNano())
}

func TestContentDiffStreamDoesNotKeepFileWhenCancelled(t *testing.T) {
	dest := writeContentDiffTree(t, map[string]string{"file": contentDiffSavedContent}, contentDiffModTimeA)
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errors.New("cancelled by the test"))
	s, err := newContentDiffStream(&contextStream{ctx: ctx}, dest)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, s.close())
	})
	st := newContentDiffStat("file")

	s.handleStat(st)

	assert.Equal(t, contentDiffModTimeB.UnixNano(), st.ModTime)
	assert.Nil(t, st.Xattrs)
	assert.Empty(t, s.kept)
}

func TestContentDiffStreamKeepsOtherXattrs(t *testing.T) {
	s, _ := newTestContentDiffStream(t)
	st := newContentDiffStat("file")
	st.Xattrs["user.test"] = []byte("value")

	s.handleStat(st)

	assert.Equal(t, map[string][]byte{"user.test": []byte("value")}, st.Xattrs)
}

func TestContentDiffStreamDoesNotHashFilesWithSameModTime(t *testing.T) {
	s, _ := newTestContentDiffStream(t)
	st := newContentDiffStat("file")
	st.ModTime = contentDiffModTimeA.UnixNano()

	s.handleStat(st)

	assert.Equal(t, contentDiffModTimeA.UnixNano(), st.ModTime)
	assert.Nil(t, st.Xattrs)
	assert.Empty(t, s.kept)
	assert.Equal(t, 0, s.hashedFiles)
}

func TestContentDiffStreamIgnoresStatWithoutDigest(t *testing.T) {
	s, _ := newTestContentDiffStream(t)
	st := newContentDiffStat("file")
	st.Xattrs = map[string][]byte{"user.test": []byte("value")}

	s.handleStat(st)

	assert.Equal(t, contentDiffModTimeB.UnixNano(), st.ModTime)
	assert.Equal(t, map[string][]byte{"user.test": []byte("value")}, st.Xattrs)
	assert.Empty(t, s.kept)
	assert.Equal(t, 0, s.hashedFiles)
}

func TestContentDiffStreamDoesNotKeepFile(t *testing.T) {
	otherContent := "other content"
	require.Len(t, otherContent, len(contentDiffSavedContent))

	tests := []struct {
		name   string
		change func(*fstypes.Stat)
		hashed int
	}{
		{
			name: "other content with the same size",
			change: func(st *fstypes.Stat) {
				st.Xattrs[contentDigestXattr] = contentDiffDigest(otherContent)
			},
			hashed: 1,
		},
		{
			name: "other size",
			change: func(st *fstypes.Stat) {
				st.Size++
			},
		},
		{
			name: "digest with another length",
			change: func(st *fstypes.Stat) {
				st.Xattrs[contentDigestXattr] = st.Xattrs[contentDigestXattr][:16]
			},
		},
		{
			name: "hardlink",
			change: func(st *fstypes.Stat) {
				st.Linkname = "other"
			},
		},
		{
			name: "directory",
			change: func(st *fstypes.Stat) {
				st.Mode = uint32(os.ModeDir | 0700)
			},
		},
		{
			name: "directory in the destination",
			change: func(st *fstypes.Stat) {
				st.Path = "dir"
			},
		},
		{
			name: "missing in the destination",
			change: func(st *fstypes.Stat) {
				st.Path = "missing"
			},
		},
		{
			name: "path outside of the destination",
			change: func(st *fstypes.Stat) {
				st.Path = "dir/../../file"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, dest := newTestContentDiffStream(t)
			// what "dir/../../file" would reach without the checks of the path
			outside := filepath.Join(filepath.Dir(dest), "file")
			require.NoError(t, os.WriteFile(outside, []byte(contentDiffSavedContent), 0600))
			require.NoError(t, os.Chtimes(outside, contentDiffModTimeA, contentDiffModTimeA))
			st := newContentDiffStat("file")
			tt.change(st)

			s.handleStat(st)

			assert.Equal(t, contentDiffModTimeB.UnixNano(), st.ModTime)
			assert.Nil(t, st.Xattrs)
			assert.Empty(t, s.kept)
			assert.Equal(t, 0, s.hashedFiles-tt.hashed)
		})
	}
}

// newContentDiffDirStat is the stat that a client sends for a directory.
func newContentDiffDirStat(path string) *fstypes.Stat {
	return &fstypes.Stat{Path: path, Mode: uint32(os.ModeDir | 0700)}
}

func TestContentDiffStreamStopsAtPathOutOfOrder(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "repeated path", path: "file"},
		{name: "path before the last one", path: "a"},
		{name: "parent that was not sent", path: "dir/file"},
		{name: "unclean path", path: "dir/../file"},
		{name: "absolute path", path: "/file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, dest := newTestContentDiffStream(t)
			for _, name := range []string{"a", filepath.Join("dir", "file"), "z"} {
				p := filepath.Join(dest, name)
				require.NoError(t, os.WriteFile(p, []byte(contentDiffSavedContent), 0600))
				require.NoError(t, os.Chtimes(p, contentDiffModTimeA, contentDiffModTimeA))
			}
			first, wrong, later := newContentDiffStat("file"), newContentDiffStat(tt.path), newContentDiffStat("z")

			s.handleStat(first)
			s.handleStat(wrong)
			s.handleStat(later)

			// the first file is kept; nothing is looked up from the path that fsutil rejects
			assert.Equal(t, []keptFile{{name: "file", modTime: contentDiffModTimeB.UnixNano()}}, s.kept)
			assert.Equal(t, 1, s.hashedFiles)
			assert.True(t, s.invalid)
			assert.Equal(t, contentDiffModTimeB.UnixNano(), wrong.ModTime)
			assert.Equal(t, contentDiffModTimeB.UnixNano(), later.ModTime)
			assert.Nil(t, wrong.Xattrs)
			assert.Nil(t, later.Xattrs)
		})
	}
}

func TestContentDiffStreamKeepsFilesOfDirectories(t *testing.T) {
	s, dest := newTestContentDiffStream(t)
	nested := filepath.Join(dest, "dir", "sub", "file")
	require.NoError(t, os.MkdirAll(filepath.Dir(nested), 0700))
	require.NoError(t, os.WriteFile(nested, []byte(contentDiffSavedContent), 0600))
	require.NoError(t, os.Chtimes(nested, contentDiffModTimeA, contentDiffModTimeA))

	for _, st := range []*fstypes.Stat{
		newContentDiffDirStat("dir"),
		newContentDiffDirStat("dir/sub"),
		newContentDiffStat("dir/sub/file"),
		newContentDiffStat("file"),
	} {
		s.handleStat(st)
	}

	assert.False(t, s.invalid)
	assert.Equal(t, []keptFile{
		{name: filepath.Join("dir", "sub", "file"), modTime: contentDiffModTimeB.UnixNano()},
		{name: "file", modTime: contentDiffModTimeB.UnixNano()},
	}, s.kept)
	require.NoError(t, s.finish())
	fi, err := os.Lstat(nested)
	require.NoError(t, err)
	assert.Equal(t, contentDiffModTimeB.UnixNano(), fi.ModTime().UnixNano())
}

func TestIsComparedXattr(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{name: "user.test", expected: true},
		{name: "trusted.test", expected: true},
		{name: "security.capability", expected: true},
		// not part of the content hash of the cache, but fsutil transfers them
		{name: "security.ima", expected: true},
		{name: "system.posix_acl_access", expected: true},
		{name: "system.posix_acl_default", expected: true},
		// the node of the daemon labels every file
		{name: "security.selinux"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isComparedXattr(tt.name))
		})
	}
}

func TestSameComparedXattrs(t *testing.T) {
	const acl = "system.posix_acl_access"
	tests := []struct {
		name     string
		sent     map[string][]byte
		saved    map[string][]byte
		expected bool
	}{
		{name: "none", expected: true},
		{
			name:     "same",
			sent:     map[string][]byte{"user.a": []byte("1"), "security.capability": []byte("cap")},
			saved:    map[string][]byte{"user.a": []byte("1"), "security.capability": []byte("cap")},
			expected: true,
		},
		{name: "only in the saved file", saved: map[string][]byte{"security.capability": []byte("cap")}},
		{name: "only sent", sent: map[string][]byte{"user.a": []byte("1")}},
		{
			name:  "other value",
			sent:  map[string][]byte{"user.a": []byte("1")},
			saved: map[string][]byte{"user.a": []byte("2")},
		},
		{
			name:  "other name",
			sent:  map[string][]byte{"user.a": []byte("1")},
			saved: map[string][]byte{"user.b": []byte("1")},
		},
		{
			name:  "one more in the saved file",
			sent:  map[string][]byte{"user.a": []byte("1")},
			saved: map[string][]byte{"user.a": []byte("1"), "user.b": []byte("2")},
		},
		{
			name:     "same ACL",
			sent:     map[string][]byte{acl: []byte("acl")},
			saved:    map[string][]byte{acl: []byte("acl")},
			expected: true,
		},
		{
			name:  "ACL that the client changed",
			sent:  map[string][]byte{acl: []byte("new")},
			saved: map[string][]byte{acl: []byte("old")},
		},
		{name: "ACL that the client removed", saved: map[string][]byte{acl: []byte("acl")}},
		{name: "ACL that the client added", sent: map[string][]byte{acl: []byte("acl")}},
		{name: "security attribute only in the saved file", saved: map[string][]byte{"security.ima": []byte("ima")}},
		{
			name:  "security attribute with another value",
			sent:  map[string][]byte{"security.ima": []byte("new")},
			saved: map[string][]byte{"security.ima": []byte("old")},
		},
		{
			name:     "SELinux label that only the client has",
			sent:     map[string][]byte{"security.selinux": []byte("label")},
			expected: true,
		},
		{
			name:     "SELinux label that only the saved file has",
			saved:    map[string][]byte{"security.selinux": []byte("label")},
			expected: true,
		},
		{
			name:     "other SELinux label",
			sent:     map[string][]byte{"security.selinux": []byte("client")},
			saved:    map[string][]byte{"security.selinux": []byte("node")},
			expected: true,
		},
		{
			name:  "same SELinux label and another ACL",
			sent:  map[string][]byte{"security.selinux": []byte("label"), acl: []byte("new")},
			saved: map[string][]byte{"security.selinux": []byte("label"), acl: []byte("old")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, sameComparedXattrs(tt.sent, tt.saved))
		})
	}
}

func TestSavedDirsParentTooDeep(t *testing.T) {
	dest := t.TempDir()
	deepest := filepath.Join(dest, strings.Repeat("d"+string(filepath.Separator), savedDirsMaxDepth+1))
	require.NoError(t, os.MkdirAll(deepest, 0700))
	dirs, err := newSavedDirs(dest)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, dirs.close())
	})
	nested := func(depth int) string {
		return strings.Repeat("d"+string(filepath.Separator), depth) + "file"
	}

	dir, _ := dirs.parent(nested(savedDirsMaxDepth + 1))
	assert.Nil(t, dir)
	assert.Empty(t, dirs.roots)

	dir, base := dirs.parent(nested(savedDirsMaxDepth))
	assert.NotNil(t, dir)
	assert.Equal(t, "file", base)
	assert.Len(t, dirs.roots, savedDirsMaxDepth)
}

// walkContentDiffStats returns the stat of every path of fs.
func walkContentDiffStats(t *testing.T, fs fsutil.FS) map[string]*fstypes.Stat {
	t.Helper()
	stats := map[string]*fstypes.Stat{}
	err := fs.Walk(t.Context(), "", func(p string, d gofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		stats[filepath.ToSlash(p)] = fi.Sys().(*fstypes.Stat)
		return nil
	})
	require.NoError(t, err)
	return stats
}

func TestWithContentDigests(t *testing.T) {
	dir := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	base, err := fsutil.NewFS(dir)
	require.NoError(t, err)

	fs := withContentDigests(base)
	stats := walkContentDiffStats(t, fs)

	require.Len(t, stats, len(contentDiffTree)+2)
	for name, content := range contentDiffTree {
		assert.Equal(t, contentDiffDigest(content), stats[name].Xattrs[contentDigestXattr], name)
		assert.Equal(t, contentDiffModTimeA.UnixNano(), stats[name].ModTime, name)
	}
	assert.NotContains(t, stats["src"].Xattrs, contentDigestXattr)
	assert.NotContains(t, stats["src/lib"].Xattrs, contentDigestXattr)
}

func TestWithContentDigestsKeepsOtherXattrs(t *testing.T) {
	dir := writeContentDiffTree(t, map[string]string{"file": "content"}, contentDiffModTimeA)
	base, err := fsutil.NewFS(dir)
	require.NoError(t, err)
	withXattr, err := fsutil.NewFilterFS(base, &fsutil.FilterOpt{
		Map: func(_ string, st *fstypes.Stat) fsutil.MapResult {
			st.Xattrs = map[string][]byte{"user.test": []byte("value")}
			return fsutil.MapResultKeep
		},
	})
	require.NoError(t, err)

	fs := withContentDigests(withXattr)
	stats := walkContentDiffStats(t, fs)

	assert.Equal(t, map[string][]byte{
		"user.test":        []byte("value"),
		contentDigestXattr: contentDiffDigest("content"),
	}, stats["file"].Xattrs)
}

func TestContentDiffKeepsFilesOfManyDirectories(t *testing.T) {
	// more files than a batch of the sender, in directories of several levels
	files := map[string]string{}
	for d := range 40 {
		for f := range 40 {
			files[fmt.Sprintf("dir%02d/sub%d/file%02d", d, f%3, f)] = fmt.Sprintf("content %d %d", d, f)
		}
	}
	require.Greater(t, len(files), contentDigestBatchEntries)
	dirA := writeContentDiffTree(t, files, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, files, contentDiffModTimeB)
	dest := t.TempDir()

	first := syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Len(t, first.writtenFiles(), len(files))

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Empty(t, second.writtenFiles())
	assert.False(t, second.sawDigests)
	requireSameContentDiffTree(t, dirB, dest)
}

func TestContentDiffStreamFinishIgnoresFilesThatAreGone(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		remove string
	}{
		{name: "file", path: "file", remove: "file"},
		{name: "directory of the file", path: "dir/file", remove: "dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, dest := newTestContentDiffStream(t)
			s.kept = []keptFile{{name: filepath.FromSlash(tt.path), modTime: contentDiffModTimeB.UnixNano()}}
			require.NoError(t, os.RemoveAll(filepath.Join(dest, tt.remove)))

			require.NoError(t, s.finish())
		})
	}
}

func TestContentDiffStreamFinishFails(t *testing.T) {
	s, _ := newTestContentDiffStream(t)
	s.kept = []keptFile{{name: filepath.FromSlash("../outside"), modTime: contentDiffModTimeB.UnixNano()}}

	require.ErrorContains(t, s.finish(), "failed to set modification time")
}

func TestSavedDirsParent(t *testing.T) {
	dest := writeContentDiffTree(t, map[string]string{
		"top":        "",
		"a/file":     "",
		"a/b/file":   "",
		"a/b/c/file": "",
		"d/file":     "",
	}, contentDiffModTimeA)
	dirs, err := newSavedDirs(dest)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, dirs.close())
	})

	// the order of a walk, and a path that goes back to a directory that was closed
	tests := []struct {
		name  string
		depth int
	}{
		{name: "a/b/c/file", depth: 3},
		{name: "a/b/file", depth: 2},
		{name: "a/file", depth: 1},
		{name: "d/file", depth: 1},
		{name: "top", depth: 0},
		{name: "a/b/file", depth: 2},
	}
	for _, tt := range tests {
		dir, base := dirs.parent(filepath.FromSlash(tt.name))

		require.NotNil(t, dir, tt.name)
		assert.Equal(t, path.Base(tt.name), base, tt.name)
		_, err := dir.Lstat(base)
		require.NoError(t, err, tt.name)
		assert.Len(t, dirs.roots, tt.depth, tt.name)
		assert.Equal(t, strings.Split(path.Dir(tt.name), "/")[:tt.depth], dirs.names, tt.name)
	}
}

func TestSavedDirsParentWithoutDirectory(t *testing.T) {
	dest := writeContentDiffTree(t, map[string]string{"top": "", "a/file": ""}, contentDiffModTimeA)

	tests := []string{
		"",
		"../file",
		"a/../../file",
		"a/../file",
		"/a/file",
		"a//file",
		"a/./file",
		"missing/file",
		"missing/dir/file",
		"top/file",
	}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			dirs, err := newSavedDirs(dest)
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, dirs.close())
			})

			dir, _ := dirs.parent(filepath.FromSlash(name))

			assert.Nil(t, dir)
		})
	}
}

// contentDiffWalkTree has directories with files before and after their
// subdirectories.
var contentDiffWalkTree = map[string]string{
	"a/1":     "a1",
	"a/2":     "a2",
	"a/b/1":   "ab1",
	"a/b/2":   "ab2",
	"a/b/c/1": "abc1",
	"a/z":     "az",
	"a-b/1":   "a-b1",
	"d/1":     "d1",
	"d/2":     "d2",
	"e":       "e",
	"f/g/1":   "fg1",
	"f/h":     "fh",
	"z":       "z",
}

func newContentDiffWalkFS(t *testing.T) fsutil.FS {
	t.Helper()
	base, err := fsutil.NewFS(writeContentDiffTree(t, contentDiffWalkTree, contentDiffModTimeA))
	require.NoError(t, err)
	// the sender always wraps a filtered FS
	fs, err := fsutil.NewFilterFS(base, &fsutil.FilterOpt{})
	require.NoError(t, err)
	return fs
}

// walkContentDiffPaths returns the paths that a walk of fs gives to a
// function that answers with the result of decide.
func walkContentDiffPaths(t *testing.T, fs fsutil.FS, decide func(p string) error) ([]string, error) {
	t.Helper()
	var paths []string
	err := fs.Walk(t.Context(), "", func(p string, _ gofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		p = filepath.ToSlash(p)
		paths = append(paths, p)
		return decide(p)
	})
	return paths, err
}

func TestContentDigestFSWalksLikeTheWrappedFS(t *testing.T) {
	errStop := errors.New("stop")
	answer := func(at string, result error) func(string) error {
		return func(p string) error {
			if p == at {
				return result
			}
			return nil
		}
	}
	tests := []struct {
		name   string
		decide func(string) error
	}{
		{name: "everything", decide: answer("", nil)},
		{name: "skip a directory", decide: answer("a/b", filepath.SkipDir)},
		{name: "skip a directory with subdirectories", decide: answer("a", filepath.SkipDir)},
		{name: "skip the last directory", decide: answer("f", filepath.SkipDir)},
		{name: "skip the rest of a directory", decide: answer("a/1", filepath.SkipDir)},
		{name: "skip the rest of a subdirectory", decide: answer("a/b/1", filepath.SkipDir)},
		{name: "skip the rest of the root", decide: answer("e", filepath.SkipDir)},
		{name: "skip all from a file", decide: answer("a/b/c/1", filepath.SkipAll)},
		{name: "skip all from a directory", decide: answer("d", filepath.SkipAll)},
		{name: "error from a file", decide: answer("d/1", errStop)},
		{name: "error from a directory", decide: answer("a/b", errStop)},
		{name: "file that is gone", decide: answer("a/b/1", os.ErrNotExist)},
		{name: "directory that is gone", decide: answer("a/b", &os.PathError{Op: "open", Path: "a/b", Err: syscall.ENOENT})},
		{name: "parent that is not a directory", decide: answer("e", syscall.ENOTDIR)},
		{name: "last file that is gone", decide: answer("z", os.ErrNotExist)},
	}
	for _, tt := range tests {
		for _, batchEntries := range []int{1, 2, 3, 5, 7, contentDigestBatchEntries} {
			for _, workers := range []int{1, 4} {
				t.Run(fmt.Sprintf("%s/batch %d/workers %d", tt.name, batchEntries, workers), func(t *testing.T) {
					wrapped := newContentDiffWalkFS(t)
					expected, expectedErr := walkContentDiffPaths(t, wrapped, tt.decide)
					require.NotEmpty(t, expected)

					fs := &contentDigestFS{FS: wrapped, workers: workers, batchEntries: batchEntries, batchBytes: contentDigestBatchBytes}
					paths, err := walkContentDiffPaths(t, fs, tt.decide)

					assert.Equal(t, expected, paths)
					assert.Equal(t, expectedErr, err)
				})
			}
		}
	}
}

func TestContentDigestFSWalksLikeTheWrappedFSWithSeveralAnswers(t *testing.T) {
	errStop := errors.New("stop")
	wrapped := newContentDiffWalkFS(t)
	all, err := walkContentDiffPaths(t, wrapped, func(string) error { return nil })
	require.NoError(t, err)

	for seed := range uint64(300) {
		// an answer for every path, most of them "go on"
		rnd := rand.New(rand.NewPCG(seed, 1)) //nolint:gosec // answers of a test
		answers := map[string]error{}
		for _, p := range all {
			switch n := rnd.IntN(100); {
			case n < 12:
				answers[p] = filepath.SkipDir
			case n < 14:
				answers[p] = filepath.SkipAll
			case n < 20:
				answers[p] = os.ErrNotExist
			case n < 23:
				answers[p] = errStop
			}
		}
		decide := func(p string) error { return answers[p] }
		expected, expectedErr := walkContentDiffPaths(t, wrapped, decide)

		for _, batchEntries := range []int{1, 2, 3, 5, contentDigestBatchEntries} {
			fs := &contentDigestFS{FS: wrapped, workers: 4, batchEntries: batchEntries, batchBytes: contentDigestBatchBytes}
			paths, err := walkContentDiffPaths(t, fs, decide)

			require.Equal(t, expected, paths, "seed %d, batch %d", seed, batchEntries)
			require.Equal(t, expectedErr, err, "seed %d, batch %d", seed, batchEntries)
		}
	}
}

func TestContentDigestFSAddsDigestsInBatches(t *testing.T) {
	tests := []struct {
		name         string
		workers      int
		batchEntries int
		batchBytes   int64
	}{
		{name: "one file at a time", workers: 1, batchEntries: 1, batchBytes: contentDigestBatchBytes},
		{name: "small batches", workers: 4, batchEntries: 3, batchBytes: contentDigestBatchBytes},
		{name: "batches by size", workers: 4, batchEntries: contentDigestBatchEntries, batchBytes: 5},
		{name: "one batch", workers: 4, batchEntries: contentDigestBatchEntries, batchBytes: contentDigestBatchBytes},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &contentDigestFS{FS: newContentDiffWalkFS(t), workers: tt.workers, batchEntries: tt.batchEntries, batchBytes: tt.batchBytes}

			stats := walkContentDiffStats(t, fs)

			require.Len(t, stats, len(contentDiffWalkTree)+7)
			for name, content := range contentDiffWalkTree {
				assert.Equal(t, contentDiffDigest(content), stats[name].Xattrs[contentDigestXattr], name)
			}
			assert.NotContains(t, stats["a/b"].Xattrs, contentDigestXattr)
		})
	}
}

// stubContentDiffFS is an FS with the walk and the files that a test needs.
type stubContentDiffFS struct {
	walk func(gofs.WalkDirFunc) error
	open func(string) (io.ReadCloser, error)
}

func (fs *stubContentDiffFS) Walk(_ context.Context, _ string, fn gofs.WalkDirFunc) error {
	return fs.walk(fn)
}

func (fs *stubContentDiffFS) Open(p string) (io.ReadCloser, error) {
	return fs.open(p)
}

func newContentDiffEntry(p string) gofs.DirEntry {
	return &fsutil.DirEntryInfo{Stat: &fstypes.Stat{Path: p, Mode: 0600, Size: int64(len(contentDiffSavedContent))}}
}

func TestContentDigestFSKeepsTheOrderOfWalkErrors(t *testing.T) {
	errWalk := errors.New("walk error")
	fs := &contentDigestFS{
		FS: &stubContentDiffFS{
			walk: func(fn gofs.WalkDirFunc) error {
				for _, p := range []string{"1", "2"} {
					if err := fn(p, newContentDiffEntry(p), nil); err != nil {
						return err
					}
				}
				return fn("3", nil, errWalk)
			},
			open: func(string) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(contentDiffSavedContent)), nil
			},
		},
		workers:      4,
		batchEntries: contentDigestBatchEntries,
		batchBytes:   contentDigestBatchBytes,
	}

	var seen []string
	err := fs.Walk(t.Context(), "", func(p string, _ gofs.DirEntry, err error) error {
		seen = append(seen, p)
		return err
	})

	require.ErrorIs(t, err, errWalk)
	assert.Equal(t, []string{"1", "2", "3"}, seen)
}

func TestContentDigestFSSendsFilesThatCanNotBeRead(t *testing.T) {
	fs := &contentDigestFS{
		FS: &stubContentDiffFS{
			walk: func(fn gofs.WalkDirFunc) error {
				for _, p := range []string{"denied", "file"} {
					if err := fn(p, newContentDiffEntry(p), nil); err != nil {
						return err
					}
				}
				return nil
			},
			open: func(p string) (io.ReadCloser, error) {
				if p == "denied" {
					return nil, os.ErrPermission
				}
				return io.NopCloser(strings.NewReader(contentDiffSavedContent)), nil
			},
		},
		workers:      4,
		batchEntries: contentDigestBatchEntries,
		batchBytes:   contentDigestBatchBytes,
	}

	stats := walkContentDiffStats(t, fs)

	require.Len(t, stats, 2)
	assert.NotContains(t, stats["denied"].Xattrs, contentDigestXattr)
	assert.Equal(t, contentDiffDigest(contentDiffSavedContent), stats["file"].Xattrs[contentDigestXattr])
}

func TestContentDigestFSDoesNotReadSkippedDirectory(t *testing.T) {
	// filepath.WalkDir calls back with a directory that it can't read, unless
	// the function skipped it
	errRead := errors.New("can't read the directory")
	locked := &fsutil.DirEntryInfo{Stat: newContentDiffDirStat("locked")}
	wrapped := &stubContentDiffFS{
		walk: func(fn gofs.WalkDirFunc) error {
			if err := fn("a", newContentDiffEntry("a"), nil); err != nil {
				return err
			}
			err := fn("locked", locked, nil)
			if err == nil {
				err = fn("locked", locked, errRead)
			}
			if err != nil && err != filepath.SkipDir { //nolint:errorlint // as filepath.WalkDir
				return err
			}
			return fn("z", newContentDiffEntry("z"), nil)
		},
		open: func(string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(contentDiffSavedContent)), nil
		},
	}
	skipLocked := func(p string, _ gofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "locked" {
			return filepath.SkipDir
		}
		return nil
	}

	for _, batchEntries := range []int{1, 2, contentDigestBatchEntries} {
		t.Run(fmt.Sprintf("batch %d", batchEntries), func(t *testing.T) {
			var seen []string
			fs := &contentDigestFS{FS: wrapped, workers: 4, batchEntries: batchEntries, batchBytes: contentDigestBatchBytes}

			err := fs.Walk(t.Context(), "", func(p string, d gofs.DirEntry, err error) error {
				seen = append(seen, p)
				return skipLocked(p, d, err)
			})

			require.NoError(t, err)
			assert.Equal(t, []string{"a", "locked", "z"}, seen)
		})
	}
}
