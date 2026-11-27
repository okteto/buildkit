//go:build !windows

package local

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	gofs "io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/diff/apply"
	ctdmetadata "github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/core/snapshots"
	ctdlocal "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/containerd/v2/plugins/diff/walking"
	"github.com/containerd/containerd/v2/plugins/snapshots/native"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/contenthash"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/okteto"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/session/testutil"
	"github.com/moby/buildkit/snapshot"
	containerdsnapshot "github.com/moby/buildkit/snapshot/containerd"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/compat"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/buildkit/util/winlayers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tonistiigi/fsutil"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sync/errgroup"
)

const testLocalName = "context"

var (
	testLocalTree = map[string]string{
		"Dockerfile":  "FROM scratch\n",
		"src/main.go": "package main\n",
	}
	testLocalModTimeA = time.Date(2020, 1, 2, 3, 4, 5, 123456789, time.UTC)
	testLocalModTimeB = time.Date(2021, 6, 7, 8, 9, 10, 987654321, time.UTC)
)

func newTestCacheManager(t *testing.T) cache.Manager {
	t.Helper()
	return newTestCacheManagerAt(t, t.TempDir())
}

// newTestCacheManagerAt opens a cache manager on the state that is stored in
// tmpdir, as a daemon does with its root directory when it starts.
func newTestCacheManagerAt(t *testing.T, tmpdir string) cache.Manager {
	t.Helper()

	snapshotter, err := native.NewSnapshotter(filepath.Join(tmpdir, "snapshots"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, snapshotter.Close())
	})

	store, err := ctdlocal.NewStore(tmpdir)
	require.NoError(t, err)

	db, err := bolt.Open(filepath.Join(tmpdir, "containerdmeta.db"), 0644, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	mdb := ctdmetadata.NewDB(db, store, map[string]snapshots.Snapshotter{
		"native": snapshotter,
	})

	md, err := metadata.NewStore(filepath.Join(tmpdir, "metadata.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, md.Close())
	})

	c := mdb.ContentStore()
	cm, err := cache.NewManager(cache.ManagerOpt{
		Snapshotter:    snapshot.FromContainerdSnapshotter("native", containerdsnapshot.NSSnapshotter("buildkit", mdb.Snapshotter("native")), nil),
		MetadataStore:  md,
		LeaseManager:   leaseutil.WithNamespace(ctdmetadata.NewLeaseManager(mdb), "buildkit"),
		ContentStore:   c,
		Applier:        winlayers.NewFileSystemApplierWithWindows(c, apply.NewFileSystemApplier(c)),
		Differ:         winlayers.NewWalkingDiffWithWindows(c, walking.NewWalkingDiff(c)),
		GarbageCollect: mdb.GarbageCollect,
		Root:           tmpdir,
		MountPoolRoot:  filepath.Join(tmpdir, "cachemounts"),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, cm.Close())
	})
	return cm
}

// writeTestLocalTree creates a directory with the files of testLocalTree, all
// of them with the same modification time.
func writeTestLocalTree(t *testing.T, modTime time.Time) string {
	t.Helper()
	dir := t.TempDir()
	writeTestFiles(t, dir, testLocalTree, modTime)
	return dir
}

// writeTestFiles writes files in dir, all of them with the same modification
// time.
func writeTestFiles(t *testing.T, dir string, files map[string]string, modTime time.Time) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0700))
		require.NoError(t, os.WriteFile(p, []byte(content), 0600))
		require.NoError(t, os.Chtimes(p, modTime, modTime))
	}
}

// testJobContext is the job context of a build step. Its first build is the
// one of the client that sends the files, in the session sessionID: that
// client named source as the source of the files of the local directory, or
// none if it is empty. others are the builds of other clients that share the
// step.
type testJobContext struct {
	sessionID string
	source    string
	others    []testOtherBuild
}

// testOtherBuild is a build of another client: its session and the source
// that it named, empty if it named none.
type testOtherBuild struct {
	sessionID string
	source    string
}

func (j testJobContext) Session() session.Group {
	return session.NewGroup(j.sessionID)
}

func (j testJobContext) Cleanup(func() error) error {
	return nil
}

func (j testJobContext) ResolverCache() solver.ResolverCache {
	return nil
}

func (j testJobContext) CompatibilityVersion() (int, error) {
	return compat.CompatibilityVersionCurrent, nil
}

func (j testJobContext) EachJobValue(key string, fn func(sessionID string, value any)) {
	builds := append([]testOtherBuild{{sessionID: j.sessionID, source: j.source}}, j.others...)
	for _, b := range builds {
		var value any
		if key == okteto.LocalSourcesJobKey && b.source != "" {
			value = map[string]string{testLocalName: b.source}
		}
		fn(b.sessionID, value)
	}
}

// testLocalBuild is a build of a client that sends dir in a session with the
// given shared key.
type testLocalBuild struct {
	id        LocalIdentifier
	dir       string
	sharedKey string
	// source is the source that the client named for the files, empty if it
	// named none
	source string
	// sameSession are the sources named by other builds of the same client
	// that share the step, empty for a build that named none
	sameSession []string
	// prefixedSession are the sources named by other builds of the same
	// client whose session is written with a prefix, empty for a build that
	// named none
	prefixedSession []string
	// others are the builds of other clients that share the step
	others []testOtherBuild
	// anySession leaves the session out of the local source, so the files
	// are requested from any session of the builds of the step
	anySession bool
	// fs are the files that the client sends, instead of the ones of dir
	fs fsutil.FS
}

// snapshotTestLocal runs the snapshot of the local source id for a client
// that sends dir in a session with the given shared key, in a build that
// named source as the source of the files.
func snapshotTestLocal(t *testing.T, cm cache.Manager, id LocalIdentifier, dir, sharedKey, source string) cache.ImmutableRef {
	t.Helper()
	return snapshotTestLocalBuild(t, cm, testLocalBuild{id: id, dir: dir, sharedKey: sharedKey, source: source})
}

