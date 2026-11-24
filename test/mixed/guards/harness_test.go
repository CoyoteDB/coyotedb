//go:build integration

// Real-Postgres tests for the mixed-system guards (internal/pg
// fingerprint.go, probe.go, portability.go, verify.go; docs/MIXED_OS.md
// Step 2). Each test makes a throwaway data folder with CoyoteDB's exact
// initdb settings (internal/pg Node.InitDB) and runs Postgres with pg_ctl
// on a free port on 127.0.0.1. The Postgres programs come from the shared
// cache (./.coyote-dev/cache, or $COYOTE_PG_CACHE).
//
// Run: go test -tags=integration -count=1 ./test/mixed/guards/
package guards

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"coyotedb.com/coyote/internal/pg"
	"coyotedb.com/coyote/test/testports"
)

var inst pg.Install

func TestMain(m *testing.M) {
	cache := os.Getenv("COYOTE_PG_CACHE")
	if cache == "" {
		var err error
		if cache, err = filepath.Abs(filepath.Join("..", "..", "..", ".coyote-dev", "cache")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	var err error
	inst, err = pg.Ensure(ctx, pg.FetchOptions{CacheDir: cache})
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "postgres install:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// server is one throwaway Postgres.
type server struct {
	t    *testing.T
	node pg.Node
	port int
	pw   string
}

// newServer runs initdb the way the product does (Node.InitDB: builtin
// C.UTF-8, checksums, scram) and starts Postgres. It is stopped at the
// end of the test.
func newServer(t *testing.T) *server {
	t.Helper()
	dir, err := os.MkdirTemp("", "coyote guards ")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	n, err := pg.NewNode(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Prepare(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err := n.InitDB(ctx, inst, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	pw, err := n.Password()
	if err != nil {
		t.Fatal(err)
	}
	// The agent-owned config, as the product writes it (coyote.conf,
	// included last from postgresql.conf). The port is given on the
	// command line, which wins.
	conf := pg.RenderConf(pg.Config{Port: 1, LogDir: n.LogDir()})
	if err := os.WriteFile(filepath.Join(n.PGData(), pg.AgentConfFile), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(n.PGData(), "postgresql.conf"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\ninclude = '" + pg.AgentConfFile + "'\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	s := &server{t: t, node: n, pw: pw}
	s.start()
	t.Cleanup(s.stop)
	return s
}

func (s *server) start() {
	s.t.Helper()
	for attempt := 0; ; attempt++ {
		p, err := testports.Free()
		if err != nil {
			s.t.Fatal(err)
		}
		s.port = p
		out, err := s.pgctl("start", "-w", "-t", "120", "-l", filepath.Join(s.node.LogDir(), "pg.log"),
			"-o", "-c listen_addresses=127.0.0.1 -c port="+strconv.Itoa(p)+" -c unix_socket_directories='' -c shared_buffers=64MB")
		if err == nil {
			break
		}
		if attempt < 3 && strings.Contains(s.log(), "already in use") {
			continue
		}
		s.t.Fatalf("pg_ctl start: %v\n%s\n%s", err, out, s.log())
	}
	if err := s.node.WriteInfo(pg.NodeInfo{Port: s.port, PGVersion: pg.Version}); err != nil {
		s.t.Fatal(err)
	}
}

func (s *server) log() string {
	b, _ := os.ReadFile(filepath.Join(s.node.LogDir(), "pg.log"))
	return string(b)
}

func (s *server) stop() {
	if _, err := os.Stat(filepath.Join(s.node.PGData(), "postmaster.pid")); err != nil {
		return
	}
	if out, err := s.pgctl("stop", "-w", "-t", "120", "-m", "fast"); err != nil {
		s.t.Errorf("pg_ctl stop: %v\n%s", err, out)
	}
}

func (s *server) restart() {
	s.t.Helper()
	s.stop()
	s.start()
}

func (s *server) pgctl(args ...string) (string, error) {
	cmd := exec.Command(inst.Bin("pg_ctl"), append([]string{"-D", s.node.PGData()}, args...)...)
	cmd.Env = append(cleanEnv(), "LC_ALL=C")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "PG") {
			env = append(env, kv)
		}
	}
	return env
}

// connect opens a superuser connection to db.
func (s *server) connect(db string) *pgx.Conn {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := s.node.ConnectDB(ctx, db)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func (s *server) connectFunc() pg.ConnectFunc { return s.node.ConnectDB }

// exec runs SQL statements on db, failing the test on error.
func (s *server) exec(db string, stmts ...string) {
	s.t.Helper()
	c := s.connect(db)
	defer c.Close(context.Background())
	for _, q := range stmts {
		if _, err := c.Exec(context.Background(), q); err != nil {
			s.t.Fatalf("%s: %v", q, err)
		}
	}
}

func ctxT(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
