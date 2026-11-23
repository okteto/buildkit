//go:build linux || darwin

package filesync

import (
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/continuity/sysx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const contentDiffTestXattr = "user.okteto.test"

// setContentDiffXattr sets an extended attribute and skips the test when the
// filesystem of the temporary directory does not support it.
func setContentDiffXattr(t *testing.T, p, value string) {
	t.Helper()
	fi, err := os.Lstat(p)
	require.NoError(t, err)
	if err := sysx.LSetxattr(p, contentDiffTestXattr, []byte(value), 0); err != nil {
		t.Skipf("extended attributes are not supported in %s: %v", filepath.Dir(p), err)
	}
	require.NoError(t, os.Chtimes(p, fi.ModTime(), fi.ModTime()))
}

func TestSavedXattrs(t *testing.T) {
	dir := writeContentDiffTree(t, map[string]string{"with": "", "without": ""}, contentDiffModTimeA)
	setContentDiffXattr(t, filepath.Join(dir, "with"), "value")

	tests := []struct {
		name     string
		expected map[string][]byte
	}{
		{name: "with", expected: map[string][]byte{contentDiffTestXattr: []byte("value")}},
		{name: "without"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := os.Open(filepath.Join(dir, tt.name))
			require.NoError(t, err)
			defer f.Close()

			xattrs, err := savedXattrs(f)

			require.NoError(t, err)
			assert.Equal(t, tt.expected, xattrs)
		})
	}
}

func TestSavedXattrsBiggerThanTheFirstBuffer(t *testing.T) {
	dir := writeContentDiffTree(t, map[string]string{"file": ""}, contentDiffModTimeA)
	p := filepath.Join(dir, "file")
	setContentDiffXattr(t, p, "value")
	expected := map[string][]byte{contentDiffTestXattr: []byte("value")}
	// names that add up to more than 256 bytes and a value of more than 128
	long := make([]byte, 300)
	for i := range 12 {
		name := contentDiffTestXattr + ".a-name-that-takes-some-space-" + string(rune('a'+i))
		require.NoError(t, sysx.LSetxattr(p, name, long, 0))
		expected[name] = long
	}
	f, err := os.Open(p)
	require.NoError(t, err)
	defer f.Close()

	xattrs, err := savedXattrs(f)

	require.NoError(t, err)
	assert.Equal(t, expected, xattrs)
}

func TestContentDiffStreamComparesXattrs(t *testing.T) {
	tests := []struct {
		name   string
		saved  string
		sent   map[string][]byte
		keeps  bool
		hashed int
	}{
		{name: "same", saved: "value", sent: map[string][]byte{contentDiffTestXattr: []byte("value")}, keeps: true, hashed: 1},
		{name: "only in the saved file", saved: "value"},
		{name: "other value", saved: "value", sent: map[string][]byte{contentDiffTestXattr: []byte("other")}},
		{name: "only sent", sent: map[string][]byte{contentDiffTestXattr: []byte("value")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, dest := newTestContentDiffStream(t)
			// the other file of the destination shows that the filesystem has extended attributes
			setContentDiffXattr(t, filepath.Join(dest, "dir"), "probe")
			if tt.saved != "" {
				setContentDiffXattr(t, filepath.Join(dest, "file"), tt.saved)
			}
			st := newContentDiffStat("file")
			maps.Copy(st.Xattrs, tt.sent)

			s.handleStat(st)

			assert.Equal(t, tt.keeps, len(s.kept) == 1)
			assert.Equal(t, tt.hashed, s.hashedFiles)
			assert.Equal(t, tt.sent, st.Xattrs)
		})
	}
}

func TestContentDiffSendsFilesWithOtherXattrs(t *testing.T) {
	dirA := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, contentDiffTree, contentDiffModTimeB)
	setContentDiffXattr(t, filepath.Join(dirA, "Dockerfile"), "from A")
	setContentDiffXattr(t, filepath.Join(dirA, "src", "main.go"), "same")
	setContentDiffXattr(t, filepath.Join(dirB, "src", "main.go"), "same")
	setContentDiffXattr(t, filepath.Join(dirB, "src", "lib", "lib.go"), "from B")
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})
	saved, err := sysx.LGetxattr(filepath.Join(dest, "Dockerfile"), contentDiffTestXattr)
	require.NoError(t, err)
	require.Equal(t, "from A", string(saved))

	// same content everywhere: only the files with other attributes are sent
	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})
	assert.Equal(t, []string{"Dockerfile", "src/lib/lib.go"}, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)

	_, err = sysx.LGetxattr(filepath.Join(dest, "Dockerfile"), contentDiffTestXattr)
	require.Error(t, err)
	kept, err := sysx.LGetxattr(filepath.Join(dest, "src", "main.go"), contentDiffTestXattr)
	require.NoError(t, err)
	assert.Equal(t, "same", string(kept))
	added, err := sysx.LGetxattr(filepath.Join(dest, "src", "lib", "lib.go"), contentDiffTestXattr)
	require.NoError(t, err)
	assert.Equal(t, "from B", string(added))
}

func TestContentDiffStreamIgnoresXattrsThatFsutilDoesNotLoad(t *testing.T) {
	s, dest := newTestContentDiffStream(t)
	p := filepath.Join(dest, "file")
	if err := sysx.LSetxattr(p, "com.apple.okteto.test", []byte("value"), 0); err != nil {
		t.Skipf("this platform has no com.apple. attributes: %v", err)
	}
	require.NoError(t, os.Chtimes(p, contentDiffModTimeA, contentDiffModTimeA))
	st := newContentDiffStat("file")

	s.handleStat(st)

	assert.Len(t, s.kept, 1)
}
