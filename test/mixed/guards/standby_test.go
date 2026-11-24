//go:build integration

package guards

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// newStandby clones p with pg_basebackup and starts it as a streaming
// standby.
func newStandby(t *testing.T, p *server) *server {
	t.Helper()
	dir, err := os.MkdirTemp("", "coyote guards standby ")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	n, err := pg.NewNode(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(n.PasswordFile(), []byte(p.pw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(inst.Bin("pg_basebackup"), "-h", "127.0.0.1", "-p", strconv.Itoa(p.port), "-U", pg.Superuser, "-D", n.PGData(), "-R", "-X", "stream", "-c", "fast")
	cmd.Env = append(cleanEnv(), "LC_ALL=C", "PGPASSFILE="+p.passfile())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("pg_basebackup: %v\n%s", err, out.String())
	}
	s := &server{t: t, node: n, pw: p.pw}
	s.start()
	t.Cleanup(s.stop)
	return s
}

// The checks run on a standby (read-only), and a primary and its
// standby compare equal: same fingerprint, same probe answers, the same
// audit findings, and a clean integrity check on both.
func TestPrimaryAndStandbyAgree(t *testing.T) {
	p := newServer(t)
	ctx := ctxT(t, 10*time.Minute)
	p.exec("postgres", healthySchema...)
	p.exec("template1", "CREATE EXTENSION amcheck")
	sb := newStandby(t, p)
	p.exec("postgres", "INSERT INTO t (id, txt) SELECT g, 'after clone ' || g FROM generate_series(100001, 101000) g")
	// Wait until the standby has replayed everything.
	var lsn string
	if err := p.connect("postgres").QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn); err != nil {
		t.Fatal(err)
	}
	sc := sb.connect("postgres")
	for {
		var caught bool
		if err := sc.QueryRow(ctx, "SELECT pg_is_in_recovery() AND pg_last_wal_replay_lsn() >= $1::pg_lsn", lsn).Scan(&caught); err != nil {
			t.Fatal(err)
		}
		if caught {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("standby did not catch up")
		case <-time.After(50 * time.Millisecond):
		}
	}

	fpP, err := pg.CollectFingerprint(ctx, inst, p.node.PGData(), p.connect("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	fpS, err := pg.CollectFingerprint(ctx, inst, sb.node.PGData(), sc)
	if err != nil {
		t.Fatal(err)
	}
	if d := pg.CompareFingerprints(fpP, fpS); len(d) > 0 {
		t.Errorf("fingerprints differ: %v", d)
	}
	prP, err := pg.RunProbe(ctx, p.connect("postgres"), pg.ProbeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prS, err := pg.RunProbe(ctx, sc, pg.ProbeOptions{})
	if err != nil {
		t.Fatalf("probe on a standby: %v", err)
	}
	if d := pg.CompareProbes(prP, prS); len(d) > 0 {
		t.Errorf("probe differs between primary and standby: %+v", d)
	}
	auP, err := pg.AuditCluster(ctx, p.connectFunc(), pg.AuditOptions{})
	if err != nil {
		t.Fatal(err)
	}
	auS, err := pg.AuditCluster(ctx, sb.connectFunc(), pg.AuditOptions{})
	if err != nil {
		t.Fatalf("audit on a standby: %v", err)
	}
	if auP.Digest() != auS.Digest() {
		t.Errorf("audit differs:\nprimary %v\nstandby %v", auP.Findings, auS.Findings)
	}
	for _, s := range []*server{p, sb} {
		// Deep with AllowBlocking: on the standby the parent check is not
		// possible and the check falls back to bt_index_check.
		r, err := pg.VerifyCluster(ctx, s.connectFunc(), pg.VerifyOptions{Level: pg.VerifyDeep, AllowBlocking: true, Seed: 11})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%v", r.Checked)
		for _, f := range r.Findings {
			if f.Severity != pg.SeverityInfo {
				t.Errorf("finding: %s (%s)", f, f.Detail)
			}
		}
		if r.Checked["index_scan"] < 9 || r.Checked["partition"] != 5 {
			t.Errorf("too little checked: %v", r.Checked)
		}
	}
}
