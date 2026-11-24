//go:build integration && unix

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/dashboard"
	coyoteagent "coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/control"
)

// dash is a browser-like client of one node's dashboard: session cookie,
// CSRF token, same-origin requests.
type dash struct {
	t      *testing.T
	base   string // http://127.0.0.1:port
	cookie string // name=value
	csrf   string
	hc     *http.Client
}

type dashResp struct {
	status int
	header http.Header
	body   []byte
}

// raw sends a request with exactly the given headers (no cookie unless
// given).
func rawDash(t *testing.T, base, method, path, body string, hdr map[string]string) dashResp {
	t.Helper()
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	resp, err := (&http.Client{}).Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return dashResp{status: resp.StatusCode, header: resp.Header, body: b}
}

func errCode(b []byte) string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(b, &e)
	return e.Code
}

// signIn does what `coyote dashboard` and the page do: mint a link with
// the control token, then exchange the token from its fragment.
func signIn(t *testing.T, n *cnode) *dash {
	t.Helper()
	out := n.mustCLI("dashboard", "--no-open")
	var link string
	for _, f := range strings.Fields(out) {
		if strings.HasPrefix(f, "http://") {
			link = f
		}
	}
	u, err := url.Parse(link)
	if err != nil || u.Path != "/login" || !strings.HasPrefix(u.Fragment, "t=") || u.Host != "127.0.0.1:"+strconv.Itoa(n.ctl) {
		t.Fatalf("coyote dashboard printed no usable link: %v", err)
	}
	base := "http://" + u.Host
	r := rawDash(t, base, "POST", "/v1/session", `{"token":"`+strings.TrimPrefix(u.Fragment, "t=")+`"}`, map[string]string{"Origin": base})
	if r.status != 200 {
		t.Fatalf("sign in: %d %s", r.status, r.body)
	}
	var s coyoteagent.SessionBody
	if err := json.Unmarshal(r.body, &s); err != nil || s.CSRFToken == "" {
		t.Fatalf("session body: %v", err)
	}
	d := &dash{t: t, base: base, csrf: s.CSRFToken, hc: &http.Client{}}
	for _, c := range (&http.Response{Header: r.header}).Cookies() {
		if c.Name == "coyote_session_"+strconv.Itoa(n.ctl) {
			d.cookie = c.Name + "=" + c.Value
		}
	}
	if d.cookie == "" {
		t.Fatal("no session cookie")
	}
	// The link works once.
	if r := rawDash(t, base, "POST", "/v1/session", `{"token":"`+strings.TrimPrefix(u.Fragment, "t=")+`"}`, nil); r.status != 401 {
		t.Fatalf("sign-in link worked twice: %d", r.status)
	}
	return d
}

func (d *dash) get(path string, v any) dashResp {
	d.t.Helper()
	r := rawDash(d.t, d.base, "GET", path, "", map[string]string{"Cookie": d.cookie})
	if r.status != 200 {
		d.t.Fatalf("GET %s: %d %s", path, r.status, r.body)
	}
	if v != nil {
		if err := json.Unmarshal(r.body, v); err != nil {
			d.t.Fatalf("GET %s: %v", path, err)
		}
	}
	return r
}

func (d *dash) post(path, body string) dashResp {
	d.t.Helper()
	return rawDash(d.t, d.base, "POST", path, body, map[string]string{"Cookie": d.cookie, "Origin": d.base, control.CSRFHeader: d.csrf})
}

