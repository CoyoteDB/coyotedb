package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The integrity check (docs/MIXED_OS.md condition 7): proves that what a
// node has on disk is consistent, so a difference between machines that
// damaged an index cannot stay hidden. It runs on any node, primary or
// standby, while the database is in use.
//
// It calls the amcheck functions directly, one relation at a time, rather
// than running pg_amcheck: pg_amcheck itself is a client that calls these
// same functions, and calling them here gives per-relation timeouts and
// pauses, cancellation, structured results, and the choice of lock per
// check. RunPgAmcheck runs the real pg_amcheck as an independent second
// opinion (the proof campaign uses both).
//
// Locks taken, per check (all released when that one statement ends):
//
//	verify_heapam                AccessShareLock on the table: never blocks writes.
//	bt_index_check               AccessShareLock on table and index: never blocks
//	                             writes; works on a standby. heapallindexed and
//	                             checkunique keep the same lock (more CPU, a
//	                             memory-bounded Bloom filter).
//	bt_index_parent_check        ShareLock on the table: BLOCKS WRITES to that table
//	                             while it runs, and cannot run on a standby. Only
//	                             with VerifyOptions.AllowBlocking on a primary.
//	gin_index_check              AccessShareLock.
//	index-vs-table comparison    plain SELECTs (AccessShareLock).
//	partition placement          plain SELECT over each partition.
//
// Every statement runs with statement_timeout and lock_timeout, so a busy
// table is skipped (reported) instead of waited for.

// VerifyLevel chooses how much work the integrity check does.
type VerifyLevel int

const (
	// VerifyQuick: heap pages (no TOAST), B-tree structure, GIN
	// structure, 3 index-vs-table samples per other index. For a frequent
	// scheduled run.
	VerifyQuick VerifyLevel = iota
	// VerifyStandard adds TOAST, "every row is in every B-tree index"
	// (heapallindexed), unique-constraint checks, partition placement and
	// 10 samples per index.
	VerifyStandard
	// VerifyDeep adds the parent/child B-tree check and re-finding every
	// tuple from the root (only on a primary with AllowBlocking; otherwise
	// as Standard) and 50 samples per index.
	VerifyDeep
)

func (l VerifyLevel) String() string {
	switch l {
	case VerifyQuick:
		return "quick"
	case VerifyStandard:
		return "standard"
	case VerifyDeep:
		return "deep"
	}
	return "level " + strconv.Itoa(int(l))
}

// VerifyOptions controls the integrity check.
type VerifyOptions struct {
	Level VerifyLevel
	// AllowBlocking allows bt_index_parent_check at VerifyDeep, which
	// blocks writes to each table while its indexes are checked.
	AllowBlocking bool
	// InstallAmcheck creates the amcheck extension where it is missing
	// (primary only; on a standby a missing amcheck is reported).
	InstallAmcheck bool
	// StatementTimeout per check (default 10 minutes); LockTimeout
	// (default 2 s): a relation whose lock is not granted in time is
	// skipped and reported.
	StatementTimeout, LockTimeout time.Duration
	// Pause between relations, to stay out of the way of the workload.
	Pause time.Duration
	// Samples per index for the index-vs-table comparison (0: by level).
	Samples int
	// Databases to check (empty: every database that accepts
	// connections).
	Databases []string
	// Seed picks the sampled pages and rows (0: a new seed each run, so
	// repeated runs cover different parts of each table).
	Seed int64
}

func (o *VerifyOptions) defaults() {
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = 10 * time.Minute
	}
	if o.LockTimeout <= 0 {
		o.LockTimeout = 2 * time.Second
	}
	if o.Seed == 0 {
		o.Seed = time.Now().UnixNano() % 1000000007
	}
	if o.Samples <= 0 {
		o.Samples = map[VerifyLevel]int{VerifyQuick: 3, VerifyStandard: 10, VerifyDeep: 50}[o.Level]
		if o.Samples == 0 {
			o.Samples = 3
		}
	}
}

