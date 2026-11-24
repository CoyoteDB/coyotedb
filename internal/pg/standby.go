package pg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// standbySignal is the file that makes Postgres start as a standby.
const standbySignal = "standby.signal"

// IsStandbyDir reports whether pgdata is set up to start as a standby.
func (n Node) IsStandbyDir() bool {
	_, err := os.Stat(filepath.Join(n.PGData(), standbySignal))
	return err == nil
}

// configFor renders the full agent-owned config for this node.
func (n Node) configFor(port int, name string, st ReplState) (Config, error) {
	c := Config{Port: port, LogDir: n.LogDir(), PrimarySlot: st.Slot}
	arch, err := n.ReadArchive()
	if err != nil {
		return c, err
	}
	c.Archive = arch
	if cert, key := n.TLSFiles(); fileExists(cert) && fileExists(key) {
		c.TLSCert, c.TLSKey = cert, key
	}
	if st.Upstream != nil {
		if err := st.Upstream.Valid(); err != nil {
			return c, err
		}
		c.PrimaryConninfo = Conninfo(ConnParams{
			Upstream: *st.Upstream,
			User:     ReplUser,
			AppName:  name,
			Passfile: n.PassfilePath(),
		})
	}
	sync, err := RenderSyncNames(st.SyncCount, st.SyncNames)
	if err != nil {
		return c, err
	}
	c.SyncStandbyNames = sync
	return c, nil
}

// writeConfigLocked renders coyote.conf and pg_hba.conf from
// replication.json under the config lock (used by Start).
func (n Node) writeConfigLocked(ctx context.Context, port int, name string) error {
	l, err := n.LockConfig(ctx)
	if err != nil {
		return err
	}
	defer l.Unlock()
	st, err := n.ReadReplState()
	if err != nil {
		return err
	}
	if st.Upstream != nil {
		// Keep the passfile in step with the secrets on every start.
		s, err := n.Secrets()
		if err != nil {
			return fmt.Errorf("standby needs its secrets: %w", err)
		}
		if err := n.writePassfile(*st.Upstream, s); err != nil {
			return err
		}
	}
	c, err := n.configFor(port, name, st)
	if err != nil {
		return err
	}
	return WriteConfig(n.PGData(), c)
}

// writeConfigFiles renders config for a data dir that is not running
// (after clone or rewind), from node.json and replication.json.
func (n Node) writeConfigFiles(st ReplState) error {
	info, err := n.ReadInfo()
	if err != nil {
		return err
	}
	c, err := n.configFor(info.Port, info.Name, st)
	if err != nil {
		return err
	}
	return WriteConfig(n.PGData(), c)
}

// changeRepl applies f to replication.json, re-renders coyote.conf (and the
// passfile) and reloads the running Postgres, all under the config lock so
// it cannot race the agent's start or another one-shot command. The caller
// confirms the effect with a real check (wal receiver, sync_state).
func (n Node) changeRepl(ctx context.Context, c *pgx.Conn, f func(*ReplState) error) (Config, error) {
	info, err := n.ReadInfo()
	if err != nil {
		return Config{}, err
	}
	l, err := n.LockConfig(ctx)
	if err != nil {
		return Config{}, err
	}
	defer l.Unlock()
	st, err := n.ReadReplState()
	if err != nil {
		return Config{}, err
	}
	if err := f(&st); err != nil {
		return Config{}, err
	}
	if st.Upstream != nil {
		s, err := n.Secrets()
		if err != nil {
			return Config{}, err
		}
		if err := n.writePassfile(*st.Upstream, s); err != nil {
			return Config{}, err
		}
	}
	cfg, err := n.configFor(info.Port, info.Name, st)
	if err != nil {
		return Config{}, err
	}
	if err := n.WriteReplState(st); err != nil {
		return Config{}, err
	}
	if err := WriteConfig(n.PGData(), cfg); err != nil {
		return Config{}, err
	}
	if _, err := c.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		return Config{}, fmt.Errorf("reload: %w", err)
	}
	var bad []string
	rows, err := c.Query(ctx, "SELECT coalesce(name, '') || ': ' || error FROM pg_file_settings WHERE error IS NOT NULL")
	if err != nil {
		return Config{}, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return Config{}, err
		}
		bad = append(bad, s)
	}
	rows.Close()
	if len(bad) > 0 {
		return Config{}, fmt.Errorf("Postgres rejected the new config: %s", strings.Join(bad, "; "))
	}
	return cfg, nil
}

