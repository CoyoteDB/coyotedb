// Package mono is the clock every lease measurement uses (safety rule 4):
// monotonic (never steps with NTP or a VM time sync, never runs
// backwards), counting time the machine spends suspended (a laptop lid,
// a paused VM), and the same for every process on the machine, so the
// agent can hand the watchdog an absolute deadline.
//
//   - Linux: CLOCK_BOOTTIME
//   - macOS: CLOCK_MONOTONIC (mach_continuous_time; it counts sleep,
//     unlike the uptime clock Go's own monotonic time uses)
//   - Windows: QueryInterruptTime (counts sleep and hibernation, unlike
//     QueryUnbiasedInterruptTime)
//
// The wall clock is never used for a lease.
package mono

import "time"

// Now is the time since an arbitrary fixed point (in practice, boot). It
// is never zero.
func Now() time.Duration { return now() }
