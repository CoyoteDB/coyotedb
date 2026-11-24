//go:build linux

package migrate

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcAttr: own process group, and the kernel kills the tool if this
// command dies.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

func killTree(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	_ = p.Kill()
}
