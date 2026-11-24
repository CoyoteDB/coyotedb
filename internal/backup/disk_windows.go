//go:build windows

package backup

import (
	"strings"

	"golang.org/x/sys/windows"
)

// sameDevice compares the volumes (drive letters or mount folders) that
// hold a and b.
func sameDevice(a, b string) (bool, error) {
	va, err := volumeOf(a)
	if err != nil {
		return false, err
	}
	vb, err := volumeOf(b)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(va, vb), nil
}

func volumeOf(p string) (string, error) {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(u, &buf[0], uint32(len(buf))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf), nil
}
