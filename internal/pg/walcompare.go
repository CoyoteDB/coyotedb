package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// walCompareWindow is how many bytes of WAL before a copy's replay
// position are compared with the main node's (one WAL page).
const walCompareWindow = 8192

// SameTimelineVerdict is the result of SameTimelineCheck.
type SameTimelineVerdict struct {
	// Known: both sides had the bytes to compare. When false, nothing can
	// be said (the copy is on another timeline than the main node, the
	// main node is not past the copy's position, or a segment is gone).
	Known bool
	// Diverged: the copy's WAL right before its replay position differs
	// from the main node's WAL at the same place on the same timeline.
	Diverged bool
	TLI      uint32
	Replay   LSN
}

// SameTimelineCheck compares the last page of WAL this copy replayed with
// the main node's WAL at the same position, when both are on the same
// timeline (D56). Timeline history (FollowCheck, pg_rewind) cannot see a
// divergence under one timeline ID: two nodes that wrote different WAL as
// primaries on the same timeline (a promotion cut short and then served on
// the old timeline: finding P1). Such a copy never streams again: Postgres
// rejects the main node's records at its position ("incorrect prev-link",
// "invalid record length") and retries forever. Equal bytes say nothing
// more than "not this kind of divergence".
//
// The copy is read locally (its own pg_wal), the main node through up as
// the rewind role (pg_read_binary_file, which that role is granted for
// pg_rewind).
func (n Node) SameTimelineCheck(ctx context.Context, up Upstream, s Secrets) (SameTimelineVerdict, error) {
	var v SameTimelineVerdict
	lc, err := n.Connect(ctx)
	if err != nil {
		return v, err
	}
	defer lc.Close(context.Background())
	rec, err := inRecovery(ctx, lc)
	if err != nil {
		return v, err
	}
	if !rec {
		return v, errors.New("this node is not a copy")
	}
	var replay *string
	if err := lc.QueryRow(ctx, "SELECT pg_last_wal_replay_lsn()::text").Scan(&replay); err != nil {
		return v, err
	}
	if replay == nil {
		return v, nil
	}
	if v.Replay, err = ParseLSN(*replay); err != nil {
		return v, err
	}
	if v.Replay == 0 {
		return v, nil
	}

	uc, err := connectUpstream(ctx, up, RewindUser, s)
	if err != nil {
		return v, err
	}
	defer uc.Close(context.Background())
	var urec bool
	var flush string
	var tli int64
	if err := uc.QueryRow(ctx, `SELECT pg_is_in_recovery(), CASE WHEN pg_is_in_recovery() THEN '0/0' ELSE pg_current_wal_flush_lsn()::text END,
		CASE WHEN pg_is_in_recovery() THEN 0 ELSE ('x' || substr(pg_walfile_name(pg_current_wal_lsn()), 1, 8))::bit(32)::int END`).
		Scan(&urec, &flush, &tli); err != nil {
		return v, err
	}
	if urec {
		return v, fmt.Errorf("upstream %s: %w", up, ErrUpstreamNotPrimary)
	}
	f, err := ParseLSN(flush)
	if err != nil {
		return v, err
	}
	v.TLI = uint32(tli)
	if f <= v.Replay {
		return v, nil // the main node has nothing past the copy yet
	}
	// A copy that knows a newer timeline than the main node is not on the
	// main node's timeline.
	if _, err := os.Stat(filepath.Join(n.PGData(), "pg_wal", fmt.Sprintf("%08X.history", v.TLI+1))); err == nil {
		return v, nil
	}

	last := v.Replay - 1
	segStart := last - last%WALSegmentSize
	from := segStart
	if v.Replay-segStart > walCompareWindow {
		from = v.Replay - walCompareWindow
	}
	length := int(v.Replay - from)
	name := WALFileName(v.TLI, last)

	local, err := readAt(filepath.Join(n.PGData(), "pg_wal", name), int64(from-segStart), length)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	var remote []byte
	if err := uc.QueryRow(ctx, "SELECT pg_read_binary_file($1, $2, $3, true)",
		"pg_wal/"+name, int64(from-segStart), int64(length)).Scan(&remote); err != nil {
		return v, fmt.Errorf("read %s on %s: %w", name, up, err)
	}
	if len(remote) != length {
		return v, nil // gone (recycled) or short: cannot say
	}
	v.Known = true
	v.Diverged = !bytes.Equal(local, remote)
	return v, nil
}

func readAt(path string, off int64, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil {
		return nil, err
	}
	return b, nil
}
