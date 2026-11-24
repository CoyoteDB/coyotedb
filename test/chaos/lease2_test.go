//go:build chaos && unix

package chaos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/pg"
	"coyotedb.com/coyote/test/chaos/check"
)

// Scenarios for the lease review of 2026-11-21 (findings F1-F6). Each sets up the exact shape a
// finding describes; the verdict is the run's final Check (lost writes,
// two main nodes from the commit probes, timeline order), not anything
// the scenario asserts about the product's own reports. Scenarios that
// need a test hook the agent does not have yet fail with "hook missing"
// instead of passing on a fault that never happened.
func init() {
	scenarios = append(scenarios,
		Scenario{Name: "sync-k2-raise-then-failover", Topo: ThreeTwoWitness, Body: syncK2RaiseThenFailover},
		Scenario{Name: "watchdog-stale-budget", Topo: Three, Body: watchdogStaleBudget},
		Scenario{Name: "stall-before-primary-spawn", Topo: Three, Env: []string{faultEnv + "stall-before-primary-spawn"}, Body: stallBeforePrimarySpawn},
		Scenario{Name: "wall-clock-step-on-main", Topo: Three, Env: []string{wallStepEnv + "=wall-step"}, Body: wallClockStepOnMain},
		Scenario{Name: "switch-with-frozen-old-main", Topo: Three, Body: switchWithFrozenOldMain},
	)
}

// F1: k raised to 2 while a copy lags. R = {b, c}, k = 1, the stream to
// c is cut silently (blackhole), so acknowledged writes are on a and b
// only. `dev set-sync --count 2` then records k = 2 in R. The main node a
// loses power and b is cut off at the same moment. Counted with k = 2,
// rule 2 needs one cut copy and c alone would do; but the writes were
// acknowledged with k = 1, so promoting c loses every write acknowledged
// while c lagged. Correct behaviour: no failover until b is back (or b
// is chosen). Two witnesses keep a quorum with a and b gone.
func syncK2RaiseThenFailover(r *Run) {
	p := r.Primary()
	s := r.Standbys(p)
	b, c := s[0], s[1]
	r.C.Log.Mark("blackhole the stream " + p.Name + " -> " + c.Name)
	r.C.Net.SetFlow(p.Name, c.Name, Blackhole)
	r.Let(4 * time.Second)
	lb, lc := r.receiveLSN(b), r.receiveLSN(c)
	r.C.Log.Event("received: %s %s, %s %s", b.Name, lb, c.Name, lc)
	if lc >= lb {
		r.C.Fail("precondition: %s (%s) is not behind %s (%s); the blackhole did not make it lag", c.Name, lc, b.Name, lb)
	}
	var acked int
	for _, op := range r.W.H.snapshotAppends() {
		if op.Outcome == OK && op.Start.After(time.Now().Add(-3*time.Second)) {
			acked++
		}
	}
	if acked == 0 {
		r.C.Fail("precondition: no write was acknowledged while %s lagged", c.Name)
	}
	r.C.Log.Event("%d writes acknowledged in the last 3 s while %s lagged", acked, c.Name)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		out, err := p.CLI(ctx, "dev", "set-sync", "--standbys", "node-"+b.Name+",node-"+c.Name, "--count", "2")
		if err != nil {
			err = errors.New(strings.TrimSpace(out) + ": " + err.Error())
		}
		done <- err
	}()
	k2 := poll(15*time.Second, func() bool {
		select {
		case err := <-done:
			r.C.Log.Event("set-sync --count 2 returned: %v", err)
			done <- err
			return true
		default:
		}
		st, ok := b.Status()
		return ok && st.Cluster.SyncK >= 2
	})
	if k2 != nil {
		r.C.Fail("set-sync --count 2 neither recorded k = 2 nor returned within 15 s")
	}
	st, _ := b.Status()
	r.C.Log.Event("cluster sync set %v, k = %d", st.Cluster.SyncSet, st.Cluster.SyncK)

	t0 := r.C.Log.Mark("power cut " + p.Name + ", cut off " + b.Name)
	r.C.Net.Isolate(b.Name, r.C.Names(), Blackhole)
	r.C.PowerCut(p)
	r.Let(30 * time.Second)
	who := r.C.PrimaryByCluster()
	r.C.Log.Event("%s after the double fault: main node %q (a failover to %s here loses the writes only %s had)",
		time.Since(t0).Round(time.Second), who, c.Name, b.Name)
	select {
	case err := <-done:
		r.C.Log.Event("set-sync --count 2 returned: %v", err)
	default:
	}
}

