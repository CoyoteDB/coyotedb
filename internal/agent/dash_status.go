package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/controller"
)

// DashStatus is GET /v1/status: Status with dashboard/API.md's additive
// fields. Old clients (the CLI decodes into Status) ignore them.
type DashStatus struct {
	Self       string             `json:"self"`
	Cluster    DashCluster        `json:"cluster"`
	Nodes      []DashMember       `json:"nodes"`
	Warnings   []string           `json:"warnings"`
	Controller *controller.Report `json:"controller,omitempty"`
	Health     DashHealth         `json:"health"`
	// Waiting: no copy of the cluster state yet (Status.Waiting, D62).
	Waiting *WaitingView `json:"waiting,omitempty"`
	// Backup: Status.Backup (D63).
	Backup *BackupView `json:"backup,omitempty"`
	// Problems: Status.Problems (D66).
	Problems []Problem `json:"problems"`
}

// DashCluster adds name, platform and stale to ClusterView.
type DashCluster struct {
	ClusterView
	Name     string        `json:"name"`
	Platform *PlatformView `json:"platform"`
	// Stale: this node cannot reach a leader; the members shown are its
	// last known copy of the cluster state.
	Stale bool `json:"stale"`
}

// DashMember adds the dashboard's fields to MemberView.
type DashMember struct {
	MemberView
	Platform       PlatformView `json:"platform"`
	JoinedAt       time.Time    `json:"joined_at,omitzero"`
	LastSeen       *time.Time   `json:"last_seen"`
	LagSeconds     *float64     `json:"lag_seconds"`
	Eligible       bool         `json:"eligible"`
	EligibleReason string       `json:"eligible_reason,omitempty"`
	AgentVersion   string       `json:"agent_version,omitempty"`
}

// DashHealth adds the machine-readable fields to Health.
type DashHealth struct {
	Health
	FailoverCode       string      `json:"failover_code"`
	WritesBlocked      bool        `json:"writes_blocked"`
	WritesBlockedSince time.Time   `json:"writes_blocked_since,omitzero"`
	DegradeInMS        *int64      `json:"degrade_in_ms,omitempty"`
	Switching          *Switching  `json:"switching"`
	Promote            PromoteInfo `json:"promote"`
}

// Switching is a switch-over or failover in progress.
type Switching struct {
	Kind         string    `json:"kind"` // planned, automatic, forced
	From         string    `json:"from"`
	To           string    `json:"to"`
	StartedAt    time.Time `json:"started_at"`
	Step         string    `json:"step"`
	TakeoverInMS *int64    `json:"takeover_in_ms,omitempty"`
}

// PromoteInfo: whether a forced promote of this node is offered. It never
// is: there is no forced promote in the program yet (D36).
type PromoteInfo struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// statusMaxAge: streams and the activity recorder share one status check
// at most this often (each check asks every member over the network).
const statusMaxAge = time.Second

// status returns a status no older than maxAge (0: fresh).
func (d *dashServer) status(ctx context.Context, maxAge time.Duration) DashStatus {
	d.statusMu.Lock()
	if maxAge > 0 && d.statusVal != nil && time.Since(d.statusAt) < maxAge {
		v := *d.statusVal
		d.statusMu.Unlock()
		return v
	}
	d.statusMu.Unlock()

	s := d.a.clusterStatus(ctx)
	now := time.Now()
	d.swMu.Lock()
	var sw *Switching
	if d.switching != nil {
		c := *d.switching
		sw = &c
	}
	d.swMu.Unlock()

	d.statusMu.Lock()
	defer d.statusMu.Unlock()
	for _, n := range s.Nodes {
		if n.Reachable || n.This {
			d.lastSeen[n.ID] = now
		}
	}
	v := extendStatus(d.a.cn.State(), s, d.lastSeen, d.a.cfg.Version, sw, now)
	d.statusVal, d.statusAt = &v, now
	return v
}

func (d *dashServer) cachedStatus() *DashStatus {
	d.statusMu.Lock()
	defer d.statusMu.Unlock()
	return d.statusVal
}

func (d *dashServer) invalidateStatus() {
	d.statusMu.Lock()
	d.statusVal = nil
	d.statusMu.Unlock()
}

