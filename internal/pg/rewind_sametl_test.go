package pg

import (
	"errors"
	"testing"
)

// pg_rewind says "no rewind required" on the same timeline; a target that
// wrote WAL as a primary on the source's timeline must be refused (D56).
func TestSameTimelineCheck(t *testing.T) {
	cases := []struct {
		state   string
		tli     uint32
		src     uint32
		refused bool
	}{
		{"shut down", 1, 1, true},              // P1: old main vs a copy that served on its timeline
		{"shut down", 1, 2, false},             // the normal case: the source was promoted
		{"shut down", 3, 2, false},             // pg_rewind handles (and refuses) this itself
		{"shut down in recovery", 1, 1, false}, // a cleanly stopped copy wrote nothing of its own
		{"shut down in recovery", 2, 3, false},
	}
	for _, c := range cases {
		err := sameTimelineCheck(ControlInfo{State: c.state, Timeline: c.tli}, c.src)
		if got := errors.Is(err, ErrSameTimeline); got != c.refused {
			t.Errorf("%q on timeline %d, source on %d: refused=%v, want %v (%v)", c.state, c.tli, c.src, got, c.refused, err)
		}
	}
}
