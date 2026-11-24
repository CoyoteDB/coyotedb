package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/controller"
	"coyotedb.com/coyote/internal/pg"
)

func TestSessionStoreLoginOnceAndExpiry(t *testing.T) {
	s := newSessionStore(7480)
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	tok, exp, err := s.mintLogin()
	if err != nil || exp.Sub(now) != loginTokenTTL || len(tok) < 40 {
		t.Fatalf("mint: %q %v %v", tok, exp, err)
	}
	id, si, ok, err := s.exchange(tok)
	if err != nil || !ok || id == "" || si.csrf == "" || si.csrf == id {
		t.Fatalf("exchange: %v %v", ok, err)
	}
	if _, _, ok, _ := s.exchange(tok); ok {
		t.Fatal("a login token worked twice")
	}
	if _, _, ok, _ := s.exchange(""); ok {
		t.Fatal("empty token accepted")
	}
	// Expired link.
	tok2, _, _ := s.mintLogin()
	now = now.Add(loginTokenTTL)
	if _, _, ok, _ := s.exchange(tok2); ok {
		t.Fatal("expired login token accepted")
	}

	req := func() *http.Request {
		r := httptest.NewRequest("GET", "/v1/status", nil)
		r.AddCookie(&http.Cookie{Name: s.CookieName(), Value: id})
		return r
	}
	if s.CookieName() != "coyote_session_7480" {
		t.Fatalf("cookie name %s", s.CookieName())
	}
	if csrf, ok := s.Session(req()); !ok || csrf != si.csrf {
		t.Fatal("session not found")
	}
	// Sliding idle timeout: used every 11 h, it lives until the absolute limit.
	for now.Sub(si.created) < sessionAbsolute-11*time.Hour {
		now = now.Add(11 * time.Hour)
		if _, ok := s.Session(req()); !ok {
			t.Fatalf("session ended after %s although used", now.Sub(si.created))
		}
	}
	now = si.created.Add(sessionAbsolute)
	if _, ok := s.Session(req()); ok {
		t.Fatal("session outlived its absolute lifetime")
	}

	// Idle timeout.
	tok3, _, _ := s.mintLogin()
	id3, _, _, _ := s.exchange(tok3)
	now = now.Add(sessionIdle)
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: s.CookieName(), Value: id3})
	if _, ok := s.Session(r); ok {
		t.Fatal("idle session still valid")
	}
}

func TestSessionStorePublicAndBearerOnly(t *testing.T) {
	s := newSessionStore(1)
	for _, c := range []struct {
		method, path       string
		public, bearerOnly bool
	}{
		{"GET", "/", true, false},
		{"GET", "/login", true, false},
		{"GET", "/assets/app.js", true, false},
		{"HEAD", "/nodes", true, false},
		{"POST", "/", false, false},
		{"POST", "/v1/session", true, false},
		{"GET", "/v1/session", false, false},
		{"GET", "/v1", false, false},
		{"GET", "/v1/status", false, false},
		{"POST", "/v1/session/links", false, true},
		{"GET", "/v1/conninfo", false, true},
		{"GET", "/v1/dev/status", false, true},
		{"POST", "/v1/dev/switch", false, true},
		{"POST", "/v1/app-password", false, false},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := s.Public(r); got != c.public {
			t.Errorf("%s %s public=%v", c.method, c.path, got)
		}
		if got := s.BearerOnly(r); got != c.bearerOnly {
			t.Errorf("%s %s bearerOnly=%v", c.method, c.path, got)
		}
	}
}

