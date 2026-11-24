// Package check is the pure part of the chaos checker: it judges a
// recorded history and talks to nothing. Keeping it free of build tags and
// of the cluster harness means `make test` runs its unit tests (synthetic
// histories with an overlap, a lost write and clean runs), so the checker
// itself is tested before any chaos run trusts it.
//
// # Clock
//
// Every instant in a history is taken by the chaos harness, one process on
// the machine that runs every node, with time.Now(). Go's time.Time carries
// a monotonic reading, and Before/After/Sub between two such values use
// it, so the comparisons below are on one monotonic clock and wall-clock
// steps cannot reorder them. Probe instants are stored as offsets from one
// base instant (time.Since(base)), which is the same clock made explicit.
// Server clocks are not used anywhere.
//
// What a client instant proves: a statement sent at Start and answered at
// End took effect at some instant in [Start, End], never outside it.
package check

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"
)

// Outcome of one client operation, as the client saw it.
type Outcome int

const (
	OK      Outcome = iota // acknowledged
	Failed                 // the server said ERROR: it did not happen
	Unknown                // the connection died or timed out: may or may not have happened
)

func (o Outcome) String() string { return [...]string{"ok", "failed", "unknown"}[o] }

// Append is one insert of a unique id.
type Append struct {
	ID         int64
	Node       string
	Start, End time.Time
	Outcome    Outcome
	TL         string // insert timeline the row records, 8 hex digits (ok only)
}

// LostWrites compares the appends with the rows on the final main node
// (id -> timeline). Every acknowledged append must be there with the
// timeline its client was told; no failed one may be.
func LostWrites(have map[int64]string, appends []Append, final string) (problems []string, ok, failed, unknown int) {
	var lost []string
	defer func() {
		if len(lost) > 0 {
			problems = append(limit(lost, 10), problems...)
			problems = append(problems, fmt.Sprintf("LOST WRITES: %d acknowledged appends are missing in total", len(lost)))
		}
	}()
	for _, op := range appends {
		switch op.Outcome {
		case OK:
			ok++
			tl, found := have[op.ID]
			if !found {
				lost = append(lost, fmt.Sprintf("LOST WRITE: append %d was acknowledged by %s at %s (timeline %s) but is not on the final main node %s",
					op.ID, op.Node, op.End.Format("15:04:05.000"), op.TL, final))
			} else if tl != op.TL {
				problems = append(problems, fmt.Sprintf("append %d: row says timeline %s, the client was told %s", op.ID, tl, op.TL))
			}
		case Failed:
			failed++
			if _, found := have[op.ID]; found {
				problems = append(problems, fmt.Sprintf("append %d failed on %s (server error) but is present", op.ID, op.Node))
			}
		default:
			unknown++
		}
	}
	return problems, ok, failed, unknown
}

// AckOrder checks the acknowledged appends against each other, across
// timelines (a promotion starts a new, higher timeline):
//
//   - one timeline, one node: two nodes acknowledging on the same
//     timeline means two promotions produced it;
//   - no going back: an append acknowledged on timeline t that the client
//     sent after an append on a higher timeline t' had already been
//     acknowledged proves a node still acknowledged writes after a newer
//     main node had. (The newer commit happened by its End; the older
//     one at or after its Start, which is later.) Old and new main node
//     are on different timelines in the usual two-mains case, which a
//     per-timeline grouping alone never sees.
func AckOrder(appends []Append) []string {
	var out []string
	nodes := map[string]map[string]bool{}
	type first struct {
		end time.Time
		op  Append
	}
	firstEnd := map[uint64]first{} // timeline -> earliest acknowledgement on it
	var oks []Append
	for _, op := range appends {
		if op.Outcome != OK {
			continue
		}
		oks = append(oks, op)
		if nodes[op.TL] == nil {
			nodes[op.TL] = map[string]bool{}
		}
		nodes[op.TL][op.Node] = true
		tl, err := strconv.ParseUint(op.TL, 16, 32)
		if err != nil {
			out = append(out, fmt.Sprintf("append %d: timeline %q is not hex", op.ID, op.TL))
			continue
		}
		if f, ok := firstEnd[tl]; !ok || op.End.Before(f.end) {
			firstEnd[tl] = first{end: op.End, op: op}
		}
	}
	var tls []string
	for tl := range nodes {
		tls = append(tls, tl)
	}
	sort.Strings(tls)
	for _, tl := range tls {
		if len(nodes[tl]) > 1 {
			out = append(out, fmt.Sprintf("SPLIT BRAIN: writes on timeline %s were acknowledged by more than one node: %v", tl, sortedKeys(nodes[tl])))
		}
	}
	var bad []string
	for _, op := range oks {
		tl, err := strconv.ParseUint(op.TL, 16, 32)
		if err != nil {
			continue
		}
		for t2, f := range firstEnd {
			if t2 > tl && f.end.Before(op.Start) {
				bad = append(bad, fmt.Sprintf("TWO MAIN NODES ACKNOWLEDGING: append %d was sent at %s and acknowledged by %s on timeline %s, after append %d had been acknowledged by %s on the newer timeline %08X at %s",
					op.ID, op.Start.Format("15:04:05.000"), op.Node, op.TL, f.op.ID, f.op.Node, t2, f.end.Format("15:04:05.000")))
				break
			}
		}
	}
	return append(out, limit(bad, 5)...)
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func limit(s []string, n int) []string {
	if len(s) > n {
		return append(s[:n:n], fmt.Sprintf("… and %d more like it", len(s)-n))
	}
	return s
}
