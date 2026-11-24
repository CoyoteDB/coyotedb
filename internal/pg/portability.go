package pg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The portability audit (docs/MIXED_OS.md conditions 2-5): catalog checks
// that keep a database free of things that would make two different
// machines read the same files differently. It is read-only, touches
// only the system catalogs, takes no locks beyond what any catalog read
// takes, and costs a few milliseconds per database, so it is meant to run
// on a schedule (see AuditInterval) on every node, primary and standby
// alike. No event trigger is needed: a forbidden object created between
// two runs is found by the next run, and the join check runs it too.
//
// What it finds:
//
//	(a) text sort rules from the operating system or ICU (libc and ICU
//	    collations) anywhere they are used;
//	(b) maths-library functions inside stored structures (index
//	    expressions and conditions, partition keys, generated columns;
//	    check constraints as a warning);
//	(c) tablespaces other than the two built-in ones;
//	(d) extensions not on ExtensionAllowList;
//	(e) settings that must be off, or must be this machine's own.
//
// Why expressions are read from the stored expression trees and not from
// pg_depend: Postgres records no dependency on "pinned" objects, and every
// built-in function (exp, ln, ...) and the built-in "unicode" ICU
// collation are pinned. pg_depend therefore cannot see `CREATE INDEX ON
// t (exp(x))`. The stored tree (pg_index.indexprs and friends, the
// pg_node_tree text) names every function, operator and collation by OID
// in fixed fields (:funcid, :opno, :inputcollid, ...); nodeTreeRefs reads
// those fields. It does not look at SQL text.

// AuditInterval is the suggested time between scheduled audits.
const AuditInterval = 10 * time.Minute

// ExtensionAllowList is the set of extensions allowed in a cluster whose
// machines may differ, each with the reason. plpgsql is part of every
// database. Anything not listed is blocked, including extensions that are
// fine but not yet reviewed.
var ExtensionAllowList = map[string]string{
	"plpgsql":            "part of Postgres; a language, it stores nothing itself",
	"amcheck":            "read-only checker; stores nothing",
	"pg_stat_statements": "statistics in memory and a stats file that is not replicated; no stored structures",
	"pgcrypto":           "hashes and ciphers are defined by standards; random values are just data once stored",
	"uuid-ossp":          "v3/v5 are standard hashes and v1/v4 are random data once stored; the generating library differs by build (the fingerprint's extension list makes sure every machine has it)",
	"citext":             "compares with lower() under the database's collation, which is the built-in C.UTF-8 (the audit blocks libc/ICU collations on citext columns like any other)",
	"pg_trgm":            "trigrams use the built-in locale's character classes and case rules (same Unicode tables everywhere, checked by the fingerprint and probe)",
	"btree_gin":          "B-tree style operators for core types inside GIN; text uses the column's collation, which the audit checks",
	"btree_gist":         "B-tree style operators for core types inside GiST; text uses the column's collation, which the audit checks",
	"hstore":             "keys compared by length then bytes; no collation, no maths library",
	"ltree":              "labels use the built-in locale's character classes; no maths library",
	"unaccent":           "rules come from share/tsearch_data, which the fingerprint hashes",
	"tablefunc":          "crosstab and friends compute at query time; normal_rand uses the maths library but returns random data, never usable in an index",
	"intarray":           "integer arrays only",
	"pageinspect":        "read-only inspection",
	"pg_visibility":      "read-only inspection",
	"pg_buffercache":     "read-only inspection of memory",
	"pgstattuple":        "read-only inspection",
}

// Extensions deliberately left off the allow-list, with the reason (for
// messages; anything unknown is blocked as "not reviewed").
var extensionDenyReasons = map[string]string{
	"earthdistance": "computes positions with sin/cos from the system maths library, and is normally used in indexes",
	"cube":          "earthdistance builds on it; float maths in GiST indexes not reviewed",
	"plpython3u":    "runs Python from this machine, whose version and libraries differ per system",
	"xml2":          "depends on the system's libxml2/libxslt",
	"file_fdw":      "reads files of one machine",
	"postgres_fdw":  "connects to other servers; not about stored data, but not reviewed",
	"dblink":        "connects to other servers; not about stored data, but not reviewed",
	"bloom":         "index type amcheck cannot verify; not reviewed",
	"fuzzystrmatch": "uses the C library's toupper(); not reviewed",
	"vector":        "pgvector sums its distances in an order that depends on the machine (measured: most results differ in the last digits between kinds of machines, D53), and its index types and functions like l2_normalize store such results",
}

// nodeLocalSettings must be this machine's own: they are rendered into
// coyote.conf per OS (or left at the default) and must never come from
// postgresql.conf or postgresql.auto.conf, which travel with base backups
// and rewinds from other machines.
var nodeLocalSettings = []string{
	"dynamic_shared_memory_type", "wal_sync_method", "shared_memory_type", "huge_pages",
	"huge_page_size", "effective_io_concurrency", "maintenance_io_concurrency", "io_method",
	"io_workers", "debug_io_direct",
}

