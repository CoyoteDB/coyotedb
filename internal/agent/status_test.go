package agent

import (
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/controller"
)

// threeNodes: n1 main, n2 and n3 in the sync set, as status sees them
// from a node that reached the main node; neither copy is connected.
func threeNodes() (cluster.State, Status) {
	st := cluster.State{ClusterID: "c1", Epoch: 1, PrimaryID: "n1", StandbyEligible: true,
		Members: map[string]cluster.Member{
			"n1": {ID: "n1", Name: "n1", Role: cluster.RoleData},
			"n2": {ID: "n2", Name: "n2", Role: cluster.RoleData},
			"n3": {ID: "n3", Name: "n3", Role: cluster.RoleData},
		},
		Sync: cluster.SyncSet{Members: []string{"n1", "n2", "n3"}, K: 1},
	}
	st.Settings.DegradeAfter = 30 * time.Second
	s := Status{Cluster: ClusterView{ID: "c1", Epoch: 1, PrimaryID: "n1", PrimaryName: "n1", LeaderID: "n2"}}
	s.Nodes = []MemberView{
		{ID: "n1", Name: "n1", Role: "data", Primary: true, InSyncSet: true, Reachable: true, Report: &NodeReport{}},
		{ID: "n2", Name: "n2", Role: "data", InSyncSet: true, Reachable: true, Report: &NodeReport{}, Replication: "not connected"},
		{ID: "n3", Name: "n3", Role: "data", InSyncSet: true, Reachable: true, Report: &NodeReport{}, Replication: "not connected"},
	}
	return st, s
}

// The report's case 2: the main node said "Writes are not waiting for any
// copy right now" and "Writes are waiting ..." at once (D62).
func TestStatusNeverSaysWaitingAndNotWaiting(t *testing.T) {
	st, s := threeNodes()
	h := health(st, s)
	if !h.WritesWait || strings.Contains(h.Copies, "not waiting") {
		t.Fatalf("health: %+v", h)
	}
	w := warnings(st, s, s.Nodes[0].Report)
	if !slicesHasPrefix(w, "Writes are waiting") {
		t.Fatalf("warnings: %q", w)
	}
	// No leader: the main node cannot be let carry on alone; say so.
	s.Cluster.LeaderID = ""
	w = warnings(st, s, s.Nodes[0].Report)
	for _, m := range w {
		if strings.HasPrefix(m, "Writes are waiting") && strings.Contains(m, "carries on alone") && !strings.Contains(m, "needs a leader") {
			t.Fatalf("promises a degrade with no leader: %q", m)
		}
	}
}

// A copy that cannot reach the main node does not claim writes are not
// waiting: it does not know.
func TestStatusMainUnreachableDoesNotGuess(t *testing.T) {
	st, s := threeNodes()
	s.Nodes[0].Reachable, s.Nodes[0].Report = false, nil
	s.Nodes[1].Replication, s.Nodes[2].Replication = "", ""
	h := health(st, s)
	if strings.Contains(h.Copies, "not waiting") || !strings.Contains(h.Copies, "not known") {
		t.Fatalf("health: %+v", h)
	}
}

// A node with no copy of the state yet (case 4) shows no made-up
// summary ("One data node: writes are saved on none only").
func TestStatusEmptyStateShowsNoSummary(t *testing.T) {
	h := health(cluster.State{}, Status{Waiting: &WaitingView{}})
	if h.Copies != "" || h.Failover != "" {
		t.Fatalf("health for an empty state: %+v", h)
	}
	if w := warnings(cluster.State{}, Status{}, nil); len(w) != 0 {
		t.Fatalf("warnings for an empty state: %q", w)
	}
}

func slicesHasPrefix(xs []string, p string) bool {
	for _, x := range xs {
		if strings.HasPrefix(x, p) {
			return true
		}
	}
	return false
}

// renewFrom never counts a renewal from later than now, and moves it back
// by the time since a leader last reached this node (D62).
func TestRenewFrom(t *testing.T) {
	now := controller.Reading{Mono: time.Hour}
	if got := renewFrom(now, 0); got != now {
		t.Fatalf("no contact age: %v", got)
	}
	if got := renewFrom(now, 3*time.Second); got.Mono != time.Hour-3*time.Second {
		t.Fatalf("3s: %v", got)
	}
	if got := renewFrom(now, 1<<62); got.Mono <= 0 || got.Mono > now.Mono {
		t.Fatalf("never contacted: %v", got)
	}
}