// receiveLSN is how far n's copy has received (flushed) WAL.
func (r *Run) receiveLSN(n *Node) pg.LSN {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := r.C.Connect(ctx, n)
	if err != nil {
		r.C.Fail("connect to %s: %v", n.Name, err)
	}
	defer conn.Close(context.Background())
	var s *string
	if err := conn.QueryRow(ctx, "SELECT pg_last_wal_receive_lsn()::text").Scan(&s); err != nil || s == nil {
		r.C.Fail("receive lsn on %s: %v", n.Name, err)
	}
	l, err := pg.ParseLSN(*s)
	if err != nil {
		r.C.Fail("receive lsn on %s: %v", n.Name, err)
	}
	return l
}

// F2: the watchdog starts the budget it is given from when it reads it.
// Freeze the main node's watchdog, let the agent push a fresh budget
// (renewals every second), freeze the agent, and wake the watchdog 5 s
// later, before its old deadline: it now reads a budget computed 5 s ago
// and counts it from now. That is exactly an agent frozen between
// computing a budget and writing it (F2), done from outside. The agent
// stays frozen, so the watchdog is the only guard: its deadline (about
// 13.5 s after the last renewal) is past the earliest promotion (lease +
// margin, about 11 s), and the old Postgres keeps accepting writes after
// the new main node is promoted. A fixed watchdog (absolute deadline on a
// shared clock, or capped by the renewal's send time) stops Postgres by
// 8.5 s after the last renewal however late it reads.
func watchdogStaleBudget(r *Run) {
	// The Raft leader must be a copy: a controller newly elected after
	// the main node froze waits a full lease more before promoting,
	// which would hide the overrun behind its own delay.
	p, _ := r.leaderOnCopy()
	r.WaitServed(p, time.Minute)
	if !p.StopWatchdog() {
		r.C.Fail("no watchdog on %s", p.Name)
	}
	r.Let(1200 * time.Millisecond) // at least one renewal, so a budget line waits unread in the pipe
	t0 := r.C.Log.Mark("freeze " + p.Name + "'s agent (its watchdog frozen too)")
	p.Stop(false)
	r.Let(5 * time.Second) // the unread budget grows stale; short of the watchdog's old deadline
	p.ContWatchdog()
	r.C.Log.Event("watchdog of %s woken: it reads a 5 s old budget", p.Name)
	r.WaitNewPrimary(p, t0, time.Minute)
	r.Let(10 * time.Second)
	r.expectStoppedBy(p, t0, time.Now())
	p.Cont()
	r.Let(10 * time.Second)
}

// frozenStopBound: an agent frozen at t0 sent its last renewal before t0,
// so its watchdog must stop Postgres by t0 + 85% of the 10 s lease (D33);
// 1 s more for the stop itself.
const frozenStopBound = 8500*time.Millisecond + time.Second

// expectStoppedBy: n's agent was frozen at t0 (and stayed frozen until
// until). Its Postgres must not have committed anything after t0 +
// frozenStopBound, from n's own commit probes (a commit seen in a probe
// sent after the bound happened after it).
func (r *Run) expectStoppedBy(n *Node, t0, until time.Time) {
	h := r.W.H
	h.mu.Lock()
	base := h.Base
	var last *check.Probe
	for i := range h.Probes {
		q := h.Probes[i]
		if q.Node == n.Name && q.Kind == check.RW && base.Add(q.Start).Before(until) && (last == nil || q.Start > last.Start) {
			last = &h.Probes[i]
		}
	}
	h.mu.Unlock()
	if last == nil {
		return
	}
	after := base.Add(last.Start).Sub(t0)
	r.C.Log.Event("%s's Postgres committed last %s after its agent froze (bound %s)", n.Name, after.Round(time.Millisecond), frozenStopBound)
	if after > frozenStopBound {
		r.Problem("LEASE OVERRUN: %s's Postgres committed a write %s after its agent froze; its lease allows at most %s (rule 4)",
			n.Name, after.Round(time.Millisecond), frozenStopBound)
	}
}

