package controller

import (
	"fmt"
	"slices"
	"strings"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// CutResult is one member's answer to "cut your stream".
type CutResult struct {
	Cut bool   // no WAL receiver any more, and LSN is its WAL end
	LSN pg.LSN // pg_last_wal_receive_lsn (or the replayed end, see pg.CutStream)
	Err string
}

// ChooseTarget picks the member to promote after a rule 2 cut: among the
// candidates (Candidates) whose stream is confirmed cut, the one with the
// highest WAL end; ties go to the older PostgreSQL minor (D70), then to a
// voter, then to the lowest id. It refuses
// (returns "" and why) unless at least CutsNeeded candidates are cut, so
// the old primary can no longer get a write confirmed and the chosen one
// holds every acknowledged write.
//
// D55: a candidate whose data is known to be damaged (damaged: id ->
// plain words) is never chosen. Another candidate is chosen only if it
// holds every write the damaged one holds (the same WAL end or more);
// otherwise it refuses: a copy that misses acknowledged writes is no
// better than one that cannot read some of them, and the main node coming
// back loses nothing.
func ChooseTarget(st cluster.State, cut map[string]CutResult, damaged map[string]string) (string, string) {
	cands := Candidates(st)
	if len(cands) == 0 {
		return "", "no member of the sync set can be promoted"
	}
	need := CutsNeeded(st, cands)
	var ok []string
	var failed []string
	for _, id := range cands {
		if r, found := cut[id]; found && r.Cut {
			ok = append(ok, id)
		} else {
			failed = append(failed, st.Members[id].Name)
		}
	}
	if len(ok) < need {
		return "", fmt.Sprintf("only %d of the %d copies in the sync set could be disconnected from the old main node (%d needed, safety rule 2); not reached: %s",
			len(ok), len(cands), need, strings.Join(failed, ", "))
	}
	slices.SortFunc(ok, func(a, b string) int {
		la, lb := cut[a].LSN, cut[b].LSN
		if la != lb {
			if la > lb {
				return -1
			}
			return 1
		}
		// D70: during a PostgreSQL minor update, of two copies that hold
		// the same writes the one on the older minor becomes the main
		// node, so the others (same or newer) can keep copying it.
		if c := st.ComparePG(st.Members[a].Platform, st.Members[b].Platform); c != 0 && st.SamePGLine(st.Members[a].Platform, st.Members[b].Platform) {
			return c
		}
		va, vb := st.Members[a].Suffrage == cluster.Voter, st.Members[b].Suffrage == cluster.Voter
		if va != vb {
			if va {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	top := cut[ok[0]].LSN
	var bad []string
	for _, id := range ok {
		if cut[id].LSN < top {
			break
		}
		if _, d := damaged[id]; !d {
			return id, ""
		}
		bad = append(bad, fmt.Sprintf("%s (%s)", st.Members[id].Name, damaged[id]))
	}
	return "", damagedRefusal(bad)
}

// damagedRefusal is why no copy is promoted when the only ones holding
// every saved write have damaged data.
func damagedRefusal(bad []string) string {
	return fmt.Sprintf("the only copies that hold every saved write have damaged data pages, so none is made the main node "+
		"(it could not read part of your data): %s. Start the main node again; the damaged copy is then copied fresh from it",
		strings.Join(bad, ", "))
}
