package cluster

import "slices"

// DesiredSuffrage is the suffrage policy (decision D17, revised):
//
//   - Exactly one data node: only it votes; every witness is a non-voter.
//     (Two voters would let a dead witness stall Raft, the lone primary
//     could not renew its lease and would fence itself: a helper with no
//     data would take the database down.)
//   - Exactly two data nodes and no witness: only one of them votes, so it
//     keeps Raft quorum alone and can record degraded mode (safety rule 9).
//     Switching the primary in that setup is a manual forced operation.
//     The one that votes is the current primary if it is one of the two,
//     else the earliest joined (JoinedAt, then id).
//   - Every other case: all data nodes and all witnesses vote.
func DesiredSuffrage(members []Member, primaryID string) map[string]Suffrage {
	out := make(map[string]Suffrage, len(members))
	var data, witnesses []Member
	for _, m := range members {
		if m.Role == RoleWitness {
			witnesses = append(witnesses, m)
		} else {
			data = append(data, m)
		}
	}
	sortMembers(data)
	switch {
	case len(data) == 1:
		out[data[0].ID] = Voter
		for _, w := range witnesses {
			out[w.ID] = Nonvoter
		}
	case len(data) == 2 && len(witnesses) == 0:
		keep := data[0].ID
		if data[1].ID == primaryID {
			keep = data[1].ID
		}
		for _, m := range data {
			if m.ID == keep {
				out[m.ID] = Voter
			} else {
				out[m.ID] = Nonvoter
			}
		}
	default:
		for _, m := range members {
			out[m.ID] = Voter
		}
	}
	return out
}

// ConfigServer is one server in the Raft configuration.
type ConfigServer struct {
	ID       string
	Suffrage Suffrage
}

// StepKind is a single Raft configuration change.
type StepKind int

// Step kinds, in the order they are preferred.
const (
	StepNone StepKind = iota
	StepAddNonvoter
	StepPromote
	StepRemove
	StepDemote
	// StepTransferLeadership means the only remaining change targets the
	// leader itself: hand leadership to another healthy voter first.
	StepTransferLeadership
)

func (k StepKind) String() string {
	return [...]string{"none", "add-nonvoter", "promote", "remove", "demote", "transfer-leadership"}[k]
}

// Step is one change.
type Step struct {
	Kind StepKind
	ID   string
}

// NextStep picks the next single configuration change that moves current
// towards desired without dropping below a working quorum:
//
//  1. add missing servers as non-voters (never affects quorum);
//  2. promote non-voters that should vote, once they are healthy;
//  3. remove servers that are no longer members;
//  4. demote voters that should not vote.
//
// healthy(id) means the server is reachable now; ready(id) means it has
// caught up enough to be promoted. A promotion, removal or demotion is only
// taken if, in the resulting configuration, the healthy voters are still a
// majority. Among the
// candidates of a kind, the first (by id) safe one is chosen; if none of a
// kind is safe the next kind is tried, so e.g. with a dead node either a
// demotion or a removal is chosen depending on which keeps quorum. Changes to
// self (the leader) are never returned; StepTransferLeadership is returned
// instead when they are all that is left and another healthy voter exists.
// ok is false when current already equals desired or no safe step exists.
func NextStep(current []ConfigServer, desired map[string]Suffrage, self string, healthy, ready func(id string) bool) (Step, bool) {
	cur := map[string]Suffrage{}
	for _, s := range current {
		cur[s.ID] = s.Suffrage
	}
	ids := func(m map[string]Suffrage) []string {
		var out []string
		for id := range m {
			out = append(out, id)
		}
		slices.Sort(out)
		return out
	}
	safe := func(next map[string]Suffrage) bool {
		voters, ok := 0, 0
		for id, s := range next {
			if s != Voter {
				continue
			}
			voters++
			if id == self || healthy(id) {
				ok++
			}
		}
		return voters > 0 && ok >= voters/2+1
	}
	with := func(id string, s Suffrage, remove bool) map[string]Suffrage {
		n := make(map[string]Suffrage, len(cur)+1)
		for k, v := range cur {
			n[k] = v
		}
		if remove {
			delete(n, id)
		} else {
			n[id] = s
		}
		return n
	}

	selfPending := false
	// 1. adds
	for _, id := range ids(desired) {
		if _, ok := cur[id]; !ok {
			return Step{StepAddNonvoter, id}, true
		}
	}
	// 2. promotions
	for _, id := range ids(desired) {
		if desired[id] == Voter && cur[id] == Nonvoter {
			if id == self {
				selfPending = true
				continue
			}
			if healthy(id) && ready(id) && safe(with(id, Voter, false)) {
				return Step{StepPromote, id}, true
			}
		}
	}
	// 3. removals
	for _, id := range ids(cur) {
		if _, ok := desired[id]; !ok {
			if id == self {
				selfPending = true
				continue
			}
			if safe(with(id, "", true)) {
				return Step{StepRemove, id}, true
			}
		}
	}
	// 4. demotions
	for _, id := range ids(cur) {
		if d, ok := desired[id]; ok && d == Nonvoter && cur[id] == Voter {
			if id == self {
				selfPending = true
				continue
			}
			if safe(with(id, Nonvoter, false)) {
				return Step{StepDemote, id}, true
			}
		}
	}
	if selfPending {
		for _, id := range ids(cur) {
			if id != self && cur[id] == Voter && desired[id] == Voter && healthy(id) {
				return Step{StepTransferLeadership, id}, true
			}
		}
	}
	return Step{}, false
}