// snapshotTestLocalBuild runs the snapshot of the local source of a build.
func snapshotTestLocalBuild(t *testing.T, cm cache.Manager, b testLocalBuild) cache.ImmutableRef {
	t.Helper()
	ctx := t.Context()

	dirFS := b.fs
	if dirFS == nil {
		var err error
		dirFS, err = fsutil.NewFS(b.dir)
		require.NoError(t, err)
	}
	s, err := session.NewSession(ctx, b.sharedKey)
	require.NoError(t, err)
	sm, err := session.NewManager()
	require.NoError(t, err)
	s.Allow(filesync.NewFSSyncProvider(filesync.StaticDirSource{testLocalName: dirFS}))

	id := b.id
	id.SessionID = s.ID()
	if b.anySession {
		id.SessionID = ""
	}
	ls := &localSourceHandler{
		src:         id,
		sm:          sm,
		localSource: &localSource{cm: cm},
	}

	var ref cache.ImmutableRef
	dialer := session.Dialer(testutil.TestStream(testutil.Handler(sm.HandleConn)))
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.Run(ctx, dialer) })
	g.Go(func() (reterr error) {
		defer func() {
			if err := s.Close(); reterr == nil {
				reterr = err
			}
		}()
		// the session is registered when the first request of the client arrives
		if _, err := sm.Get(ctx, s.ID(), false); err != nil {
			return err
		}
		others := slices.Clone(b.others)
		for _, source := range b.sameSession {
			others = append(others, testOtherBuild{sessionID: s.ID(), source: source})
		}
		for _, source := range b.prefixedSession {
			others = append(others, testOtherBuild{sessionID: "prefix:" + s.ID(), source: source})
		}
		ref, err = ls.Snapshot(ctx, testJobContext{sessionID: s.ID(), source: b.source, others: others})
		return err
	})
	require.NoError(t, g.Wait())
	return ref
}

// syncTestLocalBuild snapshots the local directory of a build and releases
// the result, so the copy can be reused.
func syncTestLocalBuild(t *testing.T, cm cache.Manager, b testLocalBuild) {
	t.Helper()
	b.id = LocalIdentifier{Name: testLocalName, Differ: fsutil.DiffMetadata}
	ref := snapshotTestLocalBuild(t, cm, b)
	require.NoError(t, ref.Release(t.Context()))
}

// snapshotTestLocalDir runs the snapshot of the build context.
func snapshotTestLocalDir(t *testing.T, cm cache.Manager, dir, sharedKey, source string) cache.ImmutableRef {
	t.Helper()
	return snapshotTestLocal(t, cm, LocalIdentifier{Name: testLocalName, Differ: fsutil.DiffMetadata}, dir, sharedKey, source)
}

// syncTestLocalDir snapshots dir and releases the result, so the copy can be
// reused.
func syncTestLocalDir(t *testing.T, cm cache.Manager, dir, sharedKey, source string) {
	t.Helper()
	ref := snapshotTestLocalDir(t, cm, dir, sharedKey, source)
	require.NoError(t, ref.Release(t.Context()))
}

type testSavedFile struct {
	Inode   uint64
	ModTime int64
	Content string
}

type testSavedCopy struct {
	id     string
	source string
	files  map[string]testSavedFile
}

func readTestSavedFiles(t *testing.T, dir string) map[string]testSavedFile {
	t.Helper()
	files := map[string]testSavedFile{}
	dirFS := os.DirFS(dir)
	err := gofs.WalkDir(dirFS, ".", func(p string, d gofs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		content, err := gofs.ReadFile(dirFS, p)
		if err != nil {
			return err
		}
		files[p] = testSavedFile{
			Inode:   uint64(fi.Sys().(*syscall.Stat_t).Ino),
			ModTime: fi.ModTime().UnixNano(),
			Content: string(content),
		}
		return nil
	})
	require.NoError(t, err)
	return files
}

// readTestSavedCopies returns the copies of the local source that are stored
// with the given key.
func readTestSavedCopies(t *testing.T, cm cache.Manager, key string) []testSavedCopy {
	t.Helper()
	ctx := t.Context()

	sis, err := searchSharedKey(ctx, cm, testLocalName+"::"+key)
	require.NoError(t, err)
	copies := make([]testSavedCopy, 0, len(sis))
	for _, si := range sis {
		mutable, err := cm.GetMutable(ctx, si.ID())
		require.NoError(t, err)
		mount, err := mutable.Mount(ctx, false, nil)
		require.NoError(t, err)
		lm := snapshot.LocalMounter(mount)
		dir, err := lm.Mount()
		require.NoError(t, err)
		copies = append(copies, testSavedCopy{
			id:     mutable.ID(),
			source: cacheRefMetadata{mutable}.getSyncSource(),
			files:  readTestSavedFiles(t, dir),
		})
		require.NoError(t, lm.Unmount())
		require.NoError(t, mutable.Release(ctx))
	}
	return copies
}

func testSavedCopySources(copies []testSavedCopy) []string {
	sources := make([]string, 0, len(copies))
	for _, c := range copies {
		sources = append(sources, c.source)
	}
	return sources
}

// withTestModTime returns the files with another modification time.
func withTestModTime(files map[string]testSavedFile, modTime time.Time) map[string]testSavedFile {
	out := maps.Clone(files)
	for name, f := range out {
		f.ModTime = modTime.UnixNano()
		out[name] = f
	}
	return out
}

func TestSnapshotComparesContentForOtherSource(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := writeTestLocalTree(t, testLocalModTimeA)
	dirB := writeTestLocalTree(t, testLocalModTimeB)

	syncTestLocalDir(t, cm, dirA, "repository", "a")
	first := readTestSavedCopies(t, cm, "repository")
	require.Len(t, first, 1)
	require.Len(t, first[0].files, len(testLocalTree))

	syncTestLocalDir(t, cm, dirB, "repository", "b")
	second := readTestSavedCopies(t, cm, "repository")
	require.Len(t, second, 1)

	assert.Equal(t, "a", first[0].source)
	assert.Equal(t, "b", second[0].source)
	// same inodes: no file was written again, only the modification times changed
	assert.Equal(t, withTestModTime(first[0].files, testLocalModTimeB), second[0].files)
}

func TestSnapshotComparesModTimesForSameSource(t *testing.T) {
	cm := newTestCacheManager(t)
	dir := writeTestLocalTree(t, testLocalModTimeA)

	syncTestLocalDir(t, cm, dir, "repository", "a")
	first := readTestSavedCopies(t, cm, "repository")
	require.Len(t, first, 1)

	require.NoError(t, os.Chtimes(filepath.Join(dir, "Dockerfile"), testLocalModTimeB, testLocalModTimeB))
	syncTestLocalDir(t, cm, dir, "repository", "a")
	second := readTestSavedCopies(t, cm, "repository")
	require.Len(t, second, 1)

	// digests are not requested from the source that synced the copy last, so
	// the file with another modification time is written again
	assert.NotEqual(t, first[0].files["Dockerfile"].Inode, second[0].files["Dockerfile"].Inode)
	assert.Equal(t, testLocalModTimeB.UnixNano(), second[0].files["Dockerfile"].ModTime)
	assert.Equal(t, first[0].files["src/main.go"], second[0].files["src/main.go"])
}

