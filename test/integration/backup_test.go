//go:build integration

// Integration tests for backups and point-in-time restore (M7, D63): real
// agents, real Postgres, real pg_basebackup, archive_command and recovery.
package integration

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	coyoteagent "coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/backup"
)

func (n *cnode) backups() coyoteagent.BackupsView {
	n.t.Helper()
	var v coyoteagent.BackupsView
	n.cliJSON(&v, "backup", "--json")
	return v
}

// waitArchivedAfter waits until this node has archived WAL written after
// t (a segment it finished after t) and nothing waits.
func (n *cnode) waitArchivedAfter(t time.Time, within time.Duration) {
	n.t.Helper()
	waitFor(n.t, within, n.name+" to archive everything written before "+t.Format(time.TimeOnly), func() bool {
		v := n.backups()
		a := v.Status.Archive
		return a.LastArchivedAt != nil && a.LastArchivedAt.After(t.Add(time.Second)) && a.WaitingBytes == 0
	})
}

// waitOwnBackup waits until n lists a finished full backup.
func (n *cnode) waitOwnBackup(within time.Duration) coyoteagent.BackupItem {
	n.t.Helper()
	var b coyoteagent.BackupItem
	waitFor(n.t, within, n.name+" to list a finished full backup", func() bool {
		var list []coyoteagent.BackupItem
		n.cliJSON(&list, "backup", "list", "--json")
		for _, x := range list {
			if x.State == "done" {
				b = x
				return true
			}
		}
		return false
	})
	return b
}

func pgNow(t *testing.T, c *pgx.Conn) time.Time {
	t.Helper()
	var ts time.Time
	if err := c.QueryRow(context.Background(), "SELECT clock_timestamp()").Scan(&ts); err != nil {
		t.Fatal(err)
	}
	return ts
}

func tableExists(t *testing.T, c *pgx.Conn, name string) bool {
	t.Helper()
	return queryInt(t, c, "SELECT count(*) FROM pg_class WHERE relname = $1 AND relkind = 'r'", name) == 1
}

// startRestored starts a restored folder as a new one-node cluster.
func startRestored(t *testing.T, r *cnode) *pgx.Conn {
	t.Helper()
	r.bootstrap()
	return r.conn()
}

// TestBackupPointInTimeRestore: full backup, more writes, a dropped table;
// restoring to a moment before the drop brings back the table and every
// write acknowledged up to that moment, and nothing after it. The live
// node is not touched. `backup check` passes.
func TestBackupPointInTimeRestore(t *testing.T) {
	root := t.TempDir()
	a := newCNode(t, root, "node-a")
	a.bootstrap()

	v := a.backups()
	if !v.Status.Enabled || v.Destination.Kind != "folder" || !strings.HasSuffix(v.Destination.Label, "node node-a-backups") {
		t.Fatalf("backups not on by default next to the data folder: %+v", v.Destination)
	}
	folder := filepath.Join(root, "my backups") // a space on purpose
	out := a.mustCLI("backup", "folder", folder)
	if !strings.Contains(out, "Saved. Backups now go to "+folder) {
		t.Fatalf("backup folder: %s", out)
	}
	a.mustCLI("backup", "schedule", "archive_timeout=10s")

	c := a.conn()
	itemsOn(t, c)
	out = a.mustCLI("backup", "now", "--wait")
	if !strings.Contains(out, "Done:") {
		t.Fatalf("backup now: %s", out)
	}
	insertRange(t, c, 101, 200, "after-backup-")
	target := pgNow(t, c)
	mustExec(t, c, "DROP TABLE items")
	mustExec(t, c, "CREATE TABLE after_drop (x int)")
	afterDrop := pgNow(t, c)
	a.waitArchivedAfter(afterDrop, 2*time.Minute)

	st := a.mustCLI("status")
	if !strings.Contains(st, "Last full backup") || !strings.Contains(st, "any moment from ") {
		t.Fatalf("status does not show backups:\n%s", st)
	}

	// Refused: a folder that is not empty.
	if out, err := a.cli("backup", "restore", "--into", a.dir); err == nil || !strings.Contains(out, "not empty") {
		t.Fatalf("restore into the live folder must be refused: %v\n%s", err, out)
	}
	r := newCNode(t, root, "restored")
	out = a.mustCLI("backup", "restore", "--into", r.dir, "--to", target.Format(time.RFC3339Nano))
	if !strings.Contains(out, "Restored into "+r.dir) || !strings.Contains(out, "Nothing in the running cluster was changed") {
		t.Fatalf("restore: %s", out)
	}
	t.Logf("restore said:\n%s", out)

	// The live node still runs, without the table.
	if tableExists(t, c, "items") || !tableExists(t, c, "after_drop") {
		t.Fatal("the live node was changed by the restore")
	}
	rc := startRestored(t, r)
	if got := queryInt(t, rc, "SELECT count(*) FROM items"); got != 200 {
		t.Fatalf("restored items has %d rows, want 200", got)
	}
	if got := queryInt(t, rc, "SELECT count(*) FROM items WHERE body LIKE 'after-backup-%'"); got != 100 {
		t.Fatalf("restored %d rows written after the full backup, want 100", got)
	}
	if tableExists(t, rc, "after_drop") {
		t.Fatal("the restore went past the chosen moment")
	}
	mustExec(t, rc, "INSERT INTO items VALUES (201, 'restored copy is writable')")

	// A restore to the newest moment has everything, including the drop.
	r2 := newCNode(t, root, "latest")
	a.mustCLI("backup", "restore", "--into", r2.dir)
	rc2 := startRestored(t, r2)
	if tableExists(t, rc2, "items") || !tableExists(t, rc2, "after_drop") {
		t.Fatal("latest restore does not match the live node")
	}

	out = a.mustCLI("backup", "check")
	if !strings.Contains(out, "Backup check passed") {
		t.Fatalf("check: %s", out)
	}
	t.Logf("check said: %s", strings.TrimSpace(out))
	ents, _ := os.ReadDir(backup.StateDir(a.dir))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "check-") {
			t.Fatalf("check left %s behind", e.Name())
		}
	}
	stopAll(r, r2, a)
}

