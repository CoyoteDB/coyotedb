package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ControlInfo is the part of pg_controldata output we use.
type ControlInfo struct {
	State         string // "shut down", "shut down in recovery", "in production", ...
	Timeline      uint32 // latest checkpoint's TimeLineID
	CheckpointLSN LSN    // latest checkpoint location
	SystemID      string
}

// CleanlyShutDown reports whether pg_rewind may use this data dir as is.
func (c ControlInfo) CleanlyShutDown() bool {
	return c.State == "shut down" || c.State == "shut down in recovery"
}

// ParseControlData parses pg_controldata output (run with LC_ALL=C).
func ParseControlData(out string) (ControlInfo, error) {
	var c ControlInfo
	seen := 0
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "Database cluster state":
			c.State = v
			seen++
		case "Latest checkpoint's TimeLineID":
			t, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return c, fmt.Errorf("pg_controldata: bad timeline %q", v)
			}
			c.Timeline = uint32(t)
			seen++
		case "Latest checkpoint location":
			l, err := ParseLSN(v)
			if err != nil {
				return c, fmt.Errorf("pg_controldata: %w", err)
			}
			c.CheckpointLSN = l
			seen++
		case "Database system identifier":
			c.SystemID = v
		}
	}
	if seen != 3 {
		return c, errors.New("pg_controldata output not understood")
	}
	return c, nil
}

// ControlData runs pg_controldata on this node's pgdata (read-only).
func (n Node) ControlData(ctx context.Context, inst Install) (ControlInfo, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, inst.Bin("pg_controldata"), "-D", n.PGData())
	cmd.Env = toolEnv()
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return ControlInfo{}, fmt.Errorf("pg_controldata: %w: %s", err, tail(out.String(), 5))
	}
	return ParseControlData(out.String())
}

