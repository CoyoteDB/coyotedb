package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/pg"
)

// Page health (D55): a copy whose data pages fail their checksum must not
// become the main node, and is copied fresh from the main node while the
// main node is still there.
//
// Every data node, every pageTick:
//   - reads Postgres' own checksum counters (pg_stat_database), and
//   - on a copy, runs the page check (pg.ScanPages): a paced, resumable
//     read of every page, a new pass after every Postgres start (damage is
//     most often found after a crash or a power cut) and every
//     pageRescanEvery after the last full pass.
//
// Damage found on a copy is remembered in <data-dir>/pagecheck.json (the
// counters reset on a crash), and a marker file is put in the copy's
// Postgres data folder (pageMarker). The record counts only while the
// marker is there: a re-clone replaces the data folder, which removes
// the marker, and so ends the record. A main node removes the marker
// from its own folder, so a copy cloned from it never inherits one. The node
// reports it in its status; the controller never promotes it (D55) and
// never adds it back to the sync set; and the copy retires itself the way
// a drifted copy does (rules 2 and 3: sync_remove through the main node,
// then "needs re-clone" once it is out of the sync set), which re-clones
// it from the main node (rule 8 path). A damaged main node is reported
// (status, log); it is not stopped: it is still the best copy there is.

const (
	pageTick        = 2 * time.Second
	pageRescanEvery = 24 * time.Hour
	pageRetireEvery = 5 * time.Second
	pageRetryAfter  = 30 * time.Second
	pageFileFmt     = 1
	// pageMarker is the file in the Postgres data folder that ties a
	// damage record to the data it was found in.
	pageMarker = "coyote_page_damage"
)

// pageRecord is <data-dir>/pagecheck.json.
type pageRecord struct {
	Format int `json:"format"`
	// Damage found on this node's current data, in plain words; kept
	// until the data is replaced by a re-clone.
	Damage   []string  `json:"damage,omitempty"`
	DamageAt time.Time `json:"damage_at,omitzero"`
	// Progress is the current (or last) page check pass.
	Progress *pg.PageScanProgress `json:"progress,omitempty"`
	// LastFullPass is when the last pass that read everything finished,
	// and PassStart the server start it ran on.
	LastFullPass time.Time `json:"last_full_pass,omitzero"`
	PassStart    string    `json:"pass_postmaster_start,omitempty"`
}

// pageHealth is the agent's page health state.
type pageHealth struct {
	mu   sync.Mutex
	path string
	rec  pageRecord
	// Live readings (this round).
	failures       int64
	lastFailure    string
	loggedFailures int64  // the count last logged (main node)
	start          string // pg_postmaster_start_time of the running server
	role           string
	// The running pass, if any.
	scanning   bool
	scanStop   context.CancelFunc
	scanDone   chan struct{}
	live       *pg.PageScanProgress
	lastRetire time.Time
	retireNote string
	// retryAt: a pass that could not start or stopped on an error on the
	// server that started at retryStart is tried again from then on (not
	// every tick); a restarted server gets its pass at once.
	retryAt    time.Time
	retryStart string
}

// PageReport is a data node's page health in its status.
type PageReport struct {
	// Damaged: this node's data has pages that fail their checksum (or the
	// page check found damage). A damaged copy is never made the main node.
	Damaged  bool     `json:"damaged"`
	Problems []string `json:"problems,omitempty"`
	// ChecksumFailures is Postgres' own count since its statistics were
	// last reset (a crash resets them).
	ChecksumFailures int64  `json:"checksum_failures"`
	LastFailure      string `json:"checksum_last_failure,omitempty"`
	// Scan is the page check in plain words; ScanPercent how far the
	// current pass got.
	Scan        string `json:"scan,omitempty"`
	ScanPercent int    `json:"scan_percent,omitempty"`
	// CheckedSinceStart: a full pass finished on the running server.
	CheckedSinceStart bool      `json:"checked_since_start"`
	LastFullPass      time.Time `json:"last_full_pass,omitzero"`
	// Action is what the node does about damage ("" when none).
	Action string `json:"action,omitempty"`
}

func (h *pageHealth) load(path string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.path = path
	h.rec = pageRecord{Format: pageFileFmt}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var r pageRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if r.Format != pageFileFmt {
		return fmt.Errorf("%s: unknown format %d (made by a newer version?)", path, r.Format)
	}
	h.rec = r
	return nil
}

