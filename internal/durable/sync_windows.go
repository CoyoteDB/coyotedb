//go:build windows

package durable

import (
	"errors"

	"golang.org/x/sys/windows"
)

// SyncDir flushes a directory with FlushFileBuffers where Windows allows
// a directory handle with write access (NTFS for an owner or admin). When
// it does not (access denied, or a file system that rejects it), the call
// is skipped: NTFS journals metadata such as renames, so a completed
// rename is not lost, only possibly undone to the old name.
func SyncDir(dir string) error {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return nil
		}
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.FlushFileBuffers(h); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
			return nil
		}
		return err
	}
	return nil
}
