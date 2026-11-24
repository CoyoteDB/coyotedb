// Package control is the local control channel: one-shot CLI commands
// (status, invite, leave, remove, dev ...) talk to the running agent over
// HTTP on 127.0.0.1 only. Every request carries a bearer token that the
// agent keeps in the data dir (0600), so only someone who can read the data
// dir can drive the agent. JSON in and out, under a versioned prefix (/v1)
// so the full HTTP API (M6) can grow from it.
package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/durable"
)

// Defaults and file names in the node's data dir.
const (
	DefaultPort = 7480
	Prefix      = "/v1"
	TokenFile   = "control.token" // 0600, random, kept across restarts
	InfoFile    = "control.json"  // where the running agent listens
	maxBody     = 1 << 20
)

// Info is written by the running agent so the CLI can find it.
type Info struct {
	Addr string `json:"addr"`
	PID  int    `json:"pid"`
}

// EnsureToken returns the data dir's control token, creating it (0600) if
// it does not exist yet.
func EnsureToken(dir string) (string, error) {
	tok, err := ReadToken(dir)
	if err == nil {
		return tok, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok = hex.EncodeToString(b)
	if err := writeFileAtomic(filepath.Join(dir, TokenFile), []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write control token: %w", err)
	}
	return tok, nil
}

// ReadToken reads the control token.
func ReadToken(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, TokenFile))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("%s is empty", TokenFile)
	}
	return tok, nil
}

// WriteInfo records where the agent listens.
func WriteInfo(dir string, i Info) error {
	b, err := json.Marshal(i)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, InfoFile), append(b, '\n'), 0o600)
}

// RemoveInfo deletes the info file (agent stopping).
func RemoveInfo(dir string) { _ = os.Remove(filepath.Join(dir, InfoFile)) }

// ReadInfo reads where the agent listens.
func ReadInfo(dir string) (Info, error) {
	var i Info
	b, err := os.ReadFile(filepath.Join(dir, InfoFile))
	if err != nil {
		return i, err
	}
	return i, json.Unmarshal(b, &i)
}

// ---- server ----

// Server serves the control API on 127.0.0.1.
type Server struct {
	ln  net.Listener
	srv *http.Server
	log *slog.Logger
}

// Listen binds 127.0.0.1:port (0 picks a free port) and serves h behind
// the token check. It never binds anything but loopback.
func Listen(port int, token string, h http.Handler, log *slog.Logger, opts ...Option) (*Server, error) {
	ln, err := ListenPort(port)
	if err != nil {
		return nil, err
	}
	s, err := Serve(ln, token, h, log, opts...)
	if err != nil {
		ln.Close()
	}
	return s, err
}

// ListenPort binds the control port on 127.0.0.1 (to be served later
// with Serve).
func ListenPort(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("control port %d on 127.0.0.1 is already in use (pick another with --control-port): %w", port, err)
	}
	return ln, nil
}

// Serve serves the control API on ln (from ListenPort); Close closes ln.
func Serve(ln net.Listener, token string, h http.Handler, log *slog.Logger, opts ...Option) (*Server, error) {
	if token == "" {
		return nil, errors.New("control: empty token")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	port := 0
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		port = ta.Port
	}
	s := &Server{ln: ln, log: log}
	s.srv = &http.Server{
		Handler:           guard(token, port, o.sessions, o.remoteBrowsers, h),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("control server stopped", "err", err)
		}
	}()
	return s, nil
}

// Addr is the bound address ("127.0.0.1:7480").
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops the server, waiting up to a few seconds for requests in flight.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return s.srv.Close()
	}
	return err
}

// Option changes how Serve guards the channel.
type Option func(*options)

type options struct {
	sessions       SessionValidator
	remoteBrowsers bool
}

// SessionValidator lets a browser (the dashboard) use the channel with a
// session cookie besides the CLI's bearer token. The guard keeps every
// decision about who gets in; the validator only answers questions about
// the request.
type SessionValidator interface {
	// Public reports a request that needs no credential: the dashboard's
	// static files and the exchange of a one-time sign-in token for a
	// session. It still has to be from loopback with an allowed Host, and
	// a state-changing one needs a matching Origin.
	Public(r *http.Request) bool
	// BearerOnly reports a request a session cookie may never make (CLI
	// scaffolding, minting sign-in links, reading secrets by GET).
	BearerOnly(r *http.Request) bool
	// Session reports whether r carries a valid session cookie, and that
	// session's CSRF token.
	Session(r *http.Request) (csrf string, ok bool)
}

// WithBrowserSession accepts session cookies that v validates, under the
// Host, Origin and CSRF checks below. Without it only the bearer token works.
func WithBrowserSession(v SessionValidator) Option {
	return func(o *options) { o.sessions = v }
}

