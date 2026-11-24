package check

import (
	"fmt"
	"slices"
	"time"
)

// Kind is what one probe of a node proved.
type Kind int

const (
	// RW: the node committed a transaction that holds a transaction id
	// (txid_current() in an autocommit statement, synchronous_commit =
	// local so the commit never waits for a copy): it accepted a write.
	RW Kind = iota
	// NotRW: the node answered that it is in recovery (SQLSTATE 25006 on
	// txid_current(), or 57P03 "starting up" / "in recovery mode"): at
	// that instant it could not accept a write.
	NotRW
	// Down: nothing listened on the node's Postgres port (connection
	// refused): no postmaster was running.
	Down
	// NoAnswer: no answer (timeout, broken connection, frozen process,
	// "shutting down", any other error). Never read as "not main".
	NoAnswer
)

func (k Kind) String() string { return [...]string{"rw", "not-rw", "down", "unknown"}[k] }

// Probe is one probe of one node. Start and End are harness-monotonic
// offsets (see the package doc); what it proves held at some instant in
// [Start, End].
type Probe struct {
	Node       string
	Start, End time.Duration
	Kind       Kind
	// Conn identifies the server session that answered an RW probe:
	// postmaster start time and backend pid. Two RW probes with the same
	// Conn were answered by one backend that lived in between, under one
	// postmaster that never restarted (a crash restart kills every
	// backend); a server that has left recovery never re-enters it
	// without a restart. So the node accepted writes during the whole
	// time between them.
	Conn   string
	Detail string
}

// Run is a stretch of time in which one node provably accepted writes:
// consecutive RW probes on one Conn, not interrupted by any proof of the
// opposite.
type Run struct {
	Node  string
	Conn  string
	Probe []Probe // the RW probes, in order
	// From/To bound the run from outside: the node was not read-write at
	// an instant in the NotRW/Down probe that started at From (none:
	// HasFrom false, -inf), nor at one in the probe that ended at To (none:
	// +inf). The run began after From and ended before To.
	From, To  time.Duration
	HasFrom   bool
	HasTo     bool
	firstFrom Probe // the proof behind From (for messages)
	firstTo   Probe // the proof behind To
}

// Sure is the interval in which the node certainly accepted writes:
// from the answer of its first RW probe to the send of its last. Empty
// (ok false) with a single probe.
func (r Run) Sure() (from, to time.Duration, ok bool) {
	a, b := r.Probe[0].End, r.Probe[len(r.Probe)-1].Start
	return a, b, len(r.Probe) > 1 && a <= b
}

// TwoMainsReport is the result of TwoMains.
type TwoMainsReport struct {
	// Proven: two nodes accepted writes at one instant.
	Proven []string
	// Unproven: the evidence cannot exclude that two nodes accepted
	// writes at one instant (one node's last proof of being read-write and
	// its first proof of having stopped bracket another node's start).
	Unproven []string
	Notes    []string
	Runs     []Run
}

// TwoMains decides, from the probes of every node, whether two nodes ever
// accepted writes at the same instant (safety rule 4: at most one
// writable main node). Timelines play no part: identity is the node and
// the server session that answered.
//
// For two runs a (node X) and b (node Y != X):
//   - proven overlap: their sure intervals meet, or one RW probe of a lies
//     wholly inside b's sure interval (or the other way round);
//   - proven apart: a's upper bound To is at or before b's lower bound
//     From (X had provably stopped before Y's last proof of not yet being
//     read-write), or the other way round;
//   - anything else is unproven: no evidence either way.
//
// A pass therefore needs, at every change of main node, a proof that the
// old one stopped (refused connections, or recovery) before the newest
// proof that the new one had not started.
func TwoMains(probes []Probe) TwoMainsReport {
	var rep TwoMainsReport
	by := map[string][]Probe{}
	for _, p := range probes {
		by[p.Node] = append(by[p.Node], p)
	}
	var names []string
	for n := range by {
		names = append(names, n)
	}
	slices.Sort(names)
	var runs []Run
	for _, n := range names {
		ps := by[n]
		slices.SortStableFunc(ps, func(a, b Probe) int { return cmpDur(a.Start, b.Start) })
		runs = append(runs, nodeRuns(n, ps)...)
		rep.Notes = append(rep.Notes, coverage(n, ps))
	}
	rep.Runs = runs
	for i := range runs {
		for j := i + 1; j < len(runs); j++ {
			a, b := runs[i], runs[j]
			if a.Node == b.Node {
				continue
			}
			if msg, ok := overlap(a, b); ok {
				rep.Proven = append(rep.Proven, msg)
				continue
			}
			if apart(a, b) || apart(b, a) {
				continue
			}
			if a.Probe[0].Start > b.Probe[0].Start {
				a, b = b, a
			}
			rep.Unproven = append(rep.Unproven, unprovenMsg(a, b))
		}
	}
	byStart := slices.Clone(runs)
	slices.SortFunc(byStart, func(a, b Run) int { return cmpDur(a.Probe[0].Start, b.Probe[0].Start) })
	for i := 1; i < len(byStart); i++ {
		a, b := byStart[i-1], byStart[i]
		if a.Node != b.Node && apart(a, b) {
			rep.Notes = append(rep.Notes, fmt.Sprintf("handover %s -> %s: %s provably stopped by %s (%s), %s provably not yet read-write at %s: %s apart",
				a.Node, b.Node, a.Node, ms(a.To), a.firstTo.Kind, b.Node, ms(b.From), (b.From-a.To).Round(time.Millisecond)))
		}
	}
	rep.Proven = limit(rep.Proven, 5)
	rep.Unproven = limit(rep.Unproven, 5)
	return rep
}

