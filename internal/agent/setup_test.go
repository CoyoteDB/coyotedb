package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/invite"
	"coyotedb.com/coyote/internal/netdetect"
	"coyotedb.com/coyote/internal/pg"
)

func TestContainerAddressCheck(t *testing.T) {
	in := func() (string, bool) { return "docker", true }
	out := func() (string, bool) { return "", false }
	cases := []struct {
		name   string
		listen string
		adv    []string
		in     func() (string, bool)
		refuse bool
	}{
		{"bridge address in a container", "0.0.0.0:7481", []string{"172.17.0.5:7481"}, in, true},
		{"podman network", "0.0.0.0:7481", []string{"10.88.0.4:7481"}, in, true},
		{"listener on the container address", "172.18.0.2:7481", nil, in, true},
		{"host network: LAN address", "0.0.0.0:7481", []string{"192.168.1.20:7481"}, in, false},
		{"host network: Tailscale first", "0.0.0.0:7481", []string{"100.64.0.43:7481", "172.17.0.1:7481"}, in, false},
		{"not a container", "0.0.0.0:7481", []string{"172.17.0.5:7481"}, out, false},
		{"loopback listener", "127.0.0.1:7481", nil, in, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := containerAddressCheck(c.listen, c.adv, c.in)
			if (err != nil) != c.refuse {
				t.Fatalf("refused=%v, want %v (%v)", err != nil, c.refuse, err)
			}
			if err == nil {
				return
			}
			var ee *ExitError
			if !errors.As(err, &ee) || !strings.Contains(ee.Msg, "--advertise HOST:7481") || !strings.Contains(ee.Msg, "COYOTE_ADVERTISE") || !strings.Contains(ee.Msg, "docker") {
				t.Fatalf("message: %v", err)
			}
		})
	}
}

func TestClassifyStartError(t *testing.T) {
	dir := "/data/coyote"
	wrap := func(s string) error {
		return fmt.Errorf("PostgreSQL did not start: %w\n%s", pg.ErrExitedDuringStartup, s)
	}
	cases := []struct {
		name      string
		err       error
		known     bool
		permanent bool
		want      string
	}{
		{"admin rights", wrap("Execution of PostgreSQL by a user with administrative permissions is not permitted."), true, true, "--data-dir /data/coyote"},
		{"root", wrap(`"root" execution of the PostgreSQL server is not permitted.`), true, true, "without sudo"},
		{"bad setting", wrap("LOG:  invalid value for parameter \"shared_buffers\": \"banana\"\nFATAL:  configuration file \"/data/coyote/pgdata/postgresql.auto.conf\" contains errors"), true, true, `invalid value for parameter "shared_buffers": "banana"`},
		{"permissions", wrap(`FATAL:  data directory "/x" has invalid permissions`), true, true, "--data-dir"},
		{"lock file denied", wrap(`FATAL:  could not create lock file "postmaster.pid": Permission denied`), true, true, "owns"},
		{"other version", wrap(`FATAL:  database files are incompatible with server`), true, true, "different PostgreSQL version"},
		{"disk full", wrap("FATAL:  could not write to file: No space left on device"), true, false, "Free some space"},
		{"port taken", wrap(`LOG:  could not bind IPv4 address "127.0.0.1": Address already in use`), true, false, "--pg-port"},
		{"crash", wrap("LOG:  server process was terminated by signal 9"), false, false, ""},
		{"nil", nil, false, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, ok := classifyStartError(c.err, dir)
			if ok != c.known || f.permanent != c.permanent {
				t.Fatalf("known=%v permanent=%v, want %v %v", ok, f.permanent, c.known, c.permanent)
			}
			if c.want != "" && !strings.Contains(f.msg, c.want) {
				t.Fatalf("message %q lacks %q", f.msg, c.want)
			}
		})
	}
	if got := firstPGError("x\nLOG: a\nFATAL:  configuration file contains errors\n"); got != "configuration file contains errors" {
		t.Fatalf("firstPGError: %q", got)
	}
}

func TestStartProblemOnlyForStarts(t *testing.T) {
	a := &Agent{out: &printer{w: io.Discard}, log: slog.New(slog.DiscardHandler)}
	a.node.StateDir = "/d"
	// pg_rewind's "Permission denied" is recovered by re-cloning: never a stop.
	if err := a.startProblem(errors.New(`pg_rewind: error: could not open target file "/d/pgdata/coyote.conf": Permission denied`)); err != nil {
		t.Fatalf("rewind error stopped the node: %v", err)
	}
	perm := fmt.Errorf("PostgreSQL did not start: %w\nFATAL:  configuration file \"x\" contains errors", pg.ErrExitedDuringStartup)
	// The first tries only say it: one failed start may be passing.
	for i := 1; i < permanentStartRepeats; i++ {
		if err := a.startProblem(perm); err != nil {
			t.Fatalf("try %d stopped the node: %v", i, err)
		}
	}
	err := a.startProblem(perm)
	var ee *ExitError
	if !errors.As(err, &ee) || !errors.Is(err, pg.ErrExitedDuringStartup) {
		t.Fatalf("a permanent start error did not stop: %v", err)
	}
}

