package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/backup"
	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/pg"
)

// The dashboard's backup helpers (dashboard/API.md §13): try a destination
// before it is saved (also on the setup page, before a cluster exists),
// and run `backup check` / `backup restore` from the page. A restore goes
// into a NEW folder only, exactly like the command line (D63): nothing
// here can write into the running cluster's folders or its backups.

// minBackupFree is the free space a backup folder needs (D63 skips a full
// backup below 2 GiB free).
const minBackupFree = 2 << 30

// BackupDestCheck answers POST /v1/backups/check-destination.
type BackupDestCheck struct {
	OK bool `json:"ok"`
	// Problem: why it cannot be used, in plain words, with what to do.
	Problem string `json:"problem,omitempty"`
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	// Folder is the resolved absolute folder (folder kind; "" in the
	// request means next to the data folder).
	Folder string `json:"folder,omitempty"`
	// Exists: the folder exists already (else it is created when saved).
	Exists     bool   `json:"exists,omitempty"`
	FreeBytes  uint64 `json:"free_bytes,omitempty"`
	TotalBytes uint64 `json:"total_bytes,omitempty"`
	// SameDisk: the folder is on the disk that holds the data folder.
	SameDisk  bool     `json:"same_disk,omitempty"`
	Encrypted bool     `json:"encrypted"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Same words as the dashboard (dashboard/src/lib/backups.ts).
const sameDiskWarning = "This folder is on the same disk as the database. It protects you from mistakes, like a deleted table, but not from this disk failing. Use another disk, a USB drive or a network drive if you can."

func (a *Agent) apiBackupCheckDestination(w http.ResponseWriter, r *http.Request) {
	var req BackupDestinationRequest
	if err := control.ReadJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	control.WriteJSON(w, http.StatusOK, a.checkDest(ctx, req))
}

// checkDest tries a destination without saving anything (a small test
// object is written and deleted again).
func (a *Agent) checkDest(ctx context.Context, req BackupDestinationRequest) BackupDestCheck {
	out := BackupDestCheck{Kind: req.Kind}
	var st cluster.State
	var old cluster.BackupSettings
	member := a.cn != nil
	if member {
		st = a.cn.State()
		old = st.EffectiveBackup()
	}
	switch req.Kind {
	case cluster.BackupKindFolder:
		out.Encrypted = req.Encrypt
		dir := req.Folder
		if dir == "" {
			dir = DefaultBackupFolder(a.node.StateDir)
		}
		if !filepath.IsAbs(dir) {
			out.Problem = "Type the full path of the folder, for example " + exampleFolder() + "."
			return out
		}
		dir = filepath.Clean(dir)
		out.Folder, out.Label = dir, dir
		a.checkFolder(&out)
		return out
	case cluster.BackupKindS3:
		out.Encrypted = true
		if req.S3 == nil {
			out.Problem = "Fill in the storage address, the bucket and the access keys."
			return out
		}
		s3 := *req.S3
		if s3.SecretKey == "" && old.S3 != nil && old.S3.AccessKey == s3.AccessKey {
			s3.SecretKey = old.S3.SecretKey // unchanged secret
		}
		switch {
		case strings.TrimSpace(s3.Endpoint) == "":
			out.Problem = "Fill in the storage address (endpoint), for example https://s3.us-west-002.backblazeb2.com."
			return out
		case !strings.HasPrefix(s3.Endpoint, "https://") && !strings.HasPrefix(s3.Endpoint, "http://"):
			out.Problem = "The storage address must start with https:// (for example https://s3.eu-central-1.amazonaws.com)."
			return out
		case strings.TrimSpace(s3.Bucket) == "":
			out.Problem = "Fill in the bucket name."
			return out
		case s3.AccessKey == "" || s3.SecretKey == "":
			out.Problem = "Fill in the access key ID and the secret key."
			return out
		}
		cfg := backup.Config{Kind: backup.KindS3, S3: backup.S3Settings{Endpoint: s3.Endpoint, Region: s3.Region, Bucket: s3.Bucket,
			Prefix: s3.Prefix, AccessKey: s3.AccessKey, SecretKey: s3.SecretKey, PathStyle: s3.PathStyle}}
		out.Label = cfg.Label()
		var err error
		if member {
			cfg.ClusterID, cfg.Node = st.ClusterID, a.name
			err = checkDestination(ctx, cfg)
		} else {
			err = probeStore(ctx, cfg)
		}
		if err != nil {
			out.Problem = s3Problem(err)
			return out
		}
		out.OK = true
		if strings.HasPrefix(s3.Endpoint, "http://") {
			out.Warnings = append(out.Warnings, "This address is not https. Backups are encrypted before they leave this computer, but the access keys travel unprotected. Use https if the storage offers it.")
		}
		return out
	}
	out.Problem = `Choose "folder" or "s3".`
	return out
}

func exampleFolder() string {
	if os.PathSeparator == '\\' {
		return `D:\coyote-backups`
	}
	return "/mnt/backup-disk/coyote-backups"
}

// checkFolder fills in a folder check: where, writable, free space, same
// disk as the data.
func (a *Agent) checkFolder(out *BackupDestCheck) {
	dir := out.Folder
	data := a.node.StateDir
	if within(dir, data) || within(data, dir) {
		out.Problem = "Pick a folder outside the database's own folder (" + data + "): backups inside it would be lost with it."
		return
	}
	near := backup.NearestExisting(dir)
	if near == "" {
		out.Problem = "That folder's drive does not exist on this computer."
		return
	}
	fi, err := os.Stat(near)
	if err != nil {
		out.Problem = "Cannot look at " + near + ": " + err.Error()
		return
	}
	if !fi.IsDir() {
		out.Problem = near + " is a file, not a folder. Pick a folder."
		return
	}
	out.Exists = near == dir
	f, err := os.CreateTemp(near, ".coyote-check-*")
	if err != nil {
		out.Problem = "This computer's " + brand.Command + " cannot write in " + near + ". Pick another folder, or give it permission to write there."
		return
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	if free, total, err := pg.DiskSpace(near); err == nil {
		out.FreeBytes, out.TotalBytes = free, total
		if free < minBackupFree {
			out.Problem = fmt.Sprintf("That disk has only %s free. Backups need at least 2 GB; free some space or pick another disk.", sizeText(free))
			return
		}
		if free < total/10 {
			out.Warnings = append(out.Warnings, fmt.Sprintf("That disk is over 90%% full (%s free). Full backups are skipped when it is that full.", sizeText(free)))
		}
	}
	if same, err := backup.SameDisk(dir, data); err == nil && same {
		out.SameDisk = true
	}
	if !out.Exists {
		out.Warnings = append(out.Warnings, "The folder does not exist yet; it is created when you save.")
	}
	out.OK = true
}

// within reports whether p is dir or inside it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// probeStore lists, writes and deletes a small object at a destination
// that has no cluster folder yet (the setup page).
func probeStore(ctx context.Context, cfg backup.Config) error {
	st, err := cfg.BaseStore()
	if err != nil {
		return err
	}
	if _, err := st.List(ctx, "coyote-check/"); err != nil {
		return fmt.Errorf("cannot list %s: %w", st.Label(), err)
	}
	key := "coyote-check/" + time.Now().UTC().Format("20060102T150405.000000000")
	if err := st.Put(ctx, key, []byte("ok"), false); err != nil {
		return fmt.Errorf("cannot write to %s: %w", st.Label(), err)
	}
	if err := st.Delete(ctx, key); err != nil {
		return fmt.Errorf("cannot delete from %s: %w", st.Label(), err)
	}
	return nil
}

// s3Problem turns a storage error into what to do.
func s3Problem(err error) string {
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "403") || strings.Contains(low, "accessdenied") || strings.Contains(low, "signaturedoesnotmatch") || strings.Contains(low, "invalidaccesskeyid"):
		return "The storage refused these keys. Check the access key ID and the secret key, and that the key may read, write and delete in this bucket. (" + msg + ")"
	case strings.Contains(low, "nosuchbucket") || strings.Contains(low, "404"):
		return "That bucket was not found at this address. Check the bucket name and that the address is for the bucket's region. (" + msg + ")"
	case strings.Contains(low, "no such host") || strings.Contains(low, "connection refused") || strings.Contains(low, "timeout") || strings.Contains(low, "deadline exceeded"):
		return "Could not reach the storage address from this computer. Check the address and the internet connection. (" + msg + ")"
	case errors.Is(err, backup.ErrOtherDatabase):
		return "That place already holds backups of a different database. Pick another bucket or another folder inside it."
	}
	return "That storage did not work from this computer: " + msg
}

// ---- check and restore jobs ----

// BackupJob is a check or a restore run by this agent (one at a time).
type BackupJob struct {
	Kind       string     `json:"kind"`  // "check" | "restore"
	State      string     `json:"state"` // "running" | "done" | "failed"
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Lines: progress for people, oldest first.
	Lines []string `json:"lines"`
	// Restore: the new folder and the moment asked for (none: everything
	// saved).
	Into   string     `json:"into,omitempty"`
	Target *time.Time `json:"target,omitempty"`
	// Results.
	BackupID   string   `json:"backup_id,omitempty"`
	LastChange string   `json:"last_change,omitempty"`
	StopLine   string   `json:"stop_line,omitempty"`
	Databases  int      `json:"databases,omitempty"`
	Relations  int      `json:"relations,omitempty"`
	Damage     []string `json:"damage,omitempty"`
	TookMS     int64    `json:"took_ms,omitempty"`
	Error      string   `json:"error,omitempty"`
	// NextCommand starts a restored folder as a new, separate cluster on
	// this computer (ports picked that are free right now).
	NextCommand string `json:"next_command,omitempty"`
}

func (j *BackupJob) copy() *BackupJob {
	if j == nil {
		return nil
	}
	c := *j
	c.Lines = append([]string(nil), j.Lines...)
	c.Damage = append([]string(nil), j.Damage...)
	return &c
}

// jobs returns copies of the last check and the last restore.
func (a *Agent) backupJobs() (check, restore *BackupJob) {
	a.bk.mu.Lock()
	defer a.bk.mu.Unlock()
	return a.bk.check.copy(), a.bk.restore.copy()
}

// startJob registers a new job unless one is running.
func (a *Agent) startJob(j *BackupJob) bool {
	a.bk.mu.Lock()
	defer a.bk.mu.Unlock()
	for _, o := range []*BackupJob{a.bk.check, a.bk.restore} {
		if o != nil && o.State == "running" {
			return false
		}
	}
	if j.Kind == "check" {
		a.bk.check = j
	} else {
		a.bk.restore = j
	}
	return true
}

func (a *Agent) jobUpdate(j *BackupJob, f func(*BackupJob)) {
	a.bk.mu.Lock()
	defer a.bk.mu.Unlock()
	f(j)
}

// RestoreRequest is POST /v1/backups/restore and /v1/backups/check.
type RestoreRequest struct {
	// To: RFC 3339; "" = everything saved so far.
	To string `json:"to,omitempty"`
	// Into: a new or empty folder (full path); "" in a dry run returns a
	// suggestion.
	Into string `json:"into,omitempty"`
	// DryRun: check the plan, start nothing.
	DryRun bool `json:"dry_run,omitempty"`
}

// RestorePlan answers a dry run.
type RestorePlan struct {
	OK            bool       `json:"ok"`
	Problem       string     `json:"problem,omitempty"`
	Into          string     `json:"into,omitempty"`
	SuggestedInto string     `json:"suggested_into"`
	Exists        bool       `json:"exists,omitempty"`
	FreeBytes     uint64     `json:"free_bytes,omitempty"`
	NeedBytes     int64      `json:"need_bytes,omitempty"`
	BackupID      string     `json:"backup_id,omitempty"`
	Target        *time.Time `json:"target,omitempty"`
	Warnings      []string   `json:"warnings,omitempty"`
}

func (a *Agent) jobPrereq(w http.ResponseWriter) (backup.Config, string, bool) {
	st := a.cn.State()
	bs := st.EffectiveBackup()
	if a.role != cluster.RoleData || a.backupView() == nil {
		writeErr(w, http.StatusConflict, "not_data_node", "This computer holds no data, so it has no backups. Open the dashboard on a data computer.", nil)
		return backup.Config{}, "", false
	}
	if !bs.Enabled {
		writeErr(w, http.StatusConflict, "backups_off", "Backups are off. Turn them on first.", nil)
		return backup.Config{}, "", false
	}
	exe, ok := backupExe()
	if !ok || a.inst.Root == "" {
		writeErr(w, http.StatusConflict, "not_ready", "This computer cannot run a restore yet (PostgreSQL is not installed here yet). Try again in a minute.", nil)
		return backup.Config{}, "", false
	}
	return a.backupConfig(bs, st), exe, true
}

func (a *Agent) apiBackupCheckRun(w http.ResponseWriter, r *http.Request) {
	var req struct{}
	if r.ContentLength > 0 {
		if err := control.ReadJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
			return
		}
	}
	cfg, exe, ok := a.jobPrereq(w)
	if !ok {
		return
	}
	ctx := a.jobContext()
	if ctx == nil {
		writeErr(w, http.StatusConflict, "not_ready", "Backups are still starting on this computer. Try again in a minute.", nil)
		return
	}
	into := filepath.Join(backup.StateDir(a.node.StateDir), "check-"+time.Now().UTC().Format("20060102T150405Z"))
	j := &BackupJob{Kind: "check", State: "running", StartedAt: time.Now().UTC(), Lines: []string{"Restoring the newest backup into a temporary folder on " + a.name + "."}}
	if !a.startJob(j) {
		writeErr(w, http.StatusConflict, "job_running", "A backup check or restore is already running on this computer. Wait for it to finish.", nil)
		return
	}
	go a.runJob(ctx, j, backup.RestoreOptions{Config: cfg, Into: into, Install: a.inst, Exe: exe})
	control.WriteJSON(w, http.StatusAccepted, j.copy())
}

func (a *Agent) apiBackupRestore(w http.ResponseWriter, r *http.Request) {
	var req RestoreRequest
	if err := control.ReadJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	cfg, exe, ok := a.jobPrereq(w)
	if !ok {
		return
	}
	plan := a.restorePlan(r.Context(), cfg, req)
	if req.DryRun {
		control.WriteJSON(w, http.StatusOK, plan)
		return
	}
	if !plan.OK {
		writeErr(w, http.StatusBadRequest, "restore_refused", plan.Problem, nil)
		return
	}
	ctx := a.jobContext()
	if ctx == nil {
		writeErr(w, http.StatusConflict, "not_ready", "Backups are still starting on this computer. Try again in a minute.", nil)
		return
	}
	j := &BackupJob{Kind: "restore", State: "running", StartedAt: time.Now().UTC(), Into: plan.Into, Target: plan.Target,
		Lines: []string{"Restoring into " + plan.Into + ". The running database is not touched."}}
	if !a.startJob(j) {
		writeErr(w, http.StatusConflict, "job_running", "A backup check or restore is already running on this computer. Wait for it to finish.", nil)
		return
	}
	var target time.Time
	if plan.Target != nil {
		target = *plan.Target
	}
	go a.runJob(ctx, j, backup.RestoreOptions{Config: cfg, Into: plan.Into, Target: target, Install: a.inst, Exe: exe})
	control.WriteJSON(w, http.StatusAccepted, j.copy())
}

// jobContext is the backup loop's context (nil before it runs).
func (a *Agent) jobContext() context.Context {
	a.bk.mu.Lock()
	defer a.bk.mu.Unlock()
	return a.bk.loopCtx
}

// restorePlan checks a restore request: a new or empty folder outside
// this node's folders, a moment the backups reach, room on the disk.
func (a *Agent) restorePlan(ctx context.Context, cfg backup.Config, req RestoreRequest) RestorePlan {
	p := RestorePlan{SuggestedInto: filepath.Join(filepath.Dir(a.node.StateDir), "coyote-restore-"+time.Now().Format("20060102-1504"))}
	a.bk.mu.Lock()
	cat := a.bk.cat
	a.bk.mu.Unlock()
	if cat == nil {
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		a.refreshCatalog(rctx, cfg)
		cancel()
		a.bk.mu.Lock()
		cat = a.bk.cat
		a.bk.mu.Unlock()
	}
	if cat == nil {
		p.Problem = "Cannot read the list of backups right now. Check that the backup place is reachable, then try again."
		return p
	}
	win := cat.RestoreWindow()
	if win.From.IsZero() {
		p.Problem = "There is no finished full backup yet, so there is nothing to restore. Take one with Back up now, then try again."
		return p
	}
	var target time.Time
	if req.To != "" {
		t, err := time.Parse(time.RFC3339, req.To)
		if err != nil {
			p.Problem = "That moment is not a date and time."
			return p
		}
		target = t
		switch {
		case t.Before(win.From):
			p.Problem = "That is before the oldest moment the backups reach (" + win.From.Local().Format(time.DateTime) + "). Pick a later moment."
			return p
		case t.After(win.To):
			p.Problem = "That is after the newest saved change (" + win.To.Local().Format(time.DateTime) + "). Pick an earlier moment, or restore everything saved."
			return p
		}
		u := t.UTC()
		p.Target = &u
	}
	var chosen *backup.Info
	for i := len(cat.Backups) - 1; i >= 0; i-- {
		b := cat.Backups[i]
		if cat.Usable(b) && (target.IsZero() || b.FinishedAt.Before(target)) {
			chosen = &b
			break
		}
	}
	if chosen == nil {
		p.Problem = "No full backup finished before that moment. Pick a moment after " + win.From.Local().Format(time.DateTime) + "."
		return p
	}
	p.BackupID, p.NeedBytes = chosen.ID, chosen.DataSize
	into := req.Into
	if into == "" {
		into = p.SuggestedInto
		if !req.DryRun {
			p.Problem = "Choose a new folder to restore into."
			return p
		}
	}
	if !filepath.IsAbs(into) {
		p.Problem = "Type the full path of a new folder, for example " + p.SuggestedInto + "."
		return p
	}
	into = filepath.Clean(into)
	p.Into = into
	data := a.node.StateDir
	switch {
	case within(into, data) || within(data, into):
		p.Problem = "That is this database's own folder (or holds it). A restore only goes into a new folder, so the running database is never touched."
		return p
	case cfg.Kind == backup.KindFolder && (within(into, cfg.Folder) || within(cfg.Folder, into)):
		p.Problem = "That is inside the backup folder. Pick a new folder somewhere else."
		return p
	}
	if ents, err := os.ReadDir(into); err == nil {
		if len(ents) > 0 {
			p.Problem = into + " is not empty. A restore only goes into a new or empty folder, so nothing is ever overwritten. Pick another name."
			return p
		}
		p.Exists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		p.Problem = "Cannot use " + into + ": " + err.Error()
		return p
	}
	near := backup.NearestExisting(into)
	if near == "" {
		p.Problem = "That folder's drive does not exist on this computer."
		return p
	}
	if fi, err := os.Stat(near); err != nil || !fi.IsDir() {
		p.Problem = near + " is not a folder."
		return p
	}
	if free, _, err := pg.DiskSpace(near); err == nil {
		p.FreeBytes = free
		need := uint64(max(chosen.DataSize, 0))
		if free < need+(need/10)+(256<<20) {
			p.Problem = fmt.Sprintf("That disk has %s free; the restored database needs about %s. Free some space or pick another disk.", sizeText(free), sizeText(need))
			return p
		}
	}
	p.OK = true
	return p
}

// runJob runs a check or a restore and records the outcome.
func (a *Agent) runJob(ctx context.Context, j *BackupJob, o backup.RestoreOptions) {
	o.Logger = a.log.With("component", "backup-"+j.Kind)
	o.Say = func(s string) { a.jobUpdate(j, func(j *BackupJob) { j.Lines = append(j.Lines, s) }) }
	start := time.Now()
	a.log.Info("backup: "+j.Kind+" starting", "into", o.Into)
	var err error
	if j.Kind == "check" {
		var res backup.CheckResult
		res, err = backup.Check(ctx, o)
		a.jobUpdate(j, func(j *BackupJob) {
			j.BackupID, j.LastChange = res.Restore.Backup.ID, res.Restore.LastTx
			j.Databases, j.Relations, j.Damage = res.Databases, res.Relations, res.Damage
			if err == nil && len(res.Damage) > 0 {
				err = fmt.Errorf("the restored copy has damage: %s", strings.Join(res.Damage, "; "))
			}
		})
	} else {
		var res backup.RestoreResult
		res, err = backup.Restore(ctx, o)
		a.jobUpdate(j, func(j *BackupJob) {
			j.BackupID, j.LastChange, j.StopLine = res.Backup.ID, res.LastTx, res.StopLine
			if err == nil {
				j.NextCommand = RestoredStartCommand(res.Dir)
			}
		})
	}
	a.jobUpdate(j, func(j *BackupJob) {
		now := time.Now().UTC()
		j.FinishedAt, j.TookMS = &now, time.Since(start).Milliseconds()
		j.State = "done"
		if err != nil {
			j.State, j.Error = "failed", err.Error()
		}
	})
	if err != nil {
		a.log.Warn("backup: "+j.Kind+" failed", "err", err)
	} else {
		a.log.Info("backup: "+j.Kind+" done", "took", time.Since(start).Round(time.Second))
	}
}

// RestoredStartCommand is the `up` line that starts a restored folder as
// a new, separate cluster, with ports that are free on this computer right
// now (so it does not clash with a node already running here).
func RestoredStartCommand(dir string) string {
	pgp := freePortNear(pg.DefaultPort+100, "127.0.0.1")
	cp := freePortNear(DefaultClientPort+100, "")
	_, lp, _ := net.SplitHostPort(DefaultListen)
	base, _ := strconv.Atoi(lp)
	listen := freePortNear(base+100, "")
	ctl := freePortNear(control.DefaultPort+100, "127.0.0.1")
	return fmt.Sprintf(`%s up --data-dir %s --pg-port %d --client-port %d --listen 0.0.0.0:%d --control-port %d`,
		brand.Command, quoteArg(dir), pgp, cp, listen, ctl)
}

// freePortNear tries base, base+1, ... for a port nobody listens on.
func freePortNear(base int, host string) int {
	for p := base; p < base+200; p++ {
		l, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err == nil {
			l.Close()
			return p
		}
	}
	return base
}

// backupBanner gives the Backups lines of the banner `up` prints when the
// node is ready (same words as `backup` and the dashboard).
func (a *Agent) backupBanner(st cluster.State) []string {
	bs := st.EffectiveBackup()
	if !bs.Enabled {
		return []string{fmt.Sprintf("off, so nothing is saved (turn on: %s backup on)", brand.Command)}
	}
	cfg := a.backupConfig(bs, st)
	when := "every night"
	if bs.Every != 24*time.Hour {
		when = "every " + strings.TrimSuffix(strings.TrimSuffix(bs.Every.String(), "0s"), "0m")
	} else if bs.At != "" {
		when = "every night at " + bs.At
	}
	keep := fmt.Sprintf("%d days", int(bs.Keep/(24*time.Hour)))
	if bs.Keep < 48*time.Hour {
		keep = fmt.Sprintf("%d hours", int(bs.Keep/time.Hour))
	}
	return []string{
		fmt.Sprintf("on: every change saved + a full copy %s, kept %s", when, keep),
		fmt.Sprintf("in %s (details: %s backup)", cfg.Label(), brand.Command),
	}
}
