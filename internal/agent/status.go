package agent

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/controller"
	"coyotedb.com/coyote/internal/netdetect"
	"coyotedb.com/coyote/internal/pg"
	"coyotedb.com/coyote/internal/proxy"
)

// NodeReport is one node's own view of itself (agent RPC "status").
type NodeReport struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Role             string    `json:"role"`  // data or witness
	State            string    `json:"state"` // primary, standby, witness, cloning, rejoining, waiting, starting, needs-attention
	Message          string    `json:"message,omitempty"`
	Epoch            uint64    `json:"epoch"` // of this node's copy of the cluster state
	LastPrimaryEpoch uint64    `json:"last_primary_epoch,omitempty"`
	Marker           string    `json:"rejoin_marker,omitempty"`
	Progress         *int      `json:"copy_progress,omitempty"`
	PG               *PGReport `json:"postgres,omitempty"`
	// The stream cut this node keeps (rule 2): at which epoch, and whether
	// it resumes by itself (made by a switch or failover).
	CutEpoch uint64 `json:"cut_epoch,omitempty"`
	CutHold  bool   `json:"cut_hold,omitempty"`
	// Lease: this node holds the primary lease (rule 4).
	Lease bool `json:"lease,omitempty"`
	// QuorumContactAgoMS: how long ago this node's cluster state was last
	// backed by a leader holding a quorum (conservatively); absent: never
	// (for example, no leader since this agent started).
	QuorumContactAgoMS *int64 `json:"quorum_contact_ago_ms,omitempty"`
	// Proxy is the client port's counters (data nodes).
	Proxy *ProxyReport `json:"proxy,omitempty"`
	// Peers: round trip to each other member's addresses, as last measured
	// by this node (every PeerProbeInterval).
	Peers []PeerReport `json:"peers,omitempty"`
	// DiskFree: bytes free on the disk that holds the data folder (data
	// nodes; absent until measured). A main node with a full disk hands
	// over only to a copy with room (mainhealth.go).
	DiskFree *uint64 `json:"disk_free_bytes,omitempty"`
	// Pages is the data node's page health (D55): damaged pages found,
	// Postgres' checksum counters, the page check's progress.
	Pages *PageReport `json:"pages,omitempty"`
	// PGBuild is the sha256 of the PostgreSQL build this data node runs
	// now (D70); its cluster record may still name the build it ran
	// before an update for a few seconds after its database started.
	PGBuild string `json:"pg_build,omitempty"`
}

// ProxyReport is the client port and its counters.
type ProxyReport struct {
	Port int `json:"port"`
	proxy.Stats
}

// PGReport is what a node's Postgres says.
type PGReport struct {
	Running          bool            `json:"running"`
	Port             int             `json:"port"`
	Role             string          `json:"role,omitempty"` // primary or standby
	Timeline         uint32          `json:"timeline,omitempty"`
	Streaming        bool            `json:"streaming"`
	StreamCut        bool            `json:"stream_cut,omitempty"`
	ReceiveLSN       *pg.LSN         `json:"receive_lsn,omitempty"`
	ReplayLSN        *pg.LSN         `json:"replay_lsn,omitempty"`
	FlushLSN         *pg.LSN         `json:"flush_lsn,omitempty"`
	SyncStandbyNames string          `json:"synchronous_standby_names,omitempty"`
	Standbys         []pg.StandbyRow `json:"standbys,omitempty"`
	Slots            []pg.Slot       `json:"slots,omitempty"`
	Error            string          `json:"error,omitempty"`
}

