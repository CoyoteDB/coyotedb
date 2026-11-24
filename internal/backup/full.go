package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// PartSize is how much compressed data goes into one stored part.
const PartSize = 32 << 20

// FullOptions controls TakeFull.
type FullOptions struct {
	Config  Config
	Node    pg.Node
	Install pg.Install
	Guard   *pg.Guard
	Logger  *slog.Logger
	Kind    string // "scheduled" | "manual"
	MaxRate string
	// WaitWAL is how long to wait for the WAL through the backup's end to
	// reach the archive before giving up (default 10 minutes). It bounds
	// both the server's wait inside pg_basebackup and ours after it.
	WaitWAL time.Duration
	Now     func() time.Time
}

// TakeFull takes one full backup of this node's own Postgres into the
// destination. The backup is listed as done (info.json) only once the WAL
// it needs is archived.
func TakeFull(ctx context.Context, o FullOptions) (Info, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.WaitWAL == 0 {
		o.WaitWAL = 10 * time.Minute
	}
	st, err := o.Config.Store()
	if err != nil {
		return Info{}, err
	}
	codec, err := o.Config.Codec()
	if err != nil {
		return Info{}, err
	}
	sysid, err := systemID(ctx, o.Node)
	if err != nil {
		return Info{}, err
	}
	if _, err := EnsureIdentity(ctx, st, codec, o.Config.ClusterID, sysid); err != nil {
		return Info{}, err
	}
	start := o.Now().UTC()
	id := start.Format("20060102T150405Z") + "-" + safeName(o.Config.Node)
	prefix := "base/" + id + "/"
	run := Running{ID: id, Node: o.Config.Node, StartedAt: start, Heartbeat: start}
	putRunning := func() error {
		run.Heartbeat = o.Now().UTC()
		b, _ := json.Marshal(run)
		return st.Put(ctx, prefix+"running.json", b, false)
	}
	if err := putRunning(); err != nil {
		return Info{}, err
	}
	hbCtx, stopHB := context.WithCancel(ctx)
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		t := time.NewTicker(3 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				_ = putRunning()
			}
		}
	}()
	defer func() { stopHB(); hbWG.Wait() }()

	pw := &partWriter{ctx: ctx, st: st, codec: codec, prefix: prefix}
	zw, _ := gzip.NewWriterLevel(pw, gzip.BestSpeed)
	counter := &countWriter{w: zw}
	res, err := o.Node.BaseBackupStream(ctx, pg.BaseBackupOptions{
		Install: o.Install, Guard: o.Guard, Logger: o.Logger, MaxRate: o.MaxRate,
		ArchiveWait: o.WaitWAL, // the server's own wait for the archive, bounded the same
	}, counter)
	if err == nil {
		err = zw.Close()
	}
	if err == nil {
		err = pw.flush()
	}
	if err == nil {
		err = pw.err
	}
	if err != nil {
		cleanup(st, prefix, pw.parts)
		return Info{}, fmt.Errorf("full backup: %w", err)
	}
	info := Info{
		ID: id, Node: o.Config.Node, Kind: o.Kind, StartedAt: start,
		StartLSN: res.StartLSN.String(), EndLSN: res.EndLSN.String(), Timeline: res.Timeline,
		Parts: pw.parts, StoredSize: pw.stored, DataSize: counter.n, SystemID: sysid,
		Encrypted: len(codec.Key) > 0, PGVersion: o.Install.Build.Version,
	}
	// Wait for the WAL through the end to be archived (this node pushes
	// it once the segment is complete; the main node's health record and
	// archive_timeout complete it within about a minute).
	deadline := o.Now().Add(o.WaitWAL)
	for {
		cat, err := LoadCatalog(ctx, st)
		if err == nil && cat.Usable(info) {
			break
		}
		if err == nil {
			backfill(ctx, o, cat, info)
		}
		if o.Now().After(deadline) {
			cleanup(st, prefix, pw.parts)
			if err != nil {
				return Info{}, fmt.Errorf("full backup: could not list the destination: %w", err)
			}
			return Info{}, errors.New("full backup: the WAL it needs did not reach the backup destination in time (is WAL archiving failing? see status)")
		}
		select {
		case <-ctx.Done():
			cleanup(st, prefix, pw.parts)
			return Info{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	info.FinishedAt = o.Now().UTC()
	b, _ := json.MarshalIndent(info, "", "  ")
	if err := st.Put(ctx, prefix+"info.json", b, false); err != nil {
		return Info{}, err
	}
	_ = st.Delete(ctx, prefix+"running.json")
	return info, nil
}

// backfill saves segments the backup needs that this node holds complete
// in its own pg_wal but never archived: WAL a copy received while it was
// cloned (pg_basebackup marks those as done) is the usual case, since a
// copy's backup starts at its last restartpoint, which can be the clone's.
// Push dedupes and never replaces another node's segment from a copy.
func backfill(ctx context.Context, o FullOptions, cat Catalog, in Info) {
	s, e, err := in.segments()
	if err != nil {
		return
	}
	walDir := filepath.Join(o.Node.PGData(), "pg_wal")
	for seg := s; seg <= e; seg++ {
		if slices.ContainsFunc(cat.Segments[seg], func(t uint32) bool { return t >= in.Timeline }) {
			continue
		}
		name := SegmentFileName(in.Timeline, seg)
		if _, err := os.Stat(filepath.Join(walDir, "archive_status", name+".done")); err != nil {
			continue // not complete here, or Postgres archives it itself (.ready)
		}
		r, err := Push(ctx, o.Config, PushOptions{StateDir: o.Node.StateDir, Path: filepath.Join(walDir, name), Name: name})
		if err != nil {
			o.Logger.Warn("backup: could not save a WAL segment the backup needs", "segment", name, "err", err)
			continue
		}
		o.Logger.Info("backup: saved a WAL segment from pg_wal that was never archived", "segment", name, "stored", r.Stored)
	}
}

func cleanup(st Store, prefix string, parts int) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i := range parts {
		_ = st.Delete(ctx, prefix+partName(i))
	}
	_ = st.Delete(ctx, prefix+"running.json")
}

func partName(i int) string { return fmt.Sprintf("part-%05d", i) }

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "node"
	}
	return b.String()
}

