package agent

import (
	"runtime"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/controller"
)

// A primary start gets the lease's absolute watchdog deadline (so the
// watchdog covers the process from its first instant, and taking it early
// never makes it later), and none at all once the lease starts stopping
// Postgres or is fenced: such a start is refused instead of running a
// Postgres nobody guards.
func TestLeaseDeadlineForPrimaryStart(t *testing.T) {
	clk := controller.NewFakeClock()
	a := &Agent{clock: clk}
	if _, ok := a.leaseDeadline(); ok {
		t.Fatal("a deadline without any lease")
	}
	k := controller.NewLeaseKeeper(10 * time.Second)
	sent := clk.Now()
	k.Acked(k.Sent(sent, 3), 3)
	a.lease.k, a.lease.epoch = k, 3

	d, ok := a.leaseDeadline()
	if want := sent.Mono + 8500*time.Millisecond; !ok || d != want {
		t.Fatalf("fresh lease: deadline %s ok=%v, want %s", d, ok, want)
	}
	clk.Advance(2 * time.Second)
	if d2, ok := a.leaseDeadline(); !ok || d2 != d {
		t.Fatalf("after 2s: deadline %s ok=%v, want the same %s", d2, ok, d)
	}
	clk.Advance(4*time.Second + time.Millisecond) // past the stop-fast point (60%)
	if d, ok := a.leaseDeadline(); ok {
		t.Fatalf("deadline %s after the lease stopped being held", d)
	}
	clk = controller.NewFakeClock()
	a = &Agent{clock: clk}
	k = controller.NewLeaseKeeper(10 * time.Second)
	k.Acked(k.Sent(clk.Now(), 3), 3)
	a.lease.k, a.lease.epoch, a.lease.fencing = k, 3, true
	if _, ok := a.leaseDeadline(); ok {
		t.Fatal("a deadline while fencing")
	}
}

// F3: a fence that runs while a primary start is between taking its
// deadline and spawning vetoes the start (it must not spawn) and waits
// for it to settle, instead of finding nothing to stop and moving on.
func TestFenceVetoesPrimaryStartBeforeSpawn(t *testing.T) {
	clk := controller.NewFakeClock()
	a := &Agent{clock: clk}
	k := controller.NewLeaseKeeper(10 * time.Second)
	k.Acked(k.Sent(clk.Now(), 3), 3)
	a.lease.k, a.lease.epoch = k, 3

	ps := a.beginPrimaryStart()
	if _, ok := a.startDeadline(ps); !ok {
		t.Fatal("a start under a held lease must get its deadline")
	}
	got := make(chan bool, 1)
	go func() { got <- a.fenceTarget() == nil }()
	// The fence marks the veto before it waits.
	for {
		a.pgMu.Lock()
		v := ps.veto
		a.pgMu.Unlock()
		if v {
			break
		}
		runtime.Gosched()
	}
	select {
	case <-got:
		t.Fatal("the fence did not wait for the start to settle")
	default:
	}
	if _, ok := a.startDeadline(ps); ok {
		t.Fatal("a vetoed start still got a deadline (it would spawn)")
	}
	a.endPrimaryStart(ps) // the start gave up without spawning
	if !<-got {
		t.Fatal("nothing was spawned, yet the fence found a server")
	}
}

// Crash restarts back off 1s, 2s, 4s ... up to a minute and never stop.
func TestCrashBackoff(t *testing.T) {
	a := &Agent{}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		if got := a.crashBackoff(); got != w*time.Second {
			t.Fatalf("crash %d: wait %s, want %ds", i+1, got, w)
		}
	}
	if a.crashes < crashesForAttention {
		t.Fatalf("crashes %d", a.crashes)
	}
}
