//go:build chaos && unix

package chaos

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// Recovery stories: a node goes down, comes back with no command from
// anyone (its process is started again, as a service manager or a reboot
// would), catches up, holds exactly the main node's data, and is then
// proven good enough to be the main node itself. The run's final Check
// (lost writes, identical nodes, linearizability, one main node at a
// time) applies to each as to every scenario.
func init() {
	scenarios = append(scenarios,
		Scenario{Name: "copy-down-long-then-back", Topo: TwoWitness, Body: func(r *Run) {
			// The only copy is down long enough that the main node goes on
			// alone (degrade_after) and writes a lot without it.
			p := r.Primary()
			s := r.Standbys(p)[0]
			from := r.flushLSN(p)
			s.Kill9()
			r.bulk(40)
			r.Let(5 * time.Second)
			r.C.Log.Event("WAL written while %s was down: %d MB", s.Name, (r.flushLSN(p)-from)>>20)
			r.t.Logf("WAL written while %s was down: %d MB", s.Name, (r.flushLSN(p)-from)>>20)
			t1 := r.C.Log.Mark("restart " + s.Name)
			s.Restart()
			r.waitBack(s, p, t1, 3*time.Minute)
			r.expectSameData(s, p)
			// The returned copy takes over and serves.
			t2 := r.HardKill(p)
			if n := r.WaitNewPrimary(p, t2, time.Minute); n != s {
				r.C.Fail("%s took over, want the returned copy %s", n.Name, s.Name)
			}
			r.Let(8 * time.Second)
		}},
		Scenario{Name: "old-main-back-after-failover", Topo: Three, Body: func(r *Run) {
			p := r.Primary()
			t0 := r.HardKill(p)
			n := r.WaitNewPrimary(p, t0, time.Minute)
			r.Let(5 * time.Second)
			r.bulk(20)
			t1 := r.C.Log.Mark("restart old main node " + p.Name)
			p.Restart()
			r.waitBack(p, n, t1, 3*time.Minute)
			r.expectSameData(p, n)
			r.C.Log.Event("%s rejoined by: %s", p.Name, rejoinHow(p.Output()))
			r.t.Logf("%s rejoined by: %s", p.Name, rejoinHow(p.Output()))
			// Planned switch back to it; it serves.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if out, err := n.CLI(ctx, "switch", "--to", "node-"+p.Name); err != nil {
				r.C.Fail("switch to the returned node: %v\n%s", err, out)
			}
			if got := r.WaitNewPrimary(n, time.Now(), time.Minute); got != p {
				r.C.Fail("%s is the main node after the switch, want %s", got.Name, p.Name)
			}
			r.Let(5 * time.Second)
		}},
		Scenario{Name: "whole-cluster-off-and-on", Topo: Three, Body: func(r *Run) {
			// Power cut for every machine at once, mid-write; they come
			// back one by one in a random order with random gaps.
			r.C.Log.Mark("power cut: every node")
			r.C.PowerCut(r.C.Members()...)
			r.Let(3 * time.Second)
			order := r.C.Members()
			r.Rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			var quorum time.Time
			for i, n := range order {
				if i > 0 {
					r.Let(time.Duration(1+r.Rng.IntN(6)) * time.Second)
				}
				r.C.Log.Event("power back: %s", n.Name)
				n.Launch("up", "")
				if i == 1 {
					quorum = time.Now()
				}
			}
			r.waitResume(quorum, ResumeBudget)
		}},
		Scenario{Name: "whole-cluster-off-minority-first", Topo: Three, Body: func(r *Run) {
			// Only one of three comes back: it has no majority, so it must
			// not take writes. Once a second one is back, writes resume.
			r.C.Log.Mark("power cut: every node")
			r.C.PowerCut(r.C.Members()...)
			r.Let(3 * time.Second)
			order := r.C.Members()
			r.Rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			first := order[0]
			t0 := r.C.Log.Mark("power back: " + first.Name + " alone")
			first.Launch("up", "")
			r.Let(20 * time.Second)
			if !first.Running() {
				r.C.Fail("%s exited while waiting for the others", first.Name)
			}
			if rep, ok := first.Report(); ok && rep.PG != nil && rep.PG.Role == "primary" {
				r.C.Fail("%s runs read-write without a majority", first.Name)
			}
			for _, op := range r.W.H.snapshotAppends() {
				if op.Outcome == OK && op.Start.After(t0) {
					r.C.Fail("write %d acknowledged by %s with one node of three up", op.ID, op.Node)
				}
			}
			t1 := r.C.Log.Mark("power back: " + order[1].Name + " (majority)")
			order[1].Launch("up", "")
			r.waitResume(t1, ResumeBudget)
			r.Let(3 * time.Second)
			order[2].Launch("up", "")
		}},
		Scenario{Name: "flapping-copy", Topo: Three, Body: func(r *Run) {
			// A copy dies again as soon as it is back, five times, by
			// agent kill and by power cut in turn.
			p := r.Primary()
			s := r.Standbys(p)[0]
			for i := range 5 {
				if i%2 == 0 {
					s.Kill9()
				} else {
					r.C.PowerCut(s)
				}
				s.Restart()
			}
			t1 := r.C.Log.Mark("last restart of " + s.Name)
			r.waitBack(s, p, t1, 3*time.Minute)
			r.expectSameData(s, p)
			r.expectOneOfEach(s)
		}},
	)
}

