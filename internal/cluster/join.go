package cluster

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster/transport"
	"coyotedb.com/coyote/internal/invite"
	"coyotedb.com/coyote/internal/netdetect"
	"coyotedb.com/coyote/internal/pki"
)

// Join RPC (method "join"):
//
//	request:  joinRequest  — sent by the joiner on StreamJoin (no client
//	          cert; the joiner verifies the server against the invite's CA
//	          fingerprint). A follower forwards it unchanged to the leader
//	          over StreamRPC.
//	response: joinResponse — the signed certificate, the CA certificate and
//	          the member address list, plus the Raft index they reflect.
type joinRequest struct {
	ClusterID string `json:"cluster_id"`
	Secret    []byte `json:"secret"`
	CSR       []byte `json:"csr"`
	Member    Member `json:"member"` // id, name, addresses, role, platform
	// AllowMixedOS: the joiner consents to mixing kinds of machines.
	AllowMixedOS bool `json:"allow_mixed_os,omitempty"`
	// Protocol is the joiner's JoinProtocol (absent, 0, before D37).
	Protocol int `json:"protocol,omitempty"`
	// Version is the joiner's program version, for messages.
	Version string `json:"version,omitempty"`
}

// JoinProtocol versions what a member's program must understand. 2 (D37):
// the mixed-system state fields (Platform fingerprint and probe, Member
// guard, Settings.MixedOS). The leader refuses a joiner with an older
// protocol: its state machine decodes strictly and would halt on those
// fields, which leaves a dead node in the cluster.
const JoinProtocol = 2

type joinResponse struct {
	ClusterID string              `json:"cluster_id"`
	CertDER   []byte              `json:"cert_der"`
	CACertDER []byte              `json:"ca_cert_der"`
	Peers     map[string][]string `json:"peers"`
	Index     uint64              `json:"index"`
}

func hashSecret(secret []byte) string {
	h := sha256.Sum256(secret)
	return hex.EncodeToString(h[:])
}

