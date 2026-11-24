package pg

import (
	"slices"
	"strings"
	"testing"
)

func TestRenderArchiveSettings(t *testing.T) {
	off := RenderConf(Config{Port: 7555, LogDir: "/logs"})
	for _, want := range []string{"archive_mode = always\n", "archive_command = 'exit 0'\n", "archive_timeout = '0'\n"} {
		if !strings.Contains(off, want) {
			t.Errorf("off: missing %q", want)
		}
	}
	cmd, err := Node{StateDir: "/srv/node a"}.ArchiveCommandFor("/opt/coyote 100%/coyote")
	if err != nil {
		t.Fatal(err)
	}
	if cmd != `"/opt/coyote 100%%/coyote" backup wal-push --data-dir "/srv/node a" "%p" "%f"` {
		t.Fatal(cmd)
	}
	on := RenderConf(Config{Port: 7555, LogDir: "/logs", Archive: ArchiveSettings{Command: cmd, Timeout: "60s"}})
	if !strings.Contains(on, "archive_timeout = '60s'\n") || !strings.Contains(on, `backup wal-push`) {
		t.Fatalf("on:\n%s", on)
	}
	if _, err := (Node{StateDir: "/srv/$HOME"}).ArchiveCommandFor("/bin/coyote"); err == nil {
		t.Fatal("a $ in the path must be refused")
	}
}

func TestParseBackupLabel(t *testing.T) {
	label := "START WAL LOCATION: 0/5000028 (file 000000030000000000000005)\nCHECKPOINT LOCATION: 0/5000080\nBACKUP METHOD: streamed\nBACKUP FROM: standby\nSTART TIME: 2026-11-24 09:56:01 CST\nLABEL: pg_basebackup base backup\nSTART TIMELINE: 3\n"
	lsn, tli, err := parseBackupLabel([]byte(label))
	if err != nil || lsn.String() != "0/5000028" || tli != 3 {
		t.Fatalf("%s %d %v", lsn, tli, err)
	}
	if _, _, err := parseBackupLabel([]byte("LABEL: x\n")); err == nil {
		t.Fatal("accepted a label without a start")
	}
}

func TestPlanUnarchived(t *testing.T) {
	full := func(names ...string) []walFileInfo {
		var f []walFileInfo
		for _, n := range names {
			f = append(f, walFileInfo{Name: n, Size: walSegSize})
		}
		return f
	}
	const (
		s3 = "000000010000000000000003"
		s4 = "000000010000000000000004"
		s5 = "000000010000000000000005"
		s6 = "000000010000000000000006"
		f9 = "000000010000000000000009" // recycled, future
	)
	cases := []struct {
		name    string
		files   []walFileInfo
		status  map[string]bool
		w       walPosition
		adopt   []string
		pending string
	}{
		{
			// The bug: the clone's last segment (replayed from pg_wal,
			// streaming started at the next one) has no status at all.
			name:   "clone's last segment, copy streaming the next",
			files:  full(s4, s5, f9),
			status: map[string]bool{},
			w:      walPosition{TLI: 1, ReplaySeg: 5, Receiving: true, ReceiveSeg: 5},
			adopt:  []string{s4},
		},
		{
			name:   "already marked by Postgres or pg_basebackup",
			files:  full(s3, s4, s5),
			status: map[string]bool{s3: true, s4: true},
			w:      walPosition{TLI: 1, ReplaySeg: 5, Receiving: true, ReceiveSeg: 5},
		},
		{
			name:    "copy still replaying the clone's segment from pg_wal",
			files:   full(s4),
			status:  map[string]bool{},
			w:       walPosition{TLI: 1, ReplaySeg: 4},
			pending: s4,
		},
		{
			name:   "segment being received is left alone",
			files:  full(s5),
			status: map[string]bool{},
			w:      walPosition{TLI: 1, ReplaySeg: 5, Receiving: true, ReceiveSeg: 5},
		},
		{
			name:    "replay lags, the replayed segment was never received",
			files:   full(s5, s6),
			status:  map[string]bool{},
			w:       walPosition{TLI: 1, ReplaySeg: 5, Receiving: true, ReceiveSeg: 6},
			pending: s5,
		},
		{
			name:   "other timeline (a fork's partial segment) is left alone",
			files:  full("000000010000000000000004", "000000020000000000000005"),
			status: map[string]bool{},
			w:      walPosition{TLI: 2, ReplaySeg: 6, Receiving: true, ReceiveSeg: 6},
			adopt:  []string{"000000020000000000000005"},
		},
		{
			name:   "short file is never archived under a full name",
			files:  []walFileInfo{{Name: s4, Size: 8 << 20}},
			status: map[string]bool{},
			w:      walPosition{TLI: 1, ReplaySeg: 5, Receiving: true, ReceiveSeg: 5},
		},
		{
			name:   "history and backup files are not segments",
			files:  []walFileInfo{{Name: "00000002.history", Size: 40}, {Name: s4 + ".00000028.backup", Size: 300}},
			status: map[string]bool{},
			w:      walPosition{TLI: 2, ReplaySeg: 9},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := planUnarchived(c.files, c.status, c.w)
			if !slices.Equal(p.Adopt, c.adopt) || p.Pending != c.pending {
				t.Fatalf("got adopt=%v pending=%q, want adopt=%v pending=%q", p.Adopt, p.Pending, c.adopt, c.pending)
			}
		})
	}
}

func TestParseSegmentName(t *testing.T) {
	tli, seg, ok := parseSegmentName("0000000200000001000000FF")
	if !ok || tli != 2 || seg != 0x1FF {
		t.Fatalf("got %d %x %v", tli, seg, ok)
	}
	for _, bad := range []string{"00000002.history", "000000010000000000000004.partial", "0000000100000000000001000", "00000000000000000000000A", "00000001000000000000010A"} {
		if _, _, ok := parseSegmentName(bad); ok {
			t.Fatalf("%q parsed as a segment", bad)
		}
	}
}
