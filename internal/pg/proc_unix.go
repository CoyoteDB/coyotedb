//go:build unix

package pg

import (
	"errors"

	"golang.org/x/sys/unix"
)

// processAlive reports whether pid exists and is not a zombie.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	if err != nil && !errors.Is(err, unix.EPERM) {
		return false
	}
	return !isZombie(pid)
}

// isPostgresProcess reports whether pid is a postgres server process.
func isPostgresProcess(pid int) (bool, error) {
	name, err := processName(pid)
	if err != nil {
		return false, err
	}
	return name == "postgres", nil
}

// requestStop asks a postmaster to shut down. SIGINT is Postgres' "fast"
// shutdown, SIGQUIT its "immediate" shutdown. (Only used in *_unix files:
// on Windows we go through pg_ctl.)
func requestStop(_ Install, _ string, pid int, m stopMode) error {
	sig := unix.SIGINT
	if m == modeImmediate {
		sig = unix.SIGQUIT
	}
	return unix.Kill(pid, sig)
}

func killPID(pid int) error { return unix.Kill(pid, unix.SIGKILL) }