func TestStaticHandler(t *testing.T) {
	files := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><title>CoyoteDB</title>")},
		"assets/app-1.js":  {Data: []byte("console.log(1)")},
		"assets/f.woff2":   {Data: []byte("wOF2")},
		"theme-init.js":    {Data: []byte("1")},
		".gitkeep":         {Data: nil},
		"favicon.svg":      {Data: []byte("<svg/>")},
		"assets/style.css": {Data: []byte("body{}")},
	}
	h := newStaticHandler(files, true)
	get := func(p string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		return w
	}
	for _, c := range []struct{ path, ctype, cache, body string }{
		{"/", "text/html; charset=utf-8", "no-cache", "<!doctype html>"},
		{"/login", "text/html; charset=utf-8", "no-cache", "<!doctype html>"},
		{"/nodes/node-a", "text/html; charset=utf-8", "no-cache", "<!doctype html>"},
		{"/.gitkeep", "text/html; charset=utf-8", "no-cache", "<!doctype html>"},
		{"/../../etc/passwd", "text/html; charset=utf-8", "no-cache", "<!doctype html>"},
		{"/assets/app-1.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", "console.log"},
		{"/assets/style.css", "text/css; charset=utf-8", "public, max-age=31536000, immutable", "body"},
		{"/assets/f.woff2", "font/woff2", "public, max-age=31536000, immutable", "wOF2"},
		{"/theme-init.js", "text/javascript; charset=utf-8", "no-cache", "1"},
		{"/favicon.svg", "image/svg+xml", "no-cache", "<svg"},
	} {
		w := get(c.path)
		if w.Code != 200 || w.Header().Get("Content-Type") != c.ctype || w.Header().Get("Cache-Control") != c.cache ||
			!strings.HasPrefix(w.Body.String(), c.body) {
			t.Errorf("%s: %d %q %q %q", c.path, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"), w.Body.String())
		}
		if w.Header().Get("Content-Security-Policy") != dashCSP || w.Header().Get("X-Frame-Options") != "DENY" ||
			w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: security headers %v", c.path, w.Header())
		}
	}
	if w := get("/assets/missing-2.js"); w.Code != 404 {
		t.Errorf("missing asset: %d", w.Code)
	}

	fb := newStaticHandler(fstest.MapFS{}, false)
	w := httptest.NewRecorder()
	fb.ServeHTTP(w, httptest.NewRequest("GET", "/anything", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "not included in this build") ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "style-src 'sha256-") {
		t.Fatalf("fallback: %d %v", w.Code, w.Header())
	}
}

