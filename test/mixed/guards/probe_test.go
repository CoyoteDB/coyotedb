//go:build integration

package guards

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// The fingerprint and the probe are stable: two runs, and a run after a
// restart, give identical results. Every probe statement either answers
// or fails with an error we expect (no statement is broken by accident).
func TestFingerprintAndProbeStable(t *testing.T) {
	s := newServer(t)
	ctx := ctxT(t, 10*time.Minute)

	fp := func() pg.Fingerprint {
		c := s.connect("postgres")
		f, err := pg.CollectFingerprint(ctx, inst, s.node.PGData(), c)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	probe := func() pg.ProbeResult {
		c := s.connect("postgres")
		r, err := pg.RunProbe(ctx, c, pg.ProbeOptions{KeepOutput: true})
		if err != nil {
			t.Fatal(err)
		}
		// The connection is still usable afterwards (transaction closed).
		var one int
		if err := c.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			t.Fatalf("connection unusable after probe: %v", err)
		}
		return r
	}

	f1, p1 := fp(), probe()
	if b, err := json.MarshalIndent(f1, "", "  "); err == nil {
		t.Logf("fingerprint %s:\n%s", f1.Hash(), b)
	}
	if path := os.Getenv("COYOTE_GUARDS_DUMP"); path != "" {
		b, _ := json.MarshalIndent(map[string]any{"fingerprint": f1, "probe": p1}, "", "  ")
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	total := 0
	for _, g := range p1.Groups {
		t.Logf("probe group %-8s must_match=%-5v statements=%d", g.Name, g.MustMatch, len(g.Statements))
		total += len(g.Statements)
		for _, st := range g.Statements {
			if st.SQLState != "" && !expectedError[g.Name+"/"+st.Name] {
				t.Errorf("probe %s/%s failed unexpectedly: %s", g.Name, st.Name, st.Message)
			}
			if st.SQLState == "" && expectedError[g.Name+"/"+st.Name] {
				t.Errorf("probe %s/%s should fail but answered", g.Name, st.Name)
			}
		}
	}
	t.Logf("probe: %d statements, digest %s", total, p1.Digest())

	f2, p2 := fp(), probe()
	s.restart()
	f3, p3 := fp(), probe()
	for i, f := range []pg.Fingerprint{f2, f3} {
		if d := pg.CompareFingerprints(f1, f); len(d) > 0 || f.Hash() != f1.Hash() {
			t.Errorf("fingerprint run %d differs: %v", i+2, d)
		}
	}
	for i, p := range []pg.ProbeResult{p2, p3} {
		if d := pg.CompareProbes(p1, p); len(d) > 0 || p.Digest() != p1.Digest() {
			t.Errorf("probe run %d differs: %+v", i+2, d)
		}
	}
}

// expectedError lists probe statements that test an error path.
var expectedError = map[string]bool{
	"float/f8_errors":           true,
	"float/f8_errors_underflow": true,
	"float/f8_errors_parse":     true,
	"numeric/overflow":          true,
	"integer/ovf_int2_add":      true,
	"integer/ovf_int4_add":      true,
	"integer/ovf_int4_div":      true,
	"integer/ovf_int4_abs":      true,
	"integer/ovf_int4_mul":      true,
	"integer/ovf_int8_add":      true,
	"integer/ovf_int8_div":      true,
	"integer/ovf_int8_mul":      true,
	"integer/ovf_parse":         true,
	"integer/ovf_div_zero":      true,
	"integer/ovf_f8_int4":       true,
	"time/far_date_limit":       true,
}
