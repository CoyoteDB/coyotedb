// Package cluster runs a node's membership in a CoyoteDB cluster: Raft
// (hashicorp/raft with a bbolt log store and file snapshots), the replicated
// cluster state (FSM), the one-port mTLS transport, joins and invites.
//
// Lifecycle:
//
//	Bootstrap(ctx, cfg)           first node: new cluster id, CA, single voter
//	Join(ctx, cfg, inviteCode)    new node joins through any member
//	Start(ctx, cfg)               restart from what is on disk
//	ForceNewCluster(ctx, cfg, c)  disaster recovery on a lone survivor
//	(*Node).Shutdown()
//
// Reads: State (a copy), Subscribe (change signals), LinearizableState /
// VerifyLeader / Barrier. Writes: Apply with a typed Command (forwarded to
// the leader when needed).
package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"

	"coyotedb.com/coyote/internal/cluster/transport"
	"coyotedb.com/coyote/internal/pki"
	"coyotedb.com/coyote/internal/tunnel"
)

// Config describes the local node.
type Config struct {
	// Dir is the node's cluster state directory (created 0700).
	Dir string
	// ListenAddr is where the mTLS port listens, e.g. "0.0.0.0:7481".
	ListenAddr string
	// AdvertiseAddrs are the host:port addresses other nodes use to reach
	// this node, in order of preference (e.g. Tailscale first, then LAN).
	// Empty means the listener's address, which must then be specific.
	AdvertiseAddrs []string
	// Name is a human name, unique in the cluster.
	Name string
	// Role is data or witness.
	Role Role
	// Platform of this node (checked against data nodes on join, rule 1).
	Platform Platform
	// PGAddr returns the local Postgres address (127.0.0.1:port) for the pg
	// stream, or "" when Postgres is not running. Nil refuses pg streams.
	PGAddr func() string
	// Timing; zero value means DefaultTiming().
	Timing Timing
	Logger *slog.Logger
	// Now is the wall clock used to stamp commands (tests). Nil: time.Now.
	Now func() time.Time
	// RaftLogLevel is the hclog level for Raft's own logs ("" = "warn").
	RaftLogLevel string
	// Listener, if set, is the already bound cluster port (see Listen);
	// ListenAddr is then only informational. The node owns and closes it.
	Listener net.Listener
	// AdvertiseExplicit: AdvertiseAddrs came from the operator (a flag),
	// so they replace addresses pinned with `node set-address`.
	AdvertiseExplicit bool
	// ClientPort is announced in this node's member record (data nodes'
	// client proxy port; 0 = none).
	ClientPort int
	// DialRedirect (chaos tests only) rewrites every address this node
	// dials; see transport.Timing.Redirect. Nil in normal use.
	DialRedirect func(addr string) string
	// AllowMixedOS (joining data nodes): ask the cluster to accept this
	// machine although it is a different kind than the data nodes
	// (`join --allow-mixed-os`, D37). It is still refused unless the
	// fingerprints, probes and guards pass (checkPlatform).
	AllowMixedOS bool
	// MixedJoinCheck, if set, runs on the leader before it admits a data
	// node that is not alike to every data node (a mixed join): the agent
	// uses it to have a data node run the portability audit now and record
	// it (docs/MIXED_OS.md: "the join check runs it too"). An error refuses
	// the join with ErrMixedNotReady.
	MixedJoinCheck func(ctx context.Context, joiner Member) error
	// ExplainPlatform, if set, turns a fingerprint difference into plain
	// sentences for the refusal message (joiner first).
	ExplainPlatform func(joiner, member Platform) []string
	// MixedFieldsOK, if set, runs on the leader before it admits a joiner:
	// false means some member runs a version that does not know the
	// mixed-system fields (Platform.Fingerprint..., AllowMixedOS). The
	// leader then strips them from the AddMember entry, because an older
	// node's strict decoder would halt on them (rolling upgrade). Nil:
	// always strip (library users that do not check versions).
	MixedFieldsOK func(ctx context.Context) bool
	// Version is this program's version (sent with a join; named in the
	// refusal of a joiner that is too old).
	Version string
	// PGBuilds is this program's pinned PostgreSQL builds (D70, key:
	// sha256; pg.KnownBuilds). A joining data node sends the entry for its
	// own build. Nil: none sent.
	PGBuilds map[string]PGBuild

	// wrapStable (tests only) wraps the Raft stable store, under
	// guardedStable, to fail its writes like a broken disk.
	wrapStable func(raft.StableStore) raft.StableStore
}