// The dashboard on a real three-node cluster (2 data + witness): the page,
// every read endpoint, a planned switch-over through the dashboard API, and
// the refusals (no session, wrong Host, missing CSRF or Origin, secrets by
// GET, remote connections).
func TestM6Dashboard(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	w := newWitness(t, root, "wit")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	w.join(a)
	syncFull(t, a, b)

	d := signIn(t, a)

	// ---- the page ----
	for _, p := range []string{"/", "/login", "/switch"} {
		r := rawDash(t, d.base, "GET", p, "", nil)
		if r.status != 200 || !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") ||
			r.header.Get("X-Frame-Options") != "DENY" || r.header.Get("Content-Security-Policy") == "" {
			t.Fatalf("GET %s: %d %v", p, r.status, r.header)
		}
		if dashboard.Built() {
			if !strings.Contains(string(r.body), `<script type="module"`) || !strings.Contains(r.header.Get("Content-Security-Policy"), "script-src 'self'") {
				t.Fatalf("GET %s is not the built dashboard", p)
			}
		} else if !strings.Contains(string(r.body), "not included in this build") {
			t.Fatalf("GET %s is not the fallback page", p)
		}
	}
	if dashboard.Built() {
		// Every script and stylesheet the page names is served with its type.
		page := string(rawDash(t, d.base, "GET", "/", "", nil).body)
		for _, part := range strings.Split(page, `"/assets/`)[1:] {
			name := "/assets/" + part[:strings.IndexByte(part, '"')]
			r := rawDash(t, d.base, "GET", name, "", nil)
			if r.status != 200 || r.header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
				t.Fatalf("GET %s: %d %v", name, r.status, r.header)
			}
		}
	}

	// ---- every read endpoint ----
	var sess coyoteagent.SessionBody
	d.get("/v1/session", &sess)
	if sess.CSRFToken != d.csrf {
		t.Fatal("session CSRF token changed")
	}
	var ver coyoteagent.VersionInfo
	d.get("/v1/version", &ver)
	if ver.API != coyoteagent.APIVersion || ver.PostgresVersion == "" || ver.Product == "" {
		t.Fatalf("version %+v", ver)
	}
	var setup coyoteagent.SetupView
	d.get("/v1/setup", &setup)
	if setup.State != "member" || setup.Role == nil || *setup.Role != "data" || setup.SuggestedName != "node-a" {
		t.Fatalf("setup %+v", setup)
	}
	var addrs map[string]any
	d.get("/v1/addresses", &addrs)
	if _, ok := addrs["addresses"]; !ok {
		t.Fatalf("addresses %v", addrs)
	}
	var st coyoteagent.DashStatus
	waitFor(t, 30*time.Second, "dashboard status shows node-b eligible", func() bool {
		d.get("/v1/status", &st)
		for _, n := range st.Nodes {
			if n.Name == "node-b" {
				return n.Eligible
			}
		}
		return false
	})
	if len(st.Nodes) != 3 || st.Cluster.PrimaryName != "node-a" || st.Cluster.Stale || st.Cluster.Platform == nil ||
		st.Health.FailoverCode != "" || st.Health.Switching != nil || st.Health.Promote.Allowed {
		t.Fatalf("status %+v / %+v", st.Cluster, st.Health)
	}
	for _, n := range st.Nodes {
		if n.Platform.OS == "" && n.Role == "data" || n.JoinedAt.IsZero() || n.LastSeen == nil {
			t.Fatalf("node %s lacks the new fields: %+v", n.Name, n)
		}
		if n.Name == "wit" && (n.Eligible || n.EligibleReason != "") || n.Name == "node-a" && n.Eligible {
			t.Fatalf("eligible wrong for %s", n.Name)
		}
	}
	var events struct {
		Events []coyoteagent.Event `json:"events"`
	}
	d.get("/v1/events?limit=200", &events)
	kinds := map[string]bool{}
	for _, e := range events.Events {
		kinds[e.Kind] = true
	}
	for _, k := range []string{"agent_started", "member_joined"} {
		if !kinds[k] {
			t.Fatalf("activity lacks %s: %v", k, kinds)
		}
	}
	var invites map[string][]coyoteagent.OpenInvite
	d.get("/v1/invites", &invites)
	if len(invites["invites"]) < 2 { // two used by node-b and wit
		t.Fatalf("invites %+v", invites)
	}
	var settings map[string]any
	d.get("/v1/settings", &settings)
	if settings["lease_duration"] != "10s" {
		t.Fatalf("settings %v", settings)
	}
	var node coyoteagent.NodeView
	d.get("/v1/node", &node)
	if node.Name != "node-a" || node.Ports.API != a.ctl || node.Ports.Postgres != a.client || node.Dashboard.Remote {
		t.Fatalf("node %+v", node)
	}
	var conn coyoteagent.ConnectionView
	cr := d.get("/v1/connection", &conn)
	if len(conn.Hosts) != 2 || conn.User != "app" || conn.SSLMode != "verify-full" || !strings.Contains(conn.CAPEM, "BEGIN CERTIFICATE") ||
		strings.Contains(string(cr.body), "password") {
		t.Fatalf("connection %+v", conn)
	}
	ci := a.connInfo()
	r := d.post("/v1/connection/reveal", "{}")
	var pw struct {
		Password string `json:"password"`
	}
	if r.status != 200 || json.Unmarshal(r.body, &pw) != nil || pw.Password != ci.Password {
		t.Fatalf("reveal: %d", r.status)
	}

	// The live stream opens with setup, then status.
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", d.base+"/v1/stream", nil)
		req.Header.Set("Cookie", d.cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("stream: %d %v", resp.StatusCode, resp.Header)
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 4<<20)
		var seen []string
		for sc.Scan() && len(seen) < 2 {
			if ev, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				seen = append(seen, ev)
			}
		}
		if len(seen) != 2 || seen[0] != "setup" || seen[1] != "status" {
			t.Fatalf("stream events %v (%v)", seen, sc.Err())
		}
	}()

	// ---- refusals ----
	if r := rawDash(t, d.base, "GET", "/v1/status", "", nil); r.status != 401 || errCode(r.body) != "unauthenticated" {
		t.Fatalf("no session: %d %s", r.status, r.body)
	}
	if r := rawDash(t, d.base, "GET", "/v1/status", "", map[string]string{"Cookie": d.cookie, "Host": "evil.example:" + strconv.Itoa(a.ctl)}); r.status != 403 || errCode(r.body) != "bad_host" {
		t.Fatalf("bad host: %d %s", r.status, r.body)
	}
	if r := rawDash(t, d.base, "GET", "/", "", map[string]string{"Host": "rebind.example:" + strconv.Itoa(a.ctl)}); r.status != 403 {
		t.Fatalf("page with bad host: %d", r.status)
	}
	if r := rawDash(t, d.base, "POST", "/v1/switch", `{"to":"node-b"}`, map[string]string{"Cookie": d.cookie, "Origin": d.base}); r.status != 403 || errCode(r.body) != "csrf_failed" {
		t.Fatalf("switch without CSRF: %d %s", r.status, r.body)
	}
	if r := rawDash(t, d.base, "POST", "/v1/switch", `{"to":"node-b"}`, map[string]string{"Cookie": d.cookie, control.CSRFHeader: d.csrf, "Origin": "http://evil.example"}); r.status != 403 || errCode(r.body) != "csrf_failed" {
		t.Fatalf("switch from another origin: %d %s", r.status, r.body)
	}
	if r := rawDash(t, d.base, "GET", "/v1/conninfo", "", map[string]string{"Cookie": d.cookie}); r.status != 403 || errCode(r.body) != "bearer_only" {
		t.Fatalf("conninfo with cookie: %d %s", r.status, r.body)
	}
	if r := rawDash(t, d.base, "POST", "/v1/session/links", "{}", map[string]string{"Cookie": d.cookie, "Origin": d.base, control.CSRFHeader: d.csrf}); r.status != 403 || errCode(r.body) != "bearer_only" {
		t.Fatalf("link minting with cookie: %d %s", r.status, r.body)
	}
	// Nothing listens on other interfaces: a remote browser cannot even connect.
	if ip := nonLoopbackIP(); ip != "" {
		if c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(a.ctl)), 2*time.Second); err == nil {
			c.Close()
			t.Fatalf("the dashboard port answers on %s", ip)
		}
	} else {
		t.Log("no non-loopback address on this machine; the remote refusal is covered by internal/control's guard test")
	}
	// The dashboard goes through the same checks as the CLI: a witness
	// cannot become the main node.
	if r := d.post("/v1/switch", `{"to":"wit"}`); r.status/100 == 2 || !strings.Contains(string(r.body), "witness") {
		t.Fatalf("switch to a witness: %d %s", r.status, r.body)
	}

	// ---- a planned switch-over through the dashboard ----
	start := time.Now()
	r = d.post("/v1/switch", `{"to":"node-b"}`)
	var res coyoteagent.SwitchResult
	if r.status != 200 || json.Unmarshal(r.body, &res) != nil || res.To != "node-b" || res.From != "node-a" {
		t.Fatalf("switch: %d %s", r.status, r.body)
	}
	t.Logf("planned switch through the dashboard took %s", time.Since(start).Round(100*time.Millisecond))
	if !b.isPrimary() {
		t.Fatal("node-b is not read-write after the switch")
	}
	a.waitOut("Following node-b live.", 2*time.Minute)
	insertRange(t, b.conn(), 101, 120, "after-dash-switch-")
	waitSame(t, a.pgNode, b.pgNode)
	waitFor(t, 30*time.Second, "dashboard shows node-b as main and the switch recorded", func() bool {
		d.get("/v1/status", &st)
		d.get("/v1/events?limit=200", &events)
		planned := false
		for _, e := range events.Events {
			if e.Kind == "main_changed" && e.Node == "node-b" && e.Reason == "planned" {
				planned = true
			}
		}
		return st.Cluster.PrimaryName == "node-b" && st.Health.Switching == nil && planned
	})

	// Sign out ends the session.
	if r := rawDash(t, d.base, "DELETE", "/v1/session", "", map[string]string{"Cookie": d.cookie, "Origin": d.base, control.CSRFHeader: d.csrf}); r.status != 204 {
		t.Fatalf("sign out: %d", r.status)
	}
	if r := rawDash(t, d.base, "GET", "/v1/status", "", map[string]string{"Cookie": d.cookie}); r.status != 401 {
		t.Fatalf("after sign out: %d", r.status)
	}
	stopAll(a, b, w)
}
