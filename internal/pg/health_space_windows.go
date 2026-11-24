//go:build windows

package pg

import "golang.org/x/sys/windows"

// DiskSpace is the space available to this user, and the size, of the
// volume that holds dir.
func DiskSpace(dir string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, err
	}
	var avail, all, allFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &all, &allFree); err != nil {
		return 0, 0, err
	}
	return avail, all, nil
}
