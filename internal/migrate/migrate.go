package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"coyotedb.com/coyote/internal/pg"
)

// Options controls Run.
type Options struct {
	// Source is the connection string of the database to move (URI or
	// key=value). It may hold a password; it is never printed or put on a
	// command line.
	Source  string
	Target  Target
	Install pg.Install // pg_dump and pg_restore come from here
	// Jobs is the number of tables copied at once (0: picked from the CPUs).
	Jobs   int
	DryRun bool
	// Live starts a live copy (logical replication) instead of a one-time
	// copy; Cutover finishes it (see live.go).
	Live           bool
	Replace        bool // replace a non-empty target (asks Confirm first)
	KeepGoing      bool // switch in even if some non-table objects failed
	ExcludeSchemas []string
	SkipExtensions []string
	// WorkDir holds the dump while the copy runs (default: the system's
	// temporary folder). It is removed afterwards.
	WorkDir string
	// Out receives the plain-words report.
	Out io.Writer
	// Confirm asks the person to type answer to go on (for --replace).
	// nil means nobody can answer: --replace is refused.
	Confirm func(question, answer string) bool
	// Log, if set, receives every step and the tools' raw output.
	Log io.Writer
}

// Result is what a finished move reports.
type Result struct {
	Database  string
	URL       string // the app's connection string for Database (with password)
	Tables    int
	Rows      int64
	Sequences int // sequences moved past their column's highest value
	NotCopied []string
}

// Errors Run returns (the details were already printed to Out).
var (
	// ErrBlocked: the preflight found something that stops the move.
	ErrBlocked = errors.New("the move cannot start")
	// ErrMainChanged: the main node changed (or was lost) during the copy;
	// nothing was switched in and the command can be run again.
	ErrMainChanged = errors.New("the cluster's main node changed during the copy")
	// ErrNotSwitched: the copy did not match the source or some objects
	// failed; nothing was switched in.
	ErrNotSwitched = errors.New("the copy was not switched in")
)

type mover struct {
	o       Options
	mu      sync.Mutex
	src     connParams
	plan    plan
	srcInfo *source
	tgtInfo *targetInfo
	jobs    int
	adm     *pgx.Conn // app role on the main node's "postgres" database
}

func (m *mover) say(format string, a ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := fmt.Sprintf(format, a...)
	if m.o.Out != nil {
		fmt.Fprintln(m.o.Out, s)
	}
	if m.o.Log != nil {
		fmt.Fprintf(m.o.Log, "%s %s\n", time.Now().Format(time.RFC3339), s)
	}
}

