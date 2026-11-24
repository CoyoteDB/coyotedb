package controller

import (
	"testing"
	"time"
)

func TestLeaseCountsFromSend(t *testing.T) {
	c := NewFakeClock()
	k := NewLeaseKeeper(6 * time.Second)
	if k.Phase(c.Now()) != LeaseNone {
		t.Fatal("no lease before the first renewal")
	}
	tok := k.Sent(c.Now(), 3)
	c.Advance(2 * time.Second) // the ack is slow
	k.Acked(tok, 3)
	// 2s already used: the lease runs from the send, not the ack.
	c.Advance(1599 * time.Millisecond)
	if p := k.Phase(c.Now()); p != LeaseHeld {
		t.Fatalf("at 3.599s: %s", p)
	}
	c.Advance(time.Millisecond)
	if p := k.Phase(c.Now()); p != LeaseStopFast {
		t.Fatalf("at 3.6s (60%%): %s", p)
	}
	c.Advance(1200 * time.Millisecond)
	if p := k.Phase(c.Now()); p != LeaseStopImmediate {
		t.Fatalf("at 4.8s (80%%): %s", p)
	}
	c.Advance(600 * time.Millisecond)
	if p := k.Phase(c.Now()); p != LeaseKill {
		t.Fatalf("at 5.4s (90%%): %s", p)
	}
}

func TestLeaseRenewalsExtendAndOldAcksDoNotShrink(t *testing.T) {
	c := NewFakeClock()
	k := NewLeaseKeeper(6 * time.Second)
	t1 := k.Sent(c.Now(), 1)
	c.Advance(2 * time.Second)
	t2 := k.Sent(c.Now(), 1)
	k.Acked(t2, 1)
	k.Acked(t1, 1) // a late ack of an older request must not move goodAt back
	if e := k.Elapsed(c.Now()); e != 0 {
		t.Fatalf("elapsed %s", e)
	}
	c.Advance(2900 * time.Millisecond)
	if k.Phase(c.Now()) != LeaseHeld {
		t.Fatal("renewal did not extend the lease")
	}
	// Unknown tokens are ignored.
	k.Acked(999, 1)
	if k.Phase(c.Now()) != LeaseHeld {
		t.Fatal("unknown ack changed the lease")
	}
}

func TestLeaseLostAndEpochChange(t *testing.T) {
	c := NewFakeClock()
	k := NewLeaseKeeper(6 * time.Second)
	k.Acked(k.Sent(c.Now(), 4), 4)
	k.Lost("epoch moved to 5")
	if k.Phase(c.Now()) != LeaseKill || k.LostReason() == "" {
		t.Fatal("a lost lease must fence at once")
	}
	k2 := NewLeaseKeeper(6 * time.Second)
	k2.Acked(k2.Sent(c.Now(), 4), 4)
	k2.Acked(k2.Sent(c.Now(), 5), 5)
	if k2.Phase(c.Now()) != LeaseKill {
		t.Fatal("a lease must never be extended across epochs")
	}
}

func TestLeaseSleepCounts(t *testing.T) {
	c := NewFakeClock()
	k := NewLeaseKeeper(6 * time.Second)
	k.Acked(k.Sent(c.Now(), 1), 1)
	c.Advance(500 * time.Millisecond)
	// The laptop slept for a minute: the suspend-aware clock counts it.
	c.Sleep(time.Minute)
	if p := k.Phase(c.Now()); p != LeaseKill {
		t.Fatalf("after sleep: %s (elapsed %s)", p, k.Elapsed(c.Now()))
	}
	// A pause long on the monotonic clock (SIGSTOP) is seen directly.
	k3 := NewLeaseKeeper(6 * time.Second)
	c3 := NewFakeClock()
	k3.Acked(k3.Sent(c3.Now(), 1), 1)
	c3.Advance(10 * time.Second)
	if k3.Phase(c3.Now()) != LeaseKill {
		t.Fatal("paused process must fence on wake")
	}
}

func TestFenceFractionsInOrder(t *testing.T) {
	// fast < immediate < watchdog < kill < lease.
	if !(FenceFastAt < FenceImmediateAt && FenceImmediateAt < WatchdogAt && WatchdogAt < FenceKillAt && FenceKillAt < 1) {
		t.Fatal("fence fractions out of order")
	}
}

// F4 (lease review): a wall-clock step (NTP, VM time sync) on a healthy
// main node is not time passing. Only the suspend-aware monotonic clock
// counts, so a forward step of the wall clock right after a renewal must
// leave the lease held.
func TestLeaseIgnoresWallClockStep(t *testing.T) {
	c := NewFakeClock()
	k := NewLeaseKeeper(10 * time.Second)
	k.Acked(k.Sent(c.Now(), 1), 1)
	c.Advance(100 * time.Millisecond)
	c.StepWall(6 * time.Second) // NTP steps the wall clock forward
	if p := k.Phase(c.Now()); p != LeaseHeld {
		t.Fatalf("after a 6s wall-clock step: %s (elapsed %s)", p, k.Elapsed(c.Now()))
	}
	c.StepWall(-time.Hour) // and backwards
	if p := k.Phase(c.Now()); p != LeaseHeld {
		t.Fatalf("after a backward wall-clock step: %s", p)
	}
}

// The watchdog deadline is fixed by the renewal's send time: asking later
// does not move it, a newer renewal moves it later, an older ack never
// moves it back, and a lost lease gives a deadline already past.
func TestWatchdogDeadlineFromSendTime(t *testing.T) {
	c := NewFakeClock()
	k := NewLeaseKeeper(10 * time.Second)
	if _, ok := k.WatchdogDeadline(); ok {
		t.Fatal("a deadline without a lease")
	}
	sent := c.Now()
	t1 := k.Sent(sent, 1)
	c.Advance(time.Second)
	t2 := k.Sent(c.Now(), 1)
	k.Acked(t1, 1)
	d1, _ := k.WatchdogDeadline()
	if want := sent.Mono + 8500*time.Millisecond; d1 != want {
		t.Fatalf("deadline %s, want %s", d1, want)
	}
	c.Advance(5 * time.Second)
	if d, _ := k.WatchdogDeadline(); d != d1 {
		t.Fatal("asking later moved the deadline")
	}
	k.Acked(t2, 1)
	d2, _ := k.WatchdogDeadline()
	if d2 != d1+time.Second {
		t.Fatalf("newer renewal: %s, want %s", d2, d1+time.Second)
	}
	k.Lost("moved on")
	if d, ok := k.WatchdogDeadline(); !ok || d >= c.Now().Mono {
		t.Fatalf("lost lease: deadline %s ok=%v", d, ok)
	}
}