func TestEventLogPersistsPagesAndTrims(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	l := openEventLog(dir, log)
	for i := range 5 {
		l.add(Event{Kind: "node_back", Severity: "good", Node: "n" + strconv.Itoa(i), Message: "x"})
	}
	evs, more := l.page("", 2)
	if len(evs) != 2 || !more || evs[0].ID != "000000000005" || evs[1].ID != "000000000004" {
		t.Fatalf("page 1: %+v %v", evs, more)
	}
	evs, more = l.page(evs[1].ID, 10)
	if len(evs) != 3 || more || evs[0].ID != "000000000003" || evs[2].ID != "000000000001" {
		t.Fatalf("page 2: %+v %v", evs, more)
	}
	if s := l.since("000000000003", 200); len(s) != 2 || s[0].ID != "000000000004" {
		t.Fatalf("since: %+v", s)
	}
	// A torn last line (power cut) is skipped; ids continue after a restart.
	f, _ := os.OpenFile(filepath.Join(dir, eventsFile), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"id":"0000000`)
	f.Close()
	l2 := openEventLog(dir, log)
	l2.add(Event{Kind: "agent_started", Severity: "info", Message: "y"})
	evs, _ = l2.page("", 50)
	if len(evs) != 6 || evs[0].ID != "000000000006" {
		t.Fatalf("after reload: %d %+v", len(evs), evs[0])
	}
	// Keeps the newest eventKeep and rewrites the file when it grows.
	for range 2 * eventKeep {
		l2.add(Event{Kind: "node_back", Severity: "good", Message: "z"})
	}
	evs, _ = l2.page("", 200)
	if want := fmt.Sprintf("%012d", 6+2*eventKeep); len(l2.events) != eventKeep || evs[0].ID != want {
		t.Fatalf("kept %d, newest %s", len(l2.events), evs[0].ID)
	}
	b, _ := os.ReadFile(filepath.Join(dir, eventsFile))
	if n := bytes.Count(b, []byte("\n")); n > 2*eventKeep+1 {
		t.Fatalf("file has %d lines", n)
	}
	// Subscribers get new events.
	c, cancel := l2.subscribe()
	l2.add(Event{Kind: "settings_changed", Severity: "info", Message: "s"})
	select {
	case e := <-c:
		if e.Kind != "settings_changed" {
			t.Fatal(e)
		}
	default:
		t.Fatal("subscriber got nothing")
	}
	cancel()
}

func testState() cluster.State {
	st := cluster.State{ClusterID: "c", Epoch: 3, PrimaryID: "a", StandbyEligible: true,
		Members: map[string]cluster.Member{
			"a": {ID: "a", Name: "node-a", Role: cluster.RoleData, Status: cluster.StatusActive, Platform: cluster.Platform{OS: "linux", Arch: "amd64"}},
			"b": {ID: "b", Name: "node-b", Role: cluster.RoleData, Status: cluster.StatusActive, Platform: cluster.Platform{OS: "linux", Arch: "amd64"}},
			"w": {ID: "w", Name: "wit", Role: cluster.RoleWitness, Status: cluster.StatusActive},
		},
		Sync:        cluster.SyncSet{Members: []string{"b"}, K: 1},
		Settings:    cluster.DefaultSettings(),
		JoinSecrets: map[string]cluster.JoinSecret{},
	}
	return st
}

func TestStateEvents(t *testing.T) {
	prev := testState()
	cur := prev.Clone()
	delete(cur.Members, "w")
	cur.Members["c"] = cluster.Member{ID: "c", Name: "node-c", Role: cluster.RoleData}
	cur.PrimaryID = "b"
	cur.StandbyEligible = false
	cur.Settings.DegradeAfter = time.Minute
	cur.JoinSecrets["h"] = cluster.JoinSecret{ExpiresAt: time.Now().Add(time.Hour)}
	kinds := map[string]Event{}
	for _, e := range stateEvents(prev, cur, "automatic") {
		kinds[e.Kind] = e
	}
	for _, k := range []string{"member_joined", "member_removed", "main_changed", "one_copy_started", "settings_changed", "invite_created"} {
		if _, ok := kinds[k]; !ok {
			t.Errorf("no %s in %v", k, kinds)
		}
	}
	if e := kinds["main_changed"]; e.Node != "node-b" || e.From != "node-a" || e.Reason != "automatic" || e.Severity != "warning" {
		t.Errorf("main_changed %+v", e)
	}
	if e := kinds["member_joined"]; e.Node != "node-c" || e.Role != "data" {
		t.Errorf("member_joined %+v", e)
	}
	for _, e := range stateEvents(prev, cur, "planned") {
		if e.Kind == "main_changed" && (e.Reason != "planned" || e.Severity != "info") {
			t.Errorf("planned %+v", e)
		}
	}
	if evs := stateEvents(prev, prev, "automatic"); len(evs) != 0 {
		t.Errorf("no change gave %+v", evs)
	}
	// The first state (before bootstrap) does not report settings or invites.
	if evs := stateEvents(cluster.State{}, prev, "automatic"); len(evs) != 4 { // 3 joined + main
		t.Errorf("from empty: %+v", evs)
	}
}

func TestHealthEvents(t *testing.T) {
	a := healthObs{unreachable: map[string]bool{}}
	b := healthObs{unreachable: map[string]bool{"node-b": true}, blocked: true}
	evs := healthEvents(a, b, []string{"node-a", "node-b"})
	if len(evs) != 2 || evs[0].Kind != "node_unreachable" || evs[1].Kind != "writes_blocked" {
		t.Fatalf("%+v", evs)
	}
	evs = healthEvents(b, a, []string{"node-a", "node-b"})
	if len(evs) != 2 || evs[0].Kind != "node_back" || evs[1].Kind != "writes_resumed" {
		t.Fatalf("%+v", evs)
	}
}

func healthyCopy(id, name string) MemberView {
	return MemberView{ID: id, Name: name, Role: "data", Status: cluster.StatusActive, Reachable: true, InSyncSet: true,
		Replication: "streaming (sync)", LagBytes: 0,
		Report: &NodeReport{State: "standby", PG: &PGReport{Running: true, Role: "standby", Streaming: true}}}
}

func TestEligibility(t *testing.T) {
	st := testState()
	ok := healthyCopy("b", "node-b")
	if e, why := eligibility(st, ok); !e || why != "" {
		t.Fatalf("healthy copy not eligible: %s", why)
	}
	for name, c := range map[string]struct {
		st cluster.State
		m  MemberView
	}{
		"one copy":      {func() cluster.State { s := testState(); s.StandbyEligible = false; return s }(), ok},
		"not in R":      {func() cluster.State { s := testState(); s.Sync.Members = nil; return s }(), ok},
		"unreachable":   {st, func() MemberView { m := ok; m.Reachable = false; return m }()},
		"needs reclone": {st, func() MemberView { m := ok; m.Status = cluster.StatusNeedsReclone; return m }()},
		"marker":        {st, func() MemberView { m := ok; r := *m.Report; r.Marker = "x"; m.Report = &r; return m }()},
		"not streaming": {st, func() MemberView { m := ok; m.Replication = "not connected"; return m }()},
	} {
		if e, why := eligibility(c.st, c.m); e || why == "" {
			t.Errorf("%s: eligible=%v why=%q", name, e, why)
		}
	}
	if e, why := eligibility(st, MemberView{ID: "w", Role: "witness"}); e || why != "" {
		t.Errorf("witness: %v %q", e, why)
	}
	if e, _ := eligibility(st, MemberView{ID: "a", Role: "data", Primary: true}); e {
		t.Error("main node eligible")
	}
}

func TestExtendHealth(t *testing.T) {
	st := testState()
	now := time.Now()
	ctl := func(v controller.View) *controller.Report { return &controller.Report{Leader: true, View: v} }
	s := Status{Cluster: ClusterView{LeaderID: "a", PrimaryName: "node-a"}}

	s.Controller = ctl(controller.View{LeaseHealthy: true})
	h := extendHealth(st, s, nil, now)
	if h.FailoverCode != "" || h.Switching != nil || h.WritesBlocked || h.Promote.Allowed {
		t.Fatalf("healthy: %+v", h)
	}
	// Main node stopped renewing: waiting out the lease, takeover countdown.
	s.Controller = ctl(controller.View{LeaseHealthy: false, LeaseAge: 4 * time.Second})
	h = extendHealth(st, s, nil, now)
	limit := controller.LeaseLimit(st, controller.DefaultMargin)
	if h.FailoverCode != "waiting_lease" || h.Switching == nil || h.Switching.Kind != "automatic" ||
		*h.Switching.TakeoverInMS != (limit-4*time.Second).Milliseconds() || h.Switching.From != "node-a" {
		t.Fatalf("waiting: %+v %+v", h, h.Switching)
	}
	// Blocked.
	s.Controller = ctl(controller.View{FailoverBlocked: "no copy"})
	if h = extendHealth(st, s, nil, now); h.FailoverCode != "no_eligible_copy" || h.Switching != nil {
		t.Fatalf("blocked: %+v", h)
	}
	two := testState()
	delete(two.Members, "w")
	if h = extendHealth(two, s, nil, now); h.FailoverCode != "two_nodes" || h.Promote.Allowed || h.Promote.Reason == "" {
		t.Fatalf("two nodes: %+v", h)
	}
	// No quorum.
	s2 := s
	s2.Cluster.LeaderID = ""
	s2.Controller = nil
	if h = extendHealth(st, s2, nil, now); h.FailoverCode != "no_quorum" {
		t.Fatalf("no quorum: %+v", h)
	}
	// Writes waiting, degrade countdown; strict has no countdown.
	s.Controller = ctl(controller.View{LeaseHealthy: true, WritesWaiting: true, WritesWaitingFor: 10 * time.Second})
	h = extendHealth(st, s, nil, now)
	if !h.WritesBlocked || h.DegradeInMS == nil || *h.DegradeInMS != 20000 || h.WritesBlockedSince.IsZero() {
		t.Fatalf("blocked writes: %+v", h)
	}
	st.Settings.SingleDataNodeMode = cluster.ModeStrict
	h = extendHealth(st, s, nil, now)
	if h.DegradeInMS != nil || h.FailoverCode != "strict_waiting" {
		t.Fatalf("strict: %+v", h)
	}
	// A planned switch this agent runs wins.
	p := &Switching{Kind: "planned", From: "node-a", To: "node-b"}
	if h = extendHealth(st, s, p, now); h.Switching != p {
		t.Fatal("planned switch not reported")
	}
}

func TestOpenInvites(t *testing.T) {
	now := time.Now()
	st := testState()
	st.JoinSecrets = map[string]cluster.JoinSecret{
		"aaaaaaaaaaaaaaaa": {CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
		"bbbbbbbbbbbbbbbb": {CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)},                              // expired
		"cccccccccccccccc": {CreatedAt: now.Add(-3 * time.Hour), ExpiresAt: now.Add(-2 * time.Hour), Used: true, UsedBy: "b"}, // used today
		"dddddddddddddddd": {CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-47 * time.Hour), Used: true},            // used long ago
	}
	got := openInvites(st, now)
	if len(got) != 2 || got[0].ID != "aaaaaaaaaaaa" || got[1].ID != "cccccccccccc" || got[1].UsedBy != "node-b" || !got[1].Used {
		t.Fatalf("%+v", got)
	}
}

