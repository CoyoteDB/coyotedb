package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"coyotedb.com/coyote/internal/pg"
)

// Inspection is everything one node reports after a switch.
type Inspection struct {
	Node       string            `json:"node"`
	OS         string            `json:"os"`
	Arch       string            `json:"arch"`
	At         time.Time         `json:"at"`
	InRecovery bool              `json:"in_recovery"`
	ReplayLSN  string            `json:"replay_lsn"` // pg_last_wal_replay_lsn, or the current LSN on a primary
	Finger     pg.Fingerprint    `json:"fingerprint"`
	Probe      pg.ProbeResult    `json:"probe"`
	Audit      pg.AuditReport    `json:"audit"`
	Verify     pg.VerifyReport   `json:"verify"`
	Amcheck    []pg.Finding      `json:"pg_amcheck,omitempty"`
	Content    ContentDigest     `json:"content"`
	Errors     []string          `json:"errors,omitempty"`
	Durations  map[string]string `json:"durations"`
}

// ContentDigest is a digest of every row of every user table in one
// database, read through the table (index use off), in key order.
type ContentDigest struct {
	Database string                 `json:"database"`
	Tables   map[string]TableDigest `json:"tables"`
	// LargeObjects digests every large object (oid and content).
	LargeObjects string `json:"large_objects"`
	// Sequences maps each sequence to its last value.
	Sequences map[string]string `json:"sequences"`
}

// TableDigest is one table's row count and digest.
type TableDigest struct {
	Rows   int64  `json:"rows"`
	Digest string `json:"digest"`
}

// digestSession makes every value print the same on every machine.
var digestSession = []string{
	"SET LOCAL TimeZone = 'UTC'", "SET LOCAL DateStyle = 'ISO, MDY'", "SET LOCAL IntervalStyle = 'postgres'",
	"SET LOCAL extra_float_digits = 3", "SET LOCAL bytea_output = 'hex'", "SET LOCAL search_path = pg_catalog",
	"SET LOCAL enable_indexscan = off", "SET LOCAL enable_bitmapscan = off", "SET LOCAL enable_indexonlyscan = off",
	"SET LOCAL max_parallel_workers_per_gather = 0", "SET LOCAL statement_timeout = '30min'",
}

// digestContent reads every user table of the connected database in one
// snapshot. Rows are ordered by the primary key (or by the row's text
// when there is none, compared as bytes) and hashed as their text form.
func digestContent(ctx context.Context, c *pgx.Conn) (ContentDigest, error) {
	d := ContentDigest{Database: c.Config().Database, Tables: map[string]TableDigest{}, Sequences: map[string]string{}}
	if _, err := c.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return d, err
	}
	defer func() { _, _ = c.Exec(context.WithoutCancel(ctx), "ROLLBACK") }()
	for _, s := range digestSession {
		if _, err := c.Exec(ctx, s); err != nil {
			return d, err
		}
	}
	type table struct{ name, order string }
	rows, err := c.Query(ctx, `SELECT c.oid::regclass::text,
		coalesce((SELECT string_agg(quote_ident(a.attname), ', ' ORDER BY k.n)
		          FROM pg_index i, unnest(i.indkey) WITH ORDINALITY k(att, n), pg_attribute a
		          WHERE i.indrelid = c.oid AND i.indisprimary AND a.attrelid = c.oid AND a.attnum = k.att), '')
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'm') AND c.relpersistence = 'p' AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%'
		ORDER BY 1`)
	if err != nil {
		return d, err
	}
	tables, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (table, error) {
		var t table
		return t, r.Scan(&t.name, &t.order)
	})
	if err != nil {
		return d, err
	}
	for _, t := range tables {
		order := t.order
		if order == "" {
			order = "x::text COLLATE \"C\""
		}
		td, err := digestQuery(ctx, c, fmt.Sprintf(`SELECT md5(x::text) FROM ONLY %s x ORDER BY %s`, t.name, order))
		if err != nil {
			return d, fmt.Errorf("digest %s: %w", t.name, err)
		}
		d.Tables[t.name] = td
	}
	lo, err := digestQuery(ctx, c, `SELECT oid::text || ':' || md5(lo_get(oid)) FROM pg_largeobject_metadata ORDER BY oid`)
	if err != nil {
		return d, fmt.Errorf("digest large objects: %w", err)
	}
	d.LargeObjects = fmt.Sprintf("%d:%s", lo.Rows, lo.Digest)
	seqs, err := c.Query(ctx, `SELECT c.oid::regclass::text, coalesce(s.last_value::text, '') FROM pg_class c JOIN pg_sequences s ON s.schemaname = c.relnamespace::regnamespace::text AND s.sequencename = c.relname WHERE c.relkind = 'S' ORDER BY 1`)
	if err != nil {
		return d, fmt.Errorf("sequences: %w", err)
	}
	defer seqs.Close()
	for seqs.Next() {
		var name, v string
		if err := seqs.Scan(&name, &v); err != nil {
			return d, err
		}
		d.Sequences[name] = v
	}
	return d, seqs.Err()
}