// VerifyReport is the result of an integrity check.
type VerifyReport struct {
	At        time.Time      `json:"at"`
	Level     string         `json:"level"`
	Databases []string       `json:"databases"`
	Checked   map[string]int `json:"checked"` // relations checked, by kind
	Findings  []Finding      `json:"findings"`
	Duration  time.Duration  `json:"duration"`
}

// Damaged reports whether a check found damage (any SeverityBlock
// finding).
func (r VerifyReport) Damaged() bool { return Blocking(r.Findings) }

// VerifyCluster checks every database (or o.Databases), one relation at a
// time.
func VerifyCluster(ctx context.Context, connect ConnectFunc, o VerifyOptions) (VerifyReport, error) {
	o.defaults()
	start := time.Now()
	r := VerifyReport{At: start.UTC(), Level: o.Level.String(), Checked: map[string]int{}}
	dbs := o.Databases
	if len(dbs) == 0 {
		c, err := connect(ctx, "postgres")
		if err != nil {
			return r, err
		}
		dbs, err = ListDatabases(ctx, c)
		if err == nil {
			var fs []Finding
			fs, err = checksumCounters(ctx, c)
			r.Findings = append(r.Findings, fs...)
		}
		c.Close(context.WithoutCancel(ctx))
		if err != nil {
			return r, err
		}
	}
	r.Databases = dbs
	for _, db := range dbs {
		c, err := connect(ctx, db)
		if err != nil {
			return r, fmt.Errorf("verify %s: %w", db, err)
		}
		v := &verifier{ctx: ctx, c: c, db: db, o: o, checked: r.Checked}
		err = v.run()
		c.Close(context.WithoutCancel(ctx))
		r.Findings = append(r.Findings, v.out...)
		if err != nil {
			return r, fmt.Errorf("verify %s: %w", db, err)
		}
	}
	SortFindings(r.Findings)
	r.Duration = time.Since(start)
	return r, nil
}