func (a *Agent) report(ctx context.Context) NodeReport {
	phase, msg, prog := a.phaseInfo()
	st := a.cn.State()
	f := a.st.get()
	r := NodeReport{
		ID: a.cn.ID(), Name: a.name, Role: string(a.role), State: phase, Message: msg,
		Epoch: st.Epoch, LastPrimaryEpoch: f.LastPrimaryEpoch,
		CutEpoch: f.CutEpoch, CutHold: !f.CutUntil.IsZero(), Lease: a.holdsLease(st.Epoch),
	}
	if prog >= 0 {
		r.Progress = &prog
	}
	if free, ok := a.health.diskFree(); ok {
		r.DiskFree = &free
	}
	if qc := a.cn.QuorumContact(); !qc.IsZero() {
		ms := time.Since(qc).Milliseconds()
		r.QuorumContactAgoMS = &ms
	}
	r.Peers = a.peerReports()
	if a.role != cluster.RoleData {
		return r
	}
	if p := a.proxy.Load(); p != nil {
		r.Proxy = &ProxyReport{Port: a.clientPort, Stats: p.Stats()}
	}
	if m, err := a.node.ReadMarker(); err == nil && m != nil {
		r.Marker = m.Reason
	}
	r.PG = a.pgReport(ctx)
	r.Pages = a.pages.report()
	r.PGBuild = a.inst.Build.SHA256
	return r
}

func (a *Agent) pgReport(ctx context.Context) *PGReport {
	p := &PGReport{Port: a.pgPort}
	if rs, err := a.node.ReadReplState(); err == nil {
		p.StreamCut = rs.Upstream == nil
	}
	srv, role := a.currentPG()
	if srv == nil {
		return p
	}
	p.Running, p.Role = true, role
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	c, err := a.node.Connect(cctx)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	defer c.Close(context.Background())
	rs, err := pg.ReplicationStatus(cctx, c)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	p.Role, p.Timeline = rs.Role, rs.Timeline
	p.ReceiveLSN, p.ReplayLSN, p.FlushLSN = rs.ReceiveLSN, rs.ReplayLSN, rs.FlushLSN
	p.SyncStandbyNames, p.Standbys, p.Slots = rs.SyncStandbyNames, rs.Standbys, rs.Slots
	p.Streaming = rs.WalReceiver != nil && rs.WalReceiver.Status == "streaming"
	return p
}

// Status is `coyote status --json`.
type Status struct {
	Self     string       `json:"self"` // this node's id
	Cluster  ClusterView  `json:"cluster"`
	Nodes    []MemberView `json:"nodes"`
	Warnings []string     `json:"warnings"`
	// Controller is the leader's controller report (nil: no leader, or it
	// did not answer).
	Controller *controller.Report `json:"controller,omitempty"`
	// Health is the plain-words summary `status` prints first.
	Health Health `json:"health"`
	// Waiting: this node has no copy of the cluster state yet (no leader
	// has reached it since it started); nil otherwise (D62).
	Waiting *WaitingView `json:"waiting,omitempty"`
	// Backup is this node's view of backups (data nodes; D63).
	Backup *BackupView `json:"backup,omitempty"`
	// Problems: what needs attention, each with a stable code, a plain
	// line and a long form (D66, problems.go). Every Summary is in
	// Warnings too.
	Problems []Problem `json:"problems"`
}

// WaitingView is what a node that has not heard from the cluster since it
// started knows: how long it has waited and where it last saw the others.
type WaitingView struct {
	ForMS int64         `json:"for_ms"`
	Peers []WaitingPeer `json:"peers"`
}

// WaitingPeer is another member from this node's saved address book.
type WaitingPeer struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses"` // recorded, plus any given with --local
	Answers   bool     `json:"answers"`   // one of them accepts a connection now
}

// waitingHelpAfter: how long a node waits with nobody answering before
// status explains what to do about changed addresses.
const waitingHelpAfter = 30 * time.Second

// Health is the summary of the cluster in plain words.
type Health struct {
	MainNode string `json:"main_node"`
	// Lease: "renewed" (healthy), "not renewed for 7s" or "unknown".
	Lease string `json:"lease"`
	// Copies: whether writes wait for a copy, and on which nodes.
	Copies string `json:"copies"`
	// WritesWait: a commit returns only after another node has it.
	WritesWait bool `json:"writes_wait"`
	// OneCopy: running on one data node since OneCopySince.
	OneCopy      bool      `json:"one_copy"`
	OneCopySince time.Time `json:"one_copy_since,omitzero"`
	// Failover says why the main node is not being replaced, when it is
	// gone ("" when nothing is wrong).
	Failover string `json:"failover,omitempty"`
}

