//go:build unix

package pg

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// terminatePostgresChild sends SIGTERM to a Postgres server process of
// this node (a WAL receiver ends cleanly on it). A pid that is not a
// postgres process (it ended and the number was reused) is refused.
func terminatePostgresChild(_ Install, pid int) error {
	ok, err := isPostgresProcess(pid)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("pid %d is not a postgres process", pid)
	}
	return unix.Kill(pid, unix.SIGTERM)
}