func TestSnapshotComparesModTimesWithoutSource(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := writeTestLocalTree(t, testLocalModTimeA)
	dirB := writeTestLocalTree(t, testLocalModTimeB)

	syncTestLocalDir(t, cm, dirA, "repository", "")
	first := readTestSavedCopies(t, cm, "repository")
	require.Len(t, first, 1)

	syncTestLocalDir(t, cm, dirB, "repository", "")
	second := readTestSavedCopies(t, cm, "repository")
	require.Len(t, second, 1)

	assert.Empty(t, first[0].source)
	assert.Empty(t, second[0].source)
	assert.NotEqual(t, first[0].files["Dockerfile"].Inode, second[0].files["Dockerfile"].Inode)
	assert.NotEqual(t, first[0].files["src/main.go"].Inode, second[0].files["src/main.go"].Inode)
	assert.Equal(t, testLocalModTimeB.UnixNano(), second[0].files["Dockerfile"].ModTime)
}

// writeTestLocalTreeWithOtherDockerfile creates a directory like the one of
// writeTestLocalTree, with the same modification times, whose Dockerfile has
// another content of the same size.
func writeTestLocalTreeWithOtherDockerfile(t *testing.T, modTime time.Time) string {
	t.Helper()
	const content = "FROM busybox\n"
	require.Len(t, content, len(testLocalTree["Dockerfile"]))
	dir := writeTestLocalTree(t, modTime)
	p := filepath.Join(dir, "Dockerfile")
	require.NoError(t, os.WriteFile(p, []byte(content), 0600))
	require.NoError(t, os.Chtimes(p, modTime, modTime))
	return dir
}

// linkTestLocalDockerfile gives the Dockerfile of dir a second path,
// "hardlink".
func linkTestLocalDockerfile(t *testing.T, dir string) string {
	t.Helper()
	require.NoError(t, os.Link(filepath.Join(dir, "Dockerfile"), filepath.Join(dir, "hardlink")))
	return dir
}

// A file with two paths that another source sends with the same content is
// kept with both. The copy has the paths of the client and nothing else:
// fsutil does not link the second path again, which would leave a temporary
// link in the copy.
func TestSnapshotKeepsHardlinkForOtherSource(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := linkTestLocalDockerfile(t, writeTestLocalTree(t, testLocalModTimeA))
	dirB := linkTestLocalDockerfile(t, writeTestLocalTree(t, testLocalModTimeB))

	syncTestLocalDir(t, cm, dirA, "repository", "a")
	first := readTestSavedCopies(t, cm, "repository")
	require.Len(t, first, 1)
	require.Equal(t, first[0].files["Dockerfile"], first[0].files["hardlink"])

	syncTestLocalDir(t, cm, dirB, "repository", "b")
	second := readTestSavedCopies(t, cm, "repository")
	require.Len(t, second, 1)

	// same inodes and the same paths
	assert.Equal(t, withTestModTime(first[0].files, testLocalModTimeB), second[0].files)
}

// The same file comes from another source with another content, and the size
// and the modification time of the saved one. It is received, and its second
// path is the new file too.
func TestSnapshotReceivesHardlinkWithSameModTimeForOtherSource(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := linkTestLocalDockerfile(t, writeTestLocalTree(t, testLocalModTimeA))
	dirB := linkTestLocalDockerfile(t, writeTestLocalTreeWithOtherDockerfile(t, testLocalModTimeA))

	syncTestLocalDir(t, cm, dirA, "repository", "a")
	first := readTestSavedCopies(t, cm, "repository")
	require.Len(t, first, 1)

	syncTestLocalDir(t, cm, dirB, "repository", "b")
	second := readTestSavedCopies(t, cm, "repository")
	require.Len(t, second, 1)

	assert.Len(t, second[0].files, len(first[0].files))
	assert.Equal(t, "FROM busybox\n", second[0].files["Dockerfile"].Content)
	assert.Equal(t, testLocalModTimeA.UnixNano(), second[0].files["Dockerfile"].ModTime)
	assert.NotEqual(t, first[0].files["Dockerfile"].Inode, second[0].files["Dockerfile"].Inode)
	assert.Equal(t, second[0].files["Dockerfile"], second[0].files["hardlink"])
	assert.Equal(t, first[0].files["src/main.go"], second[0].files["src/main.go"])
}

// The modification times of a copy that another source synced prove nothing:
// a file with the saved size and modification time and another content is
// received, and the one that is the same is kept.
func TestSnapshotComparesContentOfFilesWithSameModTimeForOtherSource(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := writeTestLocalTree(t, testLocalModTimeA)
	dirB := writeTestLocalTreeWithOtherDockerfile(t, testLocalModTimeA)

	syncTestLocalDir(t, cm, dirA, "repository", "a")
	first := readTestSavedCopies(t, cm, "repository")
	require.Len(t, first, 1)
	require.Equal(t, "FROM scratch\n", first[0].files["Dockerfile"].Content)

	syncTestLocalDir(t, cm, dirB, "repository", "b")
	second := readTestSavedCopies(t, cm, "repository")
	require.Len(t, second, 1)

	assert.Equal(t, "b", second[0].source)
	assert.Equal(t, "FROM busybox\n", second[0].files["Dockerfile"].Content)
	assert.Equal(t, testLocalModTimeA.UnixNano(), second[0].files["Dockerfile"].ModTime)
	assert.NotEqual(t, first[0].files["Dockerfile"].Inode, second[0].files["Dockerfile"].Inode)
	assert.Equal(t, first[0].files["src/main.go"], second[0].files["src/main.go"])
}