// ClusterView is the cluster state as this node knows it.
type ClusterView struct {
	ID    string `json:"id"`
	Epoch uint64 `json:"epoch"`
	// StandbyEligible: rule 9's flag (false: running on one copy).
	StandbyEligible      bool      `json:"standby_eligible"`
	StandbyEligibleSince time.Time `json:"standby_eligible_since,omitzero"`
	// LastServed is the last main node recorded as having served with its
	// copies caught up (it no longer counts as a failover candidate).
	LastServed  string   `json:"last_served,omitempty"`
	Mode        string   `json:"single_data_node_mode"`
	PrimaryID   string   `json:"primary_id"`
	PrimaryName string   `json:"primary_name"`
	LeaderID    string   `json:"leader_id"`
	LeaderName  string   `json:"leader_name"`
	SyncSet     []string `json:"sync_set"` // names
	SyncK       int      `json:"sync_k"`
	// MixedOS: data nodes of different kinds may share this cluster (D37).
	MixedOS bool `json:"mixed_os,omitempty"`
}

// MemberView is one member, from the cluster state plus its own report.
type MemberView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Role      string   `json:"role"`     // data or witness
	Suffrage  string   `json:"suffrage"` // voter or nonvoter
	Status    string   `json:"status"`
	Addresses []string `json:"addresses"`
	// AddressesPinned: set with `node set-address`, kept over what the
	// node detects.
	AddressesPinned bool        `json:"addresses_pinned,omitempty"`
	ClientPort      int         `json:"client_port,omitempty"`
	Primary         bool        `json:"primary"`
	Leader          bool        `json:"leader"`
	This            bool        `json:"this"`
	InSyncSet       bool        `json:"in_sync_set"`
	Reachable       bool        `json:"reachable"`
	Report          *NodeReport `json:"report,omitempty"`
	// Replication as the primary sees it: "streaming (sync)", "streaming",
	// "not connected".
	Replication string `json:"replication,omitempty"`
	// LagBytes: how far behind the main node this copy is (flushed WAL),
	// -1 when unknown.
	LagBytes int64 `json:"lag_bytes"`
	// System is "macOS arm64" and the like (data nodes); Fingerprint the
	// first 12 characters of its database fingerprint ("" = not measured);
	// Guard its latest mixed-system check results (D37).
	System      string         `json:"system,omitempty"`
	Fingerprint string         `json:"fingerprint,omitempty"`
	Guard       *cluster.Guard `json:"guard,omitempty"`
	// PostgreSQL is the recorded PostgreSQL version ("18.7") of a data
	// node whose build is in the cluster's table of pinned builds (D70).
	PostgreSQL string `json:"postgresql,omitempty"`
}