func TestNextStep(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("write: %w", syscall.ENOSPC), "free some space"},
		{fmt.Errorf("open x: %w", syscall.EACCES), "--data-dir"},
		{fmt.Errorf("listen: %w", syscall.EADDRINUSE), "pick another port"},
		{errors.New("dial tcp: lookup example.invalid: no such host"), "network connection"},
		{errors.New("something else"), ""},
	}
	for _, c := range cases {
		if got := NextStep(c.err, "/d"); !strings.Contains(got, c.want) || (c.want == "" && got != "") {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
	}
}

func TestSetupErrorCodes(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{&ExitError{Code: 1, Msg: "used", Cause: cluster.ErrSecretUsed}, "secret_used"},
		{&ExitError{Code: 1, Msg: "dup", Cause: cluster.ErrDuplicateName}, "duplicate_name"},
		{&ExitError{Code: 1, Msg: "x", Cause: fmt.Errorf("via a: %w", errors.New("dial tcp 1.2.3.4:7481: i/o timeout"))}, "failed"},
		{&ExitError{Code: 1, Msg: "port", Cause: syscall.EADDRINUSE}, "port_in_use"},
		{fmt.Errorf("initdb: %w", syscall.ENOSPC), "disk_full"},
		{invite.ErrExpired, "invite_expired"},
	}
	for _, c := range cases {
		if code, msg, _ := setupError(c.err); code != c.code || msg == "" {
			t.Errorf("%v: code %q, want %q", c.err, code, c.code)
		}
	}
}

func TestSetupTrackerFlow(t *testing.T) {
	tr := newSetupTracker()
	if v := tr.view("n"); v.State != "fresh" || v.Progress != nil || v.Role != nil {
		t.Fatalf("fresh: %+v", v)
	}
	if !tr.tryStart("join", "data") || tr.tryStart("create", "data") {
		t.Fatal("tryStart must admit exactly one")
	}
	w := tr.wait()
	tr.begin(lcJoin, cluster.RoleData)
	select {
	case <-w:
	default:
		t.Fatal("no change signalled")
	}
	ids := func() string {
		var s []string
		for _, st := range tr.view("").Progress.Steps {
			s = append(s, st.ID+"="+st.State)
		}
		return strings.Join(s, ",")
	}
	if got := ids(); got != "download=pending,verify=pending,contact=pending,copy=pending,follow=pending" {
		t.Fatalf("steps: %s", got)
	}
	tr.step(stepDownload, stepActive, "")
	tr.downloaded() // no bytes seen: already cached
	tr.step(stepContact, stepActive, "")
	tr.fail(&ExitError{Code: 1, Msg: "That invite code was already used.", Cause: cluster.ErrSecretUsed})
	v := tr.view("")
	if v.State != "failed" || v.Error == nil || v.Error.Code != "secret_used" || ids() != "download=skipped,verify=done,contact=failed,copy=pending,follow=pending" {
		t.Fatalf("failed: %+v %s", v, ids())
	}
	if !tr.reset() || tr.view("").State != "fresh" || tr.reset() {
		t.Fatal("reset")
	}
	// A create, then done.
	tr.tryStart("create", "data")
	tr.begin(lcBootstrap, cluster.RoleData)
	tr.downloadBytes(50, 100)
	tr.downloaded()
	tr.clusterOpen(false)
	if got := ids(); got != "download=done,verify=done,prepare=done,start=active" {
		t.Fatalf("create steps: %s", got)
	}
	tr.mu.Lock()
	tr.setLocked(stepStart, stepDone, "")
	tr.finishLocked()
	tr.mu.Unlock()
	if v := tr.view(""); v.State != "member" || !v.Progress.Done || *v.Role != "data" {
		t.Fatalf("done: %+v", v)
	}
	// A restart of a member.
	tr2 := newSetupTracker()
	tr2.begin(lcStart, cluster.RoleWitness)
	if v := tr2.view(""); v.State != "member" || v.Progress != nil {
		t.Fatalf("member: %+v", v)
	}
	var nilT *setupTracker
	nilT.step(stepCopy, stepDone, "") // safe on nil
	if nilT.view("x").State != "member" {
		t.Fatal("nil tracker view")
	}
}

