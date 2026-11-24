package pg

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (n Node) cloneTmp() string { return filepath.Join(n.StateDir, "pgdata.clone-tmp") }
func (n Node) oldTmp() string   { return filepath.Join(n.StateDir, "pgdata.old") }

// CloneOptions controls Clone.
type CloneOptions struct {
	Install Install
	Guard   *Guard // required (safety rule 5)
	Source  Upstream
	Secrets Secrets // the cluster's secrets (from the upstream)
	Name    string  // this node's name: slot and application_name
	Port    int     // this node's Postgres port (stored in node.json)
	// Reclone allows replacing an existing data dir. The old data is
	// removed only after the new copy is complete.
	Reclone bool
	Logger  *slog.Logger
	// Progress, if set, is called with the copy's percentage as
	// pg_basebackup reports it (0..100, only when it changes).
	Progress func(pct int)
}

// Clone makes this node a standby of o.Source: it creates the node's slot
// on the source, runs pg_basebackup (-X stream, the slot, fast checkpoint)
// into a temp dir, prepares it as a standby (standby.signal, settings
// through coyote.conf, replication settings stripped from
// postgresql.auto.conf) and moves it into place. Postgres must not be
// running on this node; the caller holds the node lock.
//
// Crash safety: a marker (rejoin.json) exists for the whole operation, so
// an interrupted clone never starts as a primary and an interrupted
// re-clone is redone. The existing pgdata is moved aside only after the
// new copy is complete.
func (n Node) Clone(ctx context.Context, o CloneOptions) error {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	log := o.Logger
	if o.Guard == nil {
		return errors.New("refusing to clone without a process guard (safety rule 5)")
	}
	if err := ValidName(o.Name); err != nil {
		return err
	}
	if err := o.Source.Valid(); err != nil {
		return err
	}
	if err := o.Secrets.complete(); err != nil {
		return err
	}
	if err := n.Prepare(); err != nil {
		return err
	}
	if st, err := n.Status(); err != nil {
		return err
	} else if st.Running {
		return fmt.Errorf("Postgres is running on %s (pid %d); stop it before cloning", n.PGData(), st.PID)
	}
	initialized := n.Initialized()
	if _, err := os.Stat(n.PGData()); err == nil && !o.Reclone {
		if initialized {
			return fmt.Errorf("%s already holds a database; refusing to clone over it (use reclone to replace it)", n.PGData())
		}
		return fmt.Errorf("%s exists but is not a Postgres data dir; move it away first", n.PGData())
	}
	if initialized {
		if err := n.ensureMarker(Marker{Reason: ReasonDemoted, Detail: "re-clone requested"}); err != nil {
			return err
		}
	} else {
		if err := n.WriteMarker(Marker{Reason: ReasonCloning, Detail: "clone from " + o.Source.String()}); err != nil {
			return err
		}
	}
	if err := n.WriteSecrets(o.Secrets); err != nil {
		return err
	}
	info, _ := n.ReadInfo()
	info.Name, info.Port = o.Name, o.Port
	info.PGVersion, info.Target = o.Install.Build.Version, o.Install.Build.Target
	if err := n.WriteInfo(info); err != nil {
		return err
	}
	if err := n.writePassfile(o.Source, o.Secrets); err != nil {
		return err
	}
	slot := SlotName(o.Name)
	if err := ensureUpstreamSlot(ctx, o.Source, o.Secrets, slot); err != nil {
		return fmt.Errorf("create replication slot on %s: %w", o.Source, err)
	}

	tmp := n.cloneTmp()
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("remove leftover %s: %w", tmp, err)
	}
	conninfo := Conninfo(ConnParams{Upstream: o.Source, User: ReplUser, AppName: o.Name + "-clone"})
	out, err := runTool(ctx, o.Guard, log, toolSpec{
		Name: "pg_basebackup",
		Bin:  o.Install.Bin("pg_basebackup"),
		Args: []string{
			"--pgdata", tmp,
			"--dbname", conninfo,
			"--wal-method=stream",
			"--slot", slot,
			"--checkpoint=fast",
			"--progress",
			"--verbose",
			"--no-password",
		},
		Env:     toolEnv("PGPASSFILE=" + n.PassfilePath()),
		LogFile: filepath.Join(n.LogDir(), "basebackup.log"),
		OnLine:  progressParser(o.Progress),
	})
	if err != nil {
		_ = os.RemoveAll(tmp)
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("%w\n%s", err, out)
	}
	if err := prepareStandbyDir(tmp); err != nil {
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
	if initialized {
		if err := n.WriteMarker(Marker{Reason: ReasonRecloning, Detail: "replacing data from " + o.Source.String()}); err != nil {
			return err
		}
	}
	if err := n.swapInPGData(tmp); err != nil {
		return err
	}
	if err := n.writeConfigFiles(st); err != nil {
		return err
	}
	log.Info("clone complete", "source", o.Source.String(), "slot", slot)
	return n.ClearMarker()
}

