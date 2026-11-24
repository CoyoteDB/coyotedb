package pg

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// HistoryEntry is one line of a timeline history file: timeline TLI ended
// (and its child began) at SwitchLSN.
type HistoryEntry struct {
	TLI       uint32
	SwitchLSN LSN
}

// ParseHistory parses a timeline history file ("<parent tli>\t<switch
// lsn>\t<reason>" per line, comments with #).
func ParseHistory(s string) ([]HistoryEntry, error) {
	var out []HistoryEntry
	for i, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return nil, fmt.Errorf("timeline history line %d not understood", i+1)
		}
		tli, err := strconv.ParseUint(f[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("timeline history line %d: %w", i+1, err)
		}
		lsn, err := ParseLSN(f[1])
		if err != nil {
			return nil, fmt.Errorf("timeline history line %d: %w", i+1, err)
		}
		out = append(out, HistoryEntry{TLI: uint32(tli), SwitchLSN: lsn})
	}
	return out, nil
}

// CanFollow reports whether one position, LSN localEnd on timeline
// localTLI, is on the history of a primary on primaryTLI whose history is
// hist (the primary's history file; empty when primaryTLI is 1). It is if
// it is on the primary's timeline, or on an ancestor of it and not past
// the point where the primary's history left that timeline. Otherwise
// that WAL is WAL the new primary never had.
func CanFollow(localTLI uint32, localEnd LSN, primaryTLI uint32, hist []HistoryEntry) bool {
	if localTLI == primaryTLI {
		return true
	}
	if localTLI > primaryTLI {
		return false
	}
	for _, h := range hist {
		if h.TLI == localTLI {
			return localEnd <= h.SwitchLSN
		}
	}
	return false
}

// TimelinePos is an LSN together with the timeline it is on, both taken
// from one read inside Postgres.
type TimelinePos struct {
	TLI uint32
	LSN LSN
}

// LocalWAL is what a running standby holds, read by readLocalWAL in this
// order: first the two positions Postgres reports without a timeline,
// then the (timeline, LSN) pairs. Every one of them only moves forward
// (a new WAL receiver stream restarts its LSN, but on a later timeline).
type LocalWAL struct {
	Replay  LSN // pg_last_wal_replay_lsn(); 0 when none
	Receive LSN // pg_last_wal_receive_lsn(); 0 when none

	Receiver    *TimelinePos // pg_stat_wal_receiver: received_tli, flushed_lsn; nil when no receiver
	Checkpoint  *TimelinePos // last restartpoint (pg_control_checkpoint)
	MinRecovery *TimelinePos // pg_control_recovery minimum recovery point; nil when unset
}

func (w LocalWAL) pairs() []TimelinePos {
	var out []TimelinePos
	for _, p := range []*TimelinePos{w.Receiver, w.Checkpoint, w.MinRecovery} {
		if p != nil && p.TLI != 0 {
			out = append(out, *p)
		}
	}
	return out
}

// CanFollow reports whether everything this standby holds is on the
// history of a primary on primaryTLI with history hist; if not, it holds
// WAL the new primary never had and must be rewound (safety rule 6
// applies to such a standby too).
//
// Every (timeline, LSN) pair must be on the primary's history. The replay
// and receive positions come without a timeline; each is checked on the
// newest timeline among the pairs, which were read after it:
//
//   - A standby that holds WAL past the point where the primary's history
//     left its timeline T stays on T: Postgres never moves it to a
//     timeline that forked off before its recovery point, and none of the
//     pairs read later can show a timeline past T. So the position is
//     checked against T or older, whose end is before it: "diverged".
//   - Otherwise the newest pair timeline N is on the standby's own
//     history, which is the primary's up to N, and an earlier-read
//     position is on that history at or before N: "can follow".
//
// The old check read the timeline first and the position second; right
// after a rewind the standby replays onto the primary's timeline from the
// WAL pg_rewind copied while its restartpoint (and WAL receiver) still
// show the old one, so a correct copy was called diverged.
func (w LocalWAL) CanFollow(primaryTLI uint32, hist []HistoryEntry) bool {
	pairs := w.pairs()
	if len(pairs) == 0 {
		return false // nothing tells which timeline this copy is on
	}
	var newest uint32
	for _, p := range pairs {
		if !CanFollow(p.TLI, p.LSN, primaryTLI, hist) {
			return false
		}
		newest = max(newest, p.TLI)
	}
	for _, l := range []LSN{w.Replay, w.Receive} {
		if l != 0 && !CanFollow(newest, l, primaryTLI, hist) {
			return false
		}
	}
	return true
}