// WithRemoteBrowsers (`--dashboard-listen`, D52) lets a request that does
// not come from loopback through the guard, only when all of these hold:
// it comes from a private or link-local address (a container's bridge
// gateway, never the internet), it carries no Authorization header (the
// CLI's bearer token stays loopback-only, so minting sign-in links and
// every bearer-only route stay on this computer), and it then passes the
// whole session path: allowed Host (a loopback name on our port, so only a
// browser on the host that publishes the port as 127.0.0.1:PORT:PORT gets
// there), a valid session from a one-time sign-in link, and Origin + CSRF
// on every change. Requires WithBrowserSession; alone it changes nothing.
func WithRemoteBrowsers() Option {
	return func(o *options) { o.remoteBrowsers = true }
}

// remotePeerOK: a peer WithRemoteBrowsers may let through.
func remotePeerOK(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// CSRFHeader carries the session's CSRF token on state-changing requests.
const CSRFHeader = "X-Coyote-CSRF"

// guard refuses anything not from loopback, whatever the credential. Then
// it accepts the bearer token (the CLI) or, with WithBrowserSession, a
// valid session cookie on a request whose Host is this machine's loopback
// name and port (against DNS rebinding) and which, when it changes
// anything, carries the session's CSRF token and this origin.
func guard(token string, port int, sessions SessionValidator, remoteBrowsers bool, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		auth := r.Header.Get("Authorization")
		if err != nil || ip == nil || !ip.IsLoopback() {
			// Only a browser, and only with --dashboard-listen (D52).
			if !remoteBrowsers || sessions == nil || auth != "" || ip == nil || !remotePeerOK(ip) {
				WriteError(w, http.StatusForbidden, "forbidden", "the control channel only accepts local connections")
				return
			}
		}
		if sessions == nil || auth != "" {
			// The CLI's path, unchanged: the bearer token or nothing.
			if subtle.ConstantTimeCompare([]byte(auth), want) != 1 {
				WriteError(w, http.StatusUnauthorized, "unauthorized", "missing or wrong control token")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			next.ServeHTTP(w, r)
			return
		}
		if !allowedHost(r.Host, port) {
			WriteError(w, http.StatusForbidden, "bad_host", "this address is not allowed; open the dashboard with the link `coyote dashboard` prints")
			return
		}
		unsafe := r.Method != http.MethodGet && r.Method != http.MethodHead
		origin := r.Header.Get("Origin")
		sameOrigin := origin == "http://"+r.Host
		switch {
		case sessions.Public(r):
			if unsafe && origin != "" && !sameOrigin {
				WriteError(w, http.StatusForbidden, "csrf_failed", "this request came from another web page and was refused")
				return
			}
		default:
			csrf, ok := sessions.Session(r)
			if !ok {
				WriteError(w, http.StatusUnauthorized, "unauthenticated", "you are not signed in; run `coyote dashboard` for a sign-in link")
				return
			}
			if sessions.BearerOnly(r) {
				WriteError(w, http.StatusForbidden, "bearer_only", "this can only be done from the command line on this computer")
				return
			}
			if unsafe {
				got := r.Header.Get(CSRFHeader)
				if !sameOrigin || got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(csrf)) != 1 {
					WriteError(w, http.StatusForbidden, "csrf_failed", "this request was refused because it did not come from the dashboard page; reload the page and try again")
					return
				}
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

// allowedHost: the Host header names this machine's loopback on our port.
func allowedHost(h string, port int) bool {
	hn, p, err := net.SplitHostPort(h)
	if err != nil || p != strconv.Itoa(port) {
		return false
	}
	switch strings.ToLower(hn) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// errorBody is every error response.
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// WriteJSON writes v with status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// WriteError writes an error response.
func WriteError(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, errorBody{Error: msg, Code: code})
}

// ReadJSON decodes the request body into v (an empty body leaves v as is).
func ReadJSON(r *http.Request, v any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// ---- client ----

// ErrNotRunning means no agent answers for this data dir.
var ErrNotRunning = errors.New("the agent for this data folder is not running")

// APIError is an error response from the agent.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message }

// Client calls a running agent.
type Client struct {
	Addr  string
	Token string
	HTTP  *http.Client
}

// NewClient finds the agent for dataDir: its token from the data dir and
// its address from control.json, unless port is non-zero.
func NewClient(dataDir string, port int) (*Client, error) {
	tok, err := ReadToken(dataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotRunning
	}
	if err != nil {
		return nil, fmt.Errorf("read control token: %w", err)
	}
	addr := ""
	if port > 0 {
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	} else if i, err := ReadInfo(dataDir); err == nil && i.Addr != "" {
		addr = i.Addr
	} else {
		return nil, ErrNotRunning
	}
	return &Client{Addr: addr, Token: tok, HTTP: &http.Client{}}, nil
}

// Do sends a request; in (may be nil) is the JSON body, out (may be nil)
// receives the JSON response.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.Addr+Prefix+path, body)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Op == "dial" {
			return ErrNotRunning
		}
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var eb errorBody
		if json.Unmarshal(b, &eb) != nil || eb.Error == "" {
			eb.Error = fmt.Sprintf("agent answered %s", resp.Status)
		}
		return &APIError{Status: resp.StatusCode, Code: eb.Code, Message: eb.Error}
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return durable.WriteFile(path, data, perm)
}