// digestQuery streams a one-column query and hashes the values in order.
func digestQuery(ctx context.Context, c *pgx.Conn, sql string) (TableDigest, error) {
	rows, err := c.Query(ctx, sql, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return TableDigest{}, err
	}
	defer rows.Close()
	h := sha256.New()
	var n int64
	for rows.Next() {
		v := rows.RawValues()[0]
		h.Write(v)
		h.Write([]byte{'\n'})
		n++
	}
	if err := rows.Err(); err != nil {
		return TableDigest{}, err
	}
	return TableDigest{Rows: n, Digest: hex.EncodeToString(h.Sum(nil))}, nil
}

// inspectOptions controls one inspection.
type inspectOptions struct {
	Database string // the application's database (content digest)
	Level    pg.VerifyLevel
	Amcheck  bool // also run pg_amcheck
	Seed     int64
}

// inspect runs every check on the local node.
func (p *peer) inspect(ctx context.Context, o inspectOptions) Inspection {
	in := Inspection{Node: p.name, OS: runtime.GOOS, Arch: runtime.GOARCH, At: time.Now().UTC(), Durations: map[string]string{}}
	fail := func(what string, err error) { in.Errors = append(in.Errors, what+": "+err.Error()) }
	timed := func(what string, f func() error) {
		start := time.Now()
		if err := f(); err != nil {
			fail(what, err)
		}
		in.Durations[what] = time.Since(start).Round(time.Millisecond).String()
	}
	c, err := p.node.ConnectDB(ctx, "postgres")
	if err != nil {
		fail("connect", err)
		return in
	}
	defer c.Close(context.WithoutCancel(ctx))
	if err := c.QueryRow(ctx, `SELECT pg_is_in_recovery(), coalesce(CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END::text, '')`).Scan(&in.InRecovery, &in.ReplayLSN); err != nil {
		fail("recovery state", err)
	}
	timed("fingerprint", func() (err error) { in.Finger, err = pg.CollectFingerprint(ctx, p.inst, p.node.PGData(), c); return })
	timed("probe", func() (err error) { in.Probe, err = pg.RunProbe(ctx, c, pg.ProbeOptions{}); return })
	timed("audit", func() (err error) { in.Audit, err = pg.AuditCluster(ctx, p.node.ConnectDB, pg.AuditOptions{}); return })
	timed("verify", func() (err error) {
		in.Verify, err = pg.VerifyCluster(ctx, p.node.ConnectDB, pg.VerifyOptions{Level: o.Level, Seed: o.Seed})
		return
	})
	if o.Amcheck {
		timed("pg_amcheck", func() error {
			pf, cleanup, err := p.passfile()
			if err != nil {
				return err
			}
			defer cleanup()
			info, err := p.node.ReadInfo()
			if err != nil {
				return err
			}
			in.Amcheck, err = pg.RunPgAmcheck(ctx, pg.PgAmcheckOptions{Install: p.inst, Port: info.Port, Passfile: pf})
			return err
		})
	}
	timed("content", func() error {
		db := o.Database
		if db == "" {
			db = "postgres"
		}
		dc, err := p.node.ConnectDB(ctx, db)
		if err != nil {
			return err
		}
		defer dc.Close(context.WithoutCancel(ctx))
		in.Content, err = digestContent(ctx, dc)
		return err
	})
	return in
}

// Comparison is the result of comparing every node's inspection.
type Comparison struct {
	OK       bool     `json:"ok"`
	Problems []string `json:"problems,omitempty"`
	// Notes are differences that do not fail the run (informational
	// probe groups, e.g. the maths library).
	Notes []string `json:"notes,omitempty"`
}