func (a *Agent) clusterStatus(ctx context.Context) Status {
	st := a.cn.State()
	self := a.cn.ID()
	leader := a.cn.LeaderID()
	s := Status{Self: self, Warnings: []string{}}
	s.Cluster = ClusterView{ID: st.ClusterID, Epoch: st.Epoch, PrimaryID: st.PrimaryID, LeaderID: leader, SyncK: st.Sync.K,
		StandbyEligible: st.StandbyEligible, StandbyEligibleSince: st.StandbyEligibleSince, Mode: st.Settings.SingleDataNodeMode,
		MixedOS: st.Settings.MixedOS}
	if m, ok := st.Members[st.LastServed]; ok {
		s.Cluster.LastServed = m.Name
	}
	if m, ok := st.Members[st.PrimaryID]; ok {
		s.Cluster.PrimaryName = m.Name
	}
	if m, ok := st.Members[leader]; ok {
		s.Cluster.LeaderName = m.Name
	}
	for _, id := range st.Sync.Members {
		if m, ok := st.Members[id]; ok {
			s.Cluster.SyncSet = append(s.Cluster.SyncSet, m.Name)
		}
	}
	slices.Sort(s.Cluster.SyncSet)

	members := st.SortedMembers()
	reports := make([]*NodeReport, len(members))
	var wg sync.WaitGroup
	if leader != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			var r controller.Report
			if err := a.call(cctx, leader, "controller_status", nil, &r); err == nil && r.Leader {
				s.Controller = &r
			}
		}()
	}
	for i, m := range members {
		if m.ID == self {
			r := a.report(ctx)
			reports[i] = &r
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			var r NodeReport
			if err := a.call(cctx, m.ID, "status", nil, &r); err == nil {
				reports[i] = &r
			}
		}()
	}
	wg.Wait()

	var primaryReport *NodeReport
	for i, m := range members {
		if m.ID == st.PrimaryID {
			primaryReport = reports[i]
		}
	}
	rows := map[string]pg.StandbyRow{}
	if primaryReport != nil && primaryReport.PG != nil {
		for _, r := range primaryReport.PG.Standbys {
			rows[r.Name] = r
		}
	}
	for i, m := range members {
		v := MemberView{
			ID: m.ID, Name: m.Name, Role: string(m.Role), Suffrage: string(m.Suffrage), Status: m.Status,
			Addresses: m.Addresses, AddressesPinned: m.AddressesPinned, ClientPort: m.ClientPort, Primary: m.ID == st.PrimaryID, Leader: m.ID == leader, This: m.ID == self,
			InSyncSet: slices.Contains(st.Sync.Members, m.ID), Reachable: reports[i] != nil, Report: reports[i],
			LagBytes: -1,
		}
		if m.Role == cluster.RoleData {
			v.System, v.Guard = systemName(m.Platform), m.Guard
			if pv, ok := st.PGVersionOf(m.Platform); ok {
				v.PostgreSQL = pv.Short()
			}
			if len(m.Platform.Fingerprint) >= 12 {
				v.Fingerprint = m.Platform.Fingerprint[:12]
			}
		}
		if m.Role == cluster.RoleData && m.ID != st.PrimaryID && primaryReport != nil {
			if r, ok := rows[m.Name]; ok && r.State == "streaming" {
				v.Replication = "streaming"
				if r.SyncState == "sync" || r.SyncState == "quorum" {
					v.Replication = "streaming (sync)"
				}
				if r.FlushLSN != nil && primaryReport.PG.FlushLSN != nil {
					v.LagBytes = max(int64(*primaryReport.PG.FlushLSN)-int64(*r.FlushLSN), 0)
				}
			} else {
				v.Replication = "not connected"
			}
		}
		s.Nodes = append(s.Nodes, v)
	}
	if !st.Bootstrapped() {
		s.Waiting = a.waitingView(ctx)
	}
	now := time.Now()
	probs, rest := statusProblems(st, s, primaryReport, a.name, now)
	probs = append(a.addressProblems(st, s, now), probs...)
	probs = dropCoveredByAddress(probs)
	s.Problems = probs
	s.Warnings = append(summaries(probs), rest...)
	if c := s.Controller; c != nil {
		for _, n := range c.View.Notes {
			if !slices.Contains(s.Warnings, n) && !strings.HasPrefix(n, "Writes are waiting") {
				s.Warnings = append(s.Warnings, n)
			}
		}
		if c.View.FailoverBlocked != "" {
			s.Warnings = append(s.Warnings, c.View.FailoverBlocked)
		}
	}
	s.Health = health(st, s)
	s.Backup = a.backupView()
	return s
}

// waitingView lists the other members from the saved address book and
// whether each answers now (a plain TCP connect).
func (a *Agent) waitingView(ctx context.Context) *WaitingView {
	w := &WaitingView{ForMS: time.Since(a.clusterOpened).Milliseconds(), Peers: []WaitingPeer{}}
	book := a.cn.PeerBook()
	var all []string
	for _, p := range book {
		all = append(all, p.Local...)
		all = append(all, p.Recorded...)
	}
	ok := map[string]bool{}
	for _, r := range netdetect.Probe(ctx, all, 2*time.Second, nil) {
		ok[r.Addr] = r.OK
	}
	for _, p := range book {
		wp := WaitingPeer{Name: cmp.Or(p.Name, p.ID)}
		for _, ad := range append(slices.Clone(p.Local), p.Recorded...) {
			if !slices.Contains(wp.Addresses, ad) {
				wp.Addresses = append(wp.Addresses, ad)
			}
			wp.Answers = wp.Answers || ok[ad]
		}
		w.Peers = append(w.Peers, wp)
	}
	return w
}

