package check

import (
	"strings"
	"testing"
	"time"
)

const msec = time.Millisecond

// series makes back-to-back probes of one kind on node n, from t0 every
// step (each answered 5 ms after it was sent).
func series(n string, k Kind, conn string, from, to, step time.Duration) []Probe {
	var out []Probe
	for t := from; t < to; t += step {
		out = append(out, Probe{Node: n, Start: t, End: t + 5*msec, Kind: k, Conn: conn})
	}
	return out
}

func cat(ps ...[]Probe) []Probe {
	var out []Probe
	for _, p := range ps {
		out = append(out, p...)
	}
	return out
}

func TestTwoMainsCleanFailover(t *testing.T) {
	// a is main until it is fenced (refused from 6 s), b is a standby
	// (not-rw) until it is promoted at 11 s.
	ps := cat(
		series("a", RW, "a1", 0, 6000*msec, 20*msec),
		series("a", Down, "", 6000*msec, 20000*msec, 50*msec),
		series("b", NotRW, "", 0, 11000*msec, 20*msec),
		series("b", RW, "b1", 11000*msec, 20000*msec, 20*msec),
		series("c", NotRW, "", 0, 20000*msec, 20*msec),
	)
	r := TwoMains(ps)
	if len(r.Proven) > 0 || len(r.Unproven) > 0 {
		t.Fatalf("clean failover flagged: %v %v", r.Proven, r.Unproven)
	}
	if len(r.Runs) != 2 {
		t.Fatalf("want 2 runs, got %d", len(r.Runs))
	}
}

func TestTwoMainsProvenOverlap(t *testing.T) {
	// The old main node keeps committing until 13 s; the new one is
	// read-write from 11 s.
	ps := cat(
		series("a", RW, "a1", 0, 13000*msec, 20*msec),
		series("a", Down, "", 13000*msec, 20000*msec, 50*msec),
		series("b", NotRW, "", 0, 11000*msec, 20*msec),
		series("b", RW, "b1", 11000*msec, 20000*msec, 20*msec),
	)
	r := TwoMains(ps)
	if len(r.Proven) == 0 || !strings.Contains(r.Proven[0], "TWO MAIN NODES") {
		t.Fatalf("overlap not proven: %+v", r)
	}
}

func TestTwoMainsSingleCommitInsideOtherRun(t *testing.T) {
	// b answers rw once (then its probes hang) while a provably accepts
	// writes all around it.
	ps := cat(
		series("a", RW, "a1", 0, 10000*msec, 20*msec),
		[]Probe{{Node: "b", Start: 0, End: 5 * msec, Kind: NotRW},
			{Node: "b", Start: 5000 * msec, End: 5010 * msec, Kind: RW, Conn: "b1"},
			{Node: "b", Start: 5010 * msec, End: 9000 * msec, Kind: NoAnswer}},
	)
	r := TwoMains(ps)
	if len(r.Proven) != 1 || !strings.Contains(r.Proven[0], "b committed a write") {
		t.Fatalf("single commit inside a's run not proven: %+v", r.Proven)
	}
}

func TestTwoMainsFrozenOldMainIsNotAPass(t *testing.T) {
	// a's probes stop answering at 5 s (frozen) and never prove it
	// stopped; b becomes read-write at 11 s. No evidence is not "not
	// main": this must not pass.
	ps := cat(
		series("a", RW, "a1", 0, 5000*msec, 20*msec),
		[]Probe{{Node: "a", Start: 5000 * msec, End: 20000 * msec, Kind: NoAnswer}},
		series("b", NotRW, "", 0, 11000*msec, 20*msec),
		series("b", RW, "b1", 11000*msec, 20000*msec, 20*msec),
	)
	r := TwoMains(ps)
	if len(r.Proven) != 0 || len(r.Unproven) != 1 {
		t.Fatalf("want one unproven handover, got %+v / %+v", r.Proven, r.Unproven)
	}
	if !strings.Contains(r.Unproven[0], "no proof it ever stopped") {
		t.Fatalf("message: %s", r.Unproven[0])
	}
}

