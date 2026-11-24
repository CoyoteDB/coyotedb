package controller

import (
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// NodeState is what one data node's agent says about itself (the
// controller's slice of the agent's status report).
type NodeState struct {
	Phase            string `json:"phase"`
	PGRunning        bool   `json:"pg_running"`
	Role             string `json:"role,omitempty"` // primary or standby (from Postgres)
	Streaming        bool   `json:"streaming"`
	StreamCut        bool   `json:"stream_cut"`
	CutHold          bool   `json:"cut_hold"` // the cut was made by a switch/failover and resumes by itself
	CutEpoch         uint64 `json:"cut_epoch,omitempty"`
	Marker           string `json:"marker,omitempty"`
	Epoch            uint64 `json:"epoch"`
	LastPrimaryEpoch uint64 `json:"last_primary_epoch"`
	// PagesDamaged: the node's data has pages that failed their checksum
	// or the page check (D55), in plain words; "" when none are known.
	PagesDamaged string `json:"pages_damaged,omitempty"`
	ReceiveLSN   *pg.LSN
	ReplayLSN    *pg.LSN
	// Primary only.
	FlushLSN  *pg.LSN
	SyncNames []string        `json:"sync_names,omitempty"` // S as this primary has it
	SyncCount int             `json:"sync_count,omitempty"`
	Standbys  []pg.StandbyRow `json:"standbys,omitempty"`
	Slots     []pg.Slot       `json:"slots,omitempty"`
}

// IsHealthyStandby: running read-only, no rejoin marker.
func (n *NodeState) IsHealthyStandby() bool {
	return n != nil && n.PGRunning && n.Role == "standby" && n.Marker == ""
}

// Damaged reports whether the node's data is known to be damaged (D55).
func (n *NodeState) Damaged() bool { return n != nil && n.PagesDamaged != "" }

// MemberObs is what the controller knows about one data member.
type MemberObs struct {
	ID     string
	Name   string
	Voter  bool
	Active bool // cluster status "active" (not "needs re-clone")
	// Removing: a remove or leave of it has started (D59); it is never
	// added to the sync set.
	Removing bool
	InR      bool
	// Report is the member's own answer this round (nil: unreachable).
	Report *NodeState
	// From the primary's pg_stat_replication this round.
	Streaming bool    // streaming from the primary
	Listed    bool    // in the primary's synchronous_standby_names
	FlushLSN  *pg.LSN // flushed on the standby, as the primary sees it
	LagBytes  int64   // primary flush - standby flush (-1: unknown)
	SlotLost  bool    // its slot on the primary lost WAL (rule 8)
	// Memory of this controller (monotonic; reset when it is elected).
	NotStreamingFor time.Duration // continuously not streaming from the primary
}

// Input is one observation of the cluster by the controller.
type Input struct {
	State cluster.State
	// LeaderFor is how long this controller has been the leader.
	LeaderFor time.Duration
	// LeaseAge is how long since the controller saw the primary's lease
	// (epoch, sequence) change, counted at most from its own election.
	LeaseAge time.Duration
	// Margin is added to the lease before the primary counts as gone.
	Margin time.Duration
	// TimingProblem: the cluster's lease settings conflict with this
	// node's Raft timing (cluster.CheckLeaseTiming); no failover.
	TimingProblem string
	// Primary is the primary's own report (nil: its agent did not answer).
	Primary *NodeState
	// Members: every data member other than the primary, by id.
	Members map[string]MemberObs
	// NoSyncFor: how long fewer than k members of S have been streaming
	// (0 while enough are).
	NoSyncFor time.Duration
}

// Settings knobs derived from the cluster settings.
const (
	// MaxAddLag: a standby joins the sync set only when it is this close
	// to the primary (so adding it does not stall writes).
	MaxAddLag = 16 << 20
	// minSyncDropAfter is the shortest time a lost member of R is kept
	// before it is taken out (rule 3, removal).
	minSyncDropAfter = 5 * time.Second
)

// SyncDropAfter is how long a member of R may be not streaming before it
// is taken out of R (if enough others are streaming).
func SyncDropAfter(s cluster.Settings) time.Duration { return max(s.LeaseDuration, minSyncDropAfter) }
