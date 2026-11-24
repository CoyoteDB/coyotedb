//go:build windows

package pg

import (
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"
)

// childProcesses finds a child of this test process (the watchdog uses it
// to find a terminated postmaster's backends). Windows only; not yet run
// on a Windows machine.
func TestChildProcessesFindsChild(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping -n 30 127.0.0.1 >NUL")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		kids, err := childProcesses(uint32(os.Getpid()))
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(kids, uint32(cmd.Process.Pid)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("child not listed")
}