// saveLocked writes the record durably (h.mu held).
func (h *pageHealth) saveLocked() error {
	if h.path == "" {
		return nil
	}
	h.rec.Format = pageFileFmt
	b, err := json.MarshalIndent(h.rec, "", "  ")
	if err != nil {
		return err
	}
	if err := pg.WriteFileDurable(h.path, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", h.path, err)
	}
	return nil
}

func (h *pageHealth) update(f func(r *pageRecord)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(&h.rec)
	return h.saveLocked()
}

// damaged reports whether this node's data is known to be damaged.
func (h *pageHealth) damaged() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rec.Damage) > 0
}

// report is the status section.
func (h *pageHealth) report() *PageReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.path == "" {
		return nil
	}
	r := &PageReport{ChecksumFailures: h.failures, LastFailure: h.lastFailure, LastFullPass: h.rec.LastFullPass, Action: h.retireNote}
	r.Problems = slices.Clone(h.rec.Damage)
	if h.failures > 0 && h.role == "primary" {
		r.Problems = append(r.Problems, failuresProblem(h.failures, h.lastFailure))
	}
	r.Damaged = len(r.Problems) > 0
	r.CheckedSinceStart = h.start != "" && h.rec.PassStart == h.start && !h.rec.LastFullPass.IsZero()
	p := h.rec.Progress
	if h.live != nil {
		p = h.live
	}
	switch {
	case h.role == "primary":
		r.Scan = "not run on the main node (its checksum counters are watched)"
	case h.scanning && p != nil:
		r.Scan, r.ScanPercent = fmt.Sprintf("running, about %d%%", p.Percent()), p.Percent()
	case r.CheckedSinceStart:
		r.Scan = "every page read since this database started"
	case len(h.rec.Damage) > 0:
		r.Scan = "stopped: damage found"
	default:
		r.Scan = "waiting"
	}
	return r
}

func failuresProblem(n int64, last string) string {
	s := fmt.Sprintf("%d data page(s) failed their checksum when read", n)
	if last != "" {
		s += " (last at " + last + ")"
	}
	return s + ": the disk returned damaged data"
}

