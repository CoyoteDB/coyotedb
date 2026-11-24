package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
)

// PGVerifier checks a Postgres with the smallest exchange that answers the
// question: startup + authentication, one simple query
// "SELECT pg_is_in_recovery()", terminate. Any role can call
// pg_is_in_recovery(); TLS is not used because the stream it runs over is
// either loopback or the agent's mTLS tunnel.
type PGVerifier struct {
	User     string
	Password string
	Database string // default "postgres"
	// AppName shows in pg_stat_activity. Default "coyote-proxy-check".
	AppName string
}

// String redacts the password.
func (v PGVerifier) String() string { return "PGVerifier{user=" + v.User + ", password=redacted}" }

// LogValue redacts the password in slog output.
func (v PGVerifier) LogValue() slog.Value { return slog.StringValue(v.String()) }

// InRecovery implements Verifier over conn (which it does not close).
func (v PGVerifier) InRecovery(ctx context.Context, conn net.Conn) (bool, error) {
	if v.User == "" {
		return false, errors.New("proxy check: no user configured")
	}
	db := v.Database
	if db == "" {
		db = "postgres"
	}
	app := v.AppName
	if app == "" {
		app = "coyote-proxy-check"
	}
	// Every parameter is explicit so nothing comes from PG* variables; the
	// password goes on the struct, never into a string.
	cs := "host=127.0.0.1 port=5432 sslmode=disable target_session_attrs=any" +
		" user=" + quoteConninfo(v.User) + " dbname=" + quoteConninfo(db)
	cfg, err := pgconn.ParseConfig(cs)
	if err != nil {
		return false, fmt.Errorf("proxy check: %w", err)
	}
	cfg.Password = v.Password
	cfg.TLSConfig = nil
	cfg.Fallbacks = nil
	cfg.RuntimeParams = map[string]string{"application_name": app}
	cfg.LookupFunc = func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }
	var once sync.Once
	cfg.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		var c net.Conn
		once.Do(func() { c = noClose{conn} })
		if c == nil {
			return nil, errors.New("proxy check: connection already used")
		}
		return c, nil
	}
	pc, err := pgconn.ConnectConfig(ctx, cfg)
	if err != nil {
		return false, fmt.Errorf("proxy check: connect: %w", err)
	}
	defer pc.Close(context.WithoutCancel(ctx))
	res := pc.ExecParams(ctx, "SELECT pg_is_in_recovery()", nil, nil, nil, nil).Read()
	if res.Err != nil {
		return false, fmt.Errorf("proxy check: query: %w", res.Err)
	}
	if len(res.Rows) != 1 || len(res.Rows[0]) != 1 {
		return false, errors.New("proxy check: unexpected answer to pg_is_in_recovery()")
	}
	switch string(res.Rows[0][0]) {
	case "t":
		return true, nil
	case "f":
		return false, nil
	}
	return false, fmt.Errorf("proxy check: unexpected value %q", res.Rows[0][0])
}

// noClose keeps pgconn from closing the proxy's conn; the proxy owns it.
type noClose struct{ net.Conn }

func (noClose) Close() error { return nil }

func quoteConninfo(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return "'" + s + "'"
}
