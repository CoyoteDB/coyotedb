//go:build integration

// Integration tests for milestone M3: the cluster wired into the program.
// Real `coyote up / join / witness` agents on localhost, each with its own
// data dir, cluster port, Postgres port and control port. Every wait is a
// poll with a deadline. Secrets (passwords, invite codes) are never
// logged by these tests.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	coyoteagent "coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/pg"
	"coyotedb.com/coyote/test/testports"
)

// cnode is one cluster node under test.
type cnode struct {
	*pgNode
	listen  int
	ctl     int
	client  int // client (proxy) port
	witness bool
	env     []string // extra environment for the agent (fault points)
}

func newCNode(t *testing.T, root, name string) *cnode {
	return &cnode{pgNode: newPGNode(t, root, name), listen: freePort(t), ctl: freePort(t), client: freePort(t)}
}

func newWitness(t *testing.T, root, name string) *cnode {
	n := newCNode(t, root, name)
	n.witness = true
	return n
}

// args is the agent command line: up, or join/witness with a code.
func (n *cnode) args(cmd, code string) []string {
	a := []string{cmd}
	if code != "" {
		a = append(a, code)
	}
	a = append(a, "--data-dir", n.dir, "--name", n.name, "--listen", "127.0.0.1:"+strconv.Itoa(n.listen),
		"--control-port", strconv.Itoa(n.ctl))
	if !n.witness {
		a = append(a, "--pg-port", strconv.Itoa(n.port), "--client-port", strconv.Itoa(n.client))
	}
	return a
}

// launch starts the agent without waiting.
func (n *cnode) launch(cmd, code string) *agent {
	n.t.Helper()
	n.a = launchCoyoteDBEnv(n.t, n.env, n.args(cmd, code)...)
	return n.a
}

// waitOut waits until the agent's output contains s.
func (n *cnode) waitOut(s string, within time.Duration) {
	n.t.Helper()
	deadline := time.Now().Add(within)
	for !strings.Contains(n.a.out.String(), s) {
		select {
		case <-n.a.exited:
			n.t.Fatalf("%s exited (%v) before %q:\n%s", n.name, n.a.err, s, n.a.out.String())
		default:
		}
		if time.Now().After(deadline) {
			n.t.Fatalf("%s: no %q after %s:\n%s", n.name, s, within, n.a.out.String())
		}
		time.Sleep(20 * time.Millisecond) // polling interval
	}
}

// bootstrap creates the cluster with this node as the main node.
func (n *cnode) bootstrap() {
	n.t.Helper()
	n.launchReady("up", "", 10*time.Minute)
}

// launchReady launches the agent and waits for its ready line. If the
// agent reports at start that one of its local ports is taken (another
// program grabbed it after the harness picked it; nothing was changed),
// the node gets new ports and is launched again.
func (n *cnode) launchReady(cmd, code string, within time.Duration) {
	n.t.Helper()
	for attempt := 1; ; attempt++ {
		n.launch(cmd, code)
		if n.a.waitReady(n.t, within) {
			return
		}
		out := n.a.out.String()
		if attempt < 4 && testports.InUse(out) {
			if strings.Contains(out, "the cluster port") {
				// A member's cluster port is its recorded address: only a
				// node that is not a member yet may move it.
				if _, err := os.Stat(filepath.Join(n.dir, "cluster", "identity.json")); err == nil {
					n.t.Fatalf("%s: its cluster port %d was taken by another program:\n%s", n.name, n.listen, out)
				}
				n.listen = freePort(n.t)
			}
			n.ctl, n.client, n.port = freePort(n.t), freePort(n.t), freePort(n.t)
			continue
		}
		n.t.Fatalf("%s exited (%v) before it was ready:\n%s", n.name, n.a.err, n.a.out.String())
	}
}

// join joins with a fresh invite from via and waits until it follows live
// (a data node) or runs (a witness).
func (n *cnode) join(via *cnode) {
	n.t.Helper()
	cmd := "join"
	if n.witness {
		cmd = "witness"
	}
	n.launchReady(cmd, via.invite(), 3*time.Minute)
	if !n.witness {
		n.waitOut("Following ", 60*time.Second)
	}
}

// restart runs plain `up` and waits for the ready banner.
func (n *cnode) restart() {
	n.t.Helper()
	n.launchReady("up", "", 3*time.Minute)
}

