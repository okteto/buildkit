//go:build !(linux || darwin || freebsd)

package filesync

import "os"

// savedXattrs returns no extended attributes: fsutil does not read or write
// them on this platform.
func savedXattrs(*os.File) (map[string][]byte, error) {
	return nil, nil
}
