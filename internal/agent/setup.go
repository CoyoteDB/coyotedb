package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/invite"
	"coyotedb.com/coyote/internal/pg"
	"coyotedb.com/coyote/internal/pki"
)

// The first run from the browser (D50, dashboard/API.md §4).
//
// `coyote setup` (and the background service, on a data folder that is not
// in a cluster) binds the local control port before anything else and
// serves the dashboard's setup page behind the same guard as always
// (internal/control: loopback first; a session from a one-time sign-in
// link; Host, Origin and CSRF checks). The page says "create" or "join";
// the agent then does exactly what `coyote up` or `coyote join` does, on
// the same port, and from the moment the cluster is open the same port
// serves the whole API. A create or join that fails before this machine is
// a member is shown on the page (state "failed") and can be tried again;
// once it is a member, a failure ends the agent as before.

// Step ids and states (API.md §4).
const (
	stepContact  = "contact"
	stepDownload = "download"
	stepVerify   = "verify"
	stepPrepare  = "prepare"
	stepStart    = "start"
	stepCopy     = "copy"
	stepFollow   = "follow"

	stepPending = "pending"
	stepActive  = "active"
	stepDone    = "done"
	stepFailed  = "failed"
	stepSkipped = "skipped"
)

// SetupStep is one step of the first run.
type SetupStep struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Percent    *int   `json:"percent,omitempty"`
	BytesDone  *int64 `json:"bytes_done,omitempty"`
	BytesTotal *int64 `json:"bytes_total,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// SetupProgress is a create or join in progress (or just finished).
type SetupProgress struct {
	Operation string      `json:"operation"` // "create" or "join"
	Role      string      `json:"role"`
	StartedAt time.Time   `json:"started_at"`
	Done      bool        `json:"done"`
	Steps     []SetupStep `json:"steps"`
}

// setupTracker is what the setup page shows. Every method is safe on a nil
// tracker (agents built without one).
type setupTracker struct {
	mu      sync.Mutex
	state   string // fresh, creating, joining, member, failed
	role    string
	prog    *SetupProgress
	err     *errBody
	sawDL   bool
	changed chan struct{} // closed and replaced on every change
}

func newSetupTracker() *setupTracker {
	return &setupTracker{state: "fresh", changed: make(chan struct{})}
}

// changedLocked wakes every stream that waits for a change.
func (t *setupTracker) changedLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// wait returns a channel closed on the next change.
func (t *setupTracker) wait() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.changed
}

func stepsFor(op, role string) []SetupStep {
	var ids []string
	switch {
	case op == "create":
		ids = []string{stepDownload, stepVerify, stepPrepare, stepStart}
	case role == string(cluster.RoleWitness):
		ids = []string{stepContact, stepStart}
	default:
		// In the order they really happen: the program files first (the
		// cluster measures this machine before it lets it in).
		ids = []string{stepDownload, stepVerify, stepContact, stepCopy, stepFollow}
	}
	out := make([]SetupStep, len(ids))
	for i, id := range ids {
		out[i] = SetupStep{ID: id, State: stepPending}
	}
	return out
}

// tryStart claims the tracker for a create or join from the page. False
// while one runs, or when the machine is already a member.
func (t *setupTracker) tryStart(op, role string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != "fresh" && t.state != "failed" {
		return false
	}
	t.state = map[string]string{"create": "creating", "join": "joining"}[op]
	t.role, t.err, t.sawDL = role, nil, false
	t.prog = &SetupProgress{Operation: op, Role: role, StartedAt: time.Now().UTC().Truncate(time.Second), Steps: stepsFor(op, role)}
	t.changedLocked()
	return true
}

// begin is called by the agent once it knows what it does. A node that is
// already a member is "member"; a create or join not started from the page
// (the CLI, a service request) gets its progress here.
func (t *setupTracker) begin(lc lifecycle, role cluster.Role) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.role = string(role)
	if lc == lcStart {
		if t.state != "creating" && t.state != "joining" {
			t.state = "member"
		}
		t.changedLocked()
		return
	}
	op := "create"
	if lc == lcJoin {
		op = "join"
	}
	if t.prog == nil || t.prog.Operation != op {
		t.prog = &SetupProgress{Operation: op, Role: string(role), StartedAt: time.Now().UTC().Truncate(time.Second), Steps: stepsFor(op, string(role))}
	}
	t.prog.Role = string(role)
	t.state = map[string]string{"create": "creating", "join": "joining"}[op]
	t.changedLocked()
}

// setLocked changes one step; returns it (nil if the operation has no
// such step).
func (t *setupTracker) setLocked(id, state, detail string) *SetupStep {
	if t.prog == nil {
		return nil
	}
	for i := range t.prog.Steps {
		if t.prog.Steps[i].ID == id {
			s := &t.prog.Steps[i]
			if s.State == stepDone || s.State == stepSkipped {
				if state == stepActive {
					return s // never back from done
				}
			}
			s.State = state
			if detail != "" {
				s.Detail = detail
			}
			return s
		}
	}
	return nil
}

func (t *setupTracker) step(id, state, detail string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.setLocked(id, state, detail) != nil {
		t.changedLocked()
	}
}

func (t *setupTracker) downloadBytes(done, total int64) {
	if t == nil || total <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sawDL = true
	s := t.setLocked(stepDownload, stepActive, "")
	if s == nil {
		return
	}
	pct := int(done * 100 / total)
	if s.Percent != nil && *s.Percent == pct {
		return
	}
	s.Percent, s.BytesDone, s.BytesTotal = &pct, &done, &total
	t.changedLocked()
}

// downloaded: the program files are here and their checksums match.
func (t *setupTracker) downloaded() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sawDL {
		t.setLocked(stepDownload, stepDone, "")
	} else {
		t.setLocked(stepDownload, stepSkipped, "already on this computer")
	}
	t.setLocked(stepVerify, stepDone, "")
	t.changedLocked()
}

// clusterOpen: the cluster was created or joined.
func (t *setupTracker) clusterOpen(join bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.prog == nil || t.prog.Done {
		return
	}
	if join {
		t.setLocked(stepContact, stepDone, "")
		if t.role == string(cluster.RoleWitness) {
			t.setLocked(stepStart, stepDone, "")
			t.finishLocked()
		}
	} else {
		t.setLocked(stepPrepare, stepDone, "")
		t.setLocked(stepStart, stepActive, "")
	}
	t.changedLocked()
}

func (t *setupTracker) finishLocked() {
	t.prog.Done = true
	t.state = "member"
}

// watch follows the database's start (create) or copy and first stream
// (join) until the first run is done.
func (t *setupTracker) watch(ctx context.Context, a *Agent) {
	if t == nil {
		return
	}
	t.mu.Lock()
	active := t.prog != nil && !t.prog.Done
	t.mu.Unlock()
	if !active {
		return
	}
	go func() {
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			if t.check(a) {
				return
			}
		}
	}()
}

// check moves the steps on from what the agent is doing; true once the
// first run is done (or there is none).
func (t *setupTracker) check(a *Agent) bool {
	if t == nil {
		return true
	}
	srv, role := a.currentPG()
	phase, _, pct := a.phaseInfo()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.prog == nil || t.prog.Done {
		return true
	}
	changed := false
	switch t.prog.Operation {
	case "create":
		if srv != nil && role == "primary" {
			t.setLocked(stepStart, stepDone, "")
			t.finishLocked()
			changed = true
		}
	case "join":
		if phase == "cloning" {
			s := t.setLocked(stepCopy, stepActive, "")
			if s != nil && pct >= 0 && (s.Percent == nil || *s.Percent != pct) {
				p := pct
				s.Percent = &p
				changed = true
			}
		}
		if srv != nil && role == "standby" {
			t.setLocked(stepCopy, stepDone, "")
			t.setLocked(stepFollow, stepDone, "")
			t.finishLocked()
			changed = true
		}
	}
	if changed {
		t.changedLocked()
	}
	return t.prog.Done
}

// fail records a create or join that stopped before this machine became a
// member.
func (t *setupTracker) fail(err error) {
	if t == nil {
		return
	}
	code, msg, details := setupError(err)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = "failed"
	t.err = &errBody{Error: msg, Code: code, Details: details}
	if t.prog != nil {
		for i := range t.prog.Steps {
			if t.prog.Steps[i].State == stepActive {
				t.prog.Steps[i].State = stepFailed
			}
		}
	}
	t.changedLocked()
}

// failMsg records a failure known only by its words.
func (t *setupTracker) failMsg(code, msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state, t.err = "failed", &errBody{Error: msg, Code: code}
	t.changedLocked()
}

func (t *setupTracker) reset() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != "failed" {
		return false
	}
	t.state, t.prog, t.err, t.role = "fresh", nil, nil, ""
	t.changedLocked()
	return true
}

// view is GET /v1/setup.
func (t *setupTracker) view(name string) SetupView {
	v := SetupView{State: "member", SuggestedName: name, Platform: PlatformView{runtime.GOOS, runtime.GOARCH}}
	if t == nil {
		return v
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	v.State = t.state
	if t.role != "" {
		r := t.role
		v.Role = &r
	}
	if t.prog != nil {
		p := *t.prog
		p.Steps = append([]SetupStep(nil), t.prog.Steps...)
		v.Progress = &p
	}
	if t.err != nil {
		e := *t.err
		v.Error = &e
	}
	return v
}

// setupError is a failure's code and plain words for the setup page.
func setupError(err error) (code, msg string, details map[string]any) {
	msg = err.Error()
	var ee *ExitError
	if errors.As(err, &ee) {
		msg = ee.Msg
	} else if next := NextStep(err, ""); next != "" {
		msg = upperFirst(msg) + ". " + next
	}
	var ce *cluster.Error
	switch {
	case errors.As(err, &ce):
		code = ce.Code
	case errors.Is(err, invite.ErrNotInvite):
		code = "not_invite"
	case errors.Is(err, invite.ErrCorrupt):
		code = "invite_corrupt"
	case errors.Is(err, invite.ErrExpired):
		code = "invite_expired"
	case errors.Is(err, invite.ErrVersion):
		code = "invite_version"
	case errors.Is(err, pki.ErrFingerprint):
		code = "wrong_cluster"
	case addrInUse(err):
		code = "port_in_use"
	case diskFull(err):
		code = "disk_full"
	case errors.As(err, &ee) && ee.Cause != nil && isJoinNetErr(ee.Cause):
		code = "unreachable"
	default:
		code = "failed"
	}
	return code, msg, details
}

// isJoinNetErr: the join could not reach the cluster (not a refusal).
func isJoinNetErr(err error) bool {
	var ce *cluster.Error
	if errors.As(err, &ce) {
		return false
	}
	var ne net.Error
	var oe *net.OpError
	return errors.As(err, &ne) || errors.As(err, &oe) || errors.Is(err, context.DeadlineExceeded)
}

// ---- the port, served before the cluster ----

// earlyControl is the local control port served from before the node is a
// member: first the setup routes, then (serve) the whole API.
type earlyControl struct {
	srv      *control.Server
	sessions *sessionStore
	h        atomic.Pointer[http.Handler]
}

func (e *earlyControl) serve(h http.Handler) { e.h.Store(&h) }

func (e *earlyControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*e.h.Load()).ServeHTTP(w, r)
}

// setupRequest is what the page (or a service request file) asks for.
type setupRequest struct {
	svc      *ServiceRequest // from the file `up`/`join` leave for the service
	mode     Mode
	code     string
	name     string
	adv      []string
	mixed    bool
	fromPage bool
}

// runSetup serves the setup page until the machine is set up, then runs
// the node. Called with the node lock held.
func (a *Agent) runSetup(ctx context.Context) error {
	has, err := cluster.HasState(a.clusterDir())
	if err != nil {
		return err
	}
	pending, err := cluster.HasPendingJoin(a.clusterDir())
	if err != nil {
		return err
	}
	if has || (pending && !a.cfg.Wizard) {
		return a.runMember(ctx) // nothing to set up (the service resumes a join)
	}
	saved := a.st.get()
	a.ctlPort = cmp.Or(a.cfg.ControlPort, saved.ControlPort, control.DefaultPort)
	ln, err := a.listenControl()
	if err != nil {
		if a.cfg.Wizard {
			return err
		}
		// A service must not stop over a port the request may still
		// choose: wait for `up` / `join` without the setup page.
		a.log.Warn("setup page not served", "err", err)
		a.say("The setup page is not available (%v). Waiting for `%s up` (new cluster) or `%s join`; they can choose other ports.", err, brand.Command, brand.Command)
		req, err := a.awaitSetup(ctx, nil)
		if err != nil {
			return err
		}
		return a.attempt(req).runMember(ctx)
	}
	tok, err := control.EnsureToken(a.node.StateDir)
	if err != nil {
		ln.Close()
		return err
	}
	e := &earlyControl{sessions: newSessionStore(a.ctlPort)}
	reqs := make(chan setupRequest, 1)
	sd := a.newSetupDash(e.sessions, reqs)
	e.serve(sd)
	srv, err := control.Serve(ln, tok, e, a.log, a.controlOptions(e.sessions)...)
	if err != nil {
		ln.Close()
		return err
	}
	e.srv, a.early = srv, e
	closed := false
	closeEarly := func() {
		if !closed {
			closed = true
			control.RemoveInfo(a.node.StateDir)
			srv.Close()
		}
	}
	defer closeEarly()
	if err := control.WriteInfo(a.node.StateDir, control.Info{Addr: infoAddr(srv.Addr()), PID: os.Getpid()}); err != nil {
		return err
	}
	if pending {
		a.setup.failMsg("unfinished_join", "This computer started joining a cluster but did not finish. Join again with the same invite code to finish, or start over.")
	}
	if a.cfg.Wizard {
		a.announceSetup(sd)
	} else {
		a.log.Info("waiting for a request", "file", ServiceRequestPath(a.node.StateDir), "control", srv.Addr())
		a.say("This machine is not in a cluster yet. Waiting for `%s up` (new cluster), `%s join`, or the setup page (`%s setup`).", brand.Command, brand.Command, brand.Command)
	}
	for {
		req, err := a.awaitSetup(ctx, reqs)
		if err != nil {
			return err
		}
		if r := req.svc; r != nil && r.ControlPort != 0 && r.ControlPort != a.ctlPort {
			// `up --control-port N` with a service: the node serves on
			// N from now on, so the page's port is given up.
			a.say("Moving the local control port from %d to %d, as asked.", a.ctlPort, r.ControlPort)
			closeEarly()
			a.early = nil
		}
		b := a.attempt(req)
		err = b.runMember(ctx)
		if err == nil || ctx.Err() != nil {
			return err
		}
		member, _ := cluster.HasState(a.clusterDir())
		if member || !req.fromPage {
			return err // as `up` / `join` always did
		}
		a.log.Warn("setup from the page failed", "err", err)
		a.setup.fail(err)
		e.serve(sd)
		_, msg, _ := setupError(err)
		a.say("Setup did not finish: %s", msg)
		a.say("Nothing is running yet. Fix that and try again on the setup page.")
	}
}

// announceSetup prints (and opens) a one-time sign-in link to the page.
func (a *Agent) announceSetup(d *dashServer) {
	link, _, err := d.loginLink("127.0.0.1")
	if err != nil {
		a.say("Could not make a sign-in link (%v). Run `%s dashboard` for one.", err, brand.Command)
		return
	}
	opened := a.cfg.OpenLink != nil && a.cfg.OpenLink(link)
	a.say("This computer is not in a cluster yet. Finish setting it up in your browser:")
	if opened {
		a.say("  The setup page is opening. If it does not, open this link on this computer")
		a.say("  (it works once, for 2 minutes; `%s dashboard --data-dir %s` makes a new one):", brand.Command, quoteArg(a.node.StateDir))
	} else {
		a.say("  Open this link in a browser on this computer (it works once, for 2 minutes;")
		a.say("  `%s dashboard --data-dir %s` makes a new one):", brand.Command, quoteArg(a.node.StateDir))
	}
	a.say("\n  %s\n", link)
	a.say("Leave this window open while you use the page; Ctrl-C stops it.")
}

// awaitSetup waits for the page's request or the service's request file.
func (a *Agent) awaitSetup(ctx context.Context, reqs <-chan setupRequest) (setupRequest, error) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if a.cfg.Service {
			has, err := cluster.HasState(a.clusterDir())
			if err != nil {
				return setupRequest{}, err
			}
			if has {
				return setupRequest{svc: &ServiceRequest{Mode: "up"}}, nil
			}
			r, err := readServiceRequest(a.node.StateDir)
			switch {
			case err == nil:
				op, role := "create", string(cluster.RoleData)
				switch r.Mode {
				case "join":
					op = "join"
				case "witness":
					op, role = "join", string(cluster.RoleWitness)
				}
				if !a.setup.tryStart(op, role) {
					a.say("A request in %s arrived while the setup page is busy; it was ignored.", ServiceRequestPath(a.node.StateDir))
					break
				}
				return setupRequest{svc: &r}, nil
			case !errors.Is(err, fs.ErrNotExist):
				a.log.Warn("unusable service request", "err", err)
				a.say("Could not read the request in %s: %v", ServiceRequestPath(a.node.StateDir), err)
			}
		}
		select {
		case <-ctx.Done():
			return setupRequest{}, ctx.Err()
		case r := <-reqs:
			return r, nil
		case <-tick.C:
		}
	}
}

// attempt is a fresh agent that does what req asks, on this agent's lock,
// state file, port and setup page.
func (a *Agent) attempt(req setupRequest) *Agent {
	cfg := a.cfg
	cfg.Service, cfg.Wizard = false, false
	if r := req.svc; r != nil {
		b := newAgent(cfg, a.node)
		b.applyServiceRequest(*r)
		if a.early != nil {
			b.cfg.ControlPort = a.ctlPort // the page's port serves on
		}
		b.share(a)
		return b
	}
	cfg.Mode, cfg.Invite, cfg.AllowMixedOS = req.mode, req.code, req.mixed
	if req.name != "" {
		cfg.Name = req.name
	}
	if len(req.adv) > 0 {
		cfg.Advertise = req.adv
	}
	if a.early != nil {
		cfg.ControlPort = a.ctlPort
	}
	b := newAgent(cfg, a.node)
	b.share(a)
	return b
}

func (b *Agent) share(a *Agent) {
	b.lock, b.st, b.early, b.setup = a.lock, a.st, a.early, a.setup
}

// ---- the setup routes ----

// newSetupDash is the API before the node is a member: sign-in, version,
// setup, addresses, invite check, create, join, reset and the stream.
// Every other /v1 route answers 409 not_set_up.
func (a *Agent) newSetupDash(s *sessionStore, reqs chan<- setupRequest) *dashServer {
	d := &dashServer{a: a, sessions: s, static: newDashboardStatic(), lastSeen: map[string]time.Time{}}
	p := control.Prefix
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+p+"/session/links", d.apiSessionLink)
	mux.HandleFunc("POST "+p+"/session", d.apiSessionCreate)
	mux.HandleFunc("GET "+p+"/session", d.apiSessionGet)
	mux.HandleFunc("DELETE "+p+"/session", d.apiSessionDelete)
	mux.HandleFunc("GET "+p+"/version", d.apiVersion)
	mux.HandleFunc("GET "+p+"/setup", d.apiSetup)
	mux.HandleFunc("GET "+p+"/addresses", d.apiAddresses)
	mux.HandleFunc("POST "+p+"/setup/check-invite", d.apiCheckInvite)
	mux.HandleFunc("POST "+p+"/setup/create", func(w http.ResponseWriter, r *http.Request) { d.apiSetupStart(w, r, "create", reqs) })
	mux.HandleFunc("POST "+p+"/setup/join", func(w http.ResponseWriter, r *http.Request) { d.apiSetupStart(w, r, "join", reqs) })
	mux.HandleFunc("POST "+p+"/setup/reset", d.apiSetupReset)
	mux.HandleFunc("POST "+p+"/backups/check-destination", a.apiBackupCheckDestination) // the setup page's backup step (backup_jobs.go)
	mux.HandleFunc("GET "+p+"/stream", d.apiSetupStream)
	mux.HandleFunc(p+"/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusConflict, "not_set_up", fmt.Sprintf("this computer is not in a cluster yet; finish the setup page first (or run `%s up` / `%s join`)", brand.Command, brand.Command), nil)
	})
	d.mux = mux
	return d
}

// SetupCreateRequest is POST /v1/setup/create.
type SetupCreateRequest struct {
	Name        string `json:"name"`
	Advertise   string `json:"advertise"`
	ClusterName string `json:"cluster_name,omitempty"`
}

// SetupJoinRequest is POST /v1/setup/join.
type SetupJoinRequest struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Advertise string `json:"advertise"`
	Role      string `json:"role,omitempty"`
	// AllowMixedOS is `join --allow-mixed-os` (D37).
	AllowMixedOS bool `json:"allow_mixed_os,omitempty"`
}

func (d *dashServer) apiSetupStart(w http.ResponseWriter, r *http.Request, op string, reqs chan<- setupRequest) {
	var (
		req  setupRequest
		name string
		adv  string
		role = string(cluster.RoleData)
	)
	req.fromPage = true
	if op == "create" {
		var b SetupCreateRequest
		if err := control.ReadJSON(r, &b); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
			return
		}
		if b.ClusterName != "" {
			writeErr(w, http.StatusNotImplemented, "not_yet_available", "Naming the cluster is not available yet; leave the name empty.", nil)
			return
		}
		name, adv, req.mode = b.Name, b.Advertise, ModeUp
	} else {
		var b SetupJoinRequest
		if err := control.ReadJSON(r, &b); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
			return
		}
		inv, err := invite.Decode(b.Code)
		if err == nil {
			err = inv.Check(time.Now())
		}
		if err != nil {
			writeInviteErr(w, inv, err)
			return
		}
		role = cmpOr(b.Role, string(cluster.RoleData))
		switch role {
		case string(cluster.RoleData):
			req.mode = ModeJoin
		case string(cluster.RoleWitness):
			req.mode = ModeWitness
		default:
			writeErr(w, http.StatusBadRequest, "bad_request", `role must be "data" or "witness"`, nil)
			return
		}
		req.code, req.mixed = invite.Normalize(b.Code), b.AllowMixedOS && role == string(cluster.RoleData)
		name, adv = b.Name, b.Advertise
	}
	if name != "" {
		if err := pg.ValidName(name); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
			return
		}
		req.name = name
	}
	if adv != "" {
		hp, err := d.advertiseHostPort(adv)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
			return
		}
		req.adv = []string{hp}
	}
	if !d.a.setup.tryStart(op, role) {
		writeErr(w, http.StatusConflict, "already_set_up", "this computer is already being set up, or is set up", nil)
		return
	}
	select {
	case reqs <- req:
	default: // cannot happen: tryStart admits one at a time
		d.a.setup.failMsg("failed", "another request was still waiting; try again")
		writeErr(w, http.StatusConflict, "already_set_up", "another request is still waiting", nil)
		return
	}
	control.WriteJSON(w, http.StatusAccepted, d.setupView())
}

// advertiseHostPort turns the page's "address other computers use" (an IP
// or host name, optionally with a port) into HOST:PORT on the cluster port.
func (d *dashServer) advertiseHostPort(s string) (string, error) {
	if h, _, err := net.SplitHostPort(s); err == nil {
		if !validHost(h) {
			return "", errors.New("the address must be an IP address or a host name")
		}
		return s, validHostPort(s)
	}
	if !validHost(s) {
		return "", errors.New("the address must be an IP address or a host name (letters, digits, dots and dashes)")
	}
	listen := firstNonEmpty(d.a.cfg.Listen, d.a.st.get().Listen, DefaultListen)
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", err
	}
	if s == "" || len(s) > 253 {
		return "", errors.New("the address must be an IP address or a host name")
	}
	hp := net.JoinHostPort(s, port)
	return hp, validHostPort(hp)
}

// validHost: an IP address or a DNS host name.
func validHost(h string) bool {
	if net.ParseIP(h) != nil {
		return true
	}
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

func (d *dashServer) apiSetupReset(w http.ResponseWriter, r *http.Request) {
	dir := d.a.clusterDir()
	if has, _ := cluster.HasState(dir); has {
		writeErr(w, http.StatusConflict, "not_failed", "this computer is a member of a cluster; there is nothing to start over", nil)
		return
	}
	if v := d.a.setup.view(""); v.State != "failed" {
		writeErr(w, http.StatusConflict, "not_failed", "there is no failed setup to start over", nil)
		return
	}
	// A join that did not finish keeps a reserved node key; starting over
	// forgets it (the invite it used is then spent or expires unused).
	if pending, _ := cluster.HasPendingJoin(dir); pending {
		if err := cluster.ClearPendingJoin(dir); err != nil {
			apiErr(w, fmt.Errorf("could not forget the unfinished join: %w", err))
			return
		}
	}
	d.a.setup.reset()
	control.WriteJSON(w, http.StatusOK, d.setupView())
}

// apiSetupStream is GET /v1/stream before the node is a member: a setup
// event at once and on every change (at most four a second).
func (d *dashServer) apiSetupStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "failed", "streaming is not supported here", nil)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("retry: 3000\n\n")); err != nil {
		return
	}
	ping := time.NewTicker(streamPing)
	defer ping.Stop()
	for {
		ch := d.a.setup.wait()
		b, err := jsonLine(d.setupView())
		if err != nil {
			return
		}
		if _, err := w.Write([]byte("event: setup\ndata: " + b + "\n\n")); err != nil {
			return
		}
		fl.Flush()
		for wait := true; wait; {
			select {
			case <-r.Context().Done():
				return
			case <-ch:
				wait = false
			case <-ping.C:
				if _, err := w.Write([]byte(": ping\n\n")); err != nil {
					return
				}
				fl.Flush()
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func jsonLine(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// loginLink mints a one-time sign-in link for host.
func (d *dashServer) loginLink(host string) (string, time.Time, error) {
	tok, exp, err := d.sessions.mintLogin()
	if err != nil {
		return "", time.Time{}, err
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(d.sessions.port)) + "/login#t=" + tok, exp, nil
}