func TestCAPathNote(t *testing.T) {
	if caPathNote("windows", `C:\ProgramData\CoyoteDB\ca.crt`) != "" || caPathNote("linux", "/home/jörg/ca.crt") != "" {
		t.Fatal("note where none is needed")
	}
	if n := caPathNote("windows", `D:\Jörg\ca.crt`); !strings.Contains(n, "sslrootcert") {
		t.Fatalf("note: %q", n)
	}
}

func TestInfoAddr(t *testing.T) {
	for in, want := range map[string]string{"0.0.0.0:7480": "127.0.0.1:7480", "[::]:7480": "[::1]:7480", "127.0.0.1:7480": "127.0.0.1:7480"} {
		if got := infoAddr(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// The setup routes behind the real guard: a session works, every cluster
// route is not_set_up, create is accepted once and handed to the agent,
// and bad input is refused before anything starts.
func TestSetupRoutesBehindGuard(t *testing.T) {
	dir := t.TempDir()
	node, err := pg.NewNode(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := loadStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: Config{Version: "test"}, node: node, st: st, log: slog.New(slog.DiscardHandler), out: &printer{w: io.Discard}, setup: newSetupTracker(), ctlPort: 7480}
	s := newSessionStore(7480)
	reqs := make(chan setupRequest, 1)
	d := a.newSetupDash(s, reqs)
	ctl, err := control.Listen(0, "tok", d, nil, control.WithBrowserSession(s))
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	// The guard checks Host against its own port.
	port := strings.TrimPrefix(ctl.Addr(), "127.0.0.1:")
	s.port = atoiT(t, port)
	base := "http://" + ctl.Addr()
	tok, _, _ := s.mintLogin()
	do := func(method, path, body string, hdr map[string]string) (int, []byte, http.Header) {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, resp.Header
	}
	code, b, h := do("POST", "/v1/session", `{"token":"`+tok+`"}`, map[string]string{"Origin": base})
	if code != 200 {
		t.Fatalf("sign in: %d %s", code, b)
	}
	var sb SessionBody
	_ = json.Unmarshal(b, &sb)
	cookie := strings.Split(h.Get("Set-Cookie"), ";")[0]
	sess := map[string]string{"Cookie": cookie}
	post := map[string]string{"Cookie": cookie, "Origin": base, control.CSRFHeader: sb.CSRFToken, "Content-Type": "application/json"}

	if code, b, _ := do("GET", "/v1/setup", "", nil); code != 401 {
		t.Fatalf("setup without a session: %d %s", code, b)
	}
	code, b, _ = do("GET", "/v1/setup", "", sess)
	var v SetupView
	if code != 200 || json.Unmarshal(b, &v) != nil || v.State != "fresh" || v.SuggestedName == "" {
		t.Fatalf("setup: %d %s", code, b)
	}
	for _, p := range []string{"/v1/status", "/v1/settings", "/v1/invites", "/v1/connection"} {
		if code, b, _ := do("GET", p, "", sess); code != 409 || !strings.Contains(string(b), "not_set_up") {
			t.Fatalf("%s: %d %s", p, code, b)
		}
	}
	code, b, _ = do("GET", "/v1/version", "", sess)
	var vi VersionInfo
	if code != 200 || json.Unmarshal(b, &vi) != nil || !vi.Features["setup"] || !vi.Features["invite_probe"] || !vi.Features["invite_cancel"] {
		t.Fatalf("version: %d %s", code, b)
	}
	// Refusals before anything starts.
	for _, c := range []struct{ path, body, code string }{
		{"/v1/setup/create", `{"name":"Not Valid!","advertise":""}`, "bad_request"},
		{"/v1/setup/create", `{"name":"a","advertise":"","cluster_name":"Bakery"}`, "not_yet_available"},
		{"/v1/setup/create", `{"name":"a","advertise":"bad host:x:y"}`, "bad_request"},
		{"/v1/setup/join", `{"code":"hello","name":"b","advertise":""}`, "not_invite"},
		{"/v1/setup/join", `{"code":"coyote-0000","name":"b","advertise":""}`, "invite_corrupt"},
	} {
		if code, b, _ := do("POST", c.path, c.body, post); !strings.Contains(string(b), `"`+c.code+`"`) || code < 400 {
			t.Fatalf("%s %s: %d %s", c.path, c.body, code, b)
		}
	}
	// No CSRF: refused by the guard.
	if code, _, _ := do("POST", "/v1/setup/create", `{"name":"a","advertise":""}`, sess); code != 403 {
		t.Fatalf("create without CSRF: %d", code)
	}
	code, b, _ = do("POST", "/v1/setup/create", `{"name":"node-a","advertise":"192.0.2.10"}`, post)
	if code != 202 || json.Unmarshal(b, &v) != nil || v.State != "creating" {
		t.Fatalf("create: %d %s", code, b)
	}
	select {
	case r := <-reqs:
		if r.mode != ModeUp || r.name != "node-a" || len(r.adv) != 1 || r.adv[0] != "192.0.2.10:7481" || !r.fromPage {
			t.Fatalf("request: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("create was not handed to the agent")
	}
	if code, b, _ := do("POST", "/v1/setup/create", `{"name":"node-a","advertise":""}`, post); code != 409 || !strings.Contains(string(b), "already_set_up") {
		t.Fatalf("second create: %d %s", code, b)
	}
	if code, b, _ := do("POST", "/v1/setup/reset", ``, post); code != 409 || !strings.Contains(string(b), "not_failed") {
		t.Fatalf("reset while creating: %d %s", code, b)
	}
	a.setup.fail(&ExitError{Code: 1, Msg: "Port 5432 is already in use.", Cause: syscall.EADDRINUSE})
	if code, b, _ := do("POST", "/v1/setup/reset", ``, post); code != 200 || !strings.Contains(string(b), `"fresh"`) {
		t.Fatalf("reset after failure: %d %s", code, b)
	}
}

func atoiT(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

var _ = httptest.NewRecorder

// TestSetupInContainer: inside a container the setup page never offers or
// takes an address only the container has (D68).
func TestSetupInContainer(t *testing.T) {
	t.Setenv(netdetect.TestContainerEnv, "docker")
	dir := t.TempDir()
	node, err := pg.NewNode(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := loadStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{cfg: Config{Version: "test"}, node: node, st: st, log: slog.New(slog.DiscardHandler), out: &printer{w: io.Discard}, setup: newSetupTracker(), ctlPort: 7480}
	s := newSessionStore(7480)
	reqs := make(chan setupRequest, 1)
	ctl, err := control.Listen(0, "tok", a.newSetupDash(s, reqs), nil, control.WithBrowserSession(s))
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	s.port = atoiT(t, strings.TrimPrefix(ctl.Addr(), "127.0.0.1:"))
	base := "http://" + ctl.Addr()
	do := func(method, path, body string, hdr map[string]string) (int, []byte, http.Header) {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b, resp.Header
	}
	tok, _, _ := s.mintLogin()
	code, b, h := do("POST", "/v1/session", `{"token":"`+tok+`"}`, map[string]string{"Origin": base})
	if code != 200 {
		t.Fatalf("sign in: %d %s", code, b)
	}
	var sb SessionBody
	_ = json.Unmarshal(b, &sb)
	cookie := strings.Split(h.Get("Set-Cookie"), ";")[0]
	post := map[string]string{"Cookie": cookie, "Origin": base, control.CSRFHeader: sb.CSRFToken, "Content-Type": "application/json"}

	code, b, _ = do("GET", "/v1/addresses", "", map[string]string{"Cookie": cookie})
	var av AddressesView
	if code != 200 || json.Unmarshal(b, &av) != nil || av.Container == nil || av.Container.Kind != "docker" {
		t.Fatalf("addresses: %d %s", code, b)
	}
	for _, x := range av.Addresses {
		if x.Addr.IsLoopback() || netdetect.ContainerPrivate(x.Addr) {
			t.Fatalf("offered %s inside a container: %s", x.Addr, b)
		}
	}
	for _, adv := range []string{"127.0.0.1", "localhost:7481x", "[::1]:7481"} {
		code, b, _ := do("POST", "/v1/setup/create", `{"name":"a","advertise":"`+adv+`"}`, post)
		if adv == "localhost:7481x" {
			if code != 400 {
				t.Fatalf("%s: %d %s", adv, code, b)
			}
			continue
		}
		if code != 422 || !strings.Contains(string(b), `"container_address"`) || !strings.Contains(string(b), "computer that runs Docker") {
			t.Fatalf("%s: %d %s", adv, code, b)
		}
	}
	if v := a.setup.view(""); v.State != "fresh" {
		t.Fatalf("a refused address started something: %+v", v)
	}
	if code, b, _ := do("POST", "/v1/setup/create", `{"name":"a","advertise":"192.0.2.10"}`, post); code != 202 {
		t.Fatalf("host address: %d %s", code, b)
	}
	if r := <-reqs; len(r.adv) != 1 || r.adv[0] != "192.0.2.10:7481" {
		t.Fatalf("request: %+v", r)
	}
}
