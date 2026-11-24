package pg

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The page check (D55): a slow, resumable read of every data page a copy
// holds, so a page that no longer matches its checksum (bit rot, a bad
// sector, a torn write) is found while the main node is still there to
// copy from, not after a failover made that copy the main node.
//
// Postgres verifies a page's checksum only when it reads the page from
// disk into shared buffers; a page nobody reads stays unnoticed. The page
// check reads every page of every permanent table, materialized view and
// TOAST table (amcheck's verify_heapam, in block ranges) and every B-tree
// index (bt_index_check), in every database that accepts connections.
// Both read through the buffer manager with a bulk-read ring (they do not
// push the workload's pages out of the cache), take only AccessShareLock,
// and work on a hot standby. Without amcheck in a database, tables are
// read with a TID-range scan instead (also checksum-verified, no index
// coverage).
//
// Not read: free-space and visibility maps (rebuilt by VACUUM), unlogged
// tables (a copy holds none), and index types other than B-tree (an index
// can always be rebuilt from its table; a damaged one never loses rows).
// A page already in shared buffers is checked as Postgres holds it in
// memory, not as it is on disk; on a copy that has just started, the
// cache is empty.
//
// Cost: reads are paced to PageScanOptions.BytesPerSecond (default
// DefaultPageScanRate), in chunks of PageScanOptions.ChunkBlocks pages.
// Progress (database, relation, block) is kept in PageScanProgress, so an
// interrupted pass resumes where it stopped instead of starting over.

// DefaultPageScanRate is the page check's default read rate.
const DefaultPageScanRate = 32 << 20

// pageScanChunk: pages read per statement (8 MiB at the default page size).
const pageScanChunk = 1024

// PageScanOptions controls one pass of the page check.
type PageScanOptions struct {
	// BytesPerSecond paces the reads (0: DefaultPageScanRate; < 0: no
	// pacing, for tests).
	BytesPerSecond int64
	// ChunkBlocks is the number of pages read per statement (0: 1024).
	ChunkBlocks int64
	// StatementTimeout per statement (default 10 minutes).
	StatementTimeout time.Duration
	// SaveEvery: how often Save is called while the pass runs (default
	// 10 s); it is always called when the pass ends.
	SaveEvery time.Duration
	// Save receives the progress so far (to keep it across restarts).
	Save func(PageScanProgress)
	// Now is the clock (tests); nil: time.Now.
	Now func() time.Time
}

// PageScanProgress is where a pass is, and what it found so far. It is
// plain data (JSON), kept by the caller between runs.
type PageScanProgress struct {
	// PostmasterStart is pg_postmaster_start_time() of the server the pass
	// runs on: a pass resumes only on the same running server (a restart
	// may follow damage, so it starts a new pass).
	PostmasterStart string    `json:"postmaster_start"`
	Started         time.Time `json:"started"`
	// Done is set when the pass finished.
	Done time.Time `json:"done,omitzero"`
	// TotalBytes is the size of the databases when the pass started (for
	// a percentage only).
	TotalBytes int64 `json:"total_bytes"`
	BytesRead  int64 `json:"bytes_read"`
	// DoneDBs are the databases already finished; DB, RelOID and
	// NextBlock are the resume point inside the current one.
	DoneDBs   []string `json:"done_dbs,omitempty"`
	DB        string   `json:"db,omitempty"`
	RelOID    uint32   `json:"rel_oid,omitempty"`
	NextBlock int64    `json:"next_block,omitempty"`
	// Relations counts the relations fully read.
	Relations int `json:"relations"`
	// Skipped: parts that could not be read this pass (busy, cancelled by
	// the copy's replay, timed out). They are read next pass.
	Skipped int `json:"skipped,omitempty"`
	// Damage is what was found, in plain words (at most maxPageDamage).
	Damage []string `json:"damage,omitempty"`
	// NoAmcheck lists databases read without amcheck (tables only).
	NoAmcheck []string `json:"no_amcheck,omitempty"`
}