// cli runs a one-shot command against this node.
func (n *cnode) cli(args ...string) (string, error) {
	return runCoyoteDB(append(args, "--data-dir", n.dir)...)
}

func (n *cnode) mustCLI(args ...string) string {
	n.t.Helper()
	out, err := n.cli(args...)
	if err != nil {
		n.t.Fatalf("%s %v: %v\n%s", n.name, args, err, out)
	}
	return out
}

// invite returns a new invite code (never logged).
func (n *cnode) invite(extra ...string) string {
	n.t.Helper()
	var r coyoteagent.InviteResponse
	n.cliJSON(&r, append([]string{"invite", "--json"}, extra...)...)
	if !strings.HasPrefix(r.Code, "coyote-") {
		n.t.Fatal("invite: no code in the answer")
	}
	return r.Code
}

func (n *cnode) cliJSON(v any, args ...string) {
	n.t.Helper()
	out := n.mustCLI(args...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		n.t.Fatalf("%s %v: bad JSON: %v\n%s", n.name, args, err, out)
	}
}

func (n *cnode) clusterStatus() coyoteagent.Status {
	n.t.Helper()
	var s coyoteagent.Status
	n.cliJSON(&s, "status", "--json")
	return s
}

func (n *cnode) report() coyoteagent.NodeReport {
	n.t.Helper()
	var r coyoteagent.NodeReport
	n.cliJSON(&r, "dev", "status")
	return r
}

func (n *cnode) tryReport() (coyoteagent.NodeReport, bool) {
	var r coyoteagent.NodeReport
	out, err := n.cli("dev", "status")
	if err != nil || json.Unmarshal([]byte(out), &r) != nil {
		return r, false
	}
	return r, true
}

// isPrimary reports whether this node's own agent says it runs read-write.
func (n *cnode) isPrimary() bool {
	r, ok := n.tryReport()
	return ok && r.PG != nil && r.PG.Role == "primary"
}

func (n *cnode) isStreaming() bool {
	r, ok := n.tryReport()
	return ok && r.PG != nil && r.PG.Role == "standby" && r.PG.Streaming
}

func (n *cnode) dev(sub string, args ...string) string {
	n.t.Helper()
	return n.mustCLI(append([]string{"dev", sub}, args...)...)
}

// stopAll stops agents in order and checks nothing of theirs is left.
func stopAll(nodes ...*cnode) {
	for _, n := range nodes {
		if n.a != nil {
			select {
			case <-n.a.exited:
			default:
				n.a.stop(n.t)
			}
		}
	}
	for _, n := range nodes {
		waitNoNodeProcs(n.t, n.dir, 15*time.Second)
	}
}

// itemsOn creates the items table on the primary.
func itemsOn(t *testing.T, c *pgx.Conn) {
	t.Helper()
	mustExec(t, c, `CREATE TABLE items (id int PRIMARY KEY, body text)`)
	insertRange(t, c, 1, 100, "first-")
}

// waitAllSame polls until every node has the same items table.
func waitAllSame(t *testing.T, nodes ...*cnode) string {
	t.Helper()
	var h string
	for _, n := range nodes[1:] {
		h = waitSame(t, nodes[0].pgNode, n.pgNode)
	}
	return h
}

// noSecretsIn fails if any of the node's passwords appear in s.
func noSecretsIn(t *testing.T, s string, nodes ...*cnode) {
	t.Helper()
	for _, n := range nodes {
		if n.witness {
			continue
		}
		sec, err := pg.ReadSecretsFile(filepath.Join(n.dir, "cluster.secrets"))
		if err != nil {
			continue
		}
		for _, p := range []string{sec.Superuser, sec.Replication, sec.Rewind} {
			if p != "" && strings.Contains(s, p) {
				t.Fatalf("a password of %s appears in the output", n.name)
			}
		}
	}
}

// ---- 1, 2, 11: join, replicate, witness, status agreement, control ----

