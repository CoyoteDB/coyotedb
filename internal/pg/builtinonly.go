package pg

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"coyotedb.com/coyote/internal/brand"
)

// Builtin-only (D48): a cluster that mixes kinds of machines may only sort
// and compare text with Postgres' built-in collation provider (C.UTF-8,
// the default of every cluster this program creates, plus "C", "POSIX",
// pg_c_utf8, pg_unicode_fast and ucs_basic). Built-in rules are compiled
// into Postgres itself, so the same Postgres release behaves the same on
// every operating system; ICU and the operating system's rules do not (the
// macOS and Linux builds ship different ICU versions).
//
// Three layers keep it so:
//  1. PrepareBuiltinOnly drops every collation that is not built in from
//     every database (template0 and template1 included) and installs the
//     event triggers of builtinonly.sql, which refuse CREATE COLLATION and
//     any command that makes or changes an object using a collation that
//     is not built in (the predefined ICU "unicode" collation cannot be
//     dropped, so its use is refused there).
//  2. CheckBuiltinOnly, a cheap read-only check that also runs on a copy:
//     every database uses the built-in provider (CREATE DATABASE is
//     invisible to event triggers), the triggers are in place and on, no
//     droppable non-built-in collation is left, and the DDL log position
//     of every database (the guards audit again when it moves).
//  3. The full audit (AuditCluster), as before.

// GuardSchema is the schema the builtin-only objects live in, in every
// database.
const GuardSchema = brand.Command + "_guard"

// Event trigger names (builtinonly.sql).
const (
	guardTriggerCollation = GuardSchema + "_refuse_collation"
	guardTriggerDDL       = GuardSchema + "_check_ddl"
)

//go:embed builtinonly.sql
var builtinOnlySQL string

func builtinSQL() string { return strings.ReplaceAll(builtinOnlySQL, "@guard@", GuardSchema) }

// CheckBuiltinOnlyName is the Finding.Check of builtin-only findings.
const CheckBuiltinOnlyName = "builtin_only"

// housekeeping marks c's session as housekeeping: never wait for a copy
// (with the only copy down in strict mode it would wait forever), do not
// hang on locks, and work even while client writes are refused (a
// drifted main node, SetWritesRefused). Lost in a failover, it is simply
// done again.
func housekeeping(ctx context.Context, c *pgx.Conn) error {
	_, err := c.Exec(ctx, "SET synchronous_commit = local; SET lock_timeout = '10s'; SET default_transaction_read_only = off")
	return err
}

// Housekeeping is housekeeping for callers outside this package.
func Housekeeping(ctx context.Context, c *pgx.Conn) error { return housekeeping(ctx, c) }

// SetWritesRefused turns refusing client writes on this primary on or
// off (default_transaction_read_only through ALTER SYSTEM and a reload:
// every session picks it up before its next transaction). Used for a
// main node whose measurement drifted with no healthy copy to hand over
// to (D48); in a cluster that mixes machines the setting belongs to this
// program. It reports whether anything changed.
func SetWritesRefused(ctx context.Context, c *pgx.Conn, on bool) (bool, error) {
	var src string
	// pg_file_settings reads the files as they are now (a session's own
	// SET, like Housekeeping's, does not hide them).
	if err := c.QueryRow(ctx, `SELECT coalesce((SELECT CASE WHEN setting IN ('on', 'true', '1', 'yes') THEN 'on' ELSE 'off' END
		FROM pg_file_settings WHERE name = 'default_transaction_read_only' AND sourcefile LIKE '%postgresql.auto.conf'
		ORDER BY seqno DESC LIMIT 1), 'off')`).Scan(&src); err != nil {
		return false, err
	}
	if (src == "on") == on {
		return false, nil
	}
	stmt := "ALTER SYSTEM RESET default_transaction_read_only"
	if on {
		stmt = "ALTER SYSTEM SET default_transaction_read_only = on"
	}
	if _, err := c.Exec(ctx, stmt); err != nil {
		return false, err
	}
	if _, err := c.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		return true, err
	}
	return true, nil
}

