package controller

import (
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// world is a test cluster: n1 primary, data members n2..nN, optional witness.
type world struct {
	in Input
}

func newWorld(data int, witness bool) *world {
	st := cluster.State{
		Epoch:           3,
		Members:         map[string]cluster.Member{},
		PrimaryID:       "n1",
		Lease:           cluster.Lease{Holder: "n1", Epoch: 3, Seq: 40, Duration: 6 * time.Second},
		Settings:        rigSettings(),
		StandbyEligible: true,
		LastServed:      "n1",
	}
	add := func(id string, role cluster.Role) {
		st.Members[id] = cluster.Member{ID: id, Name: "node-" + id, Role: role, Suffrage: cluster.Voter, Status: cluster.StatusActive}
	}
	in := Input{State: st, LeaderFor: time.Hour, Margin: time.Second, Members: map[string]MemberObs{}}
	add("n1", cluster.RoleData)
	var r []string
	for i := 2; i <= data; i++ {
		id := "n" + string(rune('0'+i))
		add(id, cluster.RoleData)
		r = append(r, id)
		lsn := pg.LSN(1000)
		in.Members[id] = MemberObs{ID: id, Name: "node-" + id, Voter: true, Active: true, InR: true, Streaming: true, Listed: true,
			LagBytes: 0, FlushLSN: &lsn, Report: &NodeState{PGRunning: true, Role: "standby", Streaming: true, Epoch: 3}}
	}
	if witness {
		add("w", cluster.RoleWitness)
	}
	st.Sync = cluster.SyncSet{Members: append([]string{"n1"}, r...), K: 1, Epoch: 3}
	in.State = st
	var names []string
	for _, id := range r {
		names = append(names, "node-"+id)
	}
	in.Primary = &NodeState{PGRunning: true, Role: "primary", Epoch: 3, SyncNames: names, SyncCount: 1}
	return &world{in: in}
}

func (w *world) member(id string, f func(*MemberObs)) *world {
	m := w.in.Members[id]
	f(&m)
	w.in.Members[id] = m
	return w
}

func (w *world) primaryGone() *world { w.in.LeaseAge = 8 * time.Second; return w }

func down(m *MemberObs) { m.Streaming, m.Report = false, nil }

func TestDecideTable(t *testing.T) {
	cases := []struct {
		name  string
		w     *world
		kind  ActKind
		mem   string
		check func(t *testing.T, p Plan)
	}{
		{name: "healthy three nodes: nothing", w: newWorld(3, false), kind: ActNone,
			check: func(t *testing.T, p Plan) {
				if !p.View.LeaseHealthy || p.View.WritesWaiting || p.View.FailoverBlocked != "" {
					t.Fatalf("view %+v", p.View)
				}
			}},
		{name: "lease ran out: fail over", w: newWorld(3, false).primaryGone(), kind: ActFailover,
			check: func(t *testing.T, p Plan) {
				if p.ExpectedLeaseSeq != 40 {
					t.Fatalf("seq %d", p.ExpectedLeaseSeq)
				}
			}},
		{name: "lease almost out: wait", w: func() *world { w := newWorld(3, false); w.in.LeaseAge = 6900 * time.Millisecond; return w }(), kind: ActNone},
		{name: "new controller waits a full lease after election", w: func() *world {
			w := newWorld(3, false).primaryGone()
			w.in.LeaderFor = 5 * time.Second
			return w
		}(), kind: ActNone},
		{name: "two data nodes and a witness fail over", w: newWorld(2, true).primaryGone(), kind: ActFailover},
		{name: "two data nodes without a witness never fail over", w: newWorld(2, false).primaryGone(), kind: ActNone,
			check: func(t *testing.T, p Plan) {
				if !strings.Contains(p.View.FailoverBlocked, "no witness") {
					t.Fatalf("blocked %q", p.View.FailoverBlocked)
				}
			}},
		{name: "one copy: no failover", w: func() *world {
			w := newWorld(3, false).primaryGone()
			w.in.State.StandbyEligible = false
			return w
		}(), kind: ActNone, check: func(t *testing.T, p Plan) {
			if !strings.Contains(p.View.FailoverBlocked, "may be missing writes") {
				t.Fatalf("blocked %q", p.View.FailoverBlocked)
			}
		}},
		{name: "rule 2: too few copies reachable (k=1 of 2 needs both)", w: newWorld(3, false).primaryGone().member("n3", down), kind: ActNone,
			check: func(t *testing.T, p Plan) {
				if !strings.Contains(p.View.FailoverBlocked, "Only 1 of the copies") {
					t.Fatalf("blocked %q", p.View.FailoverBlocked)
				}
			}},
		{name: "rule 2: five nodes, k=2, one down still blocks (D42: Raft's K is not Postgres' count)", w: func() *world {
			w := newWorld(5, false).primaryGone().member("n5", down)
			w.in.State.Sync.K = 2
			return w
		}(), kind: ActNone},
		{name: "rule 2: five nodes, k=2, all reachable", w: func() *world {
			w := newWorld(5, false).primaryGone()
			w.in.State.Sync.K = 2
			return w
		}(), kind: ActFailover},
		{name: "rule 2: five nodes, k=2, two down blocks", w: func() *world {
			w := newWorld(5, false).primaryGone().member("n5", down).member("n4", down)
			w.in.State.Sync.K = 2
			return w
		}(), kind: ActNone},
		{name: "unsafe timing: no failover", w: func() *world {
			w := newWorld(3, false).primaryGone()
			w.in.TimingProblem = "lease too short"
			return w
		}(), kind: ActNone},
		{name: "no primary at all", w: func() *world { w := newWorld(3, false); w.in.State.PrimaryID = ""; return w }(), kind: ActNone},
		{name: "orphan switch cut is resumed", w: newWorld(3, false).member("n3", func(m *MemberObs) {
			m.Streaming = false
			m.Report.StreamCut, m.Report.CutHold, m.Report.CutEpoch = true, true, 3
		}), kind: ActResumeCut, mem: "n3"},
		{name: "a manual cut (no hold) is left alone", w: newWorld(3, false).member("n3", func(m *MemberObs) {
			m.Streaming = false
			m.Report.StreamCut = true
		}), kind: ActNone},
		{name: "S names a node outside R", w: func() *world {
			w := newWorld(3, false)
			w.in.Primary.SyncNames = append(w.in.Primary.SyncNames, "stranger")
			return w
		}(), kind: ActApplyS},
		{name: "eligible but S empty: record one copy", w: func() *world { w := newWorld(3, false); w.in.Primary.SyncNames = nil; return w }(), kind: ActRecordIneligible},
		{name: "S missing a member of R", w: func() *world { w := newWorld(3, false); w.in.Primary.SyncNames = []string{"node-n2"}; return w }(), kind: ActApplyS},
		{name: "streaming standby outside R is added", w: newWorld(3, false).member("n3", func(m *MemberObs) { m.InR = false }).withR("n1", "n2"), kind: ActSyncAdd, mem: "n3"},
		{name: "standby being removed is not added (D59)", w: newWorld(3, false).member("n3", func(m *MemberObs) { m.InR, m.Removing = false, true }).withR("n1", "n2"), kind: ActNone,
			check: func(t *testing.T, p Plan) {
				if !strings.Contains(strings.Join(p.View.Notes, " "), "node-n3 is being removed") {
					t.Fatalf("notes %q", p.View.Notes)
				}
			}},
		{name: "lagging standby is not added yet", w: newWorld(3, false).member("n3", func(m *MemberObs) { m.InR, m.LagBytes = false, MaxAddLag+1 }).withR("n1", "n2"), kind: ActNone},
		{name: "lost member of R is removed while another streams", w: newWorld(3, false).member("n3", func(m *MemberObs) {
			down(m)
			m.NotStreamingFor = 7 * time.Second
		}), kind: ActSyncRemove, mem: "n3"},
		{name: "lost member of R kept while it is the last copy", w: newWorld(2, true).member("n2", func(m *MemberObs) {
			down(m)
			m.NotStreamingFor = 7 * time.Second
		}).noSyncFor(10 * time.Second), kind: ActNone, check: func(t *testing.T, p Plan) {
			if !p.View.WritesWaiting || !strings.Contains(strings.Join(p.View.Notes, " "), "Writes are waiting") {
				t.Fatalf("view %+v", p.View)
			}
		}},
		{name: "degrade after degrade_after", w: newWorld(2, true).member("n2", down).noSyncFor(30 * time.Second), kind: ActDegrade},
		{name: "strict never degrades", w: func() *world {
			w := newWorld(2, true).member("n2", down).noSyncFor(time.Hour)
			w.in.State.Settings.SingleDataNodeMode = cluster.ModeStrict
			return w
		}(), kind: ActNone, check: func(t *testing.T, p Plan) {
			if !strings.Contains(strings.Join(p.View.Notes, " "), "strict") {
				t.Fatalf("notes %v", p.View.Notes)
			}
		}},
		{name: "one copy: caught-up standby made eligible", w: func() *world {
			w := newWorld(2, true)
			w.in.State.StandbyEligible = false
			w.in.Primary.SyncNames = nil
			return w
		}(), kind: ActMakeEligible, mem: "n2"},
		{name: "one copy: standby being removed is not made eligible (D59)", w: func() *world {
			w := newWorld(2, true).member("n2", func(m *MemberObs) { m.Removing = true })
			w.in.State.StandbyEligible = false
			w.in.Primary.SyncNames = nil
			return w
		}(), kind: ActNone},
		{name: "one copy: lagging standby not yet eligible", w: func() *world {
			w := newWorld(2, true).member("n2", func(m *MemberObs) { m.LagBytes = -1 })
			w.in.State.StandbyEligible = false
			w.in.Primary.SyncNames = nil
			return w
		}(), kind: ActNone, check: func(t *testing.T, p Plan) {
			if !p.View.OneCopy || !strings.Contains(strings.Join(p.View.Notes, " "), "Running on one copy") {
				t.Fatalf("view %+v", p.View)
			}
		}},
		{name: "rule 8: gone too long, outside R: needs re-clone", w: newWorld(3, false).member("n3", func(m *MemberObs) {
			down(m)
			m.InR = false
			m.NotStreamingFor = 31 * time.Minute
		}).withR("n1", "n2"), kind: ActMarkReclone, mem: "n3"},
		{name: "rule 8: gone too long in R: out of R first", w: newWorld(3, false).member("n3", func(m *MemberObs) {
			down(m)
			m.NotStreamingFor = 31 * time.Minute
		}), kind: ActSyncRemove, mem: "n3"},
		{name: "rule 8: slot lost", w: newWorld(3, false).member("n3", func(m *MemberObs) { m.SlotLost = true }), kind: ActSyncRemove, mem: "n3"},
		{name: "rule 8: one copy, prune from R", w: func() *world {
			w := newWorld(2, true).member("n2", func(m *MemberObs) { down(m); m.NotStreamingFor = time.Hour })
			w.in.State.StandbyEligible = false
			w.in.Primary.SyncNames = nil
			return w
		}(), kind: ActPrune, mem: "n2"},
		{name: "primary not reporting: wait", w: func() *world { w := newWorld(3, false); w.in.Primary = nil; return w }(), kind: ActNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Decide(c.w.in)
			if p.Kind != c.kind || (c.mem != "" && p.Member != c.mem) {
				t.Fatalf("got %s %q (%s), want %s %q; view %+v", p.Kind, p.Member, p.Why, c.kind, c.mem, p.View)
			}
			if c.check != nil {
				c.check(t, p)
			}
		})
	}
}

