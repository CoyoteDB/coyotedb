package backup

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/durable"
)

// SegmentSize is Postgres' WAL segment size (the initdb default we use).
const SegmentSize = 16 << 20

// Limits that keep a failing destination from filling the disk (D63).
const (
	MaxBacklogBytes = 2 << 30 // WAL waiting on this node
	MinFreeBytes    = 1 << 30 // free space on the data disk
)

// WALName is a parsed WAL archive file name.
type WALName struct {
	Name     string
	TLI      uint32
	Seg      uint64 // log*256 + seg (16 MiB segments)
	Kind     string // "segment", "history", "partial", "backup"
	Original string
}

// ParseWALName parses a file name Postgres archives.
func ParseWALName(name string) (WALName, bool) {
	w := WALName{Name: name}
	base := name
	switch {
	case strings.HasSuffix(name, ".history") && len(name) == 16:
		t, err := strconv.ParseUint(name[:8], 16, 32)
		if err != nil {
			return w, false
		}
		w.TLI, w.Kind = uint32(t), "history"
		return w, true
	case strings.HasSuffix(name, ".partial"):
		base, w.Kind = strings.TrimSuffix(name, ".partial"), "partial"
	case strings.HasSuffix(name, ".backup") && len(name) > 24:
		base, w.Kind = name[:24], "backup"
	default:
		w.Kind = "segment"
	}
	if len(base) != 24 {
		return w, false
	}
	t, err1 := strconv.ParseUint(base[:8], 16, 32)
	l, err2 := strconv.ParseUint(base[8:16], 16, 32)
	s, err3 := strconv.ParseUint(base[16:], 16, 32)
	if err1 != nil || err2 != nil || err3 != nil || s >= 256 {
		return w, false
	}
	w.TLI, w.Seg = uint32(t), l*256+s
	return w, true
}

// SegmentFileName is the file name of segment seg on timeline tli.
func SegmentFileName(tli uint32, seg uint64) string {
	return fmt.Sprintf("%08X%08X%08X", tli, seg/256, seg%256)
}

// SegmentOf is the segment number holding lsn.
func SegmentOf(lsn uint64) uint64 { return lsn / SegmentSize }

// SegmentSystemID reads the database system identifier from a WAL
// segment's first (long) page header: xlp_magic(2) xlp_info(2) xlp_tli(4)
// xlp_pageaddr(8) xlp_rem_len(4) padding(4) xlp_sysid(8).
func SegmentSystemID(seg []byte) (string, error) {
	if len(seg) < 32 {
		return "", errors.New("WAL segment too short")
	}
	const xlpLongHeader = 0x0002
	if binary.LittleEndian.Uint16(seg[2:4])&xlpLongHeader == 0 {
		return "", errors.New("WAL segment has no long page header")
	}
	return strconv.FormatUint(binary.LittleEndian.Uint64(seg[24:32]), 10), nil
}

// ---- local bookkeeping (written by wal-push, read by the agent) ----

// PushState is <data-dir>/backup/push.json.
type PushState struct {
	LastName      string    `json:"last_name,omitempty"`
	LastAt        time.Time `json:"last_at,omitzero"`
	FailingSince  time.Time `json:"failing_since,omitzero"`
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitzero"`
	Conflicts     int       `json:"conflicts,omitempty"`
	GapFrom       string    `json:"gap_from,omitempty"` // first segment given up on
	GapTo         string    `json:"gap_to,omitempty"`   // last one
	GapAt         time.Time `json:"gap_at,omitzero"`
	GapReason     string    `json:"gap_reason,omitempty"`
	GapsTotal     int       `json:"gaps_total,omitempty"`
	GapClosedByID string    `json:"gap_closed_by,omitempty"` // full backup taken after the gap
}

// PushStatePath is where PushState lives.
func PushStatePath(stateDir string) string { return filepath.Join(StateDir(stateDir), "push.json") }

