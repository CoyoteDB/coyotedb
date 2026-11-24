package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/backup"
	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// Backups (D63): this agent renders the cluster's backup settings for its
// own Postgres (archive_command) and for wal-push (backup/config.json),
// takes full backups on schedule, applies retention and reports status.

const (
	backupTick       = 20 * time.Second
	backupCatalogAge = 5 * time.Minute
	// backupTakeOver: on a shared destination the next node in line takes
	// a due full backup this long after the one before it should have.
	backupTakeOver = time.Hour
	backupMaxRate  = "64M"
	// backupRetry: how soon a backup that could not start yet (archiving
	// not live) is tried again.
	backupRetry = 5 * time.Second
	// backupStartPatience: how long a backup may wait for archiving to
	// become live before that is reported as an error (and a manual
	// request is given up).
	backupStartPatience = 5 * time.Minute
)

// backupState is the backup loop's state.
type backupState struct {
	mu        sync.Mutex
	startedAt time.Time // this agent first saw backups on
	now       chan string
	rendered  string // JSON of the last rendered config
	cfg       backup.Config
	settings  cluster.BackupSettings
	renderErr string
	reloadErr string
	running   *backup.Running
	lastErr   string
	lastErrAt time.Time
	// arch is the archive setting this node's Postgres should run with.
	arch pg.ArchiveSettings
	// manualAt: a `backup now` is pending since then (until it starts or
	// is given up); waiting/waitingSince: why a due backup has not
	// started yet (archiving not live on the running Postgres).
	manualAt     time.Time
	waiting      string
	waitingSince time.Time
	cat          *backup.Catalog
	catAt        time.Time
	catErr       string
	capsKey      string
	capsAt       time.Time
	// loopCtx is the backup loop's context: checks and restores started
	// from the dashboard run under it (backup_jobs.go).
	loopCtx context.Context
	check   *BackupJob // last `check` from the dashboard
	restore *BackupJob // last restore from the dashboard
}

// backupExe is the program archive_command runs. A Go test binary is not
// this program, so under `go test` archiving stays a no-op.
func backupExe() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	base := strings.ToLower(filepath.Base(exe))
	if strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") {
		return "", false
	}
	return exe, true
}

// backupConfig resolves the cluster's settings for this node.
func (a *Agent) backupConfig(bs cluster.BackupSettings, st cluster.State) backup.Config {
	c := backup.Config{Enabled: bs.Enabled, ClusterID: st.ClusterID, Node: a.name, Kind: bs.Kind}
	switch bs.Kind {
	case cluster.BackupKindFolder:
		c.Folder = bs.Folder
		if c.Folder == "" {
			c.Folder = DefaultBackupFolder(a.node.StateDir)
		}
		if bs.Encrypt {
			c.Key = bs.Key
		}
	case cluster.BackupKindS3:
		if bs.S3 != nil {
			s := bs.S3
			c.S3 = backup.S3Settings{Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, Prefix: s.Prefix,
				AccessKey: s.AccessKey, SecretKey: s.SecretKey, PathStyle: s.PathStyle}
		}
		c.Key = bs.Key
	}
	return c
}

// DefaultBackupFolder is the folder next to a data folder.
func DefaultBackupFolder(stateDir string) string {
	return filepath.Join(filepath.Dir(stateDir), filepath.Base(stateDir)+"-backups")
}

func (a *Agent) backupLoop(ctx context.Context) {
	a.bk.mu.Lock()
	a.bk.now = make(chan string, 1)
	a.bk.loopCtx = ctx
	a.bk.mu.Unlock()
	t := time.NewTicker(backupTick)
	defer t.Stop()
	retry := a.backupStep(ctx, "")
	for {
		var soon <-chan time.Time
		if retry {
			soon = time.After(backupRetry)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			retry = a.backupStep(ctx, "")
		case <-soon:
			retry = a.backupStep(ctx, "")
		case kind := <-a.bk.now:
			retry = a.backupStep(ctx, kind)
		}
	}
}

// backupNow asks the loop for a full backup now (kind "manual"). False if
// the loop is not running or one is already queued.
func (a *Agent) backupNow() bool {
	a.bk.mu.Lock()
	ch := a.bk.now
	a.bk.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- "manual":
	default: // one is queued already
	}
	return true
}

