package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/invite"
	"coyotedb.com/coyote/internal/netdetect"
	"coyotedb.com/coyote/internal/pg"
)

// The dashboard's side of the local API (dashboard/API.md, D36). It is
// served on the same loopback port as the CLI's control channel, behind
// the same guard (internal/control): loopback only, then the bearer token
// or a session cookie with Host, Origin and CSRF checks.
//
// Every action goes through the handler the CLI uses (switch, invites,
// remove, leave, settings, app password, addresses), so the dashboard
// passes exactly the same safety checks. There is no other way to change
// the main node here.

// APIVersion is dashboard/API.md's version.
const APIVersion = 1

// dashServer is the whole HTTP surface of the agent's local port.
type dashServer struct {
	a        *Agent
	sessions *sessionStore
	events   *eventLog
	static   *staticHandler
	mux      *http.ServeMux

	statusMu  sync.Mutex
	statusAt  time.Time
	statusVal *DashStatus
	lastSeen  map[string]time.Time // member id → last answer

	swMu      sync.Mutex
	switching *Switching // a planned switch this agent is running
}

// newDash builds the local API and starts the activity recorder (until ctx
// ends). It is the handler for control.Serve, and d.sessions its
// control.WithBrowserSession validator.
func (a *Agent) newDash(ctx context.Context, sessions *sessionStore) *dashServer {
	if sessions == nil {
		sessions = newSessionStore(a.ctlPort)
	}
	d := &dashServer{
		a:        a,
		sessions: sessions,
		events:   openEventLog(a.node.LogDir(), a.log),
		static:   newDashboardStatic(),
		lastSeen: map[string]time.Time{},
	}
	d.mux = a.controlHandler(d)
	go d.record(ctx)
	return d
}

func (d *dashServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == control.Prefix || strings.HasPrefix(r.URL.Path, control.Prefix+"/") {
		securityHeaders(w.Header(), dashCSP)
		w.Header().Set("Cache-Control", "no-store")
		d.mux.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "bad_request", "only GET is served outside /v1", nil)
		return
	}
	d.static.ServeHTTP(w, r)
}

// dashRoutes adds the dashboard's routes to the control mux.
func (d *dashServer) routes(mux *http.ServeMux) {
	p := control.Prefix
	mux.HandleFunc("POST "+p+"/session/links", d.apiSessionLink)
	mux.HandleFunc("POST "+p+"/session", d.apiSessionCreate)
	mux.HandleFunc("GET "+p+"/session", d.apiSessionGet)
	mux.HandleFunc("DELETE "+p+"/session", d.apiSessionDelete)
	mux.HandleFunc("GET "+p+"/version", d.apiVersion)
	mux.HandleFunc("GET "+p+"/setup", d.apiSetup)
	mux.HandleFunc("GET "+p+"/addresses", d.apiAddresses)
	mux.HandleFunc("GET "+p+"/problem-codes", func(w http.ResponseWriter, r *http.Request) {
		// The status problems' codes and texts (D66): the same as `coyote explain`.
		control.WriteJSON(w, http.StatusOK, map[string]any{"codes": ProblemCodes()})
	})
	mux.HandleFunc("POST "+p+"/setup/check-invite", d.apiCheckInvite)
	mux.HandleFunc("POST "+p+"/setup/create", d.apiAlreadySetUp)
	mux.HandleFunc("POST "+p+"/setup/join", d.apiAlreadySetUp)
	mux.HandleFunc("POST "+p+"/setup/reset", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusConflict, "not_failed", "this computer is a working member of a cluster; there is nothing to start over", nil)
	})
	mux.HandleFunc("GET "+p+"/status", func(w http.ResponseWriter, r *http.Request) {
		control.WriteJSON(w, http.StatusOK, d.status(r.Context(), 0))
	})
	mux.HandleFunc("GET "+p+"/stream", d.apiStream)
	mux.HandleFunc("GET "+p+"/events", d.apiEvents)
	mux.HandleFunc("POST "+p+"/invites", d.apiInvite)
	mux.HandleFunc("GET "+p+"/invites", d.apiInvites)
	mux.HandleFunc("DELETE "+p+"/invites/{id}", d.apiCancelInvite)
	mux.HandleFunc("PATCH "+p+"/members/{node}", d.notYet("Renaming a computer is not available yet."))
	mux.HandleFunc("POST "+p+"/switch", d.apiSwitch)
	mux.HandleFunc("POST "+p+"/promote", d.notYet("Making a computer the main node by force is not available yet."))
	mux.HandleFunc("GET "+p+"/settings", func(w http.ResponseWriter, r *http.Request) {
		control.WriteJSON(w, http.StatusOK, dashSettings(d.a.cn.State().Settings))
	})
	mux.HandleFunc("POST "+p+"/settings", d.apiSetSettings)
	mux.HandleFunc("GET "+p+"/node", d.apiNode)
	mux.HandleFunc("PATCH "+p+"/node", d.apiSetNode)
	mux.HandleFunc("GET "+p+"/connection", d.apiConnection)
	mux.HandleFunc("POST "+p+"/connection/reveal", d.apiReveal)
	d.backupRoutes(mux, p) // backup_api.go (D63)
	for _, route := range []string{"GET /agents", "POST /agents/keys", "DELETE /agents/keys/{id}"} {
		m, path, _ := strings.Cut(route, " ")
		mux.HandleFunc(m+" "+p+path, d.notYetM7("Coding agent access arrives in a later version."))
	}
}

