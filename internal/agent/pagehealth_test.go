package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

func TestPageRecordSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pagecheck.json")
	var h pageHealth
	if err := h.load(path); err != nil {
		t.Fatal(err)
	}
	if h.damaged() {
		t.Fatal("fresh record is damaged")
	}
	p := &pg.PageScanProgress{PostmasterStart: "s1", DB: "app", RelOID: 16390, NextBlock: 2048}
	if err := h.update(func(r *pageRecord) {
		r.Damage = []string{"public.t in database app: invalid page in block 1"}
		r.Progress = p
	}); err != nil {
		t.Fatal(err)
	}
	var h2 pageHealth
	if err := h2.load(path); err != nil {
		t.Fatal(err)
	}
	if !h2.damaged() || h2.rec.Progress == nil || h2.rec.Progress.NextBlock != 2048 {
		t.Fatalf("reloaded %+v", h2.rec)
	}
	// A record from a newer program is refused, not misread.
	if err := os.WriteFile(path, []byte(`{"format": 99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h2.load(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("format 99: %v", err)
	}
}

func TestPageReport(t *testing.T) {
	h := pageHealth{path: "x"}
	h.start, h.role = "s1", "standby"
	r := h.report()
	if r.Damaged || r.CheckedSinceStart || r.Scan != "waiting" {
		t.Fatalf("fresh: %+v", r)
	}
	h.rec.LastFullPass, h.rec.PassStart = time.Now(), "s1"
	if r = h.report(); !r.CheckedSinceStart || r.Damaged {
		t.Fatalf("after a full pass: %+v", r)
	}
	// A restart means the pass no longer covers the running server.
	h.start = "s2"
	if r = h.report(); r.CheckedSinceStart {
		t.Fatalf("after a restart: %+v", r)
	}
	// A copy's damage comes from the record.
	h.rec.Damage = []string{"bad page"}
	if r = h.report(); !r.Damaged || r.Problems[0] != "bad page" {
		t.Fatalf("damaged copy: %+v", r)
	}
	// A main node's live counters are reported as damage.
	h = pageHealth{path: "x", role: "primary", failures: 2, lastFailure: "2026-11-23 08:00:00+00"}
	if r = h.report(); !r.Damaged || !strings.Contains(r.Problems[0], "2 data page(s) failed their checksum") {
		t.Fatalf("damaged main: %+v", r)
	}
	// Off (no record path): no section.
	if (&pageHealth{}).report() != nil {
		t.Fatal("report without a record")
	}
}