func (w *world) withR(ids ...string) *world {
	w.in.State.Sync.Members = ids
	var names []string
	for _, id := range ids {
		if id != w.in.State.PrimaryID {
			names = append(names, "node-"+id)
		}
	}
	w.in.Primary.SyncNames = names
	return w
}

func (w *world) noSyncFor(d time.Duration) *world { w.in.NoSyncFor = d; return w }

func TestChooseTarget(t *testing.T) {
	st := newWorld(4, false).in.State
	cut := func(lsn pg.LSN) CutResult { return CutResult{Cut: true, LSN: lsn} }
	cases := []struct {
		name string
		k    int
		res  map[string]CutResult
		last string
		want string
	}{
		{name: "highest LSN wins", k: 1, res: map[string]CutResult{"n2": cut(10), "n3": cut(30), "n4": cut(20)}, want: "n3"},
		{name: "need all three cut with k=1", k: 1, res: map[string]CutResult{"n2": cut(10), "n3": cut(30)}, want: ""},
		{name: "k=2 still needs all three (Raft's K is not what Postgres waited for)", k: 2, res: map[string]CutResult{"n2": cut(10), "n4": cut(20)}, want: ""},
		{name: "k=2 with all three cut", k: 2, res: map[string]CutResult{"n2": cut(10), "n3": cut(5), "n4": cut(20)}, want: "n4"},
		{name: "tie: lowest id among voters", k: 1, res: map[string]CutResult{"n2": cut(30), "n3": cut(30), "n4": cut(30)}, want: "n2"},
		{name: "failed cut does not count", k: 2, res: map[string]CutResult{"n2": {Err: "x"}, "n3": cut(5)}, want: ""},
		{name: "named primary that never served is a candidate", k: 1, last: "n9",
			res: map[string]CutResult{"n1": cut(50), "n2": cut(10), "n3": cut(30), "n4": cut(20)}, want: "n1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := st
			s.Sync.K = c.k
			if c.last != "" {
				s.LastServed = c.last
			}
			got, why := ChooseTarget(s, c.res, nil)
			if got != c.want {
				t.Fatalf("got %q (%s), want %q", got, why, c.want)
			}
			if got == "" && why == "" {
				t.Fatal("a refusal must say why")
			}
		})
	}
	// Tie between a voter and a non-voter goes to the voter.
	s := st
	m := s.Members["n2"]
	m.Suffrage = cluster.Nonvoter
	s.Members = map[string]cluster.Member{}
	for id, v := range st.Members {
		s.Members[id] = v
	}
	s.Members["n2"] = m
	if got, _ := ChooseTarget(s, map[string]CutResult{"n2": cut(30), "n3": cut(30), "n4": cut(30)}, nil); got != "n3" {
		t.Fatalf("voter tie-break: %q", got)
	}
	// Needs-re-clone members are never candidates.
	m = s.Members["n3"]
	m.Status = cluster.StatusNeedsReclone
	s.Members["n3"] = m
	for _, id := range Candidates(s) {
		if id == "n3" || id == "n1" {
			t.Fatalf("candidates %v", Candidates(s))
		}
	}
}

