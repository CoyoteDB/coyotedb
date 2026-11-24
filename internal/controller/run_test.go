package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// fakeCluster runs the real FSM without Raft.
type fakeCluster struct {
	mu     sync.Mutex
	f      *cluster.FSM
	idx    uint64
	leader bool
	log    *[]string
	// onApply, if set, sees every command before it is applied.
	onApply func(ctx context.Context, cmd cluster.Command)
}

func (c *fakeCluster) IsLeader() bool                     { return c.leader }
func (c *fakeCluster) State() cluster.State               { return c.f.State() }
func (c *fakeCluster) VerifyLeader(context.Context) error { return nil }
func (c *fakeCluster) LinearizableState(context.Context) (cluster.State, error) {
	return c.f.State(), nil
}
func (c *fakeCluster) Apply(ctx context.Context, cmd cluster.Command) (cluster.Result, error) {
	if c.onApply != nil {
		c.onApply(ctx, cmd)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.idx++
	r, err := c.f.ApplyEntry(c.idx, cmd)
	if c.log != nil && err == nil {
		*c.log = append(*c.log, fmt.Sprintf("raft %T", cmd))
	}
	return r, err
}

func (c *fakeCluster) must(t *testing.T, cmd cluster.Command) {
	t.Helper()
	if _, err := c.Apply(context.Background(), cmd); err != nil {
		t.Fatalf("%T: %v", cmd, err)
	}
}

type fakeAgents struct {
	mu      sync.Mutex
	nodes   map[string]*NodeState
	cutLSN  map[string]pg.LSN
	calls   []string
	onCut   func(id string)
	sFlush  pg.LSN
	cutFail map[string]bool
	sFail   int // the next sFail set_s calls fail (the main node briefly without its lease)
}

func (a *fakeAgents) record(s string) { a.calls = append(a.calls, s) }

func (a *fakeAgents) Status(_ context.Context, id string) (*NodeState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, ok := a.nodes[id]
	if !ok || n == nil {
		return nil, errors.New("unreachable")
	}
	c := *n
	return &c, nil
}

func (a *fakeAgents) Cut(_ context.Context, id string, epoch uint64, hold time.Duration) (pg.LSN, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.record("cut " + id)
	if a.cutFail[id] || a.nodes[id] == nil {
		return 0, errors.New("unreachable")
	}
	if a.onCut != nil {
		a.onCut(id)
	}
	return a.cutLSN[id], nil
}

func (a *fakeAgents) Resume(_ context.Context, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.record("resume " + id)
	return nil
}

func (a *fakeAgents) SetS(_ context.Context, primary string, req SetSRequest) (pg.LSN, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.record(fmt.Sprintf("set_s %s %v k=%d barrier=%v", primary, req.Names, req.K, req.Barrier))
	if a.sFail > 0 {
		a.sFail--
		return 0, errors.New("this node is not the main node right now: it does not hold its main-node lease for term 1 at this moment")
	}
	if p := a.nodes[primary]; p != nil {
		p.SyncNames, p.SyncCount = slices.Clone(req.Names), req.K
	}
	return a.sFlush, nil
}

type rig struct {
	t     *testing.T
	cl    *fakeCluster
	ag    *fakeAgents
	clock *FakeClock
	c     *Controller
	log   []string
	fault string
}

// rigSettings: the defaults with a 6s lease, which the step timings in
// these tests are written for (the controller reads the lease from the
// state, so only the relative timing matters).
func rigSettings() cluster.Settings {
	set := cluster.DefaultSettings()
	set.LeaseDuration, set.LeaseRenewInterval = 6*time.Second, time.Second
	return set
}

// newRig: n1 primary; n2..nN streaming standbys in R; optional witness.
func newRig(t *testing.T, data int, witness bool) *rig {
	r := &rig{t: t, clock: NewFakeClock()}
	r.cl = &fakeCluster{f: cluster.NewFSM(nil), leader: true, log: &r.log}
	plat := cluster.Platform{OS: "darwin", Arch: "arm64", PGBuildSHA: "x"}
	mem := func(id string, role cluster.Role) cluster.Member {
		return cluster.Member{ID: id, Name: "node-" + id, Addresses: []string{"127.0.0.1:1"}, Role: role, Platform: plat}
	}
	set := rigSettings()
	r.cl.must(t, &cluster.BootstrapState{ClusterID: "c1", CA: cluster.CAData{CertDER: []byte{1}, KeyDER: []byte{2}},
		Member: mem("n1", cluster.RoleData), Settings: set, At: time.Now()})
	r.ag = &fakeAgents{nodes: map[string]*NodeState{}, cutLSN: map[string]pg.LSN{}, cutFail: map[string]bool{}, sFlush: 500}
	var r2 []string
	var rows []pg.StandbyRow
	for i := 2; i <= data; i++ {
		id := fmt.Sprintf("n%d", i)
		r.cl.must(t, &cluster.AddMember{Member: mem(id, cluster.RoleData), At: time.Now()})
		r.cl.must(t, &cluster.SetSuffrage{ID: id, Suffrage: cluster.Voter})
		r2 = append(r2, id)
		f := pg.LSN(1000)
		rows = append(rows, pg.StandbyRow{Name: "node-" + id, State: "streaming", SyncState: "quorum", FlushLSN: &f})
		r.ag.nodes[id] = &NodeState{PGRunning: true, Role: "standby", Streaming: true, Epoch: 1}
		r.ag.cutLSN[id] = pg.LSN(100 * i)
	}
	if witness {
		r.cl.must(t, &cluster.AddMember{Member: mem("w", cluster.RoleWitness), At: time.Now()})
	}
	if len(r2) > 0 {
		r.cl.must(t, &cluster.SetSyncSet{ExpectedEpoch: 1, Members: r2, K: 1})
		r.cl.must(t, &cluster.SetStandbyEligible{ExpectedEpoch: 1, Eligible: true, At: time.Now()})
	}
	r.cl.must(t, &cluster.RenewLease{NodeID: "n1", Epoch: 1, Served: true})
	pf := pg.LSN(1000)
	r.ag.nodes["n1"] = &NodeState{PGRunning: true, Role: "primary", Epoch: 1, FlushLSN: &pf, Standbys: rows,
		SyncNames: namesOf(r.cl.State(), r2), SyncCount: 1}
	r.log = nil
	r.c = New(Config{Cluster: r.cl, Agents: r.ag, Clock: r.clock, Timing: cluster.DefaultTiming(),
		Fault: func(_ context.Context, p string) error {
			if p == r.fault {
				return errors.New("controller crashed at " + p)
			}
			return nil
		}})
	return r
}

// run ticks for d of fake time in 500ms steps, renewing the primary's
// lease each tick while renew is true.
func (r *rig) run(d time.Duration, renew bool) {
	for el := time.Duration(0); el < d; el += 500 * time.Millisecond {
		if renew {
			st := r.cl.State()
			r.cl.must(r.t, &cluster.RenewLease{NodeID: st.PrimaryID, Epoch: st.Epoch, Served: true})
		}
		r.c.Tick(context.Background())
		r.clock.Advance(500 * time.Millisecond)
	}
}

func (r *rig) kill(id string) {
	r.ag.mu.Lock()
	r.ag.nodes[id] = nil
	if p := r.ag.nodes["n1"]; p != nil && id != "n1" {
		p.Standbys = slices.DeleteFunc(p.Standbys, func(s pg.StandbyRow) bool { return s.Name == "node-"+id })
	}
	r.ag.mu.Unlock()
}

func TestFailoverWaitsOutLeaseThenPromotesMostWAL(t *testing.T) {
	r := newRig(t, 3, false)
	r.run(3*time.Second, true)
	r.kill("n1")
	r.run(6500*time.Millisecond, false)
	if len(r.ag.calls) != 0 || r.cl.State().Epoch != 1 {
		t.Fatalf("acted before lease+margin passed: %v", r.ag.calls)
	}
	r.run(1500*time.Millisecond, false)
	st := r.cl.State()
	if st.Epoch != 2 || st.PrimaryID != "n3" {
		t.Fatalf("epoch %d primary %s; calls %v", st.Epoch, st.PrimaryID, r.ag.calls)
	}
	slices.Sort(r.ag.calls)
	if !slices.Equal(r.ag.calls, []string{"cut n2", "cut n3"}) {
		t.Fatalf("calls %v", r.ag.calls)
	}
	if !slices.Contains(r.log, "raft *cluster.Promote") {
		t.Fatalf("raft log %v", r.log)
	}
}

func TestNewLeaderWaitsFullLease(t *testing.T) {
	r := newRig(t, 3, false)
	r.kill("n1")
	r.cl.leader = false
	r.run(30*time.Second, false) // the primary died long ago, but we were not leader
	r.cl.leader = true
	r.run(6500*time.Millisecond, false)
	if r.cl.State().Epoch != 1 {
		t.Fatal("a new leader must wait a full lease + margin after its election")
	}
	r.run(2*time.Second, false)
	if r.cl.State().Epoch != 2 {
		t.Fatal("no failover after the wait")
	}
}

func TestFailoverRenewedDuringCutAborts(t *testing.T) {
	r := newRig(t, 3, false)
	r.kill("n1")
	// The primary was only paused: it renews while the cuts run.
	r.ag.onCut = func(id string) {
		if id == "n3" {
			r.cl.must(t, &cluster.RenewLease{NodeID: "n1", Epoch: 1, Served: true})
		}
	}
	r.run(8*time.Second, false)
	st := r.cl.State()
	if st.Epoch != 1 || st.PrimaryID != "n1" {
		t.Fatalf("promoted although the lease was renewed: epoch %d", st.Epoch)
	}
	for _, id := range []string{"n2", "n3"} {
		if !slices.Contains(r.ag.calls, "resume "+id) {
			t.Fatalf("cut %s not resumed: %v", id, r.ag.calls)
		}
	}
}

func TestFailoverBlockedWhenTooFewCut(t *testing.T) {
	r := newRig(t, 3, false)
	r.kill("n1")
	r.ag.cutFail["n2"] = true
	r.run(9*time.Second, false)
	if r.cl.State().Epoch != 1 {
		t.Fatal("promoted with too few cut")
	}
	if !slices.Contains(r.ag.calls, "resume n3") {
		t.Fatalf("calls %v", r.ag.calls)
	}
	rep := r.c.Report()
	if !strings.Contains(rep.View.FailoverBlocked, "safety rule 2") {
		t.Fatalf("blocked %q", rep.View.FailoverBlocked)
	}
}

func TestControllerCrashAtEachFailoverStep(t *testing.T) {
	for _, point := range []string{"failover-after-cut", "failover-before-cas"} {
		t.Run(point, func(t *testing.T) {
			r := newRig(t, 3, false)
			r.kill("n1")
			r.fault = point
			r.run(9*time.Second, false)
			if r.cl.State().Epoch != 1 {
				t.Fatal("promoted although the controller died before the compare-and-set")
			}
			// The cuts stay (they resume by themselves after CutHold); a
			// new controller re-cuts and finishes.
			if slices.Contains(r.ag.calls, "resume n2") {
				t.Fatal("a crashed controller cannot resume")
			}
			r.fault = ""
			r.c = New(Config{Cluster: r.cl, Agents: r.ag, Clock: r.clock, Timing: cluster.DefaultTiming()})
			r.run(8*time.Second, false)
			if st := r.cl.State(); st.Epoch != 2 || st.PrimaryID != "n3" {
				t.Fatalf("new controller did not finish: epoch %d", st.Epoch)
			}
		})
	}
	t.Run("failover-after-cas", func(t *testing.T) {
		r := newRig(t, 3, false)
		r.kill("n1")
		r.fault = "failover-after-cas"
		r.run(9*time.Second, false)
		if st := r.cl.State(); st.Epoch != 2 {
			t.Fatal("the compare-and-set is the commit point")
		}
	})
}

func TestTwoNodesWithoutWitnessNeverFailOver(t *testing.T) {
	r := newRig(t, 2, false)
	r.kill("n1")
	r.run(60*time.Second, false)
	if r.cl.State().Epoch != 1 || len(r.ag.calls) != 0 {
		t.Fatalf("acted: %v", r.ag.calls)
	}
	if !strings.Contains(r.c.Report().View.FailoverBlocked, "no witness") {
		t.Fatalf("view %+v", r.c.Report().View)
	}
}

// 2 data + witness, the primary dies: its node is the only other data
// member, so the new primary has no copy it could wait for. Default mode:
// promote and record "running on one copy" in the same entry (rule 9 at
// once, no degrade_after wait). Strict: the old primary joins R, writes
// wait for it.
func TestTwoNodesWithWitnessFailover(t *testing.T) {
	r := newRig(t, 2, true)
	r.kill("n1")
	r.run(9*time.Second, false)
	st := r.cl.State()
	if st.PrimaryID != "n2" || len(st.Sync.Members) != 0 || st.StandbyEligible || st.StandbyEligibleSince.IsZero() {
		t.Fatalf("primary %s R %v eligible %v", st.PrimaryID, st.Sync.Members, st.StandbyEligible)
	}
	if n := strings.Count(strings.Join(r.log, ","), "raft *cluster.Promote"); n != 1 {
		t.Fatalf("raft log %v", r.log)
	}

	s := newRig(t, 2, true)
	set := rigSettings()
	set.SingleDataNodeMode = cluster.ModeStrict
	s.cl.must(t, &cluster.SetSettings{Settings: set})
	s.kill("n1")
	s.run(9*time.Second, false)
	st = s.cl.State()
	if st.PrimaryID != "n2" || !slices.Equal(st.Sync.Members, []string{"n1", "n2"}) || !st.StandbyEligible {
		t.Fatalf("strict: primary %s R %v eligible %v", st.PrimaryID, st.Sync.Members, st.StandbyEligible)
	}
}

// 3 data nodes, one copy already down: when the primary dies too, the
// survivor is promoted on one copy at once.
func TestThreeNodesSecondLossPromotesOnOneCopy(t *testing.T) {
	r := newRig(t, 3, false)
	r.kill("n2")
	r.cl.must(t, &cluster.SetSyncSet{ExpectedEpoch: 1, Members: []string{"n3"}, K: 1})
	r.kill("n1")
	r.run(9*time.Second, false)
	st := r.cl.State()
	if st.PrimaryID != "n3" || st.StandbyEligible || len(st.Sync.Members) != 0 {
		t.Fatalf("primary %s R %v eligible %v (calls %v)", st.PrimaryID, st.Sync.Members, st.StandbyEligible, r.ag.calls)
	}
	// With a live copy (n2 up) the ordinary path is kept.
	q := newRig(t, 3, false)
	q.kill("n1")
	q.run(9*time.Second, false)
	if st := q.cl.State(); !st.StandbyEligible || len(st.Sync.Members) == 0 {
		t.Fatalf("degraded although a copy was alive: R %v", st.Sync.Members)
	}
}

func TestDegradeOrderAndStrict(t *testing.T) {
	r := newRig(t, 2, true)
	r.kill("n2")
	r.run(29*time.Second, true)
	if !r.cl.State().StandbyEligible || len(r.ag.calls) != 0 {
		t.Fatalf("degraded early: %v", r.ag.calls)
	}
	r.run(2*time.Second, true)
	if r.cl.State().StandbyEligible {
		t.Fatal("did not degrade")
	}
	// eligible=false is in Raft before S is cleared.
	iE := slices.Index(r.log, "raft *cluster.SetStandbyEligible")
	if iE < 0 || len(r.ag.calls) == 0 || r.ag.calls[0] != "set_s n1 [] k=0 barrier=true" {
		t.Fatalf("order: raft %v agents %v", r.log, r.ag.calls)
	}

	s := newRig(t, 2, true)
	set := rigSettings()
	set.SingleDataNodeMode = cluster.ModeStrict
	s.cl.must(t, &cluster.SetSettings{Settings: set})
	s.kill("n2")
	s.run(5*time.Minute, true)
	if !s.cl.State().StandbyEligible || len(s.ag.calls) != 0 {
		t.Fatalf("strict degraded: %v", s.ag.calls)
	}
	if v := s.c.Report().View; !v.WritesWaiting {
		t.Fatalf("view %+v", v)
	}
}

// F2: a degrade whose second half (clearing S on the main node) fails
// once, or whose controller dies between the halves, is finished on a
// later round: writes never stay blocked behind a recorded degrade.
func TestDegradeRetriedUntilDone(t *testing.T) {
	for _, how := range []string{"set_s fails once", "controller dies between the halves"} {
		r := newRig(t, 2, true)
		if how == "set_s fails once" {
			r.ag.sFail = 1
		} else {
			r.fault = "degrade-after-eligible"
		}
		r.kill("n2")
		r.run(31*time.Second, true)
		if r.cl.State().StandbyEligible {
			t.Fatalf("%s: did not record the degrade", how)
		}
		if got := r.ag.nodes["n1"].SyncNames; len(got) != 0 && how == "set_s fails once" {
			t.Fatalf("%s: S still %v after the retry; calls %v", how, got, r.ag.calls)
		}
		r.fault = ""
		r.run(2*time.Second, true)
		if got := r.ag.nodes["n1"].SyncNames; len(got) != 0 {
			t.Fatalf("%s: S still %v; calls %v", how, got, r.ag.calls)
		}
		if v := r.c.Report().View; v.WritesWaiting {
			t.Fatalf("%s: writes still waiting: %+v", how, v)
		}
		n := 0
		for _, c := range r.ag.calls {
			if c == "set_s n1 [] k=0 barrier=true" {
				n++
			}
		}
		if n < 2 && how == "set_s fails once" {
			t.Fatalf("%s: the failed set_s was not retried: %v", how, r.ag.calls)
		}
	}
}

func TestMakeEligibleOrder(t *testing.T) {
	r := newRig(t, 2, true)
	r.cl.must(t, &cluster.SetStandbyEligible{ExpectedEpoch: 1, Eligible: false, At: time.Now()})
	r.cl.must(t, &cluster.SetSyncSet{ExpectedEpoch: 1, Members: nil, K: 0})
	r.ag.nodes["n1"].SyncNames, r.ag.nodes["n1"].SyncCount = nil, 0
	r.log = nil
	r.ag.sFlush = 900 // n2 has flushed 1000: caught up
	r.run(time.Second, true)
	st := r.cl.State()
	if !st.StandbyEligible || !slices.Equal(st.Sync.Members, []string{"n2"}) {
		t.Fatalf("eligible %v R %v; %v %v", st.StandbyEligible, st.Sync.Members, r.log, r.ag.calls)
	}
	iR := slices.Index(r.log, "raft *cluster.SetSyncSet")
	iE := slices.Index(r.log, "raft *cluster.SetStandbyEligible")
	if iR < 0 || iE < iR || r.ag.calls[0] != "set_s n1 [node-n2] k=1 barrier=true" {
		t.Fatalf("order: %v %v", r.log, r.ag.calls)
	}

	// Not caught up: S includes it, but eligibility waits.
	q := newRig(t, 2, true)
	q.cl.must(t, &cluster.SetStandbyEligible{ExpectedEpoch: 1, Eligible: false, At: time.Now()})
	q.ag.nodes["n1"].SyncNames, q.ag.nodes["n1"].SyncCount = nil, 0
	q.ag.sFlush = 2000
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	q.c.Tick(ctx)
	if q.cl.State().StandbyEligible {
		t.Fatal("eligible before catching up")
	}
}

func TestSyncRemoveOrder(t *testing.T) {
	r := newRig(t, 3, false)
	r.kill("n3")
	r.ag.sFlush = 900
	r.run(8*time.Second, true)
	st := r.cl.State()
	if !slices.Equal(st.Sync.Members, []string{"n2"}) {
		t.Fatalf("R %v; %v", st.Sync.Members, r.ag.calls)
	}
	if r.ag.calls[0] != "set_s n1 [node-n2] k=1 barrier=true" {
		t.Fatalf("S first: %v", r.ag.calls)
	}
}

func TestOrphanCutResumed(t *testing.T) {
	r := newRig(t, 3, false)
	r.ag.nodes["n2"].StreamCut, r.ag.nodes["n2"].CutHold, r.ag.nodes["n2"].CutEpoch = true, true, 1
	r.run(time.Second, true)
	if !slices.Contains(r.ag.calls, "resume n2") {
		t.Fatalf("calls %v", r.ag.calls)
	}
}

func TestNeedsRecloneAfterGone(t *testing.T) {
	r := newRig(t, 3, false)
	set := rigSettings()
	set.GoneAfter = time.Minute
	r.cl.must(t, &cluster.SetSettings{Settings: set})
	r.kill("n3")
	r.ag.sFlush = 900
	r.run(70*time.Second, true)
	st := r.cl.State()
	if st.Members["n3"].Status != cluster.StatusNeedsReclone || slices.Contains(st.Sync.Members, "n3") {
		t.Fatalf("n3 %+v R %v", st.Members["n3"], st.Sync.Members)
	}
	if st.Members["n2"].Status != cluster.StatusActive {
		t.Fatal("n2 touched")
	}
}

// A sync list naming a member outside R (as a stale copy of the state
// could) is refused before it reaches the primary (rule 3), and so is one
// planned at an older term.
func TestSetSStaysWithinR(t *testing.T) {
	r := newRig(t, 3, false)
	st := r.cl.State()
	if _, err := r.c.setS(context.Background(), st, []string{"node-n2", "node-n9"}, 1, false); err == nil || !strings.Contains(err.Error(), "rule 3") {
		t.Fatalf("set_s outside R: %v", err)
	}
	old := st
	old.Epoch--
	if _, err := r.c.setS(context.Background(), old, []string{"node-n2"}, 1, false); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("set_s from an older term: %v", err)
	}
	for _, c := range r.ag.calls {
		if strings.HasPrefix(c, "set_s") {
			t.Fatalf("a refused sync list reached the primary: %v", r.ag.calls)
		}
	}
	if _, err := r.c.setS(context.Background(), st, []string{"node-n2"}, 1, false); err != nil {
		t.Fatal(err)
	}
}

// F5 (lease review): the compare-and-set must land within half the cut
// hold of the first cut. Checking the elapsed time before the write is
// not enough: the write itself may take its whole timeout. Its context
// must end where the half hold ends.
func TestFailoverCASBoundedByHalfHold(t *testing.T) {
	r := newRig(t, 3, false)
	r.kill("n1")
	r.ag.onCut = func(id string) {
		if id == "n3" {
			r.clock.Advance(CutHold/2 - time.Second) // the cuts were slow
		}
	}
	var left time.Duration = -1
	r.cl.onApply = func(ctx context.Context, cmd cluster.Command) {
		if _, ok := cmd.(*cluster.Promote); ok {
			if dl, ok := ctx.Deadline(); ok {
				left = time.Until(dl)
			} else {
				left = time.Hour
			}
		}
	}
	r.run(8*time.Second, false)
	if left < 0 {
		t.Fatal("no promote was attempted")
	}
	if left > 1100*time.Millisecond {
		t.Fatalf("the promote's write could take %s more, past half the cut hold", left.Round(time.Millisecond))
	}
}