// addressProblems: this node's own address finding, what other nodes
// reported about theirs (they may not be reachable from here), and, with
// no copy of the state, the nodes this one waits for (D66).
func (a *Agent) addressProblems(st cluster.State, s Status, now time.Time) []Problem {
	var out []Problem
	leader := s.Cluster.LeaderID != ""
	if w := s.Waiting; w != nil {
		var lines []string
		answering := 0
		for _, p := range w.Peers {
			state := "no answer"
			if p.Answers {
				state, answering = "answers", answering+1
			}
			lines = append(lines, fmt.Sprintf("%s at %s (%s)", p.Name, strings.Join(p.Addresses, ", "), state))
		}
		waited := time.Duration(w.ForMS) * time.Millisecond
		p := newProblem(codeWaitingForCluster, a.name, fmt.Sprintf("This node has not heard from the cluster since it started %s ago and is waiting to reach the other nodes.", waited.Round(time.Second)))
		if len(lines) > 0 {
			p.Summary += " It knows them as: " + strings.Join(lines, "; ") + "."
		}
		p.Since = now.Add(-waited)
		p.Evidence = lines
		p.finish("", a.name, now)
		out = append(out, p)
		if answering == 0 && len(w.Peers) > 0 && waited >= waitingHelpAfter {
			out = append(out, a.peersUnreachable(w.Peers, now))
		}
	}
	names := map[string]string{}
	reachable := map[string]bool{}
	for _, n := range s.Nodes {
		names[n.ID], reachable[n.ID] = n.Name, n.Reachable
	}
	if r := a.addrs.selfReport(); r != nil {
		out = append(out, addressProblem(a.name, *r, a.name, true, true, leader, now, now))
	}
	recv := a.addrs.received(now)
	ids := slices.Sorted(maps.Keys(recv))
	for _, id := range ids {
		r := recv[id]
		name, ok := names[id]
		if !ok {
			continue // not a member (any more)
		}
		// A report about a record that changed since is stale: the record
		// was fixed and the next report will say whether it still fails.
		if m, ok := st.Members[id]; ok && !sameSet(m.Addresses, r.Recorded) {
			continue
		}
		if len(r.Gone) == 0 && reachable[id] {
			continue // one-way seen there, but this node reaches it
		}
		out = append(out, addressProblem(name, r.addrReport, a.name, false, reachable[id], leader, r.At, now))
	}
	// No leader here and nobody answers for a while: their addresses may
	// have changed (a restored snapshot can hold old ones too).
	if s.Waiting == nil && st.Bootstrapped() && !leader && time.Since(a.clusterOpened) >= waitingHelpAfter {
		probed := a.peerReports()
		reached := 0
		for _, p := range probed {
			if slices.ContainsFunc(p.Addresses, func(x PeerAddrProbe) bool { return x.OK }) {
				reached++
			}
		}
		if reached == 0 && len(probed) > 0 {
			out = append(out, a.peersUnreachable(nil, now))
		}
	}
	return out
}

