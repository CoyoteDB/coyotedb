//go:build integration

// Integration tests for milestone M2: copying by hand. Several real
// Postgres nodes on localhost, each under its own `coyote dev manual-up`
// agent (the M2 single-node agent, kept for these tools since `up` now
// creates a cluster), driven with the manual `coyote dev` commands. Waits
// are polls with deadlines.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"coyotedb.com/coyote/internal/pg"
)

// pgNode is one node: a state dir, a Postgres port and (when running) its agent.
type pgNode struct {
	t      *testing.T
	name   string
	dir    string
	pgdata string
	port   int
	a      *agent
}

func newPGNode(t *testing.T, root, name string) *pgNode {
	dir := filepath.Join(root, "node "+name) // space on purpose
	// Runs before the temp dir is removed (cleanups run last first).
	t.Cleanup(func() {
		if t.Failed() {
			logTail(t, name, filepath.Join(dir, "logs", "coyote.log"), 80)
		}
	})
	return &pgNode{t: t, name: name, dir: dir, pgdata: filepath.Join(dir, "pgdata"), port: freePort(t)}
}

// logTail logs the last lines of a node's agent log, so a failure on a
// remote machine can be read from the test output alone.
func logTail(t *testing.T, name, path string, lines int) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	all := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	t.Logf("%s: last %d lines of %s:\n%s", name, len(all), path, strings.Join(all, "\n"))
}

func (n *pgNode) addr() string { return "127.0.0.1:" + strconv.Itoa(n.port) }

func (n *pgNode) upArgs(extra ...string) []string {
	return append([]string{"dev", "manual-up", "--data-dir", n.dir, "--pg-port", strconv.Itoa(n.port), "--name", n.name}, extra...)
}

func (n *pgNode) up() {
	n.t.Helper()
	n.a = startCoyoteDB(n.t, n.upArgs()...)
}

func (n *pgNode) cloneArgs(src *pgNode) []string {
	return n.upArgs("--clone-from", src.addr(), "--upstream-password-file", filepath.Join(src.dir, "cluster.secrets"))
}

func (n *pgNode) cloneFrom(src *pgNode) {
	n.t.Helper()
	n.a = startCoyoteDB(n.t, n.cloneArgs(src)...)
	if !strings.Contains(n.a.out.String(), "standby (read-only) of "+src.addr()) {
		n.t.Fatalf("%s: not reported as standby:\n%s", n.name, n.a.out.String())
	}
}

// stop stops the agent cleanly and waits until nothing of this node runs.
func (n *pgNode) stop() {
	n.t.Helper()
	n.a.stop(n.t)
	waitNoNodeProcs(n.t, n.dir, 10*time.Second)
}

// dev runs `coyote dev <sub> --data-dir <dir> args...` and requires success.
func (n *pgNode) dev(sub string, args ...string) string {
	n.t.Helper()
	out, err := n.devErr(sub, args...)
	if err != nil {
		n.t.Fatalf("dev %s %v on %s: %v\n%s", sub, args, n.name, err, out)
	}
	return out
}

func (n *pgNode) devErr(sub string, args ...string) (string, error) {
	return runCoyoteDB(append([]string{"dev", sub, "--data-dir", n.dir}, args...)...)
}

// runCoyoteDB runs a one-shot coyote command to completion.
func runCoyoteDB(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, coyoteBin, args...)
	cmd.Env = append(os.Environ(), "COYOTE_PG_CACHE="+pgCache)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

type nodeStatus struct {
	Name      string         `json:"name"`
	Running   bool           `json:"running"`
	Marker    *pg.Marker     `json:"rejoin_marker"`
	Persisted pg.ReplState   `json:"persisted"`
	Postgres  *pg.ReplStatus `json:"postgres"`
}

func (n *pgNode) status() nodeStatus {
	n.t.Helper()
	var s nodeStatus
	out := n.dev("status")
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		n.t.Fatalf("status JSON: %v\n%s", err, out)
	}
	return s
}

