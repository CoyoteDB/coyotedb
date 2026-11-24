package pg

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// adminAppName marks the agent's own sessions in pg_stat_activity.
const adminAppName = "coyote-agent"

// dial opens a pgx connection with every connection parameter explicit.
// The password is set on the config struct, never in a string that could
// end up in an error message or log.
func dial(ctx context.Context, host string, port int, user, password, db string) (*pgx.Conn, error) {
	cs := "host=" + quoteConninfo(host) + " port=" + strconv.Itoa(port) +
		" user=" + quoteConninfo(user) + " dbname=" + quoteConninfo(db) +
		" sslmode=disable connect_timeout=10"
	cfg, err := pgx.ParseConfig(cs)
	if err != nil {
		return nil, err
	}
	cfg.Password = password
	cfg.RuntimeParams["application_name"] = adminAppName
	cfg.ConnectTimeout = 10 * time.Second
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to %s as %s: %w", Upstream{host, port}, user, err)
	}
	return c, nil
}

// Connect opens a superuser connection to this node's own Postgres on
// 127.0.0.1, using the port in node.json and the stored password.
func (n Node) Connect(ctx context.Context) (*pgx.Conn, error) {
	info, err := n.ReadInfo()
	if err != nil {
		return nil, err
	}
	pw, err := n.Password()
	if err != nil {
		return nil, err
	}
	return dial(ctx, "127.0.0.1", info.Port, Superuser, pw, "postgres")
}

// connectUpstream opens a normal (non-replication) SQL connection to an
// upstream as user, with the password taken from s.
func connectUpstream(ctx context.Context, up Upstream, user string, s Secrets) (*pgx.Conn, error) {
	pw := ""
	switch user {
	case ReplUser:
		pw = s.Replication
	case RewindUser:
		pw = s.Rewind
	case Superuser:
		pw = s.Superuser
	default:
		return nil, fmt.Errorf("no password for role %q", user)
	}
	return dial(ctx, up.Host, up.Port, user, pw, "postgres")
}

// poll calls f every interval until it reports done, returns an error, or
// the deadline passes. It never sleeps past the deadline.
func poll(ctx context.Context, timeout, interval time.Duration, what string, f func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastErr error
	for {
		done, err := f(ctx)
		if err != nil && ctx.Err() == nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("%s: timed out after %s: %w", what, timeout, lastErr)
			}
			return fmt.Errorf("%s: timed out after %s", what, timeout)
		case <-t.C:
		}
		lastErr = err
	}
}
