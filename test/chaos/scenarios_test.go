//go:build chaos && unix

package chaos

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	coyoteagent "coyotedb.com/coyote/internal/agent"
)

const faultEnv = "COYOTE_TEST_FAULT="

// scenarios is the named list `make chaos` runs.
var scenarios = []Scenario{
	{Name: "kill9-primary", Topo: Three, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.HardKill(p)
		r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(8 * time.Second)
	}},
	// Proxy in the path (rule 10): the clients are applications on the
	// client ports. Rule 10 is checked on every acknowledged operation
	// (checkRule10) plus the proxies' own counters below.
	{Name: "proxy-kill9-primary", Topo: Three, ViaProxy: true, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.HardKill(p)
		r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(8 * time.Second)
		r.expectProxy("every client connection went through a proxy and none was refused for plaintext",
			func(sum coyoteagent.ProxyReport, _ map[string]coyoteagent.ProxyReport) bool {
				return sum.Forwarded >= 9 && sum.RefusedPlain == 0
			})
	}},
	{Name: "proxy-isolate-primary", Topo: Three, ViaProxy: true, Body: func(r *Run) {
		// The cut-off main node's proxy loses its view within the lease and
		// drops and refuses its clients. The other proxies close what they
		// had piped to it, either on the epoch change or, while the new
		// leader is being elected, because they too briefly have no
		// trustworthy view; new connections then reach the new main node.
		p := r.Primary()
		t0 := r.C.Log.Mark("isolate " + p.Name)
		r.C.Net.Isolate(p.Name, r.C.Names(), Blackhole)
		r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(10 * time.Second)
		r.expectProxy("the isolated node's proxy dropped or refused clients (no view) and the others closed their connections to it",
			func(sum coyoteagent.ProxyReport, per map[string]coyoteagent.ProxyReport) bool {
				old := per[p.Name]
				others := (sum.DroppedOnEpoch + sum.DroppedNoView) - (old.DroppedOnEpoch + old.DroppedNoView)
				return old.DroppedNoView+old.RefusedNoView >= 1 && others >= 1
			})
	}},
	{Name: "proxy-witness-primary-gone-for-good", Topo: TwoWitness, ViaProxy: true, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.HardKill(p)
		n := r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(15 * time.Second)
		r.expectStatus(n, "Running on one copy")
		r.expectResumeWithin(t0, OneCopyResume)
	}},
	{Name: "kill9-primary-witness", Topo: TwoWitness, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.HardKill(p)
		r.WaitNewPrimary(p, t0, time.Minute)
		// Writes wait for the old primary to rejoin (it is put in the sync
		// set): bring it back.
		r.Let(5 * time.Second)
		p.Restart()
		r.Let(5 * time.Second)
	}},
	{Name: "witness-primary-gone-for-good", Topo: TwoWitness, Body: func(r *Run) {
		// The old main node never returns: its node was the only other data
		// node, so the new one runs on one copy at once (rule 9) instead of
		// making writes wait for degrade_after.
		p := r.Primary()
		t0 := r.HardKill(p)
		n := r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(15 * time.Second)
		r.expectStatus(n, "Running on one copy")
		r.expectResumeWithin(t0, OneCopyResume)
	}},
	{Name: "three-lose-two-in-sequence", Topo: ThreeTwoWitness, Body: func(r *Run) {
		// Lose the main node; once the new one has served, lose it too: the
		// last data node runs on one copy at once and writes go on. (With
		// three voters only, losing two loses the quorum and nothing can
		// take over; the two witnesses keep a quorum of five.)
		p := r.Primary()
		t0 := r.HardKill(p)
		n := r.WaitNewPrimary(p, t0, time.Minute)
		r.WaitServed(n, time.Minute)
		r.Let(3 * time.Second)
		t1 := r.HardKill(n)
		last := r.WaitNewPrimary(n, t1, time.Minute)
		r.Let(15 * time.Second)
		r.expectStatus(last, "Running on one copy")
		r.expectResumeWithin(t1, OneCopyResume)
	}},
	{Name: "kill9-primary-five", Topo: Five, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.HardKill(p)
		n := r.WaitNewPrimary(p, t0, time.Minute)
		r.WaitServed(n, time.Minute)
		r.Let(5 * time.Second)
		t1 := r.HardKill(n)
		r.WaitNewPrimary(n, t1, time.Minute)
		r.Let(5 * time.Second)
	}},
	{Name: "five-primary-and-copy-down-blocks-then-recovers", Topo: Five, Body: func(r *Run) {
		// k=1 over four copies: with the primary and one copy gone, the
		// gone copy may hold a write the others lack (rule 2): no failover
		// until it is back.
		p := r.Primary()
		s := r.Standbys(p)[0]
		s.Kill9()
		t0 := r.C.Log.Mark("primary down too")
		p.Kill9()
		r.Let(15 * time.Second)
		if name := r.C.PrimaryByCluster(); name != p.Name {
			r.C.Fail("failed over to %s although a copy that may hold the latest writes was down", name)
		}
		r.expectStatus(r.Standbys(p)[1], "safety rule 2")
		s.Restart()
		r.WaitNewPrimary(p, t0, 2*time.Minute)
	}},
	{Name: "sigstop-primary-agent", Topo: Three, Body: func(r *Run) {
		// The agent freezes but Postgres keeps running: the watchdog must
		// stop it when the lease runs out, before anyone is promoted.
		p := r.Primary()
		t0 := r.C.Log.Mark("freeze " + p.Name)
		p.Stop(false)
		r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(5 * time.Second)
		r.expectStoppedBy(p, t0, time.Now())
		p.Cont()
		r.Let(10 * time.Second)
	}},
	{Name: "sigstop-primary-agent-and-postmaster", Topo: Three, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.C.Log.Mark("freeze " + p.Name)
		p.Stop(true)
		r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(10 * time.Second)
		p.Cont()
		r.Let(10 * time.Second)
	}},
	{Name: "kill-postmaster-primary", Topo: Three, Body: func(r *Run) {
		p := r.Primary()
		if !p.KillPostmaster() {
			r.C.Fail("no postmaster on %s", p.Name)
		}
		r.Let(20 * time.Second)
	}},
	{Name: "kill-watchdog-primary", Topo: Three, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.C.Log.Mark("watchdog of " + p.Name)
		if !p.KillWatchdog() {
			r.C.Fail("no watchdog on %s", p.Name)
		}
		r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(5 * time.Second)
	}},
	{Name: "isolate-primary-blackhole", Topo: Three, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.C.Log.Mark("isolate " + p.Name)
		r.C.Net.Isolate(p.Name, r.C.Names(), Blackhole)
		r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(10 * time.Second)
	}},
	{Name: "isolate-primary-drop", Topo: TwoWitness, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.C.Log.Mark("isolate " + p.Name)
		r.C.Net.Isolate(p.Name, r.C.Names(), Drop)
		r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(10 * time.Second)
	}},
	{Name: "primary-cannot-send", Topo: Three, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.C.Log.Mark("one-way cut out of " + p.Name)
		for _, o := range r.C.Names() {
			if o != p.Name {
				r.C.Net.SetFlow(p.Name, o, Blackhole)
			}
		}
		r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(10 * time.Second)
	}},
	{Name: "primary-cannot-hear", Topo: Three, Body: func(r *Run) {
		p := r.Primary()
		r.C.Log.Mark("one-way cut into " + p.Name)
		for _, o := range r.C.Names() {
			if o != p.Name {
				r.C.Net.SetFlow(o, p.Name, Blackhole)
			}
		}
		r.Let(25 * time.Second)
	}},
	{Name: "partition-minority-standby", Topo: Five, Body: func(r *Run) {
		p := r.Primary()
		s := r.Standbys(p)
		r.C.Net.Partition([][]string{{s[0].Name, s[1].Name}, append([]string{p.Name, s[2].Name, s[3].Name}, witnessNames(r)...)}, Blackhole)
		r.Let(25 * time.Second)
	}},
	{Name: "isolate-copy-proves-proxy", Topo: Three, Body: func(r *Run) {
		// Every byte between agents goes through the proxies: cutting one
		// copy's links must disconnect it completely.
		p := r.Primary()
		s := r.Standbys(p)[0]
		r.C.Net.Isolate(s.Name, r.C.Names(), Drop)
		err := poll(30*time.Second, func() bool {
			st, ok := p.Status()
			if !ok {
				return false
			}
			for _, n := range st.Nodes {
				if n.Name == "node-"+s.Name {
					return n.Replication == "not connected" && !n.Reachable
				}
			}
			return false
		})
		if err != nil {
			r.C.Fail("%s still connected although all its links are cut: traffic bypasses the proxies", s.Name)
		}
		for _, l := range r.C.Net.Links() {
			if l.bytes.Load() == 0 {
				r.C.Fail("link %s->%s never carried a byte", l.From, l.To)
			}
		}
		r.Let(10 * time.Second)
	}},
	{Name: "slow-network", Topo: Three, Body: func(r *Run) {
		r.C.Net.Delay(150 * time.Millisecond)
		r.Let(20 * time.Second)
	}},
	{Name: "two-nodes-no-witness-no-failover", Topo: Two, Body: func(r *Run) {
		r.noFailover = true
		p := r.Primary()
		p.Kill9()
		r.Let(25 * time.Second)
		if r.C.PrimaryByCluster() != p.Name {
			r.C.Fail("a two-node cluster without a witness failed over by itself")
		}
		r.expectStatus(r.Standbys(p)[0], "no witness")
	}},
	{Name: "copy-lost-degrade-then-eligible", Topo: TwoWitness, Body: func(r *Run) {
		p := r.Primary()
		s := r.Standbys(p)[0]
		t0 := r.C.Log.Mark("copy lost")
		s.Kill9()
		r.Let(45 * time.Second)
		d := ResumeAfter(r.W.H, t0)
		if d < 25*time.Second || d > 45*time.Second {
			r.C.Fail("writes resumed %s after losing the only copy; want about degrade_after (30s)", d)
		}
		r.expectStatus(p, "Running on one copy")
		s.Restart()
	}},
	{Name: "strict-copy-lost-blocks", Topo: TwoWitness, Body: func(r *Run) {
		p := r.Primary()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if out, err := p.CLI(ctx, "settings", "set", "single_data_node_mode=strict"); err != nil {
			r.C.Fail("settings: %v\n%s", err, out)
		}
		s := r.Standbys(p)[0]
		t0 := r.C.Log.Mark("copy lost (strict)")
		s.Kill9()
		r.Let(40 * time.Second)
		for _, op := range r.W.H.snapshotAppends() {
			if op.Outcome == OK && op.Start.After(t0.Add(time.Second)) {
				r.C.Fail("strict mode: write %d acknowledged while the only copy was down", op.ID)
			}
		}
		s.Restart()
	}},
	{Name: "planned-switch-under-load", Topo: Three, Body: func(r *Run) {
		p := r.Primary()
		to := r.Standbys(p)[r.Rng.IntN(2)]
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		out, err := p.CLI(ctx, "switch", "--to", "node-"+to.Name)
		if err != nil {
			r.C.Fail("switch: %v\n%s", err, out)
		}
		r.WaitNewPrimary(p, time.Now(), time.Minute)
		r.Let(5 * time.Second)
	}},
	{Name: "kill-controller-during-failover", Topo: Three, Env: []string{faultEnv + "failover-after-cut"}, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.C.Log.Mark("primary down")
		start := r.C.Epoch()
		p.Kill9()
		r.killAtFault("failover-after-cut", r.failedOver(start))
		r.WaitNewPrimary(p, t0, 3*time.Minute)
	}},
	{Name: "kill-controller-before-cas", Topo: TwoWitness, Env: []string{faultEnv + "failover-before-cas"}, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.C.Log.Mark("primary down")
		start := r.C.Epoch()
		p.Kill9()
		r.killAtFault("failover-before-cas", r.failedOver(start))
		r.WaitNewPrimary(p, t0, 3*time.Minute)
	}},
	{Name: "kill-controller-after-cas", Topo: Three, Env: []string{faultEnv + "failover-after-cas"}, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.C.Log.Mark("primary down")
		start := r.C.Epoch()
		p.Kill9()
		r.killAtFault("failover-after-cas", r.failedOver(start))
		r.WaitNewPrimary(p, t0, 3*time.Minute)
	}},
	{Name: "kill-controller-during-sync-change", Topo: Three, Env: []string{faultEnv + "sync-after-s"}, Body: func(r *Run) {
		p := r.Primary()
		s := r.Standbys(p)[0]
		s.Kill9()
		if r.killAtFault("sync-after-s", r.outOfSyncSet(s)) == 0 {
			r.C.Fail("the controller never reached sync-after-s")
		}
		r.Let(10 * time.Second)
	}},
	{Name: "slow-start-after-fence", Topo: Three, Env: []string{faultEnv + "slow-start-after-clear-fence"}, Body: func(r *Run) {
		// The main node fences itself (agent frozen past 60% of the lease,
		// not long enough for a failover), clears the fence and starts
		// Postgres again; the start is slow (Postgres serves, Start
		// returns 25 s late). Cut off during that start, its lease must
		// stop that Postgres before anyone else takes over: never two
		// main nodes (rule 4 covers a Postgres from its first instant).
		p := r.Primary()
		p.Stop(false)
		r.Let(7500 * time.Millisecond) // past 60% of the 10s lease, short of lease + margin
		p.Cont()
		p.WaitOut("lost its main-node lease", 15*time.Second)
		p.WaitOut("Test fault point reached: slow-start-after-clear-fence", time.Minute)
		t0 := r.C.Log.Mark("isolate " + p.Name + " during its slow start")
		r.C.Net.Isolate(p.Name, r.C.Names(), Blackhole)
		r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(25 * time.Second) // past the late return of the start
	}},
	{Name: "kill-during-clone", Topo: Topology{Data: []string{"a", "b", "c"}, Late: []string{"d"}}, Body: func(r *Run) {
		r.bulk(60)
		d := r.C.Node("d")
		done := make(chan int, 1)
		go func() { done <- WaitProc(2*time.Minute, "pg_basebackup", d.Dir) }()
		r.C.LaunchJoin(d)
		if pid := <-done; pid == 0 {
			r.C.Fail("never saw the clone run")
		}
		d.Kill9()
		if p := WaitProc(5*time.Second, "pg_basebackup", d.Dir); p != 0 {
			r.C.Fail("pg_basebackup (pid %d) outlived its agent", p)
		}
		d.Restart()
	}},
	{Name: "kill-during-rewind", Topo: Three, Body: func(r *Run) {
		p := r.Primary()
		t0 := r.HardKill(p)
		r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(3 * time.Second)
		done := make(chan int, 1)
		go func() { done <- WaitProc(2*time.Minute, "pg_rewind", p.Dir) }()
		p.Launch("up", "")
		if pid := <-done; pid == 0 {
			r.C.Fail("never saw the rewind run")
		}
		p.Kill9()
		if q := WaitProc(5*time.Second, "pg_rewind", p.Dir); q != 0 {
			r.C.Fail("pg_rewind (pid %d) outlived its agent", q)
		}
		p.Restart()
	}},
	{Name: "rolling-restart", Topo: Three, Body: func(r *Run) {
		for _, n := range r.C.Members() {
			n.Interrupt()
			n.Restart()
			r.C.WaitHealthy(3 * time.Minute)
		}
	}},
	{Name: "laptop-sleep", Topo: Three, Body: func(r *Run) {
		// Agent and postmaster frozen well past the lease (like a sleeping
		// machine), then woken: it must not serve, and rejoins.
		p := r.Primary()
		t0 := r.C.Log.Mark("sleep " + p.Name)
		p.Stop(true)
		r.WaitNewPrimary(p, t0, time.Minute)
		r.Let(30 * time.Second)
		p.Cont()
		r.Let(15 * time.Second)
	}},
}

