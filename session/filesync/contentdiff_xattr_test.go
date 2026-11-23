//go:build linux || darwin

package filesync

import (
	"encoding/binary"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/continuity/sysx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tonistiigi/fsutil"
)

const (
	contentDiffTestXattr = "user.okteto.test"

	// attributes that are not part of the content hash of the cache
	contentDiffACLXattr      = "system.posix_acl_access"
	contentDiffSecurityXattr = "security.okteto.test"
)

// setContentDiffXattr sets an extended attribute and skips the test when the
// filesystem of the temporary directory does not support it.
func setContentDiffXattr(t *testing.T, p, value string) {
	t.Helper()
	setContentDiffXattrNamed(t, p, contentDiffTestXattr, []byte(value))
}

// setContentDiffXattrNamed sets the extended attribute name and skips the
// test when it can't be set here: the filesystem has no extended attributes,
// or the attribute needs a privilege that the test does not have.
func setContentDiffXattrNamed(t *testing.T, p, name string, value []byte) {
	t.Helper()
	fi, err := os.Lstat(p)
	require.NoError(t, err)
	if err := sysx.LSetxattr(p, name, value, 0); err != nil {
		t.Skipf("the extended attribute %s can't be set in %s: %v", name, filepath.Dir(p), err)
	}
	require.NoError(t, os.Chtimes(p, fi.ModTime(), fi.ModTime()))
}

// contentDiffACL returns the value of system.posix_acl_access of an ACL that
// lets the user uid read a file of mode 0600. Its mask gives nothing to the
// group, so setting it leaves the mode as it is.
func contentDiffACL(uid uint32) []byte {
	const undefinedID = 0xffffffff
	entries := []struct {
		tag  uint16
		perm uint16
		id   uint32
	}{
		{tag: 0x01, perm: 6, id: undefinedID}, // owner
		{tag: 0x02, perm: 4, id: uid},         // the user
		{tag: 0x04, perm: 0, id: undefinedID}, // group
		{tag: 0x10, perm: 0, id: undefinedID}, // mask
		{tag: 0x20, perm: 0, id: undefinedID}, // others
	}
	acl := binary.LittleEndian.AppendUint32(nil, 2) // version of the format
	for _, e := range entries {
		acl = binary.LittleEndian.AppendUint16(acl, e.tag)
		acl = binary.LittleEndian.AppendUint16(acl, e.perm)
		acl = binary.LittleEndian.AppendUint32(acl, e.id)
	}
	return acl
}

// contentDiffXattr returns the value of an extended attribute of a file, or
// nil if the file does not have it.
func contentDiffXattr(t *testing.T, p, name string) []byte {
	t.Helper()
	value, err := sysx.LGetxattr(p, name)
	if err != nil {
		require.ErrorIs(t, err, sysx.ENODATA)
		return nil
	}
	return value
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
	// names that add up to more than 256 bytes and values of more than 128.
	// All together they fit in one block of the filesystem, which is all
	// that ext4 gives to the attributes of a file.
	long := make([]byte, 140)
	for i := range 8 {
		name := contentDiffTestXattr + ".a-name-that-takes-some-space-" + string(rune('a'+i))
		require.NoError(t, sysx.LSetxattr(p, name, long, 0))
		expected[name] = long
	}
	names := 0
	for name := range expected {
		names += len(name) + 1
	}
	require.Greater(t, names, 256)
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

// The attributes that are not part of the content hash of the cache are
// compared too: fsutil transfers them, so a kept file would keep the ones of
// an earlier client.
func TestContentDiffStreamComparesXattrsOutsideTheContentHash(t *testing.T) {
	attributes := []struct {
		name  string
		value []byte
		other []byte
	}{
		{name: contentDiffACLXattr, value: contentDiffACL(1001), other: contentDiffACL(1002)},
		{name: contentDiffSecurityXattr, value: []byte("value"), other: []byte("other")},
	}
	for _, attr := range attributes {
		tests := []struct {
			name   string
			saved  []byte
			sent   []byte
			keeps  bool
			hashed int
		}{
			{name: "same", saved: attr.value, sent: attr.value, keeps: true, hashed: 1},
			{name: "changed by the client", saved: attr.value, sent: attr.other},
			{name: "removed by the client", saved: attr.value},
			{name: "added by the client", sent: attr.value},
		}
		for _, tt := range tests {
			t.Run(attr.name+"/"+tt.name, func(t *testing.T) {
				s, dest := newTestContentDiffStream(t)
				// the other entry of the destination shows that the attribute can be set here
				setContentDiffXattrNamed(t, filepath.Join(dest, "dir"), attr.name, attr.value)
				p := filepath.Join(dest, "file")
				if tt.saved != nil {
					setContentDiffXattrNamed(t, p, attr.name, tt.saved)
				}
				fi, err := os.Lstat(p)
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0600), fi.Mode(), "the attribute changed the mode of the saved file")
				st := newContentDiffStat("file")
				if tt.sent != nil {
					st.Xattrs[attr.name] = tt.sent
				}

				s.handleStat(st)

				assert.Equal(t, tt.keeps, len(s.kept) == 1)
				assert.Equal(t, tt.hashed, s.hashedFiles)
			})
		}
	}
}