// ackWriter inserts ids one by one into whichever node runs read-write
// (as TestM3SwitchUnderLoad) and remembers which inserts were acknowledged.
type ackWriter struct {
	mu    sync.Mutex
	acked []int
	last  error // last failure, for a test failure message
	stop  chan struct{}
	done  chan struct{}
}

func startAckWriter(t *testing.T, from int, nodes ...*cnode) *ackWriter {
	t.Helper()
	urls := make([]string, len(nodes))
	for i, n := range nodes {
		urls[i] = connString(t, n)
	}
	w := &ackWriter{stop: make(chan struct{}), done: make(chan struct{})}
	fail := func(err error) {
		w.mu.Lock()
		w.last = err
		w.mu.Unlock()
	}
	go func() {
		defer close(w.done)
		var c *pgx.Conn
		id := from
		for {
			select {
			case <-w.stop:
				if c != nil {
					c.Close(context.Background())
				}
				return
			default:
			}
			if c == nil {
				for _, u := range urls {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					cc, err := pgx.Connect(ctx, u)
					cancel()
					if err != nil {
						fail(err)
						continue
					}
					var ro bool
					if cc.QueryRow(context.Background(), "SELECT pg_is_in_recovery()").Scan(&ro) == nil && !ro {
						c = cc
						break
					}
					cc.Close(context.Background())
				}
				if c == nil {
					time.Sleep(50 * time.Millisecond) // retry interval while no main node answers
					continue
				}
			}
			id++
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			_, err := c.Exec(ctx, "INSERT INTO items VALUES ($1, 'acked')", id)
			cancel()
			if err != nil {
				fail(err)
				c.Close(context.Background())
				c = nil
				continue
			}
			w.mu.Lock()
			w.acked = append(w.acked, id)
			w.mu.Unlock()
		}
	}()
	return w
}

func (w *ackWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.acked)
}

func (w *ackWriter) lastErr() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

func (w *ackWriter) finish() []int {
	close(w.stop)
	<-w.done
	return w.acked
}

// checkAcked fails unless every acknowledged id is in c's items.
func checkAcked(t *testing.T, c *pgx.Conn, acked []int) {
	t.Helper()
	rows, err := c.Query(context.Background(), "SELECT id FROM items")
	if err != nil {
		t.Fatal(err)
	}
	have := map[int]bool{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		have[id] = true
	}
	missing := 0
	for _, id := range acked {
		if !have[id] {
			missing++
		}
	}
	if missing > 0 {
		t.Fatalf("%d of %d acknowledged writes are missing from the restore", missing, len(acked))
	}
}