func (n *pgNode) conn() *pgx.Conn {
	n.t.Helper()
	return connect(n.t, n.dir)
}

func (n *pgNode) marker() *pg.Marker {
	n.t.Helper()
	m, err := pg.Node{StateDir: n.dir}.ReadMarker()
	if err != nil {
		n.t.Fatal(err)
	}
	return m
}

// ---- SQL helpers ----

func mustExec(t *testing.T, c *pgx.Conn, sql string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func queryInt(t *testing.T, c *pgx.Conn, sql string, args ...any) int {
	t.Helper()
	var v int
	if err := c.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

func queryText(t *testing.T, c *pgx.Conn, sql string, args ...any) string {
	t.Helper()
	var v string
	if err := c.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// tableHash is an order-independent fingerprint of a table's contents.
const tableHash = `SELECT count(*) || ':' || coalesce(md5(string_agg(t::text, ',' ORDER BY id)), '') FROM items t`

// waitFor polls cond until true or the deadline.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(20 * time.Millisecond) // polling interval
	}
}

// waitSame polls until both nodes have identical items tables.
func waitSame(t *testing.T, a, b *pgNode) string {
	t.Helper()
	ca, cb := a.conn(), b.conn()
	var ha, hb string
	// A sync commit returns once a copy has flushed the WAL, not replayed
	// it: right after a CREATE TABLE the copy may not have the table yet.
	hash := func(c *pgx.Conn) (string, bool) {
		var v string
		err := c.QueryRow(context.Background(), tableHash).Scan(&v)
		if sqlState(err) == "42P01" { // undefined_table: not replayed yet
			return "", false
		}
		if err != nil {
			t.Fatalf("%s: %v", tableHash, err)
		}
		return v, true
	}
	waitFor(t, 60*time.Second, "identical data on "+a.name+" and "+b.name, func() bool {
		var okA, okB bool
		ha, okA = hash(ca)
		hb, okB = hash(cb)
		return okA && okB && ha == hb
	})
	return ha
}

func insertRange(t *testing.T, c *pgx.Conn, from, to int, tag string) {
	t.Helper()
	mustExec(t, c, `INSERT INTO items SELECT g, $3 || g FROM generate_series($1::int, $2::int) g`, from, to, tag)
}

// newPrimary starts a primary with an items table holding rows 1..100.
func newPrimary(t *testing.T, root, name string) *pgNode {
	t.Helper()
	p := newPGNode(t, root, name)
	p.up()
	c := p.conn()
	mustExec(t, c, `CREATE TABLE items (id int PRIMARY KEY, body text)`)
	insertRange(t, c, 1, 100, "first-")
	return p
}

// ---- process helpers ----

// nodeProcs lists processes whose command line names the node's state
// dir: agents, watchdogs, postmasters, pg_basebackup (and its forked WAL
// streamer), pg_rewind, single-user postgres.
func nodeProcs(t *testing.T, dir string) []proc {
	var out []proc
	for _, p := range listProcs(t) {
		if !strings.Contains(p.cmd, dir) {
			continue
		}
		// Another node's agent may name this node's secrets file
		// (--upstream-password-file); only count our own agent.
		bin, d := argForm(coyoteBin), argForm(dir)
		if strings.HasPrefix(p.cmd, bin+" ") && !strings.HasPrefix(p.cmd, bin+" "+pg.WatchdogCommand) &&
			!strings.Contains(p.cmd, "--data-dir "+d+" ") && !strings.HasSuffix(p.cmd, "--data-dir "+d) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func waitNoNodeProcs(t *testing.T, dir string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		left := nodeProcs(t, dir)
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still running after %s: %+v", within, left)
		}
		time.Sleep(20 * time.Millisecond) // polling interval
	}
}

func waitGroupGone(t *testing.T, pgid int) {
	t.Helper()
	waitFor(t, 30*time.Second, fmt.Sprintf("process group %d to exit", pgid), func() bool {
		return postmasterGroupGone(t, pgid)
	})
}

// crash simulates a power cut of a node: freeze its agent, watchdog and
// postmaster, then SIGKILL all three, so Postgres gets no chance to shut
// down cleanly. Returns after every process of the node is gone.
func (n *pgNode) crash() {
	n.t.Helper()
	pf, err := pg.ReadPidFile(n.pgdata)
	if err != nil || pf == nil {
		n.t.Fatalf("no postmaster.pid: %v", err)
	}
	_, wd := procs(n.t, n.pgdata)
	victims := append([]int{n.a.cmd.Process.Pid, pf.PID}, wd...)
	for _, p := range victims {
		_ = freezePID(p)
	}
	for _, p := range []int{pf.PID, n.a.cmd.Process.Pid} {
		_ = killPID(p)
	}
	for _, p := range wd {
		_ = killPID(p)
	}
	<-n.a.exited
	waitGroupGone(n.t, pf.PID) // backends notice the postmaster died and exit
	waitNoNodeProcs(n.t, n.dir, 10*time.Second)
}

// crashWhatever kill -9s the agent, its watchdog and its postmaster if it
// still has one (a deposed primary may already have stopped its own).
func (n *pgNode) crashWhatever() {
	n.t.Helper()
	pf, _ := pg.ReadPidFile(n.pgdata)
	_, wd := procs(n.t, n.pgdata)
	victims := append([]int{n.a.cmd.Process.Pid}, wd...)
	if pf != nil {
		victims = append(victims, pf.PID)
	}
	for _, p := range victims {
		_ = freezePID(p)
	}
	for _, p := range victims {
		_ = killPID(p)
	}
	<-n.a.exited
	if pf != nil {
		waitGroupGone(n.t, pf.PID)
	}
	waitNoNodeProcs(n.t, n.dir, 10*time.Second)
}

func controlState(t *testing.T, pgdata string) pg.ControlInfo {
	t.Helper()
	build, err := pg.CurrentBuild()
	if err != nil {
		t.Fatal(err)
	}
	inst := pg.Install{Root: filepath.Join(pgCache, build.Name()), Build: build}
	cmd := exec.Command(inst.Bin("pg_controldata"), "-D", pgdata)
	cmd.Env = append(withoutPGEnv(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pg_controldata: %v\n%s", err, out)
	}
	ci, err := pg.ParseControlData(string(out))
	if err != nil {
		t.Fatal(err)
	}
	return ci
}

// primaryStarts counts how often this node's Postgres logged that it
// started accepting read-write connections.
func primaryStarts(t *testing.T, n *pgNode) int {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(n.dir, "logs", "postgresql-*.log"))
	k := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		k += bytes.Count(b, []byte("database system is ready to accept connections"))
	}
	return k
}

