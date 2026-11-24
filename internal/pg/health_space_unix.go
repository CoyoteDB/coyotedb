//go:build unix

package pg

import "golang.org/x/sys/unix"

// DiskSpace is the space available to this user, and the size, of the
// file system that holds dir.
func DiskSpace(dir string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)                                     //nolint:unconvert // field types differ by OS
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, nil //nolint:unconvert // field types differ by OS
}
