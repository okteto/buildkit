//go:build !windows

package local

import (
	gofs "io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	tmpdir := t.TempDir()

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
	for name, content := range testLocalTree {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0700))
		require.NoError(t, os.WriteFile(p, []byte(content), 0600))
		require.NoError(t, os.Chtimes(p, modTime, modTime))
	}
	return dir
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

	dirFS, err := fsutil.NewFS(b.dir)
	require.NoError(t, err)
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

// A build that names no source does nothing new: it compares modification
// times and leaves the record of the copy as it was.
func TestSnapshotKeepsSourceWhenClientHasNone(t *testing.T) {
	cm := newTestCacheManager(t)
	dirA := writeTestLocalTree(t, testLocalModTimeA)
	dirB := writeTestLocalTree(t, testLocalModTimeB)

	syncTestLocalDir(t, cm, dirA, "repository", "a")
	first := readTestSavedCopies(t, cm, "repository")
	require.Len(t, first, 1)

	syncTestLocalDir(t, cm, dirB, "repository", "")
	second := readTestSavedCopies(t, cm, "repository")
	require.Len(t, second, 1)

	assert.Equal(t, "a", second[0].source)
	assert.NotEqual(t, first[0].files["Dockerfile"].Inode, second[0].files["Dockerfile"].Inode)
	assert.Equal(t, testLocalModTimeB.UnixNano(), second[0].files["Dockerfile"].ModTime)
}

// A step can be shared with the build of another client. The source that the
// other client named does not opt in the client that sends the files: it did
// not send the option, so its files are compared by modification time and the
// record of the copy stays as it was.
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

			assert.Equal(t, "a", second[0].source)
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
// source, or there is none.
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

			assert.Equal(t, "a", second[0].source)
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
