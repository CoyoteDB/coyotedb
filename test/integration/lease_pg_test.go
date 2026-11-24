//go:build integration && unix

// Lease fixes (D39-D42): the pg layer with
// a real Postgres and the real watchdog process, without an agent. The
// watchdog re-executes this test binary, so it dispatches __watchdog here.
package integration

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"coyotedb.com/coyote/internal/mono"
	"coyotedb.com/coyote/internal/pg"
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == pg.WatchdogCommand {
		os.Exit(pg.RunWatchdog(os.Args[2:]))
	}
}

// pgEnv is one data dir with an initialised Postgres, its node lock and
// a running watchdog (the guard).
type pgEnv struct {
	t     *testing.T
	n     pg.Node
	inst  pg.Install
	guard *pg.Guard
	port  int
	log   *slog.Logger
	pgids []int // process groups of every postmaster started (cleanup)
	extra []int // other processes to kill at cleanup (backends)
}

func newPGEnv(t *testing.T) *pgEnv {
	t.Helper()
	ctx := context.Background()
	n, err := pg.NewNode(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Prepare(); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	inst, err := pg.Ensure(ctx, pg.FetchOptions{CacheDir: pgCache, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := pg.AcquireLock(ctx, n.LockPath(), nil)
	if err != nil {
		t.Fatal(err)
	}
	g, err := pg.NewGuard(n, lock, log)
	if err != nil {
		t.Fatal(err)
	}
	e := &pgEnv{t: t, n: n, inst: inst, guard: g, port: freePort(t), log: log}
	t.Cleanup(func() {
		e.killLeftovers()
		_ = g.Close()
		_ = lock.Unlock()
		waitNoNodeProcs(t, n.StateDir, 20*time.Second)
	})
	if _, err := n.InitDB(ctx, inst, log); err != nil {
		t.Fatal(err)
	}
	if err := n.WriteInfo(pg.NodeInfo{Name: "lease-test", Port: e.port}); err != nil {
		t.Fatal(err)
	}
	return e
}

// killLeftovers kills every process of this data dir's Postgres (test
// cleanup only, so a failing test leaves nothing behind).
func (e *pgEnv) killLeftovers() {
	for _, g := range e.pgids {
		_ = unix.Kill(-g, unix.SIGKILL)
	}
	for _, p := range e.extra {
		_ = unix.Kill(p, unix.SIGKILL)
	}
}

func (e *pgEnv) opts() pg.StartOptions {
	return pg.StartOptions{Install: e.inst, Port: e.port, Name: "lease-test", Guard: e.guard, Logger: e.log, ReadyTimeout: time.Minute,
		OnSpawn: func(s *pg.Server) { e.pgids = append(e.pgids, s.PID()) }}
}

// F7: the fence's last step must kill every process of the Postgres, not
// only the postmaster. A backend busy in a loop never looks for the
// postmaster's death; left running it keeps the shared memory and the
// next start fails.
func TestLeaseFenceKillsWholeServer(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	srv, err := e.n.Start(ctx, e.opts())
	if err != nil {
		t.Fatal(err)
	}
	// Two backends: one in a loop (never checks for the postmaster's
	// death), one in a long pg_sleep.
	busy := make(chan error, 2)
	var backends []int
	for _, q := range []string{"DO $$ BEGIN LOOP END LOOP; END $$", "SELECT pg_sleep(600)"} {
		c, err := e.n.Connect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var b int
		if err := c.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&b); err != nil {
			t.Fatal(err)
		}
		e.extra = append(e.extra, b)
		backends = append(backends, b)
		go func() {
			_, err := c.Exec(context.Background(), q)
			busy <- err
		}()
	}
	waitFor(t, 10*time.Second, "both backends to be busy", func() bool {
		c2, err := e.n.Connect(ctx)
		if err != nil {
			return false
		}
		defer c2.Close(ctx)
		var n int
		_ = c2.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE pid = ANY($1) AND state = 'active'", backends).Scan(&n)
		return n == 2
	})
	// Straight to the kill (a lease that is already lost).
	if err := srv.Fence(0, 0); err != nil {
		t.Fatal(err)
	}
	for _, b := range backends {
		if alive(b) {
			t.Fatalf("backend %d still runs after the fence's kill step", b)
		}
	}
	<-busy
	<-busy
	srv2, err := e.n.Start(ctx, e.opts())
	if err != nil {
		t.Fatalf("restart after the fence: %v", err)
	}
	if err := srv2.Stop(pg.DefaultStopTimeouts); err != nil {
		t.Fatal(err)
	}
}

// F3: a primary start whose lease runs out while the agent is still
// writing files (slow disk, stalled agent), before Postgres is spawned,
// must not leave a Postgres running past the lease's deadline.
func TestLeasePrimaryStartDeadlineCoversPreSpawn(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	o := e.opts()
	// The lease the agent holds when it begins the start: 1.5s of
	// watchdog budget left.
	budget := 1500 * time.Millisecond
	deadline := time.Now().Add(budget)
	until := mono.Now() + budget
	o.PrimaryDeadline = func() (time.Duration, bool) { return until, true }
	o.TestBeforeSpawn = func() { time.Sleep(2 * time.Second) } // the disk is slow
	srv, err := e.n.Start(ctx, o)
	if err == nil {
		late := time.Since(deadline)
		_ = srv.Stop(pg.DefaultStopTimeouts)
		t.Fatalf("a primary start succeeded %s after its lease deadline", late.Round(time.Millisecond))
	}
	if !errors.Is(err, pg.ErrLeaseExpired) {
		t.Fatalf("start failed, but not because the lease ran out: %v", err)
	}
}