// waitResume waits until a write started after t is acknowledged, at
// most d after t, and logs how long that took.
func (r *Run) waitResume(t time.Time, d time.Duration) {
	var got time.Duration
	err := poll(time.Until(t.Add(d)), func() bool {
		got = ResumeAfter(r.W.H, t)
		return got >= 0
	})
	if err != nil {
		r.C.Fail("no write was acknowledged within %s", d)
	}
	r.C.Log.Event("writes resumed %s after the cluster had a majority again", got.Round(time.Millisecond))
	r.t.Logf("writes resumed %s after the cluster had a majority again", got.Round(time.Millisecond))
}

// PowerCut SIGKILLs every process of the given nodes at the same moment
// (agents, watchdogs, postmasters and their children), like pulling the
// plug on those machines.
func (c *Cluster) PowerCut(nodes ...*Node) {
	ps := procs()
	kill := map[int]bool{}
	for _, n := range nodes {
		c.Log.Event("power cut %s", n.Name)
		for _, p := range ps {
			if strings.Contains(p.cmd, n.Dir) && !strings.HasPrefix(p.cmd, "ps ") {
				kill[p.pid] = true
			}
		}
	}
	for range 2 { // children (watchdog, backends), then theirs
		for _, p := range ps {
			if kill[p.ppid] {
				kill[p.pid] = true
			}
		}
	}
	for pid := range kill {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	for _, n := range nodes {
		n.waitExit(30 * time.Second)
	}
}

// flushLSN is p's current WAL flush position.
func (r *Run) flushLSN(p *Node) pg.LSN {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := r.C.Connect(ctx, p)
	if err != nil {
		r.C.Fail("connect to %s: %v", p.Name, err)
	}
	defer conn.Close(context.Background())
	var s string
	if err := conn.QueryRow(ctx, "SELECT pg_current_wal_flush_lsn()::text").Scan(&s); err != nil {
		r.C.Fail("flush lsn on %s: %v", p.Name, err)
	}
	l, err := pg.ParseLSN(s)
	if err != nil {
		r.C.Fail("flush lsn on %s: %v", p.Name, err)
	}
	return l
}

// waitBack: n was started again at t0. Waits until the main node p sees
// it streaming, then synchronous and back in the sync set with the copies
// eligible again, and records both times.
func (r *Run) waitBack(n, p *Node, t0 time.Time, within time.Duration) {
	var streaming time.Time
	err := poll(within, func() bool {
		s, ok := p.Status()
		if !ok {
			return false
		}
		for _, m := range s.Nodes {
			if m.Name != "node-"+n.Name {
				continue
			}
			if streaming.IsZero() && strings.HasPrefix(m.Replication, "streaming") {
				streaming = time.Now()
			}
			return m.Replication == "streaming (sync)" && m.InSyncSet && m.Status == "active" && s.Cluster.StandbyEligible
		}
		return false
	})
	if err != nil {
		r.C.Fail("%s was not streaming and back in the sync set within %s of its restart", n.Name, within)
	}
	inSync := time.Since(t0)
	record(&backStreaming, streaming.Sub(t0))
	record(&backInSync, inSync)
	msg := fmt.Sprintf("%s back: streaming %s, in the sync set %s after its restart",
		n.Name, streaming.Sub(t0).Round(10*time.Millisecond), inSync.Round(10*time.Millisecond))
	r.C.Log.Event("%s", msg)
	r.t.Logf("%s", msg)
}

// expectSameData pauses the workload, waits until n has replayed
// everything p has flushed, and compares the row counts and content hashes
// of the test tables, each read directly on its own node.
func (r *Run) expectSameData(n, p *Node) {
	r.W.Pause()
	defer r.W.Resume()
	want := r.flushLSN(p)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err := poll(time.Minute, func() bool {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		cn, err := r.C.Connect(cctx, n)
		if err != nil {
			return false
		}
		defer cn.Close(context.Background())
		var s *string
		if cn.QueryRow(cctx, "SELECT pg_last_wal_replay_lsn()::text").Scan(&s) != nil || s == nil {
			return false
		}
		l, err := pg.ParseLSN(*s)
		return err == nil && l >= want
	})
	if err != nil {
		r.C.Fail("%s did not replay up to %s's %s", n.Name, p.Name, want)
	}
	counts := func(x *Node) (int64, string) {
		cn, err := r.C.Connect(ctx, x)
		if err != nil {
			r.C.Fail("connect to %s: %v", x.Name, err)
		}
		defer cn.Close(context.Background())
		var k int64
		if err := cn.QueryRow(ctx, "SELECT count(*) FROM chaos_append").Scan(&k); err != nil {
			r.C.Fail("count on %s: %v", x.Name, err)
		}
		h, err := r.C.tableHash(ctx, x)
		if err != nil {
			r.C.Fail("hash on %s: %v", x.Name, err)
		}
		return k, h
	}
	kp, hp := counts(p)
	kn, hn := counts(n)
	if kn != kp || hn != hp {
		r.C.Fail("DIVERGED: %s has %d rows (hash %s), the main node %s has %d (hash %s)", n.Name, kn, hn, p.Name, kp, hp)
	}
	r.C.Log.Event("%s holds exactly %s's data: %d rows, hash %s", n.Name, p.Name, kn, hn)
}

// expectOneOfEach: n runs exactly one agent, one watchdog and one
// postmaster, and no clone or rewind tool.
func (r *Run) expectOneOfEach(n *Node) {
	agent := n.AgentPID()
	var watchdogs, postmasters int
	pgdata := filepath.Join(n.Dir, "pgdata")
	for _, p := range procs() {
		switch {
		case p.ppid == agent && strings.Contains(p.cmd, pg.WatchdogCommand):
			watchdogs++
		case strings.Contains(p.cmd, "-D") && strings.Contains(p.cmd, pgdata):
			postmasters++
		case strings.Contains(p.cmd, n.Dir) && (strings.Contains(p.cmd, "pg_basebackup") || strings.Contains(p.cmd, "pg_rewind")):
			r.C.Fail("%s: a leftover tool is running: %s", n.Name, p.cmd)
		}
	}
	if !n.Running() || watchdogs != 1 || postmasters != 1 {
		r.C.Fail("%s: agent running %v, %d watchdogs, %d postmasters; want one of each", n.Name, n.Running(), watchdogs, postmasters)
	}
}

// rejoinHow says from an agent's output how it came back as a copy.
func rejoinHow(out string) string {
	switch {
	case strings.Contains(out, "Rewind was not possible"), strings.Contains(out, "Copied the data again"):
		return "copying the data again"
	case strings.Contains(out, "steps down and rejoins as a copy"):
		return "rewind"
	default:
		return "following directly"
	}
}