func secretsOf(t *testing.T, n *pgNode) pg.Secrets {
	t.Helper()
	s, err := pg.ReadSecretsFile(filepath.Join(n.dir, "cluster.secrets"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// ---- tests ----

// 1. Clone + stream.
func TestM2CloneAndStream(t *testing.T) {
	root := t.TempDir()
	a := newPrimary(t, root, "n1")
	b := newPGNode(t, root, "n2")
	b.cloneFrom(a)

	ca, cb := a.conn(), b.conn()
	waitFor(t, 30*time.Second, "cloned rows on standby", func() bool { return queryInt(t, cb, "SELECT count(*) FROM items") == 100 })
	insertRange(t, ca, 101, 200, "streamed-")
	waitSame(t, a, b)

	sa := a.status()
	if sa.Postgres.Role != "primary" || len(sa.Postgres.Standbys) != 1 {
		t.Fatalf("primary status: %+v", sa.Postgres)
	}
	if r := sa.Postgres.Standbys[0]; r.Name != "n2" || r.State != "streaming" || r.Slot != "coyote_n2" || r.SyncState != "async" {
		t.Fatalf("pg_stat_replication row: %+v", r)
	}
	sb := b.status()
	w := sb.Postgres.WalReceiver
	if sb.Postgres.Role != "standby" || w == nil || w.Status != "streaming" || w.Slot != "coyote_n2" || w.SenderPort != a.port {
		t.Fatalf("standby status: %+v %+v", sb.Postgres, w)
	}

	// Read-only.
	_, err := cb.Exec(context.Background(), `INSERT INTO items VALUES (9999, 'nope')`)
	if sqlState(err) != "25006" {
		t.Fatalf("write on standby: want read_only_sql_transaction, got %v", err)
	}

	// Secrets: private files, never on a command line, never printed.
	for _, f := range []string{"cluster.secrets", "superuser.password", "upstream.pgpass"} {
		if err := privateFile(filepath.Join(b.dir, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	s := secretsOf(t, a)
	for _, p := range listProcs(t) {
		for _, pw := range []string{s.Superuser, s.Replication, s.Rewind} {
			if strings.Contains(p.cmd, pw) {
				t.Fatalf("a password is visible in the process list (pid %d)", p.pid)
			}
		}
	}
	for _, out := range []string{a.a.out.String(), b.a.out.String()} {
		for _, pw := range []string{s.Superuser, s.Replication, s.Rewind} {
			if strings.Contains(out, pw) {
				t.Fatal("a password was printed by up")
			}
		}
	}
	b.stop()
	a.stop()
}

// 2. Sync really waits: acknowledged means on two nodes.
func TestM2SyncReallyWaits(t *testing.T) {
	root := t.TempDir()
	a := newPrimary(t, root, "n1")
	b := newPGNode(t, root, "n2")
	b.cloneFrom(a)

	out := a.dev("set-sync", "--standbys", "n2")
	if !strings.Contains(out, "wait for 1 of: n2") {
		t.Fatalf("set-sync output: %s", out)
	}
	sa := a.status()
	if sa.Postgres.SyncStandbyNames != `ANY 1 ("n2")` || sa.Postgres.Standbys[0].SyncState != "quorum" {
		t.Fatalf("sync not in effect: %q %+v", sa.Postgres.SyncStandbyNames, sa.Postgres.Standbys)
	}
	ca := a.conn()
	insertRange(t, ca, 101, 101, "sync-")

	b.stop()
	writer := a.conn()
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_, err := writer.Exec(ctx, `INSERT INTO items VALUES (102, 'waits-for-standby')`)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("commit returned with the sync standby down (err=%v)", err)
	case <-time.After(4 * time.Second): // asserting that nothing happens for this long
	}
	if k := queryInt(t, ca, `SELECT count(*) FROM pg_stat_activity WHERE wait_event = 'SyncRep'`); k != 1 {
		t.Fatalf("want 1 backend waiting on SyncRep, got %d", k)
	}
	b.up()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("commit after standby returned: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("commit still waiting after the standby came back")
	}
	cb := b.conn()
	if queryInt(t, cb, "SELECT count(*) FROM items WHERE id = 102") != 1 {
		t.Fatal("acknowledged row missing on the standby")
	}

	a.dev("set-sync", "--standbys", "")
	if s := a.status(); s.Postgres.SyncStandbyNames != "" || s.Postgres.Standbys[0].SyncState != "async" {
		t.Fatalf("sync not cleared: %+v", s.Postgres)
	}
	b.stop()
	a.stop()
}

// 3. Planned switch.
func TestM2PlannedSwitch(t *testing.T) {
	root := t.TempDir()
	a := newPrimary(t, root, "n1")
	b := newPGNode(t, root, "n2")
	b.cloneFrom(a)
	waitSame(t, a, b)

	a.stop()
	if out := b.dev("promote", "--slots-for", "n1"); !strings.Contains(out, "timeline 2") {
		t.Fatalf("promote: %s", out)
	}
	out := a.dev("rewind", "--from", b.addr())
	if !strings.Contains(out, "Rewound.") {
		t.Fatalf("rewind: %s", out)
	}
	a.up()
	if !strings.Contains(a.a.out.String(), "standby (read-only) of "+b.addr()) {
		t.Fatalf("old primary did not rejoin as standby:\n%s", a.a.out.String())
	}
	insertRange(t, b.conn(), 101, 150, "after-switch-")
	h := waitSame(t, a, b)
	if !strings.HasPrefix(h, "150:") {
		t.Fatalf("hash %s", h)
	}
	sa := a.status()
	if w := sa.Postgres.WalReceiver; sa.Postgres.Role != "standby" || w == nil || w.SenderPort != b.port || w.ReceivedTLI != 2 || w.Slot != "coyote_n1" {
		t.Fatalf("old primary status: %+v %+v", sa.Postgres, w)
	}
	a.stop()
	b.stop()
}

// 4 + 7. Crash failover with divergence; the old primary cannot come back
// as a primary; pg_rewind removes what only it had.
func TestM2CrashFailoverDivergence(t *testing.T) {
	root := t.TempDir()
	a := newPrimary(t, root, "n1")
	b := newPGNode(t, root, "n2")
	b.cloneFrom(a)
	waitSame(t, a, b)

	b.dev("cut-stream")
	if s := b.status(); s.Postgres.WalReceiver != nil || s.Persisted.Upstream != nil {
		t.Fatalf("stream not cut: %+v", s)
	}
	ca := a.conn()
	insertRange(t, ca, 1000, 1049, "unreplicated-")
	_ = ca.Close(context.Background())
	if k := queryInt(t, b.conn(), "SELECT count(*) FROM items WHERE id >= 1000"); k != 0 {
		t.Fatalf("cut standby received %d rows", k)
	}
	startsBefore := primaryStarts(t, a)

	a.crash()
	if st := controlState(t, a.pgdata).State; st != "in production" {
		t.Fatalf("want a crashed data dir, state is %q", st)
	}

	if out := b.dev("promote", "--slots-for", "n1"); !strings.Contains(out, "timeline 2") {
		t.Fatalf("promote: %s", out)
	}
	insertRange(t, b.conn(), 2000, 2009, "new-primary-")

	// 7: fence the old primary, then try to start it normally.
	a.dev("demote")
	for _, args := range [][]string{a.upArgs(), a.cloneArgs(b)} {
		out, err := runCoyoteDB(args...)
		if err == nil || !strings.Contains(string(out), "must rejoin as a standby") {
			t.Fatalf("former primary started (err=%v):\n%s", err, out)
		}
	}
	waitNoNodeProcs(t, a.dir, 10*time.Second)
	if st := controlState(t, a.pgdata).State; st != "in production" {
		t.Fatalf("refused start touched the data (state %q)", st)
	}

	out := a.dev("rewind", "--from", b.addr())
	if !strings.Contains(out, "Rewound.") {
		t.Fatalf("rewind: %s", out)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "logs", "crash-recovery.log")); err != nil {
		t.Fatalf("crash recovery did not run: %v", err)
	}
	if a.marker() != nil {
		t.Fatal("marker left after a successful rewind")
	}
	a.up()
	h := waitSame(t, a, b)
	if !strings.HasPrefix(h, "110:") {
		t.Fatalf("want 110 rows, hash %s", h)
	}
	if k := queryInt(t, a.conn(), "SELECT count(*) FROM items WHERE id BETWEEN 1000 AND 1049"); k != 0 {
		t.Fatalf("%d un-replicated rows survived on the old primary", k)
	}
	sa := a.status()
	if w := sa.Postgres.WalReceiver; sa.Postgres.Role != "standby" || w == nil || w.ReceivedTLI != 2 || w.SenderPort != b.port {
		t.Fatalf("old primary: %+v %+v", sa.Postgres, w)
	}
	if got := primaryStarts(t, a); got != startsBefore {
		t.Fatalf("old primary accepted read-write connections after the promotion (%d starts, was %d)", got, startsBefore)
	}
	a.stop()
	b.stop()
}

// 5. Rewind impossible (required WAL removed) -> re-clone.
func TestM2RewindImpossibleReclones(t *testing.T) {
	root := t.TempDir()
	a := newPrimary(t, root, "n1")
	b := newPGNode(t, root, "n2")
	b.cloneFrom(a)
	waitSame(t, a, b)

	b.dev("cut-stream")
	ca := a.conn()
	insertRange(t, ca, 1000, 1049, "unreplicated-")
	mustExec(t, ca, "SELECT pg_switch_wal()")
	insertRange(t, ca, 1050, 1099, "unreplicated-")
	mustExec(t, ca, "CHECKPOINT")
	_ = ca.Close(context.Background())
	a.stop()

	b.dev("promote", "--slots-for", "n1")
	insertRange(t, b.conn(), 2000, 2009, "new-primary-")

	// The divergence point is in B's timeline history file.
	hist, err := os.ReadFile(filepath.Join(b.pgdata, "pg_wal", "00000002.history"))
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(hist))
	if len(f) < 2 {
		t.Fatalf("history file: %q", hist)
	}
	sw, err := pg.ParseLSN(f[1])
	if err != nil {
		t.Fatal(err)
	}
	seg := filepath.Join(a.pgdata, "pg_wal", pg.WALFileName(1, sw))
	if _, err := os.Stat(seg); err != nil {
		t.Fatalf("divergence segment missing before the test removed it: %v", err)
	}
	if err := os.Remove(seg); err != nil {
		t.Fatal(err)
	}

	out := a.dev("rewind", "--from", b.addr())
	if !strings.Contains(out, "re-cloned instead") {
		t.Fatalf("expected the re-clone fallback: %s", out)
	}
	logb, _ := os.ReadFile(filepath.Join(a.dir, "logs", "coyote.log"))
	if !strings.Contains(string(logb), "rewind failed") || !strings.Contains(string(logb), pg.WALFileName(1, sw)) {
		t.Fatalf("rewind did not fail on the removed WAL segment; log:\n%s", logb)
	}
	a.up()
	h := waitSame(t, a, b)
	if !strings.HasPrefix(h, "110:") {
		t.Fatalf("hash %s", h)
	}
	a.stop()
	b.stop()
}

// 6. Three nodes: promote one, re-point the other without a re-clone.
func TestM2ThreeNodes(t *testing.T) {
	root := t.TempDir()
	a := newPrimary(t, root, "n1")
	b := newPGNode(t, root, "n2")
	b.cloneFrom(a)
	c := newPGNode(t, root, "n3")
	c.cloneFrom(a)
	waitSame(t, a, b)
	waitSame(t, a, c)
	sysID := controlState(t, c.pgdata).SystemID

	a.stop()
	b.dev("cut-stream")
	c.dev("cut-stream")
	rb, rc := *b.status().Postgres.ReceiveLSN, *c.status().Postgres.ReceiveLSN
	if rb.Compare(rc) < 0 {
		t.Fatalf("n2 (%s) is behind n3 (%s); the test expects to promote n2", rb, rc)
	}
	pf, _ := pg.ReadPidFile(c.pgdata)
	b.dev("promote", "--slots-for", "n1,n3")
	c.dev("set-upstream", "--upstream", b.addr())

	insertRange(t, b.conn(), 101, 120, "via-n2-")
	waitSame(t, b, c)
	sc := c.status()
	if w := sc.Postgres.WalReceiver; w == nil || w.SenderPort != b.port || w.ReceivedTLI != 2 || w.Status != "streaming" {
		t.Fatalf("n3 not following n2 on timeline 2: %+v", w)
	}
	if pf2, _ := pg.ReadPidFile(c.pgdata); pf2 == nil || pf2.PID != pf.PID {
		t.Fatal("n3's Postgres was restarted")
	}
	if controlState(t, c.pgdata).SystemID != sysID {
		t.Fatal("n3 was re-cloned")
	}
	slots := map[string]bool{}
	for _, s := range b.status().Postgres.Slots {
		slots[s.Name] = s.Active
	}
	if _, ok := slots["coyote_n1"]; !ok || !slots["coyote_n3"] {
		t.Fatalf("slots on the new primary: %v", slots)
	}
	c.stop()
	b.stop()
}

// bigTable adds ~120 MB so copies take long enough to be interrupted.
func bigTable(t *testing.T, c *pgx.Conn, name string) {
	t.Helper()
	mustExec(t, c, `CREATE TABLE `+name+` AS SELECT g AS id, repeat(md5(g::text), 12) AS pad FROM generate_series(1, 300000) g`)
}

// waitToolThenKill waits until a process for tool naming path runs, then
// SIGKILLs victim.
func waitToolThenKill(t *testing.T, tool, path string, victim *agent) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		for _, p := range listProcs(t) {
			if strings.Contains(p.cmd, tool) && strings.Contains(p.cmd, path) {
				if err := victim.cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				<-victim.exited
				return
			}
		}
		select {
		case <-victim.exited:
			t.Fatalf("finished before %s was seen:\n%s", tool, victim.out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never started", tool)
		}
		time.Sleep(5 * time.Millisecond) // polling interval
	}
}

// 8. kill -9 during clone and during rewind: no strays, retry succeeds.
func TestM2Kill9DuringCloneAndRewind(t *testing.T) {
	root := t.TempDir()
	a := newPrimary(t, root, "n1")
	bigTable(t, a.conn(), "big1")
	b := newPGNode(t, root, "n2")

	// Clone.
	ag := launchCoyoteDB(t, b.cloneArgs(a)...)
	waitToolThenKill(t, "pg_basebackup", b.dir, ag)
	waitNoNodeProcs(t, b.dir, 15*time.Second)
	if _, err := os.Stat(b.pgdata); err == nil {
		t.Fatal("clone finished before the kill; the test proves nothing (make big1 bigger)")
	}
	if m := b.marker(); m == nil || m.Reason != pg.ReasonCloning {
		t.Fatalf("marker after interrupted clone: %+v", m)
	}
	if out, err := runCoyoteDB(b.upArgs()...); err == nil {
		t.Fatalf("plain up after an interrupted clone must refuse:\n%s", out)
	}
	b.cloneFrom(a)
	waitSame(t, a, b)

	// Rewind.
	b.dev("cut-stream")
	insertRange(t, a.conn(), 1000, 1009, "unreplicated-")
	a.stop()
	b.dev("promote", "--slots-for", "n1")
	bigTable(t, b.conn(), "big2") // pg_rewind must copy it
	insertRange(t, b.conn(), 2000, 2009, "new-primary-")

	rw := launchCoyoteDB(t, "dev", "rewind", "--data-dir", a.dir, "--from", b.addr())
	waitToolThenKill(t, "pg_rewind", a.pgdata, rw)
	waitNoNodeProcs(t, a.dir, 15*time.Second)
	if m := a.marker(); m == nil || m.Reason != pg.ReasonRewinding {
		t.Fatalf("rewind finished before the kill, or marker wrong: %+v", m)
	}
	if out, err := runCoyoteDB(a.upArgs()...); err == nil {
		t.Fatalf("up after an interrupted rewind must refuse:\n%s", out)
	}
	out := a.dev("rewind", "--from", b.addr())
	if !strings.Contains(out, "re-cloned instead") {
		t.Fatalf("an interrupted rewind must be redone as a re-clone: %s", out)
	}
	a.up()
	h := waitSame(t, a, b)
	if !strings.HasPrefix(h, "110:") {
		t.Fatalf("hash %s", h)
	}
	ha := queryText(t, a.conn(), "SELECT md5(string_agg(pad, '' ORDER BY id)) FROM big2")
	if hb := queryText(t, b.conn(), "SELECT md5(string_agg(pad, '' ORDER BY id)) FROM big2"); ha != hb {
		t.Fatal("big2 differs")
	}
	a.stop()
	b.stop()
}
