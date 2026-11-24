//go:build unix

package pg

import (
	"errors"
	"io/fs"
	"os/exec"
	"syscall"
)

// BinaryBroken reports whether err looks like a Postgres program that
// cannot run as it is on disk (rather than a database problem): it could
// not be executed at all, the dynamic loader failed (exit 126/127), or it
// died of SIGBUS, SIGSEGV or SIGILL, which is what a truncated or corrupt
// mapped binary or library does. Then the install is checked in full.
func BinaryBroken(err error) bool {
	if err == nil {
		return false
	}
	var pe *fs.PathError
	if errors.As(err, &pe) && pe.Op == "fork/exec" {
		return true
	}
	if errors.Is(err, syscall.ENOEXEC) {
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
	if ws.Signaled() {
		switch ws.Signal() {
		case syscall.SIGBUS, syscall.SIGSEGV, syscall.SIGILL:
			return true
		}
		return false
	}
	return ws.Exited() && (ws.ExitStatus() == 126 || ws.ExitStatus() == 127)
}