// A file with the content, the mode and the size of the saved one, and with
// another modification time, is sent again when its ACL is not the one of the
// saved file: the saved copy must not keep an ACL that the client changed or
// removed.
func TestContentDiffSendsFilesWithOtherACLs(t *testing.T) {
	files := map[string]string{"added": "content", "changed": "content", "plain": "content", "removed": "content", "same": "content"}
	dirA := writeContentDiffTree(t, files, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, files, contentDiffModTimeB)
	setContentDiffXattrNamed(t, filepath.Join(dirA, "changed"), contentDiffACLXattr, contentDiffACL(1001))
	setContentDiffXattrNamed(t, filepath.Join(dirB, "changed"), contentDiffACLXattr, contentDiffACL(1002))
	setContentDiffXattrNamed(t, filepath.Join(dirA, "removed"), contentDiffACLXattr, contentDiffACL(1001))
	setContentDiffXattrNamed(t, filepath.Join(dirB, "added"), contentDiffACLXattr, contentDiffACL(1001))
	setContentDiffXattrNamed(t, filepath.Join(dirA, "same"), contentDiffACLXattr, contentDiffACL(1001))
	setContentDiffXattrNamed(t, filepath.Join(dirB, "same"), contentDiffACLXattr, contentDiffACL(1001))
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})
	requireSameContentDiffTree(t, dirA, dest)
	require.Equal(t, contentDiffACL(1001), contentDiffXattr(t, filepath.Join(dest, "changed"), contentDiffACLXattr))
	require.Equal(t, contentDiffACL(1001), contentDiffXattr(t, filepath.Join(dest, "removed"), contentDiffACLXattr))

	// same content, mode and size everywhere: only the files with another ACL are sent
	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})

	assert.Equal(t, []string{"added", "changed", "removed"}, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)
	assert.Equal(t, contentDiffACL(1001), contentDiffXattr(t, filepath.Join(dest, "added"), contentDiffACLXattr))
	assert.Equal(t, contentDiffACL(1002), contentDiffXattr(t, filepath.Join(dest, "changed"), contentDiffACLXattr))
	assert.Nil(t, contentDiffXattr(t, filepath.Join(dest, "removed"), contentDiffACLXattr))
	assert.Equal(t, contentDiffACL(1001), contentDiffXattr(t, filepath.Join(dest, "same"), contentDiffACLXattr))
	assert.Nil(t, contentDiffXattr(t, filepath.Join(dest, "plain"), contentDiffACLXattr))
}

// The same for the security attributes that are not part of the content hash.
func TestContentDiffSendsFilesWithOtherSecurityXattrs(t *testing.T) {
	files := map[string]string{"changed": "content", "plain": "content", "removed": "content", "same": "content"}
	dirA := writeContentDiffTree(t, files, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, files, contentDiffModTimeB)
	setContentDiffXattrNamed(t, filepath.Join(dirA, "changed"), contentDiffSecurityXattr, []byte("from A"))
	setContentDiffXattrNamed(t, filepath.Join(dirB, "changed"), contentDiffSecurityXattr, []byte("from B"))
	setContentDiffXattrNamed(t, filepath.Join(dirA, "removed"), contentDiffSecurityXattr, []byte("from A"))
	setContentDiffXattrNamed(t, filepath.Join(dirA, "same"), contentDiffSecurityXattr, []byte("same"))
	setContentDiffXattrNamed(t, filepath.Join(dirB, "same"), contentDiffSecurityXattr, []byte("same"))
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})
	require.Equal(t, "from A", string(contentDiffXattr(t, filepath.Join(dest, "removed"), contentDiffSecurityXattr)))

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})

	assert.Equal(t, []string{"changed", "removed"}, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)
	assert.Equal(t, "from B", string(contentDiffXattr(t, filepath.Join(dest, "changed"), contentDiffSecurityXattr)))
	assert.Nil(t, contentDiffXattr(t, filepath.Join(dest, "removed"), contentDiffSecurityXattr))
	assert.Equal(t, "same", string(contentDiffXattr(t, filepath.Join(dest, "same"), contentDiffSecurityXattr)))
}

// The SELinux label is the one attribute that is not compared: a client with
// SELinux sends the label of its own policy with every file.
func TestContentDiffStreamIgnoresSELinuxLabelOfTheClient(t *testing.T) {
	s, _ := newTestContentDiffStream(t)
	st := newContentDiffStat("file")
	st.Xattrs[selinuxXattr] = []byte("unconfined_u:object_r:user_home_t:s0")

	s.handleStat(st)

	assert.Len(t, s.kept, 1)
	assert.Equal(t, map[string][]byte{selinuxXattr: []byte("unconfined_u:object_r:user_home_t:s0")}, st.Xattrs)
}

