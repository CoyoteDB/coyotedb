//go:build integration && unix

// Integration tests for setup ease (D50-D52): the invite code never on a
// command line, the browser setup page driving a real create and a real
// join through the API alone, the container address refusal (through its
// test hook, not a real container), and a PostgreSQL start that can never
// work stopping the node with what to do.
package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	coyoteagent "coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/invite"
)

// setupRoot is a scratch folder for one test, named setup-*.
func setupRoot(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "setup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// launchWith starts coyote with stdin and extra environment.
func launchWith(t *testing.T, stdin string, env []string, args ...string) *agent {
	t.Helper()
	a := &agent{out: &syncBuffer{}, exited: make(chan struct{})}
	a.cmd = exec.Command(coyoteBin, args...)
	a.cmd.Env = append(append(os.Environ(), "COYOTE_PG_CACHE="+pgCache), env...)
	a.cmd.Stdout, a.cmd.Stderr = a.out, a.out
	a.cmd.Stdin = strings.NewReader(stdin)
	prepareAgent(a.cmd)
	if err := a.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		a.err = a.cmd.Wait()
		close(a.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-a.exited:
		default:
			_ = interrupt(a.cmd.Process)
			select {
			case <-a.exited:
			case <-time.After(60 * time.Second):
				_ = a.cmd.Process.Kill()
			}
		}
	})
	return a
}

// procTree is pid and every descendant, with each one's command line.
func procTree(t *testing.T, pid int) map[int]string {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		t.Fatal(err)
	}
	type p struct {
		ppid int
		cmd  string
	}
	all := map[int]p{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		a, err1 := strconv.Atoi(f[0])
		b, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		all[a] = p{b, strings.Join(f[2:], " ")}
	}
	tree := map[int]string{}
	if q, ok := all[pid]; ok {
		tree[pid] = q.cmd
	}
	for grew := true; grew; {
		grew = false
		for id, q := range all {
			if _, in := tree[id]; !in {
				if _, parent := tree[q.ppid]; parent {
					tree[id] = q.cmd
					grew = true
				}
			}
		}
	}
	return tree
}

// secretParts are what must never show: the code as given and normalised.
func secretParts(code string) []string {
	return []string{strings.TrimSpace(code), invite.Normalize(code)}
}

// noInviteInArgv checks the agent and every process it started.
func noInviteInArgv(t *testing.T, pid int, code string) int {
	t.Helper()
	tree := procTree(t, pid)
	if len(tree) == 0 {
		t.Fatalf("process %d not found", pid)
	}
	for id, cmd := range tree {
		for _, s := range secretParts(code) {
			if strings.Contains(cmd, s) {
				t.Fatalf("the invite code is on the command line of process %d", id)
			}
		}
	}
	return len(tree)
}

// noInviteInChildEnv checks that no process the agent started carries the
// code in its environment (ps shows the environment of our own processes).
func noInviteInChildEnv(t *testing.T, pid int, code string) {
	t.Helper()
	for id := range procTree(t, pid) {
		if id == pid {
			continue // the agent's own initial environment is its input
		}
		out, err := exec.Command("ps", "eww", "-o", "command=", "-p", strconv.Itoa(id)).Output()
		if err != nil {
			continue // gone meanwhile
		}
		for _, s := range secretParts(code) {
			if strings.Contains(string(out), s) {
				t.Fatalf("the invite code is in the environment of child process %d", id)
			}
		}
	}
}

