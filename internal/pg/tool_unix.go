//go:build unix

package pg

import (
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

func (g *Guard) prepareTool(cmd *exec.Cmd) { cmd.SysProcAttr = toolSysProcAttr() }

// killTool kills the tool's whole process group (pg_basebackup forks a WAL
// streaming child; pg_rewind may run postgres), then the tool itself.
func killTool(p *os.Process) {
	_ = unix.Kill(-p.Pid, unix.SIGKILL)
	_ = p.Kill()
}

// killToolGroup is used by the watchdog for a tool whose agent is gone.
// The tool was started as a process group leader, so its pid is the group
// id; a group id cannot be reused while any member is alive.
func killToolGroup(pgid int) error {
	err := unix.Kill(-pgid, unix.SIGKILL)
	if err == unix.ESRCH {
		return nil
	}
	return err
}
