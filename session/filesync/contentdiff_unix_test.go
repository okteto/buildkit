//go:build !windows

package filesync

import (
	"maps"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tonistiigi/fsutil"
	fstypes "github.com/tonistiigi/fsutil/types"
)

func TestContentDiffAppliesModTimeWhenModeChanges(t *testing.T) {
	dirA := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeB)
	script := filepath.Join(dirB, "src", "main.go")
	require.NoError(t, os.Chmod(script, 0700))
	require.NoError(t, os.Chtimes(script, contentDiffModTimeB, contentDiffModTimeB))
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Equal(t, []string{"src/main.go"}, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)
}

func TestContentDiffKeepsSymlinksAndHardlinks(t *testing.T) {
	dirA := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeB)
	for _, dir := range []string{dirA, dirB} {
		require.NoError(t, os.Symlink("Dockerfile", filepath.Join(dir, "symlink")))
		require.NoError(t, os.Link(filepath.Join(dir, "Dockerfile"), filepath.Join(dir, "hardlink")))
		require.NoError(t, os.Link(filepath.Join(dir, "Dockerfile"), filepath.Join(dir, "src", "hardlink")))
	}
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})

	// the hardlink is kept with its target. If fsutil linked it again, to the
	// file that it already is, the temporary link would stay in dest
	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Empty(t, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)

	target, err := os.Readlink(filepath.Join(dest, "symlink"))
	require.NoError(t, err)
	assert.Equal(t, "Dockerfile", target)
	requireContentDiffHardlinks(t, dest, "Dockerfile", "hardlink", "src/hardlink")
}

// requireContentDiffHardlinks checks that the paths of dir are one file.
func requireContentDiffHardlinks(t *testing.T, dir string, names ...string) {
	t.Helper()
	first, err := os.Stat(filepath.Join(dir, filepath.FromSlash(names[0])))
	require.NoError(t, err)
	for _, name := range names[1:] {
		fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		require.NoError(t, err)
		assert.True(t, os.SameFile(first, fi), "%s and %s are not the same file", names[0], name)
	}
}

// contentDiffSameTimeHardlinkTrees returns two directories where "Dockerfile"
// has the hardlinks "hardlink" and "src/hardlink", with the same modification
// times in both. The file has another content of the same size in the second
// one.
func contentDiffSameTimeHardlinkTrees(t *testing.T) (string, string) {
	t.Helper()
	other := maps.Clone(contentDiffTree)
	other["Dockerfile"] = "FROM busybox\n"
	require.Len(t, other["Dockerfile"], len(contentDiffTree["Dockerfile"]))

	dirA := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, other, contentDiffModTimeA)
	for _, dir := range []string{dirA, dirB} {
		// a link does not change the modification time of the file
		require.NoError(t, os.Link(filepath.Join(dir, "Dockerfile"), filepath.Join(dir, "hardlink")))
		require.NoError(t, os.Link(filepath.Join(dir, "Dockerfile"), filepath.Join(dir, "src", "hardlink")))
	}
	return dirA, dirB
}

// A file with hardlinks comes from another directory with another content and
// the saved size and modification time. fsutil writes the file as a new one,
// and the hardlinks have to follow it: with the saved modification time
// fsutil would keep them, with the content that was replaced.
func TestContentDiffLinksHardlinksToRequestedTarget(t *testing.T) {
	dirA, dirB := contentDiffSameTimeHardlinkTrees(t)
	dest := t.TempDir()
	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})

	assert.Equal(t, []string{"Dockerfile", "hardlink", "src/hardlink"}, second.writtenFiles())
	// the content of the second directory in the three paths
	requireSameContentDiffTree(t, dirB, dest)
	requireContentDiffHardlinks(t, dest, "Dockerfile", "hardlink", "src/hardlink")
}

// The rule of fsutil for the same directories, which is the one of a transfer
// that does not compare content: the file and its hardlinks stay as saved.
func TestSyncWithoutContentDiffKeepsHardlinksWithSameModTimeAndOtherContent(t *testing.T) {
	dirA, dirB := contentDiffSameTimeHardlinkTrees(t)
	dest := t.TempDir()
	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{})

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{})

	assert.Empty(t, second.writtenFiles())
	requireSameContentDiffTree(t, dirA, dest)
	requireContentDiffHardlinks(t, dest, "Dockerfile", "hardlink", "src/hardlink")
}