// libmFunctions call the system maths library for float arguments, whose
// last digit may differ between systems (Step 0). Matched by name among
// pg_catalog functions with a float4/float8 argument.
var libmFunctions = []string{
	"exp", "dexp", "ln", "dlog1", "log", "log10", "dlog10", "power", "pow", "dpow",
	"sin", "cos", "tan", "cot", "asin", "acos", "atan", "atan2",
	"sind", "cosd", "tand", "cotd", "asind", "acosd", "atand", "atan2d",
	"sinh", "cosh", "tanh", "asinh", "acosh", "atanh",
	"cbrt", "dcbrt", "degrees", "radians", "gamma", "lgamma", "erf", "erfc",
}

// AuditOptions controls the audit.
type AuditOptions struct {
	// NodeOS is the operating system of the node whose server is checked
	// (default runtime.GOOS: the audit runs on the node itself).
	NodeOS string
	// StatementTimeout per query (default 30 s); LockTimeout (default 2 s).
	StatementTimeout, LockTimeout time.Duration
}

func (o *AuditOptions) defaults() {
	if o.NodeOS == "" {
		o.NodeOS = runtime.GOOS
	}
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = 30 * time.Second
	}
	if o.LockTimeout <= 0 {
		o.LockTimeout = 2 * time.Second
	}
}

// AuditReport is the result of AuditCluster.
type AuditReport struct {
	At        time.Time `json:"at"`
	Databases []string  `json:"databases"`
	Findings  []Finding `json:"findings"`
}

// Blocking reports whether any finding blocks mixing machines.
func (r AuditReport) Blocking() bool { return Blocking(r.Findings) }