// CrashRecover makes sure a stopped data dir was shut down cleanly, which
// pg_rewind requires. If it was not (Postgres crashed or was killed), it
// runs Postgres once in single-user mode, which replays the WAL to the end
// and then shuts down cleanly when its input ends. That temporary start
// cannot be used as a primary: a single-user backend opens no network
// port, so no client can connect or write; it starts no WAL receiver, so
// it connects to nothing (standby.signal and recovery.signal are removed
// first: single-user mode refuses standby mode, and the caller rewrites
// them); its only input is an empty stdin. It runs under the guard like
// any tool.
func (n Node) CrashRecover(ctx context.Context, inst Install, g *Guard, log *slog.Logger) error {
	ci, err := n.ControlData(ctx, inst)
	if err != nil {
		return err
	}
	if ci.CleanlyShutDown() {
		return nil
	}
	if st, err := n.Status(); err != nil {
		return err
	} else if st.Running {
		return fmt.Errorf("Postgres is running on %s (pid %d)", n.PGData(), st.PID)
	}
	log.Warn("data dir was not shut down cleanly; running crash recovery in single-user mode", "state", ci.State)
	for _, f := range []string{standbySignal, "recovery.signal"} {
		if err := os.Remove(filepath.Join(n.PGData(), f)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	// No upstream and no sync list in the config used for recovery.
	if err := n.writeConfigFiles(ReplState{}); err != nil {
		return err
	}
	out, err := runTool(ctx, g, log, toolSpec{
		Name: "postgres --single (crash recovery)",
		Bin:  inst.Bin("postgres"),
		Args: []string{
			"--single",
			"-D", n.PGData(),
			"-c", "archive_mode=off",
			"-c", "restore_command=",
			"-c", "primary_conninfo=",
			"-c", "synchronous_standby_names=",
			"template1",
		},
		Env:     toolEnv(),
		LogFile: filepath.Join(n.LogDir(), "crash-recovery.log"),
	})
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("crash recovery: %w\n%s", err, out)
	}
	ci, err = n.ControlData(ctx, inst)
	if err != nil {
		return err
	}
	if !ci.CleanlyShutDown() {
		return fmt.Errorf("crash recovery finished but the data dir state is %q", ci.State)
	}
	log.Info("crash recovery done", "timeline", ci.Timeline)
	return nil
}

// RewindOptions controls Rewind and RewindOrReclone.
type RewindOptions struct {
	Install Install
	Guard   *Guard   // required (safety rule 5)
	Source  Upstream // the current primary
	Logger  *slog.Logger
}

// Rewind turns this node (normally a former primary) into a standby of
// o.Source with pg_rewind (safety rules 6 and 7):
//
//  1. mark the node "must rejoin" so it can never start as a primary;
//  2. make sure the data dir was shut down cleanly (CrashRecover);
//  3. CHECKPOINT on the source, so its control file shows its new timeline;
//  4. create this node's slot on the source;
//  5. pg_rewind --source-server as the rewind role (password in a passfile);
//  6. drop stale slots, strip postgresql.auto.conf, write standby.signal,
//     primary_conninfo and primary_slot_name (through coyote.conf).
//
// The node is left stopped; the agent starts it as a standby. The caller
// holds the node lock and Postgres must not be running.
func (n Node) Rewind(ctx context.Context, o RewindOptions) error {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	log := o.Logger
	if o.Guard == nil {
		return errors.New("refusing to rewind without a process guard (safety rule 5)")
	}
	if err := o.Source.Valid(); err != nil {
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
	if !n.Initialized() {
		return errors.New("there is no data to rewind")
	}
	if st, err := n.Status(); err != nil {
		return err
	} else if st.Running {
		return fmt.Errorf("Postgres is running on %s (pid %d); stop it first", n.PGData(), st.PID)
	}
	ci, err := n.ControlData(ctx, o.Install)
	if err != nil {
		return err
	}
	if err := n.ensureMarker(Marker{Reason: ReasonDemoted, Timeline: ci.Timeline, Detail: "rewind requested"}); err != nil {
		return err
	}
	if err := n.CrashRecover(ctx, o.Install, o.Guard, log); err != nil {
		return err
	}
	after, err := n.ControlData(ctx, o.Install)
	if err != nil {
		return err
	}

	if err := n.writePassfile(o.Source, s); err != nil {
		return err
	}
	src, err := connectUpstream(ctx, o.Source, RewindUser, s)
	if err != nil {
		return err
	}
	rec, err := inRecovery(ctx, src)
	if err == nil && rec {
		err = fmt.Errorf("rewind source %s: %w", o.Source, ErrUpstreamNotPrimary)
	}
	if err == nil {
		_, err = src.Exec(ctx, "CHECKPOINT")
	}
	var srcTLI int64
	if err == nil {
		err = src.QueryRow(ctx, "SELECT timeline_id FROM pg_control_checkpoint()").Scan(&srcTLI)
	}
	src.Close(context.Background())
	if err != nil {
		return fmt.Errorf("prepare rewind source: %w", err)
	}
	if err := sameTimelineCheck(after, uint32(srcTLI)); err != nil {
		return err
	}
	slot := SlotName(info.Name)
	if err := ensureUpstreamSlot(ctx, o.Source, s, slot); err != nil {
		return err
	}

	if err := n.WriteMarker(Marker{Reason: ReasonRewinding, Timeline: ci.Timeline, Detail: "pg_rewind from " + o.Source.String()}); err != nil {
		return err
	}
	out, err := runTool(ctx, o.Guard, log, toolSpec{
		Name: "pg_rewind",
		Bin:  o.Install.Bin("pg_rewind"),
		Args: []string{
			"--target-pgdata", n.PGData(),
			"--source-server", Conninfo(ConnParams{Upstream: o.Source, User: RewindUser, DBName: "postgres", AppName: info.Name + "-rewind"}),
			"--no-ensure-shutdown",
			"--progress",
		},
		Env:     toolEnv("PGPASSFILE=" + n.PassfilePath()),
		LogFile: filepath.Join(n.LogDir(), "rewind.log"),
	})
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("%w\n%s", err, out)
	}

	if err := prepareStandbyDir(n.PGData()); err != nil {
		return err
	}
	st, err := n.ReadReplState()
	if err != nil {
		return err
	}
	st.Upstream, st.Slot = &o.Source, slot
	if err := n.WriteReplState(st); err != nil {
		return err
	}
	if err := n.writeConfigFiles(st); err != nil {
		return err
	}
	log.Info("rewind complete", "source", o.Source.String())
	return n.ClearMarker()
}

// ErrSameTimeline: the data to rewind wrote WAL as a primary on the very
// timeline the source is on, so pg_rewind cannot see where they differ.
var ErrSameTimeline = errors.New("this node's data and the main node are on the same timeline, so pg_rewind cannot find where they differ")

// sameTimelineCheck refuses a rewind that pg_rewind would wrongly call
// unneeded (D56). pg_rewind compares timelines only: on the same timeline
// it says "no rewind required" and leaves the target as it is. A target
// that was shut down as a primary ("shut down": a primary's own shutdown,
// or CrashRecover's end of recovery) ends with a checkpoint record it
// wrote itself. A source on the same timeline cannot hold that record:
// it would have had to stream it from the target and then become a
// primary without a promotion, which always starts a new timeline. So the
// two histories differ under one timeline ID and the target's standby
// would loop on "incorrect prev-link" forever. The only correct fix is a
// fresh copy. (A copy cleanly stopped in recovery, "shut down in
// recovery", wrote no WAL of its own and is left to pg_rewind.)
func sameTimelineCheck(target ControlInfo, sourceTLI uint32) error {
	if target.State == "shut down" && target.Timeline == sourceTLI {
		return fmt.Errorf("%w (timeline %d)", ErrSameTimeline, sourceTLI)
	}
	return nil
}

// RewindOrReclone rewinds the node and, if that fails for any reason (or
// an earlier rewind/clone was interrupted, so the data cannot be trusted),
// re-clones it from o.Source instead. It reports whether it re-cloned.
// A cancelled context never falls through to a re-clone.
func (n Node) RewindOrReclone(ctx context.Context, o RewindOptions) (bool, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	m, err := n.ReadMarker()
	if err != nil {
		return false, err
	}
	if RewindFirst(m) && n.Initialized() {
		err := n.Rewind(ctx, o)
		if err == nil {
			return false, nil
		}
		if ctx.Err() != nil {
			return false, err
		}
		o.Logger.Warn("rewind failed; re-cloning instead", "err", err)
	} else {
		o.Logger.Warn("data dir cannot be rewound; re-cloning", "marker", markerReason(m))
	}
	return true, n.Reclone(ctx, o)
}

// Reclone replaces this node's data with a fresh copy of o.Source.
func (n Node) Reclone(ctx context.Context, o RewindOptions) error {
	info, err := n.ReadInfo()
	if err != nil {
		return err
	}
	s, err := n.Secrets()
	if err != nil {
		return err
	}
	return n.Clone(ctx, CloneOptions{
		Install: o.Install, Guard: o.Guard, Source: o.Source, Secrets: s,
		Name: info.Name, Port: info.Port, Reclone: true, Logger: o.Logger,
	})
}

// Demote records that this node must not start as a primary again (it
// must be rewound or re-cloned first). In M2 an operator runs it on the
// old primary; from M3 the agent does it when Raft says another node is
// primary. The caller holds the node lock.
func (n Node) Demote(ctx context.Context, inst Install, detail string) error {
	m := Marker{Reason: ReasonDemoted, Detail: detail}
	if n.Initialized() {
		if ci, err := n.ControlData(ctx, inst); err == nil {
			m.Timeline = ci.Timeline
		}
	}
	return n.ensureMarker(m)
}

// Fenced records that this node stopped its own primary because its lease
// ran out at the given cluster epoch (see Marker.FencedEpoch). An existing
// marker is kept (it is at least as strong).
func (n Node) Fenced(ctx context.Context, inst Install, epoch uint64, detail string) error {
	m := Marker{Reason: ReasonDemoted, Detail: detail, FencedEpoch: epoch}
	if n.Initialized() {
		if ci, err := n.ControlData(ctx, inst); err == nil {
			m.Timeline = ci.Timeline
		}
	}
	return n.ensureMarker(m)
}

func markerReason(m *Marker) string {
	if m == nil {
		return ""
	}
	return m.Reason
}
