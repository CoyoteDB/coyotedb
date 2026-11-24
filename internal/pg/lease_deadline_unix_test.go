//go:build unix

package pg

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/mono"
)

// F2 (lease review): the agent works out the watchdog's deadline, then
// (frozen, descheduled) writes it to the pipe later. That delay must never
// move the deadline later than the one the agent worked out.
func TestWatchdogDeadlineNotExtendedByLateWrite(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	g := &Guard{ctl: w}
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			lines <- strings.TrimSpace(sc.Text())
		}
	}()
	fired := make(chan time.Time, 1)
	done := make(chan watchResult, 1)
	go func() { done <- watchLoop(lines, func(int) { fired <- time.Now() }, discardLogger()) }()
	if err := g.started(300); err != nil {
		t.Fatal(err)
	}

	// The agent decides at t0 that Postgres may run 300ms more ...
	t0 := time.Now()
	budget := 300 * time.Millisecond
	deadline := mono.Now() + budget
	// ... and is frozen for 200ms before the write reaches the pipe.
	time.Sleep(200 * time.Millisecond)
	if err := g.setLeaseUntil(300, deadline); err != nil {
		t.Fatal(err)
	}
	select {
	case at := <-fired:
		if late := at.Sub(t0.Add(budget)); late > 60*time.Millisecond {
			t.Fatalf("the watchdog stopped postgres %s after the deadline the agent worked out", late.Round(time.Millisecond))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the watchdog never fired")
	}
	// A deadline that is already past when it arrives fires at once.
	if err := g.setLeaseUntil(300, mono.Now()-time.Second); err != nil {
		t.Fatal(err)
	}
	sent := time.Now()
	select {
	case at := <-fired:
		if d := at.Sub(sent); d > 60*time.Millisecond {
			t.Fatalf("a past deadline fired %s after it arrived", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a past deadline never fired")
	}
	w.Close()
	<-done
}