// Digest identifies the set of findings (not the time): a scheduler can
// report only when it changes.
func (r AuditReport) Digest() string {
	h := sha256.New()
	for _, f := range r.Findings {
		fmt.Fprintf(h, "%s|%s|%s|%s|%s\n", f.Severity, f.Check, f.Database, f.Object, f.Problem)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AuditCluster audits every database that accepts connections, one at a
// time, plus the cluster-wide checks (tablespaces, settings). connect
// opens a connection to a database; AuditCluster closes it.
func AuditCluster(ctx context.Context, connect ConnectFunc, o AuditOptions) (AuditReport, error) {
	o.defaults()
	r := AuditReport{At: time.Now().UTC()}
	c, err := connect(ctx, "postgres")
	if err != nil {
		return r, err
	}
	dbs, err := ListDatabases(ctx, c)
	if err == nil {
		var fs []Finding
		fs, err = auditClusterWide(ctx, c, o)
		r.Findings = append(r.Findings, fs...)
	}
	c.Close(context.WithoutCancel(ctx))
	if err != nil {
		return r, err
	}
	r.Databases = dbs
	for _, db := range dbs {
		c, err := connect(ctx, db)
		if err != nil {
			return r, fmt.Errorf("audit %s: %w", db, err)
		}
		fs, err := AuditDatabase(ctx, c, o)
		c.Close(context.WithoutCancel(ctx))
		if err != nil {
			return r, fmt.Errorf("audit %s: %w", db, err)
		}
		r.Findings = append(r.Findings, fs...)
	}
	r.Findings = append(r.Findings, Finding{Severity: SeverityInfo, Check: "template0",
		Problem: "The template0 database cannot be checked (it accepts no connections); it keeps the system's imported sort rules. New databases are made from template1, which is checked."})
	SortFindings(r.Findings)
	return r, nil
}

// readOnly runs f in a read-only transaction with the audit's timeouts.
func readOnly(ctx context.Context, c *pgx.Conn, stmt, lock time.Duration, f func() error) error {
	if _, err := c.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = c.Exec(rctx, "ROLLBACK")
	}()
	for _, s := range []string{
		fmt.Sprintf("SET LOCAL statement_timeout = %d", stmt.Milliseconds()),
		fmt.Sprintf("SET LOCAL lock_timeout = %d", lock.Milliseconds()),
		"SET LOCAL search_path = pg_catalog",
	} {
		if _, err := c.Exec(ctx, s); err != nil {
			return err
		}
	}
	return f()
}

func auditClusterWide(ctx context.Context, c *pgx.Conn, o AuditOptions) ([]Finding, error) {
	var fs []Finding
	err := readOnly(ctx, c, o.StatementTimeout, o.LockTimeout, func() error {
		ts, err := stringList(ctx, c, `SELECT spcname FROM pg_tablespace WHERE spcname NOT IN ('pg_default', 'pg_global') ORDER BY 1`)
		if err != nil {
			return fmt.Errorf("tablespaces: %w", err)
		}
		for _, t := range ts {
			fs = append(fs, Finding{Severity: SeverityBlock, Check: "tablespace", Object: "tablespace " + t,
				Problem: "This tablespace keeps data in a folder of one machine; other machines have no such folder.",
				Fix:     fmt.Sprintf("Move its tables back (ALTER TABLE ... SET TABLESPACE pg_default), then DROP TABLESPACE %s.", quoteIdent(t))})
		}
		sfs, err := auditSettings(ctx, c, o.NodeOS)
		fs = append(fs, sfs...)
		return err
	})
	return fs, err
}

func auditSettings(ctx context.Context, c *pgx.Conn, nodeOS string) ([]Finding, error) {
	type setting struct{ name, value, source, file string }
	rows, err := c.Query(ctx, `SELECT name, setting, source, coalesce(sourcefile, '') FROM pg_settings WHERE name = ANY($1) ORDER BY name`,
		append([]string{"jit", "max_prepared_transactions", "data_checksums", "shared_preload_libraries"}, nodeLocalSettings...))
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	set, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (setting, error) {
		var s setting
		return s, r.Scan(&s.name, &s.value, &s.source, &s.file)
	})
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	var fs []Finding
	block := func(name, problem, fix, detail string) {
		fs = append(fs, Finding{Severity: SeverityBlock, Check: "setting", Object: "setting " + name, Problem: problem, Fix: fix, Detail: detail})
	}
	dsm, walSync := platformSettings(nodeOS)
	for _, s := range set {
		detail := fmt.Sprintf("value %q from %s %s", s.value, s.source, s.file)
		switch s.name {
		case "jit":
			if s.value != "off" {
				block(s.name, "Just-in-time compilation is on; it makes machines compute differently.", "Remove any jit setting from postgresql.conf and postgresql.auto.conf (ALTER SYSTEM RESET jit); the program sets jit = off.", detail)
			}
		case "max_prepared_transactions":
			if s.value != "0" {
				block(s.name, "Prepared (two-phase) transactions are allowed; a half-finished one would be carried to another machine.", "ALTER SYSTEM RESET max_prepared_transactions, then restart.", detail)
			}
		case "data_checksums":
			if s.value != "on" {
				block(s.name, "Page checksums are off, so damaged pages cannot be detected.", "This data folder must be re-created; contact support.", detail)
			}
		case "shared_preload_libraries":
			for _, lib := range strings.Split(s.value, ",") {
				lib = strings.Trim(strings.TrimSpace(lib), `"`)
				if lib != "" && lib != "pg_stat_statements" {
					fs = append(fs, Finding{Severity: SeverityWarn, Check: "setting", Object: "setting shared_preload_libraries",
						Problem: fmt.Sprintf("The library %q is loaded at start-up; it has not been reviewed for mixed machines.", lib),
						Fix:     "Remove it unless you know it behaves the same on every system.", Detail: detail})
				}
			}
		default: // node-local
			if s.source == "configuration file" && !strings.HasSuffix(s.file, AgentConfFile) {
				block(s.name, "This machine-specific setting comes from a file that is copied between machines, so another machine could start with a value that is wrong for it.",
					fmt.Sprintf("Remove %s from %s (for postgresql.auto.conf: ALTER SYSTEM RESET %s).", s.name, baseName(s.file), s.name), detail)
			}
			if s.name == "dynamic_shared_memory_type" && s.value != dsm {
				block(s.name, fmt.Sprintf("Shared memory is set to %q, but this machine needs %q.", s.value, dsm), "Restart the node; the program writes the right value.", detail)
			}
			if s.name == "wal_sync_method" && s.value != walSync {
				block(s.name, fmt.Sprintf("Disk flushing is set to %q, but this machine needs %q to make sure saved changes survive a power cut.", s.value, walSync), "Restart the node; the program writes the right value.", detail)
			}
		}
	}
	return fs, nil
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// collation is one pg_collation row.
type collation struct {
	oid                uint32
	name, provider     string
	collate, ctype     string
	catalog, imported  bool
	portable           bool
	providerPlainWords string
}

// unportableCollation is the SQL test for "sort rules from the OS or ICU".
const unportableCollation = `(c.collprovider = 'i' OR (c.collprovider = 'c' AND NOT (coalesce(c.collcollate, '') IN ('C', 'POSIX') AND coalesce(c.collctype, '') IN ('C', 'POSIX'))))`

// usableCollation: collations for another encoding cannot be named or
// used in this database (Postgres ignores them), so they are left alone.
const usableCollation = `c.collencoding IN (-1, pg_char_to_encoding(current_setting('server_encoding')))`

// AuditDatabase runs the per-database checks on c's database.
func AuditDatabase(ctx context.Context, c *pgx.Conn, o AuditOptions) ([]Finding, error) {
	o.defaults()
	db := c.Config().Database
	var fs []Finding
	err := readOnly(ctx, c, o.StatementTimeout, o.LockTimeout, func() error {
		a := &auditor{ctx: ctx, c: c, db: db}
		for _, step := range []func() error{a.loadCollations, a.loadFunctions, a.database, a.columns, a.indexes, a.types, a.expressions, a.extensions, a.collationObjects} {
			if err := step(); err != nil {
				return err
			}
		}
		fs = a.out
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := range fs {
		fs[i].Database = db
	}
	SortFindings(fs)
	return dedupe(fs), nil
}

func dedupe(fs []Finding) []Finding {
	var out []Finding
	seen := map[string]bool{}
	for _, f := range fs {
		k := string(f.Severity) + "|" + f.Check + "|" + f.Object + "|" + f.Problem
		if !seen[k] {
			seen[k] = true
			out = append(out, f)
		}
	}
	return out
}

type auditor struct {
	ctx   context.Context
	c     *pgx.Conn
	db    string
	colls map[uint32]collation
	// funcs maps OIDs of functions (and of operators, via their function)
	// that call the maths library to a readable name; printf is the same
	// for functions that print floats with the C library.
	libm, printf map[uint32]string
	libmOps      map[uint32]string
	out          []Finding
}

func (a *auditor) add(f Finding) { a.out = append(a.out, f) }

func (a *auditor) loadCollations() error {
	a.colls = map[uint32]collation{}
	rows, err := a.c.Query(a.ctx, `SELECT c.oid, c.collname, c.collprovider::text, coalesce(c.collcollate, ''), coalesce(c.collctype, ''),
		c.collnamespace = 'pg_catalog'::regnamespace, c.oid >= 12000 AND c.collnamespace = 'pg_catalog'::regnamespace, NOT `+unportableCollation+`
		FROM pg_collation c WHERE `+usableCollation)
	if err != nil {
		return fmt.Errorf("collations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k collation
		if err := rows.Scan(&k.oid, &k.name, &k.provider, &k.collate, &k.ctype, &k.catalog, &k.imported, &k.portable); err != nil {
			return err
		}
		switch k.provider {
		case "i":
			k.providerPlainWords = "ICU"
		case "c":
			k.providerPlainWords = "this operating system's"
		case "b":
			k.providerPlainWords = "built-in"
		default:
			k.providerPlainWords = "the database's default"
		}
		a.colls[k.oid] = k
	}
	return rows.Err()
}

func (a *auditor) badColl(oid uint32) (collation, bool) {
	k, ok := a.colls[oid]
	return k, ok && !k.portable
}

// collFix says how to move what to the built-in rules; how is an example.
func collFix(what, how string) string {
	s := fmt.Sprintf("Switch the %s to the built-in sort rules (COLLATE pg_c_utf8, or \"C\")", what)
	if how != "" {
		s += ", for example " + how
	}
	return s + "."
}

func (a *auditor) loadFunctions() error {
	a.libm, a.printf, a.libmOps = map[uint32]string{}, map[uint32]string{}, map[uint32]string{}
	rows, err := a.c.Query(a.ctx, `SELECT p.oid, p.proname || '(' || pg_get_function_identity_arguments(p.oid) || ')',
		p.proname = ANY($1) AND p.proargtypes::oid[] && ARRAY['float8'::regtype, 'float4'::regtype]::oid[]
		  OR (p.proname = 'polygon' AND 'circle'::regtype = ANY(p.proargtypes::oid[])),
		(p.proname IN ('numeric', 'to_char') AND p.proargtypes[0] IN ('float8'::regtype, 'float4'::regtype))
		FROM pg_proc p WHERE p.pronamespace = 'pg_catalog'::regnamespace`, libmFunctions)
	if err != nil {
		return fmt.Errorf("functions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var name string
		var libm, printf bool
		if err := rows.Scan(&oid, &name, &libm, &printf); err != nil {
			return err
		}
		if libm {
			a.libm[oid] = name
		}
		if printf {
			a.printf[oid] = name
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = a.c.Query(a.ctx, `SELECT o.oid, o.oprcode::oid FROM pg_operator o WHERE o.oprcode <> 0`)
	if err != nil {
		return fmt.Errorf("operators: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var op, fn uint32
		if err := rows.Scan(&op, &fn); err != nil {
			return err
		}
		if n, ok := a.libm[fn]; ok {
			a.libmOps[op] = n
		}
	}
	return rows.Err()
}

// database checks the database's own default sort rules.
func (a *auditor) database() error {
	var prov, collate, ctype, locale string
	err := a.c.QueryRow(a.ctx, `SELECT datlocprovider::text, datcollate, datctype, coalesce(datlocale, '') FROM pg_database WHERE datname = current_database()`).
		Scan(&prov, &collate, &ctype, &locale)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	cOK := func(s string) bool { return s == "C" || s == "POSIX" }
	obj := "database " + a.db
	switch {
	case prov == "i":
		a.add(Finding{Severity: SeverityBlock, Check: "collation", Object: obj, Problem: fmt.Sprintf("The database sorts text with ICU rules (%s), which change between ICU versions.", locale),
			Fix: "Re-create the database with the built-in C.UTF-8 rules (CREATE DATABASE ... TEMPLATE template0 LOCALE_PROVIDER builtin BUILTIN_LOCALE 'C.UTF-8') and copy the data over."})
	case prov == "c" && !(cOK(collate) && cOK(ctype)):
		a.add(Finding{Severity: SeverityBlock, Check: "collation", Object: obj, Problem: fmt.Sprintf("The database sorts text with this operating system's rules (%s), which differ between systems.", collate),
			Fix: "Re-create the database with the built-in C.UTF-8 rules (CREATE DATABASE ... TEMPLATE template0 LOCALE_PROVIDER builtin BUILTIN_LOCALE 'C.UTF-8') and copy the data over."})
	case !(cOK(collate) && cOK(ctype)):
		a.add(Finding{Severity: SeverityBlock, Check: "collation", Object: obj, Problem: fmt.Sprintf("The database's character settings come from this operating system (LC_COLLATE %s, LC_CTYPE %s).", collate, ctype),
			Fix: "Re-create the database with LOCALE 'C' and the built-in C.UTF-8 rules and copy the data over."})
	}
	return nil
}

// columns checks column collations of tables, materialised views and
// composite types.
func (a *auditor) columns() error {
	rows, err := a.c.Query(a.ctx, `SELECT c.relkind::text, quote_ident(n.nspname) || '.' || quote_ident(c.relname) || '.' || quote_ident(at.attname), at.attcollation
		FROM pg_attribute at JOIN pg_class c ON c.oid = at.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE at.attnum > 0 AND NOT at.attisdropped AND at.attcollation <> 0 AND c.relkind IN ('r', 'p', 'm', 'c', 'f')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%'`)
	if err != nil {
		return fmt.Errorf("columns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, name string
		var coll uint32
		if err := rows.Scan(&kind, &name, &coll); err != nil {
			return err
		}
		if k, bad := a.badColl(coll); bad {
			what := "column"
			if kind == "c" {
				what = "type field"
			}
			a.add(Finding{Severity: SeverityBlock, Check: "collation", Object: what + " " + name,
				Problem: fmt.Sprintf("Text in this %s is sorted with %s rules (collation %q), which differ between systems.", what, k.providerPlainWords, k.name),
				Fix:     collFix(what, "ALTER TABLE ... ALTER COLUMN ... TYPE text COLLATE pg_c_utf8")})
		}
	}
	return rows.Err()
}

// indexes checks index column collations.
func (a *auditor) indexes() error {
	rows, err := a.c.Query(a.ctx, `SELECT i.indexrelid::regclass::text, i.indcollation::oid[]
		FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%'`)
	if err != nil {
		return fmt.Errorf("indexes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var colls []uint32
		if err := rows.Scan(&name, &colls); err != nil {
			return err
		}
		for _, coll := range colls {
			if k, bad := a.badColl(coll); bad {
				a.add(Finding{Severity: SeverityBlock, Check: "collation", Object: "index " + name,
					Problem: fmt.Sprintf("This index orders text with %s rules (collation %q); another machine would look for rows in the wrong place.", k.providerPlainWords, k.name),
					Fix:     collFix("index", "DROP INDEX, then CREATE INDEX ... (column COLLATE pg_c_utf8)")})
			}
		}
	}
	return rows.Err()
}

// types checks domains and range types.
func (a *auditor) types() error {
	rows, err := a.c.Query(a.ctx, `SELECT format_type(t.oid, NULL), t.typcollation, 'domain' FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE t.typtype = 'd' AND t.typcollation <> 0 AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		UNION ALL
		SELECT format_type(r.rngtypid, NULL), r.rngcollation, 'range type' FROM pg_range r JOIN pg_type t ON t.oid = r.rngtypid JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE r.rngcollation <> 0 AND n.nspname NOT IN ('pg_catalog', 'information_schema')`)
	if err != nil {
		return fmt.Errorf("types: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, kind string
		var coll uint32
		if err := rows.Scan(&name, &coll, &kind); err != nil {
			return err
		}
		if k, bad := a.badColl(coll); bad {
			a.add(Finding{Severity: SeverityBlock, Check: "collation", Object: kind + " " + name,
				Problem: fmt.Sprintf("This %s sorts text with %s rules (collation %q).", kind, k.providerPlainWords, k.name),
				Fix:     collFix(kind, "")})
		}
	}
	return rows.Err()
}

// storedExpr is one stored expression tree and where it lives.
type storedExpr struct {
	kind   string // "index expression", "index condition", "partition key", "generated column", "check constraint"
	object string
	tree   string
	colls  []uint32 // collations stored next to the tree (partition key)
}

func (a *auditor) expressions() error {
	rows, err := a.c.Query(a.ctx, `
		SELECT 'index expression', 'index ' || i.indexrelid::regclass::text, i.indexprs::text, NULL::oid[]
		  FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE i.indexprs IS NOT NULL AND c.relnamespace <> 'pg_catalog'::regnamespace
		UNION ALL
		SELECT 'index condition', 'index ' || i.indexrelid::regclass::text, i.indpred::text, NULL
		  FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE i.indpred IS NOT NULL AND c.relnamespace <> 'pg_catalog'::regnamespace
		UNION ALL
		SELECT 'partition key', 'table ' || p.partrelid::regclass::text, coalesce(p.partexprs::text, ''), p.partcollation::oid[]
		  FROM pg_partitioned_table p
		UNION ALL
		SELECT CASE at.attgenerated WHEN 'v' THEN 'virtual generated column' ELSE 'generated column' END,
		       'column ' || d.adrelid::regclass::text || '.' || quote_ident(at.attname), d.adbin::text, NULL
		  FROM pg_attrdef d JOIN pg_attribute at ON at.attrelid = d.adrelid AND at.attnum = d.adnum
		  WHERE at.attgenerated IN ('s', 'v')
		UNION ALL
		SELECT 'check constraint', CASE WHEN co.conrelid <> 0 THEN 'constraint ' || quote_ident(co.conname) || ' on ' || co.conrelid::regclass::text
		                                ELSE 'constraint ' || quote_ident(co.conname) || ' on domain ' || format_type(co.contypid, NULL) END,
		       co.conbin::text, NULL
		  FROM pg_constraint co WHERE co.contype = 'c' AND co.conbin IS NOT NULL AND co.connamespace <> 'pg_catalog'::regnamespace`)
	if err != nil {
		return fmt.Errorf("expressions: %w", err)
	}
	exprs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (storedExpr, error) {
		var e storedExpr
		return e, r.Scan(&e.kind, &e.object, &e.tree, &e.colls)
	})
	if err != nil {
		return fmt.Errorf("expressions: %w", err)
	}
	bodies := map[uint32]*funcBody{}
	for _, e := range exprs {
		for _, coll := range e.colls {
			if k, bad := a.badColl(coll); bad {
				a.add(Finding{Severity: SeverityBlock, Check: "collation", Object: e.object,
					Problem: fmt.Sprintf("The %s sorts text with %s rules (collation %q).", e.kind, k.providerPlainWords, k.name), Fix: collFix(e.kind, "")})
			}
		}
		refs, err := nodeTreeRefs(e.tree)
		if err != nil {
			return fmt.Errorf("%s of %s: %w", e.kind, e.object, err)
		}
		if err := a.checkRefs(e, refs, bodies, nil); err != nil {
			return err
		}
	}
	return nil
}

// funcBody is a user function looked at while following calls.
type funcBody struct {
	name      string
	lang      string
	sqlBody   string // pg_node_tree of a BEGIN ATOMIC body
	extension string // owning extension, if any
}

func (a *auditor) function(oid uint32, cache map[uint32]*funcBody) (*funcBody, error) {
	if b, ok := cache[oid]; ok {
		return b, nil
	}
	b := &funcBody{}
	err := a.c.QueryRow(a.ctx, `SELECT p.oid::regprocedure::text, l.lanname, coalesce(p.prosqlbody::text, ''),
		coalesce((SELECT e.extname FROM pg_depend d JOIN pg_extension e ON e.oid = d.refobjid
		          WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e' LIMIT 1), '')
		FROM pg_proc p JOIN pg_language l ON l.oid = p.prolang WHERE p.oid = $1`, oid).Scan(&b.name, &b.lang, &b.sqlBody, &b.extension)
	if errors.Is(err, pgx.ErrNoRows) {
		b = nil
	} else if err != nil {
		return nil, fmt.Errorf("function %d: %w", oid, err)
	}
	cache[oid] = b
	return b, nil
}

// checkRefs reports maths-library calls and unportable collations in one
// expression, following calls into user functions written as SQL bodies.
// via is the chain of user functions that led here.
func (a *auditor) checkRefs(e storedExpr, r treeRefs, cache map[uint32]*funcBody, via []string) error {
	sev := SeverityBlock
	if e.kind == "check constraint" {
		// A check is re-evaluated on writes: a last-digit difference can
		// make a write fail on one machine and pass on another, but it
		// cannot misplace stored data.
		sev = SeverityWarn
	}
	where := e.kind
	if len(via) > 0 {
		where += " (through " + strings.Join(via, " → ") + ")"
	}
	for _, coll := range r.colls {
		if k, bad := a.badColl(coll); bad {
			a.add(Finding{Severity: SeverityBlock, Check: "collation", Object: e.object,
				Problem: fmt.Sprintf("The %s compares text with %s rules (collation %q).", where, k.providerPlainWords, k.name), Fix: collFix(e.kind, "")})
		}
	}
	reported := map[string]bool{}
	report := func(name string) {
		if reported[name] {
			return
		}
		reported[name] = true
		a.add(Finding{Severity: sev, Check: "libm_expr", Object: e.object,
			Problem: fmt.Sprintf("The %s uses %s, which comes from the system maths library and can give a slightly different answer on another kind of machine.", where, name),
			Fix:     "Compute it with numeric instead (for example exp(x::numeric)), or store the value in a normal column your application writes; then re-create the " + objectNoun(e.object) + "."})
	}
	// An operator's function is normally in the tree too (opfuncid); the
	// operator is matched as well in case it is not.
	defer func() {
		for _, op := range r.ops {
			if n, ok := a.libmOps[op]; ok {
				report(n)
			}
		}
	}()
	for _, fn := range r.funcs {
		if n, ok := a.libm[fn]; ok {
			report(n)
			continue
		}
		if n, ok := a.printf[fn]; ok {
			a.add(Finding{Severity: SeverityWarn, Check: "float_text_expr", Object: e.object,
				Problem: fmt.Sprintf("The %s uses %s, which formats a decimal number with the system C library.", where, n),
				Fix:     "Prefer numeric values here; the behaviour check compares this function between machines, so a difference would be found."})
			continue
		}
		if fn < 16384 {
			continue // built-in and not on either list
		}
		b, err := a.function(fn, cache)
		if err != nil {
			return err
		}
		if b == nil || slices.Contains(via, b.name) {
			continue
		}
		if b.extension != "" {
			if _, ok := ExtensionAllowList[b.extension]; ok {
				continue
			}
			continue // the extension itself is reported by extensions()
		}
		if b.sqlBody != "" {
			sub, err := nodeTreeRefs(b.sqlBody)
			if err != nil {
				return fmt.Errorf("body of %s: %w", b.name, err)
			}
			if err := a.checkRefs(e, sub, cache, append(slices.Clone(via), b.name)); err != nil {
				return err
			}
			continue
		}
		a.add(Finding{Severity: SeverityWarn, Check: "opaque_function", Object: e.object,
			Problem: fmt.Sprintf("The %s calls your function %s (language %s); its body cannot be checked automatically.", where, b.name, b.lang),
			Fix:     "Make sure it does not use exp, ln, log, power, trigonometry, cbrt or similar float functions, or rewrite it as a SQL function with a BEGIN ATOMIC body so it can be checked."})
	}
	return nil
}

// objectNoun is the first word of an Object ("index", "column", ...).
func objectNoun(object string) string {
	n, _, _ := strings.Cut(object, " ")
	return n
}

func (a *auditor) extensions() error {
	rows, err := a.c.Query(a.ctx, `SELECT extname, extversion FROM pg_extension ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("extensions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, ver string
		if err := rows.Scan(&name, &ver); err != nil {
			return err
		}
		if _, ok := ExtensionAllowList[name]; ok {
			continue
		}
		why, ok := extensionDenyReasons[name]
		if !ok {
			why = "it has not been reviewed for machines of different kinds"
		}
		a.add(Finding{Severity: SeverityBlock, Check: "extension", Object: "extension " + name,
			Problem: fmt.Sprintf("The extension %s %s is not allowed when machines differ: %s.", name, ver, why),
			Fix:     fmt.Sprintf("Remove it (DROP EXTENSION %s) if you do not need it.", quoteIdent(name))})
	}
	return rows.Err()
}

// collationObjects reports unportable collations that exist without being
// used (user-made ones as a warning; the imported system ones as info).
func (a *auditor) collationObjects() error {
	imported := 0
	keys := make([]uint32, 0, len(a.colls))
	for oid := range a.colls {
		keys = append(keys, oid)
	}
	slices.Sort(keys)
	for _, oid := range keys {
		k := a.colls[oid]
		if k.portable {
			continue
		}
		switch {
		case k.imported:
			imported++
		case k.catalog:
			// The built-in "unicode" ICU collation is part of Postgres and
			// cannot be removed; any use of it is reported elsewhere.
		default:
			a.add(Finding{Severity: SeverityWarn, Check: "collation", Object: "collation " + k.name,
				Problem: fmt.Sprintf("A collation with %s sort rules exists; using it anywhere would block mixing machines.", k.providerPlainWords),
				Fix:     fmt.Sprintf("DROP COLLATION %s if nothing needs it.", quoteIdent(k.name))})
		}
	}
	if imported > 0 {
		a.add(Finding{Severity: SeverityInfo, Check: "collation", Object: "system collations",
			Problem: fmt.Sprintf("%d sort rules imported from this machine's operating system and ICU are still installed (unused).", imported),
			Fix:     "They are removed when the cluster is created; nothing to do."})
	}
	return nil
}

// treeRefs are the OIDs a stored expression tree names.
type treeRefs struct {
	funcs, ops, colls []uint32
}

// nodeTreeRefs reads the function, operator and collation OIDs out of a
// pg_node_tree text (the nodeToString form Postgres stores expressions
// in). Fields are matched by name: any field whose name ends in "funcid"
// or "fnoid" holds a function (funcid, opfuncid, hashfuncid, negfuncid,
// aggfnoid, winfnoid); "opno"/"opnos" an operator; any name containing
// "coll" a collation (varcollid, inputcollid, collOid, inputcollids...).
// A value is an integer or a list "(o 1 2 ...)". Quoted strings and
// backslash escapes are skipped as data, so text inside a constant can
// never be read as a field.
func nodeTreeRefs(tree string) (treeRefs, error) {
	var r treeRefs
	toks, err := tokenizeNodeTree(tree)
	if err != nil {
		return r, err
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if len(t) < 2 || t[0] != ':' {
			continue
		}
		field := strings.ToLower(t[1:])
		var dst *[]uint32
		switch {
		case strings.HasSuffix(field, "funcid") || strings.HasSuffix(field, "fnoid"):
			dst = &r.funcs
		case field == "opno" || field == "opnos":
			dst = &r.ops
		case strings.Contains(field, "coll"):
			dst = &r.colls
		default:
			continue
		}
		vals, n := nodeTreeValue(toks[i+1:])
		i += n
		for _, v := range vals {
			if v != 0 && !slices.Contains(*dst, v) {
				*dst = append(*dst, v)
			}
		}
	}
	return r, nil
}

// nodeTreeValue reads the value after a field token: an integer, or a
// parenthesised list of integers (optionally tagged "o" or "i"). It
// returns the OIDs and how many tokens it consumed.
func nodeTreeValue(toks []string) ([]uint32, int) {
	if len(toks) == 0 {
		return nil, 0
	}
	if v, err := strconv.ParseUint(toks[0], 10, 32); err == nil {
		return []uint32{uint32(v)}, 1
	}
	if toks[0] != "(" {
		return nil, 0
	}
	var out []uint32
	for j := 1; j < len(toks); j++ {
		switch t := toks[j]; t {
		case ")":
			return out, j + 1
		case "o", "i", "b", "x":
		default:
			v, err := strconv.ParseUint(t, 10, 32)
			if err != nil {
				return out, j // not a list of numbers; let the caller go on
			}
			out = append(out, uint32(v))
		}
	}
	return out, len(toks)
}

// tokenizeNodeTree splits node-tree text into tokens: "(", ")", "{", "}",
// and words. A backslash makes the next character part of the word, and
// marks the word as string data (Postgres escapes only inside string
// values, never in field names or numbers); a double-quoted string is one
// token.
func tokenizeNodeTree(s string) ([]string, error) {
	var toks []string
	var cur strings.Builder
	escaped := false
	flush := func() {
		if cur.Len() > 0 {
			t := cur.String()
			if escaped {
				t = "\x00" + t // data: cannot look like a field or a number
			}
			toks = append(toks, t)
			cur.Reset()
		}
		escaped = false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				escaped = true
			}
		case ch == '"':
			flush()
			j := i + 1
			for ; j < len(s); j++ {
				if s[j] == '\\' {
					j++
					continue
				}
				if s[j] == '"' {
					break
				}
			}
			if j >= len(s) {
				return nil, errors.New("unterminated string in expression tree")
			}
			toks = append(toks, s[i:j+1])
			i = j
		case ch == '(' || ch == ')' || ch == '{' || ch == '}':
			flush()
			toks = append(toks, string(ch))
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r':
			flush()
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return toks, nil
}

// DropImportedCollations removes, from c's database, the collations
// initdb imported from the operating system and ICU (libc and ICU
// providers, in pg_catalog, not "C"/"POSIX"), so nothing can use them by
// accident. Collations for other encodings are not touched: a database
// cannot use them. A collation something depends on is kept and reported. The
// built-in "unicode" ICU collation is part of Postgres and cannot be
// removed; the audit reports any use of it. Run on the primary, in every
// database, right after the cluster is created (template1 first, so new
// databases inherit the clean list). It is idempotent.
func DropImportedCollations(ctx context.Context, c *pgx.Conn) (dropped int, kept []Finding, err error) {
	rows, err := c.Query(ctx, `SELECT c.oid, quote_ident(c.collname),
		EXISTS (SELECT 1 FROM pg_depend d WHERE d.refclassid = 'pg_collation'::regclass AND d.refobjid = c.oid)
		FROM pg_collation c WHERE c.collnamespace = 'pg_catalog'::regnamespace AND c.oid >= 12000 AND `+unportableCollation+` AND `+usableCollation+` ORDER BY c.oid`)
	if err != nil {
		return 0, nil, fmt.Errorf("list imported collations: %w", err)
	}
	type coll struct {
		oid  uint32
		name string
		used bool
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (coll, error) {
		var k coll
		return k, r.Scan(&k.oid, &k.name, &k.used)
	})
	if err != nil {
		return 0, nil, fmt.Errorf("list imported collations: %w", err)
	}
	db := c.Config().Database
	tx, err := c.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '10s'"); err != nil {
		return 0, nil, err
	}
	// Housekeeping, not a client write: it must not wait for a copy
	// (with the only copy down in strict mode it would wait forever). If
	// it is lost in a failover it is simply done again on the next start.
	if _, err := tx.Exec(ctx, "SET LOCAL synchronous_commit = local"); err != nil {
		return 0, nil, err
	}
	for _, k := range list {
		if k.used {
			kept = append(kept, Finding{Severity: SeverityWarn, Check: "collation", Database: db, Object: "collation " + k.name,
				Problem: "An imported operating-system or ICU collation is in use, so it was not removed.",
				Fix:     "Change what uses it to COLLATE pg_c_utf8; the audit lists the objects."})
			continue
		}
		if _, err := tx.Exec(ctx, "DROP COLLATION pg_catalog."+k.name); err != nil {
			return 0, kept, fmt.Errorf("drop collation %s: %w", k.name, err)
		}
		dropped++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, kept, err
	}
	return dropped, kept, nil
}

// DropImportedCollationsEverywhere runs DropImportedCollations in every
// database that accepts connections (template1 first).
func DropImportedCollationsEverywhere(ctx context.Context, connect ConnectFunc) (int, []Finding, error) {
	c, err := connect(ctx, "postgres")
	if err != nil {
		return 0, nil, err
	}
	dbs, err := ListDatabases(ctx, c)
	c.Close(context.WithoutCancel(ctx))
	if err != nil {
		return 0, nil, err
	}
	slices.SortStableFunc(dbs, func(a, b string) int {
		if a == "template1" {
			return -1
		}
		if b == "template1" {
			return 1
		}
		return 0
	})
	total := 0
	var kept []Finding
	for _, db := range dbs {
		c, err := connect(ctx, db)
		if err != nil {
			return total, kept, err
		}
		n, k, err := DropImportedCollations(ctx, c)
		c.Close(context.WithoutCancel(ctx))
		total += n
		kept = append(kept, k...)
		if err != nil {
			return total, kept, fmt.Errorf("%s: %w", db, err)
		}
	}
	return total, kept, nil
}