// Percent is how far the pass got (0-100).
func (p PageScanProgress) Percent() int {
	if !p.Done.IsZero() {
		return 100
	}
	if p.TotalBytes <= 0 {
		return 0
	}
	return int(min(99, p.BytesRead*100/p.TotalBytes))
}

const maxPageDamage = 8

func (p *PageScanProgress) addDamage(s string) {
	if len(p.Damage) < maxPageDamage && !slices.Contains(p.Damage, s) {
		p.Damage = append(p.Damage, s)
	}
}

// ChecksumFailures is Postgres' own count of pages that failed their
// checksum when read, over all databases, and the latest failure ("" if
// none). The counters live in the statistics, which a crash (and a
// re-clone) resets; callers keep a failure they saw.
func ChecksumFailures(ctx context.Context, q Querier) (int64, string, error) {
	var n int64
	var last string
	err := q.QueryRow(ctx, `SELECT coalesce(sum(checksum_failures), 0)::bigint,
		coalesce(max(checksum_last_failure)::text, '') FROM pg_stat_database`).Scan(&n, &last)
	if err != nil {
		return 0, "", fmt.Errorf("read checksum counters: %w", err)
	}
	return n, last, nil
}

// PostmasterStart is pg_postmaster_start_time() as text.
func PostmasterStart(ctx context.Context, q Querier) (string, error) {
	var s string
	if err := q.QueryRow(ctx, "SELECT pg_postmaster_start_time()::text").Scan(&s); err != nil {
		return "", fmt.Errorf("read the server start time: %w", err)
	}
	return s, nil
}