// ReadPushState reads push.json ({} if missing).
func ReadPushState(stateDir string) (PushState, error) {
	var s PushState
	b, err := os.ReadFile(PushStatePath(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// UpdatePushState applies f to push.json. Only one wal-push runs at a time
// per node (Postgres' archiver is serial); the agent only clears the gap.
func UpdatePushState(stateDir string, f func(*PushState)) error {
	s, _ := ReadPushState(stateDir)
	f(&s)
	if err := os.MkdirAll(StateDir(stateDir), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return durable.WriteFile(PushStatePath(stateDir), b, 0o600)
}

// ---- push ----

// PushOptions are the inputs of one archive_command run.
type PushOptions struct {
	StateDir string // the node's data folder (parent of pgdata)
	Path     string // %p: the file, relative to pgdata or absolute
	Name     string // %f
	// Primary: this node's Postgres is a primary (no standby.signal); it
	// may replace a stored file that differs (D63).
	Primary bool
	// Backlog and DiskFree measure the node (WAL waiting, free bytes).
	Backlog  func() int64
	DiskFree func() (uint64, error)
	Now      func() time.Time
}

// PushResult says what happened.
type PushResult struct {
	Stored   bool   // uploaded (false: already there, or given up)
	Conflict bool   // a different file had this name
	GaveUp   bool   // failed; recorded as a gap to protect the disk
	Note     string // for the log
}

// Push archives one file. An error means Postgres should retry later.
func Push(ctx context.Context, cfg Config, o PushOptions) (PushResult, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	res, err := push(ctx, cfg, o)
	now := o.Now().UTC()
	if err == nil {
		_ = UpdatePushState(o.StateDir, func(s *PushState) {
			s.LastName, s.LastAt, s.FailingSince = o.Name, now, time.Time{}
			if res.Conflict {
				s.Conflicts++
			}
		})
		return res, nil
	}
	// Failed. Protect the disk: past the limits, give this file up.
	reason := ""
	if o.Backlog != nil {
		if b := o.Backlog(); b > MaxBacklogBytes {
			reason = fmt.Sprintf("%d MB of WAL were waiting", b>>20)
		}
	}
	if o.DiskFree != nil {
		if f, ferr := o.DiskFree(); ferr == nil && f < MinFreeBytes {
			reason = fmt.Sprintf("only %d MB free on the disk", f>>20)
		}
	}
	_ = UpdatePushState(o.StateDir, func(s *PushState) {
		if s.FailingSince.IsZero() {
			s.FailingSince = now
		}
		s.LastError, s.LastErrorAt = err.Error(), now
		if reason != "" {
			if s.GapFrom == "" || s.GapClosedByID != "" {
				s.GapFrom, s.GapClosedByID = o.Name, ""
				s.GapsTotal++
			}
			s.GapTo, s.GapAt, s.GapReason = o.Name, now, reason
		}
	})
	if reason != "" {
		return PushResult{GaveUp: true, Note: "could not save " + o.Name + " (" + err.Error() + "); given up because " + reason}, nil
	}
	return res, err
}

func push(ctx context.Context, cfg Config, o PushOptions) (PushResult, error) {
	p := o.Path
	if !filepath.IsAbs(p) {
		p = filepath.Join(o.StateDir, "pgdata", p)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return PushResult{}, err
	}
	wn, ok := ParseWALName(o.Name)
	if !ok {
		return PushResult{}, fmt.Errorf("not a WAL file name: %q", o.Name)
	}
	st, err := cfg.Store()
	if err != nil {
		return PushResult{}, err
	}
	codec, err := cfg.Codec()
	if err != nil {
		return PushResult{}, err
	}
	sysid := ""
	if wn.Kind == "segment" || wn.Kind == "partial" {
		if sysid, err = SegmentSystemID(raw); err != nil {
			return PushResult{}, fmt.Errorf("%s: %w", o.Name, err)
		}
	}
	if err := identityOnce(ctx, o.StateDir, st, codec, cfg, sysid); err != nil {
		return PushResult{}, err
	}
	return storeWAL(ctx, st, codec, o, raw)
}

// identityOnce checks the archive identity, remembering a good answer in
// <data-dir>/backup/identity.ok so most pushes cost one request.
func identityOnce(ctx context.Context, stateDir string, st Store, c Codec, cfg Config, sysid string) error {
	okFile := filepath.Join(StateDir(stateDir), "identity.ok")
	want := st.Label() + "\n" + cfg.ClusterID + "\n" + sysid + "\n" + KeyID(c.Key)
	if sysid != "" {
		if b, err := os.ReadFile(okFile); err == nil && string(b) == want {
			return nil
		}
	}
	if _, err := EnsureIdentity(ctx, st, c, cfg.ClusterID, sysid); err != nil {
		return err
	}
	if sysid != "" {
		_ = os.MkdirAll(StateDir(stateDir), 0o700)
		_ = durable.WriteFile(okFile, []byte(want), 0o600)
	}
	return nil
}

func storeWAL(ctx context.Context, st Store, codec Codec, o PushOptions, raw []byte) (PushResult, error) {
	key := "wal/" + o.Name
	cid := codec.ContentID(raw)
	plainCID := Codec{}.ContentID(raw)
	same := func(h Header) bool { return h.CID == cid || (!h.Encrypted && h.CID == plainCID) }
	var sealed []byte
	for range 3 {
		head, err := st.GetPrefix(ctx, key, HeaderSize)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return PushResult{}, err
		}
		if err == nil {
			h, herr := ParseHeader(head)
			if herr == nil && same(h) {
				return PushResult{Note: o.Name + " was already saved"}, nil
			}
			// Same name, different content: only a dead branch can do
			// this (D63). Keep both versions.
			if sealed == nil {
				if sealed, err = codec.Seal(raw, key, true); err != nil {
					return PushResult{}, err
				}
			}
			stamp := o.Now().UTC().Format("20060102T150405Z")
			if !o.Primary {
				ck := "conflicts/" + o.Name + "." + stamp + ".copy"
				csealed, err := codec.Seal(raw, ck, true)
				if err != nil {
					return PushResult{}, err
				}
				if err := st.Put(ctx, ck, csealed, false); err != nil {
					return PushResult{}, err
				}
				return PushResult{Conflict: true, Note: o.Name + " differs from the saved one; kept as " + ck + " (this node is a copy)"}, nil
			}
			old, err := st.Get(ctx, key)
			if err != nil {
				return PushResult{}, err
			}
			ck := "conflicts/" + o.Name + "." + stamp + ".replaced"
			if err := st.Put(ctx, ck, old, false); err != nil {
				return PushResult{}, err
			}
			// The kept copy is stored as it was: it is sealed with its
			// original key name (wal/NAME) as associated data, so reading
			// it means opening it as that name. Kept for people, not read
			// by restores.
			if err := st.Put(ctx, key, sealed, false); err != nil {
				return PushResult{}, err
			}
			return PushResult{Stored: true, Conflict: true, Note: o.Name + " replaced a different saved version (kept as " + ck + ")"}, nil
		}
		if sealed == nil {
			if sealed, err = codec.Seal(raw, key, true); err != nil {
				return PushResult{}, err
			}
		}
		err = st.Put(ctx, key, sealed, true)
		if errors.Is(err, ErrExists) {
			continue // someone else stored it meanwhile: compare
		}
		if err != nil {
			return PushResult{}, err
		}
		return PushResult{Stored: true}, nil
	}
	return PushResult{}, errors.New("could not store " + o.Name + " (it kept changing)")
}

// ---- fetch (restore_command) ----

// Fetch writes the archived file name to dest. ErrNotFound means "not in
// the archive" (Postgres then takes it as the end of the archive).
func Fetch(ctx context.Context, st Store, codec Codec, name, dest string) error {
	if _, ok := ParseWALName(name); !ok {
		return ErrNotFound // e.g. a .partial or unknown name: not archived by us
	}
	key := "wal/" + name
	obj, err := st.Get(ctx, key)
	if err != nil {
		return err
	}
	raw, err := codec.Open(obj, key)
	if err != nil {
		return err
	}
	tmp := dest + ".coyote-tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}