// For the source that synced the copy last, and for a build that names none,
// the rule is the one of upstream: size and modification time decide.
func TestSnapshotTrustsModTimesOfSameSource(t *testing.T) {
	tests := []struct {
		name   string
		source string
	}{
		{name: "same source", source: "a"},
		{name: "no source", source: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := newTestCacheManager(t)
			dirA := writeTestLocalTree(t, testLocalModTimeA)
			dirB := writeTestLocalTreeWithOtherDockerfile(t, testLocalModTimeA)

			syncTestLocalDir(t, cm, dirA, "repository", "a")
			first := readTestSavedCopies(t, cm, "repository")
			require.Len(t, first, 1)

			syncTestLocalDir(t, cm, dirB, "repository", tt.source)
			second := readTestSavedCopies(t, cm, "repository")
			require.Len(t, second, 1)

			assert.Equal(t, first[0].files, second[0].files)
		})
	}
}

// A build that names no source compares modification times, as upstream. The
// times that it leaves in the copy are not the ones of the recorded source
// any more, so the record is removed.
func TestSnapshotRemovesSourceWhenClientHasNone(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := writeTestLocalTree(t, testLocalModTimeA)
	dirB := writeTestLocalTree(t, testLocalModTimeB)

	syncTestLocalDir(t, cm, dirA, "repository", "a")
	first := readTestSavedCopies(t, cm, "repository")
	require.Len(t, first, 1)
	require.Equal(t, "a", first[0].source)

	syncTestLocalDir(t, cm, dirB, "repository", "")
	second := readTestSavedCopies(t, cm, "repository")
	require.Len(t, second, 1)

	assert.Empty(t, second[0].source)
	assert.NotEqual(t, first[0].files["Dockerfile"].Inode, second[0].files["Dockerfile"].Inode)
	assert.Equal(t, testLocalModTimeB.UnixNano(), second[0].files["Dockerfile"].ModTime)
}

// The source that synced a copy comes back after a build without a source
// synced another directory. The modification times of the copy are the ones
// of that directory, so the files are compared by content: the ones that are
// the same are kept, not sent again.
func TestSnapshotComparesContentAfterSyncWithoutSource(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := writeTestLocalTree(t, testLocalModTimeA)
	dirB := writeTestLocalTree(t, testLocalModTimeB)

	syncTestLocalDir(t, cm, dirA, "repository", "a")
	syncTestLocalDir(t, cm, dirB, "repository", "")
	second := readTestSavedCopies(t, cm, "repository")
	require.Len(t, second, 1)

	syncTestLocalDir(t, cm, dirA, "repository", "a")
	third := readTestSavedCopies(t, cm, "repository")
	require.Len(t, third, 1)

	assert.Equal(t, "a", third[0].source)
	// same inodes: compared by content
	assert.Equal(t, withTestModTime(second[0].files, testLocalModTimeA), third[0].files)
}

// The same, when the directory that was synced without a source has a file
// with another content of the same size, and the recorded source comes back
// with the modification times that the copy has now (checkouts whose times
// are normalised or restored). The record would make the daemon keep the file
// of the other directory.
func TestSnapshotReceivesFileWithSameModTimeAfterSyncWithoutSource(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := writeTestLocalTree(t, testLocalModTimeA)
	dirB := writeTestLocalTreeWithOtherDockerfile(t, testLocalModTimeB)

	syncTestLocalDir(t, cm, dirA, "repository", "a")
	syncTestLocalDir(t, cm, dirB, "repository", "")
	second := readTestSavedCopies(t, cm, "repository")
	require.Len(t, second, 1)
	require.Equal(t, "FROM busybox\n", second[0].files["Dockerfile"].Content)

	for name := range testLocalTree {
		p := filepath.Join(dirA, filepath.FromSlash(name))
		require.NoError(t, os.Chtimes(p, testLocalModTimeB, testLocalModTimeB))
	}
	syncTestLocalDir(t, cm, dirA, "repository", "a")
	third := readTestSavedCopies(t, cm, "repository")
	require.Len(t, third, 1)

	assert.Equal(t, "a", third[0].source)
	assert.Equal(t, "FROM scratch\n", third[0].files["Dockerfile"].Content)
	assert.Equal(t, testLocalModTimeB.UnixNano(), third[0].files["Dockerfile"].ModTime)
	assert.NotEqual(t, second[0].files["Dockerfile"].Inode, third[0].files["Dockerfile"].Inode)
	assert.Equal(t, second[0].files["src/main.go"], third[0].files["src/main.go"])
}

// A step can be shared with the build of another client. The source that the
// other client named does not opt in the client that sends the files: it did
// not send the option, so its files are compared by modification time and the
// copy ends without a recorded source.
func TestSnapshotIgnoresSourceOfAnotherSession(t *testing.T) {
	tests := []struct {
		name       string
		anySession bool
	}{
		{name: "session of the local source"},
		{name: "any session of the step", anySession: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := newTestCacheManager(t)
			dirA := writeTestLocalTree(t, testLocalModTimeA)
			dirB := writeTestLocalTree(t, testLocalModTimeB)

			syncTestLocalDir(t, cm, dirA, "repository", "a")
			first := readTestSavedCopies(t, cm, "repository")
			require.Len(t, first, 1)

			syncTestLocalBuild(t, cm, testLocalBuild{
				dir:        dirB,
				sharedKey:  "repository",
				others:     []testOtherBuild{{sessionID: "another-session", source: "b"}},
				anySession: tt.anySession,
			})
			second := readTestSavedCopies(t, cm, "repository")
			require.Len(t, second, 1)

			assert.Empty(t, second[0].source)
			assert.NotEqual(t, first[0].files["Dockerfile"].Inode, second[0].files["Dockerfile"].Inode)
			assert.NotEqual(t, first[0].files["src/main.go"].Inode, second[0].files["src/main.go"].Inode)
			assert.Equal(t, testLocalModTimeB.UnixNano(), second[0].files["Dockerfile"].ModTime)
		})
	}
}

// The other way around: a client that named a source keeps it when the step
// is shared with builds of other clients, whatever they sent.
func TestSnapshotUsesSourceOfItsSession(t *testing.T) {
	tests := []struct {
		name       string
		others     []testOtherBuild
		anySession bool
	}{
		{name: "another client without source", others: []testOtherBuild{{sessionID: "another-session"}}},
		{name: "another client with another source", others: []testOtherBuild{{sessionID: "another-session", source: "c"}}},
		{name: "any session of the step", anySession: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := newTestCacheManager(t)
			dirA := writeTestLocalTree(t, testLocalModTimeA)
			dirB := writeTestLocalTree(t, testLocalModTimeB)

			syncTestLocalDir(t, cm, dirA, "repository", "a")
			first := readTestSavedCopies(t, cm, "repository")
			require.Len(t, first, 1)

			syncTestLocalBuild(t, cm, testLocalBuild{
				dir:        dirB,
				sharedKey:  "repository",
				source:     "b",
				others:     tt.others,
				anySession: tt.anySession,
			})
			second := readTestSavedCopies(t, cm, "repository")
			require.Len(t, second, 1)

			assert.Equal(t, "b", second[0].source)
			// same inodes: compared by content
			assert.Equal(t, withTestModTime(first[0].files, testLocalModTimeB), second[0].files)
		})
	}
}