func (m *mover) logf(format string, a ...any) {
	if m.o.Log == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprintf(m.o.Log, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
}

// Run checks the source and the cluster, and unless DryRun, copies the
// source into Target.Database, checks the copy and switches it in.
func Run(ctx context.Context, o Options) (*Result, error) {
	if o.Target.Database == "" {
		o.Target.Database = pg.AppDatabase
	}
	m := &mover{o: o, jobs: o.Jobs}
	if m.jobs <= 0 {
		m.jobs = min(4, max(2, runtime.NumCPU()))
	}
	if m.o.WorkDir == "" {
		m.o.WorkDir = os.TempDir()
	}
	p, err := parseConnString(o.Source)
	if err != nil {
		return nil, err
	}
	m.src = p

	m.say("Checking the source database…")
	srcConn, err := connectSource(ctx, p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = srcConn.Close(context.WithoutCancel(ctx)) }()
	s, err := inspectSource(ctx, srcConn, p)
	if err != nil {
		return nil, err
	}
	m.srcInfo = s
	excl, _ := excludedSchemas(s, o.ExcludeSchemas)
	if s.versionNum >= minSourceVersion && s.versionNum/10000 <= maxSourceMajor {
		if err := s.inspectObjects(ctx, srcConn, excl); err != nil {
			return nil, err
		}
		if o.Live {
			if err := s.inspectLive(ctx, srcConn); err != nil {
				return nil, err
			}
		}
	}

	m.say("Checking the cluster…")
	mainNode, err := o.Target.Main(ctx)
	if err != nil {
		return nil, fmt.Errorf("ask the cluster for its main node: %w", err)
	}
	adm, err := o.Target.connect(ctx, "postgres")
	if err != nil {
		return nil, err
	}
	defer func() { _ = adm.Close(context.WithoutCancel(ctx)) }()
	ti, err := inspectTarget(ctx, o.Target, adm)
	if err != nil {
		return nil, err
	}
	m.tgtInfo = ti
	adminReady := false
	if o.Target.Admin != nil {
		if c, err := o.Target.Admin(ctx, "postgres"); err == nil && c != nil {
			adminReady = true
			_ = c.Close(context.WithoutCancel(ctx))
		}
	}
	free := int64(-1)
	if f, err := freeBytes(m.o.WorkDir); err == nil {
		free = int64(min(f, uint64(1<<62)))
	}
	m.plan = buildPlan(planInput{
		src: s, tgt: ti, database: o.Target.Database, mainName: mainNode.Name, replace: o.Replace,
		userSkip: o.SkipExtensions, userExcl: o.ExcludeSchemas, adminReady: adminReady,
		freeBytes: free, workDir: m.o.WorkDir, live: o.Live,
	})
	m.printPlan()
	if len(m.plan.blockers) > 0 {
		m.say("")
		m.say("Not ready to move (nothing was copied):")
		for _, b := range m.plan.blockers {
			m.say("  - %s", b)
		}
		return nil, ErrBlocked
	}
	if o.DryRun {
		m.say("")
		m.say("Dry run: everything checks out. Nothing was copied. Run the same command without --dry-run to move the data.")
		return nil, nil
	}
	if m.plan.replacing {
		q := fmt.Sprintf("This deletes everything in database %s on the cluster and puts the copy in its place.\nType %s to go on: ", o.Target.Database, o.Target.Database)
		if o.Confirm == nil || !o.Confirm(q, o.Target.Database) {
			m.say("Not confirmed; nothing was changed.")
			return nil, ErrBlocked
		}
	}
	m.adm = adm
	return m.move(ctx, srcConn, adm, mainNode)
}

func connectSource(ctx context.Context, p connParams) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(p.withPassword())
	if err != nil {
		return nil, fmt.Errorf("the source connection string could not be read: %w", cleanErr(err))
	}
	if _, ok := p.vals["application_name"]; !ok {
		cfg.RuntimeParams["application_name"] = "coyote-migrate"
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 15 * time.Second
	}
	c, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("could not connect to the source (%s): %w", p.describe(), cleanErr(err))
	}
	return c, nil
}

func (m *mover) printPlan() {
	m.say("")
	for _, r := range m.plan.summary {
		m.say("  %-11s %s", r[0]+":", r[1])
	}
	for i, l := range m.plan.leftOut {
		label := ""
		if i == 0 {
			label = "Left out:"
		}
		m.say("  %-11s %s", label, l)
	}
	if len(m.plan.notes) > 0 {
		m.say("")
		for _, n := range m.plan.notes {
			m.say("  Note: %s", n)
		}
	}
}

