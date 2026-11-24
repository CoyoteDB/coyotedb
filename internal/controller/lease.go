package controller

import (
	"fmt"
	"time"
)

// Fractions of the lease at which a primary that has not renewed stops
// its own Postgres (safety rule 4). The controller never promotes another
// node before a full lease plus ControllerMargin has passed since it last
// saw the lease renewed, and it saw that renewal after the primary sent
// it, so a primary that is fenced by FenceKillAt has stopped with at least
// (1 - FenceKillAt) of the lease plus the margin to spare.
const (
	FenceFastAt      = 0.6  // pg_ctl stop -m fast
	FenceImmediateAt = 0.8  // -m immediate
	FenceKillAt      = 0.9  // SIGKILL
	WatchdogAt       = 0.85 // the watchdog's own deadline (immediate, then kill)
)

// LeasePhase is what a primary must be doing about its Postgres now.
type LeasePhase int

const (
	LeaseNone          LeasePhase = iota // no lease: must not run as a primary
	LeaseHeld                            // renewed recently enough
	LeaseStopFast                        // >= 60%: stop fast
	LeaseStopImmediate                   // >= 80%: stop immediate
	LeaseKill                            // >= 90% or lost: kill now
)

func (p LeasePhase) String() string {
	return [...]string{"none", "held", "stop-fast", "stop-immediate", "kill"}[p]
}

// LeaseKeeper is the primary's side of the lease: pure bookkeeping, no
// I/O and no clock of its own (every method takes a Reading), so the math
// is unit-tested with a fake clock.
//
// The lease counts from when a renew request was sent, not from when its
// acknowledgement arrived: the controller can only have seen the renewal
// after it was sent, so the primary's view always ends first.
//
// Time is the Reading's Mono part only: suspend-aware and monotonic, so a
// sleep counts as time passing and a wall-clock step (NTP, VM time sync)
// is not time at all (rule 4, D40).
type LeaseKeeper struct {
	lease time.Duration

	held   bool
	goodAt Reading // send time of the newest acknowledged renewal
	epoch  uint64
	lost   string // non-empty: the cluster said we are no longer primary
	seq    uint64
	sent   map[uint64]Reading
}

// NewLeaseKeeper returns a keeper for a lease of the given length.
func NewLeaseKeeper(lease time.Duration) *LeaseKeeper {
	return &LeaseKeeper{lease: lease, sent: map[uint64]Reading{}}
}

// Lease is the lease length.
func (k *LeaseKeeper) Lease() time.Duration { return k.lease }

// Epoch is the epoch the lease is held for (0 before the first renewal).
func (k *LeaseKeeper) Epoch() uint64 { return k.epoch }

// Sent records that a renew request is about to be sent at at, for epoch.
// It returns a token for Acked.
func (k *LeaseKeeper) Sent(at Reading, epoch uint64) uint64 {
	k.seq++
	k.sent[k.seq] = at
	if len(k.sent) > 64 { // forget requests that never got an answer
		for t := range k.sent {
			if t+32 < k.seq {
				delete(k.sent, t)
			}
		}
	}
	_ = epoch
	return k.seq
}

// Acked records that the renewal with this token succeeded at epoch.
func (k *LeaseKeeper) Acked(token, epoch uint64) {
	at, ok := k.sent[token]
	delete(k.sent, token)
	if !ok || k.lost != "" {
		return
	}
	if k.held && k.epoch != epoch {
		// A renewal for another epoch means our view is confused: never
		// extend a lease across epochs.
		k.lost = fmt.Sprintf("renewed for epoch %d while holding epoch %d", epoch, k.epoch)
		return
	}
	if !k.held || at.Mono > k.goodAt.Mono {
		k.goodAt = at
	}
	k.held, k.epoch = true, epoch
}

// Lost records that the cluster definitively refused the lease (another
// epoch, or another primary): fence at once.
func (k *LeaseKeeper) Lost(why string) {
	if k.lost == "" {
		k.lost = why
	}
}

// LostReason is why the lease was lost ("" if it was not).
func (k *LeaseKeeper) LostReason() string { return k.lost }

// Elapsed is how much of the lease is used up at now: the monotonic,
// suspend-aware time since the newest acknowledged renewal was sent.
func (k *LeaseKeeper) Elapsed(now Reading) time.Duration { return now.Sub(k.goodAt) }

// Phase is what the primary must do at now.
func (k *LeaseKeeper) Phase(now Reading) LeasePhase {
	switch {
	case k.lost != "":
		return LeaseKill
	case !k.held:
		return LeaseNone
	}
	e := k.Elapsed(now)
	switch {
	case e >= frac(k.lease, FenceKillAt):
		return LeaseKill
	case e >= frac(k.lease, FenceImmediateAt):
		return LeaseStopImmediate
	case e >= frac(k.lease, FenceFastAt):
		return LeaseStopFast
	}
	return LeaseHeld
}

// Until returns how long from now until the lease reaches fraction f
// (negative if already past).
func (k *LeaseKeeper) Until(now Reading, f float64) time.Duration {
	return frac(k.lease, f) - k.Elapsed(now)
}

// WatchdogDeadline is the absolute time (Reading.Mono, the clock every
// process on the machine shares) after which the watchdog must stop
// Postgres: WatchdogAt of the lease after the newest acknowledged
// renewal was sent. It depends only on that send time, so working it out
// early or delivering it late never moves it later (F2). A lost lease
// gives a deadline already past. ok is false with no lease at all.
func (k *LeaseKeeper) WatchdogDeadline() (time.Duration, bool) {
	switch {
	case !k.held && k.lost == "":
		return 0, false
	case k.lost != "" || !k.held:
		return 1, true // long past: stop now
	}
	return k.goodAt.Mono + frac(k.lease, WatchdogAt), true
}

func frac(d time.Duration, f float64) time.Duration { return time.Duration(float64(d) * f) }
