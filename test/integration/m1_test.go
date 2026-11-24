//go:build integration

// Integration tests for milestone M1: one node, real Postgres, real process
// guards. Run with `make itest`. The Postgres download is cached in
// ./.coyote-dev/cache (override with COYOTE_PG_CACHE).
//
// Runs on Unix and Windows; OS-specific process handling is in
// osproc_unix_test.go / osproc_windows_test.go. Windows run notes:
// docs/TESTING.md (Windows section).
package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"coyotedb.com/coyote/test/testports"

	"coyotedb.com/coyote/internal/pg"
)

var (
	coyoteBin string // built binary under test
	pgCache   string // shared Postgres download cache
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	// A path with a space, to catch quoting bugs (also for the watchdog,
	// which re-executes this binary).
	binDir, err := os.MkdirTemp("", "coyote itest bin ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(binDir)
	coyoteBin = filepath.Join(binDir, "coyote"+exeSuffix)
	// COYOTE_ITEST_BIN: a prebuilt coyote (e.g. cross-compiled for a test
	// machine without Go). It is copied into the folder with a space.
	if pre := os.Getenv("COYOTE_ITEST_BIN"); pre != "" {
		b, err := os.ReadFile(pre)
		if err == nil {
			err = os.WriteFile(coyoteBin, b, 0o755)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "COYOTE_ITEST_BIN:", err)
			return 1
		}
		return runWithCache(m)
	}
	args := []string{"build", "-o", coyoteBin}
	if os.Getenv("COYOTE_ITEST_RACE") == "1" { // the agents under test run with the race detector
		args = append(args, "-race")
	}
	build := exec.Command("go", append(args, "coyotedb.com/coyote/cmd/coyote")...)
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build coyote:", err)
		return 1
	}
	return runWithCache(m)
}

