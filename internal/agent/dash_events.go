package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/durable"
)

// The activity log (dashboard/API.md §7). Each agent records what it
// observes: cluster-wide changes when it sees them in the Raft state, and
// health changes from its own status checks. Kept in
// <data-dir>/logs/events.jsonl, the newest eventKeep entries.

// Event is one activity entry.
type Event struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	Severity string    `json:"severity"` // info, good, warning, problem
	Node     string    `json:"node,omitempty"`
	From     string    `json:"from,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Role     string    `json:"role,omitempty"`
	Message  string    `json:"message"`
}

const (
	eventKeep    = 2000
	eventMaxAge  = 30 * 24 * time.Hour
	eventsFile   = "events.jsonl"
	eventIDWidth = 12
)

type eventLog struct {
	path string
	log  *slog.Logger
	now  func() time.Time

	mu     sync.Mutex
	events []Event // oldest first
	next   uint64
	lines  int // lines in the file (rewritten when it doubles eventKeep)
	subs   map[chan Event]struct{}
}

func openEventLog(dir string, log *slog.Logger) *eventLog {
	l := &eventLog{path: filepath.Join(dir, eventsFile), log: log, now: time.Now, next: 1, subs: map[chan Event]struct{}{}}
	b, err := os.ReadFile(l.path)
	if err != nil && !os.IsNotExist(err) {
		log.Warn("read activity log", "err", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.ID == "" {
			continue // a torn last line after a power cut
		}
		l.lines++
		l.events = append(l.events, e)
		if n, err := strconv.ParseUint(e.ID, 10, 64); err == nil && n >= l.next {
			l.next = n + 1
		}
	}
	l.trimLocked()
	return l
}

func (l *eventLog) trimLocked() {
	cut := l.now().Add(-eventMaxAge)
	i := 0
	for i < len(l.events) && (len(l.events)-i > eventKeep || l.events[i].At.Before(cut)) {
		i++
	}
	l.events = slices.Clone(l.events[i:])
}

// add records events (ID and At are filled in).
func (l *eventLog) add(evs ...Event) {
	if len(evs) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var buf bytes.Buffer
	for i := range evs {
		e := &evs[i]
		e.ID = fmt.Sprintf("%0*d", eventIDWidth, l.next)
		l.next++
		if e.At.IsZero() {
			e.At = l.now().UTC().Truncate(time.Second)
		}
		b, _ := json.Marshal(e)
		buf.Write(b)
		buf.WriteByte('\n')
		l.events = append(l.events, *e)
		l.lines++
		for c := range l.subs {
			select {
			case c <- *e:
			default: // a slow stream misses it; it can page with GET /v1/events
			}
		}
	}
	l.trimLocked()
	if l.lines > 2*eventKeep {
		l.rewriteLocked()
		return
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		l.log.Warn("write activity log", "err", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(buf.Bytes()); err != nil {
		l.log.Warn("write activity log", "err", err)
	}
}

func (l *eventLog) rewriteLocked() {
	var buf bytes.Buffer
	for _, e := range l.events {
		b, _ := json.Marshal(e)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := durable.WriteFile(l.path, buf.Bytes(), 0o600); err != nil {
		l.log.Warn("rewrite activity log", "err", err)
		return
	}
	l.lines = len(l.events)
}

// page returns up to limit events older than before ("" = newest), newest
// first, and whether there are more.
func (l *eventLog) page(before string, limit int) ([]Event, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	end := len(l.events)
	if before != "" {
		end = 0
		for i, e := range l.events {
			if e.ID < before {
				end = i + 1
			}
		}
	}
	start := max(0, end-limit)
	out := make([]Event, 0, end-start)
	for i := end - 1; i >= start; i-- {
		out = append(out, l.events[i])
	}
	return out, start > 0
}

// since returns events newer than id, oldest first (at most max).
func (l *eventLog) since(id string, maxN int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, e := range l.events {
		if e.ID > id {
			out = append(out, e)
		}
	}
	if len(out) > maxN {
		out = out[len(out)-maxN:]
	}
	return out
}

func (l *eventLog) subscribe() (chan Event, func()) {
	c := make(chan Event, 64)
	l.mu.Lock()
	l.subs[c] = struct{}{}
	l.mu.Unlock()
	return c, func() {
		l.mu.Lock()
		delete(l.subs, c)
		l.mu.Unlock()
	}
}

// stateEvents is what changed between two copies of the cluster state, in
// plain words. switchKind says how a change of main node came about when
// this agent knows ("planned" while it runs a planned switch); otherwise
// it is "automatic".
func stateEvents(prev, cur cluster.State, switchKind string) []Event {
	var out []Event
	name := func(st cluster.State, id string) string {
		if m, ok := st.Members[id]; ok {
			return m.Name
		}
		return ""
	}
	for _, m := range cur.SortedMembers() {
		old, ok := prev.Members[m.ID]
		switch {
		case !ok:
			what := "a copy of the data"
			if m.Role == cluster.RoleWitness {
				what = "a witness"
			}
			out = append(out, Event{Kind: "member_joined", Severity: "good", Node: m.Name, Role: string(m.Role),
				Message: fmt.Sprintf("%s joined the cluster as %s.", m.Name, what)})
		case old.Name != m.Name:
			out = append(out, Event{Kind: "member_renamed", Severity: "info", Node: m.Name, From: old.Name,
				Message: fmt.Sprintf("%s is now called %s.", old.Name, m.Name)})
		}
		if ok && old.Status != m.Status && m.Status == cluster.StatusNeedsReclone {
			out = append(out, Event{Kind: "needs_reclone", Severity: "warning", Node: m.Name,
				Message: fmt.Sprintf("%s was away too long; it copies all the data again when it is back.", m.Name)})
		}
	}
	for _, m := range prev.SortedMembers() {
		if _, ok := cur.Members[m.ID]; !ok {
			out = append(out, Event{Kind: "member_removed", Severity: "info", Node: m.Name,
				Message: fmt.Sprintf("%s is no longer part of the cluster.", m.Name)})
		}
	}
	if cur.PrimaryID != prev.PrimaryID && cur.PrimaryID != "" {
		to, from := name(cur, cur.PrimaryID), name(prev, prev.PrimaryID)
		forced := len(cur.Forced) > len(prev.Forced)
		switch {
		case forced:
			out = append(out, Event{Kind: "forced_promote", Severity: "warning", Node: to, From: from, Reason: "forced",
				Message: fmt.Sprintf("%s was made the main node by force.", to)})
		case from == "":
			out = append(out, Event{Kind: "main_changed", Severity: "good", Node: to, Reason: switchKind,
				Message: fmt.Sprintf("%s is the main node.", to)})
		default:
			sev, msg := "warning", fmt.Sprintf("%s became the main node (%s stopped responding).", to, from)
			if switchKind == "planned" {
				sev, msg = "info", fmt.Sprintf("%s became the main node, as asked (it was %s).", to, from)
			}
			out = append(out, Event{Kind: "main_changed", Severity: sev, Node: to, From: from, Reason: switchKind, Message: msg})
		}
	}
	if prev.ClusterID != "" && prev.StandbyEligible != cur.StandbyEligible && len(cur.DataMembers()) > 1 {
		if !cur.StandbyEligible {
			out = append(out, Event{Kind: "one_copy_started", Severity: "warning", Node: name(cur, cur.PrimaryID),
				Message: fmt.Sprintf("Running on one copy: new writes are saved on %s only.", name(cur, cur.PrimaryID))})
		} else {
			out = append(out, Event{Kind: "one_copy_ended", Severity: "good",
				Message: "Writes are saved on more than one computer again."})
		}
	}
	if prev.ClusterID != "" && prev.Settings != cur.Settings {
		out = append(out, Event{Kind: "settings_changed", Severity: "info", Message: "Cluster settings were changed."})
	}
	for h, js := range cur.JoinSecrets {
		if old, ok := prev.JoinSecrets[h]; ok && !old.Used && js.Used && js.UsedBy == cancelledBy {
			out = append(out, Event{Kind: "invite_cancelled", Severity: "info", Message: "An unused invite code was cancelled; it can no longer be used."})
		}
		if _, ok := prev.JoinSecrets[h]; !ok && prev.ClusterID != "" {
			out = append(out, Event{Kind: "invite_created", Severity: "info",
				Message: fmt.Sprintf("An invite code was created (valid until %s).", js.ExpiresAt.UTC().Format("15:04 UTC"))})
		}
	}
	return out
}

// healthObs is the slice of a status check the activity log compares.
type healthObs struct {
	unreachable map[string]bool // name → not reachable
	blocked     bool
}

func observeHealth(s Status) healthObs {
	o := healthObs{unreachable: map[string]bool{}}
	for _, n := range s.Nodes {
		if !n.This && !n.Reachable {
			o.unreachable[n.Name] = true
		}
	}
	o.blocked = s.Controller != nil && s.Controller.View.WritesWaiting
	return o
}

func healthEvents(prev, cur healthObs, members []string) []Event {
	var out []Event
	for _, n := range members {
		switch {
		case cur.unreachable[n] && !prev.unreachable[n]:
			out = append(out, Event{Kind: "node_unreachable", Severity: "problem", Node: n, Message: n + " stopped responding."})
		case !cur.unreachable[n] && prev.unreachable[n]:
			out = append(out, Event{Kind: "node_back", Severity: "good", Node: n, Message: n + " is responding again."})
		}
	}
	switch {
	case cur.blocked && !prev.blocked:
		out = append(out, Event{Kind: "writes_blocked", Severity: "problem", Message: "Writes are waiting for a copy that is not connected."})
	case !cur.blocked && prev.blocked:
		out = append(out, Event{Kind: "writes_resumed", Severity: "good", Message: "Writes go through again."})
	}
	return out
}
