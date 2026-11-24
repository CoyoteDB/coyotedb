//go:build integration && s3

// Backups to a real S3-compatible server (MinIO; docs/TESTING.md
// "Backups"). Needs COYOTE_TEST_S3_ENDPOINT, _BUCKET, _ACCESS_KEY and
// _SECRET_KEY; fails without them (no skipping).
package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

func s3Env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv("COYOTE_TEST_S3_" + k)
	if v == "" {
		t.Fatalf("COYOTE_TEST_S3_%s is not set (this test needs a real S3-compatible server)", k)
	}
	return v
}

// runWithEnv runs coyote with extra environment (secrets never in argv).
func runWithEnv(t *testing.T, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, coyoteBin, args...)
	cmd.Env = append(append(os.Environ(), "COYOTE_PG_CACHE="+pgCache), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("coyote %v: %v\n%s", args[:2], err, out)
	}
	return string(out)
}

var keyRe = regexp.MustCompile(`cybk-[A-Za-z0-9_-]+`)

// TestBackupS3: two data nodes and a witness back up to S3 (one shared,
// encrypted set); then, as if every node were lost, a restore from the
// bucket alone with the backup key brings back every write.
func TestBackupS3(t *testing.T) {
	endpoint, bucket, access, secret := s3Env(t, "ENDPOINT"), s3Env(t, "BUCKET"), s3Env(t, "ACCESS_KEY"), s3Env(t, "SECRET_KEY")
	prefix := fmt.Sprintf("itest-%d", time.Now().UnixNano())
	root := t.TempDir()
	a, b, w := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newWitness(t, root, "wit")
	a.bootstrap()
	c := a.conn()
	itemsOn(t, c)
	b.join(a)
	w.join(a)

	out := runWithEnv(t, []string{"COYOTE_S3_SECRET_KEY=" + secret}, "backup", "s3", "--endpoint", endpoint, "--bucket", bucket,
		"--prefix", prefix, "--access-key", access, "--data-dir", a.dir)
	if strings.Contains(out, secret) {
		t.Fatal("the secret key was printed")
	}
	key := keyRe.FindString(out)
	if key == "" || !strings.Contains(out, "Save it now") {
		t.Fatalf("no backup key shown:\n%s", out)
	}
	if again := keyRe.FindString(a.mustCLI("backup", "key")); again != key {
		t.Fatal("backup key differs on a second look")
	}
	a.mustCLI("backup", "schedule", "archive_timeout=10s")
	out = a.mustCLI("backup", "now", "--wait")
	if !strings.Contains(out, "Full backup started on node-b") || !strings.Contains(out, "Done:") {
		t.Fatalf("a shared backup is taken by the copy: %s", out)
	}
	if st := a.mustCLI("status"); strings.Contains(st, secret) || strings.Contains(st, key) {
		t.Fatal("status shows a secret")
	}
	insertRange(t, c, 101, 300, "after-")
	last := pgNow(t, c)
	a.waitArchivedAfter(last, 2*time.Minute)
	b.waitArchivedAfter(last, 2*time.Minute)
	want := queryText(t, c, tableHash)
	stopAll(a, b, w)

	// Every node is gone: restore from the bucket alone.
	r := newCNode(t, root, "restored")
	out = runWithEnv(t, []string{"COYOTE_S3_SECRET_KEY=" + secret, "COYOTE_BACKUP_KEY=" + key},
		"backup", "restore", "--into", r.dir, "--from", "s3://"+bucket+"/"+prefix, "--endpoint", endpoint, "--access-key", access)
	t.Logf("restore said:\n%s", out)
	rc := startRestored(t, r)
	if got := queryText(t, rc, tableHash); got != want {
		t.Fatalf("restored data differs (%s, want %s)", got, want)
	}

	// A wrong backup key is refused before anything is written.
	r2 := newCNode(t, root, "wrong-key")
	cmd := exec.Command(coyoteBin, "backup", "restore", "--into", r2.dir, "--from", "s3://"+bucket+"/"+prefix, "--endpoint", endpoint, "--access-key", access)
	cmd.Env = append(os.Environ(), "COYOTE_PG_CACHE="+pgCache, "COYOTE_S3_SECRET_KEY="+secret,
		"COYOTE_BACKUP_KEY=cybk-"+strings.Repeat("A", len(key)-5))
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "key") {
		t.Fatalf("a wrong key must be refused: %v\n%s", err, out)
	}
	if _, err := os.Stat(r2.dir); err == nil {
		t.Fatal("a refused restore created the folder")
	}
	stopAll(r)
}