// A node with SELinux labels every file of the saved copy, whatever the client
// sent, and a kept file keeps the label that it has.
func TestContentDiffKeepsFilesWithOtherSELinuxLabels(t *testing.T) {
	files := map[string]string{"both": "content", "client": "content", "node": "content"}
	dirA := writeContentDiffTree(t, files, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, files, contentDiffModTimeB)
	setContentDiffXattrNamed(t, filepath.Join(dirA, "both"), selinuxXattr, []byte("label of A"))
	setContentDiffXattrNamed(t, filepath.Join(dirB, "both"), selinuxXattr, []byte("label of B"))
	setContentDiffXattrNamed(t, filepath.Join(dirA, "node"), selinuxXattr, []byte("label of A"))
	setContentDiffXattrNamed(t, filepath.Join(dirB, "client"), selinuxXattr, []byte("label of B"))
	dest := t.TempDir()

	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})
	require.Equal(t, "label of A", string(contentDiffXattr(t, filepath.Join(dest, "node"), selinuxXattr)))

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})

	assert.Empty(t, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)
	assert.Equal(t, "label of A", string(contentDiffXattr(t, filepath.Join(dest, "both"), selinuxXattr)))
	assert.Equal(t, "label of A", string(contentDiffXattr(t, filepath.Join(dest, "node"), selinuxXattr)))
	assert.Nil(t, contentDiffXattr(t, filepath.Join(dest, "client"), selinuxXattr))
}

// The key that carries the digest can't be the name of an extended attribute
// of a file, so the digest never replaces one.
func TestContentDigestXattrIsNotAnAttributeName(t *testing.T) {
	dir := writeContentDiffTree(t, map[string]string{"file": "content"}, contentDiffModTimeA)
	p := filepath.Join(dir, "file")
	// the filesystem has extended attributes
	setContentDiffXattr(t, p, "value")

	err := sysx.LSetxattr(p, contentDigestXattr, []byte("value"), 0)

	require.Error(t, err)
}

// macOS takes attribute names without a namespace: a file can have one that
// looks like a key of this protocol. It is sent as any other attribute, next
// to the digest.
func TestWithContentDigestsKeepsAttributeNamedLikeADigest(t *testing.T) {
	const name = "okteto.sha256"
	dir := writeContentDiffTree(t, map[string]string{"file": "content"}, contentDiffModTimeA)
	setContentDiffXattrNamed(t, filepath.Join(dir, "file"), name, []byte("an attribute of the file"))
	base, err := fsutil.NewFS(dir)
	require.NoError(t, err)

	stats := walkContentDiffStats(t, withContentDigests(base))

	assert.Equal(t, map[string][]byte{
		name:               []byte("an attribute of the file"),
		contentDigestXattr: contentDiffDigest("content"),
	}, stats["file"].Xattrs)
}

// The same on the side of the daemon: only the digest is removed from the
// stat, and the attribute counts for the comparison as any other.
func TestContentDiffStreamKeepsAttributeNamedLikeADigest(t *testing.T) {
	const name = "okteto.sha256"
	s, _ := newTestContentDiffStream(t)
	st := newContentDiffStat("file")
	st.Xattrs[name] = []byte("an attribute of the file")

	s.handleStat(st)

	assert.Equal(t, map[string][]byte{name: []byte("an attribute of the file")}, st.Xattrs)
	// the saved file does not have the attribute
	assert.Empty(t, s.kept)
}

// The same when the modification time is the one of the saved file: the ACL
// is compared, and the file is sent when it is another one.
func TestContentDiffSendsFilesWithSameModTimeAndOtherACL(t *testing.T) {
	files := map[string]string{"changed": "content", "removed": "content", "same": "content"}
	dirA := writeContentDiffTree(t, files, contentDiffModTimeA)
	dirB := writeContentDiffTree(t, files, contentDiffModTimeA)
	setContentDiffXattrNamed(t, filepath.Join(dirA, "changed"), contentDiffACLXattr, contentDiffACL(1001))
	setContentDiffXattrNamed(t, filepath.Join(dirB, "changed"), contentDiffACLXattr, contentDiffACL(1002))
	setContentDiffXattrNamed(t, filepath.Join(dirA, "removed"), contentDiffACLXattr, contentDiffACL(1001))
	setContentDiffXattrNamed(t, filepath.Join(dirA, "same"), contentDiffACLXattr, contentDiffACL(1001))
	setContentDiffXattrNamed(t, filepath.Join(dirB, "same"), contentDiffACLXattr, contentDiffACL(1001))
	dest := t.TempDir()
	syncContentDiffDir(t, dirA, dest, FSSendRequestOpt{ContentDiff: true})

	second := syncContentDiffDir(t, dirB, dest, FSSendRequestOpt{ContentDiff: true})

	assert.Equal(t, []string{"changed", "removed"}, second.writtenFiles())
	requireSameContentDiffTree(t, dirB, dest)
	assert.Equal(t, contentDiffACL(1002), contentDiffXattr(t, filepath.Join(dest, "changed"), contentDiffACLXattr))
	assert.Nil(t, contentDiffXattr(t, filepath.Join(dest, "removed"), contentDiffACLXattr))
	assert.Equal(t, contentDiffACL(1001), contentDiffXattr(t, filepath.Join(dest, "same"), contentDiffACLXattr))
}
