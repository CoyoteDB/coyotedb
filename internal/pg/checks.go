package pg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Shared pieces of the mixed-system guards (docs/MIXED_OS.md, safety rule
// 1): the fingerprint, the behaviour probe, the portability audit and the
// integrity check all read through a Querier and report Findings.

// Querier is what the checks need from a connection. *pgx.Conn and pgx.Tx
// both satisfy it.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Severity of a Finding.
type Severity string

const (
	// SeverityBlock: this must be fixed before machines of different kinds
	// may share the data (and, for integrity findings, before anything
	// else: data is damaged).
	SeverityBlock Severity = "block"
	// SeverityWarn: probably fine, but a person should look.
	SeverityWarn Severity = "warn"
	// SeverityInfo: recorded for the report; nothing to do.
	SeverityInfo Severity = "info"
)

func severityRank(s Severity) int {
	switch s {
	case SeverityBlock:
		return 0
	case SeverityWarn:
		return 1
	}
	return 2
}

// Finding is one result of the portability audit or the integrity check.
type Finding struct {
	Severity Severity `json:"severity"`
	// Check is a stable machine name, e.g. "collation", "libm_expr",
	// "btree", "heap".
	Check string `json:"check"`
	// Database the object lives in ("" for cluster-wide findings).
	Database string `json:"database,omitempty"`
	// Object names the thing, e.g. `index public.t_exp` or
	// `column public.t.name`.
	Object string `json:"object,omitempty"`
	// Problem is what is wrong, in plain words.
	Problem string `json:"problem"`
	// Fix is what to do about it, in plain words.
	Fix string `json:"fix,omitempty"`
	// Detail is technical detail for support (error text, SQL, values).
	Detail string `json:"detail,omitempty"`
}

func (f Finding) String() string {
	var b strings.Builder
	b.WriteString(string(f.Severity))
	b.WriteString(": ")
	if f.Database != "" {
		fmt.Fprintf(&b, "[%s] ", f.Database)
	}
	if f.Object != "" {
		b.WriteString(f.Object)
		b.WriteString(": ")
	}
	b.WriteString(f.Problem)
	if f.Fix != "" {
		b.WriteString(" Fix: ")
		b.WriteString(f.Fix)
	}
	return b.String()
}

// SortFindings orders findings: most severe first, then by database, check
// and object, so reports are stable and easy to diff.
func SortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
			return ra < rb
		}
		if a.Database != b.Database {
			return a.Database < b.Database
		}
		if a.Check != b.Check {
			return a.Check < b.Check
		}
		if a.Object != b.Object {
			return a.Object < b.Object
		}
		return a.Problem < b.Problem
	})
}

// Blocking reports whether any finding has SeverityBlock.
func Blocking(fs []Finding) bool {
	for _, f := range fs {
		if f.Severity == SeverityBlock {
			return true
		}
	}
	return false
}

// sqlState returns the SQLSTATE of a Postgres error ("" if err is not one).
func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// quoteIdent quotes a SQL identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// ListDatabases returns the databases that accept connections, sorted.
func ListDatabases(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT datname FROM pg_database WHERE datallowconn ORDER BY datname`)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	dbs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	return dbs, nil
}

// ConnectFunc opens a superuser connection to the named database of the
// node under check. The checks close what they open.
type ConnectFunc func(ctx context.Context, database string) (*pgx.Conn, error)

// ConnectDB is a ConnectFunc for this node's own Postgres (127.0.0.1, the
// stored superuser password).
func (n Node) ConnectDB(ctx context.Context, database string) (*pgx.Conn, error) {
	info, err := n.ReadInfo()
	if err != nil {
		return nil, err
	}
	pw, err := n.Password()
	if err != nil {
		return nil, err
	}
	return dial(ctx, "127.0.0.1", info.Port, Superuser, pw, database)
}
