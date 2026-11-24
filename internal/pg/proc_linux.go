//go:build linux

package pg

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

func processName(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func isZombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// Format: pid (comm) state ...; comm may contain spaces or ')'.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return false
	}
	return b[i+2] == 'Z'
}

// postmasterSysProcAttr: own process group (Ctrl-C reaches only the agent),
// and a parent-death signal so the kernel tells the postmaster to do an
// immediate shutdown (SIGQUIT) the moment the agent dies. The watchdog then
// escalates to SIGKILL if it is somehow still alive (guard_unix.go).
func postmasterSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGQUIT}
}

// toolSysProcAttr: own process group (the watchdog kills the group) and
// SIGKILL when the agent thread that started it dies.
func toolSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// startProcess starts cmd from a goroutine locked to its OS thread and keeps
// that thread alive until the process exits. Pdeathsig fires when the
// *thread* that forked the child exits, and the Go runtime may retire idle
// threads, so without this lock Postgres could be signalled while the agent
// is still alive.
func startProcess(cmd *exec.Cmd) (<-chan error, error) {
	started := make(chan error, 1)
	ch := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Never unlock: when this goroutine returns, the runtime terminates
		// the locked thread, which is fine because the child is gone by then.
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		ch <- cmd.Wait()
	}()
	if err := <-started; err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	return ch, nil
}
