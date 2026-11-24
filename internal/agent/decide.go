package agent

// The role decision of a data node, as a pure function so the table is
// unit-tested (decide_test.go). The agent observes cluster state and its
// local Postgres, asks decide what to do, does it, and asks again.
//
// Safety rules this encodes:
//   - Rule 6: a node with a rejoin marker never starts Postgres as a
//     primary; a node whose data is primary data starts it as primary only
//     when a linearizable read of the cluster (Confirmed) names it primary
//     at the very epoch it last became primary (LastPrimaryEpoch). Otherwise
//     the marker is written before anything starts, and the node rewinds
//     (or re-clones) from the current primary through the tunnel.
//   - Without a quorum there is no linearizable read, so an isolated
//     former primary never starts Postgres at all. The single-voter cases
//     (one data node; two data nodes where this one holds the vote) are
//     their own quorum and confirm themselves.
//   - Starting as a standby is always safe (read-only), so it needs no
//     confirmation.

type actKind int

const (
	actNone         actKind = iota // steady; nothing to change
	actWait                        // cannot act now; reason says why
	actConfirm                     // need a linearizable read of the cluster first
	actRemoved                     // this node is no longer a member
	actInitPrimary                 // first start of a new cluster: initdb, run as primary
	actStartPrimary                // start existing primary data as primary
	actStartStandby                // start standby data as a standby
	actClone                       // copy the data from the primary (first time or retry)
	actRejoin                      // marker, stop, rewind-or-reclone from the primary, start as standby
	actPromote                     // the cluster names this standby primary: cut, promote
	actRepoint                     // resume a cut stream towards the current primary
	actFence                       // write the demoted marker (and stop Postgres); then wait
	actClearFence                  // fenced at this very epoch and still named primary (confirmed): clear the marker
	actReclone                     // rule 8: marked "needs re-clone": copy the data again from the primary
	actMarkActive                  // re-clone done: tell the cluster this node is a full member again
	actResume                      // a promotion of this node did not finish: start its data as a standby again (D56)
)

func (k actKind) String() string {
	return [...]string{"none", "wait", "confirm", "removed", "init-primary", "start-primary",
		"start-standby", "clone", "rejoin", "promote", "repoint", "fence", "clear-fence", "reclone", "mark-active", "resume-standby"}[k]
}

type action struct {
	kind   actKind
	reason string // for actWait/actFence: plain words for the person
}

// observed is everything decide looks at.
type observed struct {
	Self string
	// Cluster state (local copy, or confirmed by a linearizable read).
	Confirmed    bool
	Bootstrapped bool
	Member       bool
	SeenMember   bool   // this run saw this node in the (monotonic) local state
	Primary      string // node id, "" = none
	PrimaryName  string
	Epoch        uint64
	ForcedSelf   bool // a forced recovery made this node primary at Epoch
	HasSecrets   bool // the cluster's Postgres passwords are known here

	NeedsReclone bool // this member's cluster status is "needs re-clone" (rule 8)

	// Local.
	Fencing           bool   // the lease loop is stopping Postgres right now
	MarkerFencedEpoch uint64 // the marker was written by a lease fence at this epoch
	Recloned          bool   // a re-clone for "needs re-clone" finished
	Initialized       bool   // pgdata holds a database
	StandbyDir        bool   // standby.signal present
	Marker            string // rejoin marker reason, "" = none
	PGRunning         bool
	PGInRecovery      bool // valid when PGRunning
	StreamCut         bool // replication.json has no upstream
	LastPrimaryEpoch  uint64
	PromotingEpoch    uint64 // a promotion at this epoch has not finished (D56)
	FreshPrimary      bool
	CutEpoch          uint64
	CutExpired        bool // the cut's hold time passed
}

func wait(reason string) action { return action{kind: actWait, reason: reason} }