// extendStatus is pure: Status plus what the dashboard needs.
func extendStatus(st cluster.State, s Status, lastSeen map[string]time.Time, version string, planned *Switching, now time.Time) DashStatus {
	out := DashStatus{Self: s.Self, Warnings: s.Warnings, Controller: s.Controller, Waiting: s.Waiting, Backup: s.Backup, Problems: s.Problems}
	if out.Problems == nil {
		out.Problems = []Problem{}
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	out.Cluster = DashCluster{ClusterView: s.Cluster, Platform: clusterPlatform(st), Stale: s.Cluster.LeaderID == ""}
	if out.Cluster.SyncSet == nil {
		out.Cluster.SyncSet = []string{}
	}
	var primary *MemberView
	for i := range s.Nodes {
		if s.Nodes[i].Primary {
			primary = &s.Nodes[i]
		}
	}
	out.Nodes = []DashMember{}
	for _, n := range s.Nodes {
		m := st.Members[n.ID]
		dm := DashMember{MemberView: n, Platform: PlatformView{OS: m.Platform.OS, Arch: m.Platform.Arch}, JoinedAt: m.JoinedAt.UTC()}
		if dm.Addresses == nil {
			dm.Addresses = []string{}
		}
		if t, ok := lastSeen[n.ID]; ok {
			t := t.UTC().Truncate(time.Second)
			dm.LastSeen = &t
		}
		if n.This {
			dm.AgentVersion = version
		}
		dm.Eligible, dm.EligibleReason = eligibility(st, n)
		if n.Role == string(cluster.RoleData) && !n.Primary && primary != nil && n.LagBytes == 0 {
			zero := 0.0
			dm.LagSeconds = &zero
		}
		out.Nodes = append(out.Nodes, dm)
	}
	out.Health = extendHealth(st, s, planned, now)
	return out
}

// eligibility mirrors the checks of a planned switch (runSwitch with
// planned = true) so the dashboard offers only copies the switch accepts.
// The switch itself checks again; this is only the offer.
func eligibility(st cluster.State, n MemberView) (bool, string) {
	if n.Role != string(cluster.RoleData) || n.Primary {
		return false, ""
	}
	switch {
	case n.Status != cluster.StatusActive:
		return false, "It needs a fresh copy of the data first."
	case !n.Reachable:
		return false, "It is not responding."
	case !st.StandbyEligible:
		return false, "The cluster is running on one copy: wait until the copies have caught up."
	case !slices.Contains(st.Sync.Members, n.ID):
		return false, "It is not a full copy yet: it is still catching up."
	case n.Report == nil || n.Report.PG == nil || !n.Report.PG.Running || n.Report.PG.Role != "standby" || n.Report.Marker != "":
		return false, "It is not running as a healthy copy right now."
	case !strings.HasPrefix(n.Replication, "streaming"):
		return false, "It is not connected to the main node."
	}
	return true, ""
}

func extendHealth(st cluster.State, s Status, planned *Switching, now time.Time) DashHealth {
	h := DashHealth{Health: s.Health, Switching: planned}
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
	h.FailoverCode = failoverCode(st, s)
	if c != nil && c.View.WritesWaiting {
		h.WritesBlocked = true
		if c.View.WritesWaitingFor > 0 {
			h.WritesBlockedSince = now.Add(-c.View.WritesWaitingFor).UTC().Truncate(time.Second)
		}
		if st.Settings.SingleDataNodeMode == cluster.ModeDegradeAfter {
			left := max(st.Settings.DegradeAfter-c.View.WritesWaitingFor, 0).Milliseconds()
			h.DegradeInMS = &left
		}
	}
	// A main node that stopped renewing its lease, with a takeover coming
	// (not blocked): the controller waits out the full lease plus margin.
	if h.Switching == nil && h.FailoverCode == "waiting_lease" && !twoNodes {
		left := max(controller.LeaseLimit(st, controller.DefaultMargin)-c.View.LeaseAge, 0).Milliseconds()
		h.Switching = &Switching{Kind: "automatic", From: s.Cluster.PrimaryName, StartedAt: now.Add(-c.View.LeaseAge).UTC().Truncate(time.Second),
			Step: "waiting_lease", TakeoverInMS: &left}
	}
	h.Promote = PromoteInfo{Allowed: false}
	if twoNodes && h.FailoverCode == "two_nodes" {
		h.Promote.Reason = "Making a computer the main node by force is not available yet."
	}
	return h
}

// ---- activity ----

func (d *dashServer) apiEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeErr(w, http.StatusBadRequest, "bad_request", "limit must be a number from 1 to 200", nil)
			return
		}
		limit = n
	}
	evs, more := d.events.page(r.URL.Query().Get("before"), limit)
	control.WriteJSON(w, http.StatusOK, map[string]any{"events": evs, "more": more})
}

// recordEvery: the activity recorder's own status check (cheap when a
// dashboard stream already keeps the shared status fresh).
const recordEvery = 10 * time.Second