// Bootstrap creates a new cluster with this node as its only member and
// voter: new cluster id, CA, node certificate, and the initial state (epoch
// 1; a data node becomes primary). It returns once the state is in Raft.
func Bootstrap(ctx context.Context, cfg Config) (*Node, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if err := cfg.validateNew(); err != nil {
		return nil, err
	}
	if has, err := HasState(cfg.Dir); err != nil || has {
		return nil, errors.Join(err, errors.New("cluster: this node already has cluster state; use Start"))
	}
	ln, addrs, err := listen(&cfg)
	if err != nil {
		return nil, err
	}
	now := cfg.Now().UTC()
	clusterID, nodeID := randomID(16), randomID(8)
	ca, err := pki.NewCA(clusterID, now)
	if err != nil {
		ln.Close()
		return nil, err
	}
	key, err := pki.GenerateKey()
	if err != nil {
		ln.Close()
		return nil, err
	}
	csr, err := pki.NewCSR(key, clusterID, nodeID)
	if err != nil {
		ln.Close()
		return nil, err
	}
	certDER, err := ca.IssueNode(csr, nodeID, now)
	if err != nil {
		ln.Close()
		return nil, err
	}
	caKey, err := ca.KeyDER()
	if err != nil {
		ln.Close()
		return nil, err
	}
	cmd := &BootstrapState{
		ClusterID: clusterID,
		CA:        CAData{CertDER: ca.Cert.Raw, KeyDER: caKey},
		Member: Member{
			ID: nodeID, Name: cfg.Name, Addresses: addrs, Role: cfg.Role,
			Suffrage: Voter, Platform: cfg.Platform, Status: StatusActive,
		},
		Settings: DefaultSettings(),
		At:       now,
	}
	data, err := EncodeCommand(cmd)
	if err != nil {
		ln.Close()
		return nil, err
	}
	if err := ensureDirs(cfg.Dir); err != nil {
		ln.Close()
		return nil, err
	}
	// The CA key exists only in memory until the Bootstrap command is in
	// the Raft log; keep it on disk (0600) so a crash in between can finish.
	if err := pki.WriteFileAtomic(filepath.Join(cfg.Dir, filePending), data, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	err = writeNodeFiles(cfg.Dir,
		identity{ClusterID: clusterID, NodeID: nodeID, Name: cfg.Name, Role: cfg.Role},
		func(p string) error { return pki.WriteKey(p, key) },
		certDER, ca.Cert.Raw,
		peerCache{Format: peersCacheFmt, Members: map[string][]string{nodeID: addrs}})
	if err != nil {
		ln.Close()
		return nil, err
	}
	n, err := open(cfg, ln, addrs, openOpts{bootstrap: &raft.Configuration{Servers: []raft.Server{
		{Suffrage: raft.Voter, ID: raft.ServerID(nodeID), Address: raft.ServerAddress(nodeID)},
	}}})
	if err != nil {
		return nil, err
	}
	if err := n.waitFor(ctx, func(st *State) bool { return st.Bootstrapped() }); err != nil {
		n.Shutdown()
		return nil, fmt.Errorf("bootstrap did not complete: %w", err)
	}
	return n, nil
}

// finishBootstrap proposes the pending Bootstrap command (leader only) and
// removes the pending file once the state is bootstrapped.
func (n *Node) finishBootstrap() {
	p := filepath.Join(n.cfg.Dir, filePending)
	if _, err := os.Stat(p); err != nil {
		return
	}
	if n.fsm.State().Bootstrapped() {
		if err := removeFile(p); err != nil {
			n.log.Warn("remove bootstrap.pending", "err", err)
		}
		return
	}
	if !n.IsLeader() {
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	if _, err := n.applyLocal(data); err != nil && !errors.Is(err, ErrAlreadyBootstrapped) {
		n.log.Warn("bootstrap command", "err", err)
		return
	}
	n.kickReconcile()
}

// Start runs a node from the state on disk (after a restart).
func Start(ctx context.Context, cfg Config) (*Node, error) {
	return start(ctx, cfg, openOpts{})
}

func start(_ context.Context, cfg Config, o openOpts) (*Node, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	has, err := HasState(cfg.Dir)
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, errors.New("cluster: no cluster state on this node; use Bootstrap or Join")
	}
	ln, addrs, err := listen(&cfg)
	if err != nil {
		return nil, err
	}
	return open(cfg, ln, addrs, o)
}

// Join joins the cluster described by inviteCode. It contacts the seed
// addresses in order (pinning the CA fingerprint), sends the join secret,
// a CSR and this node's info, stores the returned certificate and starts
// Raft. It returns once this node's own membership has replicated to it.
//
// Join is safe to retry. Before the first attempt the node id, key and
// invite are saved in Dir (join.pending.json + node.key). Network errors
// and a missing leader are retried with backoff (Timing.JoinAttempts); if
// they persist Join returns an error and keeps that state, and the next
// Join call on the same Dir resumes with the saved invite, id and key. A
// join whose response was lost after the leader committed it is then
// answered again by the leader (same node id and key), so a flaky link
// never burns an invite. Definitive refusals (expired/unknown/used secret,
// platform mismatch, duplicate name) clear the saved state and return.
func Join(ctx context.Context, cfg Config, inviteCode string) (*Node, error) {
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if err := cfg.validateNew(); err != nil {
		return nil, err
	}
	if has, err := HasState(cfg.Dir); err != nil || has {
		return nil, errors.Join(err, errors.New("cluster: this node already belongs to a cluster"))
	}
	if p, err := loadJoinPending(cfg.Dir); err != nil {
		return nil, err
	} else if p != nil {
		cfg.Logger.Info("resuming an interrupted join", "node_id", p.NodeID)
		n, err := joinAttempt(ctx, cfg, p)
		if err == nil || !definitive(err) {
			return n, err
		}
		if rerr := clearJoinPending(cfg.Dir); rerr != nil {
			return nil, errors.Join(err, rerr)
		}
		if invite.Normalize(inviteCode) == invite.Normalize(p.Invite) {
			return nil, err
		}
		// The saved attempt is dead; continue with the new invite.
	}
	inv, err := invite.Decode(inviteCode)
	if err != nil {
		return nil, err
	}
	if err := inv.Check(cfg.Now()); err != nil {
		return nil, err
	}
	key, err := pki.GenerateKey()
	if err != nil {
		return nil, err
	}
	p := &joinPending{Format: joinPendingFmt, Invite: invite.Normalize(inviteCode), NodeID: randomID(8), Name: cfg.Name, Role: cfg.Role}
	if err := saveJoinPending(cfg.Dir, p, key); err != nil {
		return nil, err
	}
	n, err := joinAttempt(ctx, cfg, p)
	if err != nil && definitive(err) {
		_ = clearJoinPending(cfg.Dir)
	}
	return n, err
}

// definitive reports whether a join error is a refusal that retrying the
// same request cannot fix.
func definitive(err error) bool {
	var ce *Error
	if errors.As(err, &ce) {
		// ErrMixedNotReady passes once a data node has audited (the
		// guard loop, or the leader's MixedJoinCheck): worth retrying.
		return !errors.Is(err, ErrNoLeader) && !errors.Is(err, ErrNotLeader) && !errors.Is(err, ErrMixedNotReady)
	}
	return errors.Is(err, invite.ErrCorrupt) || errors.Is(err, invite.ErrVersion) || errors.Is(err, invite.ErrNotInvite)
}

func joinAttempt(ctx context.Context, cfg Config, p *joinPending) (*Node, error) {
	inv, err := invite.Decode(p.Invite)
	if err != nil {
		return nil, err
	}
	key, err := pki.ReadKey(filepath.Join(cfg.Dir, fileKey))
	if err != nil {
		return nil, fmt.Errorf("read saved join key: %w", err)
	}
	csr, err := pki.NewCSR(key, inv.ClusterID, p.NodeID)
	if err != nil {
		return nil, err
	}
	ln, addrs, err := listen(&cfg)
	if err != nil {
		return nil, err
	}
	req := joinRequest{
		ClusterID: inv.ClusterID,
		Secret:    inv.Secret[:],
		CSR:       csr,
		Member: Member{
			ID: p.NodeID, Name: p.Name, Addresses: addrs, Role: p.Role, Platform: cfg.Platform,
		},
		AllowMixedOS: cfg.AllowMixedOS && p.Role == RoleData,
		Protocol:     JoinProtocol,
		Version:      cfg.Version,
	}
	resp, err := joinVia(ctx, inv, req, cfg.Timing, cfg.transportTiming())
	if err != nil {
		ln.Close()
		if !definitive(err) {
			err = fmt.Errorf("%w (progress is saved: run join again to resume; the invite stays reserved for this machine)", err)
		}
		return nil, err
	}
	if fp := pki.Fingerprint(resp.CACertDER); fp != inv.CAFingerprint {
		ln.Close()
		return nil, pki.ErrFingerprint
	}
	caCert, err := x509.ParseCertificate(resp.CACertDER)
	if err != nil {
		ln.Close()
		return nil, err
	}
	if id, err := pki.VerifyNodeCert(resp.CertDER, caCert, inv.ClusterID, cfg.Now()); err != nil || id != p.NodeID {
		ln.Close()
		return nil, errors.Join(err, errors.New("cluster: leader returned an unusable certificate"))
	}
	err = writeNodeFiles(cfg.Dir,
		identity{ClusterID: inv.ClusterID, NodeID: p.NodeID, Name: p.Name, Role: p.Role},
		func(path string) error { return pki.WriteKey(path, key) },
		resp.CertDER, resp.CACertDER,
		peerCache{Format: peersCacheFmt, Index: resp.Index, Members: resp.Peers})
	if err != nil {
		ln.Close()
		return nil, err
	}
	// identity.json now exists: the join is complete on disk.
	if err := removeFile(filepath.Join(cfg.Dir, fileJoinPending)); err != nil {
		cfg.Logger.Warn("remove join.pending.json", "err", err)
	}
	n, err := open(cfg, ln, addrs, openOpts{})
	if err != nil {
		return nil, err
	}
	if err := n.waitFor(ctx, func(st *State) bool {
		_, ok := st.Members[p.NodeID]
		return ok
	}); err != nil {
		n.Shutdown()
		return nil, fmt.Errorf("joined, but state did not replicate here yet (state is saved; Start will resume): %w", err)
	}
	return n, nil
}

// joinVia tries every seed, for up to timing.JoinAttempts rounds with
// exponential backoff. Definitive refusals return at once.
func joinVia(ctx context.Context, inv *invite.Invite, req joinRequest, timing Timing, tt transport.Timing) (joinResponse, error) {
	var lastErr error
	backoff := timing.JoinBackoff
	for round := 1; ; round++ {
		for _, addr := range orderSeeds(ctx, inv.Addrs, tt) {
			var resp joinResponse
			err := func() error {
				actx, cancel := context.WithTimeout(ctx, timing.RPCTimeout)
				defer cancel()
				c, err := transport.DialJoin(actx, addr, inv.ClusterID, inv.CAFingerprint, tt)
				if err != nil {
					return err
				}
				return callConn(actx, c, "join", req, &resp)
			}()
			if err == nil {
				return resp, nil
			}
			lastErr = fmt.Errorf("via %s: %w", addr, err)
			if definitive(err) {
				return joinResponse{}, err
			}
		}
		if round >= timing.JoinAttempts {
			return joinResponse{}, fmt.Errorf("could not reach the cluster after %d attempts: %w", round, lastErr)
		}
		select {
		case <-ctx.Done():
			return joinResponse{}, fmt.Errorf("could not join: %w", errors.Join(lastErr, ctx.Err()))
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// seedProbeTimeout bounds the reachability probe before each join round.
const seedProbeTimeout = time.Second

// orderSeeds puts the seeds that answer a TCP connect first, fastest first
// (netdetect.Probe, all in parallel), and the others after them in the
// invite's order: a code made on a machine with Tailscale and a LAN address
// joins over whichever this machine can actually reach.
func orderSeeds(ctx context.Context, seeds []string, tt transport.Timing) []string {
	if len(seeds) < 2 {
		return seeds
	}
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		if tt.Redirect != nil {
			addr = tt.Redirect(addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	ok := netdetect.Reachable(netdetect.Probe(ctx, seeds, seedProbeTimeout, dial))
	out := slices.Clone(ok)
	for _, s := range seeds {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func (n *Node) rpcJoin(ctx context.Context, raw json.RawMessage) (any, error) {
	var req joinRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if n.IsLeader() {
		if req.Protocol < JoinProtocol {
			ours := n.cfg.Version
			if ours == "" {
				ours = "the cluster's version"
			}
			return nil, errorf(ErrJoinerTooOld,
				"this machine runs an older %s than the cluster. Update %s on this machine to %s (or newer) first, then run the same command again",
				brand.Command, brand.Command, ours)
		}
		if n.cfg.MixedFieldsOK == nil || !n.cfg.MixedFieldsOK(ctx) {
			// Rolling upgrade: never write a field an older member
			// cannot decode. A mixed joiner is then refused (not
			// measured / mixing off), an alike one joins as before.
			req.Member.Platform = Platform{OS: req.Member.Platform.OS, Arch: req.Member.Platform.Arch, PGBuildSHA: req.Member.Platform.PGBuildSHA}
			req.AllowMixedOS = false
		}
		if err := n.mixedJoinCheck(ctx, req); err != nil {
			return nil, err
		}
		return n.admit(req)
	}
	leader := n.LeaderID()
	if leader == "" || leader == n.id.NodeID {
		return nil, ErrNoLeader
	}
	var resp joinResponse
	if err := n.call(ctx, leader, "join", req, &resp); err != nil {
		var se *sendError
		if errors.As(err, &se) {
			return nil, ErrNoLeader // joiner retries
		}
		return nil, err
	}
	return resp, nil
}

// admit validates a join on the leader, signs the certificate and adds the
// member (as a non-voter; reconcile promotes it per the suffrage policy once
// it has caught up).
func (n *Node) admit(req joinRequest) (joinResponse, error) {
	n.memberMu.Lock()
	defer n.memberMu.Unlock()
	if !n.IsLeader() {
		return joinResponse{}, ErrNotLeader
	}
	st := n.fsm.State()
	if !st.Bootstrapped() {
		return joinResponse{}, ErrNotBootstrapped
	}
	if req.ClusterID != st.ClusterID || len(req.Secret) != 32 {
		return joinResponse{}, ErrSecretUnknown
	}
	now := n.now()
	keyHash, err := pki.CSRKeyHash(req.CSR)
	if err != nil {
		return joinResponse{}, errorf(ErrInvalid, "%v", err)
	}
	m := req.Member
	m.Suffrage = Nonvoter
	m.Status = StatusActive
	secretHash := hashSecret(req.Secret)
	// Retry of a join that already committed (response lost): same node id
	// and same key as recorded on the consumed secret, and still a member.
	// Answer again instead of refusing. Anyone else gets ErrSecretUsed.
	if s, ok := st.JoinSecrets[secretHash]; ok && s.Used && s.UsedBy == m.ID && s.UsedByKey == keyHash {
		if _, member := st.Members[m.ID]; member {
			return n.joinResponse(st, req.CSR, m.ID, now, st.AppliedIndex, true)
		}
	}
	cmd := &AddMember{Member: m, SecretHash: secretHash, KeyHash: keyHash, At: now, AllowMixedOS: req.AllowMixedOS}
	// Dry run on a copy: same checks the FSM will make (secret, id, name,
	// platform and the mixed-system rule), before signing anything.
	dry := st.Clone()
	if err := applyCommand(&dry, cmd); err != nil {
		return joinResponse{}, n.explainRefusal(st, m, err)
	}
	// Sign first (so a bad CSR fails before anything is committed), but
	// answer only after the member is committed.
	if _, err := signCSR(st, req.CSR, m.ID, now); err != nil {
		return joinResponse{}, err
	}
	data, err := EncodeCommand(cmd)
	if err != nil {
		return joinResponse{}, err
	}
	res, err := n.applyLocal(data)
	if err != nil {
		return joinResponse{}, err
	}
	f := n.raft.AddNonvoter(raft.ServerID(m.ID), raft.ServerAddress(m.ID), 0, n.timing.ApplyTimeout)
	if err := f.Error(); err != nil {
		// Membership is committed; reconcile will add it to Raft.
		n.log.Warn("add non-voter (reconcile will retry)", "member", m.ID, "err", err)
	}
	n.log.Info("admitted member", "id", m.ID, "name", m.Name, "role", m.Role)
	return n.joinResponse(n.fsm.State(), req.CSR, m.ID, now, res.Index, false)
}

// mixedJoinCheck runs Config.MixedJoinCheck for a data joiner that is not
// alike to every data member (a mixed join, safety rule 1 extension),
// before admit, without holding memberMu (the check writes to the cluster
// state through a data node). Retries of a join that already committed
// skip it.
func (n *Node) mixedJoinCheck(ctx context.Context, req joinRequest) error {
	if n.cfg.MixedJoinCheck == nil || req.Member.Role != RoleData {
		return nil
	}
	st := n.fsm.State()
	if _, member := st.Members[req.Member.ID]; member {
		return nil
	}
	mixed := false
	for _, o := range st.DataMembers() {
		if !o.Platform.Alike(req.Member.Platform) {
			mixed = true
		}
	}
	if !mixed || (!st.Settings.MixedOS && !req.AllowMixedOS) {
		return nil // admit refuses (or accepts) on its own
	}
	cctx, cancel := context.WithTimeout(ctx, n.timing.RPCTimeout*3/4)
	defer cancel()
	if err := n.cfg.MixedJoinCheck(cctx, req.Member); err != nil {
		var ce *Error
		if errors.As(err, &ce) {
			return err
		}
		return errorf(ErrMixedNotReady, "%v", err)
	}
	return nil
}

// explainRefusal replaces a fingerprint refusal's field list with the
// plain sentences of Config.ExplainPlatform, when both sides recorded
// their fingerprint text.
func (n *Node) explainRefusal(st State, m Member, err error) error {
	if n.cfg.ExplainPlatform == nil || !errors.Is(err, ErrPlatformMismatch) || m.Platform.FingerprintText == "" {
		return err
	}
	for _, o := range st.DataMembers() {
		if o.Platform.Alike(m.Platform) || o.Platform.FingerprintText == "" || o.Platform.Fingerprint == m.Platform.Fingerprint {
			continue
		}
		if lines := n.cfg.ExplainPlatform(m.Platform, o.Platform); len(lines) > 0 {
			return errorf(ErrPlatformMismatch, "this machine's database programs differ from %q (%s): %s",
				o.Name, describePlatform(o.Platform), strings.Join(lines, " "))
		}
	}
	return err
}

func signCSR(st State, csr []byte, nodeID string, now time.Time) ([]byte, error) {
	ca, err := pki.LoadCA(st.CA.CertDER, st.CA.KeyDER)
	if err != nil {
		return nil, err
	}
	der, err := ca.IssueNode(csr, nodeID, now)
	if err != nil {
		return nil, errorf(ErrInvalid, "%v", err)
	}
	return der, nil
}

func (n *Node) joinResponse(st State, csr []byte, nodeID string, now time.Time, index uint64, retry bool) (joinResponse, error) {
	certDER, err := signCSR(st, csr, nodeID, now)
	if err != nil {
		return joinResponse{}, err
	}
	if retry {
		n.log.Info("join retried by an admitted member; answering again", "id", nodeID)
	}
	n.kickReconcile()
	peers := map[string][]string{}
	for id, mem := range st.Members {
		peers[id] = slices.Clone(mem.Addresses)
	}
	return joinResponse{
		ClusterID: st.ClusterID,
		CertDER:   certDER,
		CACertDER: st.CA.CertDER,
		Peers:     peers,
		Index:     index,
	}, nil
}

// CreateInvite creates a one-time join secret valid for ttl and returns the
// invite code. addrs are the seed addresses to put in the code; if empty,
// this node's addresses come first, then the other members'.
func (n *Node) CreateInvite(ctx context.Context, ttl time.Duration, addrs []string) (string, error) {
	if ttl <= 0 {
		return "", errors.New("invite ttl must be positive")
	}
	st := n.fsm.State()
	if !st.Bootstrapped() {
		return "", ErrNotBootstrapped
	}
	if len(addrs) == 0 {
		addrs = n.AdvertiseAddrs()
		for _, m := range st.SortedMembers() {
			for _, a := range m.Addresses {
				if !slices.Contains(addrs, a) {
					addrs = append(addrs, a)
				}
			}
		}
	}
	if len(addrs) > invite.MaxAddrs {
		addrs = addrs[:invite.MaxAddrs]
	}
	secret, err := invite.NewSecret()
	if err != nil {
		return "", err
	}
	now := n.now()
	inv := &invite.Invite{
		ClusterID:     st.ClusterID,
		Addrs:         addrs,
		Secret:        secret,
		CAFingerprint: pki.Fingerprint(st.CA.CertDER),
		// Whole seconds: the code stores unix seconds.
		Expires: now.Add(ttl).Truncate(time.Second),
	}
	code, err := inv.Encode()
	if err != nil {
		return "", err
	}
	if _, err := n.Apply(ctx, &CreateJoinSecret{Hash: hashSecret(secret[:]), CreatedAt: now, ExpiresAt: inv.Expires}); err != nil {
		return "", err
	}
	return code, nil
}

// RemoveMember removes a member from the cluster (forwarded to the leader).
// From that moment its certificate is refused everywhere; the leader then
// removes it from the Raft configuration and rebalances suffrage.
func (n *Node) RemoveMember(ctx context.Context, nodeID string) error {
	cfgServers, err := n.RaftConfiguration()
	if err == nil {
		voters := 0
		isVoter := false
		for _, s := range cfgServers {
			if s.Suffrage == Voter {
				voters++
				isVoter = isVoter || s.ID == nodeID
			}
		}
		if isVoter && voters == 1 {
			return ErrLastVoter
		}
	}
	_, err = n.Apply(ctx, &RemoveMember{ID: nodeID})
	return err
}

// Leave removes this node from the cluster and shuts it down. If it is the
// leader it hands leadership over first.
//
// The removal's reply can be lost after it committed: the leader closes the
// removed node's connections as soon as it applies the removal (the
// certificate is refused from that moment), and it may also stop between
// the commit and the reply. Leave resolves that by asking again until the
// answer is certain: the removal is done when it succeeds, when the
// cluster says this node is no longer a member, or when a member that
// accepted this node before now refuses its certificate. Leave is
// idempotent: called again after a committed removal it succeeds.
func (n *Node) Leave(ctx context.Context) error {
	if n.ctx.Err() != nil {
		return n.Shutdown() // already left (or stopped)
	}
	if n.IsLeader() {
		if err := n.waitFuture(ctx, n.raft.LeadershipTransfer()); err != nil {
			return fmt.Errorf("hand over leadership: %w", err)
		}
	}
	// Every member reachable now accepts this node once, so that a later
	// refusal by any of them is proof of the removal (refusedAfterAdmit),
	// even if the leader that took the removal is gone by then.
	n.greetMembers(ctx)
	for {
		err := n.RemoveMember(ctx, n.id.NodeID)
		switch {
		case err == nil, errors.Is(err, ErrRemoved), errors.Is(err, ErrUnknownMember):
			return n.Shutdown()
		case isDecision(err):
			// The cluster answered and refused (in the sync set, the main
			// node, the last voter, ...): nothing was removed.
			return err
		}
		n.log.Info("leave: outcome unknown, asking again", "err", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("leave: could not learn whether the removal committed (last error: %v): %w", err, ctx.Err())
		case <-n.ctx.Done():
			return errors.New("leave: node is shutting down")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// greetMembers opens one status stream to every other member, in parallel
// and briefly. Members that do not answer are skipped.
func (n *Node) greetMembers(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for id := range n.fsm.State().Members {
		if id == n.id.NodeID {
			continue
		}
		wg.Go(func() { _, _ = n.PeerStatus(ctx, id) })
	}
	wg.Wait()
}

// isDecision reports whether err is a definite answer from the cluster to a
// command (as opposed to a lost reply, a timeout or a leader change, after
// which the command may or may not have committed).
func isDecision(err error) bool {
	var ce *Error
	return errors.As(err, &ce) && !errors.Is(err, ErrNotLeader) && !errors.Is(err, ErrNoLeader)
}

// waitFor blocks until cond(state) is true or ctx ends.
func (n *Node) waitFor(ctx context.Context, cond func(*State) bool) error {
	sub := n.fsm.Subscribe()
	defer sub.Close()
	n.kickReconcile()
	for {
		st := n.fsm.State()
		if cond(&st) {
			return nil
		}
		if err := n.fsm.Halted(); err != nil {
			return err
		}
		select {
		case <-sub.C:
		case <-ctx.Done():
			return ctx.Err()
		case <-n.ctx.Done():
			return errors.New("node shut down")
		}
	}
}

// WaitFor blocks until cond holds on the local state or ctx ends.
func (n *Node) WaitFor(ctx context.Context, cond func(*State) bool) error {
	return n.waitFor(ctx, cond)
}
