package agent

import (
	"sync"
	"time"

	"coyotedb.com/coyote/internal/controller"
	"coyotedb.com/coyote/internal/mono"
)

// The hold on a stream cut (rule 2), measured without the wall clock
// (F5, D40). A cut holds until the epoch moves on, a resume, or its hold
// time passes with the epoch unchanged (the step's controller died). The
// controller's compare-and-set lands within half of that time; the hold
// running out early would let this copy follow the old main node again
// before the new term reaches it.
//
// agent.json keeps CutUntil as a wall-clock time so a restarted agent
// still knows a cut is held. While this agent runs, the hold it set is
// measured on the suspend-aware monotonic clock instead. A hold found on
// disk (set by an earlier run) ends only once the wall clock has passed
// it AND this run has itself seen a whole CutHold go by: a wall clock
// that is wrong or steps forward can never end it early.
type cutHoldClock struct {
	mu     sync.Mutex
	until  time.Time     // the CutUntil this run set
	mono   time.Duration // and its monotonic deadline
	seen   time.Time     // a CutUntil from an earlier run ...
	seenAt time.Duration // ... and when this run first saw it
}

// noteCut records the hold this run just set (until is what is
// persisted as CutUntil).
func (a *Agent) noteCut(until time.Time, hold time.Duration) {
	c := &a.cutHold
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until, c.mono = until, mono.Now()+hold
}

// cutExpired reports whether the persisted hold until has run out.
func (a *Agent) cutExpired(until time.Time) bool {
	if until.IsZero() {
		return false
	}
	now := mono.Now()
	c := &a.cutHold
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mono != 0 && c.until.Equal(until) {
		return now >= c.mono
	}
	if !c.seen.Equal(until) {
		c.seen, c.seenAt = until, now
	}
	return a.wallNow().After(until) && now-c.seenAt >= controller.CutHold
}