// transportTiming is the transport's timing for this config.
func (c *Config) transportTiming() transport.Timing {
	t := c.Timing.transport()
	t.Redirect = c.DialRedirect
	return t
}

// Node is a running cluster member.
type Node struct {
	cfg    Config
	timing Timing
	log    *slog.Logger
	id     identity
	tls    *pki.NodeTLS

	// addrs are the addresses this node announces; explicit: they came
	// from the operator (Config.AdvertiseExplicit) and have not been
	// recorded yet, so they still replace pinned ones (once, D62).
	addrMu   sync.Mutex
	addrs    []string
	explicit bool
	// ownAddrIdx: Raft index of this node's last update of its own
	// record (maintainLoop only).
	ownAddrIdx uint64

	fsm    *FSM
	raft   *raft.Raft
	tr     *transport.Transport
	rtrans *raft.NetworkTransport
	store  *raftboltdb.BoltStore

	cacheMu sync.Mutex
	cache   peerCache

	memberMu sync.Mutex // serialises membership changes on the leader
	// caughtUpTerm is the Raft term in which reconcile last confirmed (with
	// a barrier) that the local state includes every committed entry.
	// Guarded by memberMu.
	caughtUpTerm uint64
	paused       atomic.Bool

	// dropJoinResponses (tests): drop this many join responses after the
	// join is processed, as if the network lost them.
	dropJoinResponses atomic.Int32
	// dropApplyResponses (tests): drop this many replies to a node's own
	// removal after it is processed (as when the connection is cut, or
	// this node stops, between the commit and the reply).
	dropApplyResponses atomic.Int32
	kick               chan struct{}
	obsCh              chan raft.Observation
	observer           *raft.Observer

	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	shutOnce sync.Once
	shutErr  error

	// failErr: this node's disk refused a write Raft cannot go on
	// without (guardedStable); Raft's main loop has ended. Guarded by
	// failMu.
	failMu  sync.Mutex
	failErr error
}