// peersUnreachable: no other node answers at any address this node knows
// (D62 case 4). peers: from the waiting view (nil: from the peer book).
func (a *Agent) peersUnreachable(peers []WaitingPeer, now time.Time) Problem {
	if peers == nil {
		for _, p := range a.cn.PeerBook() {
			peers = append(peers, WaitingPeer{Name: cmp.Or(p.Name, p.ID), Addresses: append(slices.Clone(p.Local), p.Recorded...)})
		}
	}
	p := newProblem(codePeersUnreachable, a.name, "No other node answers at the addresses this node knows. If these computers got new addresses (a router restart can do that), "+
		"tell each computer where the others are now: on each one run `"+brand.Command+" node set-address --local NODE NEWHOST:PORT` for each other node.")
	for _, w := range peers {
		p.Evidence = append(p.Evidence, fmt.Sprintf("%s: known at %s; no answer from here.", w.Name, strings.Join(w.Addresses, ", ")))
		p.Commands = append(p.Commands, fmt.Sprintf("%s node set-address --local %s NEWHOST:%s", brand.Command, w.Name, firstPort(w.Addresses)))
	}
	p.Evidence = append(p.Evidence, fmt.Sprintf("This node (%s) started %s ago and has had no leader since.", a.name, time.Since(a.clusterOpened).Round(time.Second)))
	p.Fix = "Start the other computers. If their addresses changed, run the commands below on each computer (with the other nodes' new addresses); once a majority reach each other, the cluster runs again and each node shows the address to record for it. To avoid this, reserve each computer's address in your router, or use Tailscale."
	p.finish("", a.name, now)
	return p
}

// dropCoveredByAddress: a node with an address problem is not also listed
// as "not reachable" (the address problem says why).
func dropCoveredByAddress(ps []Problem) []Problem {
	covered := map[string]bool{}
	for _, p := range ps {
		if strings.HasPrefix(p.Code, "address_") {
			covered[p.Node] = true
		}
	}
	return slices.DeleteFunc(ps, func(p Problem) bool {
		return (p.Code == codeNodeUnreachable || p.Code == codeMainUnreachable) && covered[p.Node]
	})
}

// health is the summary in plain words.
func health(st cluster.State, s Status) Health {
	h := Health{MainNode: s.Cluster.PrimaryName, Lease: "unknown", OneCopy: !st.StandbyEligible}
	if h.MainNode == "" {
		h.MainNode = "none"
	}
	if !st.Bootstrapped() {
		// Nothing is known yet (D62): say nothing rather than guess.
		return h
	}
	if h.OneCopy {
		h.OneCopySince = st.StandbyEligibleSince
	}
	var copies []string
	for _, n := range s.Nodes {
		if n.Primary || n.Role != string(cluster.RoleData) {
			continue
		}
		if strings.HasSuffix(n.Replication, "(sync)") {
			copies = append(copies, n.Name)
		}
	}
	h.WritesWait = len(copies) > 0
	waiting, mainSeen := writesWaitingFor(st, s)
	switch {
	case h.WritesWait:
		h.Copies = "Writes wait for a copy on " + strings.Join(copies, " or ") + " before they are confirmed."
	case len(waiting) > 0:
		// Writes wait for copies that are not connected (warnings say
		// what happens next); never "not waiting" next to it (D62).
		h.WritesWait = true
		h.Copies = "Writes are waiting for a copy on " + strings.Join(waiting, " or ") + ", and none is connected."
	case !mainSeen && st.PrimaryID != "" && len(st.DataMembers()) > 1:
		h.Copies = "Whether writes reach a copy is not known here: the main node does not answer this node."
	case h.OneCopy && len(st.DataMembers()) > 1:
		h.Copies = "Running on one copy" + sinceText(st.StandbyEligibleSince) + ": writes are saved on " + h.MainNode + " only."
	case len(st.DataMembers()) <= 1:
		h.Copies = "One data node: writes are saved on " + h.MainNode + " only."
	default:
		h.Copies = "Writes are not waiting for any copy right now."
	}
	if c := s.Controller; c != nil {
		if c.View.LeaseHealthy {
			h.Lease = "renewed"
		} else if st.PrimaryID != "" {
			h.Lease = fmt.Sprintf("not renewed for %s", c.View.LeaseAge.Round(100*time.Millisecond))
		}
		h.Failover = c.View.FailoverBlocked
		if c.View.WritesWaiting {
			h.WritesWait = true
		}
	}
	if h.Failover == "" && st.PrimaryID != "" && len(st.DataMembers()) == 2 && !hasWitness(st) {
		h.Failover = "Two data nodes and no witness: if the main node dies, no other node takes over by itself. Add a witness to make that automatic."
	}
	return h
}

