//go:build integration && unix

// OS-specific process helpers for the integration tests (Unix). The
// Windows versions are in osproc_windows_test.go; the tests themselves call
// only these helpers.
package integration

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const exeSuffix = ""

// prepareAgent: nothing to do on Unix (SIGINT goes to the pid).
func prepareAgent(*exec.Cmd) {}

// interrupt asks a coyote process to stop as Ctrl-C would.
func interrupt(p *os.Process) error { return p.Signal(os.Interrupt) }

func killPID(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

func freezePID(pid int) error { return syscall.Kill(pid, syscall.SIGSTOP) }

func thawPID(pid int) error { return syscall.Kill(pid, syscall.SIGCONT) }

// quitPostgres stops a stray postmaster at once (immediate shutdown).
func quitPostgres(pid int) { _ = syscall.Kill(pid, syscall.SIGQUIT) }

// alive reports whether pid exists and is not a zombie. A killed child
// whose parent was killed too can stay a zombie until init (or a
// subreaper) reaps it; it runs nothing and holds nothing, which is how the
// product counts it as well (internal/pg processAlive). Linux shows this
// more often than macOS, where launchd reaps at once.
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err != nil && err != syscall.EPERM {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false // ps finds no such process
	}
	return !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// postmasterGroupGone: the postmaster pid is the process group of all its
// backends; the group is gone when every one of them has exited.
func postmasterGroupGone(t *testing.T, pid int) bool {
	return errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH)
}

// argForm is how an argument appears in a process command line.
func argForm(s string) string { return s }

// isPostmasterCmd matches "…/bin/postgres … -D <pgdata>".
func isPostmasterCmd(cmdline, pgdata string) bool {
	return strings.HasSuffix(cmdline, " -D "+pgdata) && strings.Contains(cmdline, "/bin/postgres ")
}

type proc struct {
	pid  int
	cmd  string
	pgid int
}

func listProcs(t *testing.T) []proc {
	t.Helper()
	out, err := exec.Command("ps", "-axww", "-o", "pid=,pgid=,command=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	var ps []proc
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pgid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		cmd := strings.TrimSpace(line)
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, f[0]))
		cmd = strings.TrimSpace(strings.TrimPrefix(cmd, f[1]))
		ps = append(ps, proc{pid: pid, pgid: pgid, cmd: cmd})
	}
	return ps
}

// privateFile: only the owner may read or write it (0600).
func privateFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm() != 0o600 {
		return fmt.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	return nil
}