func (c *Config) normalize() error {
	if c.Dir == "" {
		return errors.New("cluster: Dir is required")
	}
	if c.ListenAddr == "" {
		return errors.New("cluster: ListenAddr is required")
	}
	if c.Timing == (Timing{}) {
		c.Timing = DefaultTiming()
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return nil
}

func (c *Config) validateNew() error {
	if c.Name == "" || len(c.Name) > 64 {
		return errors.New("cluster: Name must be 1..64 characters")
	}
	if c.Role != RoleData && c.Role != RoleWitness {
		return fmt.Errorf("cluster: Role must be %q or %q", RoleData, RoleWitness)
	}
	if c.Role == RoleData && (c.Platform.OS == "" || c.Platform.Arch == "" || c.Platform.PGBuildSHA == "") {
		return errors.New("cluster: data nodes need Platform.OS, Arch and PGBuildSHA")
	}
	return nil
}

func (n *Node) now() time.Time { return n.cfg.Now().UTC() }

// Listen binds the cluster (mTLS) port with the keepalives the transport
// expects. A caller that wants every local port secured before it contacts
// the cluster binds it with this and passes it as Config.Listener. A zero
// timing means DefaultTiming().
func Listen(t Timing, addr string) (net.Listener, error) {
	if t == (Timing{}) {
		t = DefaultTiming()
	}
	lc := net.ListenConfig{KeepAliveConfig: net.KeepAliveConfig{
		Enable: true, Idle: t.KeepAliveIdle, Interval: t.KeepAliveIntvl, Count: t.KeepAliveCount,
	}}
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return ln, nil
}

// listen binds the mTLS port before the node has its certificate (a joiner
// must announce its address in the join request).
func listen(cfg *Config) (net.Listener, []string, error) {
	ln := cfg.Listener
	cfg.Listener = nil // used once
	if ln == nil {
		var err error
		if ln, err = Listen(cfg.Timing, cfg.ListenAddr); err != nil {
			return nil, nil, err
		}
	}
	addrs := slices.Clone(cfg.AdvertiseAddrs)
	if len(addrs) == 0 {
		a := ln.Addr().(*net.TCPAddr)
		if a.IP.IsUnspecified() {
			ln.Close()
			return nil, nil, fmt.Errorf("cluster: listening on %s; set AdvertiseAddrs to the addresses other nodes should use", ln.Addr())
		}
		addrs = []string{a.String()}
	}
	return ln, addrs, nil
}

type openOpts struct {
	bootstrap *raft.Configuration // non-nil only for Bootstrap
	// pauseReconcile keeps the leader from changing the Raft configuration
	// until resumeReconcile (ForceNewCluster, before ForceRecovered lands).
	pauseReconcile bool
}

// open wires up stores, transport and Raft for an identity already on disk.
func open(cfg Config, ln net.Listener, addrs []string, o openOpts) (*Node, error) {
	id, err := loadIdentity(cfg.Dir)
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("load identity: %w", err)
	}
	ntls, err := loadNodeTLS(cfg.Dir)
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("load node certificate: %w", err)
	}
	cache, err := loadPeers(cfg.Dir)
	if err != nil {
		ln.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &Node{
		cfg: cfg, timing: cfg.Timing, id: id, tls: ntls, addrs: addrs, explicit: cfg.AdvertiseExplicit, cache: cache,
		log:  cfg.Logger.With("node", id.Name),
		kick: make(chan struct{}, 1), ctx: ctx, cancel: cancel,
	}
	ok := false
	defer func() {
		if !ok {
			n.closeResources()
		}
	}()
	n.fsm = NewFSM(n.onHalt)
	n.paused.Store(o.pauseReconcile)

	if err := ensureDirs(cfg.Dir); err != nil {
		ln.Close()
		return nil, err
	}
	n.store, err = raftboltdb.New(raftboltdb.Options{
		Path:        filepath.Join(cfg.Dir, dirRaft, fileRaftDB),
		BoltOptions: &bbolt.Options{Timeout: 2 * time.Second},
	})
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("open raft store (is another agent running on this data dir?): %w", err)
	}
	hlog := n.hclogger()
	snaps, err := raft.NewFileSnapshotStoreWithLogger(filepath.Join(cfg.Dir, dirRaft), 2, hlog)
	if err != nil {
		ln.Close()
		return nil, err
	}

	n.tr, err = transport.Listen(transport.Config{
		Listener:  ln,
		TLS:       ntls,
		Authorize: n.authorize,
		Resolve:   n.resolve,
		Timing:    cfg.transportTiming(),
		Logger:    n.log,
		// Un-authenticated join streams are capped; members are not.
		MaxAnonymous: cfg.Timing.MaxJoinStreams,
	})
	if err != nil {
		ln.Close()
		return nil, err
	}
	n.rtrans = raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream:  n.tr.RaftLayer(),
		MaxPool: 3,
		Timeout: cfg.Timing.RaftIO,
		Logger:  hlog,
	})

	rc := n.raftConfig(hlog)
	if o.bootstrap != nil {
		if err := raft.BootstrapCluster(rc, n.store, n.store, snaps, n.rtrans, *o.bootstrap); err != nil {
			return nil, fmt.Errorf("raft bootstrap: %w", err)
		}
	}
	n.raft, err = raft.NewRaft(rc, n.fsm, withStallFault(n.store, cfg.Dir), newGuardedStable(n.stableStore(), n.storageFailed), snaps, n.rtrans)
	if err != nil {
		return nil, fmt.Errorf("start raft: %w", err)
	}
	n.obsCh = make(chan raft.Observation, 16)
	n.observer = raft.NewObserver(n.obsCh, false, func(o *raft.Observation) bool {
		_, ok := o.Data.(raft.LeaderObservation)
		return ok
	})
	n.raft.RegisterObserver(n.observer)

	// Handlers that use n.raft are registered only now; until then those
	// stream types are refused (callers retry).
	n.tr.Handle(transport.StreamRPC, n.serveRPC)
	n.tr.Handle(transport.StreamJoin, n.serveJoin)
	if cfg.PGAddr != nil {
		n.tr.Handle(transport.StreamPG, tunnel.Backend(cfg.PGAddr, cfg.Timing.DialTimeout))
	} else {
		n.tr.Handle(transport.StreamPG, func(s *transport.Stream) { s.Reject("this node does not run Postgres") })
	}

	n.wg.Add(2)
	go n.watchLoop()
	go n.maintainLoop()
	ok = true
	n.log.Info("cluster node started", "id", id.NodeID, "cluster", id.ClusterID, "listen", n.tr.Addr(), "advertise", addrs)
	return n, nil
}