// writesWaitingFor lists the copies in the sync set the main node waits
// for when fewer than k of them are connected (nil: writes are not held
// up, or it is not known here); mainSeen: the main node answered.
func writesWaitingFor(st cluster.State, s Status) (names []string, mainSeen bool) {
	var primary *MemberView
	for i := range s.Nodes {
		if s.Nodes[i].Primary {
			primary = &s.Nodes[i]
		}
	}
	if primary == nil || primary.Report == nil {
		return nil, false
	}
	var syncNames []string
	connected := 0
	for _, n := range s.Nodes {
		if n.InSyncSet && !n.Primary {
			syncNames = append(syncNames, n.Name)
			if strings.HasPrefix(n.Replication, "streaming") {
				connected++
			}
		}
	}
	if len(syncNames) > 0 && connected < max(st.Sync.K, 1) {
		return syncNames, true
	}
	return nil, true
}

func hasWitness(st cluster.State) bool {
	for _, m := range st.Members {
		if m.Role == cluster.RoleWitness {
			return true
		}
	}
	return false
}

func sinceText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " since " + t.Local().Format("15:04:05 Mon 2 Jan")
}

// warnings explains in plain words what needs attention (the problems'
// lines, then the warnings that have no code yet).
func warnings(st cluster.State, s Status, primary *NodeReport) []string {
	ps, rest := statusProblems(st, s, primary, "", time.Now())
	return append(summaries(ps), rest...)
}

