package agent

import (
	"testing"
	"time"
)

// F5: a cut's hold is not ended by the wall clock. A hold this run set
// runs on the monotonic clock: a CutUntil already in the past by the wall
// clock (a forward step, a wrong clock) does not end it. A hold found on
// disk from an earlier run ends only after a whole CutHold of this run's
// monotonic time, even when the wall clock says it is long past.
func TestCutHoldIgnoresWallClock(t *testing.T) {
	a := &Agent{}
	if a.cutExpired(time.Time{}) {
		t.Fatal("no hold, nothing expires")
	}
	// The wall clock stepped 10 minutes forward right after the cut: the
	// persisted CutUntil (set from the old wall clock) already looks past.
	until := time.Now().Add(-10 * time.Minute)
	a.noteCut(until, time.Minute)
	if a.cutExpired(until) {
		t.Fatal("a wall-clock step ended this run's cut hold early")
	}
	a.cutHold.mono = 1 // its monotonic deadline passes
	if !a.cutExpired(until) {
		t.Fatal("the monotonic hold passed but the cut still holds")
	}

	// From an earlier run: the wall clock alone never ends it.
	b := &Agent{}
	old := time.Now().Add(-time.Hour)
	if b.cutExpired(old) {
		t.Fatal("a hold from an earlier run ended by the wall clock alone")
	}
	b.cutHold.seenAt -= 2 * time.Minute // this run has seen more than CutHold
	if !b.cutExpired(old) {
		t.Fatal("a hold from an earlier run never ends")
	}
	// ... and not before its wall time either.
	c := &Agent{}
	future := time.Now().Add(time.Hour)
	c.cutExpired(future)
	c.cutHold.seenAt -= 2 * time.Minute
	if c.cutExpired(future) {
		t.Fatal("ended before its wall-clock time")
	}
}
