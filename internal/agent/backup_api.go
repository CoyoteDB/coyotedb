package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/backup"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
)

// BackupView is the backup part of status (`status --json` "backup",
// GET /v1/status "backup", GET /v1/backups "status"; dashboard/API.md §13).
type BackupView struct {
	Enabled     bool           `json:"enabled"`
	Destination BackupDestView `json:"destination"`
	Encrypted   bool           `json:"encrypted"`
	LastBackup  *BackupItem    `json:"last_backup,omitempty"`
	Restorable  *BackupWindow  `json:"restorable,omitempty"`
	Running     *BackupItem    `json:"running,omitempty"`
	NextAt      *time.Time     `json:"next_backup_at,omitempty"`
	Archive     BackupArchive  `json:"archive"`
	Gap         *BackupGapView `json:"gap,omitempty"`
	// Waiting: why a due or asked-for full backup has not started yet
	// (WAL archiving not live on this node's database); it is tried again
	// every few seconds, for up to 5 minutes before it is an error.
	Waiting      string     `json:"waiting,omitempty"`
	WaitingSince *time.Time `json:"waiting_since,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	LastErrorAt  *time.Time `json:"last_error_at,omitempty"`
	Problem      string     `json:"problem,omitempty"`
}

// BackupDestView says where backups go (no secrets).
type BackupDestView struct {
	Kind  string `json:"kind"` // "folder" | "s3"
	Label string `json:"label"`
	// PerNode: every data node keeps its own complete set (folder).
	PerNode bool `json:"per_node"`
	// SameDisk: a folder on the disk that holds this node's data folder
	// (protects from mistakes, not from that disk failing).
	SameDisk bool `json:"same_disk,omitempty"`
}

// BackupWindow is the span a restore can reach.
type BackupWindow struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	GapAfter string    `json:"gap_after,omitempty"`
}

// BackupArchive is this node's WAL archiving.
type BackupArchive struct {
	LastArchived   string     `json:"last_archived,omitempty"`
	LastArchivedAt *time.Time `json:"last_archived_at,omitempty"`
	// WaitingBytes is WAL on this node not archived yet; LagMS how long
	// the oldest of it has waited (0: nothing waits).
	WaitingBytes int64      `json:"waiting_bytes"`
	LagMS        int64      `json:"lag_ms"`
	FailingSince *time.Time `json:"failing_since,omitempty"`
	Conflicts    int        `json:"conflicts,omitempty"`
}

// BackupGapView: WAL this node gave up on to protect its disk.
type BackupGapView struct {
	From   string    `json:"from"`
	To     string    `json:"to"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
	Closed bool      `json:"closed"`
}