func (n *Node) stableStore() raft.StableStore {
	if n.cfg.wrapStable != nil {
		return n.cfg.wrapStable(n.store)
	}
	return n.store
}

func (n *Node) raftConfig(hlog hclog.Logger) *raft.Config {
	t := n.timing
	rc := raft.DefaultConfig()
	rc.LocalID = raft.ServerID(n.id.NodeID)
	rc.HeartbeatTimeout = t.HeartbeatTimeout
	rc.ElectionTimeout = t.ElectionTimeout
	rc.LeaderLeaseTimeout = t.LeaderLeaseTimeout
	rc.CommitTimeout = t.CommitTimeout
	rc.SnapshotInterval = t.SnapshotInterval
	rc.SnapshotThreshold = t.SnapshotThreshold
	rc.TrailingLogs = t.TrailingLogs
	rc.Logger = hlog
	rc.ShutdownOnRemove = false
	return rc
}

func (n *Node) hclogger() hclog.Logger {
	lvl := n.cfg.RaftLogLevel
	if lvl == "" {
		lvl = "warn"
	}
	return hclog.FromStandardLogger(
		slog.NewLogLogger(n.log.With("component", "raft").Handler(), slog.LevelInfo),
		&hclog.LoggerOptions{Name: "raft", Level: hclog.LevelFromString(lvl)},
	)
}

func (n *Node) onHalt(err error) {
	n.log.Error("cluster state machine halted: this node cannot apply a command from the cluster; stopping Raft on this node. Upgrade it and restart.", "err", err)
	// Shutdown waits for the FSM goroutine that is calling us: do it async.
	go n.raft.Shutdown()
}

// ---- identity, authorization, addresses ----

// ID returns this node's id.
func (n *Node) ID() string { return n.id.NodeID }

// ClusterID returns the cluster id.
func (n *Node) ClusterID() string { return n.id.ClusterID }

// Name returns this node's name.
func (n *Node) Name() string { return n.id.Name }

// Addr is the bound listen address.
func (n *Node) Addr() net.Addr { return n.tr.Addr() }

// AdvertiseAddrs are the addresses this node announces.
func (n *Node) AdvertiseAddrs() []string {
	n.addrMu.Lock()
	defer n.addrMu.Unlock()
	return slices.Clone(n.addrs)
}

func (n *Node) ownAddrs() ([]string, bool) {
	n.addrMu.Lock()
	defer n.addrMu.Unlock()
	return slices.Clone(n.addrs), n.explicit
}

// explicitRecorded: the operator's addresses are in the cluster's record;
// from now on a later `node set-address` is kept (D62).
func (n *Node) explicitRecorded(addrs []string) {
	n.addrMu.Lock()
	if slices.Equal(n.addrs, addrs) {
		n.explicit = false
	}
	n.addrMu.Unlock()
}

// DropDeadConns closes cluster connections (Raft, tunnels, RPC) whose
// local IP address this computer no longer has, so they are redialed at
// once instead of hanging until TCP gives up (D62). Loopback addresses
// always count as present. It returns how many were closed.
func (n *Node) DropDeadConns() int {
	ifAddrs, err := net.InterfaceAddrs()
	if err != nil || len(ifAddrs) == 0 {
		return 0 // cannot tell: keep everything
	}
	have := make(map[netip.Addr]bool, len(ifAddrs))
	for _, a := range ifAddrs {
		if p, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(p.IP); ok {
				have[ip.Unmap()] = true
			}
		}
	}
	return n.tr.DropLocalGone(func(ip netip.Addr) bool {
		return ip.IsLoopback() || have[ip.WithZone("")]
	})
}

// ExplicitPending reports whether addresses given explicitly
// (Config.AdvertiseExplicit) are still waiting to be recorded.
func (n *Node) ExplicitPending() bool {
	n.addrMu.Lock()
	defer n.addrMu.Unlock()
	return n.explicit
}