func TestM3JoinWitnessStatusControl(t *testing.T) {
	root := t.TempDir()
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	w := newWitness(t, root, "wit")
	ctx := context.Background()

	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	if !strings.Contains(b.a.out.String(), "Copying the data from node-a") {
		t.Fatalf("join output:\n%s", b.a.out.String())
	}
	insertRange(t, a.conn(), 101, 150, "second-")
	waitSame(t, a.pgNode, b.pgNode)

	// Rule 11: B's upstream is its local forwarder; A sees B on loopback;
	// both pg_hba files allow loopback only.
	cb := b.conn()
	ci := queryText(t, cb, "SHOW primary_conninfo")
	if !strings.Contains(ci, "host=127.0.0.1 ") {
		t.Fatal("B's primary_conninfo does not point at 127.0.0.1")
	}
	ca := a.conn()
	waitFor(t, 30*time.Second, "B in pg_stat_replication", func() bool {
		return queryInt(t, ca, "SELECT count(*) FROM pg_stat_replication WHERE application_name = 'node-b'") == 1
	})
	if k := queryInt(t, ca, "SELECT count(*) FROM pg_stat_replication WHERE client_addr IS NULL OR NOT (client_addr << '127.0.0.0/8'::inet OR client_addr = '::1'::inet)"); k != 0 {
		t.Fatalf("%d replication connections from a non-loopback address", k)
	}
	for _, n := range []*cnode{a, b} {
		cn := n.conn()
		if k := queryInt(t, cn, "SELECT count(*) FROM pg_hba_file_rules WHERE type <> 'local' AND (address IS NULL OR address NOT IN ('127.0.0.1', '::1'))"); k != 0 {
			t.Fatalf("%s: pg_hba has %d non-loopback rules", n.name, k)
		}
		if k := queryInt(t, cn, "SELECT count(*) FROM pg_hba_file_rules"); k == 0 {
			t.Fatalf("%s: no pg_hba rules read", n.name)
		}
	}

	// Sync default: the second data node is in R and S once it streams.
	waitFor(t, 60*time.Second, "node-b in the sync set", func() bool {
		s := queryText(t, ca, "SHOW synchronous_standby_names")
		return strings.Contains(s, "node-b")
	})

	c.join(a)
	w.join(b) // an invite from any member works
	insertRange(t, a.conn(), 151, 160, "third-")
	waitAllSame(t, a, b, c)

	// The witness has no Postgres and no pgdata.
	if _, err := os.Stat(w.pgdata); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("witness has a pgdata: %v", err)
	}
	if pm, wd := procs(t, w.pgdata); len(pm)+len(wd) != 0 {
		t.Fatalf("witness runs postgres %v / watchdog %v", pm, wd)
	}

	// status --json agrees everywhere: members, roles, suffrage (D21:
	// 3 data + 1 witness → all four vote), primary.
	nodes := []*cnode{a, b, c, w}
	type view struct {
		Primary string
		Members []string
	}
	viewOf := func(s coyoteagent.Status) view {
		v := view{Primary: s.Cluster.PrimaryName}
		for _, m := range s.Nodes {
			v.Members = append(v.Members, m.Name+"/"+m.Role+"/"+m.Suffrage+"/"+strconv.FormatBool(m.Primary))
		}
		slices.Sort(v.Members)
		return v
	}
	want := view{Primary: "node-a", Members: []string{
		"node-a/data/voter/true", "node-b/data/voter/false", "node-c/data/voter/false", "wit/witness/voter/false"}}
	waitFor(t, 60*time.Second, "every node's status to agree", func() bool {
		for _, n := range nodes {
			if v := viewOf(n.clusterStatus()); !slices.Equal(v.Members, want.Members) || v.Primary != want.Primary {
				return false
			}
		}
		return true
	})
	human := a.mustCLI("status")
	for _, s := range []string{"Main node: node-a", "node-b", "wit", "witness"} {
		if !strings.Contains(human, s) {
			t.Fatalf("status lacks %q:\n%s", s, human)
		}
	}

	// 11. Control channel: no token → 401; not reachable off loopback.
	addr := "127.0.0.1:" + strconv.Itoa(a.ctl)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+control.Prefix+"/status", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer wrong")
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: status %d, want 401", resp.StatusCode)
	}
	if err := privateFile(filepath.Join(a.dir, control.TokenFile)); err != nil {
		t.Fatalf("token file: %v", err)
	}
	if ip := nonLoopbackIP(); ip != "" {
		d := net.Dialer{Timeout: 2 * time.Second}
		if conn, err := d.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(a.ctl))); err == nil {
			conn.Close()
			t.Fatalf("control port reachable on %s", ip)
		}
	} else {
		t.Log("no non-loopback address on this machine; off-loopback check skipped")
	}

	for _, n := range nodes {
		noSecretsIn(t, n.a.out.String(), a, b, c)
	}
	noSecretsIn(t, human, a, b, c)
	stopAll(w, c, b, a)
}

func nonLoopbackIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, ad := range addrs {
		if ipn, ok := ad.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			return ipn.IP.String()
		}
	}
	return ""
}

// ---- 3, 4: sync really waits; restart in another order ----

func TestM3SyncWaitsAndRestartOrder(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	ca := a.conn()
	waitFor(t, 60*time.Second, "node-b in synchronous_standby_names", func() bool {
		return strings.Contains(queryText(t, ca, "SHOW synchronous_standby_names"), "node-b")
	})
	sysA := controlState(t, a.pgdata).SystemID

	// 3. Stop B: a commit on A waits; start B: it completes.
	b.a.stop(t)
	waitNoNodeProcs(t, b.dir, 15*time.Second)
	done := make(chan error, 1)
	go func() {
		c, err := pgx.Connect(context.Background(), connString(t, a))
		if err != nil {
			done <- err
			return
		}
		defer c.Close(context.Background())
		_, err = c.Exec(context.Background(), "INSERT INTO items VALUES (1000, 'waits-for-b')")
		done <- err
	}()
	waitFor(t, 30*time.Second, "the commit to wait for the sync standby", func() bool {
		return queryInt(t, ca, "SELECT count(*) FROM pg_stat_activity WHERE wait_event = 'SyncRep'") == 1
	})
	select {
	case err := <-done:
		t.Fatalf("commit finished without the sync standby: %v", err)
	default:
	}
	st := a.clusterStatus()
	if !slices.ContainsFunc(st.Warnings, func(s string) bool { return strings.Contains(s, "Writes are waiting") }) {
		t.Fatalf("status does not say writes wait: %v", st.Warnings)
	}
	b.restart()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("commit did not complete after the standby came back")
	}
	waitSame(t, a.pgNode, b.pgNode)

	// 4. Stop both, start in the other order: A stays primary, no re-clone.
	startsB := primaryStarts(t, b.pgNode)
	stopAll(b, a)
	b.launch("up", "")
	a.restart()
	b.waitOut("Press Ctrl-C to stop.", 2*time.Minute)
	waitFor(t, 60*time.Second, "A primary, B streaming", func() bool { return a.isPrimary() && b.isStreaming() })
	if strings.Contains(b.a.out.String(), "Copying") || strings.Contains(a.a.out.String(), "Copying") {
		t.Fatal("a restart re-cloned")
	}
	if got := controlState(t, a.pgdata).SystemID; got != sysA {
		t.Fatal("A's database was replaced")
	}
	if primaryStarts(t, b.pgNode) != startsB {
		t.Fatal("B started read-write")
	}
	insertRange(t, a.conn(), 2000, 2009, "after-restart-")
	waitSame(t, a.pgNode, b.pgNode)
	stopAll(b, a)
}

func connString(t *testing.T, n *cnode) string {
	t.Helper()
	out, err := n.cli("conninfo", "--admin")
	if err != nil {
		t.Fatalf("conninfo: %v", err)
	}
	return strings.TrimSpace(out)
}

// ---- 5: dev switch under a continuous writer ----

