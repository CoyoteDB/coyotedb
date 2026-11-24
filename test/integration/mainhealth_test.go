//go:build integration && unix

// Main-node health probe (D58) against a real Postgres: it answers fast
// under heavy, healthy load (no false positive), and it reports a hung
// Postgres as stalled, both for a new connection and for one it holds.
package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"coyotedb.com/coyote/internal/pg"
)

func TestMainHealthProbe(t *testing.T) {
	e := newPGEnv(t)
	ctx := context.Background()
	srv, err := e.n.Start(ctx, e.opts())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Fence(0, 0) }()
	c, err := e.n.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	// 1. The probe writes a WAL record and flushes it: the flushed WAL
	// position moves.
	var before, after string
	if err := c.QueryRow(ctx, "SELECT pg_current_wal_flush_lsn()::text").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := pg.ProbeWrite(ctx, c); err != nil {
		t.Fatalf("probe on a healthy Postgres: %v", err)
	}
	if err := c.QueryRow(ctx, "SELECT pg_current_wal_flush_lsn()::text").Scan(&after); err != nil {
		t.Fatal(err)
	}
	b, _ := pg.ParseLSN(before)
	a, _ := pg.ParseLSN(after)
	if a <= b {
		t.Fatalf("probe did not flush new WAL: %s -> %s", before, after)
	}

	// 2. Heavy but healthy load: big batches on 6 connections, one large
	// transaction, and checkpoints in a loop, for 20 s. The probe must
	// keep answering well inside stallAfter (12 s); we ask for 3 s.
	setup, err := e.n.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.Exec(ctx, `CREATE TABLE load (id bigserial, pad text)`); err != nil {
		t.Fatal(err)
	}
	setup.Close(ctx)
	lctx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	var wg sync.WaitGroup
	var rows atomic.Int64
	worker := func(q string, n int64) {
		defer wg.Done()
		w, err := e.n.Connect(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		defer w.Close(context.Background())
		for lctx.Err() == nil {
			if _, err := w.Exec(lctx, q); err == nil {
				rows.Add(n)
			}
		}
	}
	for range 6 {
		wg.Add(1)
		go worker(`INSERT INTO load (pad) SELECT repeat(md5(random()::text), 8) FROM generate_series(1, 20000)`, 20000)
	}
	wg.Add(2)
	go worker(`INSERT INTO load (pad) SELECT repeat(md5(random()::text), 16) FROM generate_series(1, 400000)`, 400000)
	go worker(`CHECKPOINT`, 0)
	var worst time.Duration
	probes := 0
	for lctx.Err() == nil {
		pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		t0 := time.Now()
		err := pg.ProbeWrite(pctx, c)
		d := time.Since(t0)
		out := pg.ClassifyProbe(pctx, err)
		cancel()
		if out != pg.ProbeOK {
			t.Fatalf("probe under healthy load: %v (%v) after %s", out, err, d)
		}
		worst = max(worst, d)
		probes++
		time.Sleep(200 * time.Millisecond)
	}
	wg.Wait()
	t.Logf("under load (%d rows written): %d probes, slowest %s", rows.Load(), probes, worst.Round(time.Millisecond))
	if worst > 3*time.Second {
		t.Fatalf("a probe took %s under healthy load", worst)
	}

	// 3. The backend of the probe's own connection hangs (SIGSTOP): the
	// probe reports stalled, not an error that would be ignored.
	var bpid int
	if err := c.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&bpid); err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(bpid, unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err = pg.ProbeWrite(pctx, c)
	if out := pg.ClassifyProbe(pctx, err); out != pg.ProbeStalled {
		t.Fatalf("hung backend: %v (%v), want stalled", out, err)
	}
	cancel()
	_ = unix.Kill(bpid, unix.SIGCONT)

	// 4. The postmaster hangs: a new connection for the probe never
	// completes; that is stalled too.
	if err := unix.Kill(srv.PID(), unix.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	pctx, cancel = context.WithTimeout(ctx, 2*time.Second)
	c2, err := e.n.Connect(pctx)
	if err == nil {
		err = pg.ProbeWrite(pctx, c2)
	}
	out := pg.ClassifyProbe(pctx, err)
	cancel()
	_ = unix.Kill(srv.PID(), unix.SIGCONT)
	if out != pg.ProbeStalled {
		t.Fatalf("hung postmaster: %v (%v), want stalled", out, err)
	}

	// 5. Postgres not running: not counted (the restart loop's job).
	if err := srv.Fence(0, 0); err != nil {
		t.Fatal(err)
	}
	pctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = e.n.Connect(pctx)
	if out := pg.ClassifyProbe(pctx, err); out != pg.ProbeOther {
		t.Fatalf("stopped Postgres: %v (%v), want other", out, err)
	}
}
