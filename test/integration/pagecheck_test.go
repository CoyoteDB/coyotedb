//go:build integration && unix

// Page check (D55, pg.ScanPages) on a real hot standby: a clean pass reads
// every page and finishes; one flipped byte in a table page that no WAL
// record rewrites is found as damage; an interrupted pass resumes.
package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

func TestPageCheckFindsFlippedByteOnCopy(t *testing.T) {
	root := t.TempDir()
	a := newPrimary(t, root, "n1")
	ac := a.conn()
	mustExec(t, ac, `CREATE EXTENSION IF NOT EXISTS amcheck`)
	mustExec(t, ac, `CREATE TABLE rot (id int PRIMARY KEY, body text)`)
	mustExec(t, ac, `INSERT INTO rot SELECT g, repeat('x', 200) FROM generate_series(1, 20000) g`)
	mustExec(t, ac, `CREATE DATABASE noam`) // no amcheck there: TID-range read
	mustExec(t, ac, `VACUUM (FREEZE) rot`)
	mustExec(t, ac, `CHECKPOINT`)
	rel := queryText(t, ac, `SELECT pg_relation_filepath('rot')`)

	b := newPGNode(t, root, "n2")
	b.cloneFrom(a)
	node, err := pg.NewNode(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	want := lsnOf(t, a, "SELECT pg_current_wal_flush_lsn()::text")
	deadline := time.Now().Add(time.Minute)
	for lsnOf(t, b, "SELECT pg_last_wal_replay_lsn()::text") < want {
		if time.Now().After(deadline) {
			t.Fatal("copy did not replay")
		}
		time.Sleep(100 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A clean pass, interrupted after the first save, then resumed.
	p, err := pg.NewPageScan(ctx, node.ConnectDB, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.TotalBytes <= 0 {
		t.Fatalf("total %d", p.TotalBytes)
	}
	sctx, stop := context.WithCancel(ctx)
	saves := 0
	got, err := pg.ScanPages(sctx, node.ConnectDB, pg.PageScanOptions{ChunkBlocks: 16, BytesPerSecond: 4 << 20, SaveEvery: time.Nanosecond,
		Save: func(pg.PageScanProgress) {
			if saves++; saves == 3 {
				stop()
			}
		}}, p)
	stop()
	if err == nil || !got.Done.IsZero() || got.BytesRead == 0 {
		t.Fatalf("interrupted pass: err %v, done %v, read %d", err, got.Done, got.BytesRead)
	}
	t.Logf("interrupted at %s rel %d block %d (%d%%)", got.DB, got.RelOID, got.NextBlock, got.Percent())
	got, err = pg.ScanPages(ctx, node.ConnectDB, pg.PageScanOptions{BytesPerSecond: -1}, got)
	if err != nil || got.Done.IsZero() || len(got.Damage) > 0 || got.Percent() != 100 {
		t.Fatalf("resumed clean pass: err %v, %+v", err, got)
	}
	if !strings.Contains(strings.Join(got.NoAmcheck, ","), "noam") {
		t.Fatalf("noam should be read without amcheck: %v", got.NoAmcheck)
	}
	t.Logf("clean pass: %d relations, %d bytes, skipped %d", got.Relations, got.BytesRead, got.Skipped)

	// Power off the copy, flip one byte in block 1 of rot, start it again.
	mustExec(t, b.conn(), "CHECKPOINT")
	b.crash()
	file := filepath.Join(b.pgdata, rel)
	f, err := os.OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	one := make([]byte, 1)
	if _, err := f.ReadAt(one, 8192+5000); err != nil {
		t.Fatal(err)
	}
	one[0] ^= 0x5a
	if _, err := f.WriteAt(one, 8192+5000); err != nil {
		t.Fatal(err)
	}
	f.Close()
	b.up()

	p, err = pg.NewPageScan(ctx, node.ConnectDB, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err = pg.ScanPages(ctx, node.ConnectDB, pg.PageScanOptions{BytesPerSecond: -1}, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Damage) == 0 || !strings.Contains(got.Damage[0], "rot") || !strings.Contains(got.Damage[0], "invalid page in block 1") {
		t.Fatalf("damage not found: %+v", got)
	}
	t.Logf("found: %s", got.Damage[0])
	n, _, err := pg.ChecksumFailures(ctx, b.conn())
	if err != nil || n == 0 {
		t.Fatalf("checksum counter %d (%v)", n, err)
	}
}