// The sign-in flow end to end over a real loopback listener, through the
// control guard: link (bearer only) → session cookie → CSRF-checked calls.
func TestDashSessionFlow(t *testing.T) {
	ln, err := control.ListenPort(0)
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	a := &Agent{log: slog.New(slog.DiscardHandler), ctlPort: port, cfg: Config{Version: "dev"}}
	d := &dashServer{a: a, sessions: newSessionStore(port), events: openEventLog(t.TempDir(), a.log),
		static: newStaticHandler(fstest.MapFS{"index.html": {Data: []byte("<!doctype html>app")}}, true), lastSeen: map[string]time.Time{}}
	d.mux = a.controlHandler(d)
	srv, err := control.Serve(ln, "tok", d, nil, control.WithBrowserSession(d.sessions))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	base := "http://" + srv.Addr()
	hc := &http.Client{}
	do := func(method, path, body string, hdr map[string]string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	code := func(body string) string {
		var e errBody
		_ = json.Unmarshal([]byte(body), &e)
		return e.Code
	}

	// The page itself needs no sign-in.
	if r, b := do("GET", "/login", "", nil); r.StatusCode != 200 || !strings.Contains(b, "app") || r.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("page: %d %s", r.StatusCode, b)
	}
	// Minting a link needs the bearer token.
	if r, b := do("POST", "/v1/session/links", "{}", nil); r.StatusCode != 401 {
		t.Fatalf("link without token: %d %s", r.StatusCode, b)
	}
	if r, b := do("POST", "/v1/session/links", `{"host":"192.168.1.2"}`, map[string]string{"Authorization": "Bearer tok"}); r.StatusCode != 400 || code(b) != "bad_host" {
		t.Fatalf("remote host link: %d %s", r.StatusCode, b)
	}
	r, b := do("POST", "/v1/session/links", "{}", map[string]string{"Authorization": "Bearer tok"})
	var link SessionLink
	if r.StatusCode != 200 || json.Unmarshal([]byte(b), &link) != nil {
		t.Fatalf("link: %d %s", r.StatusCode, b)
	}
	u, _ := url.Parse(link.URL)
	if u.Host != "127.0.0.1:"+strconv.Itoa(port) || u.Path != "/login" || !strings.HasPrefix(u.Fragment, "t=") {
		t.Fatalf("link url %s", link.URL)
	}
	tok := strings.TrimPrefix(u.Fragment, "t=")

	// Exchange: a foreign Origin is refused and does not use up the token.
	if r, b := do("POST", "/v1/session", `{"token":"`+tok+`"}`, map[string]string{"Origin": "http://evil.example"}); r.StatusCode != 403 {
		t.Fatalf("foreign origin sign-in: %d %s", r.StatusCode, b)
	}
	origin := map[string]string{"Origin": base}
	r, b = do("POST", "/v1/session", `{"token":"`+tok+`"}`, origin)
	var sess SessionBody
	if r.StatusCode != 200 || json.Unmarshal([]byte(b), &sess) != nil || sess.CSRFToken == "" || sess.IdleTimeoutS != 43200 {
		t.Fatalf("sign-in: %d %s", r.StatusCode, b)
	}
	var cookie *http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == "coyote_session_"+strconv.Itoa(port) {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Fatalf("cookie %+v", r.Cookies())
	}
	if r, b := do("POST", "/v1/session", `{"token":"`+tok+`"}`, origin); r.StatusCode != 401 || code(b) != "login_link_invalid" {
		t.Fatalf("second use: %d %s", r.StatusCode, b)
	}
	ck := map[string]string{"Cookie": cookie.Name + "=" + cookie.Value}
	if r, b := do("GET", "/v1/session", "", ck); r.StatusCode != 200 || !strings.Contains(b, sess.CSRFToken) {
		t.Fatalf("get session: %d %s", r.StatusCode, b)
	}
	if r, b := do("GET", "/v1/version", "", ck); r.StatusCode != 200 || !strings.Contains(b, `"postgres_version": "`+pg.Version) ||
		r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("version: %d %s", r.StatusCode, b)
	}
	// Secrets by GET and CLI scaffolding stay bearer only.
	for _, p := range []string{"/v1/conninfo", "/v1/dev/status"} {
		if r, b := do("GET", p, "", ck); r.StatusCode != 403 || code(b) != "bearer_only" {
			t.Fatalf("%s with cookie: %d %s", p, r.StatusCode, b)
		}
	}
	// Routes not built yet answer 501 with the milestone.
	if r, b := do("GET", "/v1/agents", "", ck); r.StatusCode != 501 || !strings.Contains(b, `"milestone": "M7"`) {
		t.Fatalf("agents: %d %s", r.StatusCode, b)
	}
	// Backups (D63) are served; this test agent is not in a cluster.
	if r, b := do("GET", "/v1/backups", "", ck); r.StatusCode != 409 || code(b) != "not_set_up" {
		t.Fatalf("backups: %d %s", r.StatusCode, b)
	}
	// Sign out needs CSRF.
	if r, _ := do("DELETE", "/v1/session", "", map[string]string{"Cookie": ck["Cookie"], "Origin": base}); r.StatusCode != 403 {
		t.Fatalf("sign out without csrf: %d", r.StatusCode)
	}
	r, _ = do("DELETE", "/v1/session", "", map[string]string{"Cookie": ck["Cookie"], "Origin": base, control.CSRFHeader: sess.CSRFToken})
	if r.StatusCode != 204 {
		t.Fatalf("sign out: %d", r.StatusCode)
	}
	if r, b := do("GET", "/v1/session", "", ck); r.StatusCode != 401 || code(b) != "unauthenticated" {
		t.Fatalf("after sign out: %d %s", r.StatusCode, b)
	}
}