func TestTwoMainsStopProofTooLate(t *testing.T) {
	// a's last commit is at 5 s, b's last not-rw answer at 11 s, but the
	// first proof that a stopped comes only at 12 s.
	ps := cat(
		series("a", RW, "a1", 0, 5000*msec, 20*msec),
		[]Probe{{Node: "a", Start: 5000 * msec, End: 12000 * msec, Kind: NoAnswer}},
		series("a", Down, "", 12000*msec, 20000*msec, 50*msec),
		series("b", NotRW, "", 0, 11000*msec, 20*msec),
		series("b", RW, "b1", 11000*msec, 20000*msec, 20*msec),
	)
	if r := TwoMains(ps); len(r.Unproven) != 1 || len(r.Proven) != 0 {
		t.Fatalf("want unproven, got %+v", r)
	}
}

func TestTwoMainsNewMainWithoutPriorProof(t *testing.T) {
	// b's first evidence ever is read-write: nothing proves it was not
	// read-write while a was.
	ps := cat(
		series("a", RW, "a1", 0, 5000*msec, 20*msec),
		series("a", Down, "", 5000*msec, 20000*msec, 50*msec),
		[]Probe{{Node: "b", Start: 0, End: 11000 * msec, Kind: NoAnswer}},
		series("b", RW, "b1", 11000*msec, 20000*msec, 20*msec),
	)
	r := TwoMains(ps)
	if len(r.Unproven) != 1 || !strings.Contains(r.Unproven[0], "no proof it was not already read-write") {
		t.Fatalf("want unproven, got %+v", r)
	}
}

func TestTwoMainsReconnectIsNotAStop(t *testing.T) {
	// a reconnects at 5 s (new session, same postmaster for all we know):
	// that is no proof a stopped, so b's promotion at 7 s while a still
	// commits in its second session is proven two mains, and a's first
	// session gives no stop bound.
	ps := cat(
		series("a", RW, "a1", 0, 5000*msec, 20*msec),
		series("a", RW, "a2", 5000*msec, 10000*msec, 20*msec),
		series("a", Down, "", 10000*msec, 20000*msec, 50*msec),
		series("b", NotRW, "", 0, 7000*msec, 20*msec),
		series("b", RW, "b1", 7000*msec, 20000*msec, 20*msec),
	)
	r := TwoMains(ps)
	if len(r.Proven) != 1 {
		t.Fatalf("want proven overlap with a's second session, got %+v", r)
	}
}

func TestTwoMainsSwitchBack(t *testing.T) {
	// a -> b -> a, each change cleanly separated.
	ps := cat(
		series("a", RW, "a1", 0, 5000*msec, 20*msec),
		series("a", NotRW, "", 5000*msec, 12000*msec, 20*msec),
		series("a", RW, "a2", 12000*msec, 20000*msec, 20*msec),
		series("b", NotRW, "", 0, 6000*msec, 20*msec),
		series("b", RW, "b1", 6000*msec, 10000*msec, 20*msec),
		series("b", Down, "", 10000*msec, 20000*msec, 20*msec),
	)
	if r := TwoMains(ps); len(r.Proven)+len(r.Unproven) != 0 {
		t.Fatalf("clean switch back flagged: %+v", r)
	}
}

func at(s float64) time.Time {
	return time.Unix(1_800_000_000, 0).Add(time.Duration(s * float64(time.Second)))
}

