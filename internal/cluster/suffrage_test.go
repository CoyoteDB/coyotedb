package cluster

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"
)

func mkMembers(data, witnesses int) []Member {
	var out []Member
	for i := range data {
		out = append(out, Member{ID: fmt.Sprintf("d%d", i+1), Role: RoleData, JoinedAt: t0.Add(time.Duration(i) * time.Minute)})
	}
	for i := range witnesses {
		out = append(out, Member{ID: fmt.Sprintf("w%d", i+1), Role: RoleWitness, JoinedAt: t0.Add(time.Hour)})
	}
	return out
}

func voters(m map[string]Suffrage) []string {
	var v []string
	for id, s := range m {
		if s == Voter {
			v = append(v, id)
		}
	}
	slices.Sort(v)
	return v
}

func TestDesiredSuffrage(t *testing.T) {
	cases := []struct {
		data, wit int
		primary   string
		voters    []string
	}{
		{1, 0, "d1", []string{"d1"}},
		{2, 0, "d1", []string{"d1"}},                   // second data node is a non-voter
		{2, 0, "d2", []string{"d2"}},                   // the primary keeps the vote
		{2, 0, "", []string{"d1"}},                     // no primary: earliest joined
		{2, 1, "d1", []string{"d1", "d2", "w1"}},       // 2 + witness: all three vote
		{2, 2, "d2", []string{"d1", "d2", "w1", "w2"}}, // 2 + 2 witnesses
		{3, 0, "d1", []string{"d1", "d2", "d3"}},
		{3, 1, "d1", []string{"d1", "d2", "d3", "w1"}}, // 3 + witness
		{4, 0, "d2", []string{"d1", "d2", "d3", "d4"}},
		{5, 0, "d1", []string{"d1", "d2", "d3", "d4", "d5"}},
		{1, 1, "d1", []string{"d1"}}, // 1 + witness: witness does not vote
		{1, 2, "d1", []string{"d1"}}, // 1 + 2 witnesses
	}
	for _, c := range cases {
		got := DesiredSuffrage(mkMembers(c.data, c.wit), c.primary)
		if len(got) != c.data+c.wit {
			t.Fatalf("%d+%d: %v", c.data, c.wit, got)
		}
		if v := voters(got); !slices.Equal(v, c.voters) {
			t.Errorf("%d data + %d witness, primary %q: voters %v, want %v", c.data, c.wit, c.primary, v, c.voters)
		}
	}
}

func toConfig(m map[string]Suffrage) []ConfigServer {
	var out []ConfigServer
	for _, id := range slices.Sorted(maps.Keys(m)) {
		out = append(out, ConfigServer{ID: id, Suffrage: m[id]})
	}
	return out
}

// drive applies NextStep until done and returns the steps taken. It checks
// every intermediate configuration keeps a healthy majority of voters.
func drive(t *testing.T, cur map[string]Suffrage, desired map[string]Suffrage, self string, dead ...string) []Step {
	t.Helper()
	healthy := func(id string) bool { return !slices.Contains(dead, id) }
	var steps []Step
	for range 20 {
		st, ok := NextStep(toConfig(cur), desired, self, healthy, healthy)
		if !ok {
			return steps
		}
		steps = append(steps, st)
		switch st.Kind {
		case StepAddNonvoter:
			cur[st.ID] = Nonvoter
		case StepPromote:
			cur[st.ID] = Voter
		case StepRemove:
			delete(cur, st.ID)
		case StepDemote:
			cur[st.ID] = Nonvoter
		case StepTransferLeadership:
			return steps
		}
		v, live := 0, 0
		for id, s := range cur {
			if s == Voter {
				v++
				if healthy(id) {
					live++
				}
			}
		}
		if v == 0 || live < v/2+1 {
			t.Fatalf("step %v left config %v without a healthy majority", st, cur)
		}
	}
	t.Fatal("did not converge")
	return nil
}