func runWithCache(m *testing.M) int {
	var err error
	pgCache = os.Getenv("COYOTE_PG_CACHE")
	if pgCache == "" {
		pgCache, err = filepath.Abs(filepath.Join("..", "..", ".coyote-dev", "cache"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return m.Run()
}

// syncBuffer collects process output safely across goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type agent struct {
	cmd    *exec.Cmd
	out    *syncBuffer
	exited chan struct{}
	err    error
}

func freePort(t *testing.T) int {
	t.Helper()
	p, err := testports.Free()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// startAgent runs `coyote up` and waits until it reports ready.
func startAgent(t *testing.T, dataDir string, port int) *agent {
	t.Helper()
	// Since M3 `up` creates a one-node cluster: give it free ports so tests
	// never meet the defaults (or each other).
	// If another program took one of the picked ports meanwhile, the agent
	// says so at once: pick new ones (not the Postgres port, which the
	// caller chose and checks).
	for attempt := 1; ; attempt++ {
		a := launchCoyoteDB(t, "up", "--data-dir", dataDir, "--pg-port", strconv.Itoa(port),
			"--listen", "127.0.0.1:"+strconv.Itoa(freePort(t)), "--control-port", strconv.Itoa(freePort(t)),
			"--client-port", strconv.Itoa(freePort(t)))
		if a.waitReady(t, 10*time.Minute) {
			return a
		}
		if out := a.out.String(); attempt < 4 && testports.InUse(out) && !strings.Contains(out, "PostgreSQL port") {
			continue
		}
		t.Fatalf("agent exited before ready (%v):\n%s", a.err, a.out.String())
	}
}

// waitReady waits for the ready line; false means the agent exited first.
func (a *agent) waitReady(t *testing.T, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(a.out.String(), "Press Ctrl-C to stop.") {
			return true
		}
		select {
		case <-a.exited:
			return strings.Contains(a.out.String(), "Press Ctrl-C to stop.")
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("agent not ready in time:\n%s", a.out.String())
	return false
}

// launchCoyoteDB starts the coyote binary with args and registers cleanup
// (SIGINT, then kill) without waiting for anything.
func launchCoyoteDB(t *testing.T, args ...string) *agent {
	t.Helper()
	return launchCoyoteDBEnv(t, nil, args...)
}

// launchCoyoteDBEnv is launchCoyoteDB with extra environment variables.
func launchCoyoteDBEnv(t *testing.T, env []string, args ...string) *agent {
	t.Helper()
	a := &agent{out: &syncBuffer{}, exited: make(chan struct{})}
	a.cmd = exec.Command(coyoteBin, args...)
	a.cmd.Env = append(append(os.Environ(), "COYOTE_PG_CACHE="+pgCache), env...)
	a.cmd.Stdout = a.out
	a.cmd.Stderr = a.out
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

// startCoyoteDB runs coyote with args and waits until it reports ready.
func startCoyoteDB(t *testing.T, args ...string) *agent {
	t.Helper()
	a := launchCoyoteDB(t, args...)
	// First run may download ~90 MB.
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		if strings.Contains(a.out.String(), "Press Ctrl-C to stop.") {
			return a
		}
		select {
		case <-a.exited:
			t.Fatalf("agent exited before ready (%v):\n%s", a.err, a.out.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("agent not ready in time:\n%s", a.out.String())
	return nil
}

// stop sends SIGINT and requires a clean exit (code 0).
func (a *agent) stop(t *testing.T) {
	t.Helper()
	if err := interrupt(a.cmd.Process); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.exited:
	case <-time.After(90 * time.Second):
		t.Fatalf("agent did not exit after SIGINT:\n%s", a.out.String())
	}
	if a.err != nil {
		t.Fatalf("agent exit: %v\n%s", a.err, a.out.String())
	}
}

func connect(t *testing.T, dataDir string) *pgx.Conn {
	t.Helper()
	out, err := exec.Command(coyoteBin, "conninfo", "--admin", "--data-dir", dataDir).Output()
	if err != nil {
		t.Fatalf("conninfo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// procs lists pids whose command line matches: postmasters ("…/postgres -D
// <pgdata>") or watchdogs ("… __watchdog --pgdata <pgdata>").
func procs(t *testing.T, pgdata string) (postmasters, watchdogs []int) {
	t.Helper()
	for _, p := range listProcs(t) {
		switch {
		case isPostmasterCmd(p.cmd, pgdata):
			postmasters = append(postmasters, p.pid)
		case strings.HasSuffix(p.cmd, "__watchdog --pgdata "+argForm(pgdata)):
			watchdogs = append(watchdogs, p.pid)
		}
	}
	return postmasters, watchdogs
}

// waitNoProcs polls until no postmaster or watchdog is left for pgdata.
func waitNoProcs(t *testing.T, pgdata string, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		pm, wd := procs(t, pgdata)
		if len(pm) == 0 && len(wd) == 0 {
			return time.Since(start)
		}
		if time.Since(start) > within {
			t.Fatalf("still running after %s: postmasters=%v watchdogs=%v", within, pm, wd)
		}
		time.Sleep(25 * time.Millisecond) // polling interval
	}
}

func newDataDir(t *testing.T) (dataDir, pgdata string) {
	dataDir = filepath.Join(t.TempDir(), "node one")
	return dataDir, filepath.Join(dataDir, "pgdata")
}

func onePostmaster(t *testing.T, pgdata string) int {
	t.Helper()
	pm, wd := procs(t, pgdata)
	if len(pm) != 1 {
		t.Fatalf("want exactly 1 postmaster, got %v", pm)
	}
	if len(wd) != 1 {
		t.Fatalf("want exactly 1 watchdog, got %v", wd)
	}
	return pm[0]
}

func TestUpQueryStopRestart(t *testing.T) {
	dataDir, pgdata := newDataDir(t)
	port := freePort(t)
	ctx := context.Background()

	a := startAgent(t, dataDir, port)
	onePostmaster(t, pgdata)
	if !strings.Contains(a.out.String(), fmt.Sprintf("127.0.0.1:%d", port)) {
		t.Fatalf("status line missing port:\n%s", a.out.String())
	}
	pw, _ := os.ReadFile(filepath.Join(dataDir, "superuser.password"))
	if strings.Contains(a.out.String(), strings.TrimSpace(string(pw))) {
		t.Fatal("password printed by up")
	}
	if err := privateFile(filepath.Join(dataDir, "superuser.password")); err != nil {
		t.Fatalf("password file: %v", err)
	}

	c := connect(t, dataDir)
	var collProvider, locale, checksums, walLevel, listen string
	if err := c.QueryRow(ctx, `SELECT datlocprovider::text, datlocale FROM pg_database WHERE datname = 'postgres'`).Scan(&collProvider, &locale); err != nil {
		t.Fatal(err)
	}
	if collProvider != "b" || locale != "C.UTF-8" {
		t.Fatalf("locale provider %q locale %q; want builtin C.UTF-8", collProvider, locale)
	}
	for q, dst := range map[string]*string{
		"SHOW data_checksums":   &checksums,
		"SHOW wal_level":        &walLevel,
		"SHOW listen_addresses": &listen,
	} {
		if err := c.QueryRow(ctx, q).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if checksums != "on" || walLevel != "replica" || listen != "127.0.0.1" {
		t.Fatalf("checksums=%s wal_level=%s listen=%s", checksums, walLevel, listen)
	}
	if _, err := c.Exec(ctx, `CREATE TABLE notes (id int PRIMARY KEY, body text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `INSERT INTO notes VALUES (1, 'hello coyote')`); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := c.QueryRow(ctx, `SELECT body FROM notes WHERE id = 1`).Scan(&body); err != nil || body != "hello coyote" {
		t.Fatalf("read back: %q %v", body, err)
	}
	// Safety rule 11 must survive ALTER SYSTEM (checked after restart).
	if _, err := c.Exec(ctx, `ALTER SYSTEM SET listen_addresses = '*'`); err != nil {
		t.Fatal(err)
	}
	_ = c.Close(ctx)

	a.stop(t)
	waitNoProcs(t, pgdata, 5*time.Second)
	if _, err := os.Stat(pg.PostmasterPidPath(pgdata)); err == nil {
		t.Fatal("postmaster.pid left after clean stop")
	}

	// Restart: same data, no re-init, row still there.
	a2 := startAgent(t, dataDir, port)
	if strings.Contains(a2.out.String(), "Creating a new database") || strings.Contains(a2.out.String(), "Downloading") {
		t.Fatalf("second run should reuse download and data:\n%s", a2.out.String())
	}
	c2 := connect(t, dataDir)
	if err := c2.QueryRow(ctx, `SELECT body FROM notes WHERE id = 1`).Scan(&body); err != nil || body != "hello coyote" {
		t.Fatalf("row after restart: %q %v", body, err)
	}
	if err := c2.QueryRow(ctx, "SHOW listen_addresses").Scan(&listen); err != nil || listen != "127.0.0.1" {
		t.Fatalf("listen_addresses after ALTER SYSTEM + restart = %q (%v); want 127.0.0.1", listen, err)
	}
	_ = c2.Close(ctx)
	a2.stop(t)
	waitNoProcs(t, pgdata, 5*time.Second)
}

func TestKill9AgentStopsPostgres(t *testing.T) {
	dataDir, pgdata := newDataDir(t)
	port := freePort(t)
	a := startAgent(t, dataDir, port)
	pm := onePostmaster(t, pgdata)

	if err := a.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-a.exited
	took := waitNoProcs(t, pgdata, 10*time.Second)
	t.Logf("postmaster %d gone %s after kill -9 of the agent", pm, took)
	if alive(pm) {
		t.Fatal("postmaster still alive")
	}

	// The node must come back cleanly (lock released, data intact).
	a2 := startAgent(t, dataDir, port)
	c := connect(t, dataDir)
	var one int
	if err := c.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("query after restart: %v", err)
	}
	_ = c.Close(context.Background())
	a2.stop(t)
	waitNoProcs(t, pgdata, 5*time.Second)
}

func TestWatchdogLossStopsPostgres(t *testing.T) {
	dataDir, pgdata := newDataDir(t)
	a := startAgent(t, dataDir, freePort(t))
	onePostmaster(t, pgdata)
	_, wd := procs(t, pgdata)
	if err := killPID(wd[0]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.exited:
	case <-time.After(60 * time.Second):
		t.Fatal("agent kept running without its watchdog")
	}
	if a.err == nil {
		t.Fatal("agent should exit non-zero when its guard is lost")
	}
	if !strings.Contains(a.out.String(), "process guard stopped unexpectedly") {
		t.Fatalf("missing explanation:\n%s", a.out.String())
	}
	waitNoProcs(t, pgdata, 5*time.Second)
}

func TestAdoptOrStopOrphanedPostgres(t *testing.T) {
	dataDir, pgdata := newDataDir(t)
	port := freePort(t)

	// Initialise the node, then stop it.
	a := startAgent(t, dataDir, port)
	a.stop(t)
	waitNoProcs(t, pgdata, 5*time.Second)

	// Start Postgres behind the agent's back, as a leftover would be.
	build, err := pg.CurrentBuild()
	if err != nil {
		t.Fatal(err)
	}
	inst := pg.Install{Root: filepath.Join(pgCache, build.Name()), Build: build}
	pgctl := exec.Command(inst.Bin("pg_ctl"), "start", "-D", pgdata, "-l", filepath.Join(t.TempDir(), "orphan.log"), "-w", "-t", "60")
	pgctl.Env = withoutPGEnv()
	// Output to a file, not a pipe: on Windows the postmaster pg_ctl starts
	// inherits the pipe and would keep it open (the read never ends).
	outPath := filepath.Join(t.TempDir(), "pg_ctl.out")
	outF, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	pgctl.Stdout, pgctl.Stderr = outF, outF
	err = pgctl.Run()
	_ = outF.Close()
	if err != nil {
		out, _ := os.ReadFile(outPath)
		t.Fatalf("pg_ctl start: %v\n%s", err, out)
	}
	orphan := 0
	if pm, _ := procs(t, pgdata); len(pm) == 1 {
		orphan = pm[0]
	} else {
		t.Fatalf("want 1 orphan postmaster, got %v", pm)
	}
	t.Cleanup(func() { // never leave it behind, even if the test fails
		if alive(orphan) {
			quitPostgres(orphan)
		}
	})

	a2 := startAgent(t, dataDir, port)
	if !strings.Contains(a2.out.String(), "Stopped a database left running") {
		t.Fatalf("agent did not report stopping the leftover:\n%s", a2.out.String())
	}
	pm := onePostmaster(t, pgdata)
	if pm == orphan || alive(orphan) {
		t.Fatalf("orphan %d still running (current postmaster %d)", orphan, pm)
	}
	c := connect(t, dataDir)
	var one int
	if err := c.QueryRow(context.Background(), "SELECT 1").Scan(&one); err != nil {
		t.Fatal(err)
	}
	_ = c.Close(context.Background())
	a2.stop(t)
	waitNoProcs(t, pgdata, 5*time.Second)
}

func TestStalePidFileFromReusedPid(t *testing.T) {
	dataDir, pgdata := newDataDir(t)
	port := freePort(t)
	a := startAgent(t, dataDir, port)
	a.stop(t)
	waitNoProcs(t, pgdata, 5*time.Second)

	// A pid file naming a live process that is not Postgres (this test).
	stale := fmt.Sprintf("%d\n%s\n0\n%d\n\n127.0.0.1\n0 0\nready\n", os.Getpid(), pgdata, port)
	if err := os.WriteFile(pg.PostmasterPidPath(pgdata), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	a2 := startAgent(t, dataDir, port)
	onePostmaster(t, pgdata)
	if !alive(os.Getpid()) {
		t.Fatal("unreachable")
	}
	a2.stop(t)
	waitNoProcs(t, pgdata, 5*time.Second)

	// A pid file naming a process that is gone: Postgres handles it.
	if err := os.WriteFile(pg.PostmasterPidPath(pgdata), []byte(strings.Replace(stale, strconv.Itoa(os.Getpid()), "999999", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	a3 := startAgent(t, dataDir, port)
	onePostmaster(t, pgdata)
	a3.stop(t)
	waitNoProcs(t, pgdata, 5*time.Second)
}

func TestSecondAgentWaitsForLock(t *testing.T) {
	dataDir, pgdata := newDataDir(t)
	port := freePort(t)
	a := startAgent(t, dataDir, port)

	// A second agent on the same data dir must not start a second Postgres.
	b := exec.Command(coyoteBin, "up", "--data-dir", dataDir, "--pg-port", strconv.Itoa(freePort(t)),
		"--listen", "127.0.0.1:"+strconv.Itoa(freePort(t)), "--control-port", strconv.Itoa(freePort(t)),
		"--client-port", strconv.Itoa(freePort(t)))
	b.Env = append(os.Environ(), "COYOTE_PG_CACHE="+pgCache)
	var out syncBuffer
	b.Stdout, b.Stderr = &out, &out
	prepareAgent(b)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(out.String(), "Waiting") {
		if time.Now().After(deadline) {
			t.Fatalf("second agent did not wait on the lock:\n%s", out.String())
		}
		time.Sleep(25 * time.Millisecond) // polling interval
	}
	onePostmaster(t, pgdata)
	_ = interrupt(b.Process)
	_ = b.Wait()
	onePostmaster(t, pgdata)
	a.stop(t)
	waitNoProcs(t, pgdata, 5*time.Second)
}

func withoutPGEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "PG") {
			env = append(env, kv)
		}
	}
	return env
}
