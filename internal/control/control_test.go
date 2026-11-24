package control

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTokenAndAuth(t *testing.T) {
	dir := t.TempDir()
	tok, err := EnsureToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := EnsureToken(dir); again != tok || len(tok) != 64 {
		t.Fatalf("token not stable: %q %q", tok, again)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, TokenFile))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("token file mode %v %v", fi, err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, map[string]string{"ok": "yes"})
	})
	s, err := Listen(0, tok, mux, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	host, _, _ := net.SplitHostPort(s.Addr())
	if host != "127.0.0.1" {
		t.Fatalf("bound to %s", s.Addr())
	}
	if err := WriteInfo(dir, Info{Addr: s.Addr(), PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for name, token := range map[string]string{"none": "", "wrong": "x" + tok[1:]} {
		c := &Client{Addr: s.Addr(), Token: token, HTTP: &http.Client{}}
		err := c.Do(ctx, "GET", "/ping", nil, nil)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusUnauthorized {
			t.Fatalf("%s token: want 401, got %v", name, err)
		}
	}
	c, err := NewClient(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := c.Do(ctx, "GET", "/ping", nil, &out); err != nil || out["ok"] != "yes" {
		t.Fatalf("authorized call: %v %v", out, err)
	}
	var ae *APIError
	if err := c.Do(ctx, "GET", "/nope", nil, nil); !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("unknown path: %v", err)
	}
}

func TestClientNotRunning(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewClient(dir, 0); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("no token: %v", err)
	}
	if _, err := EnsureToken(dir); err != nil {
		t.Fatal(err)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	c, err := NewClient(dir, port)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Do(context.Background(), "GET", "/status", nil, nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("nothing listening: %v", err)
	}
}
