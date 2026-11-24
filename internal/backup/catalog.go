package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// Info is base/<id>/info.json, written last: a full backup with it is done.
type Info struct {
	ID         string    `json:"id"`
	Node       string    `json:"node"`
	Kind       string    `json:"kind"` // "scheduled" | "manual"
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	StartLSN   string    `json:"start_lsn"`
	EndLSN     string    `json:"end_lsn"`
	Timeline   uint32    `json:"timeline"`
	Parts      int       `json:"parts"`
	StoredSize int64     `json:"stored_bytes"`
	DataSize   int64     `json:"data_bytes"` // the tar stream before compression
	SystemID   string    `json:"system_identifier"`
	Encrypted  bool      `json:"encrypted"`
	PGVersion  string    `json:"pg_version,omitempty"`
}

// Running is base/<id>/running.json, rewritten every few minutes while a
// full backup runs (shared destinations: tells other nodes to wait).
type Running struct {
	ID        string    `json:"id"`
	Node      string    `json:"node"`
	StartedAt time.Time `json:"started_at"`
	Heartbeat time.Time `json:"heartbeat"`
}

// RunningStaleAfter: a running marker older than this is a dead backup.
const RunningStaleAfter = 15 * time.Minute

// Catalog is what a destination holds.
type Catalog struct {
	Backups  []Info               // done, oldest first
	Running  []Running            // in progress (fresh or stale)
	Orphans  []string             // base/<id>/ with parts but no info or marker
	WAL      map[string]WALObject // by name
	Segments map[uint64][]uint32  // segment number -> timelines that have it (full segments)
	MaxSeg   uint64
	// NewestWAL is when the newest WAL object was stored.
	NewestWAL time.Time
}

// WALObject is one archived WAL file.
type WALObject struct {
	WALName
	Size     int64
	Modified time.Time
}

// LoadCatalog lists a destination.
func LoadCatalog(ctx context.Context, st Store) (Catalog, error) {
	c := Catalog{WAL: map[string]WALObject{}, Segments: map[uint64][]uint32{}}
	objs, err := st.List(ctx, "base/")
	if err != nil {
		return c, err
	}
	byID := map[string][]ObjectInfo{}
	for _, o := range objs {
		rest := strings.TrimPrefix(o.Key, "base/")
		id, file, ok := strings.Cut(rest, "/")
		if !ok {
			continue
		}
		o.Key = file
		byID[id] = append(byID[id], o)
	}
	for id, files := range byID {
		has := func(n string) bool {
			return slices.ContainsFunc(files, func(o ObjectInfo) bool { return o.Key == n })
		}
		switch {
		case has("info.json"):
			b, err := st.Get(ctx, "base/"+id+"/info.json")
			if err != nil {
				return c, err
			}
			var in Info
			if err := json.Unmarshal(b, &in); err != nil {
				return c, fmt.Errorf("base/%s/info.json: %w", id, err)
			}
			c.Backups = append(c.Backups, in)
		case has("running.json"):
			b, err := st.Get(ctx, "base/"+id+"/running.json")
			if err != nil && !errors.Is(err, ErrNotFound) {
				return c, err
			}
			var r Running
			if err == nil && json.Unmarshal(b, &r) == nil {
				c.Running = append(c.Running, r)
			} else {
				c.Orphans = append(c.Orphans, id)
			}
		default:
			c.Orphans = append(c.Orphans, id)
		}
	}
	sort.Slice(c.Backups, func(i, j int) bool { return c.Backups[i].FinishedAt.Before(c.Backups[j].FinishedAt) })
	sort.Strings(c.Orphans)
	wal, err := st.List(ctx, "wal/")
	if err != nil {
		return c, err
	}
	for _, o := range wal {
		name := strings.TrimPrefix(o.Key, "wal/")
		wn, ok := ParseWALName(name)
		if !ok {
			continue
		}
		c.WAL[name] = WALObject{WALName: wn, Size: o.Size, Modified: o.Modified}
		if o.Modified.After(c.NewestWAL) {
			c.NewestWAL = o.Modified
		}
		if wn.Kind == "segment" {
			c.Segments[wn.Seg] = append(c.Segments[wn.Seg], wn.TLI)
			if wn.Seg > c.MaxSeg {
				c.MaxSeg = wn.Seg
			}
		}
	}
	return c, nil
}

// Usable reports whether the WAL from a backup's start to its end is all
// archived (on its timeline or a later one), so it can be restored.
func (c Catalog) Usable(in Info) bool {
	s, e, err := in.segments()
	if err != nil {
		return false
	}
	for seg := s; seg <= e; seg++ {
		if !slices.ContainsFunc(c.Segments[seg], func(t uint32) bool { return t >= in.Timeline }) {
			return false
		}
	}
	return true
}