func TestM3SwitchUnderLoad(t *testing.T) {
	root := t.TempDir()
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	c.join(a)
	waitFor(t, 60*time.Second, "both standbys in the sync set", func() bool {
		s := a.clusterStatus()
		return len(s.Cluster.SyncSet) == 2
	})

	// The writer writes to whichever node is primary and records every
	// acknowledged id.
	var mu sync.Mutex
	var acked []int
	var stop atomic.Bool
	nodes := []*cnode{a, b, c}
	urls := map[*cnode]string{}
	for _, n := range nodes {
		urls[n] = connString(t, n)
	}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		id := 10000
		var conn *pgx.Conn
		for !stop.Load() {
			if conn == nil {
				for _, n := range nodes {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					cc, err := pgx.Connect(ctx, urls[n])
					cancel()
					if err != nil {
						continue
					}
					var ro bool
					if cc.QueryRow(context.Background(), "SELECT pg_is_in_recovery()").Scan(&ro) == nil && !ro {
						conn = cc
						break
					}
					cc.Close(context.Background())
				}
				if conn == nil {
					time.Sleep(50 * time.Millisecond) // retry interval while no primary answers
					continue
				}
			}
			id++
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, err := conn.Exec(ctx, "INSERT INTO items VALUES ($1, 'load')", id)
			cancel()
			if err != nil {
				conn.Close(context.Background())
				conn = nil
				continue
			}
			mu.Lock()
			acked = append(acked, id)
			mu.Unlock()
		}
		if conn != nil {
			conn.Close(context.Background())
		}
	}()
	waitFor(t, 30*time.Second, "writes before the switch", func() bool { mu.Lock(); defer mu.Unlock(); return len(acked) > 200 })

	out := a.dev("switch", "--to", "node-b")
	if !strings.Contains(out, "node-b is now the main node") {
		t.Fatalf("switch: %s", out)
	}
	mu.Lock()
	before := len(acked)
	mu.Unlock()
	waitFor(t, 60*time.Second, "writes on the new primary", func() bool { mu.Lock(); defer mu.Unlock(); return len(acked) > before+200 })
	stop.Store(true)
	<-writerDone

	waitFor(t, 2*time.Minute, "node-a back as a streaming standby", a.isStreaming)
	waitFor(t, 60*time.Second, "node-c following node-b", c.isStreaming)
	h := waitAllSame(t, b, a, c)
	t.Logf("after switch: %s, %d acknowledged writes", h, len(acked))
	for _, n := range nodes {
		cn := n.conn()
		missing := queryInt(t, cn, "SELECT count(*) FROM unnest($1::int[]) id WHERE NOT EXISTS (SELECT 1 FROM items i WHERE i.id = id)", acked)
		if missing != 0 {
			t.Fatalf("%s misses %d acknowledged writes", n.name, missing)
		}
	}
	// The old primary follows the new timeline.
	rb, ra := b.report(), a.report()
	if rb.PG.Timeline < 2 || ra.PG.Timeline != rb.PG.Timeline || ra.PG.Role != "standby" {
		t.Fatalf("timelines: new primary %d, old primary %d (%s)", rb.PG.Timeline, ra.PG.Timeline, ra.PG.Role)
	}
	stopAll(c, a, b)
}

// ---- 6, 7: crash, switch, isolated former primary, rejoin ----

func TestM3CrashSwitchIsolatedRejoin(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	w := newWitness(t, root, "wit")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	w.join(a)
	waitSame(t, a.pgNode, b.pgNode)

	// Make diverging rows: async, cut B's stream, write on A, crash A.
	b.dev("cut-stream") // before set-sync, so the switch below may not need the target in R
	a.dev("set-sync", "--standbys", "")
	insertRange(t, a.conn(), 500, 509, "diverged-")
	startsA := primaryStarts(t, a.pgNode)
	a.crash()

	out := w.dev("switch", "--to", "node-b")
	if !strings.Contains(out, "node-b is now the main node") {
		t.Fatalf("switch: %s", out)
	}
	insertRange(t, b.conn(), 600, 609, "new-primary-")
	hb := queryText(t, b.conn(), tableHash)

	// 7. Isolated: stop B and the witness, start A alone. It must not
	// come up read-write (no quorum to confirm it is still the main node).
	stopAll(w, b)
	a.launch("up", "")
	a.waitOut("Waiting to reach the other nodes", 60*time.Second)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if pm, _ := procs(t, a.pgdata); len(pm) != 0 {
			t.Fatalf("isolated former primary started Postgres:\n%s", a.a.out.String())
		}
		time.Sleep(100 * time.Millisecond) // polling interval
	}

	// 6. B and the witness return; A rejoins as a standby by itself.
	b.restart()
	w.restart()
	a.waitOut("Following node-b live.", 3*time.Minute)
	if !strings.Contains(a.a.out.String(), "Rejoin") && !strings.Contains(a.a.out.String(), "Rewound") {
		t.Fatalf("A did not report rejoining:\n%s", a.a.out.String())
	}
	if primaryStarts(t, a.pgNode) != startsA {
		t.Fatal("the old primary accepted writes again")
	}
	if got := waitSame(t, a.pgNode, b.pgNode); got != hb {
		t.Fatalf("A's data %s, B's %s", got, hb)
	}
	if k := queryInt(t, a.conn(), "SELECT count(*) FROM items WHERE body LIKE 'diverged-%'"); k != 0 {
		t.Fatalf("%d diverged rows survived on the old primary", k)
	}
	if _, err := a.conn().Exec(context.Background(), "INSERT INTO items VALUES (999999, 'x')"); err == nil || sqlState(err) != "25006" {
		t.Fatalf("write on the old primary: %v", err)
	}
	if a.marker() != nil {
		t.Fatalf("marker left: %+v", a.marker())
	}
	stopAll(w, a, b)
}