// backupStep renders the settings and starts a full backup when one is
// due. True: a due backup is waiting for archiving to be live; try again
// soon.
func (a *Agent) backupStep(ctx context.Context, manual string) bool {
	if a.cn == nil {
		return false
	}
	st := a.cn.State()
	me, ok := st.Members[a.cn.ID()]
	if !ok || me.Role != cluster.RoleData || st.ClusterID == "" {
		return false
	}
	bs := st.EffectiveBackup()
	cfg := a.backupConfig(bs, st)
	a.renderBackup(ctx, bs, cfg)
	now := time.Now()
	a.bk.mu.Lock()
	if manual != "" && a.bk.manualAt.IsZero() {
		a.bk.manualAt = now
	}
	if !bs.Enabled {
		a.bk.manualAt, a.bk.waiting, a.bk.waitingSince = time.Time{}, "", time.Time{}
		a.bk.mu.Unlock()
		return false
	}
	if !a.bk.manualAt.IsZero() {
		manual = "manual"
	}
	if a.bk.startedAt.IsZero() {
		a.bk.startedAt = now
	}
	running := a.bk.running != nil
	stale := a.bk.cat == nil || now.Sub(a.bk.catAt) > backupCatalogAge || manual != ""
	a.bk.mu.Unlock()
	if stale && !running {
		a.refreshCatalog(ctx, cfg)
	}
	if running {
		a.bk.mu.Lock()
		a.bk.manualAt = time.Time{} // the running one answers it
		a.bk.mu.Unlock()
		return false
	}
	if srv, _ := a.currentPG(); srv == nil {
		if manual == "" {
			return false
		}
		return a.backupWait(errors.New("this node's database is not running yet"), now)
	}
	pendingWAL, adoptErr := a.adoptWAL(ctx)
	kind, ok := a.backupDue(st, bs, cfg, now, manual)
	if !ok {
		a.backupWaitDone()
		return false
	}
	if cfg.Kind == backup.KindFolder {
		if err := folderRoom(cfg.Folder); err != nil {
			a.backupWaitDone()
			a.backupFailed(err)
			return false
		}
	}
	// pg_basebackup -X none waits, on the server, for the WAL it needs to
	// be archived: never start one on a Postgres whose archiving is not
	// live (a copy still on an old setting, a reload not done, a failing
	// archive_command). Wait and try again instead.
	if adoptErr != nil {
		return a.backupWait(adoptErr, now)
	}
	if pendingWAL != "" {
		return a.backupWait(fmt.Errorf("this copy is still replaying the changes it received while it was cloned (WAL %s)", pendingWAL), now)
	}
	if err := a.archiveLive(ctx); err != nil {
		return a.backupWait(err, now)
	}
	a.backupWaitDone()
	a.startFull(ctx, cfg, bs, kind)
	return false
}

// adoptWAL marks WAL this copy replayed from its own pg_wal, and that
// Postgres never marked, for archiving (pg.AdoptUnarchivedWAL), and wakes
// the archiver. It returns a segment that cannot be judged yet ("" = none).
func (a *Agent) adoptWAL(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u, err := a.node.AdoptUnarchivedWAL(cctx)
	if err != nil {
		return "", fmt.Errorf("cannot check this node's WAL for archiving: %w", err)
	}
	if len(u.Adopted) > 0 {
		a.log.Info("backup: WAL from the clone marked for archiving", "segments", u.Adopted)
		// A reload wakes the archiver at once (it also wakes by itself
		// within a minute).
		if err := a.node.ReloadConfig(cctx); err != nil {
			a.log.Warn("backup: reload to wake the archiver failed", "err", err)
		}
	}
	return u.Pending, nil
}

