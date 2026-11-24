package pg

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Unit tests for the mixed-system guards: parsers and comparisons, no
// Postgres needed. testdata/guards holds output captured from the real
// Postgres 18.7 build on macOS arm64 (pg_controldata, stored expression
// trees from pg_index / pg_partitioned_table).

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "guards", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var macControl = ControlFingerprint{
	ControlVersion: "1800", CatalogVersion: "202506291", MaxAlign: "8", BlockSize: "8192", SegmentBlocks: "131072",
	WALBlockSize: "8192", WALSegmentSize: "16777216", MaxIdentifier: "64", MaxIndexColumns: "32", ToastChunk: "1996",
	LOChunk: "2048", DateTime: "64-bit integers", Float8ByVal: "by value", ChecksumVersion: "1", CharSignedness: "signed",
}

func TestFromControlDataMac(t *testing.T) {
	got, err := FromControlData(readTestdata(t, "controldata-aarch64-apple-darwin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got != macControl {
		t.Errorf("got %+v\nwant %+v", got, macControl)
	}
}

// The other four targets print the same labels; their values were not
// captured on those machines (Step 0 recorded Linux x64 as identical in
// all 15 fields). These cases are synthesised from the Mac output: Windows
// line endings, and the fields that would differ on a hypothetical
// incompatible build.
func TestFromControlDataVariants(t *testing.T) {
	mac := readTestdata(t, "controldata-aarch64-apple-darwin.txt")
	win := strings.ReplaceAll(mac, "\n", "\r\n")
	if got, err := FromControlData(win); err != nil || got != macControl {
		t.Errorf("windows line endings: %+v %v", got, err)
	}
	other := strings.Replace(mac, "Maximum data alignment:               8", "Maximum data alignment:               4", 1)
	other = strings.Replace(other, "Default char data signedness:         signed", "Default char data signedness:         unsigned", 1)
	got, err := FromControlData(other)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxAlign != "4" || got.CharSignedness != "unsigned" {
		t.Errorf("variant not read: %+v", got)
	}
	missing := strings.Replace(mac, "Float8 argument passing:", "Float8 argument passing (renamed):", 1)
	if _, err := FromControlData(missing); err == nil || !strings.Contains(err.Error(), "Float8 argument passing") {
		t.Errorf("missing field not reported: %v", err)
	}
	if _, err := FromControlData("WARNING: possible byte ordering mismatch\n" + mac); err == nil {
		t.Error("byte-order warning not an error")
	}
	if _, err := FromControlData(""); err == nil {
		t.Error("empty output accepted")
	}
}

func TestControlFileByteOrder(t *testing.T) {
	for _, c := range []struct {
		order binary.ByteOrder
		want  string
	}{{binary.LittleEndian, "little"}, {binary.BigEndian, "big"}} {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "global"), 0o700); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 8192)
		c.order.PutUint64(b, 7710757956944836927)
		c.order.PutUint32(b[8:], 1800)
		if err := os.WriteFile(filepath.Join(dir, "global", "pg_control"), b, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ControlFileByteOrder(dir)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q %v", c.want, got, err)
		}
	}
}

func TestParseInstallOutputs(t *testing.T) {
	v, err := ParsePostgresVersion("postgres (PostgreSQL) 18.7\n")
	if err != nil || v != "18.7" {
		t.Errorf("version %q %v", v, err)
	}
	if _, err := ParsePostgresVersion("something else"); err == nil {
		t.Error("bad version output accepted")
	}
	conf := `'--prefix' '/x' '--enable-option-checking=fatal' '--with-icu' '--without-ldap' '--with-libxml' '--with-lz4' '--with-openssl' '--with-zstd' '--with-uuid=e2fs'`
	if got := ParseConfigure(conf); !slices.Equal(got, []string{"icu", "lz4", "zstd"}) {
		t.Errorf("options %v", got)
	}
	if got := ParseConfigure(`--with-lz4 --without-icu`); !slices.Equal(got, []string{"lz4"}) {
		t.Errorf("unquoted options %v", got)
	}
}

