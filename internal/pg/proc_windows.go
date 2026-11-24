//go:build windows

package pg

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

const stillActive = 259 // STILL_ACTIVE exit code

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		// Access denied means it exists but belongs to someone else.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}

func processName(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", err
	}
	return filepath.Base(windows.UTF16ToString(buf[:size])), nil
}

func isPostgresProcess(pid int) (bool, error) {
	name, err := processName(pid)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(name, "postgres.exe"), nil
}

// requestStop: Windows has no SIGINT/SIGQUIT for services, so we always go
// through pg_ctl (ARCHITECTURE.md, Platform notes). -W: do not wait; the
// caller polls for exit with its own deadline.
func requestStop(inst Install, pgdata string, _ int, m stopMode) error {
	cmd := exec.Command(inst.Bin("pg_ctl"), "stop", "-D", pgdata, "-m", m.String(), "-W")
	cmd.Env = cleanEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("pg_ctl stop -m %s: %w: %s", m, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func killPID(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}

// postmasterSysProcAttr: a new process group so a console Ctrl-C reaches
// only the agent, which stops Postgres in order. The Job Object installed by
// NewGuard (guard_windows.go) makes Postgres die with the agent.
func postmasterSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}

func startProcess(cmd *exec.Cmd) (<-chan error, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := make(chan error, 1)
	go func() { ch <- cmd.Wait() }()
	return ch, nil
}