// Builds of the same client share the step: all of them have to name the same
// source, or there is none. The files are then compared by modification time
// and the copy ends without a recorded source.
func TestSnapshotNeedsEveryBuildOfTheSessionToNameTheSource(t *testing.T) {
	tests := []struct {
		name            string
		sameSession     []string
		prefixedSession []string
	}{
		{name: "one without source", sameSession: []string{""}},
		{name: "one with another source", sameSession: []string{"c"}},
		{name: "one of two without source", sameSession: []string{"b", ""}},
		// "prefix:id" is the session "id" for the session manager
		{name: "one without source, its session written with a prefix", prefixedSession: []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := newTestCacheManager(t)
			dirA := writeTestLocalTree(t, testLocalModTimeA)
			dirB := writeTestLocalTree(t, testLocalModTimeB)

			syncTestLocalDir(t, cm, dirA, "repository", "a")
			first := readTestSavedCopies(t, cm, "repository")
			require.Len(t, first, 1)

			syncTestLocalBuild(t, cm, testLocalBuild{dir: dirB, sharedKey: "repository", source: "b", sameSession: tt.sameSession, prefixedSession: tt.prefixedSession})
			second := readTestSavedCopies(t, cm, "repository")
			require.Len(t, second, 1)

			assert.Empty(t, second[0].source)
			assert.NotEqual(t, first[0].files["Dockerfile"].Inode, second[0].files["Dockerfile"].Inode)
			assert.Equal(t, testLocalModTimeB.UnixNano(), second[0].files["Dockerfile"].ModTime)
		})
	}
}

// Builds of the same client that agree on the source keep it.
func TestSnapshotUsesSourceThatEveryBuildOfTheSessionNamed(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := writeTestLocalTree(t, testLocalModTimeA)
	dirB := writeTestLocalTree(t, testLocalModTimeB)

	syncTestLocalDir(t, cm, dirA, "repository", "a")
	first := readTestSavedCopies(t, cm, "repository")
	require.Len(t, first, 1)

	syncTestLocalBuild(t, cm, testLocalBuild{dir: dirB, sharedKey: "repository", source: "b", sameSession: []string{"b"}})
	second := readTestSavedCopies(t, cm, "repository")
	require.Len(t, second, 1)

	assert.Equal(t, "b", second[0].source)
	assert.Equal(t, withTestModTime(first[0].files, testLocalModTimeB), second[0].files)
}

// The session shared key is used as it is, whatever it contains.
func TestSnapshotUsesWholeSharedKey(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := writeTestLocalTree(t, testLocalModTimeA)
	dirB := writeTestLocalTree(t, testLocalModTimeB)

	syncTestLocalDir(t, cm, dirA, "repository;source=a", "")
	syncTestLocalDir(t, cm, dirB, "repository;source=b", "")

	assert.Empty(t, readTestSavedCopies(t, cm, "repository"))
	copiesA := readTestSavedCopies(t, cm, "repository;source=a")
	require.Len(t, copiesA, 1)
	assert.Empty(t, copiesA[0].source)
	assert.Equal(t, testLocalModTimeA.UnixNano(), copiesA[0].files["Dockerfile"].ModTime)
	copiesB := readTestSavedCopies(t, cm, "repository;source=b")
	require.Len(t, copiesB, 1)
	assert.Empty(t, copiesB[0].source)
	assert.Equal(t, testLocalModTimeB.UnixNano(), copiesB[0].files["Dockerfile"].ModTime)
}

// A transfer that does not compare modification times can't compare content
// instead, so the source of the build is not used for it.
func TestSnapshotIgnoresSourceWithoutMetadataDiffer(t *testing.T) {
	cm := newTestCacheManager(t)
	dir := writeTestLocalTree(t, testLocalModTimeA)

	ref := snapshotTestLocal(t, cm, LocalIdentifier{Name: testLocalName, Differ: fsutil.DiffNone}, dir, "repository", "a")
	require.NoError(t, ref.Release(t.Context()))

	copies := readTestSavedCopies(t, cm, "repository")
	require.Len(t, copies, 1)
	assert.Empty(t, copies[0].source)
}

func TestSnapshotPrefersCopyOfSameSource(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := writeTestLocalTree(t, testLocalModTimeA)
	dirB := writeTestLocalTree(t, testLocalModTimeB)

	// the copy of a is in use while b syncs, so b gets a copy of its own
	refA := snapshotTestLocalDir(t, cm, dirA, "repository", "a")
	refB := snapshotTestLocalDir(t, cm, dirB, "repository", "b")
	require.NoError(t, refA.Release(t.Context()))
	require.NoError(t, refB.Release(t.Context()))
	initial := readTestSavedCopies(t, cm, "repository")
	require.ElementsMatch(t, []string{"a", "b"}, testSavedCopySources(initial))

	syncTestLocalDir(t, cm, dirA, "repository", "a")
	afterA := readTestSavedCopies(t, cm, "repository")
	syncTestLocalDir(t, cm, dirB, "repository", "b")
	afterB := readTestSavedCopies(t, cm, "repository")

	assert.ElementsMatch(t, initial, afterA)
	assert.ElementsMatch(t, initial, afterB)
}

func TestCompareSyncSource(t *testing.T) {
	tests := []struct {
		name     string
		a        string
		b        string
		expected int
	}{
		{name: "only the first is the source", a: "source", b: "other", expected: -1},
		{name: "only the second is the source", a: "other", b: "source", expected: 1},
		{name: "both are the source", a: "source", b: "source", expected: 0},
		{name: "none is the source", a: "other", b: "", expected: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, compareSyncSource(tt.a, tt.b, "source"))
		})
	}
}

// testOpenHookFS are the files of a client that runs hook when the content of
// path is read for the nth time.
type testOpenHookFS struct {
	fsutil.FS
	path  string
	nth   int32
	hook  func()
	opens atomic.Int32
}

