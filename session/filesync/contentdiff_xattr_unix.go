//go:build linux || darwin || freebsd

package filesync

import (
	"bytes"
	"os"
	"runtime"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

// savedXattrs returns the extended attributes of an open file of the saved
// copy that are compared (see isComparedXattr), without the ones that fsutil
// does not load.
func savedXattrs(f *os.File) (map[string][]byte, error) {
	defer runtime.KeepAlive(f)
	fd := int(f.Fd())

	var small [256]byte
	names := small[:]
	n, err := unix.Flistxattr(fd, names)
	if errors.Is(err, unix.ERANGE) {
		if n, err = unix.Flistxattr(fd, nil); err == nil {
			names = make([]byte, n)
			n, err = unix.Flistxattr(fd, names)
		}
	}
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, nil
		}
		return nil, errors.WithStack(err)
	}

	var xattrs map[string][]byte
	for name := range bytes.SplitSeq(names[:n], []byte{0}) {
		if len(name) == 0 || !isComparedXattr(string(name)) || strings.HasPrefix(string(name), "com.apple.") {
			continue
		}
		value, err := fgetxattr(fd, string(name))
		if err != nil {
			return nil, err
		}
		if xattrs == nil {
			xattrs = map[string][]byte{}
		}
		xattrs[string(name)] = value
	}
	return xattrs, nil
}

func fgetxattr(fd int, name string) ([]byte, error) {
	value := make([]byte, 128)
	for {
		n, err := unix.Fgetxattr(fd, name, value)
		if err == nil {
			return value[:n], nil
		}
		if !errors.Is(err, unix.ERANGE) {
			return nil, errors.WithStack(err)
		}
		// the size that it needs
		if n, err = unix.Fgetxattr(fd, name, nil); err != nil {
			return nil, errors.WithStack(err)
		}
		value = make([]byte, n)
	}
}