// progressParser turns pg_basebackup --progress lines ("1234/5678 kB (21%),
// 0/1 tablespace") into percentages.
func progressParser(f func(int)) func(string) {
	if f == nil {
		return nil
	}
	last := -1
	return func(line string) {
		if pct, ok := ParseBasebackupProgress(line); ok && pct != last {
			last = pct
			f(pct)
		}
	}
}

// ParseBasebackupProgress extracts the percentage from a pg_basebackup
// progress line.
func ParseBasebackupProgress(line string) (int, bool) {
	i := strings.Index(line, "%)")
	if i < 0 {
		return 0, false
	}
	j := strings.LastIndex(line[:i], "(")
	if j < 0 {
		return 0, false
	}
	pct, err := strconv.Atoi(strings.TrimSpace(line[j+1 : i]))
	if err != nil || pct < 0 || pct > 100 {
		return 0, false
	}
	return pct, true
}

// prepareStandbyDir makes a copied data dir start as a standby: no stale
// replication settings in postgresql.auto.conf, no slots, standby.signal.
func prepareStandbyDir(pgdata string) error {
	if err := stripAutoConf(pgdata); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(pgdata, "recovery.signal"))
	if err := removeSlotsAt(filepath.Join(pgdata, "pg_replslot")); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(pgdata, standbySignal), nil, 0o600)
}

// looksLikePGData reports whether dir is a Postgres data directory.
func looksLikePGData(dir string) bool {
	for _, f := range []string{"PG_VERSION", filepath.Join("global", "pg_control")} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

// swapInPGData replaces pgdata with the finished copy in tmp. Only paths
// owned by this node are touched, and pgdata is moved aside only if it is
// recognisably a Postgres data dir.
func (n Node) swapInPGData(tmp string) error {
	pgdata, old := n.PGData(), n.oldTmp()
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("remove leftover %s: %w", old, err)
	}
	if _, err := os.Stat(pgdata); err == nil {
		if !looksLikePGData(pgdata) {
			return fmt.Errorf("%s does not look like a Postgres data dir; refusing to replace it", pgdata)
		}
		if err := os.Rename(pgdata, old); err != nil {
			return fmt.Errorf("move old data aside: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(tmp, pgdata); err != nil {
		return fmt.Errorf("move new data into place: %w", err)
	}
	if err := syncDir(n.StateDir); err != nil {
		return err
	}
	if err := os.RemoveAll(old); err != nil {
		return fmt.Errorf("remove old data %s: %w", old, err)
	}
	return nil
}

// ensureMarker writes m unless a marker already exists (an existing one is
// at least as strong: never downgrade "rewinding" to "demoted").
func (n Node) ensureMarker(m Marker) error {
	cur, err := n.ReadMarker()
	if err != nil {
		return err
	}
	if cur != nil {
		return nil
	}
	return n.WriteMarker(m)
}
