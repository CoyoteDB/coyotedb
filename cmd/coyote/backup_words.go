package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/backup"
	"coyotedb.com/coyote/internal/brand"
)

// The words of `coyote backup` and the backup lines of `coyote status`.
// They match the dashboard's (dashboard/src/lib/backups.ts): one plain
// summary line first, then aligned details, then what to do.

// backupHeadline is the one-line summary.
func backupHeadline(s *agent.BackupView, now time.Time) string {
	switch {
	case !s.Enabled:
		return "Backups are off."
	case s.Problem != "":
		return "Backups need attention."
	case s.LastBackup == nil && s.Running != nil:
		return "Backups are on. The first full backup is running now."
	case s.LastBackup == nil && s.NextAt != nil:
		return "Backups are on. The first full backup is taken " + whenText(*s.NextAt, now) + "."
	case s.LastBackup == nil:
		return "Backups are on. The first full backup is taken soon."
	}
	return "Backups are on and working."
}

// scheduleText: "every change within 1 min · a full copy every day at 03:00 · kept 7 days".
func scheduleText(v agent.BackupScheduleView) string {
	full := "a full copy every " + humanDur(v.Every)
	if d, err := time.ParseDuration(v.Every); err == nil && d%(24*time.Hour) == 0 && v.At != "" {
		full += " at " + v.At
	}
	return fmt.Sprintf("every change within %s · %s · kept %s", humanDur(v.ArchiveTimeout), full, humanDur(v.Keep))
}

// whereNote is the second line under "Saved to".
func whereNote(s *agent.BackupView) string {
	var parts []string
	if s.Destination.PerNode {
		parts = append(parts, "each data computer keeps its own full set")
	} else {
		parts = append(parts, "one shared set for the cluster")
	}
	if s.Encrypted {
		parts = append(parts, "encrypted")
	}
	if s.Destination.SameDisk {
		parts = append(parts, "same disk as the database")
	}
	return strings.Join(parts, " · ")
}

// whenText: "at 14:05 today", "tomorrow at 03:00", "on Mon 17 Nov at 03:00"
// (used after a verb: "is taken at 14:05 today").
func whenText(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	day := func(x time.Time) time.Time { y, m, d := x.Date(); return time.Date(y, m, d, 0, 0, 0, 0, time.Local) }
	hm := t.Format("15:04")
	switch int(day(t).Sub(day(now)).Hours() / 24) {
	case 0:
		return "at " + hm + " today"
	case 1:
		return "tomorrow at " + hm
	case -1:
		return "yesterday at " + hm
	}
	return "on " + t.Format("Mon 2 Jan") + " at " + hm
}

// stamp: "today 14:05", "yesterday 03:00", "Mon 17 Nov 03:00" (a table cell).
func stamp(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	day := func(x time.Time) time.Time { y, m, d := x.Date(); return time.Date(y, m, d, 0, 0, 0, 0, time.Local) }
	hm := t.Format("15:04")
	switch int(day(t).Sub(day(now)).Hours() / 24) {
	case 0:
		return "today " + hm
	case 1:
		return "tomorrow " + hm
	case -1:
		return "yesterday " + hm
	}
	return t.Format("Mon 2 Jan 15:04")
}

// rows prints aligned "  Label   value" lines; an empty label continues
// the line above.
func rows(w io.Writer, kv [][2]string) {
	width := 0
	for _, r := range kv {
		width = max(width, len(r[0]))
	}
	for _, r := range kv {
		if r[1] == "" {
			continue
		}
		fmt.Fprintf(w, "  %-*s  %s\n", width, r[0], r[1])
	}
}

// backupDetails are the rows under the headline.
func backupDetails(s *agent.BackupView, sched *agent.BackupScheduleView, now time.Time) [][2]string {
	var kv [][2]string
	if sched != nil {
		kv = append(kv, [2]string{"Saved to", s.Destination.Label}, [2]string{"", whereNote(s)})
		if s.Enabled {
			kv = append(kv, [2]string{"Schedule", scheduleText(*sched)})
		}
	}
	if !s.Enabled {
		return kv
	}
	last := "none yet"
	if b := s.LastBackup; b != nil && b.FinishedAt != nil {
		last = fmt.Sprintf("%s (%s ago) · %s · taken by %s", stamp(*b.FinishedAt, now), humanAgo(*b.FinishedAt), humanSize(b.SizeBytes), b.Node)
	}
	kv = append(kv, [2]string{"Last full backup", last})
	if r := s.Restorable; r != nil {
		kv = append(kv, [2]string{"Restore to", "any moment from " + stamp(r.From, now) + " to " + stamp(r.To, now)})
	} else {
		kv = append(kv, [2]string{"Restore to", "nothing yet (after the first full backup)"})
	}
	switch {
	case s.Running != nil:
		kv = append(kv, [2]string{"Now", "a full backup is running on " + s.Running.Node + " (since " + s.Running.StartedAt.Local().Format("15:04") + ")"})
	case s.Waiting != "":
		kv = append(kv, [2]string{"Now", "a full backup waits to start: " + s.Waiting})
	case s.NextAt != nil:
		kv = append(kv, [2]string{"Next full backup", stamp(*s.NextAt, now)})
	}
	if s.Archive.WaitingBytes > 0 && s.Archive.LagMS > 60_000 {
		kv = append(kv, [2]string{"Waiting", fmt.Sprintf("%s of changes not saved yet (the oldest for %s)", humanSize(s.Archive.WaitingBytes), humanAgo(time.Now().Add(-time.Duration(s.Archive.LagMS)*time.Millisecond)))})
	}
	return kv
}

