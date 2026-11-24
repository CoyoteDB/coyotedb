//go:build integration && unix

// A promotion that got past pg_promote and then failed must be finishable
// (chaos kill9-primary-five on Linux, 2026-11-22: the new main's slot step
// failed with "replication slot already exists" and every retry stopped at
// "this node is already a primary"). Real Postgres.
package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"coyotedb.com/coyote/internal/pg"
)

// Two creators of the same slot at once: both must succeed (one makes it,
// the other finds it), never "already exists".
func TestCreateSlotRace(t *testing.T) {
	e := newPGEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srv, err := e.n.Start(ctx, e.opts())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop(pg.DefaultStopTimeouts) }()
	const creators, slots = 8, 8 // slots: under max_replication_slots
	errs := make(chan error, creators*slots)
	for round := 0; round < slots; round++ {
		name := fmt.Sprintf("coyote_race_%d", round)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var conns []*pgx.Conn
		for i := 0; i < creators; i++ {
			c, err := e.n.Connect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			conns = append(conns, c)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := pg.CreateSlot(ctx, c, name); err != nil {
					errs <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		for _, c := range conns {
			_ = c.Close(ctx)
		}
	}
	close(errs)
	for err := range errs {
		t.Errorf("CreateSlot under a race: %v", err)
	}
	c, err := e.n.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	have, err := pg.ListSlots(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(have) != slots {
		t.Fatalf("%d slots after the race, want %d", len(have), slots)
	}
}

// Promote on a Postgres already out of recovery says so with
// ErrAlreadyPrimary; FinishPromote then does the remaining steps, and can
// be repeated.
func TestFinishPromoteAfterInterruptedPromote(t *testing.T) {
	e := newPGEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srv, err := e.n.Start(ctx, e.opts())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Stop(pg.DefaultStopTimeouts) }()
	// The state an interrupted promotion leaves: out of recovery, the
	// rejoin marker still there, one member's slot made, the other's not.
	if err := e.n.WriteMarker(pg.Marker{Reason: pg.ReasonDemoted, Detail: "test: promotion interrupted"}); err != nil {
		t.Fatal(err)
	}
	c, err := e.n.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if _, err := pg.CreateSlot(ctx, c, pg.SlotName("node-b")); err != nil {
		t.Fatal(err)
	}
	o := pg.PromoteOptions{Members: []string{"lease-test", "node-b", "node-c"}}
	if _, err := e.n.Promote(ctx, o); !errors.Is(err, pg.ErrAlreadyPrimary) {
		t.Fatalf("Promote on a primary: %v, want ErrAlreadyPrimary", err)
	}
	for try := 1; try <= 2; try++ {
		tli, err := e.n.FinishPromote(ctx, o)
		if err != nil {
			t.Fatalf("FinishPromote try %d: %v", try, err)
		}
		if tli == 0 {
			t.Fatalf("FinishPromote try %d: timeline 0", try)
		}
	}
	m, err := e.n.ReadMarker()
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Fatalf("rejoin marker still there after FinishPromote: %+v", m)
	}
	have, err := pg.ListSlots(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range have {
		got[s.Name] = true
	}
	for _, want := range []string{pg.SlotName("node-b"), pg.SlotName("node-c")} {
		if !got[want] {
			t.Errorf("slot %s missing after FinishPromote (have %v)", want, got)
		}
	}
	if got[pg.SlotName("lease-test")] {
		t.Error("FinishPromote made a slot for the node itself")
	}
}
