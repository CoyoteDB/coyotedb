package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"coyotedb.com/coyote/internal/pg"
)

func TestSwitchPlan(t *testing.T) {
	for n := 2; n <= 5; n++ {
		names := make([]string, n)
		for i := range names {
			names[i] = fmt.Sprintf("n%d", i)
		}
		start := names[n-1]
		plan := switchPlan(names, start)
		if len(plan) != n*(n-1) {
			t.Fatalf("%d nodes: %d switches, want %d", n, len(plan), n*(n-1))
		}
		seen := map[[2]string]bool{}
		cur := start
		for _, sw := range plan {
			if sw[0] != cur {
				t.Fatalf("%d nodes: switch %v does not start at the main node %s", n, sw, cur)
			}
			if sw[0] == sw[1] || seen[sw] {
				t.Fatalf("%d nodes: bad or repeated switch %v", n, sw)
			}
			seen[sw] = true
			cur = sw[1]
		}
		if cur != start {
			t.Errorf("%d nodes: plan ends at %s, not back at %s", n, cur, start)
		}
	}
	if switchPlan([]string{"a", "b"}, "c") != nil || switchPlan([]string{"a"}, "a") != nil {
		t.Error("impossible plans must be empty")
	}
}

func TestLedgerCheck(t *testing.T) {
	l := newLedger(100)
	ids := []int64{l.take(), l.take(), l.take(), l.take()}
	if ids[0] != 101 || ids[3] != 104 {
		t.Fatalf("ids %v", ids)
	}
	l.result(101, true)
	l.result(102, true)
	l.result(103, false) // in doubt: present or not, both fine
	l.result(104, true)
	if r := l.check([]int64{101, 102, 104}); !r.ok() || r.Acked != 3 || r.InDoubt != 1 {
		t.Errorf("clean: %+v", r)
	}
	if r := l.check([]int64{101, 102, 103, 104}); !r.ok() {
		t.Errorf("in-doubt write present: %+v", r)
	}
	r := l.check([]int64{104, 101, 999})
	if r.ok() || !slices.Equal(r.Lost, []int64{102}) || !slices.Equal(r.Unknown, []int64{999}) {
		t.Errorf("lost: %+v", r)
	}
	if r := l.check([]int64{101, 102, 104, 104}); r.ok() || !r.Repeated {
		t.Errorf("repeat: %+v", r)
	}
}

func inspection(name string) Inspection {
	return Inspection{
		Node:   name,
		Finger: pg.Fingerprint{Format: pg.FingerprintFormat, Control: pg.ControlFingerprint{MaxAlign: "8", BlockSize: "8192"}},
		Probe: pg.ProbeResult{Version: pg.ProbeVersion, Groups: []pg.ProbeGroupResult{
			{Name: "float", MustMatch: true, Digest: "f", Statements: []pg.ProbeStatementResult{{Name: "f8_sqrt", Digest: "1"}}},
			{Name: "libm", Digest: "l", Statements: []pg.ProbeStatementResult{{Name: "exp", Digest: "2"}}},
		}},
		Content: ContentDigest{Database: "app",
			Tables:       map[string]TableDigest{"t": {Rows: 3, Digest: "aa"}, "campaign_ledger": {Rows: 9, Digest: "bb"}},
			LargeObjects: "0:x", Sequences: map[string]string{"t_id_seq": "100"}},
	}
}

func TestCompareInspections(t *testing.T) {
	a, b := inspection("mac"), inspection("linux")
	if c := compareInspections([]Inspection{a, b}); !c.OK || len(c.Problems)+len(c.Notes) != 0 {
		t.Fatalf("equal nodes: %+v", c)
	}
	// A copy may be ahead on a sequence, never behind.
	b.Content.Sequences = map[string]string{"t_id_seq": "132"}
	if c := compareInspections([]Inspection{a, b}); !c.OK {
		t.Errorf("sequence ahead on the copy must pass: %+v", c)
	}
	b.Content.Sequences = map[string]string{"t_id_seq": "99"}
	if c := compareInspections([]Inspection{a, b}); c.OK || !strings.Contains(c.Problems[0], "behind on the copy") {
		t.Errorf("sequence behind: %+v", c)
	}
	// A maths-library difference is a note, not a failure.
	b = inspection("linux")
	b.Probe.Groups[1].Statements[0].Digest, b.Probe.Groups[1].Digest = "3", "l2"
	if c := compareInspections([]Inspection{a, b}); !c.OK || len(c.Notes) != 1 {
		t.Errorf("libm: %+v", c)
	}
	b.Probe.Groups[0].Statements[0].Digest, b.Probe.Groups[0].Digest = "9", "f2"
	if c := compareInspections([]Inspection{a, b}); c.OK || !strings.Contains(c.Problems[0], "probe float/f8_sqrt") {
		t.Errorf("must-match probe: %+v", c)
	}
	b = inspection("linux")
	b.Content.Tables = map[string]TableDigest{"t": {Rows: 3, Digest: "ab"}, "campaign_ledger": {Rows: 8, Digest: "bb"}, "extra": {}}
	c := compareInspections([]Inspection{a, b})
	want := []string{
		"mac vs linux: content: table campaign_ledger has 9 rows on one node and 8 on the other",
		"mac vs linux: content: table extra exists on only one node",
		"mac vs linux: content: table t has the same number of rows (3) but different content",
	}
	if c.OK || !slices.Equal(c.Problems, want) {
		t.Errorf("content problems:\n%s", strings.Join(c.Problems, "\n"))
	}
	b = inspection("linux")
	b.Finger.Control.MaxAlign = "4"
	b.Verify.Findings = []pg.Finding{{Severity: pg.SeverityBlock, Check: "btree", Object: "index public.t_txt", Problem: "damaged"}, {Severity: pg.SeverityInfo, Check: "busy"}}
	b.Audit.Findings = []pg.Finding{{Severity: pg.SeverityBlock, Check: "collation", Problem: "x"}}
	b.Errors = []string{"probe: timeout"}
	c = compareInspections([]Inspection{a, b})
	joined := strings.Join(c.Problems, "\n")
	for _, s := range []string{"linux: check failed to run: probe: timeout", "linux: audit:", "linux: integrity:", "fingerprint:", "audits found different things"} {
		if !strings.Contains(joined, s) {
			t.Errorf("missing %q in:\n%s", s, joined)
		}
	}
	if strings.Contains(joined, "busy") {
		t.Error("info findings must not fail the round")
	}
}

func TestReportRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := &reportWriter{w: &buf}
	cmp := Comparison{OK: true}
	for _, r := range []Record{
		{Kind: "start", OK: true},
		{Kind: "round", Round: 0, OK: true, Comparison: &cmp},
		{Kind: "switch", Round: 1, From: "a", To: "b", OK: true},
		{Kind: "round", Round: 1, OK: false, Message: "1 problems"},
		{Kind: "switch", Round: 2, From: "b", To: "a", OK: false, Message: "busy"},
		{Kind: "switch", Round: 3, From: "b", To: "a", OK: true},
		{Kind: "stop", OK: false},
	} {
		if err := w.write(r); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "\n"); n != 7 {
		t.Fatalf("%d lines", n)
	}
	recs, err := readReport(&buf)
	if err != nil || len(recs) != 7 || recs[0].At.IsZero() {
		t.Fatalf("read back: %v %d", err, len(recs))
	}
	s := summarize(recs)
	if s.Rounds != 2 || s.FailedRounds != 1 || s.Switches != 3 || s.FailedSwitches != 1 || s.Pairs["a>b"] != 1 || s.Pairs["b>a"] != 1 {
		t.Errorf("summary %+v", s)
	}
}

func TestSplitSQL(t *testing.T) {
	got := splitSQL("-- comment\r\n\\set ON_ERROR_STOP on\r\nCREATE TABLE a(\r\n  x int);\r\n\r\nINSERT INTO a VALUES (1);\nSELECT 'tail'")
	want := []string{"CREATE TABLE a(\n  x int)", "INSERT INTO a VALUES (1)", "SELECT 'tail'"}
	if !slices.Equal(got, want) {
		t.Errorf("%q", got)
	}
	// The real Step 0 workload splits into statements without psql lines.
	b, err := os.ReadFile(filepath.Join("..", "step0", "workload.sql"))
	if err != nil {
		t.Fatal(err)
	}
	stmts := splitSQL(string(b))
	if len(stmts) < 10 {
		t.Errorf("workload: %d statements", len(stmts))
	}
	for _, s := range stmts {
		if strings.HasPrefix(s, `\`) || strings.HasSuffix(s, ";") {
			t.Errorf("bad statement %q", s)
		}
	}
}

func TestConfigAndCA(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nodes.json")
	if err := os.WriteFile(p, []byte(`{"token_file":"tok","schema":"w.sql","nodes":[{"name":"a","peer":"http://10.0.0.1:7499"},{"name":"b","peer":"http://10.0.0.2:7499"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(p)
	if err != nil || c.TokenFile != filepath.Join(dir, "tok") || c.Schema != filepath.Join(dir, "w.sql") {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{
		`{"token_file":"t","nodes":[{"name":"a","peer":"http://x:1"}]}`,
		`{"token_file":"t","nodes":[{"name":"a","peer":"http://x:1"},{"name":"a","peer":"http://y:1"}]}`,
		`{"token_file":"t","nodes":[{"name":"a","peer":"x:1"},{"name":"b","peer":"http://y:1"}]}`,
		`{"nodes":[{"name":"a","peer":"http://x:1"},{"name":"b","peer":"http://y:1"}]}`,
	} {
		if err := os.WriteFile(p, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(p); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	ca := filepath.Join(dir, "ca dir", "ca.crt")
	if err := os.MkdirAll(filepath.Dir(ca), 0o700); err != nil {
		t.Fatal(err)
	}
	u, err := localCA(ConnInfo{URL: "postgresql://app:pw@h1:5432,h2:5432/app?sslmode=verify-full&sslrootcert=%2FUsers%2Fx%2Fca.crt&target_session_attrs=read-write", CAPEM: "PEM"}, ca)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(u, "%2FUsers%2Fx") || strings.Contains(u, "+") || !strings.Contains(u, "sslmode=verify-full") || !strings.Contains(u, "h1:5432,h2:5432") {
		t.Errorf("url %s", u)
	}
	if b, err := os.ReadFile(ca); err != nil || string(b) != "PEM" {
		t.Errorf("ca file: %q %v", b, err)
	}
	if lvl, err := parseLevel("deep"); err != nil || lvl != pg.VerifyDeep {
		t.Error("parseLevel")
	}
	if _, err := parseLevel("ultra"); err == nil {
		t.Error("bad level accepted")
	}
}
