package controller

import (
	"strings"
	"testing"

	"coyotedb.com/coyote/internal/pg"
)

// D55: a copy whose data pages are known to be damaged is never promoted,
// never (re)added to the sync set, and is reported.

func damagedCopy(m *MemberObs) {
	r := *m.Report
	r.PagesDamaged = "3 data page(s) failed their checksum when read"
	m.Report = &r
}

func TestFailoverBlockedWhenEveryCandidateDamaged(t *testing.T) {
	// C1: two data nodes and a witness; the only copy is damaged.
	w := newWorld(2, true).primaryGone().member("n2", damagedCopy)
	p := Decide(w.in)
	if p.Kind == ActFailover {
		t.Fatalf("failover planned to a damaged copy: %+v", p)
	}
	for _, s := range []string{"damaged data pages", "node-n2", "Start the main node again"} {
		if !strings.Contains(p.View.FailoverBlocked, s) {
			t.Fatalf("blocked %q: missing %q", p.View.FailoverBlocked, s)
		}
	}
}

func TestFailoverGoesAheadWhenAHealthyCandidateRemains(t *testing.T) {
	w := newWorld(3, false).primaryGone().member("n2", damagedCopy)
	if p := Decide(w.in); p.Kind != ActFailover {
		t.Fatalf("plan %v (%s), want failover (n3 is healthy)", p.Kind, p.View.FailoverBlocked)
	}
}

func TestChooseTargetSkipsDamaged(t *testing.T) {
	st := newWorld(4, false).in.State
	cut := func(lsn pg.LSN) CutResult { return CutResult{Cut: true, LSN: lsn} }
	bad := map[string]string{"n3": "damaged"}
	cases := []struct {
		name string
		res  map[string]CutResult
		want string
	}{
		{name: "damaged has the highest LSN alone: refuse (the others may miss writes)",
			res: map[string]CutResult{"n2": cut(10), "n3": cut(30), "n4": cut(20)}, want: ""},
		{name: "a healthy one ties the damaged one's LSN: it is chosen",
			res: map[string]CutResult{"n2": cut(10), "n3": cut(30), "n4": cut(30)}, want: "n4"},
		{name: "damaged is behind: the highest healthy one wins as usual",
			res: map[string]CutResult{"n2": cut(40), "n3": cut(30), "n4": cut(20)}, want: "n2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := ChooseTarget(st, c.res, bad)
			if got != c.want {
				t.Fatalf("got %q (%s), want %q", got, why, c.want)
			}
			if got == "" && !strings.Contains(why, "node-n3") {
				t.Fatalf("refusal must name the damaged copy: %q", why)
			}
		})
	}
}

func TestDamagedCopyNotAddedToSyncSet(t *testing.T) {
	// Out of R, streaming and caught up, but damaged: not added back.
	w := newWorld(3, false).withR("n2").member("n3", damagedCopy).member("n3", func(m *MemberObs) { m.InR = false })
	if p := Decide(w.in); p.Kind == ActSyncAdd && p.Member == "n3" {
		t.Fatalf("damaged copy added to the sync set: %+v", p)
	}
	// Running on one copy: a damaged copy is not made eligible.
	w = newWorld(2, true).member("n2", damagedCopy).member("n2", func(m *MemberObs) { m.InR = false })
	w.in.State.StandbyEligible = false
	w.in.State.Sync.Members = []string{"n1"}
	w.in.Primary.SyncNames = nil
	p := Decide(w.in)
	if p.Kind == ActMakeEligible {
		t.Fatalf("damaged copy made eligible: %+v", p)
	}
	if !strings.Contains(strings.Join(p.View.Notes, " "), "damaged data pages") {
		t.Fatalf("notes %q", p.View.Notes)
	}
}

func TestDamagedMainNodeReported(t *testing.T) {
	w := newWorld(3, false)
	w.in.Primary.PagesDamaged = "1 data page(s) failed their checksum when read"
	p := Decide(w.in)
	if !strings.Contains(strings.Join(p.View.Notes, " "), "(the main node) has damaged data pages") {
		t.Fatalf("notes %q", p.View.Notes)
	}
}