func witnessNames(r *Run) []string {
	var out []string
	for _, n := range r.C.Members() {
		if n.Witness {
			out = append(out, n.Name)
		}
	}
	return out
}

// OneCopyResume: when the only other data node is the one that died, writes
// must be acknowledged again about as fast as an ordinary failover (no
// degrade_after wait).
const OneCopyResume = 20 * time.Second

// expectResumeWithin: some write that started after t was acknowledged
// within d of t.
func (r *Run) expectResumeWithin(t time.Time, d time.Duration) {
	got := ResumeAfter(r.W.H, t)
	if got < 0 || got > d {
		r.C.Fail("writes resumed %s after the fault, want within %s", got.Round(time.Millisecond), d)
	}
	r.C.Log.Event("writes resumed %s after the fault", got.Round(time.Millisecond))
}

// expectStatus: n's `status` must mention s (plain-words wording).
func (r *Run) expectStatus(n *Node, s string) {
	err := poll(30*time.Second, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := n.CLI(ctx, "status")
		return err == nil && strings.Contains(out, s)
	})
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, _ := n.CLI(ctx, "status")
		r.C.Fail("status on %s does not say %q:\n%s", n.Name, s, out)
	}
}

// killAtFault: whenever an agent pauses at point, kill -9 it and start it
// again without the fault (at most three times). It stops once no running
// agent still has the fault armed, or done says the interrupted operation
// was finished by someone. Returns how many agents it killed.
func (r *Run) killAtFault(point string, done func() bool) int {
	armed := map[*Node]bool{}
	for _, n := range r.C.Members() {
		armed[n] = true
	}
	k := 0
	for range 3 {
		var hit *Node
		err := poll(90*time.Second, func() bool {
			for n := range armed {
				if n.Running() && strings.Contains(n.Output(), "Test fault point reached: "+point) {
					hit = n
					return true
				}
			}
			return !anyRunning(armed) || done()
		})
		if err != nil || hit == nil {
			break
		}
		r.C.Log.Event("%s paused at %s: killing it", hit.Name, point)
		hit.Kill9()
		delete(armed, hit)
		hit.Restart()
		k++
		if !anyRunning(armed) {
			break
		}
	}
	return k
}

