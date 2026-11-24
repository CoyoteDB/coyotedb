package migrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Tables up to this size get a checksum of every row; bigger ones get a
// checksum of the first and last sampleRows rows by primary key (when the
// key sorts the same everywhere), plus an exact row count.
const (
	fullChecksumBytes = 256 << 20
	sampleRows        = 5000
)

// stableOutput makes every value print the same way on the source and the
// cluster, whatever their settings (time zone, date style, float digits).
var stableOutput = []string{
	"SET TimeZone = 'UTC'",
	"SET DateStyle = 'ISO, YMD'",
	"SET IntervalStyle = 'postgres'",
	"SET extra_float_digits = 1",
	"SET bytea_output = 'hex'",
	"SET statement_timeout = 0",
}

// localOutput is stableOutput for a transaction (the source's snapshot).
func localOutput() []string {
	out := make([]string, len(stableOutput))
	for i, s := range stableOutput {
		out[i] = strings.Replace(s, "SET ", "SET LOCAL ", 1)
	}
	return out
}

// tableStats is what is compared per table.
type tableStats struct {
	Rows int64
	// Sum is the sum of a 60-bit hash of every checked row's text form
	// (order does not matter), or "" when only rows are counted.
	Sum string
	// How: "every row", "first and last 5000 rows" or "row count".
	How string
}

// checkMode picks how a table is compared.
func checkMode(t table) string {
	switch {
	case t.Size <= fullChecksumBytes:
		return "every row"
	case len(t.PK) > 0 && t.PKPlain:
		return fmt.Sprintf("first and last %d rows", sampleRows)
	default:
		return "row count"
	}
}

// rowText is the row's text form, with money as numeric (money prints by
// the server's locale).
func rowText(t table) string {
	parts := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		col := "t." + pgx.Identifier{c.Name}.Sanitize()
		if c.Money {
			col += "::numeric"
		}
		parts = append(parts, col)
	}
	if len(parts) == 0 {
		return "''"
	}
	return "ROW(" + strings.Join(parts, ", ") + ")::text"
}

func hashSum(t table) string {
	return "coalesce(sum(('x' || substr(md5(" + rowText(t) + "), 1, 15))::bit(60)::bigint), 0)::text"
}

func pkOrder(t table, desc bool) string {
	parts := make([]string, 0, len(t.PK))
	for _, c := range t.PK {
		p := "t." + pgx.Identifier{c}.Sanitize()
		if desc {
			p += " DESC"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

// statsQueries returns the queries for t: the count query and, if the
// table gets a checksum, the checksum query (which may count too).
func statsQuery(t table) string {
	q := t.qualified()
	switch checkMode(t) {
	case "every row":
		return "SELECT count(*), " + hashSum(t) + " FROM ONLY " + q + " t"
	case "row count":
		return "SELECT count(*), '' FROM ONLY " + q + " t"
	default:
		sample := func(desc bool) string {
			return fmt.Sprintf("(SELECT * FROM ONLY %s t ORDER BY %s LIMIT %d)", q, pkOrder(t, desc), sampleRows)
		}
		return "SELECT (SELECT count(*) FROM ONLY " + q + "), " + hashSum(t) +
			" FROM (" + sample(false) + " UNION ALL " + sample(true) + ") t"
	}
}

func readStats(ctx context.Context, q pgx.Row, t table) (tableStats, error) {
	st := tableStats{How: checkMode(t)}
	if err := q.Scan(&st.Rows, &st.Sum); err != nil {
		return st, fmt.Errorf("check table %s: %w", t.label(), cleanErr(err))
	}
	return st, nil
}

// mismatch compares source and copy in plain words ("" when equal).
func mismatch(t table, src, dst tableStats) string {
	switch {
	case src.Rows != dst.Rows:
		return fmt.Sprintf("%s: %d rows on the source, %d in the copy", t.label(), src.Rows, dst.Rows)
	case src.Sum != dst.Sum:
		return fmt.Sprintf("%s: same number of rows (%d) but different contents (%s checked)", t.label(), src.Rows, src.How)
	}
	return ""
}

// sequenceFix is one sequence moved past its column's highest value.
type sequenceFix struct {
	Sequence, Column string
	From, To         int64
}

// fixSequences makes every sequence that feeds a column (serial, identity,
// OWNED BY) hand out values above the column's current maximum, so the
// app's next insert never collides with a copied row.
func fixSequences(ctx context.Context, c *pgx.Conn) ([]sequenceFix, int, error) {
	rows, err := c.Query(ctx, `
	SELECT s.oid::regclass::text, quote_ident(n.nspname) || '.' || quote_ident(t.relname), quote_ident(a.attname),
	       n.nspname || '.' || t.relname || '.' || a.attname, ps.seqincrement
	  FROM pg_depend d
	  JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
	  JOIN pg_class t ON t.oid = d.refobjid
	  JOIN pg_namespace n ON n.oid = t.relnamespace
	  JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
	  JOIN pg_sequence ps ON ps.seqrelid = s.oid
	 WHERE d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass
	   AND d.deptype IN ('a', 'i') AND t.relkind IN ('r', 'p')
	   AND a.atttypid IN ('int2'::regtype, 'int4'::regtype, 'int8'::regtype)`)
	if err != nil {
		return nil, 0, fmt.Errorf("list sequences: %w", err)
	}
	type seqCol struct {
		Seq, Table, Col, Label string
		Inc                    int64
	}
	cols, err := pgx.CollectRows(rows, pgx.RowToStructByPos[seqCol])
	if err != nil {
		return nil, 0, fmt.Errorf("list sequences: %w", err)
	}
	var fixes []sequenceFix
	for _, sc := range cols {
		if sc.Inc < 0 {
			continue // counts down: "past the highest" does not apply
		}
		var maxv *int64
		if err := c.QueryRow(ctx, "SELECT max("+sc.Col+")::int8 FROM "+sc.Table).Scan(&maxv); err != nil {
			return nil, 0, fmt.Errorf("read the highest %s: %w", sc.Label, cleanErr(err))
		}
		if maxv == nil {
			continue
		}
		var last int64
		var called bool
		if err := c.QueryRow(ctx, "SELECT last_value, is_called FROM "+sc.Seq).Scan(&last, &called); err != nil {
			return nil, 0, fmt.Errorf("read sequence %s: %w", sc.Seq, cleanErr(err))
		}
		next := last
		if called {
			next = last + sc.Inc
		}
		if next > *maxv {
			continue
		}
		if _, err := c.Exec(ctx, "SELECT setval($1::regclass, $2, true)", sc.Seq, *maxv); err != nil {
			return nil, 0, fmt.Errorf("move sequence %s past %d: %w", sc.Seq, *maxv, cleanErr(err))
		}
		fixes = append(fixes, sequenceFix{Sequence: sc.Seq, Column: sc.Label, From: last, To: *maxv})
	}
	return fixes, len(cols), nil
}