// OwnRecord is this node's member record as this node knows it (false:
// not in its copy of the state yet).
func (n *Node) OwnRecord() (Member, bool) {
	m, ok, _ := n.fsm.member(n.id.NodeID)
	return m, ok
}

// authorize: a node id is allowed if it is a member in the FSM, or, while
// the FSM is behind the peer cache (fresh join, restart before replay), in
// the cache.
func (n *Node) authorize(nodeID string) bool {
	_, ok, idx := n.fsm.member(nodeID)
	n.cacheMu.Lock()
	cIdx := n.cache.Index
	_, inCache := n.cache.Members[nodeID]
	n.cacheMu.Unlock()
	if idx > 0 && idx >= cIdx {
		return ok
	}
	return inCache
}

// resolve lists where to dial a member: addresses given here with
// `node set-address --local` first, then the cluster's record (or the
// peer cache while the state is empty). The TLS handshake checks the
// member's certificate whichever address answers.
func (n *Node) resolve(nodeID string) []string {
	n.cacheMu.Lock()
	out := slices.Clone(n.cache.Local[nodeID])
	cached := slices.Clone(n.cache.Members[nodeID])
	n.cacheMu.Unlock()
	rec := cached
	if m, ok, _ := n.fsm.member(nodeID); ok && len(m.Addresses) > 0 {
		rec = m.Addresses
	}
	for _, a := range rec {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// ---- reads ----

// State returns a copy of the local state (may lag the leader slightly).
func (n *Node) State() State { return n.fsm.State() }

// Subscribe returns a subscription signalled on every state change and on
// every leader change. Read State (and IsLeader/LeaderID) after a signal.
func (n *Node) Subscribe() *Subscription { return n.fsm.Subscribe() }

// Timing is the node's cluster-layer timing.
func (n *Node) Timing() Timing { return n.timing }

// QuorumContact is a conservative moment at which this node's copy of the
// cluster state was last backed by a leader holding a quorum (zero: never
// or unknown). A leader keeps its role for up to about two
// LeaderLeaseTimeouts without reaching a quorum, and a follower may hear
// from a leader that has just lost its quorum, so both subtract that. A
// candidate keeps the moment it last heard from a leader: becoming a
// candidate (about a second without heartbeats) does not make what it knew
// then any less true, and no other main node can take over before the
// lease, measured from that moment, has run out (rule 4).
func (n *Node) QuorumContact() time.Time {
	slack := 2 * n.timing.LeaderLeaseTimeout
	switch n.raft.State() {
	case raft.Leader:
		return time.Now().Add(-slack)
	case raft.Follower, raft.Candidate:
		if t := n.raft.LastContact(); !t.IsZero() {
			return t.Add(-slack)
		}
	}
	return time.Time{}
}

// LeaderContactAge is how long ago a Raft leader last reached this node
// (0 on the leader itself; very long if never since start). The leader
// dials its followers, so a stale contact means the leader cannot reach
// this node, even if this node can still reach the leader (D62).
func (n *Node) LeaderContactAge() time.Duration {
	if n.raft.State() == raft.Leader {
		return 0
	}
	t := n.raft.LastContact()
	if t.IsZero() {
		return math.MaxInt64
	}
	return max(time.Since(t), 0)
}

// IsLeader reports whether this node currently believes it is the leader.
// For decisions, use VerifyLeader.
func (n *Node) IsLeader() bool { return n.raft.State() == raft.Leader }

// LeaderID returns the current leader's node id as known locally ("" if none).
func (n *Node) LeaderID() string {
	_, id := n.raft.LeaderWithID()
	return string(id)
}

// VerifyLeader confirms with a quorum that this node is still the leader.
func (n *Node) VerifyLeader(ctx context.Context) error {
	return n.waitFuture(ctx, n.raft.VerifyLeader())
}

// Barrier (leader only) waits until every earlier entry is applied locally.
func (n *Node) Barrier(ctx context.Context) error {
	timeout := n.timing.ApplyTimeout
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	return n.waitFuture(ctx, n.raft.Barrier(timeout))
}

// LinearizableState returns state that reflects every committed entry, after
// confirming leadership with a quorum. Leader only (ErrNotLeader otherwise).
func (n *Node) LinearizableState(ctx context.Context) (State, error) {
	if err := n.VerifyLeader(ctx); err != nil {
		return State{}, err
	}
	if err := n.Barrier(ctx); err != nil {
		return State{}, err
	}
	return n.fsm.State(), nil
}

func (n *Node) waitFuture(ctx context.Context, f raft.Future) error {
	done := make(chan error, 1)
	go func() { done <- f.Error() }()
	select {
	case err := <-done:
		if errors.Is(err, raft.ErrNotLeader) || errors.Is(err, raft.ErrLeadershipLost) {
			return fmt.Errorf("%w: %v", ErrNotLeader, err)
		}
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Err reports a fatal error, or nil: the state machine halted on an
// unknown command (ErrHalted), or this node's disk refused a write Raft
// needs (ErrStorage, wrapping the disk's error). Either way Raft on this
// node has stopped; the owner should stop its database and exit.
func (n *Node) Err() error {
	if err := n.fsm.Halted(); err != nil {
		return err
	}
	return n.storageErr()
}

// RaftConfiguration returns the current Raft servers and their suffrage.
func (n *Node) RaftConfiguration() ([]ConfigServer, error) {
	f := n.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return nil, err
	}
	var out []ConfigServer
	for _, s := range f.Configuration().Servers {
		su := Nonvoter
		if s.Suffrage == raft.Voter {
			su = Voter
		}
		out = append(out, ConfigServer{ID: string(s.ID), Suffrage: su})
	}
	return out, nil
}

// Snapshot asks Raft to take a snapshot now (compacting the log).
func (n *Node) Snapshot() error { return n.raft.Snapshot().Error() }

// ---- streams ----

// Dial opens a stream of type st to a member.
func (n *Node) Dial(ctx context.Context, nodeID string, st transport.StreamType) (net.Conn, error) {
	return n.tr.Dial(ctx, nodeID, st)
}

// Handle registers a handler for an extra stream type.
func (n *Node) Handle(st transport.StreamType, h transport.Handler) { n.tr.Handle(st, h) }

// PGDialer returns a tunnel.DialFunc opening pg streams through this node.
func (n *Node) PGDialer() tunnel.DialFunc {
	return func(ctx context.Context, nodeID string) (net.Conn, error) {
		return n.tr.Dial(ctx, nodeID, transport.StreamPG)
	}
}

// ---- background loops ----

// watchLoop reacts to state changes: refresh the peer cache, drop streams
// of removed members, nudge the leader's reconcile.
func (n *Node) watchLoop() {
	defer n.wg.Done()
	sub := n.fsm.Subscribe()
	defer sub.Close()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-sub.C:
		case o := <-n.obsCh:
			_ = o
			n.fsm.notify()
			n.kickReconcile()
			continue
		}
		st := n.fsm.State()
		if st.Bootstrapped() {
			n.updatePeerCache(st)
		}
		n.tr.DropUnauthorized()
		// Connections dialed to an address a member no longer has (it
		// moved, D62) may hang until a keepalive gives up: redial now.
		n.tr.DropStale()
		n.kickReconcile()
	}
}

func (n *Node) updatePeerCache(st State) {
	m := make(map[string][]string, len(st.Members))
	names := make(map[string]string, len(st.Members))
	for id, mem := range st.Members {
		m[id] = slices.Clone(mem.Addresses)
		names[id] = mem.Name
	}
	n.cacheMu.Lock()
	// A --local address is no longer needed once the member's record
	// holds it (it announced it, or an operator recorded it), nor for a
	// member that is gone.
	local := map[string][]string{}
	for id, addrs := range n.cache.Local {
		if rec, ok := m[id]; ok && !containsAll(rec, addrs) {
			local[id] = addrs
		}
	}
	if len(local) == 0 {
		local = nil
	}
	if st.AppliedIndex <= n.cache.Index || (n.cache.equalMembers(m) && maps.Equal(n.cache.Names, names) && len(local) == len(n.cache.Local)) {
		if st.AppliedIndex > n.cache.Index {
			n.cache.Index = st.AppliedIndex
		}
		n.cacheMu.Unlock()
		return
	}
	n.cache = peerCache{Format: peersCacheFmt, Index: st.AppliedIndex, Members: m, Names: names, Local: local}
	c := n.cache.clone()
	n.cacheMu.Unlock()
	if err := writeJSON(filepath.Join(n.cfg.Dir, filePeers), c); err != nil {
		n.log.Warn("write peer cache", "err", err)
	}
}

func (n *Node) kickReconcile() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

// maintainLoop runs on every node: keep our advertised addresses current,
// finish a pending bootstrap; on the leader also reconcile the Raft
// configuration and expire join secrets.
func (n *Node) maintainLoop() {
	defer n.wg.Done()
	t := time.NewTicker(n.timing.ReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-t.C:
		case <-n.kick:
		}
		if n.fsm.Halted() != nil {
			continue
		}
		n.finishBootstrap()
		n.updateOwnAddresses()
		if n.IsLeader() && !n.paused.Load() {
			n.reconcile()
			n.expireSecrets()
		}
	}
}

