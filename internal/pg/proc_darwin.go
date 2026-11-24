//go:build darwin

package pg

import (
	"bytes"
	"errors"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

const darwinSZOMB = 5 // p_stat value for a zombie (sys/proc.h)

func kinfo(pid int) (*unix.KinfoProc, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return nil, err
	}
	if k.Proc.P_pid != int32(pid) {
		return nil, errors.New("process not found")
	}
	return k, nil
}

func processName(pid int) (string, error) {
	k, err := kinfo(pid)
	if err != nil {
		return "", err
	}
	comm := k.Proc.P_comm[:]
	if i := bytes.IndexByte(comm, 0); i >= 0 {
		comm = comm[:i]
	}
	return string(comm), nil
}

func isZombie(pid int) bool {
	k, err := kinfo(pid)
	return err == nil && k.Proc.P_stat == darwinSZOMB
}

// postmasterSysProcAttr: own process group, so a Ctrl-C in the terminal
// reaches only the agent, which then stops Postgres in order. macOS has no
// parent-death signal; the watchdog covers that (see guard_unix.go).
func postmasterSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// toolSysProcAttr: tools lead their own process group so the watchdog can
// kill everything they started.
func toolSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func startProcess(cmd *exec.Cmd) (<-chan error, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := make(chan error, 1)
	go func() { ch <- cmd.Wait() }()
	return ch, nil
}