func (f *testOpenHookFS) Open(p string) (io.ReadCloser, error) {
	if filepath.ToSlash(p) == f.path && f.opens.Add(1) == f.nth {
		f.hook()
	}
	return f.FS.Open(p)
}

// readTestCompleteSyncs returns, for every copy of the local source that is
// stored with the given key, whether it is recorded as the result of a sync
// that finished. It does not take the copies, so it can be called during a
// sync.
func readTestCompleteSyncs(ctx context.Context, cm cache.Manager, key string) ([]bool, error) {
	sis, err := searchSharedKey(ctx, cm, testLocalName+"::"+key)
	if err != nil {
		return nil, err
	}
	complete := make([]bool, 0, len(sis))
	for _, si := range sis {
		complete = append(complete, si.hasCompleteSync())
	}
	return complete, nil
}

// changeTestCopies applies change to the metadata of the copies of the local
// source that source synced last.
func changeTestCopies(t *testing.T, cm cache.Manager, key, source string, change func(cacheRefMetadata) error) {
	t.Helper()
	sis, err := searchSharedKey(t.Context(), cm, testLocalName+"::"+key)
	require.NoError(t, err)
	for _, si := range sis {
		if si.getSyncSource() == source {
			require.NoError(t, change(si))
		}
	}
}

// leaveTestSyncUnfinished leaves the metadata of a copy as a daemon that dies
// during a sync of it leaves it.
func leaveTestSyncUnfinished(md cacheRefMetadata) error {
	return md.setCompleteSync(false)
}

// waitForTestSavedCopies waits until n copies of the local source are stored
// with the given key: a copy that is not reused is removed in the background.
func waitForTestSavedCopies(t *testing.T, cm cache.Manager, key string, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		sis, err := searchSharedKey(t.Context(), cm, testLocalName+"::"+key)
		return err == nil && len(sis) == n
	}, 10*time.Second, 10*time.Millisecond)
}

func TestSnapshotCopyIsNotCompleteDuringSync(t *testing.T) {
	cm := newTestCacheManager(t)
	ctx := t.Context()
	dir := writeTestLocalTree(t, testLocalModTimeA)

	syncTestLocalDir(t, cm, dir, "repository", "")
	before, err := readTestCompleteSyncs(ctx, cm, "repository")
	require.NoError(t, err)

	// the content of the file that changed is read in the middle of the sync
	require.NoError(t, os.Chtimes(filepath.Join(dir, "Dockerfile"), testLocalModTimeB, testLocalModTimeB))
	dirFS, err := fsutil.NewFS(dir)
	require.NoError(t, err)
	var (
		during    []bool
		duringErr error
	)
	syncTestLocalBuild(t, cm, testLocalBuild{
		sharedKey: "repository",
		fs: &testOpenHookFS{FS: dirFS, path: "Dockerfile", nth: 1, hook: func() {
			during, duringErr = readTestCompleteSyncs(ctx, cm, "repository")
		}},
	})
	after, err := readTestCompleteSyncs(ctx, cm, "repository")
	require.NoError(t, err)

	require.NoError(t, duringErr)
	assert.Equal(t, []bool{true}, before)
	assert.Equal(t, []bool{false}, during)
	assert.Equal(t, []bool{true}, after)
}

// testMetadataWrites are the writes to the metadata of the copies of a cache
// manager, in the order in which they happen.
type testMetadataWrites struct {
	mu     sync.Mutex
	events []string
}

func (w *testMetadataWrites) add(event string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events = append(w.events, event)
}

func (w *testMetadataWrites) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	events := w.events
	w.events = nil
	return events
}

// testRecordingManager is a cache manager whose copies record the writes to
// their metadata.
type testRecordingManager struct {
	cache.Manager
	writes *testMetadataWrites
}

func (m testRecordingManager) New(ctx context.Context, parent cache.ImmutableRef, s session.Group, opts ...cache.RefOption) (cache.MutableRef, error) {
	ref, err := m.Manager.New(ctx, parent, s, opts...)
	if err != nil {
		return nil, err
	}
	return &testRecordingRef{MutableRef: ref, writes: m.writes}, nil
}

func (m testRecordingManager) GetMutable(ctx context.Context, id string, opts ...cache.RefOption) (cache.MutableRef, error) {
	ref, err := m.Manager.GetMutable(ctx, id, opts...)
	if err != nil {
		return nil, err
	}
	return &testRecordingRef{MutableRef: ref, writes: m.writes}, nil
}

type testRecordingRef struct {
	cache.MutableRef
	writes *testMetadataWrites
}

func (r *testRecordingRef) SetString(key, value, index string) error {
	r.writes.add(key + "=" + value)
	return r.MutableRef.SetString(key, value, index)
}

func (r *testRecordingRef) SetExternal(key string, dt []byte) error {
	r.writes.add(key)
	return r.MutableRef.SetExternal(key, dt)
}

// TestSnapshotOrderOfMetadataWrites checks when a copy stops being complete
// and when it is complete again. Before the transfer, because the transfer
// changes the files. After the content hashes and the source, because a copy
// that is complete is trusted to have the ones of its files.
func TestSnapshotOrderOfMetadataWrites(t *testing.T) {
	const contentHashes = "buildkit.contenthash.v0"
	writes := &testMetadataWrites{}
	cm := testRecordingManager{Manager: newTestCacheManager(t), writes: writes}
	dir := writeTestLocalTree(t, testLocalModTimeA)

	syncTestLocalDir(t, cm, dir, "repository", "a")
	first := writes.take()

	// another source syncs the copy and a file is written again
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine\n"), 0600))
	dirFS, err := fsutil.NewFS(dir)
	require.NoError(t, err)
	syncTestLocalBuild(t, cm, testLocalBuild{
		sharedKey: "repository",
		source:    "b",
		fs: &testOpenHookFS{FS: dirFS, path: "Dockerfile", nth: 2, hook: func() {
			writes.add("transfer")
		}},
	})
	second := writes.take()

	// a new copy has no record to remove before its first transfer
	assert.Equal(t, []string{
		contentHashes,
		keySharedKey + "=" + testLocalName + "::repository",
		keySyncSource + "=a",
		keyCompleteSync + "=true",
	}, first)
	assert.Equal(t, []string{
		keyCompleteSync + "=",
		"transfer",
		contentHashes,
		keySyncSource + "=b",
		keyCompleteSync + "=true",
	}, second)
}

