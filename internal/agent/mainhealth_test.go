package agent

import (
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

func healthState(witness bool, data ...string) cluster.State {
	st := cluster.State{Epoch: 3, Members: map[string]cluster.Member{}, PrimaryID: data[0], LastServed: data[0], StandbyEligible: true}
	for _, id := range data {
		st.Members[id] = cluster.Member{ID: id, Name: "node-" + id, Role: cluster.RoleData, Status: cluster.StatusActive}
		if id != data[0] {
			st.Sync.Members = append(st.Sync.Members, id)
		}
	}
	if witness {
		st.Members["w"] = cluster.Member{ID: "w", Name: "witness", Role: cluster.RoleWitness, Status: cluster.StatusActive}
	}
	st.Sync.K = 1
	return st
}

func standbyRep(lsn pg.LSN, free uint64) *NodeReport {
	return &NodeReport{PG: &PGReport{Running: true, Role: "standby", ReceiveLSN: &lsn}, DiskFree: &free}
}

// The probe's streak: only stalled or disk-full answers count, from the
// start of the first such probe; any other answer clears it.
func TestMainHealthStreak(t *testing.T) {
	var h mainHealth
	s := time.Hour
	at := func(d time.Duration) time.Duration { return s + d }

	// A probe in flight counts from its start: a hung one shows at once.
	h.inflight, h.inflightAt = true, at(0)
	if got := h.reason(at(stallAfter - time.Millisecond)); got != "" {
		t.Fatalf("before stallAfter: %q", got)
	}
	if got := h.reason(at(stallAfter)); got != reasonStall {
		t.Fatalf("hung probe for stallAfter: %q, want stalled", got)
	}
	// It ends in a timeout; the next one hangs too: still from the first.
	h.probeDone(at(0), pg.ProbeStalled)
	h.inflight, h.inflightAt = true, at(30*time.Second)
	if d := h.stalledFor(at(31 * time.Second)); d != 31*time.Second {
		t.Fatalf("streak from the first stalled probe: %s", d)
	}
	// One success ends it.
	h.probeDone(at(30*time.Second), pg.ProbeOK)
	if d := h.stalledFor(at(40 * time.Second)); d != 0 {
		t.Fatalf("after success: %s", d)
	}
	// An answer that says nothing about this disk (Postgres restarting)
	// ends it too: no hand-over on errors every node would have.
	h.probeDone(at(41*time.Second), pg.ProbeStalled)
	h.probeDone(at(50*time.Second), pg.ProbeOther)
	if got := h.reason(at(70 * time.Second)); got != "" {
		t.Fatalf("after an other-kind answer: %q", got)
	}
	// Disk-full answers make the reason "disk full".
	h.probeDone(at(80*time.Second), pg.ProbeDiskFull)
	h.probeDone(at(85*time.Second), pg.ProbeStalled)
	if got := h.reason(at(80*time.Second + stallAfter)); got != reasonDisk {
		t.Fatalf("disk-full streak: %q", got)
	}
	// A slow but answering probe never counts, however often.
	var g mainHealth
	for i := range 100 {
		start := at(time.Duration(i) * time.Second)
		g.inflight, g.inflightAt = true, start
		if got := g.reason(start + 900*time.Millisecond); got != "" {
			t.Fatalf("slow probe %d: %q", i, got)
		}
		g.probeDone(start, pg.ProbeOK)
	}
}

func TestMainHealthDisk(t *testing.T) {
	var h mainHealth
	s := time.Hour
	h.noteDisk(s, diskCritical-1, 1<<30, nil)
	if got := h.reason(s + diskLowFor - time.Millisecond); got != "" {
		t.Fatalf("low disk only just now: %q", got)
	}
	if got := h.reason(s + diskLowFor); got != reasonDisk {
		t.Fatalf("low disk for diskLowFor: %q", got)
	}
	h.noteDisk(s+5*time.Second, diskCritical, 1<<30, nil)
	if got := h.reason(s + time.Minute); got != "" {
		t.Fatalf("space back: %q", got)
	}
	if f, ok := h.diskFree(); !ok || f != diskCritical {
		t.Fatalf("diskFree %d %v", f, ok)
	}
}

func TestHandOverBlocked(t *testing.T) {
	ok := 30 * time.Second
	if why := handOverBlocked(healthState(false, "a", "b", "c"), "a", ok); why != "" {
		t.Fatalf("3 data nodes: %q", why)
	}
	if why := handOverBlocked(healthState(true, "a", "b"), "a", ok); why != "" {
		t.Fatalf("2 data + witness: %q", why)
	}
	for name, c := range map[string]struct {
		st      cluster.State
		mainFor time.Duration
		want    string
	}{
		"alone":             {healthState(false, "a"), ok, "no other data node"},
		"two, no witness":   {healthState(false, "a", "b"), ok, "no witness"},
		"one copy (rule 9)": {func() cluster.State { s := healthState(false, "a", "b", "c"); s.StandbyEligible = false; return s }(), ok, "one copy"},
		"not served":        {func() cluster.State { s := healthState(false, "a", "b", "c"); s.LastServed = "b"; return s }(), ok, "only recently"},
		"too new":           {healthState(false, "a", "b", "c"), mainSettle - time.Second, "seconds ago"},
	} {
		if why := handOverBlocked(c.st, "a", c.mainFor); !strings.Contains(why, c.want) {
			t.Errorf("%s: %q, want it to say %q", name, why, c.want)
		}
	}
}

func TestPickHandOver(t *testing.T) {
	st := healthState(false, "a", "b", "c", "d")
	st.Sync.Members = []string{"b", "c"} // d is not in R: never a target
	big := uint64(10 << 30)
	reps := []copyReport{{"b", standbyRep(100, big)}, {"c", standbyRep(200, big)}, {"d", standbyRep(900, big)}}
	if id, why := pickHandOver(st, "a", reasonStall, 0, reps); id != "c" {
		t.Fatalf("most received in R: %q (%s)", id, why)
	}
	// Ties go by name.
	reps[1].Rep = standbyRep(100, big)
	if id, _ := pickHandOver(st, "a", reasonStall, 0, reps); id != "b" {
		t.Fatalf("tie: %q", id)
	}
	// Not running, cut, rejoining, unreachable: not a target.
	bad := []*NodeReport{
		nil,
		{PG: &PGReport{Running: false, Role: "standby"}},
		{PG: &PGReport{Running: true, Role: "standby", StreamCut: true}},
		{PG: &PGReport{Running: true, Role: "standby"}, Marker: "rewind"},
		{PG: &PGReport{Running: true, Role: "primary"}},
	}
	for i, r := range bad {
		if id, why := pickHandOver(st, "a", reasonStall, 0, []copyReport{{"b", r}}); id != "" || why == "" {
			t.Errorf("bad copy %d chosen: %q", i, id)
		}
	}
	// A full disk hands over only to a copy with room, and more than ours.
	small := []copyReport{{"b", standbyRep(100, diskTargetMin-1)}, {"c", standbyRep(100, 1<<20)}}
	id, why := pickHandOver(st, "a", reasonDisk, 1<<20, small)
	if id != "" || !strings.Contains(why, "room") {
		t.Fatalf("full disks everywhere: %q %q", id, why)
	}
	small[0].Rep = standbyRep(100, diskTargetMin)
	if id, _ := pickHandOver(st, "a", reasonDisk, 1<<20, small); id != "b" {
		t.Fatalf("copy with room: %q", id)
	}
	// An older node that does not report its disk is not trusted with it.
	old := []copyReport{{"b", &NodeReport{PG: &PGReport{Running: true, Role: "standby"}}}}
	if id, _ := pickHandOver(st, "a", reasonDisk, 0, old); id != "" {
		t.Fatalf("unknown room chosen: %q", id)
	}
	if id, _ := pickHandOver(st, "a", reasonStall, 0, old); id != "b" {
		t.Fatalf("a stall does not need room: %q", id)
	}
}
