//go:build unix && !linux

package migrate

import (
	"os"
	"os/exec"
	"syscall"
)

func setProcAttr(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killTree kills the tool's whole process group (its parallel workers),
// then the tool itself.
func killTree(p *os.Process) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	_ = p.Kill()
}
