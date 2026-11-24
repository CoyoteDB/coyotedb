package agent

import (
	"errors"
	"syscall"
)

// errorWriteProtect is ERROR_WRITE_PROTECT, "the media is write
// protected". Windows has no EROFS (Go's syscall.EROFS there is an
// invented value no system call returns).
const errorWriteProtect = syscall.Errno(19)

// isReadOnlyErrno: the system said the disk is write protected.
func isReadOnlyErrno(err error) bool { return errors.Is(err, errorWriteProtect) }