// ---- errors ----

type errBody struct {
	Error   string         `json:"error"`
	Code    string         `json:"code"`
	Details map[string]any `json:"details,omitempty"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string, details map[string]any) {
	control.WriteJSON(w, status, errBody{Error: msg, Code: code, Details: details})
}

func (d *dashServer) notYet(msg string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotImplemented, "not_yet_available", msg, nil)
	}
}

func (d *dashServer) notYetM7(msg string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotImplemented, "not_yet_available", msg, map[string]any{"milestone": "M7"})
	}
}

// ---- session ----

// SessionLinkRequest is POST /v1/session/links (bearer only).
type SessionLinkRequest struct {
	Host string `json:"host,omitempty"`
}

// SessionLink is its answer.
type SessionLink struct {
	URL     string    `json:"url"`
	Expires time.Time `json:"expires"`
}

// SessionBody is POST/GET /v1/session.
type SessionBody struct {
	CSRFToken         string    `json:"csrf_token"`
	ExpiresAt         time.Time `json:"expires_at"`
	AbsoluteExpiresAt time.Time `json:"absolute_expires_at"`
	IdleTimeoutS      int       `json:"idle_timeout_s"`
}

func sessionBody(si sessionInfo) SessionBody {
	return SessionBody{CSRFToken: si.csrf, ExpiresAt: si.idleExpiry().UTC().Truncate(time.Second),
		AbsoluteExpiresAt: si.created.Add(sessionAbsolute).UTC().Truncate(time.Second), IdleTimeoutS: int(sessionIdle / time.Second)}
}

// loopbackHost: the hosts a sign-in link may name (the dashboard is
// served on loopback only, D36).
func loopbackHost(h string) bool {
	switch strings.ToLower(strings.Trim(h, "[]")) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

func (d *dashServer) apiSessionLink(w http.ResponseWriter, r *http.Request) {
	var req SessionLinkRequest
	if err := control.ReadJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	host := cmpOr(req.Host, "127.0.0.1")
	if !loopbackHost(host) {
		writeErr(w, http.StatusBadRequest, "bad_host", "the dashboard is only served on this computer (127.0.0.1 or localhost)", nil)
		return
	}
	tok, exp, err := d.sessions.mintLogin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed", "could not create a sign-in link: "+err.Error(), nil)
		return
	}
	u := "http://" + net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(d.sessions.port)) + "/login#t=" + tok
	control.WriteJSON(w, http.StatusOK, SessionLink{URL: u, Expires: exp.UTC().Truncate(time.Second)})
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (d *dashServer) apiSessionCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := control.ReadJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	id, si, ok, err := d.sessions.exchange(req.Token)
	switch {
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "failed", "could not sign in: "+err.Error(), nil)
		return
	case !ok:
		writeErr(w, http.StatusUnauthorized, "login_link_invalid",
			fmt.Sprintf("This sign-in link has expired or was already used. Run `%s dashboard` again.", brand.Command), nil)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: d.sessions.CookieName(), Value: id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionIdle / time.Second)})
	d.a.log.Info("dashboard: signed in")
	control.WriteJSON(w, http.StatusOK, sessionBody(si))
}

func (d *dashServer) apiSessionGet(w http.ResponseWriter, r *http.Request) {
	si, ok := d.sessions.lookup(r)
	if !ok {
		// The bearer token reaches here too; it has no session.
		writeErr(w, http.StatusUnauthorized, "unauthenticated", "not signed in", nil)
		return
	}
	// Sliding idle timeout: renew the cookie's Max-Age.
	if c, err := r.Cookie(d.sessions.CookieName()); err == nil {
		http.SetCookie(w, &http.Cookie{Name: c.Name, Value: c.Value, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode, MaxAge: int(sessionIdle / time.Second)})
	}
	control.WriteJSON(w, http.StatusOK, sessionBody(si))
}

func (d *dashServer) apiSessionDelete(w http.ResponseWriter, r *http.Request) {
	d.sessions.end(r)
	http.SetCookie(w, &http.Cookie{Name: d.sessions.CookieName(), Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

// ---- version, setup, addresses ----

// VersionInfo is GET /v1/version.
type VersionInfo struct {
	Product         string `json:"product"`
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	GoVersion       string `json:"go_version"`
	OS              string `json:"os"`
	Arch            string `json:"arch"`
	PostgresVersion string `json:"postgres_version"`
	API             int    `json:"api"`
	// Features says which parts of API.md this agent serves for real; the
	// dashboard hides what is false (those routes answer 501).
	Features map[string]bool `json:"features"`
}

// dashFeatures: what this build implements of dashboard/API.md (D36).
func dashFeatures() map[string]bool {
	return map[string]bool{
		"setup":            true,  // `coyote setup` and a waiting service serve the setup page (D50)
		"invite_probe":     true,  // reachability + the cluster's CA fingerprint; not the cluster's name or platform (D50)
		"invite_role":      false, // an invite works for a data node and a witness alike
		"invite_cancel":    true,
		"rename":           false,
		"forced_promote":   false,
		"cluster_name":     false,
		"remote_dashboard": false,
		"app_password":     true,
		"set_address":      true,
		"backups":          true, // D63; page rebuilt for this API (D65)
		"agents":           false,
	}
}

func (d *dashServer) apiVersion(w http.ResponseWriter, r *http.Request) {
	v, commit := cmpOr(d.a.cfg.Version, "dev"), ""
	if f := strings.Fields(v); len(f) > 1 { // "0.6.0 (4fcccd2)" style
		v, commit = f[0], strings.Trim(f[1], "()")
	}
	control.WriteJSON(w, http.StatusOK, VersionInfo{Product: brand.Product, Version: v, Commit: commit, GoVersion: runtime.Version(),
		OS: runtime.GOOS, Arch: runtime.GOARCH, PostgresVersion: pg.Version, API: APIVersion, Features: dashFeatures()})
}

// PlatformView is {os, arch}.
type PlatformView struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// SetupView is GET /v1/setup (API.md §4): fresh, creating, joining,
// member or failed.
type SetupView struct {
	State         string         `json:"state"`
	Role          *string        `json:"role"`
	SuggestedName string         `json:"suggested_name"`
	Platform      PlatformView   `json:"platform"`
	Progress      *SetupProgress `json:"progress,omitempty"`
	Error         *errBody       `json:"error,omitempty"`
}

func (d *dashServer) apiSetup(w http.ResponseWriter, r *http.Request) {
	control.WriteJSON(w, http.StatusOK, d.setupView())
}

func (d *dashServer) setupView() SetupView {
	name := d.a.name
	if name == "" {
		name = d.a.cfg.Name
	}
	if name == "" {
		h, _ := os.Hostname()
		name = pg.SanitizeName(h)
	}
	if d.a.cn != nil {
		d.a.setup.check(d.a) // no wait for the next tick
	}
	v := d.a.setup.view(name)
	if v.Role == nil && d.a.role != "" && v.State == "member" {
		r := string(d.a.role)
		v.Role = &r
	}
	return v
}

func (d *dashServer) apiAlreadySetUp(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusConflict, "already_set_up", "this computer is already part of a cluster", nil)
}

// AddressesView is GET /v1/addresses.
type AddressesView struct {
	netdetect.Report
	Default *string `json:"default"`
}

func (d *dashServer) apiAddresses(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rep, err := netdetect.Detect(ctx, netdetect.Options{})
	if err != nil {
		apiErr(w, fmt.Errorf("look up this computer's addresses: %w", err))
		return
	}
	v := AddressesView{Report: rep}
	if def, ok := rep.Default(); ok {
		s := def.Addr.String()
		v.Default = &s
	}
	if v.Addresses == nil {
		v.Addresses = []netdetect.Address{}
	}
	control.WriteJSON(w, http.StatusOK, v)
}

// InviteCheck is POST /v1/setup/check-invite.
type InviteCheck struct {
	Valid     bool         `json:"valid"`
	ClusterID string       `json:"cluster_id"`
	Expires   time.Time    `json:"expires"`
	Addresses []string     `json:"addresses"`
	Probe     *InviteProbe `json:"probe"` // null unless asked for
}

func (d *dashServer) apiCheckInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code  string `json:"code"`
		Probe bool   `json:"probe,omitempty"`
	}
	if err := control.ReadJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	inv, err := invite.Decode(req.Code)
	if err == nil {
		err = inv.Check(time.Now())
	}
	if err != nil {
		writeInviteErr(w, inv, err)
		return
	}
	out := InviteCheck{Valid: true, ClusterID: inv.ClusterID, Expires: inv.Expires.UTC(), Addresses: inv.Addrs}
	if req.Probe {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		p := probeInvite(ctx, inv, testDialMap(d.a.log))
		out.Probe = &p
	}
	control.WriteJSON(w, http.StatusOK, out)
}

// writeInviteErr answers 422 for a code that does not decode or expired.
func writeInviteErr(w http.ResponseWriter, inv *invite.Invite, err error) {
	code := "invite_corrupt"
	var details map[string]any
	switch {
	case errors.Is(err, invite.ErrNotInvite):
		code = "not_invite"
	case errors.Is(err, invite.ErrExpired):
		code = "invite_expired"
		if inv != nil {
			details = map[string]any{"expired_at": inv.Expires.UTC()}
		}
	case errors.Is(err, invite.ErrVersion):
		code = "invite_version"
	}
	writeErr(w, http.StatusUnprocessableEntity, code, upperFirst(err.Error())+".", details)
}

// ---- invites ----

// DashInviteRequest is POST /v1/invites with the dashboard's role field.
type DashInviteRequest struct {
	TTL  string `json:"ttl,omitempty"`
	Role string `json:"role,omitempty"`
}

// DashInviteResponse adds API.md's fields to InviteResponse.
type DashInviteResponse struct {
	InviteResponse
	ID       string        `json:"id"`
	Role     string        `json:"role"`
	Command  string        `json:"command"`
	Platform *PlatformView `json:"platform"`
}

func (d *dashServer) apiInvite(w http.ResponseWriter, r *http.Request) {
	var req DashInviteRequest
	if err := control.ReadJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	role := cmpOr(req.Role, string(cluster.RoleData))
	if role != string(cluster.RoleData) && role != string(cluster.RoleWitness) {
		writeErr(w, http.StatusBadRequest, "bad_request", `role must be "data" or "witness"`, nil)
		return
	}
	res, ok := d.a.createInvite(w, r.Context(), req.TTL)
	if !ok {
		return
	}
	out := DashInviteResponse{InviteResponse: res, Role: role, Platform: clusterPlatform(d.a.cn.State())}
	if inv, err := invite.Decode(res.Code); err == nil {
		out.ID = inviteID(hashSecretHex(inv.Secret[:]))
	}
	cmd := "join"
	if role == string(cluster.RoleWitness) {
		cmd = "witness"
	}
	// The code is pasted when the command asks for it, never typed after
	// it (other users of a computer can see command lines, D50).
	out.Command = brand.Command + " " + cmd
	control.WriteJSON(w, http.StatusOK, out)
}

func hashSecretHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func inviteID(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// OpenInvite is one entry of GET /v1/invites. Role is not recorded with
// the secret yet, so it is always "data" (D36).
type OpenInvite struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	Expires   time.Time `json:"expires"`
	Used      bool      `json:"used"`
	UsedBy    string    `json:"used_by,omitempty"`
	// Cancelled: cancelled from the dashboard (DELETE /v1/invites/{id});
	// Used is true too, because a cancelled code can never be used.
	Cancelled bool `json:"cancelled,omitempty"`
}

func (d *dashServer) apiInvites(w http.ResponseWriter, r *http.Request) {
	control.WriteJSON(w, http.StatusOK, map[string][]OpenInvite{"invites": openInvites(d.a.cn.State(), time.Now())})
}

// openInvites: unused unexpired ones, and used ones from the last 24 h,
// newest first.
func openInvites(st cluster.State, now time.Time) []OpenInvite {
	out := []OpenInvite{}
	for h, js := range st.JoinSecrets {
		switch {
		case js.Used && now.Sub(js.CreatedAt) > 24*time.Hour:
			continue
		case !js.Used && !now.Before(js.ExpiresAt):
			continue
		}
		oi := OpenInvite{ID: inviteID(h), Role: string(cluster.RoleData), CreatedAt: js.CreatedAt.UTC(), Expires: js.ExpiresAt.UTC(), Used: js.Used}
		if js.Used {
			if js.UsedBy == cancelledBy {
				oi.Cancelled = true
			} else if m, ok := st.Members[js.UsedBy]; ok {
				oi.UsedBy = m.Name
			}
		}
		out = append(out, oi)
	}
	slices.SortFunc(out, func(x, y OpenInvite) int {
		if c := y.CreatedAt.Compare(x.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
	return out
}

// ---- switch ----

// apiSwitch is the CLI's planned switch (apiSwitchMode with planned =
// true: every check of `coyote switch`), plus a record of it for
// health.switching while it runs.
func (d *dashServer) apiSwitch(w http.ResponseWriter, r *http.Request) {
	from := d.a.primaryName(d.a.cn.State())
	sw := &Switching{Kind: "planned", From: from, StartedAt: time.Now().UTC().Truncate(time.Second), Step: "promoting"}
	d.swMu.Lock()
	if d.switching != nil {
		d.swMu.Unlock()
		writeErr(w, http.StatusConflict, "failed", "a switch-over started from this computer is still running", nil)
		return
	}
	d.switching = sw
	d.swMu.Unlock()
	defer func() {
		d.swMu.Lock()
		d.switching = nil
		d.swMu.Unlock()
		d.invalidateStatus()
	}()
	if b, err := bufferBody(r); err == nil {
		var req switchReq
		if json.Unmarshal(b, &req) == nil {
			if m, ok := findMember(d.a.cn.State(), req.To); ok {
				sw.To = m.Name
			} else {
				sw.To = req.To
			}
		}
	}
	d.invalidateStatus()
	d.a.apiSwitchMode(w, r, true)
}

// ---- settings ----

// DashSettings adds cluster_name to SettingsView (not stored yet: "").
type DashSettings struct {
	SettingsView
	ClusterName string `json:"cluster_name"`
}

func dashSettings(s cluster.Settings) DashSettings {
	return DashSettings{SettingsView: settingsView(s)}
}

func (d *dashServer) apiSetSettings(w http.ResponseWriter, r *http.Request) {
	b, err := bufferBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	var req SetSettingsRequest
	if json.Unmarshal(b, &req) == nil {
		if _, ok := req.Set["cluster_name"]; ok {
			writeErr(w, http.StatusNotImplemented, "not_yet_available", "Naming the cluster is not available yet.", nil)
			return
		}
	}
	tw := &rewriteSettings{ResponseWriter: w}
	d.a.apiSetSettings(tw, r)
	tw.flush()
}

// ---- this node ----

// NodeView is GET /v1/node.
type NodeView struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Role      string       `json:"role"`
	Platform  PlatformView `json:"platform"`
	DataDir   string       `json:"data_dir"`
	Advertise []string     `json:"advertise"`
	Ports     struct {
		Postgres int `json:"postgres"`
		API      int `json:"api"`
		Peer     int `json:"peer"`
	} `json:"ports"`
	Service   bool `json:"service"`
	Dashboard struct {
		Remote bool     `json:"remote"`
		URLs   []string `json:"urls"`
	} `json:"dashboard"`
}

func (d *dashServer) nodeView() NodeView {
	a := d.a
	v := NodeView{ID: a.cn.ID(), Name: a.name, Role: string(a.role), Platform: PlatformView{runtime.GOOS, runtime.GOARCH},
		DataDir: a.node.StateDir, Service: a.cfg.Service}
	if m, ok := a.cn.State().Members[a.cn.ID()]; ok {
		v.Advertise = slices.Clone(m.Addresses)
	}
	if v.Advertise == nil {
		v.Advertise = []string{}
	}
	if a.role == cluster.RoleData {
		v.Ports.Postgres = a.clientPort
	}
	v.Ports.API = a.ctlPort
	if _, p, err := net.SplitHostPort(a.listen); err == nil {
		v.Ports.Peer, _ = strconv.Atoi(p)
	}
	v.Dashboard.URLs = []string{"http://127.0.0.1:" + strconv.Itoa(a.ctlPort) + "/"}
	return v
}

func (d *dashServer) apiNode(w http.ResponseWriter, r *http.Request) {
	control.WriteJSON(w, http.StatusOK, d.nodeView())
}

func (d *dashServer) apiSetNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DashboardRemote bool `json:"dashboard_remote"`
	}
	if err := control.ReadJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	if !req.DashboardRemote {
		control.WriteJSON(w, http.StatusOK, d.nodeView())
		return
	}
	writeErr(w, http.StatusNotImplemented, "not_yet_available",
		"The dashboard is only served on this computer. Opening it from other devices is not available yet.", nil)
}

// ---- connection ----

// ConnHost is one host apps may use.
type ConnHost struct {
	Node      string `json:"node"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	This      bool   `json:"this"`
	Reachable bool   `json:"reachable"`
}