// record writes activity events until ctx ends.
func (d *dashServer) record(ctx context.Context) {
	sub := d.a.cn.Subscribe()
	defer sub.Close()
	prev := d.a.cn.State()
	d.events.add(Event{Kind: "agent_started", Severity: "info", Node: d.a.name,
		Message: fmt.Sprintf("CoyoteDB started on %s.", d.a.name)})
	var obs *healthObs
	tick := time.NewTicker(recordEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.C:
			cur := d.a.cn.State()
			kind := "automatic"
			d.swMu.Lock()
			if d.switching != nil {
				kind = "planned"
			}
			d.swMu.Unlock()
			d.events.add(stateEvents(prev, cur, kind)...)
			prev = cur
			d.invalidateStatus()
		case <-tick.C:
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			s := d.status(cctx, recordEvery/2)
			cancel()
			if ctx.Err() != nil {
				return
			}
			o := observeHealth(Status{Nodes: dashNodesToViews(s.Nodes), Controller: s.Controller})
			if obs != nil {
				var names []string
				for _, n := range s.Nodes {
					names = append(names, n.Name)
				}
				d.events.add(healthEvents(*obs, o, names)...)
			}
			obs = &o
		}
	}
}

func dashNodesToViews(ns []DashMember) []MemberView {
	out := make([]MemberView, len(ns))
	for i, n := range ns {
		out[i] = n.MemberView
	}
	return out
}

// ---- live stream ----

const (
	streamStatusEvery = time.Second
	streamStatusMax   = 10 * time.Second
	streamPing        = 15 * time.Second
)

// apiStream is GET /v1/stream (Server-Sent Events, API.md §6).
func (d *dashServer) apiStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "failed", "streaming is not supported here", nil)
		return
	}
	topics := map[string]bool{"status": true, "setup": true, "activity": true}
	if t := r.URL.Query().Get("topics"); t != "" {
		topics = map[string]bool{}
		for _, x := range strings.Split(t, ",") {
			topics[strings.TrimSpace(x)] = true
		}
	}
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(event, id string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return false
		}
		var sb strings.Builder
		sb.WriteString("event: " + event + "\n")
		if id != "" {
			sb.WriteString("id: " + id + "\n")
		}
		sb.WriteString("data: ")
		sb.Write(b)
		sb.WriteString("\n\n")
		if _, err := w.Write([]byte(sb.String())); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if _, err := w.Write([]byte("retry: 3000\n\n")); err != nil {
		return
	}
	if topics["setup"] && !send("setup", "", d.setupView()) {
		return
	}
	var evs chan Event
	if topics["activity"] {
		c, cancel := d.events.subscribe()
		defer cancel()
		evs = c
		if last := r.Header.Get("Last-Event-ID"); last != "" {
			for _, e := range d.events.since(last, 200) {
				if !send("activity", e.ID, e) {
					return
				}
			}
		}
	}
	var lastJSON []byte
	var lastSent time.Time
	statusTick := time.NewTicker(streamStatusEvery)
	defer statusTick.Stop()
	ping := time.NewTicker(streamPing)
	defer ping.Stop()
	sendStatus := func() bool {
		s := d.status(ctx, streamStatusEvery)
		if ctx.Err() != nil {
			return false
		}
		b, _ := json.Marshal(statusForCompare(s))
		if lastJSON != nil && string(b) == string(lastJSON) && time.Since(lastSent) < streamStatusMax {
			return true
		}
		lastJSON, lastSent = b, time.Now()
		return send("status", "", s)
	}
	if topics["status"] && !sendStatus() {
		return
	}
	// Setup changes while a first run finishes (copy, first stream).
	var setupCh <-chan struct{}
	if topics["setup"] && d.a.setup != nil {
		setupCh = d.a.setup.wait()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-setupCh:
			setupCh = d.a.setup.wait()
			if !send("setup", "", d.setupView()) {
				return
			}
		case <-statusTick.C:
			if setupCh != nil {
				d.a.setup.check(d.a) // move the steps on without waiting for the tracker's tick
			}
			if topics["status"] && !sendStatus() {
				return
			}
		case e := <-evs:
			if !send("activity", e.ID, e) {
				return
			}
		case <-ping.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// statusForCompare drops fields that change on every check (ages, LSNs
// moving by a few bytes are real changes and stay), so an idle cluster
// does not resend its status every second.
func statusForCompare(s DashStatus) DashStatus {
	if s.Controller != nil {
		c := *s.Controller
		c.View.LeaseAge = 0
		c.View.WritesWaitingFor = 0
		s.Controller = &c
	}
	nodes := make([]DashMember, len(s.Nodes))
	for i, n := range s.Nodes {
		n.LastSeen = nil
		if n.Report != nil {
			r := *n.Report
			r.QuorumContactAgoMS = nil
			r.Peers = nil
			r.Proxy = nil
			n.Report = &r
		}
		nodes[i] = n
	}
	s.Nodes = nodes
	if s.Health.Switching != nil {
		sw := *s.Health.Switching
		sw.TakeoverInMS = nil
		s.Health.Switching = &sw
	}
	s.Health.DegradeInMS = nil
	s.Health.Lease = ""
	return s
}