func TestSnapshotReplacesCopyThatIsNotComplete(t *testing.T) {
	tests := []struct {
		name   string
		change func(cacheRefMetadata) error
	}{
		{
			name:   "a sync of the copy did not finish",
			change: leaveTestSyncUnfinished,
		},
		{
			name: "the copy was synced by a daemon that does not record it",
			change: func(md cacheRefMetadata) error {
				return md.ClearValueAndIndex(keyCompleteSync, "")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cm := newTestCacheManager(t)
			dir := writeTestLocalTree(t, testLocalModTimeA)

			syncTestLocalDir(t, cm, dir, "repository", "a")
			first := readTestSavedCopies(t, cm, "repository")
			require.Len(t, first, 1)
			changeTestCopies(t, cm, "repository", "a", tt.change)

			syncTestLocalDir(t, cm, dir, "repository", "a")
			waitForTestSavedCopies(t, cm, "repository", 1)
			second := readTestSavedCopies(t, cm, "repository")
			require.Len(t, second, 1)
			complete, err := readTestCompleteSyncs(t.Context(), cm, "repository")
			require.NoError(t, err)

			// the source and the modification times are the ones of the
			// saved copy, and the sync still fills another copy
			assert.NotEqual(t, first[0].id, second[0].id)
			assert.Equal(t, "a", second[0].source)
			assert.Len(t, second[0].files, len(testLocalTree))
			assert.Equal(t, []bool{true}, complete)
		})
	}
}

func TestSnapshotReusesAnotherCopyWhenOneIsNotComplete(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := writeTestLocalTree(t, testLocalModTimeA)
	dirB := writeTestLocalTree(t, testLocalModTimeB)

	// the copy of a is in use while b syncs, so b gets a copy of its own
	refA := snapshotTestLocalDir(t, cm, dirA, "repository", "a")
	refB := snapshotTestLocalDir(t, cm, dirB, "repository", "b")
	require.NoError(t, refA.Release(t.Context()))
	require.NoError(t, refB.Release(t.Context()))
	initial := readTestSavedCopies(t, cm, "repository")
	require.ElementsMatch(t, []string{"a", "b"}, testSavedCopySources(initial))
	copyB := initial[slices.IndexFunc(initial, func(c testSavedCopy) bool { return c.source == "b" })]
	changeTestCopies(t, cm, "repository", "a", leaveTestSyncUnfinished)

	syncTestLocalDir(t, cm, dirA, "repository", "a")
	waitForTestSavedCopies(t, cm, "repository", 1)
	final := readTestSavedCopies(t, cm, "repository")

	// a syncs the copy of b: its files are compared by content and stay
	assert.Equal(t, []testSavedCopy{{id: copyB.id, source: "a", files: withTestModTime(copyB.files, testLocalModTimeA)}}, final)
}

const (
	// testKilledSyncEnv has the testKilledSync of the process that is killed
	testKilledSyncEnv = "BUILDKIT_TEST_KILLED_SYNC"
	// testKilledSyncLastFile is the file whose content the daemon never gets
	testKilledSyncLastFile = "zzz/last"
)

var (
	// testKilledSyncOld is the directory in the first sync, and
	// testKilledSyncNew in the sync that is killed and in the next one: a file
	// has other content, a file is gone and the last one has another size.
	testKilledSyncOld = map[string]string{
		"aaa/changed":          "old\n",
		"aaa/removed":          "removed\n",
		"aaa/same":             "same\n",
		testKilledSyncLastFile: "old, last\n",
	}
	testKilledSyncNew = map[string]string{
		"aaa/changed":          "new\n",
		"aaa/same":             "same\n",
		testKilledSyncLastFile: "new, last, with another size\n",
	}
	testKilledSyncChanged = map[string]string{
		"aaa/changed":          testKilledSyncNew["aaa/changed"],
		testKilledSyncLastFile: testKilledSyncNew[testKilledSyncLastFile],
	}
)

// testKilledSync is what the process that is killed does: a daemon on Root
// that syncs FirstDir and then KilledDir, where it stops for ever when the
// client reads the last file for the StopAtOpen time. It creates the file
// Reached at that moment.
type testKilledSync struct {
	Root         string
	FirstDir     string
	FirstSource  string
	KilledDir    string
	KilledSource string
	StopAtOpen   int32
	Reached      string
}

// writeTestKilledSyncNew creates a directory with testKilledSyncNew. The
// files that are also in testKilledSyncOld with the same content have the
// given modification time, and the ones that changed a later one.
func writeTestKilledSyncNew(t *testing.T, modTime time.Time) string {
	t.Helper()
	dir := t.TempDir()
	writeTestFiles(t, dir, testKilledSyncNew, modTime)
	writeTestFiles(t, dir, testKilledSyncChanged, modTime.Add(time.Hour))
	return dir
}

// testKilledSyncIsHalfway reports whether the daemon of the process that is
// going to be killed has the file that changed, with its content and its
// modification time, does not have the one that is gone, and waits for the
// content of the last one.
func testKilledSyncIsHalfway(c testKilledSync, modTime time.Time) bool {
	if _, err := os.Stat(c.Reached); err != nil {
		return false
	}
	// the native snapshotter keeps the files of the copy in a directory of its root
	dirs, err := filepath.Glob(filepath.Join(c.Root, "snapshots", "snapshots", "*", "aaa"))
	if err != nil {
		return false
	}
	return slices.ContainsFunc(dirs, func(dir string) bool {
		content, err := os.ReadFile(filepath.Join(dir, "changed"))
		if err != nil || string(content) != testKilledSyncNew["aaa/changed"] {
			return false
		}
		fi, err := os.Stat(filepath.Join(dir, "changed"))
		if err != nil || !fi.ModTime().Equal(modTime.Add(time.Hour)) {
			return false
		}
		_, err = os.Stat(filepath.Join(dir, "removed"))
		return os.IsNotExist(err)
	})
}

