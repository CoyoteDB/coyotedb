//go:build windows

package migrate

import (
	"os"
	"os/exec"
)

// On Windows pg_dump and pg_restore run parallel jobs as threads, so the
// process is the whole tree. Ctrl-C in the console reaches it directly.
func setProcAttr(cmd *exec.Cmd) {}

func killTree(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}