// printBackupStatus prints the backup block of `status`.
func printBackupStatus(w io.Writer, s *agent.BackupView) {
	if s == nil {
		return
	}
	now := time.Now()
	fmt.Fprintf(w, "\n%s\n", backupHeadline(s, now))
	if !s.Enabled {
		fmt.Fprintf(w, "  Nothing is saved, so a deleted table or a bad change cannot be undone.\n  Turn them on: `%s backup on`.\n", brand.Command)
		return
	}
	rows(w, backupDetails(s, nil, now))
	if s.Problem != "" {
		fmt.Fprint(w, wrap("! ", s.Problem, 88))
	}
}

// printBackupOverview is `coyote backup` with no arguments: where, when,
// the last backup, how far back, and what to do next.
func printBackupOverview(w io.Writer, v agent.BackupsView) {
	now := time.Now()
	s := &v.Status
	fmt.Fprintf(w, "%s\n\n", backupHeadline(s, now))
	if !s.Enabled {
		fmt.Fprintln(w, "  Nothing is saved, so a deleted table or a bad change cannot be undone.")
		fmt.Fprintln(w)
		cmds(w, [][2]string{
			{"backup on", "turn them on (every change saved, a full copy every night, kept 7 days)"},
			{"backup help", "all backup commands"},
		})
		return
	}
	rows(w, backupDetails(s, &v.Schedule, now))
	if s.Problem != "" {
		fmt.Fprint(w, "\n"+wrap("! ", s.Problem, 88))
	}
	if s.Destination.SameDisk && s.Problem == "" {
		fmt.Fprintln(w, "\n  Tip: the backups are on the same disk as the database. That protects you from")
		fmt.Fprintf(w, "  mistakes, not from this disk failing. Move them: `%s backup folder PATH-ON-ANOTHER-DISK`.\n", brand.Command)
	}
	fmt.Fprintln(w, "\nWhat you can do")
	c := [][2]string{
		{"backup now", "take a full backup now"},
		{"backup list", "see every full backup"},
		{"backup check", "prove the newest backup restores (a few minutes)"},
		{`backup restore --to "15m ago" --into NEW-FOLDER`, "get the database back as it was, into a new folder"},
	}
	if s.Destination.Kind == "folder" {
		c = append(c, [2]string{"backup s3 --endpoint URL --bucket NAME --access-key ID", "keep backups in cloud storage (encrypted)"})
	}
	c = append(c, [2]string{"backup help", "all backup commands"})
	cmds(w, c)
}

// cmds prints "  coyote CMD   what it does", the text below a long command.
func cmds(w io.Writer, c [][2]string) {
	width := 0
	for _, r := range c {
		if l := len(r[0]); l <= 24 {
			width = max(width, l)
		}
	}
	for _, r := range c {
		cmd := brand.Command + " " + r[0]
		if len(r[0]) > 24 {
			fmt.Fprintf(w, "  %s\n  %-*s  %s\n", cmd, width+len(brand.Command)+1, "", r[1])
			continue
		}
		fmt.Fprintf(w, "  %-*s  %s\n", width+len(brand.Command)+1, cmd, r[1])
	}
}

// restoreNext says what to do after a failed restore or check.
func restoreNext(err error, check bool) string {
	msg := err.Error()
	switch {
	case errors.Is(err, backup.ErrNotRestorable):
		return fmt.Sprintf("Pick a moment the backups reach: `%s backup` shows the range.", brand.Command)
	case errors.Is(err, backup.ErrWrongKey):
		return fmt.Sprintf("Use the backup key shown when the backups were set up (on a cluster computer: `%s backup key`), in %s.", brand.Command, backupKeyEnv)
	case strings.Contains(msg, "not empty"):
		return "Pick a new folder name: a restore never writes over anything."
	case strings.Contains(msg, "in use by another program"):
		return "Another program uses that folder. Pick a new folder."
	case check:
		return fmt.Sprintf("Your running database is fine; this backup is not. Take a new one (`%s backup now --wait`) and check again. If that fails too, look at the backup lines in the node's log.", brand.Command)
	}
	return fmt.Sprintf("Nothing in the running cluster was changed. Fix the cause above and run the restore again into a new, empty folder. `%s backup` shows what can be restored.", brand.Command)
}

// wrap breaks text into lines of at most width columns: the first line
// starts with lead, the rest are indented to match. Long words (paths)
// stay whole.
func wrap(lead, text string, width int) string {
	var b strings.Builder
	pad := strings.Repeat(" ", len(lead))
	line, words := lead, 0
	for _, word := range strings.Fields(text) {
		if words > 0 && len(line)+1+len(word) > width {
			b.WriteString(line + "\n")
			line, words = pad, 0
		}
		if words > 0 {
			line += " "
		}
		line += word
		words++
	}
	b.WriteString(line + "\n")
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
