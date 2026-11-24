package migrate

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// objectInUse is SQLSTATE 55006: another session is connected to the
// database (the agent's own checks visit every database now and then).
const objectInUse = "55006"

// retryInUse runs f until it stops failing with "in use", for up to a
// minute, ending this role's own sessions on db before each try (the app
// role may end its own sessions; the agent's sessions end by themselves).
func retryInUse(ctx context.Context, c *pgx.Conn, db string, f func() error) error {
	deadline := time.Now().Add(time.Minute)
	for {
		if db != "" {
			_, _ = c.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			   WHERE datname = $1 AND usename = current_user AND pid <> pg_backend_pid()`, db)
		}
		err := f()
		if err == nil || sqlState(err) != objectInUse || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func ident(s string) string { return pgx.Identifier{s}.Sanitize() }

func dropDatabase(ctx context.Context, c *pgx.Conn, db string) error {
	return retryInUse(ctx, c, db, func() error {
		_, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+ident(db))
		return err
	})
}

func renameDatabase(ctx context.Context, c *pgx.Conn, from, to string) error {
	return retryInUse(ctx, c, from, func() error {
		_, err := c.Exec(ctx, "ALTER DATABASE "+ident(from)+" RENAME TO "+ident(to))
		return err
	})
}

func databaseExists(ctx context.Context, c *pgx.Conn, db string) (bool, error) {
	var ok bool
	err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", db).Scan(&ok)
	return ok, err
}

// prepareStaging clears what an earlier, interrupted run left and creates
// an empty <db>__migrating. It is made from template1, which carries what
// the cluster installs in every database (amcheck) and none of the
// operating system's sort rules.
func (m *mover) prepareStaging(ctx context.Context, c *pgx.Conn) error {
	db := m.o.Target.Database
	staging, old := stagingName(db), replacedName(db)
	if m.tgtInfo.staging {
		m.say("  removing an unfinished copy left by an earlier run (%s)", staging)
		if err := dropDatabase(ctx, c, staging); err != nil {
			return fmt.Errorf("remove the unfinished copy %s: %w", staging, cleanErr(err))
		}
	}
	if m.tgtInfo.replaced {
		has, err := databaseExists(ctx, c, db)
		if err != nil {
			return err
		}
		if has {
			// The earlier run switched in its copy; only the old
			// contents were not deleted yet.
			if err := dropDatabase(ctx, c, old); err != nil {
				return fmt.Errorf("remove %s left by an earlier run: %w", old, cleanErr(err))
			}
		} else {
			// It stopped between the two renames: put the old contents
			// back under their name first.
			m.say("  putting back database %s (an earlier run stopped while switching)", db)
			if err := renameDatabase(ctx, c, old, db); err != nil {
				return fmt.Errorf("put back database %s: %w", db, cleanErr(err))
			}
		}
	}
	err := retryInUse(ctx, c, "", func() error {
		_, err := c.Exec(ctx, "CREATE DATABASE "+ident(staging)+" TEMPLATE template1")
		return err
	})
	if err != nil {
		return fmt.Errorf("create database %s on the cluster: %w", staging, cleanErr(err))
	}
	_, err = c.Exec(ctx, "COMMENT ON DATABASE "+ident(staging)+" IS 'Unfinished copy made by coyote migrate. Not in use; safe to drop.'")
	return err
}

// switchIn gives the checked copy the real name: the old database (if any)
// is renamed aside, the copy renamed into place, then the old one dropped.
// Between the two renames the name is briefly free; a run interrupted
// there is repaired by the next run (prepareStaging).
func (m *mover) switchIn(ctx context.Context, c *pgx.Conn) error {
	db := m.o.Target.Database
	staging, old := stagingName(db), replacedName(db)
	has, err := databaseExists(ctx, c, db)
	if err != nil {
		return err
	}
	if has {
		if err := renameDatabase(ctx, c, db, old); err != nil {
			return fmt.Errorf("move database %s aside (are other programs still connected to it?): %w", db, cleanErr(err))
		}
	}
	if err := renameDatabase(ctx, c, staging, db); err != nil {
		if has {
			if rerr := renameDatabase(ctx, c, old, db); rerr != nil {
				m.logf("could not put %s back: %v", db, rerr)
			}
		}
		return fmt.Errorf("give the copy the name %s: %w", db, cleanErr(err))
	}
	comment := movedComment + time.Now().UTC().Format("2006-01-02") + "."
	if _, err := c.Exec(ctx, "COMMENT ON DATABASE "+ident(db)+" IS "+quoteLiteral(comment)); err != nil {
		m.logf("comment on database: %v", err)
	}
	if has {
		if err := dropDatabase(ctx, c, old); err != nil {
			m.say("  Note: the old contents are still in database %s (it could not be deleted: %v). Delete it later with: DROP DATABASE %s;", old, cleanErr(err), ident(old))
		}
	}
	return nil
}

func quoteLiteral(s string) string {
	out := []byte{'\''}
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, s[i])
	}
	return string(append(out, '\''))
}