// move does the copy. Every step that changes the cluster is done as the
// application role through the client port, so it lands on the main node
// and reaches the copies by normal replication.
func (m *mover) move(ctx context.Context, srcConn, adm *pgx.Conn, main0 MainNode) (*Result, error) {
	o := m.o
	db := o.Target.Database
	work, err := os.MkdirTemp(m.o.WorkDir, "coyote-migrate-")
	if err != nil {
		return nil, fmt.Errorf("make a work folder in %s: %w", m.o.WorkDir, err)
	}
	defer os.RemoveAll(work)
	dumpDir := filepath.Join(work, "dump")

	// One snapshot for the dump and for the source side of the check, so
	// they agree even while the app keeps writing to the source. A live
	// copy dumps only the schema; the subscription copies the rows.
	srcStats := make([]tableStats, len(m.srcInfo.tables))
	var srcLO int64
	statsErr := make(chan error, 1)
	sctx, stopStats := context.WithCancel(ctx)
	defer stopStats()
	snapArg := "--schema-only"
	var tx pgx.Tx
	if o.Live {
		statsErr <- nil
	} else {
		tx, err = srcConn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, fmt.Errorf("start reading the source: %w", cleanErr(err))
		}
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
		var snap string
		if err := tx.QueryRow(ctx, "SELECT pg_export_snapshot()").Scan(&snap); err != nil {
			return nil, fmt.Errorf("the source cannot give a consistent snapshot (%v). If this is a connection pooler's address (for example port 6543 on Supabase), use the direct database address instead", cleanErr(err))
		}
		for _, q := range localOutput() {
			if _, err := tx.Exec(ctx, q); err != nil {
				return nil, fmt.Errorf("%s: %w", q, cleanErr(err))
			}
		}
		snapArg = "--snapshot=" + snap
		go func() { // runs while pg_dump reads; same snapshot
			for i, t := range m.srcInfo.tables {
				st, err := readStats(sctx, tx.QueryRow(sctx, statsQuery(t)), t)
				if err != nil {
					statsErr <- err
					return
				}
				srcStats[i] = st
			}
			// The transaction stays open until pg_dump is done: its
			// workers take up the snapshot only when they start.
			statsErr <- tx.QueryRow(sctx, "SELECT count(*) FROM pg_largeobject_metadata").Scan(&srcLO)
		}()
	}

	m.say("")
	if o.Live {
		m.say("Copying the tables' definitions out of the source…")
	} else {
		m.say("Copying out of the source (%d at a time)…", m.jobs)
	}
	prog := newProgress(m, "copied out", m.srcInfo.tables, nil)
	args := []string{"--format=directory", "--jobs=" + strconv.Itoa(m.jobs), "--file=" + dumpDir,
		snapArg, "--encoding=UTF8", "--no-publications", "--no-subscriptions",
		"--no-security-labels", "--no-tablespaces", "--verbose"}
	for _, s := range m.plan.excludedSchemas {
		args = append(args, "--exclude-schema="+pattern(s))
	}
	for _, e := range m.plan.skipExtensions {
		args = append(args, "--exclude-extension="+pattern(e))
	}
	args = append(args, "--dbname="+m.src.conninfo())
	stopBeat := prog.heartbeat(ctx, "written", func(context.Context) (int64, error) { return dirSize(dumpDir) })
	tail, err := tool{Name: "pg_dump", Bin: o.Install.Bin("pg_dump"), Args: args, Password: m.src.password,
		Log: o.Log, OnLine: prog.dumpLine}.run(ctx)
	stopBeat()
	if err != nil {
		stopStats()
		<-statsErr // the session is not shared with anyone after this
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("copying out of the source failed: %w\n%s", err, indent(tail))
	}
	if err := <-statsErr; err != nil {
		return nil, fmt.Errorf("counting rows on the source: %w", err)
	}
	if !o.Live {
		_ = tx.Rollback(ctx)
		m.say("  copied out %d %s.", len(m.srcInfo.tables), plural(len(m.srcInfo.tables), "table", "tables"))
	}

	// The archive's table of contents decides the restore order.
	listPath := filepath.Join(work, "toc.list")
	if tail, err := (tool{Name: "pg_restore", Bin: o.Install.Bin("pg_restore"),
		Args: []string{"--list", "--format=directory", "--file=" + listPath, dumpDir}, Log: o.Log}).run(ctx); err != nil {
		return nil, fmt.Errorf("read the copy's contents: %w\n%s", err, indent(tail))
	}
	f, err := os.Open(listPath)
	if err != nil {
		return nil, err
	}
	entries, err := parseTOC(f)
	f.Close()
	if err != nil {
		return nil, fmt.Errorf("read the copy's contents: %w", err)
	}
	tp := planTOC(entries, m.plan.dropPolicies)

	// Staging database: the copy goes in under a name that says it is not
	// finished, and gets the real name only after it has been checked.
	if err := m.sameMain(ctx, main0); err != nil {
		return nil, err
	}
	staging := stagingName(db)
	if err := m.prepareStaging(ctx, adm); err != nil {
		return nil, m.mainLost(ctx, main0, err)
	}

	m.say("")
	if o.Live {
		m.say("Creating the tables on the cluster (main node %s)…", main0.Name)
	} else {
		m.say("Copying into the cluster (main node %s)…", main0.Name)
	}
	stURL, err := o.Target.dbURL(staging)
	if err != nil {
		return nil, err
	}
	res := &restoreLog{}
	restore := func(entries []tocEntry, name string, jobs int, onLine func(string)) error {
		lp := filepath.Join(work, name)
		if err := os.WriteFile(lp, []byte(listFile(entries)), 0o600); err != nil {
			return err
		}
		_, err := tool{Name: "pg_restore", Bin: o.Install.Bin("pg_restore"), Args: []string{
			"--format=directory", "--jobs=" + strconv.Itoa(jobs), "--no-owner", "--no-privileges",
			"--no-tablespaces", "--verbose", "--use-list=" + lp, "--dbname=" + stURL, dumpDir,
		}, Password: o.Target.Password, Log: o.Log, OnLine: onLine}.run(ctx)
		return err
	}
	var restoreErr error
	if len(tp.schemas) > 0 {
		restoreErr = restore(tp.schemas, "schemas.list", 1, res.line)
	}
	if restoreErr == nil && len(m.plan.adminExtensions) > 0 {
		restoreErr = m.adminExtensions(ctx, staging)
	}
	if restoreErr == nil && !res.lost() {
		rp := newProgress(m, "copied in", m.srcInfo.tables, tp.dataByID)
		rp.post = len(tp.rest) - len(tp.dataByID)
		// adm is idle while pg_restore runs: the heartbeat may use it.
		stopBeat := rp.heartbeat(ctx, "copied in", func(ctx context.Context) (int64, error) {
			var n int64
			err := adm.QueryRow(ctx, "SELECT pg_database_size($1)", staging).Scan(&n)
			return n, err
		})
		restoreErr = restore(tp.rest, "rest.list", m.jobs, func(l string) { res.line(l); rp.restoreLine(l) })
		stopBeat()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := m.sameMain(ctx, main0); err != nil {
		return nil, err
	}
	if res.lost() {
		return nil, m.mainLost(ctx, main0, errors.New(res.lostLine))
	}
	if restoreErr != nil && len(res.errs) == 0 {
		return nil, m.notSwitched(ctx, adm, fmt.Errorf("copying into the cluster failed: %w", restoreErr))
	}
	for _, l := range tp.leftOut {
		m.say("  Left out: %s", l)
	}
	if len(res.errs) > 0 {
		m.say("")
		m.say("These parts of the database could not be created on the cluster:")
		for _, e := range limit(res.errs, 15) {
			m.say("  - %s", e)
		}
		if !o.KeepGoing {
			m.say("")
			m.say("Nothing was switched in: database %s is unchanged. Fix these on the source and run the command again, or add --keep-going to finish without them (every table must still match the source).", db)
			return nil, m.notSwitched(ctx, adm, nil)
		}
		m.say("  (--keep-going: finishing without them)")
	}

	if o.Live {
		return nil, m.startLive(ctx, adm, main0)
	}

	// Check the copy against the source's snapshot.
	m.say("")
	m.say("Checking the copy against the source…")
	cp, err := o.Target.connect(ctx, staging)
	if err != nil {
		return nil, m.mainLost(ctx, main0, err)
	}
	defer func() { _ = cp.Close(context.WithoutCancel(ctx)) }()
	for _, q := range stableOutput {
		if _, err := cp.Exec(ctx, q); err != nil {
			return nil, m.mainLost(ctx, main0, err)
		}
	}
	var bad []string
	var rows int64
	full, sampled, counted := 0, 0, 0
	for i, t := range m.srcInfo.tables {
		st, err := readStats(ctx, cp.QueryRow(ctx, statsQuery(t)), t)
		if sqlState(err) == "42P01" {
			bad = append(bad, t.label()+": missing in the copy")
			continue
		}
		if err != nil {
			return nil, m.mainLost(ctx, main0, err)
		}
		if d := mismatch(t, srcStats[i], st); d != "" {
			bad = append(bad, d)
		}
		rows += st.Rows
		switch st.How {
		case "every row":
			full++
		case "row count":
			counted++
		default:
			sampled++
		}
	}
	var dstLO int64
	if err := cp.QueryRow(ctx, "SELECT count(*) FROM pg_largeobject_metadata").Scan(&dstLO); err != nil {
		return nil, m.mainLost(ctx, main0, err)
	}
	if dstLO != srcLO {
		bad = append(bad, fmt.Sprintf("large objects: %d on the source, %d in the copy", srcLO, dstLO))
	}
	if len(bad) > 0 {
		m.say("The copy does not match the source:")
		for _, b := range limit(bad, 20) {
			m.say("  - %s", b)
		}
		m.say("Nothing was switched in: database %s is unchanged.", db)
		return nil, m.notSwitched(ctx, adm, nil)
	}
	how := fmt.Sprintf("%d with every row compared", full)
	if sampled > 0 {
		how += fmt.Sprintf(", %d by row count and the first and last %d rows", sampled, sampleRows)
	}
	if counted > 0 {
		how += fmt.Sprintf(", %d by row count only (large, no primary key)", counted)
	}
	m.say("  %d %s the source (%s); %s rows.", len(m.srcInfo.tables), plural(len(m.srcInfo.tables), "table matches", "tables match"), how, humanCount(float64(rows)))

	fixes, nseq, err := fixSequences(ctx, cp)
	if err != nil {
		return nil, m.mainLost(ctx, main0, err)
	}
	if nseq > 0 {
		m.say("  %d %s checked; %d moved past the highest id in use.", nseq, plural(nseq, "sequence", "sequences"), len(fixes))
	}
	for _, f := range fixes {
		m.logf("sequence %s for %s moved from %d to %d", f.Sequence, f.Column, f.From, f.To)
	}
	if _, err := cp.Exec(ctx, "ANALYZE"); err != nil {
		return nil, m.mainLost(ctx, main0, err)
	}
	_ = cp.Close(ctx)

	// Switch in, unless the main node changed since the copy started.
	if err := m.sameMain(ctx, main0); err != nil {
		return nil, err
	}
	if err := m.switchIn(ctx, adm); err != nil {
		return nil, m.mainLost(ctx, main0, err)
	}
	r := &Result{Database: db, URL: o.Target.AppURLFor(db), Tables: len(m.srcInfo.tables), Rows: rows,
		Sequences: len(fixes), NotCopied: res.errs}
	m.say("")
	m.say("Done. Database %s on the cluster now holds the copy, and the other nodes receive it as usual.", db)
	return r, nil
}

