package agent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Dashboard sign-in (dashboard/API.md §2, D36). `coyote dashboard` asks
// for a one-time login token over the bearer-protected control channel;
// the browser exchanges it for a session cookie. Only SHA-256 hashes of
// tokens and session ids are kept, in memory: nothing survives a restart,
// nothing is written to disk or Raft, nothing is logged.

const (
	loginTokenTTL    = 120 * time.Second
	sessionIdle      = 12 * time.Hour
	sessionAbsolute  = 7 * 24 * time.Hour
	maxLoginTokens   = 64
	maxSessions      = 256
	sessionCookieFmt = "coyote_session_"
)

type sessionInfo struct {
	csrf    string
	created time.Time
	used    time.Time
}

// sessionStore implements control.SessionValidator for the dashboard.
type sessionStore struct {
	port int
	now  func() time.Time

	mu       sync.Mutex
	logins   map[[32]byte]time.Time // hash → expiry
	sessions map[[32]byte]*sessionInfo
}

func newSessionStore(port int) *sessionStore {
	return &sessionStore{port: port, now: time.Now, logins: map[[32]byte]time.Time{}, sessions: map[[32]byte]*sessionInfo{}}
}

// CookieName carries the API port: browsers share cookies across ports of
// one host, and local test clusters run several agents on one machine.
func (s *sessionStore) CookieName() string { return sessionCookieFmt + strconv.Itoa(s.port) }

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(t string) [32]byte { return sha256.Sum256([]byte(t)) }

// mintLogin creates a one-time login token valid for loginTokenTTL.
func (s *sessionStore) mintLogin() (string, time.Time, error) {
	tok, err := randomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now()
	exp := now.Add(loginTokenTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)
	if len(s.logins) >= maxLoginTokens {
		// Drop the one closest to expiry: someone is minting links in a loop.
		var oldest [32]byte
		var at time.Time
		for h, e := range s.logins {
			if at.IsZero() || e.Before(at) {
				oldest, at = h, e
			}
		}
		delete(s.logins, oldest)
	}
	s.logins[hashToken(tok)] = exp
	return tok, exp, nil
}

// exchange uses up a login token and opens a session. It returns the
// session id (the cookie value) and its info; ok is false when the token
// is unknown, used or expired.
func (s *sessionStore) exchange(tok string) (id string, info sessionInfo, ok bool, err error) {
	now := s.now()
	h := hashToken(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)
	// A map lookup by SHA-256 leaks nothing useful about the token.
	_, found := s.logins[h]
	if !found || tok == "" {
		return "", sessionInfo{}, false, nil
	}
	delete(s.logins, h)
	id, err = randomToken()
	if err != nil {
		return "", sessionInfo{}, false, err
	}
	csrf, err := randomToken()
	if err != nil {
		return "", sessionInfo{}, false, err
	}
	if len(s.sessions) >= maxSessions {
		var oldest [32]byte
		var at time.Time
		for k, v := range s.sessions {
			if at.IsZero() || v.used.Before(at) {
				oldest, at = k, v.used
			}
		}
		delete(s.sessions, oldest)
	}
	si := &sessionInfo{csrf: csrf, created: now, used: now}
	s.sessions[hashToken(id)] = si
	return id, *si, true, nil
}

// lookup finds the request's session and refreshes its idle timer.
func (s *sessionStore) lookup(r *http.Request) (sessionInfo, bool) {
	c, err := r.Cookie(s.CookieName())
	if err != nil || c.Value == "" {
		return sessionInfo{}, false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	si, ok := s.sessions[hashToken(c.Value)]
	if !ok || !s.liveLocked(si, now) {
		return sessionInfo{}, false
	}
	si.used = now
	return *si, true
}

func (s *sessionStore) end(r *http.Request) {
	c, err := r.Cookie(s.CookieName())
	if err != nil {
		return
	}
	s.mu.Lock()
	delete(s.sessions, hashToken(c.Value))
	s.mu.Unlock()
}

func (s *sessionStore) liveLocked(si *sessionInfo, now time.Time) bool {
	return now.Sub(si.used) < sessionIdle && now.Sub(si.created) < sessionAbsolute
}

func (s *sessionStore) expireLocked(now time.Time) {
	for k, e := range s.logins {
		if !now.Before(e) {
			delete(s.logins, k)
		}
	}
	for k, si := range s.sessions {
		if !s.liveLocked(si, now) {
			delete(s.sessions, k)
		}
	}
}

// expiries of a session as API.md's Session body states them.
func (si sessionInfo) idleExpiry() time.Time {
	idle, abs := si.used.Add(sessionIdle), si.created.Add(sessionAbsolute)
	if idle.Before(abs) {
		return idle
	}
	return abs
}

// ---- control.SessionValidator ----

// Public: the dashboard's static files (any GET outside /v1/) and the
// exchange of a sign-in token for a session.
func (s *sessionStore) Public(r *http.Request) bool {
	p := r.URL.Path
	if r.Method == http.MethodPost && p == "/v1/session" {
		return true
	}
	return (r.Method == http.MethodGet || r.Method == http.MethodHead) && p != "/v1" && !strings.HasPrefix(p, "/v1/")
}

// BearerOnly: CLI scaffolding, minting sign-in links, and GET routes that
// return secrets (the dashboard reads the password only through the POST
// /v1/connection/reveal, which needs the CSRF token).
func (s *sessionStore) BearerOnly(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/v1/dev/"), p == "/v1/session/links", p == "/v1/conninfo":
		return true
	}
	return false
}

// Session reports the request's session and its CSRF token.
func (s *sessionStore) Session(r *http.Request) (string, bool) {
	si, ok := s.lookup(r)
	return si.csrf, ok
}