func TestTransitions(t *testing.T) {
	V, N := Voter, Nonvoter
	type tc struct {
		name    string
		cur     map[string]Suffrage
		members []Member
		primary string
		dead    []string
		want    []Step
		final   map[string]Suffrage
	}
	m := func(data, wit int) []Member { return mkMembers(data, wit) }
	cases := []tc{
		{
			name: "second data node joins: added as non-voter only",
			cur:  map[string]Suffrage{"d1": V}, members: m(2, 0), primary: "d1",
			want:  []Step{{StepAddNonvoter, "d2"}},
			final: map[string]Suffrage{"d1": V, "d2": N},
		},
		{
			name: "witness joins a 2-node cluster: add, then promote both",
			cur:  map[string]Suffrage{"d1": V, "d2": N}, members: m(2, 1), primary: "d1",
			want:  []Step{{StepAddNonvoter, "w1"}, {StepPromote, "d2"}, {StepPromote, "w1"}},
			final: map[string]Suffrage{"d1": V, "d2": V, "w1": V},
		},
		{
			name: "third data node joins: non-voter promoted",
			cur:  map[string]Suffrage{"d1": V, "d2": N}, members: m(3, 0), primary: "d1",
			want:  []Step{{StepAddNonvoter, "d3"}, {StepPromote, "d2"}, {StepPromote, "d3"}},
			final: map[string]Suffrage{"d1": V, "d2": V, "d3": V},
		},
		{
			name: "3 -> 2 data nodes (removed one dead): remove first, then demote",
			cur:  map[string]Suffrage{"d1": V, "d2": V, "d3": V}, members: m(2, 0), primary: "d1", dead: []string{"d3"},
			want:  []Step{{StepRemove, "d3"}, {StepDemote, "d2"}},
			final: map[string]Suffrage{"d1": V, "d2": N},
		},
		{
			name: "witness removed while it is alive",
			cur:  map[string]Suffrage{"d1": V, "d2": V, "w1": V}, members: m(2, 0), primary: "d1",
			want:  []Step{{StepRemove, "w1"}, {StepDemote, "d2"}},
			final: map[string]Suffrage{"d1": V, "d2": N},
		},
		{
			name: "witness removed while d2 is dead: demote d2 first to keep quorum",
			cur:  map[string]Suffrage{"d1": V, "d2": V, "w1": V}, members: m(2, 0), primary: "d1", dead: []string{"d2"},
			want:  []Step{{StepDemote, "d2"}, {StepRemove, "w1"}},
			final: map[string]Suffrage{"d1": V, "d2": N},
		},
		{
			name: "witness joins a single data node: stays a non-voter",
			cur:  map[string]Suffrage{"d1": V}, members: m(1, 1), primary: "d1",
			want:  []Step{{StepAddNonvoter, "w1"}},
			final: map[string]Suffrage{"d1": V, "w1": N},
		},
		{
			name: "second data node joins 1+witness: both promoted",
			cur:  map[string]Suffrage{"d1": V, "w1": N}, members: m(2, 1), primary: "d1",
			want:  []Step{{StepAddNonvoter, "d2"}, {StepPromote, "d2"}, {StepPromote, "w1"}},
			final: map[string]Suffrage{"d1": V, "d2": V, "w1": V},
		},
		{
			name: "2+witness -> one data node removed: remove it, then demote the witness",
			cur:  map[string]Suffrage{"d1": V, "d2": V, "w1": V}, members: []Member{mkMembers(1, 0)[0], mkMembers(0, 1)[0]}, primary: "d1",
			want:  []Step{{StepRemove, "d2"}, {StepDemote, "w1"}},
			final: map[string]Suffrage{"d1": V, "w1": N},
		},
		{
			name: "2+witness -> dead data node removed, leaving 1+witness",
			cur:  map[string]Suffrage{"d1": V, "d2": V, "w1": V}, members: []Member{mkMembers(1, 0)[0], mkMembers(0, 1)[0]}, primary: "d1", dead: []string{"d2"},
			want:  []Step{{StepRemove, "d2"}, {StepDemote, "w1"}},
			final: map[string]Suffrage{"d1": V, "w1": N},
		},
		{
			name: "3+witness -> 2+witness: all still vote",
			cur:  map[string]Suffrage{"d1": V, "d2": V, "d3": V, "w1": V}, members: append(mkMembers(2, 0), mkMembers(0, 1)...), primary: "d1",
			want:  []Step{{StepRemove, "d3"}},
			final: map[string]Suffrage{"d1": V, "d2": V, "w1": V},
		},
		{
			name: "5 -> 4 data nodes",
			cur:  map[string]Suffrage{"d1": V, "d2": V, "d3": V, "d4": V, "d5": V}, members: m(4, 0), primary: "d1",
			want:  []Step{{StepRemove, "d5"}},
			final: map[string]Suffrage{"d1": V, "d2": V, "d3": V, "d4": V},
		},
		{
			name: "dead non-voter is not promoted",
			cur:  map[string]Suffrage{"d1": V, "d2": N}, members: m(3, 0), primary: "d1", dead: []string{"d2"},
			want:  []Step{{StepAddNonvoter, "d3"}, {StepPromote, "d3"}},
			final: map[string]Suffrage{"d1": V, "d2": N, "d3": V},
		},
		{
			name: "2-node: primary moved to d2, leader d1 must hand over",
			cur:  map[string]Suffrage{"d1": V, "d2": N}, members: m(2, 0), primary: "d2",
			want:  []Step{{StepPromote, "d2"}, {StepTransferLeadership, "d2"}},
			final: map[string]Suffrage{"d1": V, "d2": V},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			desired := DesiredSuffrage(c.members, c.primary)
			cur := maps.Clone(c.cur)
			got := drive(t, cur, desired, "d1", c.dead...)
			if !slices.Equal(got, c.want) {
				t.Fatalf("steps %v, want %v", got, c.want)
			}
			if !maps.Equal(cur, c.final) {
				t.Fatalf("final %v, want %v", cur, c.final)
			}
		})
	}
}

func TestNoSafeStep(t *testing.T) {
	// d1 (self) + d2 voters, d2 dead, removing d2 would be fine (only d1
	// left) — but promoting a dead d3 never happens and quorum is never
	// dropped.
	healthy := func(id string) bool { return id != "d2" && id != "d3" }
	cur := []ConfigServer{{"d1", Voter}, {"d2", Voter}, {"d3", Nonvoter}}
	desired := map[string]Suffrage{"d1": Voter, "d2": Voter, "d3": Voter}
	if st, ok := NextStep(cur, desired, "d1", healthy, healthy); ok {
		t.Fatalf("unexpected step %v", st)
	}
}
