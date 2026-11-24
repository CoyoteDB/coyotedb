//go:build s3

// Tests of our S3 client against a real S3-compatible server (MinIO; see
// docs/TESTING.md "Backups"). Needs COYOTE_TEST_S3_ENDPOINT,
// COYOTE_TEST_S3_BUCKET, COYOTE_TEST_S3_ACCESS_KEY and
// COYOTE_TEST_S3_SECRET_KEY; without them the tests fail (no skipping).
package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func realS3(t *testing.T) *S3Store {
	t.Helper()
	get := func(k string) string {
		v := os.Getenv("COYOTE_TEST_S3_" + k)
		if v == "" {
			t.Fatalf("COYOTE_TEST_S3_%s is not set (these tests need a real S3-compatible server)", k)
		}
		return v
	}
	return &S3Store{Endpoint: get("ENDPOINT"), Bucket: get("BUCKET"), AccessKey: get("ACCESS_KEY"), SecretKey: get("SECRET_KEY"),
		Region: os.Getenv("COYOTE_TEST_S3_REGION"), PathStyle: true,
		Prefix: fmt.Sprintf("unit-%d/", time.Now().UnixNano())}
}

func TestS3RealSemantics(t *testing.T) {
	ctx := context.Background()
	s := realS3(t)
	data := bytes.Repeat([]byte{0, 1, 2, 255}, 1<<18) // 1 MiB, binary
	if err := s.Put(ctx, "a/x y+z~ü.bin", data, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "a/x y+z~ü.bin", []byte("other"), true); !errors.Is(err, ErrExists) {
		t.Fatalf("If-None-Match must refuse an existing key: %v", err)
	}
	got, err := s.Get(ctx, "a/x y+z~ü.bin")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("get: %v", err)
	}
	head, err := s.GetPrefix(ctx, "a/x y+z~ü.bin", 41)
	if err != nil || !bytes.Equal(head, data[:41]) {
		t.Fatalf("range: %v %d", err, len(head))
	}
	if _, err := s.Get(ctx, "a/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.Put(ctx, "a/x y+z~ü.bin", []byte("replaced"), false); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, "a/x y+z~ü.bin"); string(got) != "replaced" {
		t.Fatal("overwrite")
	}
	// Two writers racing for one key: exactly one wins.
	var wg sync.WaitGroup
	wins := make(chan int, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Put(ctx, "race", []byte{byte(i)}, true) == nil {
				wins <- i
			}
		}()
	}
	wg.Wait()
	close(wins)
	if n := len(wins); n != 1 {
		t.Fatalf("%d writers won the same key", n)
	}
	// Listing across pages (the server returns at most 1000 per page).
	var pwg sync.WaitGroup
	sem := make(chan struct{}, 32)
	for i := range 1010 {
		pwg.Add(1)
		sem <- struct{}{}
		go func() {
			defer pwg.Done()
			defer func() { <-sem }()
			if err := s.Put(ctx, fmt.Sprintf("many/%04d", i), []byte("x"), false); err != nil {
				t.Error(err)
			}
		}()
	}
	pwg.Wait()
	l, err := s.List(ctx, "many/")
	if err != nil || len(l) != 1010 || l[0].Key != "many/0000" || l[1009].Key != "many/1009" {
		t.Fatalf("list: %v %d", err, len(l))
	}
	all, _ := s.List(ctx, "")
	for _, o := range all {
		if err := s.Delete(ctx, o.Key); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(ctx, "never-there"); err != nil {
		t.Fatalf("delete of a missing key: %v", err)
	}
	if l, _ := s.List(ctx, ""); len(l) != 0 {
		t.Fatalf("left over: %d", len(l))
	}
	bad := *s
	bad.SecretKey = "wrong"
	var se *S3Error
	if _, err := bad.Get(ctx, "x"); !errors.As(err, &se) || se.Status != 403 {
		t.Fatalf("wrong secret: %v", err)
	}
}

func TestS3RealPushEncrypted(t *testing.T) {
	ctx := context.Background()
	s := realS3(t)
	k, _ := NewKey()
	cfg := Config{Enabled: true, ClusterID: "cl", Node: "a", Kind: KindS3, Key: EncodeKey(k),
		S3: S3Settings{Endpoint: s.Endpoint, Bucket: s.Bucket, Prefix: s.Prefix, AccessKey: s.AccessKey, SecretKey: s.SecretKey, PathStyle: "on"}}
	n := t.TempDir()
	seg := fakeSegment(99, 'q')
	if r, err := Push(ctx, cfg, pushOpts(t, n, "000000010000000000000001", seg, true)); err != nil || !r.Stored {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := Push(ctx, cfg, pushOpts(t, t.TempDir(), "000000010000000000000001", seg, false)); err != nil || r.Stored {
		t.Fatalf("second push must be a no-op: %+v %v", r, err)
	}
	st, _ := cfg.Store()
	raw, _ := st.Get(ctx, "wal/000000010000000000000001")
	if bytes.Contains(raw, seg[100:200]) {
		t.Fatal("stored in plain text")
	}
	dst := t.TempDir() + "/seg"
	codec, _ := cfg.Codec()
	if err := Fetch(ctx, st, codec, "000000010000000000000001", dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, seg) {
		t.Fatal("roundtrip")
	}
	all, _ := s.List(ctx, "")
	for _, o := range all {
		_ = s.Delete(ctx, o.Key)
	}
}