// checksumCounters reports pages that failed their checksum when read
// (any database), as counted by Postgres itself.
func checksumCounters(ctx context.Context, c *pgx.Conn) ([]Finding, error) {
	rows, err := c.Query(ctx, `SELECT coalesce(datname, '(shared)'), checksum_failures, coalesce(checksum_last_failure::text, '')
		FROM pg_stat_database WHERE checksum_failures > 0 ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("checksum counters: %w", err)
	}
	defer rows.Close()
	var fs []Finding
	for rows.Next() {
		var db, last string
		var n int64
		if err := rows.Scan(&db, &n, &last); err != nil {
			return nil, err
		}
		fs = append(fs, Finding{Severity: SeverityBlock, Check: "checksum", Database: db,
			Problem: fmt.Sprintf("%d data page(s) failed their checksum when read (last at %s): the disk returned damaged data.", n, last),
			Fix:     "Check the disk. This copy must be replaced from a healthy node (re-clone).", Detail: fmt.Sprintf("pg_stat_database.checksum_failures = %d", n)})
	}
	return fs, rows.Err()
}

type verifier struct {
	ctx        context.Context
	c          *pgx.Conn
	db         string
	o          VerifyOptions
	inRecovery bool
	hasGIN     bool
	amSchema   string // quoted schema of the amcheck extension
	checked    map[string]int
	out        []Finding
}

func (v *verifier) add(f Finding) {
	f.Database = v.db
	v.out = append(v.out, f)
}

// rel is one relation to check.
type rel struct {
	oid      uint32
	name     string // qualified, quoted
	kind     string // "table", "btree", "gin", "gist", ...
	unlogged bool
	unique   bool
	table    string // for indexes: the table (qualified, quoted)
	tableOID uint32
}

func (v *verifier) run() error {
	for _, s := range []string{
		fmt.Sprintf("SET statement_timeout = %d", v.o.StatementTimeout.Milliseconds()),
		fmt.Sprintf("SET lock_timeout = %d", v.o.LockTimeout.Milliseconds()),
		"SET search_path = pg_catalog",
		"SET application_name = '" + adminAppName + " verify'",
		"SET extra_float_digits = 3",
		"SET max_parallel_workers_per_gather = 0",
	} {
		if _, err := v.c.Exec(v.ctx, s); err != nil {
			return err
		}
	}
	if err := v.c.QueryRow(v.ctx, "SELECT pg_is_in_recovery()").Scan(&v.inRecovery); err != nil {
		return err
	}
	ok, err := v.amcheck()
	if err != nil || !ok {
		return err
	}
	rels, err := v.relations()
	if err != nil {
		return err
	}
	for i, r := range rels {
		if i > 0 && v.o.Pause > 0 {
			t := time.NewTimer(v.o.Pause)
			select {
			case <-v.ctx.Done():
				t.Stop()
				return v.ctx.Err()
			case <-t.C:
			}
		}
		if err := v.check(r); err != nil {
			return err
		}
	}
	if v.o.Level >= VerifyStandard {
		if err := v.partitions(); err != nil {
			return err
		}
	}
	return nil
}

// amcheck makes sure the extension is there (or says why not).
func (v *verifier) amcheck() (bool, error) {
	err := v.c.QueryRow(v.ctx, "SELECT quote_ident(extnamespace::regnamespace::text) FROM pg_extension WHERE extname = 'amcheck'").Scan(&v.amSchema)
	if errors.Is(err, pgx.ErrNoRows) {
		if !v.o.InstallAmcheck || v.inRecovery {
			v.add(Finding{Severity: SeverityWarn, Check: "amcheck", Object: "database " + v.db,
				Problem: "The integrity checker (amcheck) is not installed in this database, so it was not checked.",
				Fix:     "Run CREATE EXTENSION amcheck on the main node; it reaches every copy."})
			return false, nil
		}
		if _, err := v.c.Exec(v.ctx, "CREATE EXTENSION IF NOT EXISTS amcheck SCHEMA public"); err != nil {
			return false, fmt.Errorf("install amcheck: %w", err)
		}
		v.amSchema = "public"
	} else if err != nil {
		return false, err
	}
	err = v.c.QueryRow(v.ctx, `SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'gin_index_check' AND pronamespace = $1::regnamespace)`, v.amSchema).Scan(&v.hasGIN)
	return true, err
}

func (v *verifier) relations() ([]rel, error) {
	rows, err := v.c.Query(v.ctx, `SELECT * FROM (
		SELECT c.oid, c.oid::regclass::text AS name, 'table' AS kind, c.relpersistence = 'u', false, '', 0::oid
		  FROM pg_class c WHERE c.relkind IN ('r', 'm') AND c.relpersistence <> 't'
		UNION ALL
		SELECT c.oid, c.oid::regclass::text, a.amname, c.relpersistence = 'u', i.indisunique, i.indrelid::regclass::text, i.indrelid
		  FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_am a ON a.oid = c.relam
		  WHERE c.relkind = 'i' AND c.relpersistence <> 't' AND i.indisvalid AND i.indisready AND i.indislive
		) r ORDER BY kind <> 'table', name`)
	if err != nil {
		return nil, fmt.Errorf("list relations: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (rel, error) {
		var x rel
		return x, r.Scan(&x.oid, &x.name, &x.kind, &x.unlogged, &x.unique, &x.table, &x.tableOID)
	})
}

// check runs the right check for one relation.
func (v *verifier) check(r rel) error {
	if r.unlogged && v.inRecovery {
		return nil // a standby holds no data for unlogged relations
	}
	switch r.kind {
	case "table":
		return v.heap(r)
	case "btree":
		return v.btree(r)
	case "gin":
		if !v.hasGIN {
			v.add(Finding{Severity: SeverityInfo, Check: "gin", Object: "index " + r.name, Problem: "This amcheck version cannot check GIN indexes."})
			return nil
		}
		return v.stmt(r, "gin", "SELECT "+v.amSchema+".gin_index_check($1::regclass)", r.oid)
	case "gist", "spgist", "brin", "hash":
		return v.indexVsTable(r)
	}
	v.add(Finding{Severity: SeverityInfo, Check: "index", Object: "index " + r.name, Problem: fmt.Sprintf("Indexes of type %s are not checked.", r.kind)})
	return nil
}

func (v *verifier) heap(r rel) error {
	toast := v.o.Level >= VerifyStandard
	rows, err := v.c.Query(v.ctx, `SELECT blkno, offnum, attnum, msg FROM `+v.amSchema+`.verify_heapam($1::regclass, on_error_stop => false, check_toast => $2::bool, skip => 'none')`, r.oid, toast)
	if err != nil {
		return v.failed(r, "heap", err)
	}
	var probs []string
	for rows.Next() {
		var blk int64
		var off, att *int32
		var msg string
		if err := rows.Scan(&blk, &off, &att, &msg); err != nil {
			rows.Close()
			return err
		}
		loc := fmt.Sprintf("page %d", blk)
		if off != nil {
			loc += fmt.Sprintf(" item %d", *off)
		}
		if att != nil {
			loc += fmt.Sprintf(" column %d", *att)
		}
		probs = append(probs, loc+": "+msg)
	}
	if err := rows.Err(); err != nil {
		return v.failed(r, "heap", err)
	}
	v.checked["table"]++
	if len(probs) > 0 {
		detail := strings.Join(probs[:min(len(probs), 10)], "; ")
		if len(probs) > 10 {
			detail += fmt.Sprintf("; and %d more", len(probs)-10)
		}
		v.add(Finding{Severity: SeverityBlock, Check: "heap", Object: "table " + r.name,
			Problem: fmt.Sprintf("The table's stored rows are damaged (%d problem(s) found).", len(probs)),
			Fix:     "This copy must be replaced from a healthy node (re-clone); if this is the main node, switch to a healthy copy first.", Detail: detail})
	}
	return nil
}

func (v *verifier) btree(r rel) error {
	switch {
	case v.o.Level >= VerifyDeep && v.o.AllowBlocking && !v.inRecovery:
		return v.stmt(r, "btree", "SELECT "+v.amSchema+".bt_index_parent_check($1::regclass, heapallindexed => true, rootdescend => true, checkunique => $2::bool)", r.oid, r.unique)
	case v.o.Level >= VerifyStandard:
		return v.stmt(r, "btree", "SELECT "+v.amSchema+".bt_index_check($1::regclass, heapallindexed => true, checkunique => $2::bool)", r.oid, r.unique)
	}
	return v.stmt(r, "btree", "SELECT "+v.amSchema+".bt_index_check($1::regclass, heapallindexed => false)", r.oid)
}

// stmt runs one checking statement whose failure means damage.
func (v *verifier) stmt(r rel, kind, sql string, args ...any) error {
	if _, err := v.c.Exec(v.ctx, sql, args...); err != nil {
		return v.failed(r, kind, err)
	}
	v.checked[kind]++
	return nil
}

// failed turns a checking error into a finding; it returns an error only
// when the check itself cannot go on (cancelled, connection lost).
func (v *verifier) failed(r rel, kind string, err error) error {
	if v.ctx.Err() != nil {
		return v.ctx.Err()
	}
	state := sqlState(err)
	obj := "index " + r.name
	if r.kind == "table" {
		obj = "table " + r.name
	}
	switch {
	case state == "":
		return err
	case state == "XX001" || state == "XX002" || (state == "23505" && kind == "btree"):
		fix := "Rebuild the index (REINDEX INDEX CONCURRENTLY ...) on the main node; if this is a copy, re-clone it. Keep the details for support."
		if r.kind == "table" {
			fix = "This copy must be replaced from a healthy node (re-clone); if this is the main node, switch to a healthy copy first. Keep the details for support."
		}
		v.add(Finding{Severity: SeverityBlock, Check: kind, Object: obj, Problem: "The integrity check found damage.", Fix: fix, Detail: err.Error()})
	case state == "55P03":
		v.add(Finding{Severity: SeverityInfo, Check: kind, Object: obj, Problem: "Skipped: it was busy (locked by other work). It is checked next time.", Detail: err.Error()})
	case state == "57014":
		v.add(Finding{Severity: SeverityWarn, Check: kind, Object: obj, Problem: "Skipped: the check took longer than allowed.", Fix: "Run a check with a longer time limit when the database is quiet.", Detail: err.Error()})
	case state == "42P01" || state == "3F000":
		// Dropped while we were checking: nothing to report.
	default:
		v.add(Finding{Severity: SeverityWarn, Check: kind, Object: obj, Problem: "The check could not run.", Detail: err.Error()})
	}
	return nil
}

// indexVsTable compares index answers with table-scan answers for index
// types amcheck cannot verify: the same condition must find exactly the
// same rows (by ctid, in one snapshot) with the index forced on and off.
func (v *verifier) indexVsTable(r rel) error {
	var keyExpr, pred, opfamily string
	var keyAtt int
	err := v.c.QueryRow(v.ctx, `SELECT i.indkey[0], CASE WHEN i.indkey[0] = 0 THEN pg_get_indexdef(i.indexrelid, 1, true)
		  ELSE quote_ident((SELECT attname FROM pg_attribute WHERE attrelid = i.indrelid AND attnum = i.indkey[0])) END,
		coalesce(pg_get_expr(i.indpred, i.indrelid, true), ''), oc.opcfamily::text
		FROM pg_index i JOIN pg_opclass oc ON oc.oid = i.indclass[0] WHERE i.indexrelid = $1`, r.oid).Scan(&keyAtt, &keyExpr, &pred, &opfamily)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return v.failed(r, "index_scan", err)
	}
	cond := ""
	if pred != "" {
		cond = " AND (" + pred + ")"
	}
	// Sample key values from about 4 random pages per sample (repeatable
	// for a given seed), spread over the sampled rows.
	var pages int64
	if err := v.c.QueryRow(v.ctx, "SELECT pg_relation_size($1::regclass) / current_setting('block_size')::int", r.tableOID).Scan(&pages); err != nil {
		return v.failed(r, "index_scan", err)
	}
	pct := 100.0
	if pages > 0 {
		pct = min(100, max(0.0001, 100*float64(v.o.Samples*4)/float64(pages)))
	}
	rows, err := v.c.Query(v.ctx, fmt.Sprintf(`SELECT k::text, pg_typeof(k)::oid, pg_typeof(k)::text FROM (SELECT %s AS k, ctid FROM ONLY %s TABLESAMPLE SYSTEM (%g) REPEATABLE (%d) WHERE true%s) s
		WHERE k IS NOT NULL ORDER BY md5(ctid::text || %d) LIMIT %d`,
		keyExpr, r.table, pct, v.o.Seed, cond, v.o.Seed, v.o.Samples))
	if err != nil {
		return v.failed(r, "index_scan", err)
	}
	type sample struct {
		val, typName string
		typ          uint32
	}
	samples, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (sample, error) {
		var s sample
		return s, row.Scan(&s.val, &s.typ, &s.typName)
	})
	if err != nil {
		return v.failed(r, "index_scan", err)
	}
	if len(samples) == 0 {
		v.checked["index_scan_empty"]++
		return nil
	}
	ops, err := v.searchOperators(opfamily, samples[0].typ)
	if err != nil {
		return v.failed(r, "index_scan", err)
	}
	if len(ops) == 0 {
		v.add(Finding{Severity: SeverityInfo, Check: "index_scan", Object: "index " + r.name, Problem: "No search operator to compare this index with its table."})
		return nil
	}
	used, compared := false, 0
	for _, op := range ops {
		for _, s := range samples {
			q := fmt.Sprintf(`SELECT count(*), md5(coalesce(string_agg(ctid::text, ',' ORDER BY ctid), '')) FROM ONLY %s WHERE (%s) OPERATOR(%s) (%s::text::%s)%s`,
				r.table, keyExpr, op.qualified, quoteLiteral(s.val), s.typName, cond)
			mismatch, usedIndex, detail, err := v.compareScans(r, q)
			if err != nil {
				return v.failed(r, "index_scan", err)
			}
			if !usedIndex {
				continue
			}
			used = true
			compared++
			if mismatch {
				v.add(Finding{Severity: SeverityBlock, Check: "index_scan", Object: "index " + r.name,
					Problem: "The index gives a different answer than the table it belongs to.",
					Fix:     "Rebuild the index (REINDEX INDEX CONCURRENTLY ...) on the main node; if this is a copy, re-clone it. Keep the details for support.",
					Detail:  fmt.Sprintf("condition %s %s %s: %s", keyExpr, op.name, s.val, detail)})
				return nil
			}
		}
	}
	if !used {
		v.add(Finding{Severity: SeverityInfo, Check: "index_scan", Object: "index " + r.name, Problem: "Not compared: the database chose a different index for every test search."})
		return nil
	}
	v.checked["index_scan"]++
	v.checked["index_scan_queries"] += compared
	return nil
}

type searchOp struct{ name, qualified string }

// searchOperators lists boolean search operators of an operator family
// that take the key's type on both sides, best first ("=" and overlap
// first, as they reach every part of the tree), at most two. The key type
// may reach the operator's type through a domain, a binary-compatible
// cast (varchar to text) or a polymorphic type (anyrange).
func (v *verifier) searchOperators(opfamily string, keyType uint32) ([]searchOp, error) {
	rows, err := v.c.Query(v.ctx, `WITH k AS (SELECT CASE WHEN typtype = 'd' THEN typbasetype ELSE oid END AS t FROM pg_type WHERE oid = $2)
		SELECT o.oprname, o.oprnamespace::regnamespace::text || '.' || o.oprname
		FROM pg_amop a JOIN pg_operator o ON o.oid = a.amopopr, k
		WHERE a.amopfamily = $1::oid AND a.amoppurpose = 's' AND a.amoplefttype = a.amoprighttype AND o.oprresult = 'bool'::regtype
		  AND (a.amoplefttype = k.t
		       OR a.amoplefttype IN ('anyrange'::regtype, 'anymultirange'::regtype, 'anyarray'::regtype, 'anyelement'::regtype, 'anyenum'::regtype)
		       OR EXISTS (SELECT 1 FROM pg_cast c WHERE c.castsource = k.t AND c.casttarget = a.amoplefttype AND c.castmethod = 'b'))
		ORDER BY CASE o.oprname WHEN '=' THEN 0 WHEN '&&' THEN 1 WHEN '@>' THEN 2 WHEN '<<' THEN 3 WHEN '~=' THEN 4 ELSE 5 END, a.amopstrategy LIMIT 2`, opfamily, keyType)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (searchOp, error) {
		var o searchOp
		return o, r.Scan(&o.name, &o.qualified)
	})
}

// compareScans runs q with the index forced on, then forced off, in one
// snapshot. usedIndex says whether the plan really used index r.
func (v *verifier) compareScans(r rel, q string) (mismatch, usedIndex bool, detail string, err error) {
	if _, err := v.c.Exec(v.ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return false, false, "", err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(v.ctx), 10*time.Second)
		defer cancel()
		_, _ = v.c.Exec(rctx, "ROLLBACK")
	}()
	exec := func(stmts ...string) error {
		for _, s := range stmts {
			if _, err := v.c.Exec(v.ctx, s); err != nil {
				return err
			}
		}
		return nil
	}
	if err := exec("SET LOCAL enable_seqscan = off", "SET LOCAL enable_indexscan = on", "SET LOCAL enable_bitmapscan = on", "SET LOCAL enable_indexonlyscan = off", "SET LOCAL enable_tidscan = off"); err != nil {
		return false, false, "", err
	}
	// Simple protocol for everything here: a prepared statement keeps its
	// plan when enable_* settings change, so the "table" run would reuse
	// the index plan.
	simple := pgx.QueryExecModeSimpleProtocol
	var plan string
	if err := v.c.QueryRow(v.ctx, "EXPLAIN (FORMAT JSON, COSTS OFF) "+q, simple).Scan(&plan); err != nil {
		return false, false, "", err
	}
	short := r.name
	if i := strings.LastIndex(short, "."); i >= 0 {
		short = short[i+1:]
	}
	short = strings.Trim(short, `"`)
	if !strings.Contains(plan, `"Index Name": "`+short+`"`) {
		return false, false, "", nil
	}
	var n1, n2 int64
	var d1, d2 string
	if err := v.c.QueryRow(v.ctx, q, simple).Scan(&n1, &d1); err != nil {
		return false, true, "", err
	}
	if err := exec("SET LOCAL enable_seqscan = on", "SET LOCAL enable_indexscan = off", "SET LOCAL enable_bitmapscan = off"); err != nil {
		return false, true, "", err
	}
	if err := v.c.QueryRow(v.ctx, "EXPLAIN (FORMAT JSON, COSTS OFF) "+q, simple).Scan(&plan); err != nil {
		return false, true, "", err
	}
	if strings.Contains(plan, `"Index Name"`) {
		return false, false, "", errors.New("could not turn the index off for the table scan")
	}
	if err := v.c.QueryRow(v.ctx, q, simple).Scan(&n2, &d2); err != nil {
		return false, true, "", err
	}
	return n1 != n2 || d1 != d2, true, fmt.Sprintf("%d row(s) through the index, %d through the table", n1, n2), nil
}