// NewPageScan starts a pass on the server behind connect.
func NewPageScan(ctx context.Context, connect ConnectFunc, now time.Time) (PageScanProgress, error) {
	c, err := connect(ctx, "postgres")
	if err != nil {
		return PageScanProgress{}, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	start, err := PostmasterStart(ctx, c)
	if err != nil {
		return PageScanProgress{}, err
	}
	p := PageScanProgress{PostmasterStart: start, Started: now.UTC()}
	if err := c.QueryRow(ctx, "SELECT coalesce(sum(pg_database_size(oid)), 0)::bigint FROM pg_database WHERE datallowconn").Scan(&p.TotalBytes); err != nil {
		return p, fmt.Errorf("database sizes: %w", err)
	}
	return p, nil
}

// ScanPages runs (or resumes) the pass in p until it is finished, damage
// is found, or ctx ends. It returns the progress; p.Done is set when the
// pass finished. Damage stops the pass at once (p.Damage is not empty):
// one damaged page is enough to replace the copy.
func ScanPages(ctx context.Context, connect ConnectFunc, o PageScanOptions, p PageScanProgress) (PageScanProgress, error) {
	if o.BytesPerSecond == 0 {
		o.BytesPerSecond = DefaultPageScanRate
	}
	if o.ChunkBlocks <= 0 {
		o.ChunkBlocks = pageScanChunk
	}
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = 10 * time.Minute
	}
	if o.SaveEvery <= 0 {
		o.SaveEvery = 10 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &pageScan{o: o, p: p, lastSave: o.Now()}
	err := s.run(ctx, connect)
	if o.Save != nil {
		o.Save(s.p)
	}
	return s.p, err
}

type pageScan struct {
	o        PageScanOptions
	p        PageScanProgress
	lastSave time.Time
	// pacing: bytes read since paceFrom.
	paceFrom  time.Time
	paceBytes int64
}

func (s *pageScan) run(ctx context.Context, connect ConnectFunc) error {
	c, err := connect(ctx, "postgres")
	if err != nil {
		return err
	}
	start, err := PostmasterStart(ctx, c)
	var dbs []string
	if err == nil {
		dbs, err = ListDatabases(ctx, c)
	}
	c.Close(context.WithoutCancel(ctx))
	if err != nil {
		return err
	}
	if start != s.p.PostmasterStart {
		return errors.New("the database server restarted; the page check starts a new pass")
	}
	s.paceFrom = s.o.Now()
	for _, db := range dbs {
		if slices.Contains(s.p.DoneDBs, db) {
			continue
		}
		if s.p.DB != db {
			s.p.DB, s.p.RelOID, s.p.NextBlock = db, 0, 0
		}
		if err := s.database(ctx, connect, db); err != nil {
			return err
		}
		if len(s.p.Damage) > 0 {
			return nil
		}
		s.p.DoneDBs = append(s.p.DoneDBs, db)
		s.p.DB, s.p.RelOID, s.p.NextBlock = "", 0, 0
	}
	s.p.Done = s.o.Now().UTC()
	return nil
}

// pageRel is one relation the page check reads.
type pageRel struct {
	oid   uint32
	name  string
	btree bool
}

func (s *pageScan) database(ctx context.Context, connect ConnectFunc, db string) error {
	c, err := connect(ctx, db)
	if err != nil {
		if sqlState(err) == "3D000" {
			return nil // dropped since it was listed
		}
		return fmt.Errorf("page check %s: %w", db, err)
	}
	defer c.Close(context.WithoutCancel(ctx))
	for _, q := range []string{
		fmt.Sprintf("SET statement_timeout = %d", s.o.StatementTimeout.Milliseconds()),
		"SET lock_timeout = 2000",
		"SET search_path = pg_catalog",
		"SET application_name = '" + adminAppName + " page check'",
		"SET max_parallel_workers_per_gather = 0",
	} {
		if _, err := c.Exec(ctx, q); err != nil {
			return fmt.Errorf("page check %s: %w", db, err)
		}
	}
	var am string
	err = c.QueryRow(ctx, "SELECT quote_ident(extnamespace::regnamespace::text) FROM pg_extension WHERE extname = 'amcheck'").Scan(&am)
	if errors.Is(err, pgx.ErrNoRows) {
		am = ""
		if !slices.Contains(s.p.NoAmcheck, db) {
			s.p.NoAmcheck = append(s.p.NoAmcheck, db)
		}
	} else if err != nil {
		return fmt.Errorf("page check %s: %w", db, err)
	}
	rows, err := c.Query(ctx, `SELECT c.oid, c.oid::regclass::text, c.relkind = 'i'
		FROM pg_class c LEFT JOIN pg_index i ON i.indexrelid = c.oid LEFT JOIN pg_am a ON a.oid = c.relam
		WHERE c.relpersistence = 'p' AND c.oid >= $1 AND (c.relkind IN ('r', 'm', 't')
		   OR (c.relkind = 'i' AND a.amname = 'btree' AND i.indisvalid AND i.indisready AND i.indislive AND $2))
		ORDER BY c.oid`, s.p.RelOID, am != "")
	if err != nil {
		return fmt.Errorf("page check %s: list relations: %w", db, err)
	}
	rels, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (pageRel, error) {
		var x pageRel
		return x, r.Scan(&x.oid, &x.name, &x.btree)
	})
	if err != nil {
		return fmt.Errorf("page check %s: list relations: %w", db, err)
	}
	for _, r := range rels {
		if r.oid != s.p.RelOID {
			s.p.RelOID, s.p.NextBlock = r.oid, 0
		}
		if err := s.relation(ctx, c, db, am, r); err != nil {
			return err
		}
		if len(s.p.Damage) > 0 {
			return nil
		}
		s.p.Relations++
	}
	return nil
}

