package backup

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSealOpen(t *testing.T) {
	raw := bytes.Repeat([]byte("wal record "), 5000)
	key, _ := NewKey()
	for _, c := range []Codec{{}, {Key: key}} {
		for _, gz := range []bool{false, true} {
			obj, err := c.Seal(raw, "wal/000000010000000000000001", gz)
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Open(obj, "wal/000000010000000000000001")
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("roundtrip enc=%v gz=%v: %v", c.Key != nil, gz, err)
			}
			if c.Key != nil {
				if _, err := c.Open(obj, "wal/000000010000000000000002"); !errors.Is(err, ErrWrongKey) {
					t.Fatalf("renamed object must fail: %v", err)
				}
				other, _ := NewKey()
				if _, err := (Codec{Key: other}).Open(obj, "wal/000000010000000000000001"); !errors.Is(err, ErrWrongKey) {
					t.Fatalf("wrong key must fail: %v", err)
				}
				if _, err := (Codec{}).Open(obj, "wal/000000010000000000000001"); !errors.Is(err, ErrWrongKey) {
					t.Fatalf("no key must fail: %v", err)
				}
				if bytes.Contains(obj, []byte("wal record")) {
					t.Fatal("encrypted object holds plain text")
				}
			}
			obj[len(obj)-1] ^= 1
			if _, err := c.Open(obj, "wal/000000010000000000000001"); err == nil {
				t.Fatal("damaged object opened")
			}
		}
	}
	k, _ := NewKey()
	s := EncodeKey(k)
	back, err := DecodeKey(s)
	if err != nil || !bytes.Equal(back, k) {
		t.Fatalf("key text roundtrip: %v", err)
	}
	if _, err := DecodeKey(s[:len(s)-2]); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestParseWALName(t *testing.T) {
	cases := map[string]WALName{
		"000000010000000000000003":                 {TLI: 1, Seg: 3, Kind: "segment"},
		"0000000200000001000000FF":                 {TLI: 2, Seg: 511, Kind: "segment"},
		"00000002.history":                         {TLI: 2, Kind: "history"},
		"000000010000000000000005.partial":         {TLI: 1, Seg: 5, Kind: "partial"},
		"000000010000000000000002.00000028.backup": {TLI: 1, Seg: 2, Kind: "backup"},
	}
	for name, want := range cases {
		got, ok := ParseWALName(name)
		if !ok || got.TLI != want.TLI || got.Seg != want.Seg || got.Kind != want.Kind {
			t.Errorf("%s: got %+v ok=%v", name, got, ok)
		}
	}
	for _, bad := range []string{"", "RECOVERYXLOG", "../etc/passwd", "0000000100000000000001XX", "000000010000000000000100"} {
		if _, ok := ParseWALName(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if SegmentFileName(2, 511) != "0000000200000001000000FF" {
		t.Fatal(SegmentFileName(2, 511))
	}
}

func fakeSegment(sysid uint64, fill byte) []byte {
	b := bytes.Repeat([]byte{fill}, 64<<10)
	binary.LittleEndian.PutUint16(b[0:2], 0xD118)
	binary.LittleEndian.PutUint16(b[2:4], 0x0002) // long header
	binary.LittleEndian.PutUint64(b[24:32], sysid)
	return b
}

func pushOpts(t *testing.T, dir, name string, seg []byte, primary bool) PushOptions {
	t.Helper()
	p := filepath.Join(dir, "pgdata", "pg_wal", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, seg, 0o600); err != nil {
		t.Fatal(err)
	}
	return PushOptions{StateDir: dir, Path: "pg_wal/" + name, Name: name, Primary: primary}
}

func TestPushFolder(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cfg := Config{Enabled: true, ClusterID: "c1", Node: "a", Kind: KindFolder, Folder: root}
	n1, n2 := t.TempDir(), t.TempDir()
	name := "000000010000000000000001"
	seg := fakeSegment(42, 'a')
	r, err := Push(ctx, cfg, pushOpts(t, n1, name, seg, true))
	if err != nil || !r.Stored {
		t.Fatalf("first push: %+v %v", r, err)
	}
	// A copy pushing the same content: success, nothing uploaded.
	r, err = Push(ctx, cfg, pushOpts(t, n2, name, seg, false))
	if err != nil || r.Stored || r.Conflict {
		t.Fatalf("same content: %+v %v", r, err)
	}
	// A copy with different content never replaces.
	other := fakeSegment(42, 'b')
	r, err = Push(ctx, cfg, pushOpts(t, n2, name, other, false))
	if err != nil || !r.Conflict || r.Stored {
		t.Fatalf("copy conflict: %+v %v", r, err)
	}
	st, _ := cfg.Store()
	dst := filepath.Join(t.TempDir(), "x")
	if err := Fetch(ctx, st, Codec{}, name, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, seg) {
		t.Fatal("copy replaced the stored segment")
	}
	// A primary with different content replaces and keeps the old one.
	r, err = Push(ctx, cfg, pushOpts(t, n2, name, other, true))
	if err != nil || !r.Conflict || !r.Stored {
		t.Fatalf("primary conflict: %+v %v", r, err)
	}
	if err := Fetch(ctx, st, Codec{}, name, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, other) {
		t.Fatal("primary did not replace")
	}
	cs, _ := st.List(ctx, "conflicts/")
	if len(cs) != 2 {
		t.Fatalf("conflicts kept: %d", len(cs))
	}
	// Another database's WAL is refused.
	_, err = Push(ctx, cfg, pushOpts(t, n1, "000000010000000000000002", fakeSegment(43, 'c'), true))
	if !errors.Is(err, ErrOtherDatabase) {
		t.Fatalf("other database: %v", err)
	}
	ps, _ := ReadPushState(n1)
	if ps.FailingSince.IsZero() || ps.LastError == "" {
		t.Fatalf("failure not recorded: %+v", ps)
	}
	// Missing in the archive.
	if err := Fetch(ctx, st, Codec{}, "000000010000000000000009", dst); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestPushGivesUpToProtectTheDisk(t *testing.T) {
	ctx := context.Background()
	cfg := Config{Enabled: true, ClusterID: "c1", Node: "a", Kind: KindFolder, Folder: "relative/is/bad"}
	dir := t.TempDir()
	o := pushOpts(t, dir, "000000010000000000000001", fakeSegment(1, 'a'), true)
	if _, err := Push(ctx, cfg, o); err == nil {
		t.Fatal("expected a failure")
	}
	o.Backlog = func() int64 { return MaxBacklogBytes + 1 }
	r, err := Push(ctx, cfg, o)
	if err != nil || !r.GaveUp {
		t.Fatalf("expected give-up: %+v %v", r, err)
	}
	ps, _ := ReadPushState(dir)
	if ps.GapFrom != "000000010000000000000001" || ps.GapReason == "" || ps.GapsTotal != 1 {
		t.Fatalf("gap not recorded: %+v", ps)
	}
	o.Backlog, o.DiskFree = nil, func() (uint64, error) { return 10 << 20, nil }
	if r, err := Push(ctx, cfg, o); err != nil || !r.GaveUp {
		t.Fatalf("low disk: %+v %v", r, err)
	}
}

func TestEncryptedPushAndIdentity(t *testing.T) {
	ctx := context.Background()
	k, _ := NewKey()
	cfg := Config{Enabled: true, ClusterID: "c1", Node: "a", Kind: KindFolder, Folder: t.TempDir(), Key: EncodeKey(k)}
	n := t.TempDir()
	seg := fakeSegment(7, 'z')
	if _, err := Push(ctx, cfg, pushOpts(t, n, "000000010000000000000001", seg, true)); err != nil {
		t.Fatal(err)
	}
	st, _ := cfg.Store()
	id, err := ReadIdentity(ctx, st)
	if err != nil || id.SystemID != "7" || id.KeyID != KeyID(k) {
		t.Fatalf("identity: %+v %v", id, err)
	}
	k2, _ := NewKey()
	cfg2 := cfg
	cfg2.Key = EncodeKey(k2)
	_, err = Push(ctx, cfg2, pushOpts(t, t.TempDir(), "000000010000000000000002", seg, true))
	if !errors.Is(err, ErrWrongKey) {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestSigV4AWSExamples(t *testing.T) {
	// From the AWS S3 documentation ("Signature Calculations for the
	// Authorization Header: Transferring Payload in a Single Chunk").
	s := &S3Store{Endpoint: "https://s3.amazonaws.com", Region: "us-east-1", Bucket: "examplebucket",
		AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) }}
	u, _ := s.objectURL("test.txt", nil)
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	req.Header.Set("Range", "bytes=0-9")
	s.sign(req, nil)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("GET object:\n got %s\nwant %s", got, want)
	}
	q := url.Values{"max-keys": {"2"}, "prefix": {"J"}}
	u, _ = s.objectURL("", q)
	req, _ = http.NewRequest(http.MethodGet, u.String(), nil)
	s.sign(req, nil)
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7") {
		t.Fatalf("list: %s", got)
	}
}

func TestSchedule(t *testing.T) {
	loc := time.FixedZone("X", 3600)
	s := Schedule{Every: 24 * time.Hour, At: "03:00"}
	now := time.Date(2026, 11, 24, 2, 0, 0, 0, loc)
	if got := s.LastSlot(now); !got.Equal(time.Date(2026, 11, 23, 3, 0, 0, 0, loc)) {
		t.Fatal(got)
	}
	if got := s.NextSlot(now); !got.Equal(time.Date(2026, 11, 24, 3, 0, 0, 0, loc)) {
		t.Fatal(got)
	}
	last := time.Date(2026, 11, 23, 3, 1, 0, 0, loc)
	if _, due := s.DueSince(now, last, time.Time{}); due {
		t.Fatal("not due yet")
	}
	if since, due := s.DueSince(now.Add(2*time.Hour), last, time.Time{}); !due || since.Hour() != 3 {
		t.Fatal("due at 03:00")
	}
	if _, due := s.DueSince(now, time.Time{}, now.Add(time.Minute)); due {
		t.Fatal("first backup waits")
	}
	six := Schedule{Every: 6 * time.Hour, At: "03:00"}
	if got := six.LastSlot(time.Date(2026, 11, 24, 16, 0, 0, 0, loc)); got.Hour() != 15 {
		t.Fatal(got)
	}
	if got := six.LastSlot(time.Date(2026, 11, 24, 1, 0, 0, 0, loc)); got.Hour() != 21 || got.Day() != 23 {
		t.Fatal(got)
	}
	week := Schedule{Every: 7 * 24 * time.Hour, At: "03:00"}
	a := week.LastSlot(time.Date(2026, 11, 24, 12, 0, 0, 0, loc))
	b := week.LastSlot(time.Date(2026, 11, 26, 12, 0, 0, 0, loc))
	if !a.Equal(b) || week.NextSlot(a.Add(time.Hour)).Sub(a) != 7*24*time.Hour {
		t.Fatal(a, b)
	}
}

func TestCatalogAndRetention(t *testing.T) {
	ctx := context.Background()
	st := FolderStore{Root: t.TempDir()}
	day := 24 * time.Hour
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	put := func(k string) {
		if err := st.Put(ctx, k, []byte("x"), false); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(id string, fin time.Time, start, end string) {
		b := []byte(`{"id":"` + id + `","node":"a","started_at":"` + fin.Add(-time.Minute).Format(time.RFC3339) + `","finished_at":"` + fin.Format(time.RFC3339) +
			`","start_lsn":"` + start + `","end_lsn":"` + end + `","timeline":1,"parts":1}`)
		if err := st.Put(ctx, "base/"+id+"/info.json", b, false); err != nil {
			t.Fatal(err)
		}
		put("base/" + id + "/part-00000")
	}
	mk("old", now.Add(-10*day), "0/1000028", "0/1000100")
	mk("edge", now.Add(-8*day), "0/3000028", "0/3000100")
	mk("new", now.Add(-1*day), "0/5000028", "0/5000100")
	mk("broken", now.Add(-time.Hour), "0/9000028", "0/9000100")
	for _, seg := range []uint64{1, 2, 3, 4, 5, 6, 7} {
		put("wal/" + SegmentFileName(1, seg))
	}
	put("wal/00000002.history")
	put("base/dead/part-00000")
	cat, err := LoadCatalog(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Backups) != 4 || len(cat.Orphans) != 1 {
		t.Fatalf("catalog: %+v", cat)
	}
	if nb, ok := cat.Newest(); !ok || nb.ID != "new" {
		t.Fatalf("newest usable: %+v", nb)
	}
	if cat.Usable(cat.Backups[3]) {
		t.Fatal("backup without its WAL counted as usable")
	}
	plan := cat.PlanRetention(now, 7*day)
	if strings.Join(plan.DeleteBackups, ",") != "old" {
		t.Fatalf("delete backups: %v", plan.DeleteBackups)
	}
	if strings.Join(plan.DeleteWAL, ",") != SegmentFileName(1, 1)+","+SegmentFileName(1, 2) {
		t.Fatalf("delete wal: %v", plan.DeleteWAL)
	}
	if err := plan.Apply(ctx, st, now); err != nil {
		t.Fatal(err)
	}
	cat, _ = LoadCatalog(ctx, st)
	if len(cat.Backups) != 3 || cat.WAL["00000002.history"].Kind != "history" || len(cat.WAL) != 6 {
		t.Fatalf("after: %d backups, %d wal", len(cat.Backups), len(cat.WAL))
	}
}

func TestFolderPutIfAbsent(t *testing.T) {
	ctx := context.Background()
	st := FolderStore{Root: t.TempDir()}
	if err := st.Put(ctx, "a/b", []byte("1"), true); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "a/b", []byte("2"), true); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	if b, _ := st.Get(ctx, "a/b"); string(b) != "1" {
		t.Fatal(string(b))
	}
	if _, err := st.Get(ctx, "../x"); err == nil {
		t.Fatal("path escape")
	}
	if b, err := st.GetPrefix(ctx, "a/b", 10); err != nil || string(b) != "1" {
		t.Fatal(err)
	}
}