// The saved copy has two files with the same content, and for the client they
// are one file with two paths: the first one is kept and fsutil links the
// second one to it.
func TestContentDiffLinksFileThatIsAHardlinkOfKeptTarget(t *testing.T) {
	separate := maps.Clone(contentDiffTree)
	separate["hardlink"] = separate["Dockerfile"]
	dirA := writeContentDiffTree(t, separate, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeB)
	require.NoError(t, os.Link(filepath.Join(dirB, "Dockerfile"), filepath.Join(dirB, "hardlink")))
	dest := t.TempDir()
	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})

	assert.Equal(t, []string{"hardlink"}, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)
	requireContentDiffHardlinks(t, dest, "Dockerfile", "hardlink")
}

// The other way around: the saved copy has one file with two paths, and for
// the client they are two files with the same content. The second path
// becomes a file of its own.
func TestContentDiffSplitsHardlinkThatIsAFileForTheClient(t *testing.T) {
	separate := maps.Clone(contentDiffTree)
	separate["hardlink"] = separate["Dockerfile"]
	dirA := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	require.NoError(t, os.Link(filepath.Join(dirA, "Dockerfile"), filepath.Join(dirA, "hardlink")))
	dirB := writeContentDiffTree(t, separate, contentDiffModTimeB)
	dest := t.TempDir()
	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})

	assert.Equal(t, []string{"hardlink"}, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)
	file, err := os.Stat(filepath.Join(dest, "Dockerfile"))
	require.NoError(t, err)
	hardlink, err := os.Stat(filepath.Join(dest, "hardlink"))
	require.NoError(t, err)
	assert.False(t, os.SameFile(file, hardlink))
}

// syncContentDiffClientDir is syncContentDiffDir with the directory as the
// client of BuildKit gives it to the session (prepareSyncedFiles of the
// client package): behind a filter that makes root the owner of every path.
// That filter reads every path of the directory, also the ones that the
// request of the daemon filters out afterwards, and reading the paths is what
// names the hardlinks. The receiver gives the files to the user of the test,
// who may not be root.
func syncContentDiffClientDir(t *testing.T, src, dest string, opt FSSendRequestOpt) *recordingUpdater {
	t.Helper()
	srcFS, err := fsutil.NewFS(src)
	require.NoError(t, err)
	srcFS, err = fsutil.NewFilterFS(srcFS, &fsutil.FilterOpt{
		Map: func(_ string, st *fstypes.Stat) fsutil.MapResult {
			st.Uid = 0
			st.Gid = 0
			return fsutil.MapResultKeep
		},
	})
	require.NoError(t, err)

	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	opt.Filter = func(_ string, st *fstypes.Stat) bool {
		st.Uid = uid
		st.Gid = gid
		return true
	}
	return syncContentDiffFS(t, srcFS, dest, opt)
}

// contentDiffFilteredFirstPaths are requests of the daemon that filter out
// the first path of the file of writeContentDiffHardlinks, and keep its
// hardlinks.
var contentDiffFilteredFirstPaths = []struct {
	name  string
	first string
	opt   FSSendRequestOpt
}{
	{
		name:  "excluded path",
		first: "hardlink-a",
		opt:   FSSendRequestOpt{ContentDiff: true, ExcludePatterns: []string{"hardlink-a"}},
	},
	{
		name:  "path that is not included",
		first: "hardlink-a",
		opt:   FSSendRequestOpt{ContentDiff: true, FollowPaths: []string{"file", "hardlink-b", "hardlink-c"}},
	},
	{
		// fsutil does not go into an excluded directory, unless a pattern
		// with a wildcard could include one of its paths again
		name:  "path of an excluded directory",
		first: "dir/hardlink-a",
		opt:   FSSendRequestOpt{ContentDiff: true, ExcludePatterns: []string{"dir", "!**/*.keep"}},
	},
}