// sameMain fails with ErrMainChanged if the cluster has a different main
// node (or epoch) than when the copy started.
func (m *mover) sameMain(ctx context.Context, main0 MainNode) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	now, err := m.o.Target.Main(cctx)
	if err == nil && now.Epoch == main0.Epoch && now.Name == main0.Name {
		return nil
	}
	if err != nil {
		m.say("")
		m.say("Lost contact with the cluster during the copy (%v).", err)
	} else {
		m.say("")
		m.say("The cluster's main node changed during the copy (was %s, now %s).", main0.Name, orNone(now.Name))
	}
	m.say("Nothing was switched in: database %s is unchanged. Once `coyote status` shows a healthy main node, run the same command again; the unfinished copy is cleared first.", m.o.Target.Database)
	return ErrMainChanged
}

// mainLost turns a failure that happened while talking to the cluster into
// ErrMainChanged if the main node changed, or reports it as it is.
func (m *mover) mainLost(ctx context.Context, main0 MainNode, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var pe *pgconn.PgError
	if errors.As(cause, &pe) && !serverGoingAway(pe.Code) {
		// The server answered: a real error, not a lost main node.
		return m.notSwitched(ctx, m.adm, cause)
	}
	// A failover takes a lease (about 10 s) to show: wait for it before
	// deciding what happened.
	deadline := time.Now().Add(30 * time.Second)
	for {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		now, err := m.o.Target.Main(cctx)
		cancel()
		if err == nil && (now.Epoch != main0.Epoch || now.Name != main0.Name) {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	if err := m.sameMain(ctx, main0); err != nil {
		m.logf("cause: %v", cause)
		return err
	}
	m.say("")
	m.say("The connection to the cluster's main node (%s) was lost during the copy: %v", main0.Name, cleanErr(cause))
	m.say("Nothing was switched in: database %s is unchanged. Run the same command again; the unfinished copy is cleared first.", m.o.Target.Database)
	return ErrMainChanged
}

// notSwitched removes the unfinished copy (best effort) and returns why.
func (m *mover) notSwitched(ctx context.Context, adm *pgx.Conn, cause error) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	if err := dropDatabase(cctx, adm, stagingName(m.o.Target.Database)); err != nil {
		m.logf("could not remove the unfinished copy: %v", err)
	}
	if cause != nil {
		m.say("")
		m.say("Error: %v", cleanErr(cause))
		m.say("Nothing was switched in: database %s is unchanged.", m.o.Target.Database)
		return fmt.Errorf("%w: %w", ErrNotSwitched, cause)
	}
	return ErrNotSwitched
}

func orNone(s string) string {
	if s == "" {
		return "none yet"
	}
	return s
}

func limit(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(slices.Clone(xs[:n]), fmt.Sprintf("… and %d more (all of them are in the log)", len(xs)-n))
}

func indent(s string) string {
	if s == "" {
		return ""
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

// pattern quotes a name for pg_dump's -N / --exclude-extension, which take
// patterns: inside double quotes every character is literal.
func pattern(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// adminExtensions creates extensions the app role may not create, as the
// administrator on this machine (only when it is the main node).
func (m *mover) adminExtensions(ctx context.Context, staging string) error {
	c, err := m.o.Target.Admin(ctx, staging)
	if err != nil {
		return err
	}
	if c == nil {
		return errors.New("this computer is no longer the main node, so it cannot create the extensions that need the administrator")
	}
	defer c.Close(context.WithoutCancel(ctx))
	for _, e := range m.plan.adminExtensions {
		q := "CREATE EXTENSION IF NOT EXISTS " + pgx.Identifier{e.Name}.Sanitize() + " WITH SCHEMA " + pgx.Identifier{e.Schema}.Sanitize()
		if _, err := c.Exec(ctx, q); err != nil {
			return fmt.Errorf("create extension %s: %w", e.Name, cleanErr(err))
		}
		m.say("  created extension %s (as the administrator)", e.Name)
	}
	return nil
}

// serverGoingAway: SQLSTATEs a server sends when it is shutting down or
// was made read-only (the main node stopping or being replaced).
func serverGoingAway(code string) bool {
	switch code {
	case "57P01", "57P02", "57P03", "25006", "08000", "08003", "08006":
		return true
	}
	return false
}