func (in Info) segments() (uint64, uint64, error) {
	s, err := pg.ParseLSN(in.StartLSN)
	if err != nil {
		return 0, 0, err
	}
	e, err := pg.ParseLSN(in.EndLSN)
	if err != nil {
		return 0, 0, err
	}
	end := uint64(e)
	if end > 0 && end%SegmentSize == 0 {
		end-- // an end exactly on a boundary lies in the previous segment
	}
	return SegmentOf(uint64(s)), SegmentOf(end), nil
}

// FirstGap is the first segment after from with no archived copy on any
// timeline, up to the newest archived one (0, false if none).
func (c Catalog) FirstGap(from uint64) (uint64, bool) {
	for seg := from; seg <= c.MaxSeg; seg++ {
		if len(c.Segments[seg]) == 0 {
			return seg, true
		}
	}
	return 0, false
}

// Newest returns the newest usable backup.
func (c Catalog) Newest() (Info, bool) {
	for i := len(c.Backups) - 1; i >= 0; i-- {
		if c.Usable(c.Backups[i]) {
			return c.Backups[i], true
		}
	}
	return Info{}, false
}

// Window is the span a destination can restore to.
type Window struct {
	From, To time.Time // zero when nothing is restorable
	// GapAfter, if set, is the first missing segment after the newest
	// usable backup: To is then only an upper bound.
	GapAfter string
}

// RestoreWindow computes the restorable span.
func (c Catalog) RestoreWindow() Window {
	var w Window
	for _, b := range c.Backups {
		if c.Usable(b) {
			w.From = b.FinishedAt
			break
		}
	}
	nb, ok := c.Newest()
	if !ok {
		return Window{}
	}
	w.To = c.NewestWAL
	if w.To.Before(nb.FinishedAt) {
		w.To = nb.FinishedAt
	}
	s, _, _ := nb.segments()
	if g, ok := c.FirstGap(s); ok {
		w.GapAfter = SegmentFileName(nb.Timeline, g)
	}
	return w
}

// Retention decides what to delete: every done backup older than keep
// except the newest such one (it covers the start of the window), never
// the newest usable backup, and the WAL before the oldest kept backup.
type Retention struct {
	DeleteBackups []string // ids
	DeleteWAL     []string // names
	DeleteOrphans []string // ids of unfinished backups (stale)
	KeepFromSeg   uint64
}

// PlanRetention plans a cleanup at now keeping keep.
func (c Catalog) PlanRetention(now time.Time, keep time.Duration) Retention {
	var r Retention
	cut := now.Add(-keep)
	newest, hasNewest := c.Newest()
	var kept []Info
	oldIdx := -1 // newest backup finished before cut
	for i, b := range c.Backups {
		if b.FinishedAt.Before(cut) {
			oldIdx = i
		}
	}
	for i, b := range c.Backups {
		keepIt := !b.FinishedAt.Before(cut) || i == oldIdx || (hasNewest && b.ID == newest.ID)
		// An unusable backup older than a usable kept one helps no one.
		if keepIt {
			kept = append(kept, b)
		} else {
			r.DeleteBackups = append(r.DeleteBackups, b.ID)
		}
	}
	if len(kept) > 0 {
		from := uint64(1<<63 - 1)
		for _, b := range kept {
			if s, _, err := b.segments(); err == nil && s < from {
				from = s
			}
		}
		r.KeepFromSeg = from
		for name, w := range c.WAL {
			if w.Kind != "history" && w.Seg < from {
				r.DeleteWAL = append(r.DeleteWAL, name)
			}
		}
	}
	sort.Strings(r.DeleteWAL)
	for _, run := range c.Running {
		if now.Sub(run.Heartbeat) > 24*time.Hour {
			r.DeleteOrphans = append(r.DeleteOrphans, run.ID)
		}
	}
	r.DeleteOrphans = append(r.DeleteOrphans, c.Orphans...)
	return r
}

// Apply deletes what a retention plan says. Orphans are deleted only when
// they look abandoned for a day (checked by object times).
func (r Retention) Apply(ctx context.Context, st Store, now time.Time) error {
	var errs []error
	for _, id := range append(append([]string{}, r.DeleteBackups...), r.DeleteOrphans...) {
		objs, err := st.List(ctx, "base/"+id+"/")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fresh := false
		for _, o := range objs {
			if slices.Contains(r.DeleteOrphans, id) && now.Sub(o.Modified) < 24*time.Hour {
				fresh = true
			}
		}
		if fresh {
			continue
		}
		// info.json first: the backup stops being listed as done before
		// its parts go.
		slices.SortFunc(objs, func(a, b ObjectInfo) int {
			ai, bi := strings.HasSuffix(a.Key, "/info.json"), strings.HasSuffix(b.Key, "/info.json")
			switch {
			case ai && !bi:
				return -1
			case bi && !ai:
				return 1
			}
			return strings.Compare(a.Key, b.Key)
		})
		for _, o := range objs {
			if err := st.Delete(ctx, o.Key); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, n := range r.DeleteWAL {
		if err := st.Delete(ctx, "wal/"+n); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