// InstallBuiltinOnly installs (or refreshes) the builtin-only event
// triggers in c's database. Idempotent. Primary only.
func InstallBuiltinOnly(ctx context.Context, c *pgx.Conn) error {
	if err := housekeeping(ctx, c); err != nil {
		return err
	}
	if _, err := c.Exec(ctx, builtinSQL()); err != nil {
		return fmt.Errorf("install the built-in-sorting guard in %s: %w", c.Config().Database, err)
	}
	return nil
}

// UninstallBuiltinOnly removes the triggers and the guard schema from c's
// database (a cluster that does not mix machines keeps today's freedom
// to use any collation). Idempotent. Primary only.
func UninstallBuiltinOnly(ctx context.Context, c *pgx.Conn) error {
	if err := housekeeping(ctx, c); err != nil {
		return err
	}
	_, err := c.Exec(ctx, "DROP EVENT TRIGGER IF EXISTS "+guardTriggerCollation+"; DROP EVENT TRIGGER IF EXISTS "+guardTriggerDDL+"; DROP SCHEMA IF EXISTS "+GuardSchema+" CASCADE")
	if err != nil {
		return fmt.Errorf("remove the built-in-sorting guard from %s: %w", c.Config().Database, err)
	}
	return nil
}

// BuiltinOnlyInstalled reports whether c's database has both triggers.
func BuiltinOnlyInstalled(ctx context.Context, c *pgx.Conn) (bool, error) {
	var n int
	err := c.QueryRow(ctx, `SELECT count(*) FROM pg_event_trigger WHERE evtname IN ($1, $2)`, guardTriggerCollation, guardTriggerDDL).Scan(&n)
	return n == 2, err
}

// withTemplate0 opens template0 (which normally refuses connections) for
// f, and always closes it again. The guard's objects and the removal of
// imported collations must be in template0 too: CREATE DATABASE ...
// TEMPLATE template0 (pg_restore --create does that) would otherwise
// bring them back.
func withTemplate0(ctx context.Context, connect ConnectFunc, f func(c *pgx.Conn) error) (err error) {
	admin, err := connect(ctx, "postgres")
	if err != nil {
		return err
	}
	defer admin.Close(context.WithoutCancel(ctx))
	if err := housekeeping(ctx, admin); err != nil {
		return err
	}
	if _, err := admin.Exec(ctx, "ALTER DATABASE template0 WITH ALLOW_CONNECTIONS true"); err != nil {
		return fmt.Errorf("open template0: %w", err)
	}
	defer func() {
		if _, cerr := admin.Exec(context.WithoutCancel(ctx), "ALTER DATABASE template0 WITH ALLOW_CONNECTIONS false"); cerr != nil {
			err = errors.Join(err, fmt.Errorf("close template0 again: %w", cerr))
		}
	}()
	c, err := connect(ctx, "template0")
	if err != nil {
		return err
	}
	defer c.Close(context.WithoutCancel(ctx))
	return f(c)
}

// PrepareResult is what PrepareBuiltinOnly did.
type PrepareResult struct {
	Dropped int       // collations removed
	Kept    []Finding // collations kept because something uses them
}

// PrepareBuiltinOnly makes every database builtin-only: drops the
// collations that are not built in (kept and reported if in use) and
// installs the triggers, template0 first when withT0, then template1,
// then the rest. Idempotent. Primary only.
func PrepareBuiltinOnly(ctx context.Context, connect ConnectFunc, withT0 bool) (PrepareResult, error) {
	var res PrepareResult
	one := func(c *pgx.Conn) error {
		if err := housekeeping(ctx, c); err != nil {
			return err
		}
		n, kept, err := DropImportedCollations(ctx, c)
		res.Dropped += n
		res.Kept = append(res.Kept, kept...)
		if err != nil {
			return err
		}
		return InstallBuiltinOnly(ctx, c)
	}
	if withT0 {
		if err := withTemplate0(ctx, connect, one); err != nil {
			return res, fmt.Errorf("template0: %w", err)
		}
	}
	dbs, err := listDatabasesVia(ctx, connect)
	if err != nil {
		return res, err
	}
	for _, db := range dbs {
		c, err := connect(ctx, db)
		if err != nil {
			return res, err
		}
		err = one(c)
		c.Close(context.WithoutCancel(ctx))
		if err != nil {
			return res, fmt.Errorf("%s: %w", db, err)
		}
	}
	return res, nil
}