// writeContentDiffHardlinks creates a directory with "file" and with another
// file that has three paths: first, which a walk has to find before the other
// two, "hardlink-b" and "hardlink-c".
func writeContentDiffHardlinks(t *testing.T, first, content string, modTime time.Time) string {
	t.Helper()
	dir := writeContentDiffTree(t, map[string]string{"file": "content", first: content}, modTime)
	for _, name := range []string{"hardlink-b", "hardlink-c"} {
		// a link does not change the modification time of the file
		require.NoError(t, os.Link(filepath.Join(dir, filepath.FromSlash(first)), filepath.Join(dir, name)))
	}
	return dir
}

// requireContentDiffNames checks the names of the paths that dir has.
func requireContentDiffNames(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	actual := make([]string, 0, len(entries))
	for _, entry := range entries {
		actual = append(actual, entry.Name())
	}
	assert.Equal(t, names, actual)
}

// The request of the daemon filters out the first path of a file with
// hardlinks. For the client the next path is a hardlink of a path that is not
// sent, and fsutil sends it as a regular file. It has to be compared by
// content as any other file: here it comes from another directory with the
// same content, and nothing is sent.
func TestContentDiffKeepsHardlinksWhenFirstPathIsFilteredOut(t *testing.T) {
	for _, tc := range contentDiffFilteredFirstPaths {
		t.Run(tc.name, func(t *testing.T) {
			dirA := writeContentDiffHardlinks(t, tc.first, "content", contentDiffModTimeA)
			dirB := writeContentDiffHardlinks(t, tc.first, "content", contentDiffModTimeB)
			dest := t.TempDir()
			first := syncContentDiffClientDir(t, dirA, dest, tc.opt)
			require.Equal(t, []string{"file", "hardlink-b", "hardlink-c"}, first.writtenFiles())

			second := syncContentDiffClientDir(t, dirB, dest, tc.opt)

			assert.Empty(t, second.writtenFiles())
			requireContentDiffNames(t, dest, "file", "hardlink-b", "hardlink-c")
			requireContentDiffHardlinks(t, dest, "hardlink-b", "hardlink-c")
			fi, err := os.Stat(filepath.Join(dest, "hardlink-b"))
			require.NoError(t, err)
			assert.Equal(t, contentDiffModTimeB.UnixNano(), fi.ModTime().UnixNano())
		})
	}
}

// The same file comes with another content and the saved size and
// modification time: without its digest the saved content would stay.
func TestContentDiffSendsHardlinksWhenFirstPathIsFilteredOut(t *testing.T) {
	for _, tc := range contentDiffFilteredFirstPaths {
		t.Run(tc.name, func(t *testing.T) {
			dirA := writeContentDiffHardlinks(t, tc.first, "content of A", contentDiffModTimeA)
			dirB := writeContentDiffHardlinks(t, tc.first, "content of B", contentDiffModTimeA)
			dest := t.TempDir()
			syncContentDiffClientDir(t, dirA, dest, tc.opt)

			second := syncContentDiffClientDir(t, dirB, dest, tc.opt)

			assert.Equal(t, []string{"hardlink-b", "hardlink-c"}, second.writtenFiles())
			requireContentDiffNames(t, dest, "file", "hardlink-b", "hardlink-c")
			requireContentDiffHardlinks(t, dest, "hardlink-b", "hardlink-c")
			content, err := os.ReadFile(filepath.Join(dest, "hardlink-c"))
			require.NoError(t, err)
			assert.Equal(t, "content of B", string(content))
		})
	}
}

// The client names the hardlinks of every directory that it reads, and no
// request filters them here: the mount of the client does not change what the
// tests above get from a plain directory.
func TestContentDiffLinksHardlinksOfClientToRequestedTarget(t *testing.T) {
	dirA, dirB := contentDiffSameTimeHardlinkTrees(t)
	dest := t.TempDir()
	syncContentDiffClientDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})

	second := syncContentDiffClientDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})

	assert.Equal(t, []string{"Dockerfile", "hardlink", "src/hardlink"}, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)
	requireContentDiffHardlinks(t, dest, "Dockerfile", "hardlink", "src/hardlink")
}