func (n *Node) updateOwnAddresses() {
	// Known addresses: the FSM's, or the peer cache's while the FSM is
	// still empty (right after a restart the leader may not be able to
	// reach us at all until we announce the new address).
	m, ok, idx := n.fsm.member(n.id.NodeID)
	if ok && idx < n.ownAddrIdx {
		// This node's copy of the state does not hold its own last
		// address update yet: deciding on it would undo that update.
		return
	}
	known := m.Addresses
	if !ok {
		n.cacheMu.Lock()
		known, ok = n.cache.Members[n.id.NodeID]
		known = slices.Clone(known)
		n.cacheMu.Unlock()
	}
	if !ok {
		return
	}
	addrs, explicit := n.ownAddrs()
	if explicit && ok && slices.Equal(known, addrs) && n.fsmHasMember() {
		n.explicitRecorded(addrs)
		explicit = false
	}
	if !explicit && m.AddressesPinned && len(m.Addresses) > 0 && !slices.Equal(m.Addresses, addrs) {
		// An operator pinned this node's addresses (`node set-address`,
		// possibly run on another node): this node announces those.
		n.addrMu.Lock()
		if !n.explicit {
			n.addrs = slices.Clone(m.Addresses)
		}
		n.addrMu.Unlock()
		addrs = slices.Clone(m.Addresses)
	}
	upd := &UpdateMember{ID: n.id.NodeID, UnlessPinned: !explicit}
	// Addresses an operator pinned stay until the operator gives this node
	// other addresses explicitly (a flag value it has not recorded yet;
	// once recorded, a later `node set-address` wins again, D62).
	if !slices.Equal(known, addrs) && (!m.AddressesPinned || explicit) {
		upd.Addresses = addrs
	}
	if m.ClientPort != n.cfg.ClientPort && n.fsmHasMember() {
		p := n.cfg.ClientPort
		upd.ClientPort = &p
	}
	if upd.Addresses == nil && upd.ClientPort == nil {
		return
	}
	ctx, cancel := context.WithTimeout(n.ctx, n.timing.RPCTimeout)
	defer cancel()
	if res, err := n.Apply(ctx, upd); err != nil {
		n.log.Debug("update own member record", "err", err)
	} else {
		n.ownAddrIdx = res.Index
		n.log.Info("updated own member record", "addresses", upd.Addresses, "client_port", n.cfg.ClientPort)
		if explicit && upd.Addresses != nil {
			n.explicitRecorded(upd.Addresses)
		}
	}
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// PeerBook is what this node knows about another member without asking
// the cluster: its name, the addresses it was last recorded at, and any
// given here with `node set-address --local`.
type PeerBook struct {
	ID       string
	Name     string
	Recorded []string
	Local    []string
}

// PeerBook lists the other members from this node's peer cache (written
// on every state change, so it holds the last known addresses even while
// this node's copy of the state is empty after a restart).
func (n *Node) PeerBook() []PeerBook {
	n.cacheMu.Lock()
	defer n.cacheMu.Unlock()
	var out []PeerBook
	for id, addrs := range n.cache.Members {
		if id == n.id.NodeID {
			continue
		}
		out = append(out, PeerBook{ID: id, Name: n.cache.Names[id], Recorded: slices.Clone(addrs), Local: slices.Clone(n.cache.Local[id])})
	}
	slices.SortFunc(out, func(a, b PeerBook) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// FindPeer finds another member by name or id, in the local state or
// else in the peer cache (no leader needed).
func (n *Node) FindPeer(nameOrID string) (id, name string, ok bool) {
	st := n.fsm.State()
	for mid, m := range st.Members {
		if mid == nameOrID || strings.EqualFold(m.Name, nameOrID) {
			return mid, m.Name, true
		}
	}
	for _, p := range n.PeerBook() {
		if p.ID == nameOrID || (p.Name != "" && strings.EqualFold(p.Name, nameOrID)) {
			return p.ID, p.Name, true
		}
	}
	return "", "", false
}

// SetLocalAddrs records, on this machine only, where to reach member id
// (`node set-address --local`, D62): for when the cluster cannot record
// it because no leader can be elected (every computer got a new
// address). It is dialed before the recorded addresses and forgotten
// once the cluster's record holds it. The caller checks the addresses
// first (ProbePeer).
func (n *Node) SetLocalAddrs(id string, addrs []string) error {
	if id == n.id.NodeID {
		return errors.New("this is this node itself; its own address is detected or given with --advertise")
	}
	n.cacheMu.Lock()
	c := n.cache.clone()
	if c.Local == nil {
		c.Local = map[string][]string{}
	}
	c.Local[id] = slices.Clone(addrs)
	if err := writeJSON(filepath.Join(n.cfg.Dir, filePeers), c); err != nil {
		n.cacheMu.Unlock()
		return err
	}
	n.cache = c
	n.cacheMu.Unlock()
	n.log.Info("addresses given on this machine for a member", "member", id, "addresses", addrs)
	n.kickReconcile()
	return nil
}

func (n *Node) fsmHasMember() bool {
	_, ok, _ := n.fsm.member(n.id.NodeID)
	return ok
}

// ProbePeer checks that addr answers as member nodeID: a TCP connect and a
// full mutual-TLS handshake that must present nodeID's certificate. It
// returns the time the connect took (about one round trip).
func (n *Node) ProbePeer(ctx context.Context, nodeID, addr string) (time.Duration, error) {
	var d net.Dialer
	t0 := time.Now()
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, err
	}
	rtt := time.Since(t0)
	defer raw.Close()
	c := tls.Client(raw, n.tls.ClientConfig(nodeID))
	if err := c.HandshakeContext(ctx); err != nil {
		return rtt, fmt.Errorf("%s answered but is not %s of this cluster: %w", addr, nodeID, err)
	}
	return rtt, nil
}

func (n *Node) expireSecrets() {
	st := n.fsm.State()
	now := n.now()
	for _, s := range st.JoinSecrets {
		if !now.Before(s.ExpiresAt.Add(SecretRetention)) {
			ctx, cancel := context.WithTimeout(n.ctx, n.timing.RPCTimeout)
			_, _ = n.Apply(ctx, &ExpireJoinSecrets{At: now})
			cancel()
			return
		}
	}
}

// ---- shutdown ----

// Shutdown stops Raft, the transport and the stores. It is safe to call
// more than once. It does not leave the cluster (see Leave).
func (n *Node) Shutdown() error {
	n.shutOnce.Do(func() {
		n.cancel()
		n.wg.Wait()
		n.shutErr = n.closeResources()
		n.log.Info("cluster node stopped")
	})
	return n.shutErr
}

func (n *Node) closeResources() error {
	n.cancel()
	var errs []error
	if n.raft != nil {
		if n.observer != nil {
			n.raft.DeregisterObserver(n.observer)
		}
		if err := n.stopRaft(); err != nil {
			errs = append(errs, err)
			// Raft may still be using the store: leave it to the
			// process exit rather than wait on it.
			n.store = nil
		}
	}
	if n.rtrans != nil {
		n.rtrans.Close()
	}
	if n.tr != nil {
		n.tr.Close()
	}
	if n.store != nil {
		if err := n.store.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stopRaft shuts Raft down. After a storage failure the wait is bounded:
// a disk that refuses writes can leave a Raft goroutine stuck in it, and
// the agent must still stop and say why.
func (n *Node) stopRaft() error {
	f := n.raft.Shutdown()
	if n.storageErr() == nil {
		return f.Error()
	}
	done := make(chan error, 1)
	go func() { done <- f.Error() }()
	select {
	case err := <-done:
		return err
	case <-time.After(storageStopWait):
		return fmt.Errorf("raft did not stop within %s after the disk failure", storageStopWait)
	}
}

// removeFile deletes a file, ignoring "not found".
func removeFile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