// compareInspections checks that every node agrees with the first one
// (the main node) and that none found a problem on its own.
func compareInspections(ins []Inspection) Comparison {
	var c Comparison
	bad := func(format string, a ...any) { c.Problems = append(c.Problems, fmt.Sprintf(format, a...)) }
	for _, in := range ins {
		for _, e := range in.Errors {
			bad("%s: check failed to run: %s", in.Node, e)
		}
		for _, f := range in.Audit.Findings {
			if f.Severity == pg.SeverityBlock {
				bad("%s: audit: %s", in.Node, f)
			}
		}
		for _, f := range in.Verify.Findings {
			if f.Severity != pg.SeverityInfo {
				bad("%s: integrity: %s (%s)", in.Node, f, f.Detail)
			}
		}
		for _, f := range in.Amcheck {
			bad("%s: pg_amcheck: %s (%s)", in.Node, f, f.Detail)
		}
	}
	if len(ins) < 2 {
		c.OK = len(c.Problems) == 0
		return c
	}
	ref := ins[0]
	for _, in := range ins[1:] {
		for _, d := range pg.CompareFingerprints(ref.Finger, in.Finger) {
			bad("%s vs %s: fingerprint: %s", ref.Node, in.Node, d.Text)
		}
		for _, d := range pg.CompareProbes(ref.Probe, in.Probe) {
			if d.MustMatch {
				bad("%s vs %s: probe %s/%s: %s", ref.Node, in.Node, d.Group, d.Statement, d.Text)
			} else {
				c.Notes = append(c.Notes, fmt.Sprintf("%s vs %s: probe %s/%s: %s", ref.Node, in.Node, d.Group, d.Statement, d.Text))
			}
		}
		if ref.Audit.Digest() != in.Audit.Digest() {
			bad("%s vs %s: the audits found different things", ref.Node, in.Node)
		}
		for _, p := range compareContent(ref.Content, in.Content) {
			bad("%s vs %s: content: %s", ref.Node, in.Node, p)
		}
	}
	c.OK = len(c.Problems) == 0
	return c
}

// compareContent lists the tables, large objects and sequences that
// differ between two content digests; a is the main node.
func compareContent(a, b ContentDigest) []string {
	var out []string
	names := map[string]bool{}
	for n := range a.Tables {
		names[n] = true
	}
	for n := range b.Tables {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	slices.Sort(sorted)
	for _, n := range sorted {
		ta, okA := a.Tables[n]
		tb, okB := b.Tables[n]
		switch {
		case !okA || !okB:
			out = append(out, fmt.Sprintf("table %s exists on only one node", n))
		case ta.Rows != tb.Rows:
			out = append(out, fmt.Sprintf("table %s has %d rows on one node and %d on the other", n, ta.Rows, tb.Rows))
		case ta.Digest != tb.Digest:
			out = append(out, fmt.Sprintf("table %s has the same number of rows (%d) but different content", n, ta.Rows))
		}
	}
	if a.LargeObjects != b.LargeObjects {
		out = append(out, "large objects differ")
	}
	// Postgres writes sequences to the change log ahead of use (32
	// values), so a copy may be ahead of the main node, never behind: a
	// copy that became the main node would hand out a used number.
	for _, n := range sortedKeys(a.Sequences, b.Sequences) {
		va, okA := a.Sequences[n]
		vb, okB := b.Sequences[n]
		if !okA || !okB {
			out = append(out, fmt.Sprintf("sequence %s exists on only one node", n))
			continue
		}
		ia, errA := strconv.ParseInt(va, 10, 64)
		ib, errB := strconv.ParseInt(vb, 10, 64)
		switch {
		case va == "" || vb == "":
			// never used on one side (NULL last_value): nothing to compare
		case errA != nil || errB != nil:
			out = append(out, fmt.Sprintf("sequence %s: unreadable values %q, %q", n, va, vb))
		case ib < ia:
			out = append(out, fmt.Sprintf("sequence %s is behind on the copy (%d, main node %d)", n, ib, ia))
		}
	}
	return out
}

func sortedKeys(ms ...map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range ms {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	slices.Sort(out)
	return out
}

// shortErr keeps an error to one line.
func shortErr(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
