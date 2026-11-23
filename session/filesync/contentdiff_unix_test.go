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
	}
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})

	// fsutil links the hardlink again because its modification time changed,
	// which does not need the content
	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Equal(t, []string{"hardlink"}, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)

	target, err := os.Readlink(filepath.Join(dest, "symlink"))
	require.NoError(t, err)
	assert.Equal(t, "Dockerfile", target)
	file, err := os.Stat(filepath.Join(dest, "Dockerfile"))
	require.NoError(t, err)
	hardlink, err := os.Stat(filepath.Join(dest, "hardlink"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(file, hardlink))
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
