package cluster

import (
	"time"

	"coyotedb.com/coyote/internal/cluster/transport"
)

// Timing holds every timeout of the cluster layer in one place.
type Timing struct {
	// Raft. HeartbeatTimeout: a follower with no leader contact for this
	// long starts an election. ElectionTimeout: a candidate with no result
	// for this long retries. LeaderLeaseTimeout: a leader that cannot reach
	// a quorum for this long steps down; it must stay below the primary
	// lease (Settings.LeaseDuration, safety rule 4). CommitTimeout: max delay
	// before an idle leader sends a heartbeat carrying the commit index.
	HeartbeatTimeout   time.Duration
	ElectionTimeout    time.Duration
	LeaderLeaseTimeout time.Duration
	CommitTimeout      time.Duration

	// Raft log housekeeping.
	SnapshotInterval  time.Duration
	SnapshotThreshold uint64
	TrailingLogs      uint64

	// Network. DialTimeout is per address attempt (TCP + TLS + header).
	// HandshakeTimeout bounds an inbound TLS handshake + header. RaftIO is
	// the I/O deadline Raft puts on its RPCs. RPCTimeout bounds one cluster
	// RPC (join, forwarded command, status).
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration
	RaftIO           time.Duration
	RPCTimeout       time.Duration
	KeepAliveIdle    time.Duration
	KeepAliveIntvl   time.Duration
	KeepAliveCount   int

	// ApplyTimeout bounds how long a proposal waits to be enqueued.
	ApplyTimeout time.Duration
	// ReconcileInterval is how often the leader re-checks the Raft
	// configuration against membership (also run on every change).
	ReconcileInterval time.Duration
	// PromoteMaxLag: a non-voter is promoted only when its applied index is
	// within this many entries of the leader's.
	PromoteMaxLag uint64

	// Join path. JoinAttempts rounds over all seeds, starting JoinBackoff
	// apart and doubling (max 5s). JoinRequestTimeout bounds how long an
	// un-authenticated client may take, after its TLS handshake, to send
	// its join request. MaxJoinStreams caps concurrent un-authenticated
	// join streams per node (member streams are not limited).
	JoinAttempts       int
	JoinBackoff        time.Duration
	JoinRequestTimeout time.Duration
	MaxJoinStreams     int
}

// DefaultTiming is tuned for LAN and VPN links (Tailscale): round trips of
// tens of milliseconds with occasional hiccups of a few hundred. A leader
// loss is noticed after ~1s and an election takes 1-2s. The 500ms leader
// lease is well below the 6s default primary lease.
func DefaultTiming() Timing {
	return Timing{
		HeartbeatTimeout:   1000 * time.Millisecond,
		ElectionTimeout:    1000 * time.Millisecond,
		LeaderLeaseTimeout: 500 * time.Millisecond,
		CommitTimeout:      50 * time.Millisecond,
		SnapshotInterval:   2 * time.Minute,
		SnapshotThreshold:  8192,
		TrailingLogs:       10240,
		DialTimeout:        5 * time.Second,
		HandshakeTimeout:   5 * time.Second,
		RaftIO:             10 * time.Second,
		RPCTimeout:         30 * time.Second,
		KeepAliveIdle:      15 * time.Second,
		KeepAliveIntvl:     5 * time.Second,
		KeepAliveCount:     3,
		ApplyTimeout:       10 * time.Second,
		ReconcileInterval:  5 * time.Second,
		PromoteMaxLag:      256,
		JoinAttempts:       6,
		JoinBackoff:        500 * time.Millisecond,
		JoinRequestTimeout: 5 * time.Second,
		MaxJoinStreams:     8,
	}
}

// FastTiming is for tests on loopback (still safe under -race).
func FastTiming() Timing {
	t := DefaultTiming()
	t.HeartbeatTimeout = 300 * time.Millisecond
	t.ElectionTimeout = 300 * time.Millisecond
	t.LeaderLeaseTimeout = 150 * time.Millisecond
	t.CommitTimeout = 10 * time.Millisecond
	t.DialTimeout = 2 * time.Second
	t.HandshakeTimeout = 3 * time.Second
	t.RaftIO = 3 * time.Second
	t.RPCTimeout = 10 * time.Second
	t.KeepAliveIdle = 2 * time.Second
	t.KeepAliveIntvl = time.Second
	t.ReconcileInterval = 200 * time.Millisecond
	t.SnapshotInterval = time.Hour // tests snapshot explicitly
	t.JoinAttempts = 4
	t.JoinBackoff = 50 * time.Millisecond
	t.JoinRequestTimeout = 2 * time.Second
	return t
}

func (t Timing) transport() transport.Timing {
	return transport.Timing{
		DialTimeout:      t.DialTimeout,
		HandshakeTimeout: t.HandshakeTimeout,
		KeepAliveIdle:    t.KeepAliveIdle,
		KeepAliveIntvl:   t.KeepAliveIntvl,
		KeepAliveCount:   t.KeepAliveCount,
	}
}

// MinLeaseRenewInterval is the shortest primary lease renew interval the
// settings accept.
const MinLeaseRenewInterval = 200 * time.Millisecond

// CheckLeaseTiming checks the primary lease settings against this node's
// Raft timing (safety rule 4). Settings that break it are refused when
// they are changed, and a controller whose local Raft timing conflicts with
// the cluster's settings will not fail over (it says why in status):
//
//   - lease >= 3x renew interval (also checked by the state machine);
//   - lease > Raft LeaderLeaseTimeout + 1s: a leader cut off from its
//     quorum steps down well before a primary lease it granted runs out;
//   - 60% of the lease >= renew interval + HeartbeatTimeout +
//     ElectionTimeout: one ordinary leader election does not make a
//     healthy primary fence itself (it starts stopping at 60% of the
//     lease, and renewals are retried while one is unanswered).
func CheckLeaseTiming(s Settings, t Timing) error {
	if err := validSettings(s); err != nil {
		return err
	}
	if s.LeaseDuration <= t.LeaderLeaseTimeout+time.Second {
		return errorf(ErrInvalid, "the primary lease (%s) must be longer than the Raft leader lease (%s) plus 1s", s.LeaseDuration, t.LeaderLeaseTimeout)
	}
	// The main node starts stopping Postgres at 60% of the lease
	// (controller.FenceFastAt; this package cannot import it).
	if stop := s.LeaseDuration * 6 / 10; stop < t.HeartbeatTimeout+t.ElectionTimeout+s.LeaseRenewInterval {
		return errorf(ErrInvalid, "60%% of the primary lease (%s) must cover a renew interval plus a Raft leader election (%s)",
			stop, t.HeartbeatTimeout+t.ElectionTimeout+s.LeaseRenewInterval)
	}
	return nil
}