// A data node joins with the code in COYOTE_INVITE and a witness with the
// code piped to it: neither command line (nor anything they start) shows
// the code, and the data node's children do not inherit it.
func TestSetupJoinInviteNotOnCommandLine(t *testing.T) {
	root := setupRoot(t)
	a := newCNode(t, root, "sa")
	b := newCNode(t, root, "sb")
	w := newWitness(t, root, "sw")
	a.bootstrap()
	defer stopAll(w, b, a)

	code := a.invite()
	b.a = launchWith(t, "", []string{"COYOTE_INVITE=" + code}, b.args("join", "")...)
	b.waitOut("Following ", 4*time.Minute)
	if n := noInviteInArgv(t, b.a.cmd.Process.Pid, code); n < 3 {
		t.Fatalf("expected the agent, its watchdog and Postgres; found %d processes", n)
	}
	noInviteInChildEnv(t, b.a.cmd.Process.Pid, code)
	if strings.Contains(b.a.out.String(), "Tip: an invite code typed after") {
		t.Fatal("warned although the code came from the environment")
	}

	wcode := a.invite()
	w.a = launchWith(t, wcode+"\n", nil, w.args("witness", "")...)
	if !w.a.waitReady(t, 2*time.Minute) {
		t.Fatalf("witness not ready:\n%s", w.a.out.String())
	}
	noInviteInArgv(t, w.a.cmd.Process.Pid, wcode)

	s := a.clusterStatus()
	if len(s.Nodes) != 3 {
		t.Fatalf("cluster has %d nodes, want 3", len(s.Nodes))
	}
	// The code as an argument still works but warns (checked without
	// starting a node: an empty pipe and no code at all is refused first).
	out, err := runCoyoteDB("join", "--data-dir", filepath.Join(root, "nobody"))
	if err == nil || !strings.Contains(out, "No invite code given") || !strings.Contains(out, "COYOTE_INVITE") {
		t.Fatalf("join without a code: %v\n%s", err, out)
	}
	out, _ = runCoyoteDB("join", "coyote-0000", "--data-dir", filepath.Join(root, "nobody"))
	if !strings.Contains(out, "Tip: an invite code typed after") || !strings.Contains(out, "does not work") {
		t.Fatalf("join with a bad code as an argument:\n%s", out)
	}
}

// pageNode is a node started with `coyote setup` (the browser setup page).
type pageNode struct {
	*cnode
	page *dash
}

func newPageNode(t *testing.T, root, name string) *pageNode {
	return &pageNode{cnode: newCNode(t, root, name)}
}

// start runs `coyote setup --no-open` and signs in with the link it prints.
func (n *pageNode) start() {
	n.t.Helper()
	args := []string{"setup", "--no-open", "--data-dir", n.dir, "--listen", "127.0.0.1:" + strconv.Itoa(n.listen),
		"--control-port", strconv.Itoa(n.ctl), "--pg-port", strconv.Itoa(n.port), "--client-port", strconv.Itoa(n.client)}
	n.a = launchWith(n.t, "", nil, args...)
	n.waitOut("/login#t=", time.Minute)
	if strings.Contains(n.a.out.String(), "Press Ctrl-C to stop.") {
		n.t.Fatal("the setup page agent claims a running node before it is set up")
	}
	var link string
	for _, f := range strings.Fields(n.a.out.String()) {
		if strings.HasPrefix(f, "http://") && strings.Contains(f, "/login#t=") {
			link = f
		}
	}
	n.page = signInLink(n.t, link, n.ctl)
}

// setupView polls GET /v1/setup until cond or the deadline.
func (n *pageNode) waitSetup(within time.Duration, cond func(coyoteagent.SetupView) bool) coyoteagent.SetupView {
	n.t.Helper()
	deadline := time.Now().Add(within)
	for {
		var v coyoteagent.SetupView
		n.page.get("/v1/setup", &v)
		if cond(v) {
			return v
		}
		select {
		case <-n.a.exited:
			n.t.Fatalf("%s exited (%v):\n%s", n.name, n.a.err, n.a.out.String())
		default:
		}
		if time.Now().After(deadline) {
			b, _ := json.Marshal(v)
			n.t.Fatalf("%s: setup still %s after %s:\n%s", n.name, b, within, n.a.out.String())
		}
		time.Sleep(200 * time.Millisecond) // polling interval
	}
}

func doneMember(v coyoteagent.SetupView) bool {
	return v.State == "member" && v.Progress != nil && v.Progress.Done
}