// ConnectionView is GET /v1/connection: no password.
type ConnectionView struct {
	Database string     `json:"database"`
	User     string     `json:"user"`
	SSLMode  string     `json:"sslmode"`
	Hosts    []ConnHost `json:"hosts"`
	Local    *struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	} `json:"local"`
	// CAFile is the cluster CA certificate on this computer; CAPEM is its
	// text (public), for apps on other computers.
	CAFile  string `json:"ca_file"`
	CAPEM   string `json:"ca_pem,omitempty"`
	Pending bool   `json:"pending,omitempty"`
	Note    string `json:"note,omitempty"`
}

func (d *dashServer) apiConnection(w http.ResponseWriter, r *http.Request) {
	control.WriteJSON(w, http.StatusOK, d.connectionView(r.Context()))
}

func (d *dashServer) connectionView(ctx context.Context) ConnectionView {
	a := d.a
	st := a.cn.State()
	ci := a.connInfo()
	v := ConnectionView{Database: pg.AppDatabase, User: pg.AppUser, SSLMode: "verify-full", CAFile: ci.CAFile, Pending: ci.Pending, Note: ci.Note}
	if len(st.CA.CertDER) > 0 {
		v.CAPEM = string(pemCert(st.CA.CertDER))
	}
	reach := map[string]bool{}
	if s := d.cachedStatus(); s != nil {
		for _, n := range s.Nodes {
			reach[n.ID] = n.Reachable
		}
	}
	self := a.cn.ID()
	for _, hp := range ci.Hosts {
		h, ps, err := net.SplitHostPort(hp)
		if err != nil {
			continue
		}
		port, _ := strconv.Atoi(ps)
		ch := ConnHost{Host: h, Port: port}
		for id, m := range st.Members {
			if m.ClientPort != port {
				continue
			}
			for _, ad := range m.Addresses {
				if ah, _, err := net.SplitHostPort(ad); err == nil && ah == h {
					ch.Node, ch.This = m.Name, id == self
					ch.Reachable = id == self || reach[id]
				}
			}
		}
		v.Hosts = append(v.Hosts, ch)
	}
	if v.Hosts == nil {
		v.Hosts = []ConnHost{}
	}
	if a.role == cluster.RoleData && a.clientPort > 0 {
		v.Local = &struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		}{"127.0.0.1", a.clientPort}
	}
	return v
}

