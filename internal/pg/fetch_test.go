package pg

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func fakeBuild(t *testing.T) (Build, []byte) {
	t.Helper()
	b := Build{Version: "0.0.1", Target: "test-target"}
	exe := "postgres"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	data := makeTarGz(t, []tarEntry{
		{name: b.Name() + "/", typ: tar.TypeDir},
		{name: b.Name() + "/bin/" + exe, typ: tar.TypeReg, body: "pg", mode: 0o755},
	})
	sum := sha256.Sum256(data)
	b.SHA256 = hex.EncodeToString(sum[:])
	return b, data
}

func TestVerifyFileMismatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := VerifyFile(p, strings.Repeat("0", 64))
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}
	sum := sha256.Sum256([]byte("hello"))
	if err := VerifyFile(p, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("matching file rejected: %v", err)
	}
}

func TestEnsureFromArchiveAndReuse(t *testing.T) {
	b, data := fakeBuild(t)
	dir := t.TempDir()
	arch := filepath.Join(dir, "pg.tar.gz")
	if err := os.WriteFile(arch, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache dir with spaces")
	inst, err := Ensure(context.Background(), FetchOptions{CacheDir: cache, Build: b, Archive: arch})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := os.Stat(inst.Bin("postgres")); err != nil {
		t.Fatalf("postgres missing: %v", err)
	}
	// Second call must reuse without touching the archive.
	if err := os.Remove(arch); err != nil {
		t.Fatal(err)
	}
	inst2, err := Ensure(context.Background(), FetchOptions{CacheDir: cache, Build: b, Archive: arch})
	if err != nil || inst2.Root != inst.Root {
		t.Fatalf("reuse failed: %v", err)
	}
}

func TestEnsureRejectsTamperedArchive(t *testing.T) {
	b, data := fakeBuild(t)
	data[len(data)-5] ^= 0xff
	dir := t.TempDir()
	arch := filepath.Join(dir, "pg.tar.gz")
	if err := os.WriteFile(arch, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache")
	_, err := Ensure(context.Background(), FetchOptions{CacheDir: cache, Build: b, Archive: arch})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, b.Name())); err == nil {
		t.Fatal("tampered archive was installed")
	}
}

// rangeServer serves data with Range support. If cutAt > 0, the first full
// (non-range) response is cut short after cutAt bytes.
func rangeServer(t *testing.T, data []byte, cutAt int) (*httptest.Server, *atomic.Int32) {
	var ranged atomic.Int32
	var first atomic.Bool
	first.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rg := r.Header.Get("Range"); rg != "" {
			start, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			if err != nil || start > len(data) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if start == len(data) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			ranged.Add(1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(len(data)-start))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start:])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if cutAt > 0 && first.Swap(false) {
			_, _ = w.Write(data[:cutAt])
			// Abort the connection mid-body.
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, &ranged
}

func TestDownloadResumesAfterCut(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 10000)
	srv, ranged := rangeServer(t, data, 30000)
	dst := filepath.Join(t.TempDir(), "x.partial")
	var last int64
	err := download(context.Background(), srv.Client(), srv.URL, dst, func(d, _ int64) { last = d }, discardLogger(), "test")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, data) {
		t.Fatalf("content mismatch: %d bytes", len(got))
	}
	if ranged.Load() == 0 {
		t.Fatal("expected a resumed (ranged) request")
	}
	if last != int64(len(data)) {
		t.Fatalf("progress ended at %d", last)
	}
}

func TestDownloadCompletePartial(t *testing.T) {
	data := []byte("complete")
	srv, _ := rangeServer(t, data, 0)
	dst := filepath.Join(t.TempDir(), "x.partial")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := download(context.Background(), srv.Client(), srv.URL, dst, nil, discardLogger(), "test"); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, data) {
		t.Fatalf("content changed: %q", got)
	}
}

func TestParseContentRange(t *testing.T) {
	s, n, ok := parseContentRange("bytes 100-199/200")
	if !ok || s != 100 || n != 200 {
		t.Fatalf("got %d %d %v", s, n, ok)
	}
	for _, bad := range []string{"", "bytes */200", "items 1-2/3", "bytes 1-2"} {
		if _, _, ok := parseContentRange(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}
