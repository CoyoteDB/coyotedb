//go:build chaos && unix

package chaos

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Main-node health (D58): the main node hands its role over when its own
// database cannot save anything (worst-case findings D1, D2; those
// scenarios are in worst_test.go). This is the other side: a main node
// that is busy but healthy must NOT hand over. A false positive would
// flap the cluster under exactly the load people care about.
func init() {
	scenarios = append(scenarios,
		Scenario{Name: "mainhealth-heavy-load-no-handover", Topo: Three, Body: heavyLoadNoHandOver},
	)
}

// heavyLoadNoHandOver: for 60 s the main node runs, besides the chaos
// workload, 8 connections of bulk inserts (20,000 rows a statement), one
// connection of 1,000,000-row transactions, a CHECKPOINT loop and a
// VACUUM loop: WAL flushes and the disk as busy as this machine can make
// them. Expected: same main node, same term, no hand-over said, and
// writes keep being acknowledged.
func heavyLoadNoHandOver(r *Run) {
	p := r.Primary()
	e0 := r.C.Epoch()
	t0 := r.C.Log.Mark("heavy healthy load on main node " + p.Name + " for 60 s")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	setup, err := r.C.Connect(ctx, p)
	if err != nil {
		r.C.Fail("connect: %v", err)
	}
	if _, err := setup.Exec(ctx, `CREATE TABLE IF NOT EXISTS heavy_load (id bigserial, pad text)`); err != nil {
		r.C.Fail("create: %v", err)
	}
	setup.Close(context.Background())
	var wg sync.WaitGroup
	var rows, stmts atomic.Int64
	loop := func(q string, n int64) {
		defer wg.Done()
		c, err := r.C.Connect(ctx, p)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		for ctx.Err() == nil {
			if _, err := c.Exec(ctx, q); err == nil {
				rows.Add(n)
				stmts.Add(1)
			}
		}
	}
	for range 8 {
		wg.Add(1)
		go loop(`INSERT INTO heavy_load (pad) SELECT repeat(md5(random()::text), 4) FROM generate_series(1, 20000)`, 20000)
	}
	wg.Add(3)
	go loop(`INSERT INTO heavy_load (pad) SELECT repeat(md5(random()::text), 4) FROM generate_series(1, 1000000)`, 1000000)
	go loop(`CHECKPOINT`, 0)
	go loop(`VACUUM heavy_load`, 0)
	wg.Wait()
	msg := fmt.Sprintf("after %s of heavy load: %d statements, %d rows; main node %q, term %d -> %d",
		time.Since(t0).Round(time.Second), stmts.Load(), rows.Load(), r.C.PrimaryByCluster(), e0, r.C.Epoch())
	r.C.Log.Event("%s", msg)
	r.t.Logf("%s", msg)
	if r.C.Epoch() != e0 || r.C.PrimaryByCluster() != p.Name {
		r.Problem("FALSE POSITIVE: the main node changed under heavy but healthy load (%s)", msg)
	}
	if out := p.AllOutput(); strings.Contains(out, "Handing the main-node role") || strings.Contains(out, "has not been able to save anything") {
		r.Problem("FALSE POSITIVE: %s's health check fired under heavy but healthy load", p.Name)
	}
	if rows.Load() == 0 {
		r.C.Fail("precondition: the heavy load wrote nothing")
	}
	// Drop the table so the final whole-cluster comparison stays quick.
	dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
	defer dcancel()
	if c, err := r.C.Connect(dctx, p); err == nil {
		_, _ = c.Exec(dctx, `DROP TABLE heavy_load`)
		c.Close(context.Background())
	}
	r.Let(5 * time.Second)
}
