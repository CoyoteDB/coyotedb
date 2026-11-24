//go:build !windows

package agent

import (
	"errors"
	"syscall"
)

// isReadOnlyErrno: the system said the file system is read-only (EROFS).
func isReadOnlyErrno(err error) bool { return errors.Is(err, syscall.EROFS) }