func systemID(ctx context.Context, n pg.Node) (string, error) {
	c, err := n.Connect(ctx)
	if err != nil {
		return "", err
	}
	defer c.Close(context.Background())
	var id string
	err = c.QueryRow(ctx, "SELECT system_identifier::text FROM pg_control_system()").Scan(&id)
	return id, err
}

// partWriter cuts a stream into sealed parts.
type partWriter struct {
	ctx    context.Context
	st     Store
	codec  Codec
	prefix string
	buf    bytes.Buffer
	parts  int
	stored int64
	err    error
}

func (p *partWriter) Write(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	p.buf.Write(b)
	for p.buf.Len() >= PartSize {
		chunk := make([]byte, PartSize)
		copy(chunk, p.buf.Next(PartSize))
		if err := p.put(chunk); err != nil {
			p.err = err
			return 0, err
		}
	}
	return len(b), nil
}

func (p *partWriter) flush() error {
	if p.err != nil {
		return p.err
	}
	if p.buf.Len() == 0 && p.parts > 0 {
		return nil
	}
	err := p.put(append([]byte{}, p.buf.Bytes()...))
	p.buf.Reset()
	return err
}

func (p *partWriter) put(chunk []byte) error {
	key := p.prefix + partName(p.parts)
	obj, err := p.codec.Seal(chunk, key, false)
	if err != nil {
		return err
	}
	if err := p.st.Put(p.ctx, key, obj, false); err != nil {
		return err
	}
	p.parts++
	p.stored += int64(len(obj))
	return nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

// partsReader streams a full backup's parts back in order.
type partsReader struct {
	ctx    context.Context
	st     Store
	codec  Codec
	prefix string
	n, i   int
	cur    *bytes.Reader
}

func (r *partsReader) Read(b []byte) (int, error) {
	for r.cur == nil || r.cur.Len() == 0 {
		if r.i >= r.n {
			return 0, io.EOF
		}
		key := r.prefix + partName(r.i)
		obj, err := r.st.Get(r.ctx, key)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", key, err)
		}
		raw, err := r.codec.Open(obj, key)
		if err != nil {
			return 0, err
		}
		r.cur = bytes.NewReader(raw)
		r.i++
	}
	return r.cur.Read(b)
}
