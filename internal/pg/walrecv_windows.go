//go:build windows

package pg

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// terminatePostgresChild asks a Postgres server process of this node to
// stop. Windows has no signals: Postgres emulates them through a named
// pipe that only its own processes open, which pg_ctl kill writes to, so a
// reused pid can never be hit.
func terminatePostgresChild(inst Install, pid int) error {
	cmd := exec.Command(inst.Bin("pg_ctl"), "kill", "TERM", strconv.Itoa(pid))
	cmd.Env = cleanEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("pg_ctl kill TERM %d: %w: %s", pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}