// killTestSync runs the process of c and kills it when it is in the middle of
// its second sync, as a daemon that runs out of memory is killed.
func killTestSync(t *testing.T, c testKilledSync, modTime time.Time) {
	t.Helper()
	config, err := json.Marshal(c)
	require.NoError(t, err)

	var output bytes.Buffer
	// the timeout ends the process if nobody is left to kill it
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSnapshotKilledSyncProcess$", "-test.count=1", "-test.v", "-test.timeout=5m") //nolint:gosec // the test binary runs itself
	cmd.Env = append(os.Environ(), testKilledSyncEnv+"="+string(config))
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Start())
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.Now().Add(2 * time.Minute)
	for !testKilledSyncIsHalfway(c, modTime) {
		select {
		case err := <-exited:
			require.FailNowf(t, "the process ended before it was killed", "%v\n%s", err, output.String())
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			require.NoError(t, cmd.Process.Kill())
			<-exited
			require.FailNowf(t, "the process did not get to the middle of the sync", "%s", output.String())
		}
	}
	require.NoError(t, cmd.Process.Kill())
	<-exited
}

// TestSnapshotKilledSyncProcess is the process that TestSnapshotAfterKilledSync
// kills. It does nothing when the tests of the package run.
func TestSnapshotKilledSyncProcess(t *testing.T) {
	config, ok := os.LookupEnv(testKilledSyncEnv)
	if !ok {
		t.Skip("it only runs as the process that TestSnapshotAfterKilledSync kills")
	}
	var c testKilledSync
	require.NoError(t, json.Unmarshal([]byte(config), &c))

	cm := newTestCacheManagerAt(t, c.Root)
	syncTestLocalDir(t, cm, c.FirstDir, "repository", c.FirstSource)

	killedFS, err := fsutil.NewFS(c.KilledDir)
	require.NoError(t, err)
	syncTestLocalBuild(t, cm, testLocalBuild{
		sharedKey: "repository",
		source:    c.KilledSource,
		fs: &testOpenHookFS{FS: killedFS, path: testKilledSyncLastFile, nth: c.StopAtOpen, hook: func() {
			_ = os.WriteFile(c.Reached, nil, 0600)
			<-t.Context().Done()
		}},
	})
	t.Fatal("the sync finished: the process had to be killed in the middle of it")
}

// readTestChecksums returns the content hashes that the cache has for the
// paths of a local source, empty for the paths that it has none for.
func readTestChecksums(t *testing.T, ref cache.ImmutableRef, paths ...string) map[string]string {
	t.Helper()
	sums := make(map[string]string, len(paths))
	for _, p := range paths {
		sums[p] = ""
		if dgst, err := contenthash.Checksum(t.Context(), ref, p, contenthash.ChecksumOpts{}, nil); err == nil {
			sums[p] = dgst.String()
		}
	}
	return sums
}

// TestSnapshotAfterKilledSync checks the content hashes of a build that comes
// after a daemon was killed in the middle of a sync. They are what the build
// steps that read the files are cached by, so they have to be the ones of the
// files that the build gets.
func TestSnapshotAfterKilledSync(t *testing.T) {
	const (
		// the files are compared by modification time: the daemon only asks
		// the client for the content of the last file
		readForContent = 1
		// the files are compared by content: the client reads the last file
		// to hash it before the daemon asks for its content
		readForDigestAndContent = 2
	)
	tests := []struct {
		name         string
		firstSource  string
		killedSource string
		stopAtOpen   int32
		// killedModTime and nextModTime are the modification times of the
		// files that did not change in the directory of the sync that is
		// killed and in the one of the next sync
		killedModTime time.Time
		nextSource    string
		nextModTime   time.Time
	}{
		{
			name:          "the same directory syncs again, without source",
			stopAtOpen:    readForContent,
			killedModTime: testLocalModTimeA,
			nextModTime:   testLocalModTimeA,
		},
		{
			name:          "the same directory syncs again, with its source",
			firstSource:   "a",
			killedSource:  "a",
			stopAtOpen:    readForContent,
			killedModTime: testLocalModTimeA,
			nextSource:    "a",
			nextModTime:   testLocalModTimeA,
		},
		{
			name:          "another clone is killed and syncs again",
			firstSource:   "a",
			killedSource:  "b",
			stopAtOpen:    readForDigestAndContent,
			killedModTime: testLocalModTimeB,
			nextSource:    "b",
			nextModTime:   testLocalModTimeB,
		},
		{
			name:          "another clone is killed and a third one syncs next",
			firstSource:   "a",
			killedSource:  "b",
			stopAtOpen:    readForDigestAndContent,
			killedModTime: testLocalModTimeB,
			nextSource:    "c",
			nextModTime:   testLocalModTimeB.Add(24 * time.Hour),
		},
	}
	paths := []string{"/", "aaa", "aaa/changed", "aaa/removed", "aaa/same", testKilledSyncLastFile}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			firstDir := t.TempDir()
			writeTestFiles(t, firstDir, testKilledSyncOld, testLocalModTimeA)
			nextDir := writeTestKilledSyncNew(t, tt.nextModTime)
			c := testKilledSync{
				Root:         t.TempDir(),
				FirstDir:     firstDir,
				FirstSource:  tt.firstSource,
				KilledDir:    writeTestKilledSyncNew(t, tt.killedModTime),
				KilledSource: tt.killedSource,
				StopAtOpen:   tt.stopAtOpen,
				Reached:      filepath.Join(t.TempDir(), "reached"),
			}
			killTestSync(t, c, tt.killedModTime)

			// a daemon starts on the state of the one that was killed
			cm := newTestCacheManagerAt(t, c.Root)
			ref := snapshotTestLocalDir(t, cm, nextDir, "repository", tt.nextSource)
			sums := readTestChecksums(t, ref, paths...)
			require.NoError(t, ref.Release(ctx))
			waitForTestSavedCopies(t, cm, "repository", 1)
			copies := readTestSavedCopies(t, cm, "repository")
			require.Len(t, copies, 1)
			complete, err := readTestCompleteSyncs(ctx, cm, "repository")
			require.NoError(t, err)

			// what a daemon without state has for the same directory
			plainRef := snapshotTestLocalDir(t, newTestCacheManager(t), nextDir, "repository", "")
			plainSums := readTestChecksums(t, plainRef, paths...)
			require.NoError(t, plainRef.Release(ctx))

			content := make(map[string]string, len(copies[0].files))
			for name, f := range copies[0].files {
				content[name] = f.Content
			}
			assert.Equal(t, testKilledSyncNew, content)
			assert.Equal(t, plainSums, sums)
			assert.Empty(t, sums["aaa/removed"])
			assert.NotEmpty(t, sums["aaa/changed"])
			assert.Equal(t, tt.nextSource, copies[0].source)
			assert.Equal(t, []bool{true}, complete)
		})
	}
}