// waitSetting polls until this session sees setting = want (a reload
// reaches each backend asynchronously).
func waitSetting(ctx context.Context, c *pgx.Conn, setting, want string, timeout time.Duration) error {
	return poll(ctx, timeout, 20*time.Millisecond, "wait for "+setting, func(ctx context.Context) (bool, error) {
		var got string
		if err := c.QueryRow(ctx, "SELECT current_setting($1)", setting).Scan(&got); err != nil {
			return false, err
		}
		return got == want, nil
	})
}

func inRecovery(ctx context.Context, c *pgx.Conn) (bool, error) {
	var r bool
	err := c.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&r)
	return r, err
}

func walReceiverCount(ctx context.Context, c *pgx.Conn) (int, error) {
	var k int
	err := c.QueryRow(ctx, "SELECT count(*) FROM pg_stat_wal_receiver").Scan(&k)
	return k, err
}

// CutStream is safety rule 2's primitive: clear primary_conninfo, reload,
// and wait until pg_stat_wal_receiver is empty, so this standby can no
// longer confirm writes for the old primary. The cut is persisted in
// replication.json, so it survives a restart. It returns the flushed
// receive LSN (pg_last_wal_receive_lsn), the value to compare between
// candidates. On a primary there is nothing to cut.
func (n Node) CutStream(ctx context.Context, timeout time.Duration) (LSN, error) {
	c, err := n.Connect(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close(context.Background())
	rec, err := inRecovery(ctx, c)
	if err != nil {
		return 0, err
	}
	if _, err := n.changeRepl(ctx, c, func(st *ReplState) error { st.Upstream = nil; return nil }); err != nil {
		return 0, err
	}
	if !rec {
		return 0, nil
	}
	if err := poll(ctx, timeout, 20*time.Millisecond, "wait for the WAL receiver to stop", func(ctx context.Context) (bool, error) {
		k, err := walReceiverCount(ctx, c)
		return k == 0, err
	}); err != nil {
		return 0, err
	}
	return localWALEnd(ctx, c, timeout)
}

// localWALEnd is the end of the WAL this cut standby holds durably: the
// value rule 2 compares between candidates. pg_last_wal_receive_lsn() is
// that end when a WAL receiver ran since Postgres started (it stays at the
// last flushed position after the receiver stops). If none ran (the node
// restarted while the primary was already gone), it is NULL; the WAL on
// disk is then replayed from pg_wal first, so once the startup process is
// waiting for more WAL, pg_last_wal_replay_lsn() is the end. An unknown
// end is an error, never zero: a candidate must not look emptier than it
// is.
func localWALEnd(ctx context.Context, c *pgx.Conn, timeout time.Duration) (LSN, error) {
	var end LSN
	err := poll(ctx, timeout, 20*time.Millisecond, "wait for the local WAL to be replayed", func(ctx context.Context) (bool, error) {
		var recv, replay, wait *string
		err := c.QueryRow(ctx, `SELECT pg_last_wal_receive_lsn()::text, pg_last_wal_replay_lsn()::text,
			(SELECT wait_event FROM pg_stat_activity WHERE backend_type = 'startup')`).Scan(&recv, &replay, &wait)
		if err != nil {
			return false, err
		}
		var r, p LSN
		if replay != nil {
			if p, err = ParseLSN(*replay); err != nil {
				return false, err
			}
		}
		if recv != nil {
			if r, err = ParseLSN(*recv); err != nil {
				return false, err
			}
			end = max(r, p)
			return true, nil
		}
		if replay != nil && wait != nil && (*wait == "RecoveryRetrieveRetryInterval" || *wait == "RecoveryWalStream") {
			end = p
			return true, nil
		}
		return false, nil
	})
	return end, err
}

// LocalWALEnd opens a connection and returns localWALEnd (for a standby).
func (n Node) LocalWALEnd(ctx context.Context, timeout time.Duration) (LSN, error) {
	c, err := n.Connect(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close(context.Background())
	return localWALEnd(ctx, c, timeout)
}

// SyncBarrier runs CHECKPOINT on the primary. The checkpointer applies a
// reloaded synchronous_standby_names to the shared state every committing
// backend reads, and it processes a pending reload before it performs a
// requested checkpoint; so once this returns, every commit that starts
// afterwards waits for the new list. (The walsender's sync_state, which
// SetSync confirms, is updated by a different process.)
func (n Node) SyncBarrier(ctx context.Context) error {
	c, err := n.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	_, err = c.Exec(ctx, "CHECKPOINT")
	return err
}

// FlushLSN is pg_current_wal_flush_lsn() on a primary: every commit that
// was acknowledged so far is at or below it.
func (n Node) FlushLSN(ctx context.Context) (LSN, error) {
	c, err := n.Connect(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close(context.Background())
	var s string
	if err := c.QueryRow(ctx, "SELECT pg_current_wal_flush_lsn()::text").Scan(&s); err != nil {
		return 0, err
	}
	return ParseLSN(s)
}

// SetUpstream points a running standby at up: makes sure its slot exists
// there, rewrites the passfile and primary_conninfo, reloads (both
// settings are reloadable in PostgreSQL 13+, so no restart), and waits
// until the WAL receiver streams from up. A primary is refused: it must be
// rewound first (safety rule 6).
func (n Node) SetUpstream(ctx context.Context, up Upstream, timeout time.Duration) error {
	if err := up.Valid(); err != nil {
		return err
	}
	info, err := n.ReadInfo()
	if err != nil {
		return err
	}
	s, err := n.Secrets()
	if err != nil {
		return err
	}
	c, err := n.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	rec, err := inRecovery(ctx, c)
	if err != nil {
		return err
	}
	if !rec {
		return errors.New("this node is a primary; it cannot follow another node until it is rewound (dev rewind)")
	}
	slot := SlotName(info.Name)
	if err := ensureUpstreamSlot(ctx, up, s, slot); err != nil {
		return err
	}
	if _, err := n.changeRepl(ctx, c, func(st *ReplState) error {
		st.Upstream = &up
		st.Slot = slot
		return nil
	}); err != nil {
		return err
	}
	return poll(ctx, timeout, 50*time.Millisecond, "wait for streaming from "+up.String(), func(ctx context.Context) (bool, error) {
		var k int
		err := c.QueryRow(ctx, `SELECT count(*) FROM pg_stat_wal_receiver
			WHERE status = 'streaming' AND sender_host = $1 AND sender_port = $2`, up.Host, up.Port).Scan(&k)
		return k == 1, err
	})
}

// ensureUpstreamSlot creates this node's slot on the upstream (as the
// replication role, which may manage slots).
// ErrUpstreamNotPrimary: the node named primary is still a standby (it has
// not finished promoting). Retrying soon is safe and expected.
var ErrUpstreamNotPrimary = errors.New("the upstream is not a primary yet")

func ensureUpstreamSlot(ctx context.Context, up Upstream, s Secrets, slot string) error {
	uc, err := connectUpstream(ctx, up, ReplUser, s)
	if err != nil {
		return err
	}
	defer uc.Close(context.Background())
	if r, err := inRecovery(ctx, uc); err != nil {
		return err
	} else if r {
		return fmt.Errorf("upstream %s is a standby; cascading is not supported: %w", up, ErrUpstreamNotPrimary)
	}
	_, err = CreateSlot(ctx, uc, slot)
	return err
}

// UpstreamIsPrimary connects to up as the rewind role and reports whether
// it is a primary (not in recovery). Callers use it before a rewind so a
// source that is not promoted yet is waited for, instead of being taken as
// "rewind impossible" (which would re-clone).
func UpstreamIsPrimary(ctx context.Context, up Upstream, s Secrets) (bool, error) {
	c, err := connectUpstream(ctx, up, RewindUser, s)
	if err != nil {
		return false, err
	}
	defer c.Close(context.Background())
	rec, err := inRecovery(ctx, c)
	return err == nil && !rec, err
}

// SetSync sets synchronous_standby_names to ANY count (names...) or clears
// it, reloads, and confirms the change took effect: every connected
// standby that is listed reports a sync state other than async, and every
// connected standby that is not listed reports async. A listed standby
// that is not connected cannot be confirmed, which is fine: commits wait
// for it until it connects.
func (n Node) SetSync(ctx context.Context, count int, names []string, timeout time.Duration) error {
	want, err := RenderSyncNames(count, names)
	if err != nil {
		return err
	}
	c, err := n.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	if _, err := n.changeRepl(ctx, c, func(st *ReplState) error {
		st.SyncCount, st.SyncNames = count, slices.Clone(names)
		return nil
	}); err != nil {
		return err
	}
	if err := waitSetting(ctx, c, "synchronous_standby_names", want, timeout); err != nil {
		return err
	}
	rec, err := inRecovery(ctx, c)
	if err != nil || rec {
		return err // on a standby it applies once promoted
	}
	listed := map[string]bool{}
	for _, nm := range names {
		listed[strings.ToLower(nm)] = true
	}
	return poll(ctx, timeout, 20*time.Millisecond, "confirm sync_state", func(ctx context.Context) (bool, error) {
		rows, err := c.Query(ctx, "SELECT application_name, sync_state FROM pg_stat_replication")
		if err != nil {
			return false, err
		}
		defer rows.Close()
		ok := true
		for rows.Next() {
			var app, state string
			if err := rows.Scan(&app, &state); err != nil {
				return false, err
			}
			if listed[strings.ToLower(app)] == (state == "async") {
				ok = false
			}
		}
		return ok, rows.Err()
	})
}

// PromoteOptions controls Promote.
type PromoteOptions struct {
	// Members are the other nodes' names; a slot is made for each right
	// after promotion (safety rule 7). The node's own name is skipped.
	Members []string
	Timeout time.Duration // default 60s
}

// Promote turns a standby whose stream was already cut (CutStream, rule 2)
// into the primary: pg_promote, wait until out of recovery, CHECKPOINT (so
// the control file shows the new timeline, which pg_rewind relies on),
// then create slots for the other members. It returns the new timeline.
func (n Node) Promote(ctx context.Context, o PromoteOptions) (uint32, error) {
	if o.Timeout == 0 {
		o.Timeout = 60 * time.Second
	}
	info, err := n.ReadInfo()
	if err != nil {
		return 0, err
	}
	st, err := n.ReadReplState()
	if err != nil {
		return 0, err
	}
	c, err := n.Connect(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close(context.Background())
	rec, err := inRecovery(ctx, c)
	if err != nil {
		return 0, err
	}
	if !rec {
		return 0, ErrAlreadyPrimary
	}
	if k, err := walReceiverCount(ctx, c); err != nil {
		return 0, err
	} else if st.Upstream != nil || k > 0 {
		return 0, errors.New("the stream from the old primary is not cut; cut it first (safety rule 2)")
	}
	var ok bool
	if err := c.QueryRow(ctx, "SELECT pg_promote(true, $1)", int(o.Timeout.Seconds())).Scan(&ok); err != nil {
		return 0, fmt.Errorf("pg_promote: %w", err)
	}
	if err := poll(ctx, o.Timeout, 20*time.Millisecond, "wait for promotion", func(ctx context.Context) (bool, error) {
		r, err := inRecovery(ctx, c)
		return !r, err
	}); err != nil {
		return 0, err
	}
	return n.finishPromote(ctx, c, info, o)
}

// ErrAlreadyPrimary: Promote found Postgres already out of recovery.
var ErrAlreadyPrimary = errors.New("this node is already a primary")

// ResumeStandby puts standby.signal back on a stopped data dir whose
// promotion did not finish (D56), so the next start is a standby again and
// the promotion can be done from the start.
//
// Why it is needed: during pg_promote, Postgres removes standby.signal
// before it writes the new timeline's history file and its end-of-recovery
// record. Stopped in between (power cut, kill, lease fence), the data dir
// looks like a crashed primary whose control file is still on the old
// timeline; started as it is, crash recovery would go on along the old
// main node's timeline and serve writes there, under the same timeline ID
// as the old main node's own WAL past the fork point (finding P1).
//
// Started as a standby instead (no upstream: the stream stays cut), it
// replays every byte of WAL it holds, along the newest timeline it knows
// (recovery_target_timeline=latest), and a new promotion then picks a
// timeline ID above every history file on disk. Nothing is dropped: WAL
// written after a promotion that did complete is replayed too, and the new
// timeline forks after it.
func (n Node) ResumeStandby() error {
	if !n.Initialized() {
		return errors.New("there is no data to start as a standby")
	}
	if st, err := n.Status(); err != nil {
		return err
	} else if st.Running {
		return fmt.Errorf("Postgres is running on %s (pid %d)", n.PGData(), st.PID)
	}
	if err := os.Remove(filepath.Join(n.PGData(), "recovery.signal")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return writeFileAtomic(filepath.Join(n.PGData(), standbySignal), nil, 0o600)
}

// FinishPromote does the steps after pg_promote (CHECKPOINT, slots for
// the other members, clear the standby's slot and the rejoin marker) on a
// Postgres that is already out of recovery. It is for one caller only: an
// agent whose own Promote in this same term got past pg_promote and then
// failed. Each step is safe to repeat.
func (n Node) FinishPromote(ctx context.Context, o PromoteOptions) (uint32, error) {
	info, err := n.ReadInfo()
	if err != nil {
		return 0, err
	}
	c, err := n.Connect(ctx)
	if err != nil {
		return 0, err
	}
	defer c.Close(context.Background())
	rec, err := inRecovery(ctx, c)
	if err != nil {
		return 0, err
	}
	if rec {
		return 0, errors.New("finish promote: this node is still a standby")
	}
	return n.finishPromote(ctx, c, info, o)
}

func (n Node) finishPromote(ctx context.Context, c *pgx.Conn, info NodeInfo, o PromoteOptions) (uint32, error) {
	if _, err := c.Exec(ctx, "CHECKPOINT"); err != nil {
		return 0, fmt.Errorf("checkpoint after promote: %w", err)
	}
	for _, m := range o.Members {
		if m == info.Name {
			continue
		}
		if err := ValidName(m); err != nil {
			return 0, err
		}
		if _, err := CreateSlot(ctx, c, SlotName(m)); err != nil {
			return 0, err
		}
	}
	if _, err := n.changeRepl(ctx, c, func(st *ReplState) error { st.Slot = ""; return nil }); err != nil {
		return 0, err
	}
	if err := n.ClearMarker(); err != nil {
		return 0, err
	}
	return currentTimeline(ctx, c, false)
}

// currentTimeline: on a primary, the timeline of the current WAL file; on
// a standby, the timeline its WAL receiver is on, else the latest
// checkpoint/restartpoint's.
func currentTimeline(ctx context.Context, c *pgx.Conn, standby bool) (uint32, error) {
	var tli int64
	if !standby {
		err := c.QueryRow(ctx, "SELECT ('x' || substr(pg_walfile_name(pg_current_wal_lsn()), 1, 8))::bit(32)::int").Scan(&tli)
		return uint32(tli), err
	}
	err := c.QueryRow(ctx, `SELECT coalesce((SELECT received_tli FROM pg_stat_wal_receiver),
		(SELECT timeline_id FROM pg_control_checkpoint()))`).Scan(&tli)
	return uint32(tli), err
}

// ---- status ----

// WalReceiver is pg_stat_wal_receiver.
type WalReceiver struct {
	Status      string `json:"status"`
	SenderHost  string `json:"sender_host"`
	SenderPort  int    `json:"sender_port"`
	Slot        string `json:"slot"`
	ReceivedTLI uint32 `json:"received_tli"`
	FlushedLSN  *LSN   `json:"flushed_lsn,omitempty"`
}

// StandbyRow is one row of pg_stat_replication (plus the slot it uses).
type StandbyRow struct {
	Name      string `json:"name"` // application_name
	State     string `json:"state"`
	SyncState string `json:"sync_state"`
	Slot      string `json:"slot,omitempty"`
	SentLSN   *LSN   `json:"sent_lsn,omitempty"`
	WriteLSN  *LSN   `json:"write_lsn,omitempty"`
	FlushLSN  *LSN   `json:"flush_lsn,omitempty"`
	ReplayLSN *LSN   `json:"replay_lsn,omitempty"`
}

// ReplStatus is what a node reports about its replication.
type ReplStatus struct {
	Role     string `json:"role"` // primary or standby
	Timeline uint32 `json:"timeline"`
	// Primary only.
	FlushLSN *LSN `json:"flush_lsn,omitempty"` // pg_current_wal_flush_lsn()
	// Standby only. ReceiveLSN is pg_last_wal_receive_lsn(): flushed to
	// disk on this standby, the value to compare between candidates (rule 2).
	ReceiveLSN  *LSN         `json:"receive_lsn,omitempty"`
	ReplayLSN   *LSN         `json:"replay_lsn,omitempty"`
	WalReceiver *WalReceiver `json:"wal_receiver,omitempty"`

	SyncStandbyNames string       `json:"synchronous_standby_names"`
	Standbys         []StandbyRow `json:"standbys"`
	Slots            []Slot       `json:"slots"`
}

func lsnPtr(s *string) (*LSN, error) {
	if s == nil {
		return nil, nil
	}
	l, err := ParseLSN(*s)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// ReplicationStatus queries the running Postgres.
func ReplicationStatus(ctx context.Context, c *pgx.Conn) (ReplStatus, error) {
	var st ReplStatus
	rec, err := inRecovery(ctx, c)
	if err != nil {
		return st, err
	}
	st.Role = "primary"
	if rec {
		st.Role = "standby"
	}
	if st.Timeline, err = currentTimeline(ctx, c, rec); err != nil {
		return st, err
	}
	if err := c.QueryRow(ctx, "SHOW synchronous_standby_names").Scan(&st.SyncStandbyNames); err != nil {
		return st, err
	}
	if rec {
		var recv, replay *string
		if err := c.QueryRow(ctx, "SELECT pg_last_wal_receive_lsn()::text, pg_last_wal_replay_lsn()::text").Scan(&recv, &replay); err != nil {
			return st, err
		}
		if st.ReceiveLSN, err = lsnPtr(recv); err != nil {
			return st, err
		}
		if st.ReplayLSN, err = lsnPtr(replay); err != nil {
			return st, err
		}
		var w WalReceiver
		var flushed *string
		var tli int64
		err := c.QueryRow(ctx, `SELECT status, coalesce(sender_host, ''), coalesce(sender_port, 0),
			coalesce(slot_name, ''), coalesce(received_tli, 0), flushed_lsn::text FROM pg_stat_wal_receiver`).
			Scan(&w.Status, &w.SenderHost, &w.SenderPort, &w.Slot, &tli, &flushed)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return st, err
		default:
			w.ReceivedTLI = uint32(tli)
			if w.FlushedLSN, err = lsnPtr(flushed); err != nil {
				return st, err
			}
			st.WalReceiver = &w
		}
	} else {
		var f string
		if err := c.QueryRow(ctx, "SELECT pg_current_wal_flush_lsn()::text").Scan(&f); err != nil {
			return st, err
		}
		l, err := ParseLSN(f)
		if err != nil {
			return st, err
		}
		st.FlushLSN = &l
	}
	rows, err := c.Query(ctx, `SELECT coalesce(r.application_name, ''), coalesce(r.state, ''), coalesce(r.sync_state, ''),
		coalesce(s.slot_name::text, ''), r.sent_lsn::text, r.write_lsn::text, r.flush_lsn::text, r.replay_lsn::text
		FROM pg_stat_replication r LEFT JOIN pg_replication_slots s ON s.active_pid = r.pid
		ORDER BY 1`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var r StandbyRow
		var sent, write, flush, replay *string
		if err := rows.Scan(&r.Name, &r.State, &r.SyncState, &r.Slot, &sent, &write, &flush, &replay); err != nil {
			rows.Close()
			return st, err
		}
		for _, p := range []struct {
			src *string
			dst **LSN
		}{{sent, &r.SentLSN}, {write, &r.WriteLSN}, {flush, &r.FlushLSN}, {replay, &r.ReplayLSN}} {
			if *p.dst, err = lsnPtr(p.src); err != nil {
				rows.Close()
				return st, err
			}
		}
		st.Standbys = append(st.Standbys, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}
	if st.Slots, err = ListSlots(ctx, c); err != nil {
		return st, err
	}
	if st.Standbys == nil {
		st.Standbys = []StandbyRow{}
	}
	if st.Slots == nil {
		st.Slots = []Slot{}
	}
	return st, nil
}

// removeSlotsAt deletes the slot folders in a stopped data dir's
// pg_replslot. pg_rewind leaves the target's own slots in place (it skips
// that folder), so a rewound former primary would otherwise keep holding
// WAL for slots that mean nothing on a standby.
func removeSlotsAt(dir string) error {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return syncDir(dir)
}