func TestHashTree(t *testing.T) {
	mk := func(files map[string]string) string {
		dir := t.TempDir()
		for p, c := range files {
			full := filepath.Join(dir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		h, err := HashTree(dir)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := mk(map[string]string{"english.stop": "a\nthe\n", "sub/x.rules": "é e\n"})
	if again := mk(map[string]string{"sub/x.rules": "é e\n", "english.stop": "a\nthe\n"}); again != base {
		t.Error("hash depends on creation order")
	}
	for name, files := range map[string]map[string]string{
		"crlf":    {"english.stop": "a\r\nthe\r\n", "sub/x.rules": "é e\n"},
		"renamed": {"english2.stop": "a\nthe\n", "sub/x.rules": "é e\n"},
		"moved":   {"english.stop": "a\nthe\n", "x.rules": "é e\n"},
		"extra":   {"english.stop": "a\nthe\n", "sub/x.rules": "é e\n", "new": ""},
	} {
		if mk(files) == base {
			t.Errorf("%s: hash did not change", name)
		}
	}
	if _, err := HashTree(filepath.Join(t.TempDir(), "missing")); !os.IsNotExist(err) {
		t.Errorf("missing tree: %v", err)
	}
}

func sampleFingerprint() Fingerprint {
	return Fingerprint{Format: FingerprintFormat, OS: "darwin", Arch: "arm64", Control: macControl,
		Install: InstallFingerprint{Version: "18.7", Options: []string{"icu", "lz4", "zstd"}, TimezoneSHA256: strings.Repeat("a", 64),
			TimezoneSetsSHA256: strings.Repeat("b", 64), TSearchSHA256: strings.Repeat("c", 64), SnowballSHA256: strings.Repeat("d", 64)},
		SQL: SQLFingerprint{VersionNum: "180007", Encoding: "UTF8", UnicodeVersion: "16.0", ICUUnicodeVersion: "16.0", ICUCollatorVersion: "153.128",
			BuiltinCollations: []string{"pg_c_utf8=1", "pg_unicode_fast=1", "ucs_basic=1"}, Extensions: []string{"amcheck=1.5", "pg_trgm=1.6"}}}
}

func TestCompareFingerprints(t *testing.T) {
	a := sampleFingerprint()
	b := sampleFingerprint()
	b.OS, b.Arch, b.Control.ByteOrder = "linux", "amd64", ""
	a.Control.ByteOrder = ""
	if d := CompareFingerprints(a, b); len(d) != 0 || a.Hash() != b.Hash() {
		t.Errorf("OS and CPU must not count: %v", d)
	}
	b.Control.MaxAlign = "4"
	b.Install.TSearchSHA256 = strings.Repeat("e", 64)
	b.SQL.Extensions = []string{"amcheck=1.5", "pg_trgm=1.6", "plpython3u=1.0"}
	b.Install.Options = []string{"icu", "zstd"}
	d := CompareFingerprints(a, b)
	keys := make([]string, len(d))
	for i, x := range d {
		keys[i] = x.Key
		t.Log(x.Text)
	}
	if !slices.Equal(keys, []string{"max_align", "build_options", "tsearch", "extensions"}) {
		t.Fatalf("keys %v", keys)
	}
	if !strings.Contains(d[0].Text, "(8 on this machine, 4 on the other)") {
		t.Errorf("alignment text: %s", d[0].Text)
	}
	if !strings.Contains(d[1].Text, "only on this machine: lz4; only on the other: none") {
		t.Errorf("options text: %s", d[1].Text)
	}
	if !strings.Contains(d[2].Text, "cccccccccccc") || strings.Contains(d[2].Text, strings.Repeat("c", 13)) {
		t.Errorf("hash not shortened: %s", d[2].Text)
	}
	if !strings.Contains(d[3].Text, "only on the other: plpython3u=1.0") {
		t.Errorf("extensions text: %s", d[3].Text)
	}
	if a.Hash() == b.Hash() {
		t.Error("hash equal despite differences")
	}
	// Every compared field is in the canonical form exactly once.
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(a.Canonical()), "\n") {
		k, _, _ := strings.Cut(line, "=")
		if seen[k] {
			t.Errorf("duplicate key %s", k)
		}
		seen[k] = true
	}
	if len(seen) != len(fpFields) {
		t.Errorf("%d keys, %d fields", len(seen), len(fpFields))
	}
}

func TestProbeDefinition(t *testing.T) {
	if err := ValidateProbe(); err != nil {
		t.Fatal(err)
	}
	must, info := 0, 0
	for _, g := range ProbeGroups() {
		for _, s := range g.Statements {
			if g.MustMatch {
				must++
			} else {
				info++
			}
			if s.What == "" || g.What == "" {
				t.Errorf("%s/%s: missing plain-words description", g.Name, s.Name)
			}
		}
		// The maths-library functions are only in the informational group.
		if g.MustMatch {
			for _, s := range g.Statements {
				for _, fn := range []string{"exp(", " ln(", "sin(", "cos(", "tan(", "cbrt(", "erf(", "gamma("} {
					if strings.Contains(s.SQL, fn) && g.Name != "numeric" {
						t.Errorf("%s/%s: %s in a must-match group", g.Name, s.Name, fn)
					}
				}
			}
		}
	}
	if must < 100 || info < 30 {
		t.Errorf("probe has %d must-match and %d informational statements", must, info)
	}
}

func probeResult(groups ...ProbeGroupResult) ProbeResult {
	return ProbeResult{Version: ProbeVersion, Groups: groups}
}

func TestCompareProbes(t *testing.T) {
	g := func(name string, must bool, st ...ProbeStatementResult) ProbeGroupResult {
		gr := ProbeGroupResult{Name: name, MustMatch: must, Statements: st}
		for _, s := range st {
			gr.Digest += s.Digest
		}
		return gr
	}
	a := probeResult(g("float", true, ProbeStatementResult{Name: "f8_sqrt", Digest: "1"}, ProbeStatementResult{Name: "f8_round", Digest: "2"}),
		g("libm", false, ProbeStatementResult{Name: "exp", Digest: "3"}))
	if d := CompareProbes(a, a); len(d) != 0 || ProbeMismatch(d) {
		t.Errorf("same results differ: %v", d)
	}
	b := probeResult(g("float", true, ProbeStatementResult{Name: "f8_sqrt", Digest: "1"}, ProbeStatementResult{Name: "f8_round", Digest: "9"}),
		g("libm", false, ProbeStatementResult{Name: "exp", Digest: "4"}))
	d := CompareProbes(a, b)
	if len(d) != 2 || !d[0].MustMatch || d[0].Statement != "f8_round" || d[1].MustMatch || d[1].Statement != "exp" || !ProbeMismatch(d) {
		t.Fatalf("diffs %+v", d)
	}
	if !strings.Contains(d[0].Text, "rounding float8") || !strings.Contains(d[0].Text, "decimal (floating-point) numbers") {
		t.Errorf("text: %s", d[0].Text)
	}
	if a.Digest() == b.Digest() {
		t.Error("run digests equal")
	}
	onlyLibm := probeResult(a.Groups[0], b.Groups[1])
	if d := CompareProbes(a, onlyLibm); len(d) != 1 || ProbeMismatch(d) || a.Digest() != onlyLibm.Digest() {
		t.Errorf("libm-only difference must not block: %+v", d)
	}
	errB := probeResult(g("float", true, ProbeStatementResult{Name: "f8_sqrt", Digest: "1"}, ProbeStatementResult{Name: "f8_round", Digest: "x", SQLState: "22003"}), a.Groups[1])
	if d := CompareProbes(a, errB); len(d) != 1 || !strings.Contains(d[0].Text, "an answer here, an error (22003) on the other machine") {
		t.Errorf("error text: %+v", d)
	}
	old := a
	old.Version = ProbeVersion - 1
	if d := CompareProbes(a, old); len(d) != 1 || !d[0].MustMatch || d[0].Group != "version" {
		t.Errorf("version mismatch: %+v", d)
	}
}

func TestNodeTreeRefs(t *testing.T) {
	for _, c := range []struct {
		file              string
		funcs, ops, colls []uint32
	}{
		{"tree-index-exp.txt", []uint32{1347}, nil, nil},
		{"tree-partition-power.txt", []uint32{1368, 1746}, nil, nil},
		// ROW(a, b) < ROW(1, exp(2)): operators as a list.
		{"tree-rowcompare.txt", []uint32{316, 1347}, []uint32{672}, nil},
		// COLLATE "en-x-icu" (12878), "C" (950), default (100); a text
		// constant containing ":funcid 1347 (x)" must not count.
		{"tree-collate-strings.txt", []uint32{740}, []uint32{664}, []uint32{12878, 950, 100}},
	} {
		r, err := nodeTreeRefs(readTestdata(t, c.file))
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		for _, x := range []struct {
			name      string
			got, want []uint32
		}{{"funcs", r.funcs, c.funcs}, {"ops", r.ops, c.ops}, {"colls", r.colls, c.colls}} {
			g, w := slices.Sorted(slices.Values(x.got)), slices.Sorted(slices.Values(x.want))
			if !slices.Equal(g, w) {
				t.Errorf("%s %s: got %v want %v", c.file, x.name, g, w)
			}
		}
	}
	// Quoted strings and escapes are data.
	r, err := nodeTreeRefs(`({FUNCEXPR :funcid 10 :args ({CONST :name "a :funcid 99 \" (" :x \:funcid 98})})`)
	if err != nil || !slices.Equal(r.funcs, []uint32{10}) {
		t.Errorf("strings: %v %v", r.funcs, err)
	}
	if _, err := nodeTreeRefs(`({X :name "open`); err == nil {
		t.Error("unterminated string accepted")
	}
	if r, err := nodeTreeRefs(""); err != nil || len(r.funcs)+len(r.ops)+len(r.colls) != 0 {
		t.Errorf("empty tree: %+v %v", r, err)
	}
}

func TestParsePgAmcheck(t *testing.T) {
	out := `pg_amcheck: warning: skipping database "template1": amcheck is not installed
btree index "postgres.public.w_s_c":
    ERROR:  item order invariant violated for index "w_s_c"
    DETAIL:  Lower index tid=(3,6) (points to index tid=(7,1)) higher index tid=(3,7) (points to index tid=(8,1)) page lsn=0/1903D10.
btree index "app.public.w_pkey":
    ERROR:  heap tuple (16,41) from table "w" lacks matching index tuple within index "w_pkey"
heap table "app.public.big", block 3, offset 7:
    invalid tuple length
`
	fs := parsePgAmcheck(strings.ReplaceAll(out, "\n", "\r\n"))
	if len(fs) != 3 {
		t.Fatalf("findings %+v", fs)
	}
	want := []struct{ db, obj, check, detail string }{
		{"postgres", "index public.w_s_c", "pg_amcheck_btree", "item order invariant violated"},
		{"app", "index public.w_pkey", "pg_amcheck_btree", "lacks matching index tuple"},
		{"app", "table public.big", "pg_amcheck_heap", "invalid tuple length"},
	}
	for i, w := range want {
		f := fs[i]
		if f.Database != w.db || f.Object != w.obj || f.Check != w.check || !strings.Contains(f.Detail, w.detail) || f.Severity != SeverityBlock {
			t.Errorf("%d: %+v", i, f)
		}
	}
	if fs := parsePgAmcheck("pg_amcheck: warning: skipping database \"template1\"\n"); len(fs) != 0 {
		t.Errorf("warnings are not findings: %v", fs)
	}
}

func TestParseChecksums(t *testing.T) {
	out := `pg_checksums: error: checksum verification failed in file "/d/base/5/16394", block 0: calculated checksum 7B but block contains 68C6
Checksum operation completed
Files scanned:   954
Blocks scanned:  2972
Bad checksums:  1
Data checksum version: 1
`
	r := ParseChecksums(out)
	if r.FilesScanned != 954 || r.BlocksScanned != 2972 || r.BadChecksums != 1 || len(r.Problems) != 1 || !strings.Contains(r.Problems[0], "block 0") {
		t.Errorf("%+v", r)
	}
}

func TestFindingsOrder(t *testing.T) {
	fs := []Finding{
		{Severity: SeverityInfo, Check: "a"},
		{Severity: SeverityWarn, Check: "b", Database: "z"},
		{Severity: SeverityBlock, Check: "c", Database: "b", Object: "y"},
		{Severity: SeverityBlock, Check: "c", Database: "b", Object: "x"},
		{Severity: SeverityBlock, Check: "c", Database: "a"},
	}
	SortFindings(fs)
	var got []string
	for _, f := range fs {
		got = append(got, string(f.Severity)+f.Database+f.Object)
	}
	if !slices.Equal(got, []string{"blocka", "blockbx", "blockby", "warnz", "info"}) {
		t.Errorf("order %v", got)
	}
	if !Blocking(fs) || Blocking(fs[3:]) {
		t.Error("Blocking")
	}
	if s := (Finding{Severity: SeverityWarn, Database: "app", Object: "index i", Problem: "P.", Fix: "F."}).String(); s != "warn: [app] index i: P. Fix: F." {
		t.Errorf("String: %q", s)
	}
}

func TestExtensionLists(t *testing.T) {
	for name := range extensionDenyReasons {
		if _, ok := ExtensionAllowList[name]; ok {
			t.Errorf("%s is both allowed and denied", name)
		}
	}
	for name, why := range ExtensionAllowList {
		if why == "" {
			t.Errorf("%s: no reason", name)
		}
	}
}