// UnprepareBuiltinOnly removes the triggers from every database,
// template0 included. Primary only.
func UnprepareBuiltinOnly(ctx context.Context, connect ConnectFunc) error {
	if err := withTemplate0(ctx, connect, func(c *pgx.Conn) error { return UninstallBuiltinOnly(ctx, c) }); err != nil {
		return fmt.Errorf("template0: %w", err)
	}
	dbs, err := listDatabasesVia(ctx, connect)
	if err != nil {
		return err
	}
	for _, db := range dbs {
		c, err := connect(ctx, db)
		if err != nil {
			return err
		}
		err = UninstallBuiltinOnly(ctx, c)
		c.Close(context.WithoutCancel(ctx))
		if err != nil {
			return err
		}
	}
	return nil
}

// listDatabasesVia lists the connectable databases, template1 first.
func listDatabasesVia(ctx context.Context, connect ConnectFunc) ([]string, error) {
	c, err := connect(ctx, "postgres")
	if err != nil {
		return nil, err
	}
	dbs, err := ListDatabases(ctx, c)
	c.Close(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(dbs, func(a, b string) int {
		switch {
		case a == b:
			return 0
		case a == "template1":
			return -1
		case b == "template1":
			return 1
		}
		return 0
	})
	return dbs, nil
}

// BuiltinOnlyState is the result of CheckBuiltinOnly.
type BuiltinOnlyState struct {
	At       time.Time `json:"at"`
	Findings []Finding `json:"findings,omitempty"`
	// DDL is each connectable database's DDL log position (0 when the log
	// is missing). The guards audit again when any of it moves.
	DDL map[string]int64 `json:"ddl"`
}

// Blocking reports whether a finding blocks mixing machines.
func (s BuiltinOnlyState) Blocking() bool { return Blocking(s.Findings) }

// SameDDL reports whether s and o saw the same databases at the same DDL
// log positions (nothing was changed between them).
func (s BuiltinOnlyState) SameDDL(o BuiltinOnlyState) bool {
	if len(s.DDL) != len(o.DDL) {
		return false
	}
	for k, v := range s.DDL {
		if w, ok := o.DDL[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// CheckBuiltinOnly is the quick builtin-only check (milliseconds per
// database). Read-only; works on a copy (standby). template0 accepts no
// connections, so only its provider is checked here; PrepareBuiltinOnly
// cleans it.
func CheckBuiltinOnly(ctx context.Context, connect ConnectFunc) (BuiltinOnlyState, error) {
	s := BuiltinOnlyState{At: time.Now().UTC(), DDL: map[string]int64{}}
	block := func(db, object, problem, fix string) {
		s.Findings = append(s.Findings, Finding{Severity: SeverityBlock, Check: CheckBuiltinOnlyName, Database: db, Object: object, Problem: problem, Fix: fix})
	}
	c, err := connect(ctx, "postgres")
	if err != nil {
		return s, err
	}
	rows, err := c.Query(ctx, `SELECT datname, datlocprovider::text, coalesce(datlocale, ''), datcollate, datctype, datallowconn FROM pg_database ORDER BY datname`)
	if err != nil {
		c.Close(context.WithoutCancel(ctx))
		return s, fmt.Errorf("list databases: %w", err)
	}
	type dbRow struct {
		name, provider, locale, collate, ctype string
		allow                                  bool
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dbRow, error) {
		var d dbRow
		return d, r.Scan(&d.name, &d.provider, &d.locale, &d.collate, &d.ctype, &d.allow)
	})
	c.Close(context.WithoutCancel(ctx))
	if err != nil {
		return s, fmt.Errorf("list databases: %w", err)
	}
	var dbs []string
	for _, d := range all {
		switch {
		case d.provider == "b":
		case d.provider == "c" && (d.collate == "C" || d.collate == "POSIX") && (d.ctype == "C" || d.ctype == "POSIX"):
		case d.provider == "i":
			block(d.name, "database "+d.name, fmt.Sprintf("The database sorts text with ICU rules (%s), which differ between machines.", d.locale),
				"Make a new database without LOCALE_PROVIDER (the built-in C.UTF-8 is the default), move the data over, then drop this one.")
		default:
			block(d.name, "database "+d.name, fmt.Sprintf("The database sorts text with this operating system's rules (%s), which differ between machines.", d.collate),
				"Make a new database without LOCALE_PROVIDER or LC_COLLATE (the built-in C.UTF-8 is the default), move the data over, then drop this one.")
		}
		if d.allow {
			dbs = append(dbs, d.name)
		}
	}
	for _, db := range dbs {
		c, err := connect(ctx, db)
		if err != nil {
			return s, fmt.Errorf("check %s: %w", db, err)
		}
		var trig int
		var leftover []string
		var logged *int64
		err = c.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM pg_event_trigger WHERE evtname IN ($1, $2) AND evtenabled IN ('O', 'A')),
			(SELECT coalesce(array_agg(quote_ident(c.collname) ORDER BY c.collname), '{}') FROM pg_collation c
			 WHERE c.oid >= 12000 AND `+unportableCollation+` AND `+usableCollation+`),
			(SELECT CASE WHEN to_regclass($3) IS NULL THEN NULL ELSE 0 END)`, guardTriggerCollation, guardTriggerDDL, GuardSchema+".ddl_log").Scan(&trig, &leftover, &logged)
		var floatMath []string
		if err == nil && logged != nil {
			err = c.QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM `+GuardSchema+`.ddl_log`).Scan(logged)
		}
		if err == nil && logged != nil {
			// D49, second line behind the DDL trigger (and the only one
			// for objects that existed before the guard was installed).
			floatMath, err = stringList(ctx, c, `SELECT f FROM `+GuardSchema+`.float_math_findings(NULL, NULL) f ORDER BY 1`)
		}
		c.Close(context.WithoutCancel(ctx))
		if err != nil {
			return s, fmt.Errorf("check %s: %w", db, err)
		}
		if logged != nil {
			s.DDL[db] = *logged
		} else {
			s.DDL[db] = 0
		}
		if trig != 2 {
			block(db, "database "+db, "The guard that keeps text sorting built in is missing or turned off in this database.",
				"The main node puts it back by itself within a minute; if this stays, see the program's log.")
		}
		for _, f := range floatMath {
			block(db, f, "It recomputes decimal (floating-point) or geometric maths and compares it with stored results; such maths can differ in the last digit between kinds of machines.",
				"Drop it, or build it on an exact type (numeric, integer). Plain indexes on float columns are fine.")
		}
		if len(leftover) > 0 {
			block(db, "database "+db, fmt.Sprintf("Collations that are not built in exist (%s).", strings.Join(leftover, ", ")),
				"Change what uses them to COLLATE pg_c_utf8 (the audit lists the objects); the main node then removes them.")
		}
	}
	SortFindings(s.Findings)
	return s, nil
}

// WaitReplayed waits until this standby has replayed up to lsn, so its
// catalog shows every change it received. On a primary it returns at
// once.
func (n Node) WaitReplayed(ctx context.Context, lsn LSN, timeout time.Duration) error {
	c, err := n.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	return poll(ctx, timeout, 20*time.Millisecond, "wait for the copy to apply what it received", func(ctx context.Context) (bool, error) {
		var replay *string
		if err := c.QueryRow(ctx, `SELECT CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn()::text ELSE 'primary' END`).Scan(&replay); err != nil {
			return false, err
		}
		if replay == nil {
			return false, nil
		}
		if *replay == "primary" {
			return true, nil
		}
		p, err := ParseLSN(*replay)
		if err != nil {
			return false, err
		}
		return p >= lsn, nil
	})
}