// statusProblems: what needs attention in the cluster state, with codes;
// rest is what has no code yet (mixed systems).
func statusProblems(st cluster.State, s Status, primary *NodeReport, viewer string, now time.Time) (out []Problem, rest []string) {
	if !st.Bootstrapped() {
		return nil, nil // addressProblems explains (D62)
	}
	viewer = cmp.Or(viewer, "this node")
	add := func(code, node, summary, what string, since time.Time, evidence ...string) {
		p := newProblem(code, node, summary)
		p.Since, p.Evidence = since, evidence
		p.finish(what, viewer, now)
		out = append(out, p)
	}
	if s.Cluster.LeaderID == "" {
		var ev []string
		for _, n := range s.Nodes {
			if !n.This {
				ev = append(ev, fmt.Sprintf("%s (%s): %s from %s.", n.Name, n.Suffrage, map[bool]string{true: "answers", false: "no answer"}[n.Reachable], viewer))
			}
		}
		add(codeNoLeader, "", "The cluster has no leader right now: not enough voting nodes can reach each other. Nothing can change until they do.", "", time.Time{}, ev...)
	}
	for _, n := range s.Nodes {
		if n.Report != nil && n.Report.State == "needs-attention" {
			add(codeNeedsAttention, n.Name, fmt.Sprintf("%s: %s", n.Name, n.Report.Message), "", time.Time{}, "Its own report says: "+n.Report.Message)
		}
	}
	if st.PrimaryID == "" {
		add(codeNoMain, "", "No node is the main node right now.", "", time.Time{}, fmt.Sprintf("Cluster term (epoch) %d, as %s knows it.", st.Epoch, viewer))
	} else if primary == nil {
		add(codeMainUnreachable, s.Cluster.PrimaryName, fmt.Sprintf("The main node (%s) is not reachable from here.", s.Cluster.PrimaryName), "", time.Time{},
			fmt.Sprintf("%s could not get a status report from %s at %s.", viewer, s.Cluster.PrimaryName, strings.Join(st.Members[st.PrimaryID].Addresses, ", ")))
	}
	// Rule 9: a lost sync standby blocks writes until the controller
	// lets the main node carry on alone. Say so.
	if syncNames, _ := writesWaitingFor(st, s); len(syncNames) > 0 {
		msg := fmt.Sprintf("Writes are waiting: every write must reach a copy on %s, and not enough of them are connected. ", strings.Join(syncNames, " or "))
		switch {
		case st.Settings.SingleDataNodeMode == cluster.ModeStrict:
			msg += "This cluster is set to strict, so writes wait until a copy is back."
		case s.Cluster.LeaderID == "":
			msg += "Letting the main node carry on alone needs a leader, and the cluster has none right now, so writes wait until a copy or a leader is back."
		default:
			msg += fmt.Sprintf("If no copy is back within %s of losing it, the main node carries on alone.", st.Settings.DegradeAfter)
		}
		var since time.Time
		if c := s.Controller; c != nil && c.View.WritesWaitingFor > 0 {
			since = now.Add(-c.View.WritesWaitingFor)
		}
		var ev []string
		for _, n := range s.Nodes {
			if n.InSyncSet && !n.Primary {
				ev = append(ev, fmt.Sprintf("%s: %s.", n.Name, cmp.Or(n.Replication, "unknown")))
			}
		}
		ev = append(ev, fmt.Sprintf("Each write needs %d of them; mode %s.", max(st.Sync.K, 1), st.Settings.SingleDataNodeMode))
		add(codeWritesWaiting, s.Cluster.PrimaryName, msg, "", since, ev...)
	}
	if st.PrimaryID != "" && !st.StandbyEligible && len(st.DataMembers()) > 1 {
		add(codeOneCopy, s.Cluster.PrimaryName, "Running on one copy"+sinceText(st.StandbyEligibleSince)+": writes are saved on "+s.Cluster.PrimaryName+" only.", "",
			st.StandbyEligibleSince)
	}
	switch failoverCode(st, s) {
	case "waiting_lease":
		c := s.Controller
		add(codeWaitingLease, s.Cluster.PrimaryName, fmt.Sprintf("The main node (%s) has not renewed its lease for %s; another node takes over once it has surely run out.", s.Cluster.PrimaryName, c.View.LeaseAge.Round(100*time.Millisecond)),
			"", now.Add(-c.View.LeaseAge), fmt.Sprintf("Lease last renewed %s ago, as the leader (%s) sees it.", c.View.LeaseAge.Round(100*time.Millisecond), s.Cluster.LeaderName))
	case "no_eligible_copy":
		add(codeNoEligibleCopy, s.Cluster.PrimaryName, s.Controller.View.FailoverBlocked, "", time.Time{})
	case "two_nodes":
		add(codeTwoNodes, s.Cluster.PrimaryName, s.Controller.View.FailoverBlocked, "", time.Time{})
	}
	rest = append(rest, mixedWarnings(st)...)
	for _, n := range s.Nodes {
		switch {
		case !n.Reachable && !n.This:
			if n.Primary && primary == nil {
				continue // main_unreachable says it
			}
			add(codeNodeUnreachable, n.Name, fmt.Sprintf("%s is not reachable.", n.Name), "", time.Time{},
				fmt.Sprintf("%s could not get a status report from %s at %s.", viewer, n.Name, strings.Join(n.Addresses, ", ")))
		case n.Report != nil && n.Report.State == "waiting" && n.Report.Message != "":
			add(codeNodeWaiting, n.Name, fmt.Sprintf("%s: %s", n.Name, n.Report.Message), "", time.Time{}, "Its own report says: "+n.Report.Message)
		}
	}
	return out, rest
}

// failoverCode is the machine form of why the main node is not being
// replaced (dashboard health.failover_code).
func failoverCode(st cluster.State, s Status) string {
	data, witnesses := 0, 0
	for _, m := range st.Members {
		if m.Role == cluster.RoleData {
			data++
		} else {
			witnesses++
		}
	}
	twoNodes := data == 2 && witnesses == 0
	c := s.Controller
	switch {
	case s.Cluster.LeaderID == "":
		return "no_quorum"
	case c == nil:
	case c.View.FailoverBlocked != "" && twoNodes:
		return "two_nodes"
	case c.View.FailoverBlocked != "":
		return "no_eligible_copy"
	case !c.View.LeaseHealthy && st.PrimaryID != "":
		return "waiting_lease"
	case c.View.WritesWaiting && st.Settings.SingleDataNodeMode == cluster.ModeStrict:
		return "strict_waiting"
	}
	return ""
}
