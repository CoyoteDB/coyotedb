package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/pg"
)

var errUsage = errors.New("bad usage")

// peer serves one node's checks to the driver.
type peer struct {
	name    string
	dataDir string
	node    pg.Node
	inst    pg.Install
	token   string
	log     *slog.Logger
	// one inspection or setup at a time: they are heavy
	mu sync.Mutex
}

func cmdPeer(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("peer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", "", "the node's data folder (as given to coyote up/join)")
	listen := fs.String("listen", "127.0.0.1:7499", "where the driver reaches this peer")
	tokenFile := fs.String("token-file", "", "file holding the shared secret token (the same on every peer and the driver)")
	cache := fs.String("pg-cache", "", "Postgres program files (default: $COYOTE_PG_CACHE or <data-dir>/cache)")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *dataDir == "" || *tokenFile == "" {
		fs.Usage()
		return errUsage
	}
	tok, err := readToken(*tokenFile)
	if err != nil {
		return err
	}
	node, err := pg.NewNode(*dataDir)
	if err != nil {
		return err
	}
	info, err := node.ReadInfo()
	if err != nil {
		return err
	}
	if *cache == "" {
		*cache = os.Getenv("COYOTE_PG_CACHE")
	}
	if *cache == "" {
		*cache = filepath.Join(node.StateDir, "cache")
	}
	inst, err := pg.Ensure(ctx, pg.FetchOptions{CacheDir: *cache})
	if err != nil {
		return fmt.Errorf("postgres programs: %w", err)
	}
	p := &peer{name: info.Name, dataDir: node.StateDir, node: node, inst: inst, token: tok,
		log: slog.New(slog.NewTextHandler(stderr, nil)).With("peer", info.Name)}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: p.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	p.log.Info("serving", "addr", ln.Addr().String(), "os", runtime.GOOS, "arch", runtime.GOARCH)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if len(t) < 16 {
		return "", errors.New("the token must be at least 16 characters")
	}
	return t, nil
}