// pageLoop runs on data nodes for the agent's lifetime.
func (a *Agent) pageLoop(ctx context.Context) {
	if err := a.pages.load(filepath.Join(a.node.StateDir, "pagecheck.json")); err != nil {
		a.log.Error("page check: cannot read its record; the page check is off", "err", err)
		return
	}
	defer a.stopPageScan()
	t := time.NewTicker(pageTick)
	defer t.Stop()
	for {
		a.pageStep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *Agent) stopPageScan() {
	h := &a.pages
	h.mu.Lock()
	stop, done := h.scanStop, h.scanDone
	h.mu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

// pageStep is one round of the page health loop.
func (a *Agent) pageStep(ctx context.Context) {
	if a.cn == nil {
		return
	}
	h := &a.pages
	st := a.cn.State()
	self := a.cn.ID()
	me, ok := st.Members[self]
	if !ok || me.Role != cluster.RoleData {
		return
	}
	srv, role := a.currentPG()
	if srv == nil {
		a.stopPageScan()
		h.mu.Lock()
		h.failures, h.lastFailure, h.start, h.role = 0, "", "", ""
		h.mu.Unlock()
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	n, last, start, err := a.readPageCounters(cctx)
	cancel()
	if err != nil {
		a.log.Debug("page check: cannot read the checksum counters", "err", err)
		return
	}
	h.mu.Lock()
	h.failures, h.lastFailure, h.role = n, last, role
	if h.start != start {
		h.start = start
	}
	h.mu.Unlock()

	if role != "standby" {
		a.stopPageScan()
		// A main node's data folder is what copies are cloned from: no
		// marker in it (its own damage stays in the record and is reported).
		if err := os.Remove(a.pageMarkerPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.log.Warn("page check: could not remove the damage marker", "err", err)
		}
		h.mu.Lock()
		changed := n != h.loggedFailures
		h.loggedFailures = n
		h.mu.Unlock()
		if n > 0 && changed {
			a.log.Error("page check: this main node's data has damaged pages; check its disk and switch to a healthy copy", "checksum_failures", n, "last", last)
		}
		return
	}
	a.pageDataReplaced()
	if n > 0 && !h.damaged() {
		why := failuresProblem(n, last)
		if err := a.pageDamage(why); err != nil {
			a.log.Warn("page check: could not record damage", "err", err)
		}
	}
	if h.damaged() {
		a.stopPageScan()
		a.retireDamaged(ctx, st)
		return
	}
	if me.Status != cluster.StatusActive {
		a.stopPageScan()
		return
	}
	a.maybeStartPageScan(ctx, start)
}

// pageMarkerPath is the damage marker in this node's data folder.
func (a *Agent) pageMarkerPath() string { return filepath.Join(a.node.PGData(), pageMarker) }

// pageDataReplaced clears the damage record once the data it was found in
// is gone (a re-clone replaced the data folder, so the marker is gone).
// Run while this node's Postgres runs as a copy.
func (a *Agent) pageDataReplaced() {
	if !a.pages.damaged() {
		return
	}
	if _, err := os.Stat(a.pageMarkerPath()); !errors.Is(err, fs.ErrNotExist) {
		return
	}
	a.stopPageScan()
	if err := a.pages.update(func(r *pageRecord) { *r = pageRecord{Format: pageFileFmt} }); err != nil {
		a.log.Warn("page check: could not clear the damage record", "err", err)
		return
	}
	a.pages.mu.Lock()
	a.pages.retireNote = ""
	a.pages.mu.Unlock()
	a.log.Warn("page check: this copy's data was replaced (copied fresh); the damage record is cleared and a new pass starts")
	a.say("This copy's data was copied fresh from the main node; the damaged pages are gone.")
}

func (a *Agent) readPageCounters(ctx context.Context) (int64, string, string, error) {
	c, err := a.node.Connect(ctx)
	if err != nil {
		return 0, "", "", err
	}
	defer c.Close(context.WithoutCancel(ctx))
	n, last, err := pg.ChecksumFailures(ctx, c)
	if err != nil {
		return 0, "", "", err
	}
	start, err := pg.PostmasterStart(ctx, c)
	return n, last, start, err
}

// pageDamage records damage on this copy's data (kept until re-cloned):
// the marker first, then the record (a record without its marker would be
// cleared at once; a marker without a record means nothing).
func (a *Agent) pageDamage(why string) error {
	if err := pg.WriteFileDurable(a.pageMarkerPath(), []byte(why+"\n"), 0o600); err != nil {
		return fmt.Errorf("write the damage marker: %w", err)
	}
	a.log.Error("page check: this copy's data is damaged; it will not be made the main node and is copied fresh from the main node", "damage", why)
	a.say("This copy's data has damaged pages (%s). It will not be made the main node, and it is copied fresh from the main node.", why)
	return a.pages.update(func(r *pageRecord) {
		if !slices.Contains(r.Damage, why) && len(r.Damage) < 8 {
			r.Damage = append(r.Damage, why)
		}
		if r.DamageAt.IsZero() {
			r.DamageAt = time.Now().UTC()
		}
	})
}

// maybeStartPageScan starts or resumes a pass when one is due.
func (a *Agent) maybeStartPageScan(ctx context.Context, start string) {
	h := &a.pages
	h.mu.Lock()
	if h.scanning || (h.retryStart == start && time.Now().Before(h.retryAt)) {
		h.mu.Unlock()
		return
	}
	var resume *pg.PageScanProgress
	if p := h.rec.Progress; p != nil && p.PostmasterStart == start && p.Done.IsZero() && len(p.Damage) == 0 {
		c := *p
		resume = &c
	}
	due := resume != nil || h.rec.PassStart != start || time.Since(h.rec.LastFullPass) >= pageRescanEvery
	if !due {
		h.mu.Unlock()
		return
	}
	sctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	h.scanning, h.scanStop, h.scanDone = true, stop, done
	h.mu.Unlock()
	go func() {
		defer close(done)
		defer stop()
		a.runPageScan(sctx, start, resume)
		h.mu.Lock()
		h.scanning, h.scanStop, h.live = false, nil, nil
		h.mu.Unlock()
	}()
}

func (a *Agent) runPageScan(ctx context.Context, start string, resume *pg.PageScanProgress) {
	h := &a.pages
	backoff := func() {
		if ctx.Err() == nil {
			h.mu.Lock()
			h.retryAt, h.retryStart = time.Now().Add(pageRetryAfter), start
			h.mu.Unlock()
		}
	}
	var p pg.PageScanProgress
	if resume != nil {
		p = *resume
		a.log.Info("page check: resuming the pass", "percent", p.Percent())
	} else {
		var err error
		if p, err = pg.NewPageScan(ctx, a.node.ConnectDB, time.Now()); err != nil {
			a.log.Info("page check: could not start a pass; will retry", "err", err)
			backoff()
			return
		}
		if p.PostmasterStart != start {
			return // restarted meanwhile; the next round starts over
		}
		a.log.Info("page check: reading every page of this copy", "bytes", p.TotalBytes)
	}
	save := func(p pg.PageScanProgress) {
		c := p
		h.mu.Lock()
		h.live = &c
		h.rec.Progress = &c
		err := h.saveLocked()
		h.mu.Unlock()
		if err != nil {
			a.log.Warn("page check: could not save progress", "err", err)
		}
	}
	save(p)
	p, err := pg.ScanPages(ctx, a.node.ConnectDB, pg.PageScanOptions{Save: save}, p)
	if len(p.Damage) > 0 {
		for _, d := range p.Damage {
			if derr := a.pageDamage(d); derr != nil {
				a.log.Warn("page check: could not record damage", "err", derr)
			}
		}
		return
	}
	if err != nil {
		if ctx.Err() == nil {
			a.log.Info("page check: pass interrupted; it resumes", "err", err, "percent", p.Percent())
		}
		backoff()
		return
	}
	h.mu.Lock()
	h.rec.LastFullPass, h.rec.PassStart = p.Done, p.PostmasterStart
	serr := h.saveLocked()
	h.mu.Unlock()
	if serr != nil {
		a.log.Warn("page check: could not save progress", "err", serr)
	}
	a.log.Info("page check: every page read, no damage", "relations", p.Relations, "bytes", p.BytesRead,
		"took", p.Done.Sub(p.Started).Round(time.Second), "skipped", p.Skipped, "no_amcheck", strings.Join(p.NoAmcheck, ","))
}

// retireDamaged takes a damaged copy out of failover and gets it copied
// fresh: like retireDrifted (rules 2 and 3), it asks the main node to
// remove it from the sync set (S first, then R), and once it is out of R
// marks itself "needs re-clone" (never while in R: that would shrink the
// rule 2 count). The re-clone then replaces its data (rule 8 path).
//
// In a strict cluster where it is the only copy in the sync set it stays:
// taking it out would let writes go on with no copy, which strict forbids;
// it keeps receiving every write, the controller never promotes it, and
// status says to replace it.
func (a *Agent) retireDamaged(ctx context.Context, st cluster.State) {
	h := &a.pages
	h.mu.Lock()
	if time.Since(h.lastRetire) < pageRetireEvery {
		h.mu.Unlock()
		return
	}
	h.lastRetire = time.Now()
	h.mu.Unlock()
	note := func(s string) {
		h.mu.Lock()
		h.retireNote = s
		h.mu.Unlock()
	}
	self := a.cn.ID()
	me := st.Members[self]
	if st.PrimaryID == "" || st.PrimaryID == self {
		note("waiting for a main node to copy from")
		return
	}
	if slices.Contains(st.Sync.Members, self) {
		others := 0
		for _, id := range st.Sync.Members {
			if id != self && id != st.PrimaryID {
				others++
			}
		}
		if st.Settings.SingleDataNodeMode == cluster.ModeStrict && others == 0 {
			note("kept as the only copy (strict): it is never made the main node; replace it (remove this node and join it again)")
			return
		}
		cctx, cancel := context.WithTimeout(ctx, time.Minute)
		err := a.call(cctx, st.PrimaryID, "sync_remove", syncNodeReq{Node: self}, nil)
		cancel()
		if err != nil {
			note("leaving the sync set (retrying)")
			a.log.Info("page check: could not leave the sync set yet; will retry", "err", err)
			return
		}
		note("left the sync set; copying fresh from the main node next")
		a.log.Warn("page check: left the sync set (not a failover candidate any more)")
		return
	}
	if me.Status == cluster.StatusActive {
		s := cluster.StatusNeedsReclone
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if _, err := a.cn.Apply(cctx, &cluster.UpdateMember{ID: self, Status: &s}); err != nil {
			note("marking for a fresh copy (retrying)")
			a.log.Info("page check: could not mark this copy for a fresh copy yet; will retry", "err", err)
			return
		}
		note("copying fresh from the main node")
		a.log.Warn("page check: marked for a fresh copy from the main node")
		a.kickLoop()
	}
}