func TestContentDiffReplacesSymlinkWithDirectory(t *testing.T) {
	dirA := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	require.NoError(t, os.Symlink("src", filepath.Join(dirA, "link")))
	files := maps.Clone(contentDiffTree)
	files["link/main.go"] = contentDiffTree["src/main.go"]
	dirB := writeContentDiffTree(t, files, contentDiffModTimeB)
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Equal(t, []string{"link/main.go"}, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)
}

func TestContentDiffStreamDoesNotFollowSymlinks(t *testing.T) {
	tests := []struct {
		name  string
		stats []*fstypes.Stat
	}{
		{
			name:  "symlink to a file with the same content",
			stats: []*fstypes.Stat{newContentDiffStat("symlink")},
		},
		{
			name:  "parent that leaves the destination",
			stats: []*fstypes.Stat{newContentDiffDirStat("outside"), newContentDiffStat("outside/file")},
		},
		{
			name:  "parent that is a symlink to a directory of the destination",
			stats: []*fstypes.Stat{newContentDiffDirStat("link"), newContentDiffStat("link/file")},
		},
		{
			name:  "parent that is a symlink to the destination",
			stats: []*fstypes.Stat{newContentDiffDirStat("self"), newContentDiffDirStat("self/self"), newContentDiffStat("self/self/file")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, dest := newTestContentDiffStream(t)
			outside := writeContentDiffTree(t, map[string]string{"file": contentDiffSavedContent}, contentDiffModTimeA)
			inside := filepath.Join(dest, "dir", "file")
			require.NoError(t, os.WriteFile(inside, []byte(contentDiffSavedContent), 0600))
			require.NoError(t, os.Chtimes(inside, contentDiffModTimeA, contentDiffModTimeA))
			require.NoError(t, os.Symlink("file", filepath.Join(dest, "symlink")))
			require.NoError(t, os.Symlink(outside, filepath.Join(dest, "outside")))
			require.NoError(t, os.Symlink("dir", filepath.Join(dest, "link")))
			require.NoError(t, os.Symlink(".", filepath.Join(dest, "self")))

			for _, st := range tt.stats {
				s.handleStat(st)
			}

			st := tt.stats[len(tt.stats)-1]
			assert.False(t, s.invalid)
			assert.Equal(t, contentDiffModTimeB.UnixNano(), st.ModTime)
			assert.Nil(t, st.Xattrs)
			assert.Empty(t, s.kept)
			assert.Equal(t, 0, s.hashedFiles)
			for _, root := range s.dirs.roots {
				assert.Nil(t, root)
			}
		})
	}
}

// watchContentDiffFIFO fails the test if the FIFO is opened for reading. That
// open blocks until the FIFO has a writer: this function gives it one, so a
// mistake is a failed test and not one that never ends.
func watchContentDiffFIFO(t *testing.T, p string) {
	t.Helper()
	stop := make(chan struct{})
	writer := make(chan int, 1)
	go func() {
		for {
			select {
			case <-stop:
				writer <- -1
				return
			default:
			}
			// without a reader it fails with ENXIO. The writer stays open:
			// a reader that is waiting may miss one that closes at once.
			if fd, err := syscall.Open(p, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				writer <- fd
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		fd := <-writer
		if !assert.Equal(t, -1, fd, "%s was opened for reading", p) {
			syscall.Close(fd)
		}
	})
}

func TestContentDiffStreamDoesNotOpenSpecialFiles(t *testing.T) {
	// opening a FIFO blocks until it has a writer
	tests := []struct {
		name  string
		stats []*fstypes.Stat
	}{
		{
			name:  "FIFO where the client has a file",
			stats: []*fstypes.Stat{newContentDiffStat("pipe")},
		},
		{
			name:  "FIFO where the client has a directory",
			stats: []*fstypes.Stat{newContentDiffDirStat("pipe"), newContentDiffStat("pipe/file")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, dest := newTestContentDiffStream(t)
			require.NoError(t, syscall.Mkfifo(filepath.Join(dest, "pipe"), 0600))
			watchContentDiffFIFO(t, filepath.Join(dest, "pipe"))

			for _, st := range tt.stats {
				s.handleStat(st)
			}

			st := tt.stats[len(tt.stats)-1]
			assert.False(t, s.invalid)
			assert.Equal(t, contentDiffModTimeB.UnixNano(), st.ModTime)
			assert.Empty(t, s.kept)
			assert.Equal(t, 0, s.hashedFiles)
		})
	}
}

func TestContentDiffReplacesFIFOWithDirectory(t *testing.T) {
	dirA := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	require.NoError(t, syscall.Mkfifo(filepath.Join(dirA, "pipe"), 0600))
	files := maps.Clone(contentDiffTree)
	files["pipe/main.go"] = contentDiffTree["src/main.go"]
	dirB := writeContentDiffTree(t, files, contentDiffModTimeB)
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})
	fi, err := os.Lstat(filepath.Join(dest, "pipe"))
	require.NoError(t, err)
	require.Equal(t, os.ModeNamedPipe, fi.Mode().Type())
	// fsutil removes the FIFO during the transfer, and nothing could reach a
	// reader that waits on it then: with a writer no open of it waits
	writer, err := syscall.Open(filepath.Join(dest, "pipe"), syscall.O_RDWR|syscall.O_NONBLOCK, 0)
	require.NoError(t, err)
	t.Cleanup(func() {
		syscall.Close(writer)
	})

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Equal(t, []string{"pipe/main.go"}, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)
}