func TestLostWrites(t *testing.T) {
	appends := []Append{
		{ID: 1, Node: "a", Start: at(0), End: at(0.01), Outcome: OK, TL: "00000001"},
		{ID: 2, Node: "a", Start: at(1), End: at(1.01), Outcome: OK, TL: "00000001"},
		{ID: 3, Node: "a", Start: at(2), End: at(2.01), Outcome: Failed},
		{ID: 4, Node: "a", Start: at(3), End: at(23), Outcome: Unknown},
		{ID: 5, Node: "b", Start: at(12), End: at(12.01), Outcome: OK, TL: "00000002"},
	}
	clean := map[int64]string{1: "00000001", 2: "00000001", 5: "00000002"}
	if p, ok, f, u := LostWrites(clean, appends, "b"); len(p) != 0 || ok != 3 || f != 1 || u != 1 {
		t.Fatalf("clean history: %v ok=%d failed=%d unknown=%d", p, ok, f, u)
	}
	withUnknown := map[int64]string{1: "00000001", 2: "00000001", 4: "00000001", 5: "00000002"}
	if p, _, _, _ := LostWrites(withUnknown, appends, "b"); len(p) != 0 {
		t.Fatalf("an unknown write may be present: %v", p)
	}
	lost := map[int64]string{1: "00000001", 5: "00000002"}
	p, _, _, _ := LostWrites(lost, appends, "b")
	if len(p) != 2 || !strings.Contains(p[1], "1 acknowledged appends are missing") || !strings.HasPrefix(p[0], "LOST WRITE: append 2") {
		t.Fatalf("lost write not reported: %v", p)
	}
	phantom := map[int64]string{1: "00000001", 2: "00000001", 3: "00000001", 5: "00000002"}
	if p, _, _, _ := LostWrites(phantom, appends, "b"); len(p) != 1 || !strings.Contains(p[0], "failed on a") {
		t.Fatalf("failed write present not reported: %v", p)
	}
	wrongTL := map[int64]string{1: "00000001", 2: "00000002", 5: "00000002"}
	if p, _, _, _ := LostWrites(wrongTL, appends, "b"); len(p) != 1 {
		t.Fatalf("timeline mismatch not reported: %v", p)
	}
}

func TestAckOrder(t *testing.T) {
	clean := []Append{
		{ID: 1, Node: "a", Start: at(0), End: at(0.01), Outcome: OK, TL: "00000001"},
		{ID: 2, Node: "a", Start: at(9), End: at(11.5), Outcome: OK, TL: "00000001"}, // sent before b's first ack ended
		{ID: 3, Node: "b", Start: at(11), End: at(11.2), Outcome: OK, TL: "00000002"},
		{ID: 4, Node: "a", Start: at(12), End: at(30), Outcome: Unknown},
	}
	if p := AckOrder(clean); len(p) != 0 {
		t.Fatalf("clean history flagged: %v", p)
	}
	late := append(clean, Append{ID: 5, Node: "a", Start: at(12), End: at(12.01), Outcome: OK, TL: "00000001"})
	if p := AckOrder(late); len(p) != 1 || !strings.Contains(p[0], "append 5") {
		t.Fatalf("old main acknowledging after the new one not flagged: %v", p)
	}
	sameTL := append(clean, Append{ID: 6, Node: "c", Start: at(13), End: at(13.01), Outcome: OK, TL: "00000002"})
	if p := AckOrder(sameTL); len(p) != 1 || !strings.Contains(p[0], "SPLIT BRAIN") {
		t.Fatalf("two nodes on one timeline not flagged: %v", p)
	}
}

func TestCoverageNote(t *testing.T) {
	ps := cat(series("a", RW, "a1", 0, 100*msec, 20*msec),
		[]Probe{{Node: "a", Start: 100 * msec, End: 2100 * msec, Kind: NoAnswer}},
		series("a", Down, "", 2100*msec, 2200*msec, 50*msec))
	r := TwoMains(ps)
	want := "probes of a: 5 rw, 0 not-rw, 2 down, 1 unknown (no evidence for 2s in total, longest 2s)"
	if len(r.Notes) < 1 || r.Notes[0] != want {
		t.Fatalf("note %q, want %q", r.Notes, want)
	}
}

func TestHandoverNote(t *testing.T) {
	ps := cat(
		series("a", RW, "a1", 0, 6000*msec, 20*msec),
		series("a", Down, "", 6000*msec, 20000*msec, 50*msec),
		series("b", NotRW, "", 0, 11000*msec, 20*msec),
		series("b", RW, "b1", 11000*msec, 20000*msec, 20*msec),
	)
	r := TwoMains(ps)
	want := "handover a -> b: a provably stopped by +6.005s (down), b provably not yet read-write at +10.980s: 4.975s apart"
	if r.Notes[len(r.Notes)-1] != want {
		t.Fatalf("notes %q", r.Notes)
	}
}