func (p *peer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"name": p.name, "os": runtime.GOOS, "arch": runtime.GOARCH})
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		p.forward(w, r, http.MethodGet, "/status", nil, time.Minute)
	})
	mux.HandleFunc("POST /v1/switch", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			To string `json:"to"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.To == "" {
			writeError(w, http.StatusBadRequest, "body must be {\"to\": \"<node>\"}")
			return
		}
		p.forward(w, r, http.MethodPost, "/switch", map[string]string{"to": req.To}, 7*time.Minute)
	})
	mux.HandleFunc("GET /v1/conninfo", p.conninfo)
	mux.HandleFunc("GET /v1/lsn", p.lsn)
	mux.HandleFunc("POST /v1/setup", p.setup)
	mux.HandleFunc("POST /v1/inspect", p.handleInspect)
	return p.auth(mux)
}

func (p *peer) auth(next http.Handler) http.Handler {
	want := []byte("Bearer " + p.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeError(w, http.StatusUnauthorized, "wrong token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// forward passes a request to the node's own agent (control channel).
func (p *peer) forward(w http.ResponseWriter, r *http.Request, method, path string, body any, timeout time.Duration) {
	c, err := control.NewClient(p.dataDir, 0)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	var out json.RawMessage
	if err := c.Do(ctx, method, path, body, &out); err != nil {
		status := http.StatusBadGateway
		var ae *control.APIError
		if errors.As(err, &ae) {
			status = ae.Status
		}
		writeError(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// ConnInfo is what the driver needs to reach the application port.
type ConnInfo struct {
	URL      string `json:"url"`
	User     string `json:"user"`
	Database string `json:"database"`
	CAPEM    string `json:"ca_pem"`
	Pending  bool   `json:"pending,omitempty"`
}

func (p *peer) conninfo(w http.ResponseWriter, r *http.Request) {
	c, err := control.NewClient(p.dataDir, 0)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	var ci struct {
		URL      string `json:"url"`
		User     string `json:"user"`
		Database string `json:"database"`
		CAFile   string `json:"ca_file"`
		Pending  bool   `json:"pending"`
	}
	if err := c.Do(r.Context(), http.MethodGet, "/conninfo", nil, &ci); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	out := ConnInfo{URL: ci.URL, User: ci.User, Database: ci.Database, Pending: ci.Pending}
	if ci.CAFile != "" {
		b, err := os.ReadFile(ci.CAFile)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out.CAPEM = string(b)
	}
	writeJSON(w, http.StatusOK, out)
}

// LSNReport is the node's position in the change log.
type LSNReport struct {
	InRecovery bool   `json:"in_recovery"`
	LSN        string `json:"lsn"` // current (primary) or last replayed (standby)
	Behind     bool   `json:"behind,omitempty"`
}

func (p *peer) lsn(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	c, err := p.node.ConnectDB(ctx, "postgres")
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer c.Close(context.WithoutCancel(ctx))
	var rep LSNReport
	want := r.URL.Query().Get("at_least")
	err = c.QueryRow(ctx, `SELECT pg_is_in_recovery(), coalesce(CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END::text, ''),
		CASE WHEN $1 = '' THEN false ELSE coalesce(CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END < $1::pg_lsn, true) END`, want).
		Scan(&rep.InRecovery, &rep.LSN, &rep.Behind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// SetupRequest creates the workload schema on the main node.
type SetupRequest struct {
	Database   string   `json:"database"`
	AppUser    string   `json:"app_user"`
	Statements []string `json:"statements"`
}

func (p *peer) setup(w http.ResponseWriter, r *http.Request) {
	var req SetupRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&req); err != nil || req.Database == "" || req.AppUser == "" {
		writeError(w, http.StatusBadRequest, "body must be a SetupRequest")
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	c, err := p.node.ConnectDB(ctx, req.Database)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer c.Close(context.WithoutCancel(ctx))
	var inRecovery, exists bool
	if err := c.QueryRow(ctx, `SELECT pg_is_in_recovery(), to_regclass('public.campaign_ledger') IS NOT NULL`).Scan(&inRecovery, &exists); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if inRecovery {
		writeError(w, http.StatusConflict, "this node is not the main node")
		return
	}
	if !exists {
		stmts := append(append([]string{}, req.Statements...), setupTail(req.AppUser)...)
		for _, s := range stmts {
			if _, err := c.Exec(ctx, s); err != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("%s: %v", firstLine(s), err))
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"created": !exists})
}

// setupTail creates the ledger and lets the application role use the
// workload tables.
func setupTail(appUser string) []string {
	u := `"` + strings.ReplaceAll(appUser, `"`, `""`) + `"`
	return []string{
		`CREATE TABLE IF NOT EXISTS public.campaign_ledger (id bigint PRIMARY KEY, at timestamptz NOT NULL DEFAULT now(), body text NOT NULL)`,
		`GRANT USAGE ON SCHEMA public TO ` + u,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO ` + u,
		`GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO ` + u,
		`GRANT SELECT ON pg_largeobject TO ` + u,
	}
}

// InspectRequest asks for one inspection.
type InspectRequest struct {
	Database string `json:"database"`
	Level    string `json:"level"` // quick, standard, deep
	Amcheck  bool   `json:"pg_amcheck"`
	Seed     int64  `json:"seed"`
}

func parseLevel(s string) (pg.VerifyLevel, error) {
	switch s {
	case "", "quick":
		return pg.VerifyQuick, nil
	case "standard":
		return pg.VerifyStandard, nil
	case "deep":
		return pg.VerifyDeep, nil
	}
	return 0, fmt.Errorf("unknown level %q (quick, standard, deep)", s)
}

func (p *peer) handleInspect(w http.ResponseWriter, r *http.Request) {
	var req InspectRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	lvl, err := parseLevel(req.Level)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Hour)
	defer cancel()
	writeJSON(w, http.StatusOK, p.inspect(ctx, inspectOptions{Database: req.Database, Level: lvl, Amcheck: req.Amcheck, Seed: req.Seed}))
}

// passfile writes a temporary libpq password file for the superuser.
func (p *peer) passfile() (string, func(), error) {
	info, err := p.node.ReadInfo()
	if err != nil {
		return "", nil, err
	}
	pw, err := p.node.Password()
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(p.node.StateDir, "campaign-")
	if err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, "pgpass")
	body := pg.RenderPassfile([]pg.PassEntry{{Host: "127.0.0.1", Port: fmt.Sprint(info.Port), DB: "*", User: pg.Superuser, Password: pw}})
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return path, func() { os.RemoveAll(dir) }, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