// A cluster made and joined from the setup page's API alone: create on
// one computer, invite and cancel an invite there, check an invite (with
// the cluster's answer and a forged CA fingerprint) on the other, a join
// that fails (name taken) and is tried again, until both are members.
func TestSetupWizardCreateAndJoin(t *testing.T) {
	root := setupRoot(t)
	a := newPageNode(t, root, "pa")
	b := newPageNode(t, root, "pb")
	defer stopAll(b.cnode, a.cnode)

	a.start()
	if v := a.waitSetup(5*time.Second, func(coyoteagent.SetupView) bool { return true }); v.State != "fresh" || v.SuggestedName == "" {
		t.Fatalf("fresh setup: %+v", v)
	}
	if r := rawDash(t, a.page.base, "GET", "/v1/status", "", map[string]string{"Cookie": a.page.cookie}); r.status != 409 || errCode(r.body) != "not_set_up" {
		t.Fatalf("status before setup: %d %s", r.status, r.body)
	}
	var adr struct {
		Addresses []json.RawMessage `json:"addresses"`
	}
	a.page.get("/v1/addresses", &adr)
	var ver coyoteagent.VersionInfo
	a.page.get("/v1/version", &ver)
	if !ver.Features["setup"] || !ver.Features["invite_probe"] || !ver.Features["invite_cancel"] {
		t.Fatalf("features: %v", ver.Features)
	}
	if r := a.page.post("/v1/setup/create", `{"name":"pa","advertise":"127.0.0.1"}`); r.status != 202 {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	v := a.waitSetup(10*time.Minute, doneMember)
	for _, s := range v.Progress.Steps {
		if s.State != "done" && s.State != "skipped" {
			t.Fatalf("create finished with step %s %s", s.ID, s.State)
		}
	}
	// The same sign-in now reaches the whole API.
	var st coyoteagent.DashStatus
	a.page.get("/v1/status", &st)
	if len(st.Nodes) != 1 {
		t.Fatalf("status after create: %d nodes", len(st.Nodes))
	}

	// An invite made and cancelled from the page.
	var inv coyoteagent.DashInviteResponse
	r := a.page.post("/v1/invites", `{"ttl":"30m"}`)
	if r.status != 200 || json.Unmarshal(r.body, &inv) != nil || inv.ID == "" {
		t.Fatalf("invite: %d %s", r.status, r.body)
	}
	if r := rawDash(t, a.page.base, "DELETE", "/v1/invites/"+inv.ID, "", map[string]string{"Cookie": a.page.cookie, "Origin": a.page.base, "X-Coyote-CSRF": a.page.csrf}); r.status != 204 {
		t.Fatalf("cancel: %d %s", r.status, r.body)
	}
	var list struct {
		Invites []coyoteagent.OpenInvite `json:"invites"`
	}
	a.page.get("/v1/invites", &list)
	cancelled := false
	for _, oi := range list.Invites {
		cancelled = cancelled || (oi.ID == inv.ID && oi.Cancelled && oi.Used)
	}
	if !cancelled {
		t.Fatalf("cancelled invite not shown as cancelled: %+v", list.Invites)
	}

	// The second computer checks a code before joining.
	b.start()
	r = a.page.post("/v1/invites", `{"ttl":"30m"}`)
	if r.status != 200 || json.Unmarshal(r.body, &inv) != nil {
		t.Fatalf("invite: %d %s", r.status, r.body)
	}
	var chk coyoteagent.InviteCheck
	r = b.page.post("/v1/setup/check-invite", fmt.Sprintf(`{"code":%q,"probe":true}`, inv.Code))
	if r.status != 200 || json.Unmarshal(r.body, &chk) != nil || !chk.Valid || chk.Probe == nil || !chk.Probe.Reachable || !chk.Probe.ClusterVerified {
		t.Fatalf("check-invite: %d %s", r.status, r.body)
	}
	forged, err := invite.Decode(inv.Code)
	if err != nil {
		t.Fatal(err)
	}
	forged.CAFingerprint[0] ^= 0xff
	fc, err := forged.Encode()
	if err != nil {
		t.Fatal(err)
	}
	r = b.page.post("/v1/setup/check-invite", fmt.Sprintf(`{"code":%q,"probe":true}`, fc))
	if r.status != 200 || json.Unmarshal(r.body, &chk) != nil || chk.Probe == nil || chk.Probe.Reachable || !chk.Probe.WrongCluster {
		t.Fatalf("check-invite with a forged CA fingerprint: %d %s", r.status, r.body)
	}
	if r := b.page.post("/v1/setup/check-invite", `{"code":"coyote-0000","probe":true}`); r.status != 422 || errCode(r.body) != "invite_corrupt" {
		t.Fatalf("check-invite with a broken code: %d %s", r.status, r.body)
	}

	// A join that is refused (the name is taken) is shown and tried again.
	if r := b.page.post("/v1/setup/join", fmt.Sprintf(`{"code":%q,"name":"pa","advertise":"127.0.0.1","role":"data"}`, inv.Code)); r.status != 202 {
		t.Fatalf("join: %d %s", r.status, r.body)
	}
	v = b.waitSetup(5*time.Minute, func(v coyoteagent.SetupView) bool { return v.State == "failed" })
	if v.Error == nil || v.Error.Code != "duplicate_name" || v.Error.Error == "" {
		t.Fatalf("failed join: %+v", v.Error)
	}
	if r := b.page.post("/v1/setup/join", fmt.Sprintf(`{"code":%q,"name":"pb","advertise":"127.0.0.1","role":"data"}`, inv.Code)); r.status != 202 {
		t.Fatalf("join again: %d %s", r.status, r.body)
	}
	v = b.waitSetup(5*time.Minute, doneMember)
	ids := []string{}
	for _, s := range v.Progress.Steps {
		ids = append(ids, s.ID+"="+s.State)
	}
	if got := strings.Join(ids, ","); !strings.Contains(got, "contact=done") || !strings.Contains(got, "copy=done") || !strings.Contains(got, "follow=done") {
		t.Fatalf("join steps: %s", got)
	}
	if !b.isStreaming() {
		t.Fatal("the joined copy does not follow the main node")
	}
	if s := a.clusterStatus(); len(s.Nodes) != 2 {
		t.Fatalf("cluster has %d nodes, want 2", len(s.Nodes))
	}
	// An invite made and cancelled at once on a node that is not the
	// leader (the invite reaches its copy of the state a moment later).
	r = b.page.post("/v1/invites", `{"ttl":"30m"}`)
	if r.status != 200 || json.Unmarshal(r.body, &inv) != nil {
		t.Fatalf("invite on the copy: %d %s", r.status, r.body)
	}
	if r := rawDash(t, b.page.base, "DELETE", "/v1/invites/"+inv.ID, "", map[string]string{"Cookie": b.page.cookie, "Origin": b.page.base, "X-Coyote-CSRF": b.page.csrf}); r.status != 204 {
		t.Fatalf("cancel on the copy: %d %s", r.status, r.body)
	}
	if r := b.page.post("/v1/setup/create", `{"name":"x","advertise":""}`); r.status != 409 {
		t.Fatalf("create on a member: %d %s", r.status, r.body)
	}
}

// signInLink trades a printed sign-in link for a browser session.
func signInLink(t *testing.T, link string, port int) *dash {
	t.Helper()
	i := strings.Index(link, "/login#t=")
	if i < 0 || !strings.HasPrefix(link, "http://127.0.0.1:"+strconv.Itoa(port)) {
		t.Fatalf("no usable sign-in link printed")
	}
	base, tok := link[:i], link[i+len("/login#t="):]
	r := rawDash(t, base, "POST", "/v1/session", `{"token":"`+tok+`"}`, map[string]string{"Origin": base})
	if r.status != 200 {
		t.Fatalf("sign in: %d %s", r.status, r.body)
	}
	var s coyoteagent.SessionBody
	if err := json.Unmarshal(r.body, &s); err != nil || s.CSRFToken == "" {
		t.Fatalf("session body: %v", err)
	}
	d := &dash{t: t, base: base, csrf: s.CSRFToken}
	for _, c := range r.header.Values("Set-Cookie") {
		if strings.HasPrefix(c, "coyote_session_"+strconv.Itoa(port)+"=") {
			d.cookie = strings.Split(c, ";")[0]
		}
	}
	if d.cookie == "" {
		t.Fatal("no session cookie")
	}
	return d
}

// In a container (simulated with the test hook), a node that would
// announce the container's private address is refused before anything
// changes, with what to do; an address given in COYOTE_ADVERTISE is used
// as given.
func TestSetupContainerAddressRefused(t *testing.T) {
	root := setupRoot(t)
	n := newCNode(t, root, "ct")
	upArgs := func(n *cnode) []string {
		return []string{"up", "--data-dir", n.dir, "--listen", "172.17.0.5:" + strconv.Itoa(n.listen),
			"--control-port", strconv.Itoa(n.ctl), "--pg-port", strconv.Itoa(n.port), "--client-port", strconv.Itoa(n.client)}
	}
	args := upArgs(n)
	run := func(env ...string) (string, int) {
		a := launchWith(t, "", env, args...)
		select {
		case <-a.exited:
		case <-time.After(2 * time.Minute):
			t.Fatalf("did not stop:\n%s", a.out.String())
		}
		return a.out.String(), a.cmd.ProcessState.ExitCode()
	}
	out, code := run("COYOTE_TEST_CONTAINER=docker")
	if code != 2 || !strings.Contains(out, "inside a container (docker)") || !strings.Contains(out, "172.17.0.5") ||
		!strings.Contains(out, "--advertise HOST:"+strconv.Itoa(n.listen)) || !strings.Contains(out, "COYOTE_ADVERTISE") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(n.dir, "cluster", "identity.json")); err == nil {
		t.Fatal("a cluster was created although the address was refused")
	}
	// The same outside a container (forced: the Linux suite itself runs
	// in one) is not refused. Where 172.17.0.5 is not on this machine it
	// stops at binding; where it is (a container on that network), it
	// starts, so it is stopped once it is past the check. A separate
	// folder keeps that start away from the runs around it.
	n2 := newCNode(t, root, "ct2")
	a2 := launchWith(t, "", []string{"COYOTE_TEST_CONTAINER=none"}, upArgs(n2)...)
	deadline := time.After(2 * time.Minute)
	for past := false; !past; {
		select {
		case <-a2.exited:
			past = true
		case <-deadline:
			t.Fatalf("neither stopped nor started:\n%s", a2.out.String())
		case <-time.After(200 * time.Millisecond):
			past = strings.Contains(a2.out.String(), "Creating a new cluster")
		}
	}
	if strings.Contains(a2.out.String(), "inside a container") {
		t.Fatalf("refused outside a container:\n%s", a2.out.String())
	}
	select {
	case <-a2.exited:
	default:
		_ = interrupt(a2.cmd.Process)
		select {
		case <-a2.exited:
		case <-time.After(60 * time.Second):
			t.Fatalf("did not stop after Ctrl-C:\n%s", a2.out.String())
		}
	}
	waitNoNodeProcs(t, n2.dir, 15*time.Second)
	// With COYOTE_ADVERTISE the address is the person's choice: no refusal.
	out, _ = run("COYOTE_TEST_CONTAINER=docker", "COYOTE_ADVERTISE=127.0.0.1:"+strconv.Itoa(n.listen))
	if strings.Contains(out, "inside a container") || !strings.Contains(out, "Other machines will reach this one at 127.0.0.1:"+strconv.Itoa(n.listen)) {
		t.Fatalf("COYOTE_ADVERTISE not used:\n%s", out)
	}
	waitNoNodeProcs(t, n.dir, 15*time.Second)
}