// nodeRuns splits one node's probes (in time order) into runs. A run's
// bounds come only from NotRW and Down probes of that node: an RW probe in
// another session (a reconnect, possibly to the same postmaster) proves
// nothing about when the node stopped accepting writes.
func nodeRuns(node string, ps []Probe) []Run {
	var runs []Run
	var cur *Run
	var proofs []Probe // NotRW and Down, in order
	for _, p := range ps {
		switch {
		case p.Kind == RW && cur != nil && p.Conn == cur.Conn:
			cur.Probe = append(cur.Probe, p)
		case p.Kind == RW:
			if cur != nil {
				runs = append(runs, *cur)
			}
			cur = &Run{Node: node, Conn: p.Conn, Probe: []Probe{p}}
		case p.Kind == NotRW || p.Kind == Down:
			if cur != nil {
				runs = append(runs, *cur)
				cur = nil
			}
			proofs = append(proofs, p)
		}
	}
	if cur != nil {
		runs = append(runs, *cur)
	}
	for i := range runs {
		r := &runs[i]
		first, last := r.Probe[0], r.Probe[len(r.Probe)-1]
		for _, p := range proofs {
			if p.End <= first.Start {
				r.From, r.HasFrom, r.firstFrom = p.Start, true, p
			}
			if p.Start >= last.End && !r.HasTo {
				r.To, r.HasTo, r.firstTo = p.End, true, p
			}
		}
	}
	return runs
}

func overlap(a, b Run) (string, bool) {
	af, at, aok := a.Sure()
	bf, bt, bok := b.Sure()
	if aok && bok && af <= bt && bf <= at {
		lo, hi := max(af, bf), min(at, bt)
		return fmt.Sprintf("TWO MAIN NODES: %s and %s both accepted writes throughout %s..%s (%s accepted writes %s..%s, %s %s..%s)",
			a.Node, b.Node, ms(lo), ms(hi), a.Node, ms(af), ms(at), b.Node, ms(bf), ms(bt)), true
	}
	inside := func(x, y Run) (string, bool) {
		yf, yt, ok := y.Sure()
		if !ok {
			return "", false
		}
		for _, p := range x.Probe {
			if p.Start >= yf && p.End <= yt {
				return fmt.Sprintf("TWO MAIN NODES: %s committed a write in %s..%s while %s accepted writes throughout %s..%s",
					x.Node, ms(p.Start), ms(p.End), y.Node, ms(yf), ms(yt)), true
			}
		}
		return "", false
	}
	if m, ok := inside(a, b); ok {
		return m, true
	}
	return inside(b, a)
}

// apart: a provably ended before b started.
func apart(a, b Run) bool { return a.HasTo && b.HasFrom && a.To <= b.From }

func unprovenMsg(a, b Run) string {
	last := a.Probe[len(a.Probe)-1]
	stop := "no proof it ever stopped"
	if a.HasTo {
		stop = fmt.Sprintf("first proof it had stopped (%s) only by %s", a.firstTo.Kind, ms(a.To))
	}
	start := "no proof it was not already read-write"
	if b.HasFrom {
		start = fmt.Sprintf("last proof it was not yet read-write (%s) from %s", b.firstFrom.Kind, ms(b.From))
	}
	return fmt.Sprintf("TWO MAIN NODES NOT EXCLUDED: %s last accepted a write in %s..%s, %s; %s accepted writes from %s..%s, %s",
		a.Node, ms(last.Start), ms(last.End), stop, b.Node, ms(b.Probe[0].Start), ms(b.Probe[0].End), start)
}

// coverage: how much of a node's probed time has no evidence at all.
func coverage(node string, ps []Probe) string {
	if len(ps) == 0 {
		return node + ": no probes"
	}
	var count [4]int
	var blind, longest time.Duration
	for _, p := range ps {
		count[p.Kind]++
		if p.Kind == NoAnswer {
			d := p.End - p.Start
			blind += d
			longest = max(longest, d)
		}
	}
	return fmt.Sprintf("probes of %s: %d rw, %d not-rw, %d down, %d unknown (no evidence for %s in total, longest %s)",
		node, count[RW], count[NotRW], count[Down], count[NoAnswer], blind.Round(time.Millisecond), longest.Round(time.Millisecond))
}

func ms(d time.Duration) string { return fmt.Sprintf("+%.3fs", d.Seconds()) }

func cmpDur(a, b time.Duration) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
