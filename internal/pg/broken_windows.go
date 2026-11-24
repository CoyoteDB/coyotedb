//go:build windows

package pg

import (
	"errors"
	"io/fs"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// NTSTATUS exit codes of a program that cannot run as it is on disk.
const (
	statusAccessViolation    = 0xC0000005
	statusInvalidImageFormat = 0xC000007B
	statusDLLNotFound        = 0xC0000135
	statusDLLInitFailed      = 0xC0000142
	statusInPageError        = 0xC0000006 // the Windows SIGBUS: a mapped file could not be read
)

// BinaryBroken reports whether err looks like a Postgres program that
// cannot run as it is on disk: it could not be executed (bad image), a
// DLL is missing or broken, or it crashed reading its own mapped code.
// Then the install is checked in full.
func BinaryBroken(err error) bool {
	if err == nil {
		return false
	}
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Op == "fork/exec" {
		return true
	}
	if errors.Is(err, windows.ERROR_BAD_EXE_FORMAT) {
		return true
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	switch uint32(ws.ExitCode) {
	case statusAccessViolation, statusInvalidImageFormat, statusDLLNotFound, statusDLLInitFailed, statusInPageError:
		return true
	}
	return false
}
