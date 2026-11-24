package pg

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/mono"
)

// The watchdog's control protocol (one line per message, agent to
// watchdog), the same on every OS. Deadlines are absolute readings of
// internal/mono (nanoseconds), the suspend-aware monotonic clock every
// process on the machine shares (D39): the agent works a deadline out
// from the send time of the renewal that justifies it, so however late the
// line is written or read, the deadline is never later than the agent's.
//
//	armat NS       the next postgres started is a primary: it may run
//	               until NS (sent before it is spawned, so the deadline
//	               covers it from its first instant, even if the agent
//	               stalls before it can say "pid")
//	disarm         that start did not happen
//	pid N          postgres N was started (without a preceding arm it has
//	               no lease: a standby)
//	leaseat N NS   postgres N may run as a primary until NS (0: no limit,
//	               it is a standby)
//	tool N         a tool process (pg_basebackup, pg_rewind) was started
//	untool N       that tool has exited
//	bye            the agent is shutting down in order
//
// watchLoop below is the part that decides; the OS-specific RunWatchdog
// feeds it lines and gives it a way to stop Postgres.

// watchResult is what the watchdog knows once the agent's pipe ends.
type watchResult struct {
	told  int          // the last postgres pid it was told about
	bye   bool         // the agent said goodbye (orderly shutdown)
	tools map[int]bool // tools still running
}

// watchCheckEvery is how often the deadline is compared with the clock
// besides the timer: Go's timers run on a clock that may stand still while
// the machine is suspended, so after a resume the deadline is seen at
// most this late.
const watchCheckEvery = 50 * time.Millisecond

// watchLoop reads control lines until lines is closed. Whenever a lease
// deadline passes before the agent moves it, it calls leaseExpired with
// the postgres pid, or 0 if the lease was armed for a start whose pid the
// agent never sent (the caller then finds the postmaster in its pid file;
// it checks that the process still is that postgres and stops it at once).
func watchLoop(lines <-chan string, leaseExpired func(pid int), log *slog.Logger) watchResult {
	return watchLoopClock(lines, leaseExpired, log, mono.Now)
}

func watchLoopClock(lines <-chan string, leaseExpired func(pid int), log *slog.Logger, now func() time.Duration) watchResult {
	res := watchResult{tools: map[int]bool{}}
	num := func(line, prefix string) int {
		p, err := strconv.Atoi(strings.TrimPrefix(line, prefix))
		if err != nil || p <= 1 {
			return 0
		}
		return p
	}
	// One primary deadline at a time: armed (leasePID 0, a start is on its
	// way) or for leasePID. deadline 0: none.
	var leasePID int
	var deadline time.Duration
	// armSpent: an armed deadline passed before the agent said which pid
	// it started. The start may still be on its way (the agent stalled
	// between arming and spawning): the next "pid" is stopped at once,
	// unless the agent says "disarm" first.
	armSpent := false
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	tick := time.NewTicker(watchCheckEvery)
	defer timer.Stop()
	defer tick.Stop()
	clearLease := func() {
		timer.Stop()
		leasePID, deadline = 0, 0
	}
	setLease := func(pid int, until time.Duration) {
		leasePID, deadline = pid, max(until, 1)
		timer.Reset(max(deadline-now(), 0))
	}
	armed := func() bool { return deadline != 0 && leasePID == 0 }
	check := func() {
		if deadline == 0 || now() < deadline {
			return
		}
		pid := leasePID
		armSpent = pid == 0
		clearLease()
		log.Warn("the agent did not renew the primary lease in time; stopping postgres now", "postgres_pid", pid)
		leaseExpired(pid)
	}
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return res
			}
			switch {
			case strings.HasPrefix(line, "armat "):
				ns, err := strconv.ParseInt(strings.TrimPrefix(line, "armat "), 10, 64)
				if err != nil || ns <= 0 {
					continue
				}
				armSpent = false
				setLease(0, time.Duration(ns))
				log.Info("primary start armed", "lease_left", (time.Duration(ns) - now()).Round(time.Millisecond))
			case line == "disarm":
				armSpent = false
				if armed() {
					clearLease()
				}
			case strings.HasPrefix(line, "pid "):
				if p := num(line, "pid "); p > 0 {
					res.told = p
					if armSpent {
						armSpent = false
						clearLease()
						log.Warn("a primary postgres registered after its armed lease ran out; stopping it now", "postgres_pid", p)
						leaseExpired(p)
						continue
					}
					if armed() {
						leasePID = p // the armed deadline now covers it
						log.Info("postgres registered as a primary under the armed lease", "postgres_pid", p)
					} else {
						clearLease() // a new postmaster without an arm: no lease
						log.Info("postgres registered", "postgres_pid", p)
					}
				}
			case strings.HasPrefix(line, "leaseat "):
				f := strings.Fields(line)
				if len(f) != 3 {
					continue
				}
				pid, err1 := strconv.Atoi(f[1])
				ns, err2 := strconv.ParseInt(f[2], 10, 64)
				if err1 != nil || err2 != nil || pid != res.told || ns < 0 {
					continue
				}
				clearLease()
				if ns > 0 {
					setLease(pid, time.Duration(ns))
				}
			case strings.HasPrefix(line, "tool "):
				if p := num(line, "tool "); p > 0 {
					res.tools[p] = true
					log.Info("tool registered", "tool_pid", p)
				}
			case strings.HasPrefix(line, "untool "):
				delete(res.tools, num(line, "untool "))
			case line == "bye":
				res.bye = true
			}
			check() // a deadline that is already past fires now
		case <-timer.C:
			check()
		case <-tick.C:
			check()
		}
	}
}