func timelineOf(t *testing.T, c *pgx.Conn) int {
	t.Helper()
	return queryInt(t, c, "SELECT timeline_id FROM pg_control_checkpoint()")
}

// TestBackupAcrossFailover: the main node is killed while writes and
// archiving run; the new main node's own backup folder restores every
// acknowledged write, across the timeline switch.
func TestBackupAcrossFailover(t *testing.T) {
	root := t.TempDir()
	a, b, c := newCNode(t, root, "node-a"), newCNode(t, root, "node-b"), newCNode(t, root, "node-c")
	dumpBackupState(t, a, b, c)
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	c.join(a)
	syncFull(t, a, b, c)
	a.mustCLI("backup", "schedule", "archive_timeout=10s")
	out := a.mustCLI("backup", "now")
	if !strings.Contains(out, "node-a") || !strings.Contains(out, "node-b") || !strings.Contains(out, "node-c") {
		t.Fatalf("a folder backup runs on every data node: %s", out)
	}
	for _, n := range []*cnode{a, b, c} {
		n.waitOwnBackup(5 * time.Minute)
	}

	w := startAckWriter(t, 1000, a, b, c)
	waitFor(t, time.Minute, "writes flowing", func() bool {
		if err := w.lastErr(); err != nil {
			t.Logf("writer: %v", err)
		}
		return w.count() > 50
	})
	a.killAgent()
	var p *cnode
	waitFor(t, 90*time.Second, "a new main node", func() bool {
		p = primaryOf(t, b, c)
		return p != nil && p.clusterStatus().Cluster.PrimaryName == p.name
	})
	n0 := w.count()
	waitFor(t, time.Minute, "writes on the new main node", func() bool { return w.count() > n0+50 })
	acked := w.finish()
	pc := p.conn()
	if tl := timelineOf(t, pc); tl < 2 {
		t.Fatalf("no timeline switch (timeline %d)", tl)
	}
	p.waitArchivedAfter(pgNow(t, pc), 2*time.Minute)

	r := newCNode(t, root, "restored")
	out = p.mustCLI("backup", "restore", "--into", r.dir)
	t.Logf("restore said:\n%s", out)
	rc := startRestored(t, r)
	checkAcked(t, rc, acked)
	if got := queryInt(t, rc, "SELECT count(*) FROM items WHERE id <= 100"); got != 100 {
		t.Fatalf("rows from before the backup: %d", got)
	}
	t.Logf("restored %d acknowledged writes across the failover; restored timeline %d", len(acked), timelineOf(t, rc))
	stopAll(r, b, c)
}

// dumpBackupState logs, on failure, what a stuck backup needs: the tail
// of each node's Postgres log and its WAL archive status files.
func dumpBackupState(t *testing.T, nodes ...*cnode) {
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, n := range nodes {
			logs, _ := filepath.Glob(filepath.Join(n.dir, "logs", "postgresql-*.log"))
			for _, l := range logs {
				logTail(t, n.name+" postgres", l, 60)
			}
			wal, _ := os.ReadDir(filepath.Join(n.pgdata, "pg_wal"))
			st, _ := os.ReadDir(filepath.Join(n.pgdata, "pg_wal", "archive_status"))
			var names []string
			for _, e := range wal {
				names = append(names, e.Name())
			}
			names = append(names, "|")
			for _, e := range st {
				names = append(names, e.Name())
			}
			t.Logf("%s pg_wal: %s", n.name, strings.Join(names, " "))
		}
	})
}

