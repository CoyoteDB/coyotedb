//go:build !windows

package agent

import "syscall"

// readOnlyErrno is the error isReadOnlyErrno looks for.
func readOnlyErrno() error { return syscall.EROFS }
