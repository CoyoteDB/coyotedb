package pg

import "testing"

func mustLSN(t *testing.T, s string) LSN {
	t.Helper()
	l, err := ParseLSN(s)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestParseHistory(t *testing.T) {
	h, err := ParseHistory("1\t0/3000000\tno recovery target specified\r\n\n# comment\n2\t0/300C3D8\tno recovery target specified\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 || h[0].TLI != 1 || h[0].SwitchLSN != mustLSN(t, "0/3000000") || h[1].TLI != 2 || h[1].SwitchLSN != mustLSN(t, "0/300C3D8") {
		t.Fatalf("got %+v", h)
	}
	if _, err := ParseHistory("1\n"); err == nil {
		t.Fatal("short line accepted")
	}
	if _, err := ParseHistory("x\t0/1\n"); err == nil {
		t.Fatal("bad timeline accepted")
	}
}

// The primary in the TestM4PlannedSwitch trace (Windows): node-a back on
// timeline 3; its history left timeline 2 at 0/300C3D8 (from the trace).
// The other LSNs in these tests are made up around that point.
func m4History(t *testing.T) []HistoryEntry {
	return []HistoryEntry{{TLI: 1, SwitchLSN: mustLSN(t, "0/3000000")}, {TLI: 2, SwitchLSN: mustLSN(t, "0/300C3D8")}}
}

func pos(t *testing.T, tli uint32, lsn string) *TimelinePos {
	return &TimelinePos{TLI: tli, LSN: mustLSN(t, lsn)}
}

func TestLocalWALCanFollow(t *testing.T) {
	hist := m4History(t)
	cases := []struct {
		name string
		w    LocalWAL
		want bool
	}{
		// The false "diverged" from the trace: node-b, rewound from
		// node-a, replays timeline 3 from the WAL pg_rewind copied while
		// its restartpoint is still the common checkpoint on timeline 2
		// and its WAL receiver has only received timeline 2 (to the
		// switch point). pg_rewind set the minimum recovery point on
		// timeline 3.
		{"rewound copy, receiver still on the old timeline", LocalWAL{
			Replay: mustLSN(t, "0/3010F28"), Receive: mustLSN(t, "0/300C3D8"),
			Receiver:    pos(t, 2, "0/300C3D8"),
			Checkpoint:  pos(t, 2, "0/3000060"),
			MinRecovery: pos(t, 3, "0/3010F28"),
		}, true},
		{"rewound copy, no receiver yet", LocalWAL{
			Replay:      mustLSN(t, "0/3010F28"),
			Checkpoint:  pos(t, 2, "0/3000060"),
			MinRecovery: pos(t, 3, "0/3010F28"),
		}, true},
		// The race with two reads: a copy that was not rewound crosses
		// from timeline 2 to 3; the receive position (read first) is
		// already on timeline 3, and so is the receiver (read after).
		{"receiver crossed to the new timeline", LocalWAL{
			Replay: mustLSN(t, "0/300C3D8"), Receive: mustLSN(t, "0/3011000"),
			Receiver:    pos(t, 3, "0/3011000"),
			Checkpoint:  pos(t, 2, "0/3000060"),
			MinRecovery: pos(t, 2, "0/300C3D8"),
		}, true},
		{"exactly at the switch point", LocalWAL{
			Replay: mustLSN(t, "0/300C3D8"), Receive: mustLSN(t, "0/300C3D8"),
			Receiver:   pos(t, 2, "0/300C3D8"),
			Checkpoint: pos(t, 2, "0/3000060"),
		}, true},
		{"on the primary's timeline", LocalWAL{
			Replay: mustLSN(t, "0/4000000"), Receive: mustLSN(t, "0/4000000"),
			Receiver: pos(t, 3, "0/4000000"), Checkpoint: pos(t, 3, "0/3F00000"),
		}, true},

		// Real divergence: this copy received more from the old main
		// node (timeline 2) than node-a had when it was promoted.
		{"replayed past the switch point", LocalWAL{
			Replay: mustLSN(t, "0/300D100"), Receive: mustLSN(t, "0/300D100"),
			Receiver:    pos(t, 2, "0/300D100"),
			Checkpoint:  pos(t, 2, "0/3000060"),
			MinRecovery: pos(t, 2, "0/300C800"),
		}, false},
		{"replayed past the switch point, no receiver", LocalWAL{
			Replay: mustLSN(t, "0/300D100"), Receive: mustLSN(t, "0/300D100"),
			Checkpoint: pos(t, 2, "0/3000060"),
		}, false},
		{"received past the switch point, not yet replayed", LocalWAL{
			Replay: mustLSN(t, "0/300C000"), Receive: mustLSN(t, "0/300D100"),
			Checkpoint: pos(t, 2, "0/3000060"),
		}, false},
		{"receiver pair past the switch point", LocalWAL{
			Replay: mustLSN(t, "0/300C000"), Receive: mustLSN(t, "0/300C000"),
			Receiver:   pos(t, 2, "0/300D100"),
			Checkpoint: pos(t, 2, "0/3000060"),
		}, false},
		{"restartpoint past the switch point", LocalWAL{
			Checkpoint: pos(t, 2, "0/300D000"),
		}, false},
		{"older ancestor past its end", LocalWAL{
			Replay:     mustLSN(t, "0/3000100"),
			Checkpoint: pos(t, 1, "0/2000080"),
		}, false},
		{"timeline the primary never had", LocalWAL{
			Replay: mustLSN(t, "0/300C000"), Checkpoint: pos(t, 4, "0/300C000"),
		}, false},
		{"nothing tells the timeline", LocalWAL{Replay: mustLSN(t, "0/300C000")}, false},
	}
	for _, c := range cases {
		if got := c.w.CanFollow(3, hist); got != c.want {
			t.Errorf("%s: CanFollow = %v, want %v", c.name, got, c.want)
		}
	}
}

// What the old check did with the first case above: the timeline from the
// receiver (or restartpoint), the position from the replay. It said
// "diverged". Kept so the failure mode stays documented.
func TestOldCheckMisreadRewoundCopy(t *testing.T) {
	if CanFollow(2, mustLSN(t, "0/3010F28"), 3, m4History(t)) {
		t.Fatal("per-position rule changed: timeline 2 past 0/300C3D8 must not be on timeline 3's history")
	}
}

func TestCanFollowPosition(t *testing.T) {
	hist := m4History(t)
	for _, c := range []struct {
		tli  uint32
		lsn  string
		want bool
	}{
		{3, "0/9000000", true},
		{4, "0/1000000", false},
		{2, "0/300C3D8", true},
		{2, "0/300C3D9", false},
		{1, "0/3000000", true},
		{1, "0/3000001", false},
	} {
		if got := CanFollow(c.tli, mustLSN(t, c.lsn), 3, hist); got != c.want {
			t.Errorf("CanFollow(%d, %s) = %v, want %v", c.tli, c.lsn, got, c.want)
		}
	}
	if !CanFollow(1, mustLSN(t, "0/9000000"), 1, nil) {
		t.Error("same timeline 1 must follow")
	}
}
