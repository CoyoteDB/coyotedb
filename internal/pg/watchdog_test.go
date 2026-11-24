package pg

import (
	"fmt"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/mono"
)

// leaseIn / armIn are the agent's lines for a deadline ms from now.
func leaseIn(pid, ms int) string {
	return fmt.Sprintf("leaseat %d %d", pid, int64(mono.Now()+time.Duration(ms)*time.Millisecond))
}

func armIn(ms int) string {
	return fmt.Sprintf("armat %d", int64(mono.Now()+time.Duration(ms)*time.Millisecond))
}

// The lease the watchdog enforces: it fires only for the postgres it was
// told about, only if not renewed in time, never for a standby (0), and a
// new postmaster starts without a lease.
func TestWatchLoopLease(t *testing.T) {
	lines := make(chan string)
	fired := make(chan int, 8)
	done := make(chan watchResult, 1)
	go func() { done <- watchLoop(lines, func(pid int) { fired <- pid }, discardLogger()) }()
	send := func(s string) { lines <- s }
	expectNone := func(d time.Duration) {
		t.Helper()
		select {
		case p := <-fired:
			t.Fatalf("lease fired for %d", p)
		case <-time.After(d):
		}
	}
	expect := func(pid int, within time.Duration) {
		t.Helper()
		select {
		case p := <-fired:
			if p != pid {
				t.Fatalf("fired for %d, want %d", p, pid)
			}
		case <-time.After(within):
			t.Fatalf("lease for %d did not fire", pid)
		}
	}

	send("pid 100")
	send(leaseIn(99, 50)) // not the registered postgres: ignored
	expectNone(150 * time.Millisecond)

	send(leaseIn(100, 200)) // renewed before it runs out: no stop
	for range 4 {
		time.Sleep(100 * time.Millisecond)
		send(leaseIn(100, 200))
	}
	expectNone(50 * time.Millisecond)

	send("leaseat 100 0") // a standby: no limit
	expectNone(300 * time.Millisecond)

	send(leaseIn(100, 80)) // not renewed: fires once
	expect(100, 2*time.Second)
	expectNone(200 * time.Millisecond)

	send(leaseIn(100, 80))
	send("pid 101") // a new postmaster clears the old lease
	expectNone(250 * time.Millisecond)

	send("tool 555")
	send("tool 556")
	send("untool 555")
	send("bye")
	close(lines)
	res := <-done
	if res.told != 101 || !res.bye || len(res.tools) != 1 || !res.tools[556] {
		t.Fatalf("result %+v", res)
	}
}

// A primary start is armed before the process exists: the deadline covers
// the postmaster from its first instant, renewals extend it, a start that
// never registers still ends with a stop (pid 0: the caller reads the pid
// file), and disarm cancels a start that did not happen.
func TestWatchLoopArmedStart(t *testing.T) {
	lines := make(chan string)
	fired := make(chan int, 8)
	done := make(chan watchResult, 1)
	go func() { done <- watchLoop(lines, func(pid int) { fired <- pid }, discardLogger()) }()
	send := func(s string) { lines <- s }
	expectNone := func(d time.Duration) {
		t.Helper()
		select {
		case p := <-fired:
			t.Fatalf("lease fired for %d", p)
		case <-time.After(d):
		}
	}
	expect := func(pid int, within time.Duration) {
		t.Helper()
		select {
		case p := <-fired:
			if p != pid {
				t.Fatalf("fired for %d, want %d", p, pid)
			}
		case <-time.After(within):
			t.Fatalf("lease for %d did not fire", pid)
		}
	}

	// Armed, then registered: the armed deadline (not a fresh one) applies.
	start := time.Now()
	send(armIn(300))
	time.Sleep(150 * time.Millisecond)
	send("pid 200")
	expect(200, 2*time.Second)
	if el := time.Since(start); el > 600*time.Millisecond {
		t.Fatalf("fired %s after the arm; the registration must not restart the deadline", el)
	}

	// Armed, registered, renewed while still starting: no stop.
	send(armIn(200))
	send("pid 201")
	for range 4 {
		time.Sleep(100 * time.Millisecond)
		send(leaseIn(201, 200))
	}
	expectNone(50 * time.Millisecond)
	send("leaseat 201 0") // it is a standby now (or stopped in order)
	expectNone(300 * time.Millisecond)

	// Armed, and the agent stalls before it can say pid: still stopped.
	send(armIn(100))
	expect(0, 2*time.Second)
	// ... and spawns it after all: it is stopped the moment it registers.
	send("pid 203")
	expect(203, 500*time.Millisecond)
	expectNone(200 * time.Millisecond)

	// Armed, then the start failed before the process existed.
	send(armIn(100))
	send("disarm")
	expectNone(300 * time.Millisecond)

	// Without an arm a new postmaster has no lease (a standby).
	send("pid 202")
	expectNone(300 * time.Millisecond)
	close(lines)
	<-done
}