// partitions checks that every row sits in the partition its key belongs
// to (hash partitions depend on hash functions, list and range ones on
// comparisons; a difference between machines would put rows in the wrong
// place).
func (v *verifier) partitions() error {
	rows, err := v.c.Query(v.ctx, `SELECT c.oid::regclass::text, pg_get_partition_constraintdef(c.oid)
		FROM pg_class c WHERE c.relispartition AND c.relkind IN ('r', 'm') AND c.relpersistence <> 't' AND pg_get_partition_constraintdef(c.oid) IS NOT NULL ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("list partitions: %w", err)
	}
	type part struct{ name, def string }
	parts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (part, error) {
		var p part
		return p, r.Scan(&p.name, &p.def)
	})
	if err != nil {
		return fmt.Errorf("list partitions: %w", err)
	}
	for _, p := range parts {
		var n int64
		err := v.c.QueryRow(v.ctx, fmt.Sprintf("SELECT count(*) FROM ONLY %s WHERE (%s) IS NOT TRUE", p.name, p.def)).Scan(&n)
		if err != nil {
			if err := v.failed(rel{name: p.name, kind: "table"}, "partition", err); err != nil {
				return err
			}
			continue
		}
		v.checked["partition"]++
		if n > 0 {
			v.add(Finding{Severity: SeverityBlock, Check: "partition", Object: "table " + p.name,
				Problem: fmt.Sprintf("%d row(s) are in a partition they do not belong to.", n),
				Fix:     "Move the rows (UPDATE the key, or re-insert them) on the main node, then find out why: report it to support.",
				Detail:  "partition rule: " + p.def})
		}
	}
	return nil
}

// ---- pg_amcheck and pg_checksums (programs) ----

// PgAmcheckOptions controls RunPgAmcheck.
type PgAmcheckOptions struct {
	Install Install
	Host    string // default 127.0.0.1
	Port    int
	User    string // default Superuser
	// Passfile is a libpq password file for User (never a password on the
	// command line or in the environment).
	Passfile string
	// Deep adds --parent-check --rootdescend (blocks writes per table;
	// primary only). Otherwise --heapallindexed --checkunique.
	Deep bool
	// InstallMissing passes --install-missing (primary only).
	InstallMissing bool
	Jobs           int // default 1
	LogFile        string
}

// pgAmcheckLine matches the start of each problem pg_amcheck reports.
// pg_amcheck prints `btree index "db.schema.name":` or `heap table
// "db.schema.name", block 3, offset 7:`.
var pgAmcheckLine = regexp.MustCompile(`^(btree index|heap table|gin index) "([^"]+)"`)

// RunPgAmcheck runs pg_amcheck --all and turns its report into findings.
// It returns an error only when pg_amcheck could not run.
func RunPgAmcheck(ctx context.Context, o PgAmcheckOptions) ([]Finding, error) {
	if o.Host == "" {
		o.Host = "127.0.0.1"
	}
	if o.User == "" {
		o.User = Superuser
	}
	if o.Jobs <= 0 {
		o.Jobs = 1
	}
	args := []string{"--all", "-h", o.Host, "-p", strconv.Itoa(o.Port), "-U", o.User, "--no-password", "--jobs", strconv.Itoa(o.Jobs)}
	if o.Deep {
		args = append(args, "--heapallindexed", "--parent-check", "--rootdescend", "--checkunique")
	} else {
		args = append(args, "--heapallindexed", "--checkunique")
	}
	if o.InstallMissing {
		args = append(args, "--install-missing")
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, o.Install.Bin("pg_amcheck"), args...)
	env := []string{}
	if o.Passfile != "" {
		env = append(env, "PGPASSFILE="+o.Passfile)
	}
	cmd.Env = toolEnv(env...)
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	text := out.String()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	fs := parsePgAmcheck(text)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return fs, nil
	case errors.As(err, &ee) && ee.ExitCode() == 2:
		if len(fs) == 0 { // corruption reported in a form we did not parse
			fs = append(fs, Finding{Severity: SeverityBlock, Check: "pg_amcheck", Problem: "pg_amcheck found damage.", Detail: tail(text, 20)})
		}
		return fs, nil
	}
	return fs, fmt.Errorf("pg_amcheck: %w: %s", err, tail(text, 10))
}

// parsePgAmcheck reads pg_amcheck's output: each problem starts with a
// line like `btree index "db.schema.name":` followed by indented detail
// lines. The database is the part before the first dot.
func parsePgAmcheck(out string) []Finding {
	var fs []Finding
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if m := pgAmcheckLine.FindStringSubmatch(line); m != nil {
			kind := "btree"
			obj := "index "
			switch m[1] {
			case "heap table":
				kind, obj = "heap", "table "
			case "gin index":
				kind = "gin"
			}
			db, name, _ := strings.Cut(m[2], ".")
			fs = append(fs, Finding{Severity: SeverityBlock, Check: "pg_amcheck_" + kind, Database: db,
				Object: obj + name, Problem: "pg_amcheck found damage.", Detail: strings.TrimSpace(line)})
			continue
		}
		if len(fs) > 0 && strings.HasPrefix(line, "    ") {
			last := &fs[len(fs)-1]
			if len(last.Detail) < 2000 {
				last.Detail += "\n" + strings.TrimSpace(line)
			}
		}
	}
	return fs
}