// readLocalWAL reads what the standby on c holds, in the order
// LocalWAL.CanFollow relies on: positions first, then the pairs.
func readLocalWAL(ctx context.Context, c *pgx.Conn) (LocalWAL, error) {
	var w LocalWAL
	var replay, recv *string
	if err := c.QueryRow(ctx, "SELECT pg_last_wal_replay_lsn()::text, pg_last_wal_receive_lsn()::text").Scan(&replay, &recv); err != nil {
		return w, err
	}
	for _, v := range []struct {
		s   *string
		dst *LSN
	}{{replay, &w.Replay}, {recv, &w.Receive}} {
		if v.s != nil {
			l, err := ParseLSN(*v.s)
			if err != nil {
				return w, err
			}
			*v.dst = l
		}
	}
	// received_tli and flushed_lsn are copied under one spinlock, and the
	// WAL receiver updates them together.
	var rtli *int64
	var rflushed *string
	err := c.QueryRow(ctx, "SELECT received_tli::bigint, flushed_lsn::text FROM pg_stat_wal_receiver").Scan(&rtli, &rflushed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return w, err
	default:
		if w.Receiver, err = timelinePos(rtli, rflushed); err != nil {
			return w, err
		}
	}
	// Each function reads pg_control once, so each pair is consistent.
	var ctli, mtli *int64
	var clsn, mlsn *string
	if err := c.QueryRow(ctx, `SELECT c.timeline_id::bigint, c.checkpoint_lsn::text,
		r.min_recovery_end_timeline::bigint, r.min_recovery_end_lsn::text
		FROM pg_control_checkpoint() c, pg_control_recovery() r`).Scan(&ctli, &clsn, &mtli, &mlsn); err != nil {
		return w, err
	}
	if w.Checkpoint, err = timelinePos(ctli, clsn); err != nil {
		return w, err
	}
	if w.MinRecovery, err = timelinePos(mtli, mlsn); err != nil {
		return w, err
	}
	if w.MinRecovery != nil && w.MinRecovery.LSN == 0 {
		w.MinRecovery = nil
	}
	return w, nil
}

func timelinePos(tli *int64, lsn *string) (*TimelinePos, error) {
	if tli == nil || lsn == nil || *tli <= 0 {
		return nil, nil
	}
	l, err := ParseLSN(*lsn)
	if err != nil {
		return nil, err
	}
	return &TimelinePos{TLI: uint32(*tli), LSN: l}, nil
}

// FollowCheck connects to the primary up (as the rewind role, which may
// read files) and reports whether this running standby can follow it
// without a rewind (see LocalWAL.CanFollow).
func (n Node) FollowCheck(ctx context.Context, up Upstream, s Secrets) (bool, error) {
	c, err := n.Connect(ctx)
	if err != nil {
		return false, err
	}
	defer c.Close(context.Background())
	rec, err := inRecovery(ctx, c)
	if err != nil {
		return false, err
	}
	if !rec {
		return false, errors.New("this node is not a standby")
	}
	local, err := readLocalWAL(ctx, c)
	if err != nil {
		return false, err
	}

	pc, err := connectUpstream(ctx, up, RewindUser, s)
	if err != nil {
		return false, err
	}
	defer pc.Close(context.Background())
	if r, err := inRecovery(ctx, pc); err != nil {
		return false, err
	} else if r {
		return false, fmt.Errorf("%s: %w", up, ErrUpstreamNotPrimary)
	}
	primaryTLI, err := currentTimeline(ctx, pc, false)
	if err != nil {
		return false, err
	}
	var hist []HistoryEntry
	if primaryTLI > 1 {
		var b []byte
		name := fmt.Sprintf("pg_wal/%08X.history", primaryTLI)
		if err := pc.QueryRow(ctx, "SELECT pg_read_binary_file($1)", name).Scan(&b); err != nil {
			return false, fmt.Errorf("read %s on the primary: %w", name, err)
		}
		if hist, err = ParseHistory(string(b)); err != nil {
			return false, err
		}
	}
	return local.CanFollow(primaryTLI, hist), nil
}
