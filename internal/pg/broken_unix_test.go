//go:build unix

package pg

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

const brokenHelperEnv = "COYOTE_TEST_BROKEN_HELPER"

// TestBrokenHelper is the child process of TestBinaryBroken.
func TestBrokenHelper(t *testing.T) {
	v := os.Getenv(brokenHelperEnv)
	if v == "" {
		t.Skip("helper process only")
	}
	code, _ := strconv.Atoi(v)
	os.Exit(code)
}

func TestBinaryBroken(t *testing.T) {
	run := func(v string) error {
		if sig, _ := strconv.Atoi(v); sig < 0 {
			// A C program (the Go runtime handles SIGBUS/SIGSEGV itself).
			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Process.Signal(syscall.Signal(-sig))
			return cmd.Wait()
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestBrokenHelper$")
		cmd.Env = append(os.Environ(), brokenHelperEnv+"="+v)
		return cmd.Run()
	}
	for _, c := range []struct {
		v    string
		want bool
	}{
		{strconv.Itoa(-int(syscall.SIGBUS)), true},
		{strconv.Itoa(-int(syscall.SIGSEGV)), true},
		{strconv.Itoa(-int(syscall.SIGTERM)), false},
		{"127", true},
		{"1", false},
	} {
		if got := BinaryBroken(run(c.v)); got != c.want {
			t.Errorf("helper %s: BinaryBroken = %v, want %v", c.v, got, c.want)
		}
	}
	// A "program" that is not one (a truncated binary is the same to exec).
	p := filepath.Join(t.TempDir(), "postgres")
	if err := os.WriteFile(p, []byte{0x7f, 'E', 'L'}, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command(p).Run(); !BinaryBroken(err) {
		t.Errorf("exec of a truncated program: %v not seen as broken", err)
	}
	if BinaryBroken(nil) || BinaryBroken(ErrExitedDuringStartup) {
		t.Error("broken without a cause")
	}
}
