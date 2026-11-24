package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPageScanPercent(t *testing.T) {
	p := PageScanProgress{TotalBytes: 1000, BytesRead: 500}
	if p.Percent() != 50 {
		t.Fatalf("50%%: %d", p.Percent())
	}
	p.BytesRead = 5000 // the database grew during the pass
	if p.Percent() != 99 {
		t.Fatalf("capped: %d", p.Percent())
	}
	p.Done = time.Now()
	if p.Percent() != 100 {
		t.Fatalf("done: %d", p.Percent())
	}
	if (PageScanProgress{}).Percent() != 0 {
		t.Fatal("empty")
	}
}

func TestPageDamageCapped(t *testing.T) {
	var p PageScanProgress
	for i := 0; i < 20; i++ {
		p.addDamage(string(rune('a' + i)))
		p.addDamage(string(rune('a' + i))) // duplicates are kept once
	}
	if len(p.Damage) != maxPageDamage {
		t.Fatalf("%d", len(p.Damage))
	}
}

func TestPgMessage(t *testing.T) {
	err := &pgconn.PgError{Severity: "ERROR", Code: "XX001", Message: `invalid page in block 1 of relation "base/5/16390"`}
	if got := pgMessage(err); got != `invalid page in block 1 of relation "base/5/16390"` {
		t.Fatalf("%q", got)
	}
}

// The pace: reading 4 MiB at 2 MiB/s takes about 2 s of waiting, and a
// cancelled context stops the wait at once.
func TestPageScanPacing(t *testing.T) {
	now := time.Now()
	s := &pageScan{o: PageScanOptions{BytesPerSecond: 2 << 20, Now: func() time.Time { return now }}, paceFrom: now}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.read(ctx, 4<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
	s = &pageScan{o: PageScanOptions{BytesPerSecond: -1, Now: time.Now}}
	if err := s.read(context.Background(), 1<<30); err != nil {
		t.Fatalf("unpaced: %v", err)
	}
	// Far behind (slow disk): the pace restarts instead of bursting.
	s = &pageScan{o: PageScanOptions{BytesPerSecond: 1 << 30, Now: func() time.Time { return now.Add(time.Minute) }}, paceFrom: now}
	if err := s.read(context.Background(), 1<<20); err != nil || s.paceBytes != 0 || !s.paceFrom.Equal(now.Add(time.Minute)) {
		t.Fatalf("catch-up: err %v bytes %d from %v", err, s.paceBytes, s.paceFrom)
	}
}