// ---- 8: join failures ----

func TestM3JoinFailures(t *testing.T) {
	root := t.TempDir()
	a := newCNode(t, root, "node-a")
	a.bootstrap()

	// Garbled: a friendly message, at once, nothing created.
	g := newCNode(t, root, "garbled")
	start := time.Now()
	out, err := runCoyoteDB(g.args("join", "coyote-0000garbled0000")...)
	if err == nil || !strings.Contains(out, "invite code") || time.Since(start) > 5*time.Second {
		t.Fatalf("garbled code: %v (%s)\n%s", err, time.Since(start), out)
	}
	if _, err := os.Stat(g.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a garbled code created the data folder")
	}

	// Expired.
	var r coyoteagent.InviteResponse
	a.cliJSON(&r, "invite", "--json", "--ttl", "1s")
	waitFor(t, 10*time.Second, "the invite to expire", func() bool { return time.Now().After(r.Expires.Add(time.Second)) })
	e := newCNode(t, root, "expired")
	out, err = runCoyoteDB(e.args("join", r.Code)...)
	if err == nil || !strings.Contains(out, "expired") {
		t.Fatalf("expired code: %v\n%s", err, out)
	}

	// Reused by another machine.
	code := a.invite()
	b := newCNode(t, root, "node-b")
	b.launch("join", code)
	b.waitOut("Following ", 3*time.Minute)
	x := newCNode(t, root, "node-x")
	out, err = runCoyoteDB(x.args("join", code)...)
	if err == nil || !strings.Contains(out, "already used") {
		t.Fatalf("reused code: %v\n%s", err, out)
	}

	// First node down: retries, then a resumable error; later it works.
	code = a.invite()
	stopAll(b, a)
	d := newCNode(t, root, "node-d")
	out, err = runCoyoteDB(d.args("join", code)...)
	if err == nil || !strings.Contains(out, "run the same command again") {
		t.Fatalf("join with the cluster down: %v\n%s", err, out)
	}
	a.restart()
	b.restart()
	d.launch("join", code)
	d.waitOut("Following ", 3*time.Minute)
	itemsOn(t, a.conn())
	waitAllSame(t, a, b, d)
	for _, s := range []string{code, r.Code} {
		if strings.Contains(d.a.out.String(), s) || strings.Contains(a.a.out.String(), s) {
			t.Fatal("an invite code was printed by an agent")
		}
	}
	stopAll(d, b, a)
}

// ---- 9: leave and remove ----

func TestM3LeaveRemove(t *testing.T) {
	root := t.TempDir()
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	c.join(a)
	ca := a.conn()
	waitFor(t, 30*time.Second, "slot for node-c", func() bool {
		return queryInt(t, ca, "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'coyote_node_c'") == 1
	})

	// Removing the main node is refused.
	if out, err := b.cli("remove", "node-a"); err == nil || !strings.Contains(out, "main node") {
		t.Fatalf("remove primary: %v\n%s", err, out)
	}

	// Remove C (it is in the sync set): its agent stops Postgres and says why.
	out := a.mustCLI("remove", "node-c")
	if !strings.Contains(out, "Removed node-c") {
		t.Fatalf("remove: %s", out)
	}
	select {
	case <-c.a.exited:
	case <-time.After(60 * time.Second):
		t.Fatalf("removed node kept running:\n%s", c.a.out.String())
	}
	if c.a.err == nil || !strings.Contains(c.a.out.String(), "removed from the cluster") {
		t.Fatalf("removed node: %v\n%s", c.a.err, c.a.out.String())
	}
	waitNoNodeProcs(t, c.dir, 15*time.Second)
	waitFor(t, 30*time.Second, "node-c's slot dropped", func() bool {
		return queryInt(t, ca, "SELECT count(*) FROM pg_replication_slots WHERE slot_name = 'coyote_node_c'") == 0
	})
	if s := queryText(t, ca, "SHOW synchronous_standby_names"); strings.Contains(s, "node-c") {
		t.Fatalf("node-c still in S: %s", s)
	}
	// It is refused if it comes back.
	out, err := runCoyoteDB(c.args("up", "")...)
	if err == nil || !strings.Contains(out, "removed from the cluster") {
		t.Fatalf("removed node restarted: %v\n%s", err, out)
	}

	// B leaves.
	out = b.mustCLI("leave")
	if !strings.Contains(out, "Left the cluster") {
		t.Fatalf("leave: %s", out)
	}
	select {
	case <-b.a.exited:
	case <-time.After(60 * time.Second):
		t.Fatal("left node kept running")
	}
	if b.a.err != nil {
		t.Fatalf("leave should end the agent cleanly: %v\n%s", b.a.err, b.a.out.String())
	}
	waitNoNodeProcs(t, b.dir, 15*time.Second)
	waitFor(t, 30*time.Second, "a one-node cluster", func() bool {
		s := a.clusterStatus()
		return len(s.Nodes) == 1 && len(s.Cluster.SyncSet) == 0
	})
	// With no sync standby left, writes go through.
	insertRange(t, a.conn(), 200, 209, "alone-")
	stopAll(a)
}