// F1 (lease review): Raft's sync count K is not the count Postgres was
// waiting for. The primary is given ANY min(K, |S|) of S, and S can be
// smaller than R (a member is added to R before S, rule 3) or not set yet
// (a new primary before Served). A write acknowledged under ANY 1 is on
// one copy only, so rule 2 must cut every candidate, whatever K says.
func TestRule2CountIgnoresRaftK(t *testing.T) {
	// R = {n1 (primary), n2, n3}, K = 2; the primary's S was still ANY 1
	// (n2) when n3 entered R. n2 is down, n1 is gone.
	w := newWorld(3, false).member("n2", down).primaryGone()
	w.in.State.Sync.K = 2
	w.in.Primary = nil
	p := Decide(w.in)
	if p.Kind == ActFailover {
		t.Fatalf("failover planned with n2 (possibly the only holder of acknowledged writes) unreachable: %+v", p)
	}
	if p.View.FailoverBlocked == "" {
		t.Fatal("a blocked failover must say why")
	}
	st := w.in.State
	if got, why := ChooseTarget(st, map[string]CutResult{"n3": {Cut: true, LSN: 900}}, nil); got != "" {
		t.Fatalf("promoted %s with only n3 cut while K=2 (why %q)", got, why)
	}
	if n := CutsNeeded(st, Candidates(st)); n != 2 {
		t.Fatalf("CutsNeeded = %d with 2 candidates, want 2", n)
	}
}