// TestBackupRightAfterJoin: a copy whose clone left its last WAL segment
// with no archive status archives that segment, and a full backup asked
// for the moment the copy is up finishes on it.
//
// Found on a slower Linux test machine (2026-11-24, 2 of 2 runs): the main node ends a
// base backup with a segment switch, so the clone's last segment is
// complete, but pg_basebackup marks it done only if its WAL stream got the
// whole segment before stopping (a race: on a fast machine it usually does, on
// the slower one it did not). The copy replays that segment from its own pg_wal and
// streams from the next one, so nothing in Postgres marks it for archiving.
// It is also where the copy's first full backup starts: pg_basebackup -X
// none waited for it without end, and the copy's own folder never held it.
// The test forces that state: it removes the status pg_basebackup may have
// written (and the copy of it the backup may have saved) while the copy is
// stopped.
func TestBackupRightAfterJoin(t *testing.T) {
	root := t.TempDir()
	a, b := newCNode(t, root, "node-a"), newCNode(t, root, "node-b")
	dumpBackupState(t, a, b)
	a.bootstrap()
	itemsOn(t, a.conn())
	b.join(a)
	label, err := os.ReadFile(filepath.Join(b.pgdata, "backup_label.old"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`START WAL LOCATION: \S+ \(file ([0-9A-F]{24})\)`).FindSubmatch(label)
	if m == nil {
		t.Fatalf("no start segment in the clone's backup_label:\n%s", label)
	}
	seg := string(m[1])
	b.a.stop(t)
	waitNoNodeProcs(t, b.dir, 15*time.Second)
	for _, p := range []string{
		filepath.Join(b.pgdata, "pg_wal", "archive_status", seg+".done"),
		filepath.Join(b.pgdata, "pg_wal", "archive_status", seg+".ready"),
	} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	saved, _ := filepath.Glob(filepath.Join(b.dir+"-backups", "*", "wal", seg))
	for _, p := range saved {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(b.pgdata, "pg_wal", seg)); err != nil {
		t.Fatalf("the clone's segment is not in pg_wal: %v", err)
	}

	b.restart()
	out := b.mustCLI("backup", "now")
	if !strings.Contains(out, "node-b") {
		t.Fatalf("backup now: %s", out)
	}
	waitFor(t, 2*time.Minute, "node-b to archive "+seg+" (the segment its clone ended in) to its own folder", func() bool {
		got, _ := filepath.Glob(filepath.Join(b.dir+"-backups", "*", "wal", seg))
		return len(got) == 1
	})
	a.waitOwnBackup(3 * time.Minute)
	b.waitOwnBackup(3 * time.Minute)
	stopAll(a, b)
}

// TestBackupWaitsWhileArchivingFails: a full backup is not started while
// this node cannot save WAL (pg_basebackup would wait for the archive
// without end); status says why in plain words, and the backup starts by
// itself once saving works again.
func TestBackupWaitsWhileArchivingFails(t *testing.T) {
	root := t.TempDir()
	a := newCNode(t, root, "node-a")
	dumpBackupState(t, a)
	a.bootstrap()
	c := a.conn()
	itemsOn(t, c)
	mustExec(t, c, "SELECT pg_switch_wal()")
	a.waitArchivedAfter(time.Now().Add(-time.Second), 2*time.Minute)

	// Break the destination: its wal folder becomes a file.
	walDirs, err := filepath.Glob(filepath.Join(a.dir+"-backups", "*", "wal"))
	if err != nil || len(walDirs) != 1 {
		t.Fatalf("wal folder of the default destination: %v %v", walDirs, err)
	}
	walDir := walDirs[0]
	if err := os.RemoveAll(walDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(walDir, []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	insertRange(t, c, 101, 200, "while-broken-")
	mustExec(t, c, "SELECT pg_switch_wal()")
	waitFor(t, time.Minute, "archiving to fail", func() bool {
		return queryInt(t, c, "SELECT failed_count FROM pg_stat_archiver") > 0
	})

	out := a.mustCLI("backup", "now")
	if !strings.Contains(out, "node-a") {
		t.Fatalf("backup now: %s", out)
	}
	var v coyoteagent.BackupsView
	waitFor(t, time.Minute, "status to say why the backup waits", func() bool {
		v = a.backups()
		return strings.Contains(v.Status.Waiting, "failing")
	})
	if v.Status.Running != nil {
		t.Fatalf("a backup is running while archiving fails: %+v", v.Status.Running)
	}
	st := a.mustCLI("backup")
	if !strings.Contains(st, "waits to start: saving changes (WAL) to the backup place is failing") {
		t.Fatalf("`backup` does not say why:\n%s", st)
	}
	for _, b := range v.Backups {
		if b.State == "done" {
			t.Fatalf("a backup is listed as done while archiving fails: %+v", b)
		}
	}

	// Fixed: the waiting backup starts by itself and finishes.
	if err := os.Remove(walDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(walDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, "SELECT pg_switch_wal()")
	a.waitOwnBackup(4 * time.Minute)
	if v := a.backups(); v.Status.Waiting != "" {
		t.Fatalf("still waiting after the backup: %q", v.Status.Waiting)
	}
	stopAll(a)
}