// ChecksumResult is what pg_checksums --check reports.
type ChecksumResult struct {
	FilesScanned, BlocksScanned, BadChecksums int64
	Problems                                  []string // the error lines naming bad blocks
}

// CheckChecksumsOffline runs pg_checksums --check on a STOPPED data
// folder (pg_checksums refuses a running one). Every page is read and its
// checksum verified.
func CheckChecksumsOffline(ctx context.Context, inst Install, pgdata string) (ChecksumResult, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, inst.Bin("pg_checksums"), "--check", "-D", pgdata)
	cmd.Env = toolEnv()
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	res := ParseChecksums(out.String())
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && res.BadChecksums > 0) {
		return res, fmt.Errorf("pg_checksums: %w: %s", err, tail(out.String(), 5))
	}
	return res, nil
}

// ParseChecksums reads pg_checksums --check output.
func ParseChecksums(out string) ChecksumResult {
	var r ChecksumResult
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		k, val, ok := strings.Cut(line, ":")
		n, perr := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		switch {
		case ok && perr == nil && strings.TrimSpace(k) == "Files scanned":
			r.FilesScanned = n
		case ok && perr == nil && strings.TrimSpace(k) == "Blocks scanned":
			r.BlocksScanned = n
		case ok && perr == nil && strings.TrimSpace(k) == "Bad checksums":
			r.BadChecksums = n
		case strings.Contains(line, "checksum verification failed"):
			r.Problems = append(r.Problems, strings.TrimSpace(line))
		}
	}
	return r
}