func decide(o observed) action {
	if !o.Bootstrapped {
		return wait("Waiting to reach the other nodes…")
	}
	if !o.Member {
		// The local state is replayed from the log after a restart, so
		// "not a member" can just mean "not caught up yet". It means
		// removed only once confirmed, or after this run saw the node as
		// a member (the state only moves forward).
		if o.Confirmed || o.SeenMember {
			return action{kind: actRemoved}
		}
		return action{kind: actConfirm}
	}
	self := o.Primary == o.Self && o.Self != ""
	if o.Fencing {
		return wait("This node lost its main-node lease and is stopping its database…")
	}
	if o.NeedsReclone && !self {
		switch {
		case o.Recloned:
			return action{kind: actMarkActive}
		case o.Primary == "":
			return wait("This node was away too long and must copy the data again; waiting for a main node to copy from…")
		case !o.HasSecrets:
			return wait("Waiting for the cluster's database passwords…")
		}
		return action{kind: actReclone}
	}

	if o.Marker != "" {
		switch {
		case self && o.MarkerFencedEpoch != 0 && o.MarkerFencedEpoch == o.Epoch && o.LastPrimaryEpoch == o.Epoch && !o.PGRunning:
			// Rule 4 then rule 6: the node stopped itself when its lease ran
			// out. If the cluster still names it primary at that very
			// epoch, nobody else was promoted, so its data is the latest.
			if !o.Confirmed {
				return action{kind: actConfirm}
			}
			return action{kind: actClearFence}
		case self:
			return wait("This node was a main node before and must rejoin as a copy first, but the cluster still names it as the main node. " +
				"Its database stays stopped. Make another node the main one (coyote dev switch --to <node>).")
		case o.Primary == "":
			return wait("Waiting for the cluster to name a main node before rejoining as a copy…")
		case o.Marker == "cloning" && !o.Initialized:
			return action{kind: actClone}
		}
		return action{kind: actRejoin}
	}

	if !o.Initialized {
		switch {
		case self && o.FreshPrimary:
			if !o.Confirmed {
				return action{kind: actConfirm}
			}
			return action{kind: actInitPrimary}
		case self:
			return wait("The cluster names this node as the main node, but its database folder is missing. " +
				"The database will not be created again here; make another node the main one.")
		case o.Primary == "":
			return wait("Waiting for the cluster to name a main node to copy from…")
		case !o.HasSecrets:
			return wait("Waiting for the cluster's database passwords…")
		}
		return action{kind: actClone}
	}

	if o.PGRunning {
		if o.PGInRecovery {
			switch {
			case self:
				if !o.Confirmed {
					return action{kind: actConfirm}
				}
				return action{kind: actPromote}
			case o.Primary == "":
				return action{kind: actNone}
			case o.StreamCut && (o.CutEpoch == 0 || o.Epoch > o.CutEpoch || o.CutExpired):
				return action{kind: actRepoint}
			}
			return action{kind: actNone}
		}
		// Running as a primary.
		switch {
		case self:
			return action{kind: actNone}
		case o.Epoch < o.LastPrimaryEpoch:
			// Our local copy of the state is behind what we already
			// confirmed; wait for it to catch up.
			return action{kind: actNone}
		case o.Primary == "":
			return action{kind: actFence, reason: "The cluster no longer names this node as the main node; its database was stopped and will rejoin as a copy once a main node is chosen."}
		}
		return action{kind: actRejoin}
	}

	if o.StandbyDir {
		return action{kind: actStartStandby}
	}
	// Primary data, not running: only a confirmed view may start it.
	if !o.Confirmed {
		return action{kind: actConfirm}
	}
	switch {
	case self && o.PromotingEpoch != 0 && o.PromotingEpoch == o.Epoch && o.LastPrimaryEpoch == o.Epoch:
		// D56: this node's own promotion in this term was cut short after
		// Postgres removed standby.signal. Started as it is, crash
		// recovery would serve on the old main node's timeline. Start it
		// as a standby (always safe) and promote it again.
		return action{kind: actResume}
	case self && (o.LastPrimaryEpoch == o.Epoch || o.ForcedSelf):
		return action{kind: actStartPrimary}
	case self:
		return action{kind: actFence, reason: "The cluster names this node as the main node for a newer term than its data was main in; " +
			"its data may be missing writes, so it will not start as the main node. Make another node the main one (coyote dev switch --to <node>)."}
	case o.Primary == "":
		return wait("No node is the main node right now; this node's database stays stopped.")
	}
	return action{kind: actRejoin}
}
