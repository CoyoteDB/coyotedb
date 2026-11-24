package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSessions accepts the cookie "s=good" (CSRF token "c1"); /v1/session
// is public and /v1/dev/* is bearer only.
type fakeSessions struct{}

func (fakeSessions) Public(r *http.Request) bool {
	return r.URL.Path == "/v1/session" || r.URL.Path == "/"
}
func (fakeSessions) BearerOnly(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/v1/dev/")
}
func (fakeSessions) Session(r *http.Request) (string, bool) {
	c, err := r.Cookie("s")
	if err != nil || c.Value != "good" {
		return "", false
	}
	return "c1", true
}

func TestGuardBrowserSession(t *testing.T) {
	const tok, port = "tok", 7480
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	withSess := guard(tok, port, fakeSessions{}, false, ok)
	plain := guard(tok, port, nil, false, ok)
	// --dashboard-listen (D52).
	remoteOK := guard(tok, port, fakeSessions{}, true, ok)
	remoteNoSess := guard(tok, port, nil, true, ok)

	type req struct {
		name, method, path, remote, host, origin, csrf, cookie, auth string
		h                                                            http.Handler
		status                                                       int
		code                                                         string
	}
	const local, remote, host = "127.0.0.1:50000", "192.168.1.9:50000", "127.0.0.1:7480"
	cases := []req{
		// Bearer path unchanged, with and without the option.
		{name: "bearer ok", method: "POST", path: "/v1/switch", remote: local, host: host, auth: "Bearer tok", h: plain, status: 200},
		{name: "bearer ok with sessions", method: "POST", path: "/v1/dev/switch", remote: local, host: "whatever:1", auth: "Bearer tok", h: withSess, status: 200},
		{name: "bearer wrong", method: "GET", path: "/v1/status", remote: local, host: host, auth: "Bearer nope", h: withSess, status: 401, code: "unauthorized"},
		{name: "no credential without option", method: "GET", path: "/v1/status", remote: local, host: host, h: plain, status: 401, code: "unauthorized"},
		{name: "cookie ignored without option", method: "GET", path: "/v1/status", remote: local, host: host, cookie: "good", h: plain, status: 401, code: "unauthorized"},
		{name: "bearer from remote", method: "GET", path: "/v1/status", remote: remote, host: host, auth: "Bearer tok", h: withSess, status: 403, code: "forbidden"},

		// Cookie path.
		{name: "cookie get", method: "GET", path: "/v1/status", remote: local, host: host, cookie: "good", h: withSess, status: 200},
		{name: "cookie localhost host", method: "GET", path: "/v1/status", remote: "[::1]:5", host: "localhost:7480", cookie: "good", h: withSess, status: 200},
		{name: "valid cookie from remote", method: "GET", path: "/v1/status", remote: remote, host: host, cookie: "good", h: withSess, status: 403, code: "forbidden"},
		{name: "bad host", method: "GET", path: "/v1/status", remote: local, host: "evil.example:7480", cookie: "good", h: withSess, status: 403, code: "bad_host"},
		{name: "wrong port in host", method: "GET", path: "/v1/status", remote: local, host: "127.0.0.1:7481", cookie: "good", h: withSess, status: 403, code: "bad_host"},
		{name: "bad host on static", method: "GET", path: "/", remote: local, host: "rebind.example:7480", h: withSess, status: 403, code: "bad_host"},
		{name: "no cookie", method: "GET", path: "/v1/status", remote: local, host: host, h: withSess, status: 401, code: "unauthenticated"},
		{name: "bad cookie", method: "GET", path: "/v1/status", remote: local, host: host, cookie: "bad", h: withSess, status: 401, code: "unauthenticated"},
		{name: "post no csrf", method: "POST", path: "/v1/switch", remote: local, host: host, origin: "http://" + host, cookie: "good", h: withSess, status: 403, code: "csrf_failed"},
		{name: "post wrong csrf", method: "POST", path: "/v1/switch", remote: local, host: host, origin: "http://" + host, csrf: "c2", cookie: "good", h: withSess, status: 403, code: "csrf_failed"},
		{name: "post no origin", method: "POST", path: "/v1/switch", remote: local, host: host, csrf: "c1", cookie: "good", h: withSess, status: 403, code: "csrf_failed"},
		{name: "post foreign origin", method: "DELETE", path: "/v1/members/x", remote: local, host: host, origin: "http://evil.example", csrf: "c1", cookie: "good", h: withSess, status: 403, code: "csrf_failed"},
		{name: "post ok", method: "POST", path: "/v1/switch", remote: local, host: host, origin: "http://" + host, csrf: "c1", cookie: "good", h: withSess, status: 200},
		{name: "bearer only refused for cookie", method: "GET", path: "/v1/dev/status", remote: local, host: host, cookie: "good", h: withSess, status: 403, code: "bearer_only"},

		// Public routes.
		{name: "public static", method: "GET", path: "/", remote: local, host: host, h: withSess, status: 200},
		{name: "public static from remote", method: "GET", path: "/", remote: remote, host: host, h: withSess, status: 403, code: "forbidden"},
		{name: "public sign-in no origin", method: "POST", path: "/v1/session", remote: local, host: host, h: withSess, status: 200},
		{name: "public sign-in same origin", method: "POST", path: "/v1/session", remote: local, host: host, origin: "http://" + host, h: withSess, status: 200},
		{name: "public sign-in foreign origin", method: "POST", path: "/v1/session", remote: local, host: host, origin: "http://evil.example", h: withSess, status: 403, code: "csrf_failed"},

		// WithRemoteBrowsers: a browser on the container's host, through
		// the bridge gateway; the bearer path stays loopback-only.
		{name: "remote: cookie from bridge gateway", method: "GET", path: "/v1/status", remote: "172.17.0.1:40000", host: host, cookie: "good", h: remoteOK, status: 200},
		{name: "remote: static from bridge gateway", method: "GET", path: "/", remote: "172.17.0.1:40000", host: host, h: remoteOK, status: 200},
		{name: "remote: sign-in from bridge gateway", method: "POST", path: "/v1/session", remote: "192.168.65.1:40000", host: host, origin: "http://" + host, h: remoteOK, status: 200},
		{name: "remote: post with csrf", method: "POST", path: "/v1/switch", remote: "172.17.0.1:40000", host: host, origin: "http://" + host, csrf: "c1", cookie: "good", h: remoteOK, status: 200},
		{name: "remote: post without csrf", method: "POST", path: "/v1/switch", remote: "172.17.0.1:40000", host: host, origin: "http://" + host, cookie: "good", h: remoteOK, status: 403, code: "csrf_failed"},
		{name: "remote: no session", method: "GET", path: "/v1/status", remote: "172.17.0.1:40000", host: host, h: remoteOK, status: 401, code: "unauthenticated"},
		{name: "remote: bearer refused", method: "GET", path: "/v1/status", remote: "172.17.0.1:40000", host: host, auth: "Bearer tok", h: remoteOK, status: 403, code: "forbidden"},
		{name: "remote: sign-in link minting refused", method: "POST", path: "/v1/session/links", remote: "172.17.0.1:40000", host: host, auth: "Bearer tok", h: remoteOK, status: 403, code: "forbidden"},
		{name: "remote: bearer only for cookie", method: "GET", path: "/v1/dev/status", remote: "172.17.0.1:40000", host: host, cookie: "good", h: remoteOK, status: 403, code: "bearer_only"},
		{name: "remote: LAN host name refused", method: "GET", path: "/v1/status", remote: "192.168.1.9:50000", host: "192.168.1.20:7480", cookie: "good", h: remoteOK, status: 403, code: "bad_host"},
		{name: "remote: public peer refused", method: "GET", path: "/v1/status", remote: "203.0.113.7:50000", host: host, cookie: "good", h: remoteOK, status: 403, code: "forbidden"},
		{name: "remote: needs sessions", method: "GET", path: "/v1/status", remote: "172.17.0.1:40000", host: host, auth: "Bearer tok", h: remoteNoSess, status: 403, code: "forbidden"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "http://"+c.host+c.path, nil)
			r.RemoteAddr, r.Host = c.remote, c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.csrf != "" {
				r.Header.Set(CSRFHeader, c.csrf)
			}
			if c.cookie != "" {
				r.AddCookie(&http.Cookie{Name: "s", Value: c.cookie})
			}
			if c.auth != "" {
				r.Header.Set("Authorization", c.auth)
			}
			w := httptest.NewRecorder()
			c.h.ServeHTTP(w, r)
			if w.Code != c.status {
				t.Fatalf("status %d, want %d (%s)", w.Code, c.status, w.Body.String())
			}
			if c.code != "" {
				var eb errorBody
				if err := json.Unmarshal(w.Body.Bytes(), &eb); err != nil || eb.Code != c.code {
					t.Fatalf("code %q (%v), want %q", eb.Code, err, c.code)
				}
			}
		})
	}
}