func (d *dashServer) apiReveal(w http.ResponseWriter, r *http.Request) {
	if err := control.ReadJSON(r, &struct{}{}); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	ci := d.a.connInfo()
	if ci.Pending {
		writeErr(w, http.StatusConflict, "not_ready", "the application role is not set up yet; it is created once the main node is running", nil)
		return
	}
	d.a.log.Info("connection password revealed in the dashboard")
	control.WriteJSON(w, http.StatusOK, map[string]string{"password": ci.Password})
}

// ---- helpers for wrapping the CLI's handlers ----

// bufferBody reads the request body (already capped by the guard) and puts
// the same bytes back for the real handler.
func bufferBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, err
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// rewriteSettings turns the CLI handler's SettingsView into DashSettings.
type rewriteSettings struct {
	http.ResponseWriter
	status int
	buf    []byte
}

func (w *rewriteSettings) WriteHeader(s int) { w.status = s }
func (w *rewriteSettings) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)
	return len(b), nil
}

func (w *rewriteSettings) flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status == http.StatusOK {
		var sv SettingsView
		if json.Unmarshal(w.buf, &sv) == nil {
			control.WriteJSON(w.ResponseWriter, http.StatusOK, DashSettings{SettingsView: sv})
			return
		}
	}
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(w.buf)
}

func clusterPlatform(st cluster.State) *PlatformView {
	for _, m := range st.SortedMembers() {
		if m.Role == cluster.RoleData && m.Platform.OS != "" {
			return &PlatformView{OS: m.Platform.OS, Arch: m.Platform.Arch}
		}
	}
	return nil
}
