package mono

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestNowMonotonicAndAdvances(t *testing.T) {
	a := Now()
	start := time.Now()
	for time.Since(start) < 20*time.Millisecond {
	}
	b := Now()
	if a <= 0 || b <= a {
		t.Fatalf("not monotonic: %d then %d", a, b)
	}
	if d := b - a; d < 15*time.Millisecond || d > time.Second {
		t.Fatalf("advanced %s over ~20ms of real time", d)
	}
}

// The clock is shared by every process on the machine: a reading taken
// in a child process lies between two taken here.
func TestNowSharedAcrossProcesses(t *testing.T) {
	if os.Getenv("MONO_CHILD") == "1" {
		os.Stdout.WriteString(strconv.FormatInt(int64(Now()), 10))
		os.Exit(0)
	}
	before := Now()
	cmd := exec.Command(os.Args[0], "-test.run=^TestNowSharedAcrossProcesses$")
	cmd.Env = append(os.Environ(), "MONO_CHILD=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	after := Now()
	child, err := strconv.ParseInt(string(out), 10, 64)
	if err != nil {
		t.Fatalf("child said %q", out)
	}
	if c := time.Duration(child); c < before || c > after {
		t.Fatalf("child reading %d outside [%d, %d]", c, before, after)
	}
}