func anyRunning(ns map[*Node]bool) bool {
	for n := range ns {
		if n.Running() {
			return true
		}
	}
	return false
}

// failedOver: the cluster names a main node at an epoch after start and
// that node runs Postgres read-write.
func (r *Run) failedOver(start uint64) func() bool {
	return func() bool {
		if r.C.Epoch() <= start {
			return false
		}
		name := r.C.PrimaryByCluster()
		if name == "" {
			return false
		}
		rep, ok := r.C.Node(name).Report()
		return ok && rep.PG != nil && rep.PG.Role == "primary"
	}
}

// outOfSyncSet: the cluster's sync set (R, as the newest state has it) no
// longer lists n.
func (r *Run) outOfSyncSet(n *Node) func() bool {
	return func() bool {
		var best *coyoteagent.Status
		for _, m := range r.C.Members() {
			if !m.Running() {
				continue
			}
			if s, ok := m.Status(); ok && (best == nil || s.Cluster.Epoch > best.Cluster.Epoch) {
				best = &s
			}
		}
		return best != nil && !slices.Contains(best.Cluster.SyncSet, "node-"+n.Name)
	}
}

// bulk writes about mb megabytes so a clone takes long enough to interrupt.
func (r *Run) bulk(mb int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, _, err := r.C.FindPrimary(ctx, "")
	if err != nil {
		r.C.Fail("bulk: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `CREATE TABLE chaos_filler AS SELECT g AS id, repeat(md5(g::text), 31) AS pad
		FROM generate_series(1, $1::int) g`, mb*1000); err != nil {
		r.C.Fail("bulk: %v", err)
	}
}

func (h *History) snapshotAppends() []AppendOp {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]AppendOp(nil), h.Appends...)
}

func TestScenarios(t *testing.T) {
	re := onlyRe()
	for _, sc := range scenarios {
		if re != nil && !re.MatchString(sc.Name) {
			continue
		}
		t.Run(sc.Name, func(t *testing.T) { runScenario(t, sc, seedFor(sc.Name)) })
	}
}
