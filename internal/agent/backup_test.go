package agent

import (
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/backup"
	"coyotedb.com/coyote/internal/pg"
)

func TestArchiveProblem(t *testing.T) {
	const ours = `"/usr/local/bin/coyote" backup wal-push --data-dir "/data" "%p" "%f"`
	want := pg.ArchiveSettings{Command: ours, Timeout: "60s"}
	t0 := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	good := pg.ArchiveLive{Mode: "always", Command: ours, InRecovery: true, ArchivedCount: 3, LastArchived: "000000010000000000000004", LastArchivedAt: t0}
	cases := []struct {
		name string
		live pg.ArchiveLive
		want pg.ArchiveSettings
		ps   backup.PushState
		err  string // "" = may start
	}{
		{"copy, archiving live", good, want, backup.PushState{}, ""},
		{"fresh copy, nothing archived yet", pg.ArchiveLive{Mode: "always", Command: ours, InRecovery: true}, want, backup.PushState{}, ""},
		{"main node with archive_mode on", pg.ArchiveLive{Mode: "on", Command: ours}, want, backup.PushState{}, ""},
		{"copy with archive_mode on", pg.ArchiveLive{Mode: "on", Command: ours, InRecovery: true}, want, backup.PushState{}, "archive_mode is \"on\""},
		{"archiving off", pg.ArchiveLive{Mode: "off", Command: ours}, want, backup.PushState{}, "not active"},
		{"old command still running (before reload)", pg.ArchiveLive{Mode: "always", Command: "exit 0", InRecovery: true}, want, backup.PushState{}, "not picked up the backup settings"},
		{"archive library", pg.ArchiveLive{Mode: "always", Library: "basic_archive", InRecovery: true}, want, backup.PushState{}, "basic_archive"},
		{"nothing rendered", good, pg.ArchiveSettings{}, backup.PushState{}, "not set up"},
		{"failing since the last success", func() pg.ArchiveLive {
			l := good
			l.FailedCount, l.LastFailed, l.LastFailedAt = 7, "000000010000000000000005", t0.Add(time.Minute)
			return l
		}(), want, backup.PushState{LastError: "permission denied", LastErrorAt: t0.Add(time.Minute)}, "failing on this node (last failure: 000000010000000000000005"},
		{"failed once, archived since", func() pg.ArchiveLive {
			l := good
			l.FailedCount, l.LastFailed, l.LastFailedAt = 1, "000000010000000000000003", t0.Add(-time.Minute)
			return l
		}(), want, backup.PushState{}, ""},
		{"failing, never archived", pg.ArchiveLive{Mode: "always", Command: ours, InRecovery: true, FailedCount: 2, LastFailed: "000000010000000000000004", LastFailedAt: t0}, want, backup.PushState{}, "failing"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := archiveProblem(c.live, c.want, c.ps)
			switch {
			case c.err == "" && err != nil:
				t.Fatalf("want no problem, got %v", err)
			case c.err != "" && err == nil:
				t.Fatalf("want a problem with %q, got none", c.err)
			case c.err != "" && !strings.Contains(err.Error(), c.err):
				t.Fatalf("want %q in %q", c.err, err)
			}
		})
	}
	// The push error is carried into the words when it is the same failure.
	l := good
	l.LastFailed, l.LastFailedAt = "000000010000000000000005", t0.Add(time.Minute)
	err := archiveProblem(l, want, backup.PushState{LastError: "disk full", LastErrorAt: t0.Add(time.Minute)})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("push error missing: %v", err)
	}
}
