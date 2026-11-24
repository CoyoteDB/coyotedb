package pg

import (
	"strings"
	"testing"
)

// ExplainCanonical turns the canonical text kept in the cluster state
// back into the same plain sentences CompareFingerprints gives.
func TestExplainCanonical(t *testing.T) {
	a := Fingerprint{Format: FingerprintFormat, OS: "darwin", Arch: "arm64", Control: macControl,
		Install: InstallFingerprint{Version: "18.7", Options: []string{"icu", "lz4", "zstd"}},
		SQL:     SQLFingerprint{VersionNum: "180007", Encoding: "UTF8", ICUCollatorVersion: "153.120", Extensions: []string{"amcheck=1.5", "citext=1.6"}}}
	b := a
	b.OS = "linux"
	if got := ExplainCanonical(a.Canonical(), b.Canonical()); len(got) != 0 {
		t.Fatalf("OS is not compared, got %q", got)
	}
	// Format 2 (D48): ICU versions are recorded, not compared (mixed
	// clusters are builtin-only). The real macOS and Linux builds differ
	// exactly like this.
	b.SQL.ICUCollatorVersion, b.SQL.ICUUnicodeVersion = "153.80", "15.0"
	if got := ExplainCanonical(a.Canonical(), b.Canonical()); len(got) != 0 || a.Hash() != b.Hash() {
		t.Fatalf("ICU versions must not be compared, got %q", got)
	}
	b.SQL.UnicodeVersion = "15.1"
	b.SQL.Extensions = []string{"amcheck=1.5", "citext=1.6", "plv8=3.2"}
	b.Install.Options = []string{"icu", "lz4"}
	got := ExplainCanonical(a.Canonical(), b.Canonical())
	want := CompareFingerprints(a, b)
	if len(got) != len(want) || len(got) != 3 {
		t.Fatalf("got %d sentences, CompareFingerprints %d: %q", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i].Text {
			t.Errorf("sentence %d:\n got  %s\n want %s", i, got[i], want[i].Text)
		}
	}
	joined := strings.Join(got, " ")
	for _, s := range []string{"Unicode tables for upper/lower case is different (missing on this machine, 15.1 on the other)", "only on the other: plv8=3.2", "only on this machine: zstd"} {
		if !strings.Contains(joined, s) {
			t.Errorf("missing %q in %q", s, joined)
		}
	}
	// A field this version does not know (a newer program on one side).
	got = ExplainCanonical(a.Canonical()+"new_field=1\n", a.Canonical())
	if len(got) != 1 || !strings.Contains(got[0], `"new_field"`) {
		t.Errorf("unknown field: %q", got)
	}
}

func TestProbeMustMatchGroups(t *testing.T) {
	r := ProbeResult{Version: ProbeVersion, Groups: []ProbeGroupResult{
		{Name: "float", MustMatch: true, Digest: "d1"}, {Name: "libm", MustMatch: false, Digest: "d2"}, {Name: "text", MustMatch: true, Digest: "d3"},
	}}
	if got := strings.Join(r.MustMatchGroups(), ","); got != "float=d1,text=d3" {
		t.Fatalf("got %s", got)
	}
}