func TestSavedDirsParentOnlyOpensDirectories(t *testing.T) {
	dest := writeContentDiffTree(t, map[string]string{"dir/file": "", "file": ""}, contentDiffModTimeA)
	require.NoError(t, os.Symlink("dir", filepath.Join(dest, "link")))
	require.NoError(t, os.Symlink(".", filepath.Join(dest, "self")))
	require.NoError(t, syscall.Mkfifo(filepath.Join(dest, "pipe"), 0600))
	watchContentDiffFIFO(t, filepath.Join(dest, "pipe"))

	for _, name := range []string{"link/file", "self/file", "self/dir/file", "pipe/file", "file/file"} {
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

func TestWithContentDigestsSkipsLinks(t *testing.T) {
	dir := writeContentDiffTree(t, map[string]string{"file": "content"}, contentDiffModTimeA)
	require.NoError(t, os.Symlink("file", filepath.Join(dir, "symlink")))
	require.NoError(t, os.Link(filepath.Join(dir, "file"), filepath.Join(dir, "hardlink")))
	base, err := fsutil.NewFS(dir)
	require.NoError(t, err)

	fs := withContentDigests(base)
	stats := walkContentDiffStats(t, fs)

	// fsutil sends the first path of a file with several hard links as a
	// regular file and the other ones as links to it
	assert.Equal(t, contentDiffDigest("content"), stats["file"].Xattrs[contentDigestXattr])
	assert.Equal(t, "file", stats["hardlink"].Linkname)
	assert.NotContains(t, stats["hardlink"].Xattrs, contentDigestXattr)
	assert.NotContains(t, stats["symlink"].Xattrs, contentDigestXattr)
}

func TestContentDiffStreamDoesNotHashFilesWithAnotherMode(t *testing.T) {
	s, dest := newTestContentDiffStream(t)
	require.NoError(t, os.Chmod(filepath.Join(dest, "file"), 0640))
	require.NoError(t, os.Chtimes(filepath.Join(dest, "file"), contentDiffModTimeA, contentDiffModTimeA))
	st := newContentDiffStat("file")

	s.handleStat(st)

	assert.Equal(t, contentDiffModTimeB.UnixNano(), st.ModTime)
	assert.Nil(t, st.Xattrs)
	assert.Empty(t, s.kept)
	assert.Equal(t, 0, s.hashedFiles)
}

func TestContentDiffStreamDoesNotKeepFileThatCanNotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files without permissions")
	}
	s, dest := newTestContentDiffStream(t)
	require.NoError(t, os.Chmod(filepath.Join(dest, "file"), 0))
	require.NoError(t, os.Chtimes(filepath.Join(dest, "file"), contentDiffModTimeA, contentDiffModTimeA))
	st := newContentDiffStat("file")
	st.Mode = 0

	s.handleStat(st)

	assert.Equal(t, contentDiffModTimeB.UnixNano(), st.ModTime)
	assert.Empty(t, s.kept)
	assert.Equal(t, 0, s.hashedFiles)
}
