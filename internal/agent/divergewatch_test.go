package agent

import (
	"testing"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// The watch re-clones only after divergeStrikes differing results in a
// row; an unknown or equal result starts the count again.
func TestDivergeWatchStrikes(t *testing.T) {
	var w divergeWatch
	diff := pg.SameTimelineVerdict{Known: true, Diverged: true}
	same := pg.SameTimelineVerdict{Known: true}
	unknown := pg.SameTimelineVerdict{}
	for i := 1; i < divergeStrikes; i++ {
		if w.note(diff) {
			t.Fatalf("re-clone after %d strikes, want %d", i, divergeStrikes)
		}
	}
	if w.note(same) || w.note(unknown) {
		t.Fatal("re-clone on an equal or unknown result")
	}
	for i := 1; i < divergeStrikes; i++ {
		if w.note(diff) {
			t.Fatalf("count not restarted: re-clone after %d strikes", i)
		}
	}
	if !w.note(diff) {
		t.Fatalf("no re-clone after %d strikes in a row", divergeStrikes)
	}
}

func TestDivergeWatchDue(t *testing.T) {
	var w divergeWatch
	t0 := time.Now()
	if !w.due(t0) {
		t.Fatal("first check not due")
	}
	if w.due(t0.Add(divergeCheckEvery - time.Millisecond)) {
		t.Fatal("check due before the interval")
	}
	if !w.due(t0.Add(divergeCheckEvery)) {
		t.Fatal("check not due after the interval")
	}
}
