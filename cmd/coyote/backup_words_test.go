package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/backup"
)

// backupViews builds the states `coyote backup` and `coyote status` print,
// relative to now (the printers read the clock).
func backupViews(now time.Time) map[string]agent.BackupsView {
	sched := agent.BackupScheduleView{Enabled: true, Every: "24h0m0s", At: "03:00", Keep: "168h0m0s", ArchiveTimeout: "1m0s"}
	folder := agent.BackupDestView{Kind: "folder", Label: "/var/lib/coyote/node-a-backups", PerNode: true, SameDisk: true}
	last := now.Add(-10 * time.Hour).Truncate(time.Minute)
	oldest := last.Add(-6 * 24 * time.Hour)
	next := last.Add(24 * time.Hour)
	archived := now.Add(-20 * time.Second)
	item := agent.BackupItem{ID: "20261124T030041Z-node-a", Kind: "scheduled", State: "done", Node: "node-a", StartedAt: last.Add(-4 * time.Minute), FinishedAt: &last, SizeBytes: 1_270_000_000, DataBytes: 3_650_000_000}
	healthy := agent.BackupView{
		Enabled: true, Destination: folder, LastBackup: &item,
		Restorable: &agent.BackupWindow{From: oldest, To: archived}, NextAt: &next,
		Archive: agent.BackupArchive{LastArchived: "00000001000000000000002A", LastArchivedAt: &archived},
	}
	first := agent.BackupView{Enabled: true, Destination: folder, NextAt: ptr(now.Add(8 * time.Minute))}
	failing := now.Add(-47 * time.Minute)
	usb := agent.BackupDestView{Kind: "folder", Label: "/mnt/usb/coyote-backups", PerNode: true}
	attention := healthy
	attention.Destination = usb
	attention.Archive = agent.BackupArchive{LastArchivedAt: &failing, WaitingBytes: 224 << 20, LagMS: 47 * 60_000, FailingSince: &failing}
	attention.LastError = "cannot write to /mnt/usb/coyote-backups: no such file or directory"
	attention.Problem = "Saving changes to the backup has failed for 47 minutes: cannot write to /mnt/usb/coyote-backups: no such file or directory. Plug the drive back in, or move backups: `coyote backup folder PATH`."
	s3 := healthy
	s3.Destination = agent.BackupDestView{Kind: "s3", Label: "s3://bakery-backups/coyote (https://s3.us-west-004.backblazeb2.com)"}
	s3.Encrypted = true
	off := agent.BackupView{Enabled: false, Destination: folder}
	offSched := sched
	offSched.Enabled = false
	return map[string]agent.BackupsView{
		"healthy":   {Schedule: sched, Destination: healthy.Destination, Status: healthy, Backups: []agent.BackupItem{item}},
		"first":     {Schedule: sched, Destination: folder, Status: first},
		"attention": {Schedule: sched, Destination: usb, Status: attention, Backups: []agent.BackupItem{item}},
		"s3":        {Schedule: sched, Destination: s3.Destination, Status: s3, Backups: []agent.BackupItem{item}},
		"off":       {Schedule: offSched, Destination: folder, Status: off},
	}
}

func ptr[T any](v T) *T { return &v }

func TestBackupWords(t *testing.T) {
	views := backupViews(time.Now())
	want := map[string][]string{
		"healthy":   {"Backups are on and working.", "Last full backup", "any moment from ", "same disk as the database", "What you can do", "coyote backup restore"},
		"first":     {"Backups are on. The first full backup is taken at ", "What you can do"},
		"attention": {"Backups need attention.", "! Saving changes to the backup has failed"},
		"s3":        {"Backups are on and working.", "s3://bakery-backups/coyote", "encrypted"},
		"off":       {"Backups are off.", "Nothing is saved, so a deleted table or a bad change cannot be undone.", "coyote backup on"},
	}
	out := os.Getenv("COYOTE_SAMPLE_DIR")
	for name, v := range views {
		var ov, st bytes.Buffer
		printBackupOverview(&ov, v)
		printBackupStatus(&st, &v.Status)
		for _, w := range want[name] {
			if !strings.Contains(ov.String(), w) {
				t.Errorf("%s overview lacks %q:\n%s", name, w, ov.String())
			}
		}
		// `coyote status` starts with the same one-line summary.
		if head := strings.SplitN(ov.String(), "\n", 2)[0]; !strings.Contains(st.String(), head) {
			t.Errorf("%s: status block does not start with %q:\n%s", name, head, st.String())
		}
		for _, line := range strings.Split(ov.String()+st.String(), "\n") {
			if len([]rune(line)) > 100 {
				t.Errorf("%s: line over 100 columns: %q", name, line)
			}
		}
		if out != "" {
			text := fmt.Sprintf("$ coyote backup\n%s\n$ coyote status   (backup lines)\n%s", ov.String(), st.String())
			if err := os.WriteFile(filepath.Join(out, "terminal-backup-"+name+".txt"), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRestoreNext(t *testing.T) {
	cases := []struct {
		err   error
		check bool
		want  string
	}{
		{fmt.Errorf("x: %w", backup.ErrNotRestorable), false, "Pick a moment the backups reach"},
		{fmt.Errorf("x: %w", backup.ErrWrongKey), false, "backup key"},
		{errors.New("/tmp/x is not empty"), false, "Pick a new folder name"},
		{errors.New("amcheck: damage"), true, "Your running database is fine; this backup is not."},
		{errors.New("pg_ctl failed"), false, "Nothing in the running cluster was changed."},
	}
	for _, c := range cases {
		if got := restoreNext(c.err, c.check); !strings.Contains(got, c.want) {
			t.Errorf("restoreNext(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}