// relation reads one relation from s.p.NextBlock on.
func (s *pageScan) relation(ctx context.Context, c *pgx.Conn, db, am string, r pageRel) error {
	if r.btree {
		var size int64
		if err := c.QueryRow(ctx, "SELECT pg_relation_size($1::regclass)", r.oid).Scan(&size); err != nil {
			return s.failed(ctx, c, db, r, err)
		}
		if _, err := c.Exec(ctx, "SELECT "+am+".bt_index_check($1::regclass, false)", r.oid); err != nil {
			return s.failed(ctx, c, db, r, err)
		}
		return s.read(ctx, size)
	}
	for {
		var blocks int64
		if err := c.QueryRow(ctx, "SELECT pg_relation_size($1::regclass) / current_setting('block_size')::bigint", r.oid).Scan(&blocks); err != nil {
			return s.failed(ctx, c, db, r, err)
		}
		if s.p.NextBlock >= blocks {
			return nil
		}
		end := min(s.p.NextBlock+s.o.ChunkBlocks, blocks) - 1
		var err error
		if am != "" {
			var n int64
			var first string
			err = c.QueryRow(ctx, `SELECT count(*), coalesce(min('page ' || blkno || ': ' || msg), '') FROM `+am+
				`.verify_heapam($1::regclass, on_error_stop => true, check_toast => false, skip => 'none', startblock => $2, endblock => $3)`,
				r.oid, s.p.NextBlock, end).Scan(&n, &first)
			if err == nil && n > 0 {
				s.p.addDamage(fmt.Sprintf("%s in database %s: %s", r.name, db, first))
				return nil
			}
		} else {
			_, err = c.Exec(ctx, fmt.Sprintf("SELECT count(*) FROM ONLY %s WHERE ctid >= '(%d,0)'::tid AND ctid < '(%d,0)'::tid",
				r.name, s.p.NextBlock, end+1))
		}
		if err != nil {
			return s.failed(ctx, c, db, r, err)
		}
		bs := (end + 1 - s.p.NextBlock) * 8192
		s.p.NextBlock = end + 1
		if err := s.read(ctx, bs); err != nil {
			return err
		}
	}
}

// failed sorts a statement error: damage, a relation that went away, a
// part to read next pass, or a reason to stop the pass.
func (s *pageScan) failed(ctx context.Context, c *pgx.Conn, db string, r pageRel, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	state := sqlState(err)
	switch {
	case state == "":
		return fmt.Errorf("page check %s: %w", db, err) // connection lost
	case state == "XX001" || state == "XX002":
		s.p.addDamage(fmt.Sprintf("%s in database %s: %s", r.name, db, pgMessage(err)))
		return nil
	case state == "42P01" || state == "3F000":
		s.p.NextBlock = 1 << 62 // dropped while being read
		return nil
	}
	// The relation may have been dropped (by the main node, replayed
	// here) between the listing and this statement.
	var exists bool
	if qerr := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_class WHERE oid = $1)", r.oid).Scan(&exists); qerr == nil && !exists {
		s.p.NextBlock = 1 << 62
		return nil
	}
	// Busy (55P03), timed out (57014), cancelled by the copy's replay
	// (40001), or something else: this part is read next pass.
	s.p.Skipped++
	s.p.NextBlock = 1 << 62
	return nil
}

// pgMessage is the server's message without the driver's decorations.
func pgMessage(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, " (SQLSTATE"); i > 0 {
		msg = msg[:i]
	}
	return strings.TrimPrefix(msg, "ERROR: ")
}

// read accounts for n bytes read, paces to the rate, and saves progress.
func (s *pageScan) read(ctx context.Context, n int64) error {
	s.p.BytesRead += n
	s.paceBytes += n
	now := s.o.Now()
	if s.o.Save != nil && now.Sub(s.lastSave) >= s.o.SaveEvery {
		s.o.Save(s.p)
		s.lastSave = now
	}
	if s.o.BytesPerSecond < 0 {
		return ctx.Err()
	}
	due := s.paceFrom.Add(time.Duration(float64(s.paceBytes) / float64(s.o.BytesPerSecond) * float64(time.Second)))
	wait := due.Sub(now)
	if wait <= 0 {
		if -wait > 5*time.Second {
			// Long behind (slow disk): do not burst to catch up.
			s.paceFrom, s.paceBytes = now, 0
		}
		return ctx.Err()
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