// archiveLive checks on the running Postgres that WAL archiving is what
// this node rendered and is working.
func (a *Agent) archiveLive(ctx context.Context) error {
	a.bk.mu.Lock()
	want, renderErr, reloadErr := a.bk.arch, a.bk.renderErr, a.bk.reloadErr
	a.bk.mu.Unlock()
	if renderErr != "" {
		return errors.New(renderErr)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	live, err := a.node.ReadArchiveLive(cctx)
	if err != nil {
		return fmt.Errorf("cannot ask this node's database about WAL archiving: %w", err)
	}
	ps, _ := backup.ReadPushState(a.node.StateDir)
	if err := archiveProblem(live, want, ps); err != nil {
		if reloadErr != "" {
			return fmt.Errorf("%w (reload: %s)", err, reloadErr)
		}
		return err
	}
	return nil
}

// archiveProblem says in plain words why a full backup must not start on
// a Postgres with this live archive state (nil: it may start). want is
// the setting rendered for it; ps is wal-push's own record.
func archiveProblem(live pg.ArchiveLive, want pg.ArchiveSettings, ps backup.PushState) error {
	if want.Command == "" {
		return errors.New("WAL archiving is not set up for this node (the program that saves changes was not found)")
	}
	switch {
	case live.Mode == "always":
	case live.Mode == "on" && !live.InRecovery:
	default:
		return fmt.Errorf("WAL archiving is not active in this node's running database (archive_mode is %q; it takes effect when the database restarts)", live.Mode)
	}
	if live.Library != "" {
		return fmt.Errorf("this node's database archives WAL with %q, not with this program", live.Library)
	}
	if live.Command != want.Command {
		return errors.New("this node's database has not picked up the backup settings yet (its archive command is not ours); it is reloaded within seconds")
	}
	failing := !live.LastFailedAt.IsZero() && !live.LastFailedAt.Before(live.LastArchivedAt)
	if failing {
		msg := fmt.Sprintf("saving changes (WAL) to the backup place is failing on this node (last failure: %s at %s)",
			live.LastFailed, live.LastFailedAt.Local().Format(time.DateTime))
		if ps.LastError != "" && !ps.LastErrorAt.Before(live.LastFailedAt.Add(-time.Minute)) {
			msg += ": " + ps.LastError
		}
		return errors.New(msg + "; fix that first, see `" + brand.Command + " backup`")
	}
	return nil
}

// backupWait records why a due backup did not start. True: try again
// soon. Past backupStartPatience it is an error, and a manual request is
// given up.
func (a *Agent) backupWait(why error, now time.Time) bool {
	a.bk.mu.Lock()
	if a.bk.waitingSince.IsZero() {
		a.bk.waitingSince = now
		a.log.Info("backup: full backup waits", "why", why)
	}
	a.bk.waiting = why.Error()
	since := a.bk.waitingSince
	a.bk.mu.Unlock()
	if now.Sub(since) < backupStartPatience {
		return true
	}
	a.bk.mu.Lock()
	a.bk.manualAt, a.bk.waiting, a.bk.waitingSince = time.Time{}, "", time.Time{}
	a.bk.mu.Unlock()
	a.backupFailed(fmt.Errorf("full backup not started: %w", why))
	return false
}

func (a *Agent) backupWaitDone() {
	a.bk.mu.Lock()
	a.bk.manualAt, a.bk.waiting, a.bk.waitingSince = time.Time{}, "", time.Time{}
	a.bk.mu.Unlock()
}

// renderBackup writes backup/config.json and archive.json when they
// changed, and reloads Postgres for a new archive setting.
func (a *Agent) renderBackup(ctx context.Context, bs cluster.BackupSettings, cfg backup.Config) {
	b, _ := json.Marshal(cfg)
	arch := pg.ArchiveSettings{}
	var renderErr string
	if bs.Enabled {
		if exe, ok := backupExe(); ok {
			cmd, err := a.node.ArchiveCommandFor(exe)
			if err != nil {
				renderErr = err.Error()
			} else {
				arch = pg.ArchiveSettings{Command: cmd, Timeout: fmt.Sprintf("%ds", int64(bs.ArchiveTimeout/time.Second))}
			}
		}
	}
	a.bk.mu.Lock()
	changed := string(b) != a.bk.rendered
	a.bk.arch = arch
	a.bk.settings = bs
	a.bk.cfg = cfg
	a.bk.renderErr = renderErr
	a.bk.mu.Unlock()
	if changed {
		if err := backup.WriteConfig(backup.ConfigFile(a.node.StateDir), cfg); err != nil {
			a.log.Error("backup: cannot write its settings", "err", err)
			return
		}
		a.bk.mu.Lock()
		a.bk.rendered = string(b)
		a.bk.cat, a.bk.catErr = nil, ""
		a.bk.mu.Unlock()
		if bs.Enabled && cfg.Kind == backup.KindFolder {
			if err := os.MkdirAll(cfg.Folder, 0o700); err != nil {
				a.backupFailed(fmt.Errorf("cannot create the backup folder %s: %w", cfg.Folder, err))
			}
		}
	}
	cur, err := a.node.ReadArchive()
	if err == nil && cur == arch {
		a.bk.mu.Lock()
		reloadPending := a.bk.reloadErr != ""
		a.bk.mu.Unlock()
		if !reloadPending {
			return
		}
	}
	if err == nil && cur != arch {
		if err := a.node.WriteArchive(arch); err != nil {
			a.log.Error("backup: cannot write the archive settings", "err", err)
			return
		}
		a.log.Info("backup: archive settings changed", "on", arch.Command != "")
	}
	if srv, _ := a.currentPG(); srv == nil {
		return // the next start renders it
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rerr := a.node.ReloadConfig(rctx)
	a.bk.mu.Lock()
	a.bk.reloadErr = ""
	if rerr != nil {
		a.bk.reloadErr = rerr.Error()
	}
	a.bk.mu.Unlock()
	if rerr != nil {
		a.log.Warn("backup: reload for new archive settings failed; retrying", "err", rerr)
	}
}

func (a *Agent) refreshCatalog(ctx context.Context, cfg backup.Config) {
	st, err := cfg.Store()
	var cat backup.Catalog
	if err == nil {
		lctx, cancel := context.WithTimeout(ctx, time.Minute)
		cat, err = backup.LoadCatalog(lctx, st)
		cancel()
	}
	a.bk.mu.Lock()
	defer a.bk.mu.Unlock()
	a.bk.catAt = time.Now()
	if err != nil {
		a.bk.catErr = err.Error()
		return
	}
	a.bk.cat, a.bk.catErr = &cat, ""
}

// backupOrder is who takes full backups on a shared destination: the
// copies by id, then the main node.
func backupOrder(st cluster.State) []string {
	var ids []string
	for _, m := range st.DataMembers() {
		if m.ID != st.PrimaryID && !m.Removing {
			ids = append(ids, m.ID)
		}
	}
	slices.Sort(ids)
	if st.PrimaryID != "" {
		ids = append(ids, st.PrimaryID)
	}
	return ids
}

// backupDue decides whether this node takes a full backup now.
func (a *Agent) backupDue(st cluster.State, bs cluster.BackupSettings, cfg backup.Config, now time.Time, manual string) (string, bool) {
	if manual != "" {
		return manual, true
	}
	a.bk.mu.Lock()
	cat := a.bk.cat
	started := a.bk.startedAt
	a.bk.mu.Unlock()
	if cat == nil {
		return "", false
	}
	ps, _ := backup.ReadPushState(a.node.StateDir)
	gapOpen := ps.GapFrom != "" && ps.GapClosedByID == ""
	var lastStart time.Time
	for _, b := range cat.Backups {
		if cfg.Shared() || b.Node == cfg.Node {
			if b.StartedAt.After(lastStart) {
				lastStart = b.StartedAt
			}
		}
	}
	sched := backup.Schedule{Every: bs.Every, At: bs.At}
	since, due := sched.DueSince(now.Local(), lastStart, started.Add(backup.FirstDelay))
	if gapOpen && (lastStart.IsZero() || lastStart.Before(ps.GapAt)) {
		since, due = ps.GapAt, true
	}
	if !due {
		return "", false
	}
	if !cfg.Shared() {
		return "scheduled", true
	}
	for _, r := range cat.Running {
		if now.Sub(r.Heartbeat) < backup.RunningStaleAfter {
			return "", false // someone is on it
		}
	}
	rank := slices.Index(backupOrder(st), a.cn.ID())
	if rank < 0 {
		return "", false
	}
	return "scheduled", !now.Before(since.Add(time.Duration(rank) * backupTakeOver))
}

// folderRoom refuses a full backup that would leave the folder's disk
// under 10 % or 2 GiB free (D63).
func folderRoom(folder string) error {
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return fmt.Errorf("cannot create the backup folder %s: %w", folder, err)
	}
	free, total, err := pg.DiskSpace(folder)
	if err != nil {
		return nil // unknown: try
	}
	need := max(total/10, 2<<30)
	if free < need {
		return fmt.Errorf("the disk of the backup folder %s has only %d MB free; full backups are skipped below %d MB so the disk does not fill", folder, free>>20, need>>20)
	}
	return nil
}

func (a *Agent) backupFailed(err error) {
	a.bk.mu.Lock()
	first := a.bk.lastErr != err.Error()
	a.bk.lastErr, a.bk.lastErrAt = err.Error(), time.Now()
	a.bk.mu.Unlock()
	if first {
		a.log.Warn("backup: full backup not taken", "err", err)
	}
}

func (a *Agent) startFull(ctx context.Context, cfg backup.Config, bs cluster.BackupSettings, kind string) {
	run := &backup.Running{Node: a.name, StartedAt: time.Now().UTC()}
	a.bk.mu.Lock()
	a.bk.running = run
	a.bk.mu.Unlock()
	a.log.Info("backup: full backup starting", "kind", kind, "to", cfg.Label())
	go func() {
		info, err := backup.TakeFull(ctx, backup.FullOptions{
			Config: cfg, Node: a.node, Install: a.inst, Guard: a.guard,
			Logger: a.log.With("component", "backup"), Kind: kind, MaxRate: backupMaxRate,
		})
		a.bk.mu.Lock()
		a.bk.running = nil
		a.bk.mu.Unlock()
		if err != nil {
			a.backupFailed(err)
			return
		}
		a.bk.mu.Lock()
		a.bk.lastErr = ""
		a.bk.mu.Unlock()
		a.log.Info("backup: full backup done", "id", info.ID, "bytes", info.StoredSize)
		_ = backup.UpdatePushState(a.node.StateDir, func(s *backup.PushState) {
			if s.GapFrom != "" && s.GapClosedByID == "" && s.GapAt.Before(info.StartedAt) {
				s.GapClosedByID = info.ID
			}
		})
		a.refreshCatalog(ctx, cfg)
		a.applyRetention(ctx, cfg, bs)
	}()
}

func (a *Agent) applyRetention(ctx context.Context, cfg backup.Config, bs cluster.BackupSettings) {
	a.bk.mu.Lock()
	cat := a.bk.cat
	a.bk.mu.Unlock()
	st, err := cfg.Store()
	if cat == nil || err != nil {
		return
	}
	plan := cat.PlanRetention(time.Now(), bs.Keep)
	if len(plan.DeleteBackups)+len(plan.DeleteWAL)+len(plan.DeleteOrphans) == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := plan.Apply(rctx, st, time.Now()); err != nil {
		a.log.Warn("backup: removing old backups failed (tried again after the next one)", "err", err)
	} else {
		a.log.Info("backup: old backups removed", "full", len(plan.DeleteBackups), "wal", len(plan.DeleteWAL))
	}
	a.refreshCatalog(ctx, cfg)
}

// ---- settings writes ----

// backupFieldsOK: every member's agent knows the backup settings, so a
// set_backup entry cannot halt an older state machine.
func (a *Agent) backupFieldsOK(ctx context.Context) error {
	st := a.cn.State()
	if st.Backup != nil {
		return nil
	}
	ids := slices.Sorted(maps.Keys(st.Members))
	key := strings.Join(ids, ",")
	a.bk.mu.Lock()
	if a.bk.capsKey == key && time.Since(a.bk.capsAt) < capsCacheFor {
		a.bk.mu.Unlock()
		return nil
	}
	a.bk.mu.Unlock()
	for _, id := range ids {
		if id == a.cn.ID() {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		var r capsResp
		err := a.call(cctx, id, "caps", nil, &r)
		cancel()
		if err != nil || !r.Backup {
			return fmt.Errorf("%s runs an older program or does not answer; update and start every computer first", a.memberName(id))
		}
	}
	a.bk.mu.Lock()
	a.bk.capsKey, a.bk.capsAt = key, time.Now()
	a.bk.mu.Unlock()
	return nil
}

// setBackup writes new backup settings through Raft.
func (a *Agent) setBackup(ctx context.Context, f func(*cluster.BackupSettings) error) (cluster.BackupSettings, error) {
	if err := a.backupFieldsOK(ctx); err != nil {
		return cluster.BackupSettings{}, err
	}
	bs := a.cn.State().EffectiveBackup()
	if err := f(&bs); err != nil {
		return bs, err
	}
	if (bs.Kind == cluster.BackupKindS3 || bs.Encrypt) && bs.Key == "" {
		k, err := backup.NewKey()
		if err != nil {
			return bs, err
		}
		bs.Key = backup.EncodeKey(k)
	}
	bs.ChangedAt = time.Now().UTC()
	if _, err := a.cn.Apply(ctx, &cluster.SetBackup{Backup: bs}); err != nil {
		return bs, err
	}
	a.log.Info("backup: settings changed", "settings", bs)
	return bs, nil
}

// checkDestination tries a destination before it is saved: list, write
// and delete a small object.
func checkDestination(ctx context.Context, cfg backup.Config) error {
	st, err := cfg.Store()
	if err != nil {
		return err
	}
	if _, err := st.List(ctx, "base/"); err != nil {
		return fmt.Errorf("cannot list %s: %w", st.Label(), err)
	}
	key := "check-" + time.Now().UTC().Format("20060102T150405.000000000")
	if err := st.Put(ctx, key, []byte("ok"), false); err != nil {
		return fmt.Errorf("cannot write to %s: %w", st.Label(), err)
	}
	if err := st.Delete(ctx, key); err != nil {
		return fmt.Errorf("cannot delete from %s: %w", st.Label(), err)
	}
	if id, err := backup.ReadIdentity(ctx, st); err == nil && id.ClusterID != cfg.ClusterID {
		return backup.ErrOtherDatabase
	} else if err != nil && !errors.Is(err, backup.ErrNotFound) {
		return err
	}
	return nil
}