// D70: during a PostgreSQL minor update, of copies holding the same
// writes the one on the older minor becomes the main node (the others can
// copy it); a higher WAL end still always wins.
func TestChooseTargetOlderMinorOnTie(t *testing.T) {
	st := newWorld(4, false).in.State
	sha := func(c byte) string { return strings.Repeat(string(c), 64) }
	s := st
	s.Members = map[string]cluster.Member{}
	for id, v := range st.Members {
		s.Members[id] = v
	}
	set := func(id string, c byte) {
		m := s.Members[id]
		m.Platform = cluster.Platform{OS: "linux", Arch: "amd64", PGBuildSHA: sha(c)}
		s.Members[id] = m
	}
	set("n1", '1')
	set("n2", '2')
	set("n3", '2')
	set("n4", '1')
	s.Sync.K = 1
	cut := func(lsn pg.LSN) CutResult { return CutResult{Cut: true, LSN: lsn} }
	res := map[string]CutResult{"n2": cut(30), "n3": cut(30), "n4": cut(30)}
	// Without the table: as before (lowest id).
	if got, _ := ChooseTarget(s, res, nil); got != "n2" {
		t.Fatalf("without the table: %q", got)
	}
	s.PGBuilds = map[string]cluster.PGBuild{
		sha('1'): {Version: "18.6.0", OS: "linux", Arch: "amd64"},
		sha('2'): {Version: "18.7.0", OS: "linux", Arch: "amd64"},
	}
	if got, _ := ChooseTarget(s, res, nil); got != "n4" {
		t.Fatalf("tie: %q, want n4 (18.6)", got)
	}
	res["n3"] = cut(31)
	if got, _ := ChooseTarget(s, res, nil); got != "n3" {
		t.Fatalf("higher WAL end: %q, want n3", got)
	}
}