// A PostgreSQL setting that can never work (as if set with ALTER SYSTEM)
// stops the node with the plain next step, instead of retrying forever.
func TestSetupPermanentStartErrorStops(t *testing.T) {
	root := setupRoot(t)
	n := newCNode(t, root, "pe")
	n.bootstrap()
	n.a.stop(t)
	waitNoNodeProcs(t, n.dir, 30*time.Second)
	f, err := os.OpenFile(filepath.Join(n.pgdata, "postgresql.auto.conf"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("shared_buffers = 'banana'\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	start := time.Now()
	n.launch("up", "")
	select {
	case <-n.a.exited:
	case <-time.After(3 * time.Minute):
		t.Fatalf("still retrying after 3 minutes:\n%s", n.a.out.String())
	}
	out := n.a.out.String()
	if code := n.a.cmd.ProcessState.ExitCode(); code != 1 || !strings.Contains(out, "PostgreSQL refused its settings") ||
		!strings.Contains(out, "ALTER SYSTEM") || !strings.Contains(out, "shared_buffers") {
		t.Fatalf("exit %d after %s:\n%s", code, time.Since(start).Round(time.Second), out)
	}
	if strings.Count(out, "Problem (will retry)") > 1 {
		t.Fatalf("retried before stopping:\n%s", out)
	}
	waitNoNodeProcs(t, n.dir, 30*time.Second)
}