// BackupItem is one full backup.
type BackupItem struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	State      string     `json:"state"` // "running" | "done" | "incomplete"
	Node       string     `json:"node"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	SizeBytes  int64      `json:"size_bytes"`
	DataBytes  int64      `json:"data_bytes,omitempty"`
}

// BackupsView is GET /v1/backups.
type BackupsView struct {
	Schedule    BackupScheduleView `json:"schedule"`
	Destination BackupDestView     `json:"destination"`
	Status      BackupView         `json:"status"`
	Backups     []BackupItem       `json:"backups"`
	// Check and Restore: the last ones started from the dashboard on this
	// node (kept until the agent restarts).
	Check   *BackupJob `json:"check,omitempty"`
	Restore *BackupJob `json:"restore,omitempty"`
}

// BackupScheduleView is the schedule (PUT /v1/backups/schedule).
type BackupScheduleView struct {
	Enabled        bool   `json:"enabled"`
	Every          string `json:"every"`
	At             string `json:"at"`
	Keep           string `json:"keep"`
	ArchiveTimeout string `json:"archive_timeout"`
}

func scheduleView(bs cluster.BackupSettings) BackupScheduleView {
	return BackupScheduleView{Enabled: bs.Enabled, Every: bs.Every.String(), At: bs.At, Keep: bs.Keep.String(), ArchiveTimeout: bs.ArchiveTimeout.String()}
}

func ptrTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// archiveWaiting measures WAL waiting to be archived on this node.
func (a *Agent) archiveWaiting() (bytes int64, oldest time.Time) {
	dir := filepath.Join(a.node.PGData(), "pg_wal", "archive_status")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, time.Time{}
	}
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".ready") {
			continue
		}
		bytes += backup.SegmentSize
		if fi, err := e.Info(); err == nil && (oldest.IsZero() || fi.ModTime().Before(oldest)) {
			oldest = fi.ModTime()
		}
	}
	return bytes, oldest
}

// backupView builds the status view (nil on a witness or before the
// cluster is known).
func (a *Agent) backupView() *BackupView {
	if a.cn == nil {
		return nil
	}
	st := a.cn.State()
	me, ok := st.Members[a.cn.ID()]
	if !ok || me.Role != cluster.RoleData {
		return nil
	}
	bs := st.EffectiveBackup()
	cfg := a.backupConfig(bs, st)
	v := &BackupView{
		Enabled:     bs.Enabled,
		Destination: a.destView(cfg),
		Encrypted:   cfg.Key != "",
	}
	if !bs.Enabled {
		return v
	}
	a.bk.mu.Lock()
	cat, catErr, run := a.bk.cat, a.bk.catErr, a.bk.running
	lastErr, lastErrAt, renderErr, reloadErr := a.bk.lastErr, a.bk.lastErrAt, a.bk.renderErr, a.bk.reloadErr
	started := a.bk.startedAt
	v.Waiting, v.WaitingSince = a.bk.waiting, ptrTime(a.bk.waitingSince)
	a.bk.mu.Unlock()
	now := time.Now()
	if run != nil {
		v.Running = &BackupItem{Kind: "running", State: "running", Node: run.Node, StartedAt: run.StartedAt}
	}
	var lastStart time.Time
	if cat != nil {
		if nb, ok := cat.Newest(); ok {
			v.LastBackup = itemOf(nb, "done")
		}
		if w := cat.RestoreWindow(); !w.From.IsZero() {
			v.Restorable = &BackupWindow{From: w.From.UTC(), To: w.To.UTC(), GapAfter: w.GapAfter}
		}
		for _, r := range cat.Running {
			if v.Running == nil && now.Sub(r.Heartbeat) < backup.RunningStaleAfter {
				v.Running = &BackupItem{ID: r.ID, State: "running", Node: r.Node, StartedAt: r.StartedAt}
			}
		}
		for _, b := range cat.Backups {
			if (cfg.Shared() || b.Node == cfg.Node) && b.StartedAt.After(lastStart) {
				lastStart = b.StartedAt
			}
		}
	}
	sched := backup.Schedule{Every: bs.Every, At: bs.At}
	if lastStart.IsZero() && !started.IsZero() {
		v.NextAt = ptrTime(started.Add(backup.FirstDelay))
	} else if since, due := sched.DueSince(now.Local(), lastStart, started); due {
		v.NextAt = ptrTime(since)
	} else {
		v.NextAt = ptrTime(sched.NextSlot(now.Local()))
	}
	ps, _ := backup.ReadPushState(a.node.StateDir)
	v.Archive.LastArchived = ps.LastName
	v.Archive.LastArchivedAt = ptrTime(ps.LastAt)
	v.Archive.FailingSince = ptrTime(ps.FailingSince)
	v.Archive.Conflicts = ps.Conflicts
	wb, oldest := a.archiveWaiting()
	v.Archive.WaitingBytes = wb
	if !oldest.IsZero() {
		v.Archive.LagMS = now.Sub(oldest).Milliseconds()
	}
	if ps.GapFrom != "" {
		v.Gap = &BackupGapView{From: ps.GapFrom, To: ps.GapTo, At: ps.GapAt.UTC(), Reason: ps.GapReason, Closed: ps.GapClosedByID != ""}
	}
	switch {
	case renderErr != "":
		v.LastError = renderErr
	case lastErr != "":
		v.LastError, v.LastErrorAt = lastErr, ptrTime(lastErrAt)
	case catErr != "":
		v.LastError = "cannot read the backup destination: " + catErr
	case reloadErr != "":
		v.LastError = "the new archive setting is not active yet: " + reloadErr
	}
	v.Problem = backupProblem(v, ps, now)
	return v
}

// backupProblem is the one plain-words line status shows when something
// needs attention ("" when all is well).
func backupProblem(v *BackupView, ps backup.PushState, now time.Time) string {
	switch {
	case v.Gap != nil && !v.Gap.Closed:
		return fmt.Sprintf("Backups have a gap since %s: changes were not saved because %s. A new full backup is taken as soon as saving works again.", v.Gap.At.Local().Format(time.DateTime), v.Gap.Reason)
	case !ps.FailingSince.IsZero() && now.Sub(ps.FailingSince) > 2*time.Minute:
		return fmt.Sprintf("Saving changes to the backup has failed since %s: %s", ps.FailingSince.Local().Format(time.DateTime), ps.LastError)
	case v.LastError != "":
		return "Backups: " + v.LastError
	case v.Archive.LagMS > int64(10*time.Minute/time.Millisecond):
		return fmt.Sprintf("Backups are behind: %d MB of changes have waited %s to be saved.", v.Archive.WaitingBytes>>20, (time.Duration(v.Archive.LagMS) * time.Millisecond).Round(time.Second))
	}
	return ""
}

// destView says where backups go, and whether a folder shares the data's disk.
func (a *Agent) destView(cfg backup.Config) BackupDestView {
	v := BackupDestView{Kind: cfg.Kind, Label: cfg.Label(), PerNode: !cfg.Shared()}
	if cfg.Kind == backup.KindFolder && cfg.Folder != "" {
		v.SameDisk, _ = backup.SameDisk(cfg.Folder, a.node.StateDir)
	}
	return v
}

func itemOf(b backup.Info, state string) *BackupItem {
	return &BackupItem{ID: b.ID, Kind: b.Kind, State: state, Node: b.Node, StartedAt: b.StartedAt.UTC(),
		FinishedAt: ptrTime(b.FinishedAt), SizeBytes: b.StoredSize, DataBytes: b.DataSize}
}

// ---- HTTP API (dashboard/API.md §13) ----

func (d *dashServer) backupRoutes(mux *http.ServeMux, p string) {
	a := d.a
	// The setup guard answers not_set_up before a node is a member; this
	// covers an agent without a cluster node all the same.
	member := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if a.cn == nil {
				writeErr(w, http.StatusConflict, "not_set_up", "this computer is not in a cluster yet", nil)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET "+p+"/backups", member(a.apiBackups))
	mux.HandleFunc("POST "+p+"/backups", member(a.apiBackupNow))
	mux.HandleFunc("PUT "+p+"/backups/schedule", member(a.apiBackupSchedule))
	mux.HandleFunc("PUT "+p+"/backups/destination", member(a.apiBackupDestination))
	mux.HandleFunc("POST "+p+"/backups/key/reveal", member(a.apiBackupKey))
	mux.HandleFunc("POST "+p+"/backups/check-destination", a.apiBackupCheckDestination) // also before setup (setup.go)
	mux.HandleFunc("POST "+p+"/backups/check", member(a.apiBackupCheckRun))
	mux.HandleFunc("POST "+p+"/backups/restore", member(a.apiBackupRestore))
	mux.HandleFunc("POST "+p+"/backups/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotImplemented, "not_yet_available",
			"A restore never replaces the running database. Restore into a new folder (POST /v1/backups/restore, or `coyote backup restore --to TIME --into FOLDER`), check it, then start it with `coyote up --data-dir FOLDER`.",
			map[string]any{"milestone": "M7"})
	})
}

func (a *Agent) apiBackups(w http.ResponseWriter, r *http.Request) {
	st := a.cn.State()
	bs := st.EffectiveBackup()
	view := a.backupView()
	if view == nil {
		writeErr(w, http.StatusConflict, "not_data_node", "This node holds no data; ask a data node.", nil)
		return
	}
	cfg := a.backupConfig(bs, st)
	if r.URL.Query().Get("refresh") == "1" && bs.Enabled {
		a.refreshCatalog(r.Context(), cfg)
		view = a.backupView()
	}
	out := BackupsView{Schedule: scheduleView(bs), Destination: view.Destination, Status: *view, Backups: []BackupItem{}}
	a.bk.mu.Lock()
	cat := a.bk.cat
	a.bk.mu.Unlock()
	if cat != nil {
		for i := len(cat.Backups) - 1; i >= 0; i-- {
			b := cat.Backups[i]
			s := "done"
			if !cat.Usable(b) {
				s = "incomplete"
			}
			out.Backups = append(out.Backups, *itemOf(b, s))
		}
	}
	if view.Running != nil {
		out.Backups = append([]BackupItem{*view.Running}, out.Backups...)
	}
	out.Check, out.Restore = a.backupJobs()
	control.WriteJSON(w, http.StatusOK, out)
}

// BackupNowResult is POST /v1/backups (202).
type BackupNowResult struct {
	Message string   `json:"message"`
	Nodes   []string `json:"nodes"`
}

type backupNowReq struct{}

func (a *Agent) apiBackupNow(w http.ResponseWriter, r *http.Request) {
	var req backupNowReq
	if r.ContentLength > 0 {
		if err := control.ReadJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
			return
		}
	}
	st := a.cn.State()
	bs := st.EffectiveBackup()
	if !bs.Enabled {
		writeErr(w, http.StatusConflict, "backups_off", "Backups are off. Turn them on with `coyote backup on`.", nil)
		return
	}
	cfg := a.backupConfig(bs, st)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var nodes []string
	ask := func(id string) bool {
		if id == a.cn.ID() {
			if a.role != cluster.RoleData || !a.backupNow() {
				return false
			}
			nodes = append(nodes, a.name)
			return true
		}
		if err := a.call(ctx, id, "backup_now", nil, nil); err != nil {
			return false
		}
		nodes = append(nodes, a.memberName(id))
		return true
	}
	if cfg.Shared() {
		for _, id := range backupOrder(st) {
			if ask(id) {
				break
			}
		}
	} else {
		for _, m := range st.DataMembers() {
			ask(m.ID)
		}
	}
	if len(nodes) == 0 {
		writeErr(w, http.StatusConflict, "no_backup_node", "No data node could start a backup now.", nil)
		return
	}
	control.WriteJSON(w, http.StatusAccepted, BackupNowResult{Message: "Full backup started on " + strings.Join(nodes, ", ") + ".", Nodes: nodes})
}

// BackupScheduleRequest is PUT /v1/backups/schedule; empty fields keep
// their value.
type BackupScheduleRequest struct {
	Enabled        *bool  `json:"enabled,omitempty"`
	Every          string `json:"every,omitempty"`
	At             string `json:"at,omitempty"`
	Keep           string `json:"keep,omitempty"`
	ArchiveTimeout string `json:"archive_timeout,omitempty"`
}

func parseDays(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var d int
		if _, err := fmt.Sscanf(n, "%d", &d); err != nil || d <= 0 {
			return 0, fmt.Errorf("%q: expected a number of days like 7d", s)
		}
		return time.Duration(d) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func (a *Agent) apiBackupSchedule(w http.ResponseWriter, r *http.Request) {
	var req BackupScheduleRequest
	if err := control.ReadJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	bs, err := a.setBackup(ctx, func(bs *cluster.BackupSettings) error {
		if req.Enabled != nil {
			bs.Enabled = *req.Enabled
		}
		for _, f := range []struct {
			s   string
			dst *time.Duration
		}{{req.Every, &bs.Every}, {req.Keep, &bs.Keep}, {req.ArchiveTimeout, &bs.ArchiveTimeout}} {
			if f.s == "" {
				continue
			}
			d, err := parseDays(f.s)
			if err != nil {
				return err
			}
			*f.dst = d
		}
		if req.At != "" {
			bs.At = req.At
		}
		return nil
	})
	if err != nil {
		backupWriteErr(w, err)
		return
	}
	control.WriteJSON(w, http.StatusOK, scheduleView(bs))
}

// BackupDestinationRequest is PUT /v1/backups/destination.
type BackupDestinationRequest struct {
	Kind    string            `json:"kind"`             // "folder" | "s3"
	Folder  string            `json:"folder,omitempty"` // "" = next to each data folder
	S3      *cluster.BackupS3 `json:"s3,omitempty"`
	Encrypt bool              `json:"encrypt,omitempty"` // folder only; S3 is always encrypted
	// Check: try the destination from this node before saving (default on).
	SkipCheck bool `json:"skip_check,omitempty"`
}

// BackupDestinationResult answers it. Key is set only when a new backup
// key was made: shown once, keep it somewhere safe.
type BackupDestinationResult struct {
	Destination BackupDestView `json:"destination"`
	Encrypted   bool           `json:"encrypted"`
	NewKey      string         `json:"new_key,omitempty"`
}

func (a *Agent) apiBackupDestination(w http.ResponseWriter, r *http.Request) {
	var req BackupDestinationRequest
	if err := control.ReadJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
		return
	}
	if req.Folder != "" && !filepath.IsAbs(req.Folder) {
		writeErr(w, http.StatusBadRequest, "bad_request", "the backup folder must be a full path", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	st := a.cn.State()
	old := st.EffectiveBackup()
	next := old
	next.Kind, next.Folder, next.S3, next.Encrypt = req.Kind, req.Folder, req.S3, req.Encrypt
	if next.Kind == cluster.BackupKindS3 && next.S3 != nil && next.S3.SecretKey == "" && old.S3 != nil {
		next.S3.SecretKey = old.S3.SecretKey // unchanged secret
	}
	if !req.SkipCheck && a.role == cluster.RoleData {
		if err := checkDestination(ctx, a.backupConfig(next, st)); err != nil {
			writeErr(w, http.StatusBadRequest, "destination_failed", "That backup destination does not work from "+a.name+": "+err.Error(), nil)
			return
		}
	}
	hadKey := old.Key != ""
	bs, err := a.setBackup(ctx, func(bs *cluster.BackupSettings) error {
		bs.Kind, bs.Folder, bs.S3, bs.Encrypt = next.Kind, next.Folder, next.S3, next.Encrypt
		return nil
	})
	if err != nil {
		backupWriteErr(w, err)
		return
	}
	cfg := a.backupConfig(bs, a.cn.State())
	res := BackupDestinationResult{Destination: a.destView(cfg), Encrypted: cfg.Key != ""}
	if !hadKey && bs.Key != "" {
		res.NewKey = bs.Key
	}
	control.WriteJSON(w, http.StatusOK, res)
}

// BackupKey is POST /v1/backups/key/reveal.
type BackupKey struct {
	Key string `json:"key"`
}

func (a *Agent) apiBackupKey(w http.ResponseWriter, r *http.Request) {
	bs := a.cn.State().EffectiveBackup()
	if bs.Key == "" {
		writeErr(w, http.StatusNotFound, "no_key", "Backups are not encrypted, so there is no backup key.", nil)
		return
	}
	control.WriteJSON(w, http.StatusOK, BackupKey{Key: bs.Key})
}

func backupWriteErr(w http.ResponseWriter, err error) {
	var ce *cluster.Error
	if errors.As(err, &ce) {
		apiErr(w, err)
		return
	}
	writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
}

// rpcBackupNow is the "backup_now" agent RPC.
func (a *Agent) rpcBackupNow(_ context.Context, _ json.RawMessage) (any, error) {
	if a.role != cluster.RoleData || !a.backupNow() {
		return nil, errors.New("this node cannot take a backup now")
	}
	return nil, nil
}