// TestM3RemoveDuringSyncAdd: `remove` runs while the controller is adding
// that very node to the sync set (its write proposed, not yet applied).
// The voters' Raft logs are held up for a few seconds from the moment
// node-c is admitted, so the controller's add of node-c waits in Raft and
// the remove starts inside that window. The remove must still hand node-c
// over and succeed (D59).
func TestM3RemoveDuringSyncAdd(t *testing.T) {
	root := t.TempDir()
	stall := []string{"COYOTE_TEST_RAFT_STALL_FILE=raft-stall"}
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	b.env, c.env = stall, stall
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	waitFor(t, 60*time.Second, "node-b in the sync set", func() bool {
		return memberNamed(a.clusterStatus(), "node-b").InSyncSet
	})

	// node-c joins. Once it is admitted, and before it streams (the
	// controller adds it only then), node-b's and node-c's Raft log writes
	// are held up for 4 s, below the lease's tolerance for a stalled disk
	// (D33). The leader, node-a, keeps working, so the controller's
	// sync-set write for node-c is proposed at once, but with two of the
	// three voters stalled it cannot commit: the remove below starts
	// while that write is on its way and node-a's state does not show it.
	if s := a.clusterStatus(); s.Cluster.LeaderName != "node-a" {
		t.Fatalf("the Raft leader is %q, not node-a", s.Cluster.LeaderName)
	}
	logLen := map[*cnode]int{a: len(agentLog(a))}
	code := a.invite()
	c.launch("join", code)
	c.waitOut("Joined.", 3*time.Minute)
	end := time.Now().Add(4 * time.Second)
	for _, n := range []*cnode{b, c} {
		if err := os.WriteFile(filepath.Join(n.dir, "cluster", "raft-stall"), []byte(strconv.FormatInt(end.UnixNano(), 10)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 4*time.Second, "the controller starting to add node-c to the sync set", func() bool {
		return strings.Contains(agentLog(a)[logLen[a]:], "step=sync-add")
	})
	if time.Until(end) < time.Second {
		t.Fatalf("the controller started adding node-c only %s before the stall ends; the race cannot be set up", time.Until(end))
	}
	// node-a's own view does not show node-c in the sync set yet: the add
	// is on its way, not applied.
	if memberNamed(a.clusterStatus(), "node-c").InSyncSet {
		t.Fatal("node-c is already in the sync set; the race was not produced")
	}

	out := a.mustCLI("remove", "node-c")
	if !strings.Contains(out, "Removed node-c") {
		t.Fatalf("remove: %s", out)
	}
	// The race happened: the removal was first refused (node-c had
	// entered the sync set meanwhile) and node-c was handed over.
	if !strings.Contains(agentLog(a)[logLen[a]:], "member is in the sync set; the main node hands it over first") {
		t.Fatal("the remove was never refused for the sync set; the race was not produced")
	}
	select {
	case <-c.a.exited:
	case <-time.After(60 * time.Second):
		t.Fatalf("removed node kept running:\n%s", c.a.out.String())
	}
	waitNoNodeProcs(t, c.dir, 15*time.Second)
	s := a.clusterStatus()
	if len(s.Nodes) != 2 || slices.Contains(s.Cluster.SyncSet, "node-c") {
		t.Fatalf("after the remove: %+v", s.Cluster)
	}
	waitFor(t, 30*time.Second, "node-c out of the main node's sync list", func() bool {
		return !strings.Contains(queryText(t, a.conn(), "SHOW synchronous_standby_names"), "node-c")
	})
	// node-b stays a synchronous copy: writes still wait for it.
	if !memberNamed(a.clusterStatus(), "node-b").InSyncSet {
		t.Fatal("node-b left the sync set")
	}
	insertRange(t, a.conn(), 300, 309, "after-")
	stopAll(b, a)
}

// ---- 10: kill -9 mid-join clone, mid-switch, steady state ----

func TestM3Kill9Recovers(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	a.env = []string{"COYOTE_TEST_FAULT=switch-after-cut"}
	a.bootstrap()
	itemsOn(t, a.conn())
	bigTable(t, a.conn(), "big1")

	// Mid-join clone.
	code := a.invite()
	ag := b.launch("join", code)
	waitToolThenKill(t, "pg_basebackup", b.dir, ag)
	waitNoNodeProcs(t, b.dir, 15*time.Second)
	if _, err := os.Stat(b.pgdata); err == nil {
		t.Fatal("clone finished before the kill (make big1 bigger)")
	}
	b.launch("join", code) // the same command resumes
	b.waitOut("Following node-a live.", 3*time.Minute)
	waitSame(t, a.pgNode, b.pgNode)

	insertRange(t, a.conn(), 3000, 3009, "before-switch-")

	// Mid-switch, after the streams are cut (A is the leader: the only
	// voter). Nothing changed in the cluster state: A comes back primary.
	switchOut := make(chan string, 1)
	go func() {
		out, _ := b.cli("dev", "switch", "--to", "node-b")
		switchOut <- out
	}()
	a.waitOut("Test fault point reached: switch-after-cut", 60*time.Second)
	a.crash()
	<-switchOut
	a.env = []string{"COYOTE_TEST_FAULT=switch-after-cas"}
	a.restart()
	if !a.isPrimary() {
		t.Fatal("A is not primary after the interrupted switch")
	}

	// Rerunning the switch goes further; kill A after the compare-and-set:
	// the new state stands, B becomes primary, A rejoins.
	go func() {
		out, _ := b.cli("dev", "switch", "--to", "node-b")
		switchOut <- out
	}()
	a.waitOut("Test fault point reached: switch-after-cas", 60*time.Second)
	// After the compare-and-set A is no longer the main node: its lease
	// loop may already have stopped its Postgres (M4, rule 4).
	a.crashWhatever()
	<-switchOut
	a.env = nil
	a.launch("up", "") // rejoins as a standby (B promotes once A, the voter, confirms it)
	waitFor(t, 2*time.Minute, "B primary and A streaming", func() bool { return b.isPrimary() && a.isStreaming() })
	insertRange(t, b.conn(), 4000, 4009, "on-b-")
	waitSame(t, a.pgNode, b.pgNode)
	if k := queryInt(t, b.conn(), "SELECT count(*) FROM items WHERE body LIKE 'before-switch-%'"); k != 10 {
		t.Fatalf("acknowledged rows lost: %d of 10", k)
	}

	// Steady state.
	b.crash()
	b.restart()
	waitFor(t, 2*time.Minute, "B primary and A streaming again", func() bool { return b.isPrimary() && a.isStreaming() })
	insertRange(t, b.conn(), 5000, 5009, "after-steady-crash-")
	waitSame(t, a.pgNode, b.pgNode)
	stopAll(a, b)

	for _, p := range listProcs(t) {
		for _, tool := range []string{"pg_basebackup", "pg_rewind"} {
			if strings.Contains(p.cmd, tool) && strings.Contains(p.cmd, root) {
				t.Fatalf("stray %s: %+v", tool, p)
			}
		}
	}
}

// memberNamed returns the member called name in s (zero if none).
func memberNamed(s coyoteagent.Status, name string) coyoteagent.MemberView {
	for _, m := range s.Nodes {
		if m.Name == name {
			return m
		}
	}
	return coyoteagent.MemberView{}
}

// agentLog returns n's agent log ("" before it exists).
func agentLog(n *cnode) string {
	b, _ := os.ReadFile(filepath.Join(n.dir, "logs", "coyote.log"))
	return string(b)
}
