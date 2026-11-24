// Package controller is CoyoteDB's failover brain. It runs on the Raft
// leader only and owns the primary role and the sync set:
//
//   - leases (rule 4): it watches the primary's lease sequence with its own
//     monotonic clock; the primary side of the lease (renew, self-fence)
//     is LeaseKeeper, used by the primary's agent;
//   - automatic failover (rule 2): wait out the lease, cut the WAL stream
//     on enough members of the sync set R, pick the member with the most
//     WAL, compare-and-set the primary;
//   - the sync set (rules 3 and 9): add caught-up standbys, drop lost ones,
//     degrade to one data node after degrade_after (or never, in strict
//     mode), make copies eligible again once they have caught up;
//   - members gone too long (rule 8): mark them "needs re-clone";
//   - the planned switch.
//
// Decisions are pure functions of what the controller observed (Decide,
// ChooseTarget), table-tested with a fake clock. Side effects go through
// two small interfaces (Cluster: Raft; Agents: the other nodes' agents),
// and every write is a compare-and-set on the epoch made right after a
// leadership check, so a deposed controller's late writes fail.
package controller

import (
	"time"

	"coyotedb.com/coyote/internal/mono"
)

// Reading is one clock reading. Every duration this package measures
// (leases above all, rule 4) comes from Mono: monotonic, counting time the
// machine was suspended, shared by every process on the machine
// (internal/mono). Wall is only for timestamps shown to people or stored
// as "since" times; it never measures a duration here.
type Reading struct {
	Mono time.Duration // internal/mono: monotonic, suspend-aware
	Wall time.Time     // wall clock, display only
}

// Sub is the monotonic time from r to later.
func (r Reading) Sub(earlier Reading) time.Duration { return r.Mono - earlier.Mono }

// IsZero reports whether r is the zero reading (never taken).
func (r Reading) IsZero() bool { return r.Mono == 0 && r.Wall.IsZero() }

// Clock gives readings. Tests use FakeClock.
type Clock interface {
	Now() Reading
}

// SystemClock reads internal/mono (and the wall clock for display).
type SystemClock struct{}

// NewSystemClock returns the system clock. Its monotonic readings are
// never zero, so a reading is never mistaken for "never taken".
func NewSystemClock() *SystemClock { return &SystemClock{} }

// Now implements Clock.
func (c *SystemClock) Now() Reading {
	return Reading{Mono: mono.Now(), Wall: time.Now().Round(0)}
}

// FakeClock is a manual clock for tests.
type FakeClock struct{ r Reading }

// NewFakeClock starts at a fixed time.
func NewFakeClock() *FakeClock {
	return &FakeClock{r: Reading{Mono: time.Hour, Wall: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}}
}

// Now implements Clock.
func (c *FakeClock) Now() Reading { return c.r }

// Advance moves both clocks forward by d.
func (c *FakeClock) Advance(d time.Duration) { c.r.Mono += d; c.r.Wall = c.r.Wall.Add(d) }

// Sleep is a suspend (laptop lid, paused VM) of d: the suspend-aware
// monotonic clock counts it like any other time.
func (c *FakeClock) Sleep(d time.Duration) { c.Advance(d) }

// StepWall moves only the wall clock by d (an NTP or VM time-sync step).
func (c *FakeClock) StepWall(d time.Duration) { c.r.Wall = c.r.Wall.Add(d) }