// F3: the budget for the watchdog is taken before the slow work that
// precedes a primary's spawn (node info, TLS, config, all fsynced). Needs
// the hook COYOTE_TEST_FAULT=stall-before-primary-spawn: on the first
// primary start after a cleared fence, the agent says "Test fault point
// reached: stall-before-primary-spawn" and sleeps 8 s between taking the
// lease budget and spawning Postgres (a disk that stalls there). The
// agent is frozen 7.5 s first (as in slow-start-after-fence) so it fences
// and then clears the fence. During the stall its lease loop reaches 60%,
// finds nothing to stop and drops the lease; renewals stop, another node
// is promoted, and the stalled start then spawns a primary on the stale
// budget. Correct: the fence vetoes the start, or the budget is taken
// right before the spawn.
func stallBeforePrimarySpawn(r *Run) {
	p := r.Primary()
	p.Stop(false)
	r.Let(7500 * time.Millisecond) // past 60% of the 10 s lease, short of lease + margin
	p.Cont()
	p.WaitOut("lost its main-node lease", 15*time.Second)
	const point = "Test fault point reached: stall-before-primary-spawn"
	if err := p.waitOut(point, time.Minute); err != nil {
		r.C.Fail("hook missing in the agent: COYOTE_TEST_FAULT=stall-before-primary-spawn was never reached (%v)", err)
	}
	r.C.Log.Mark("start of " + p.Name + " stalls before the spawn")
	r.Let(30 * time.Second)
}

// wallStepEnv names the F4 hook: while <cluster dir>/<file> holds a Go
// duration, the agent's wall clock readings (lease and cut hold) are that
// far ahead, monotonic time unchanged, and the agent says "Test fault
// point reached: wall clock stepped by <d>". A real step needs root.
const wallStepEnv = "COYOTE_TEST_WALL_STEP_FILE"

// F4: an 8 s forward wall-clock step (NTP, VM time sync) on a healthy
// main node must not make it fence itself; rule 4 says monotonic only.
func wallClockStepOnMain(r *Run) {
	p := r.Primary()
	epoch, fences := r.C.Epoch(), fenceCount(p)
	r.C.Log.Mark("step " + p.Name + "'s wall clock 8 s forward")
	if err := os.WriteFile(filepath.Join(clusterDir(r, p), "wall-step"), []byte("8s"), 0o600); err != nil {
		r.C.Fail("write the step file: %v", err)
	}
	if err := p.waitOut("Test fault point reached: wall clock stepped", 15*time.Second); err != nil {
		r.C.Fail("hook missing in the agent: %s had no effect (%v)", wallStepEnv, err)
	}
	r.Let(15 * time.Second)
	r.expectNoFence(p, epoch, fences)
}

// F6: a planned switch while the old main node's agent is frozen. The
// switch runs on the Raft leader (a copy), needs nothing from the old
// main node, cuts the copies and records the target; the old main node
// would stop only when its agent sees the new term, and it is frozen, so
// its Postgres keeps accepting (unacknowledgeable) writes until the
// watchdog fires, while the target is already promoted. Correct: the
// switch stops the old Postgres first, or waits out its lease.
func switchWithFrozenOldMain(r *Run) {
	p, l := r.leaderOnCopy()
	to := l
	for _, s := range r.Standbys(p) {
		if s != l {
			to = s
		}
	}
	r.WaitServed(p, time.Minute)
	t0 := r.C.Log.Mark("freeze " + p.Name + "'s agent, then switch to " + to.Name)
	p.Stop(false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, err := l.CLI(ctx, "switch", "--to", "node-"+to.Name)
	r.C.Log.Event("switch: %v %s", err, strings.TrimSpace(out))
	if err != nil {
		r.C.Fail("switch with the old main node frozen: %v\n%s", err, out)
	}
	r.WaitNewPrimary(p, t0, time.Minute)
	r.Let(12 * time.Second) // past the old main node's watchdog deadline
	r.expectStoppedBy(p, t0, time.Now())
	p.Cont()
	r.Let(10 * time.Second)
}
