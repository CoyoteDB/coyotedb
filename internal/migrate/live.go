package migrate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Live copy (D47, phase 2): the cluster's main node subscribes to a
// publication on the source (PostgreSQL logical replication). The tables'
// definitions are copied first as in a one-time move; the subscription then
// copies the rows and keeps applying every change until the cutover, which
// checks the copy, copies the sequences, removes the subscription and the
// publication, and switches the copy in.
//
// The subscription lives in the staging database's catalog, so it reaches
// the copies by physical replication like everything else: after a
// failover the new main node's Postgres resumes it from the last change it
// holds. Its commits wait for a copy (synchronous_commit = on), so the
// source never forgets a change the new main node lacks. Creating and
// dropping a subscription needs the administrator, who logs in only on the
// main node's own computer: --live, --status, --cutover and --cancel run
// there.

// liveComment starts the staging database's comment while a live copy runs.
const liveComment = "coyote migrate: live copy"

// ErrNotMain: the command must run on the main node's computer.
var ErrNotMain = errors.New("this computer is not the main node")

// liveName is the publication, subscription and slot name for db.
func liveName(db string) string {
	var b strings.Builder
	b.WriteString("coyote_migrate_")
	for _, r := range strings.ToLower(db) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

// startLive runs after the tables' definitions are in the staging
// database: publication on the source, subscription on the main node,
// then it follows the first copy of the rows.
func (m *mover) startLive(ctx context.Context, adm *pgx.Conn, main0 MainNode) error {
	db := m.o.Target.Database
	staging, name := stagingName(db), liveName(db)
	src, err := connectSource(ctx, m.src)
	if err != nil {
		return err
	}
	defer src.Close(context.WithoutCancel(ctx))
	fail := func(err error) error {
		m.say("")
		m.say("Error: %v", cleanErr(err))
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_, _ = src.Exec(cctx, "DROP PUBLICATION IF EXISTS "+ident(name))
		dropSlot(cctx, src, name)
		if err := dropDatabase(cctx, adm, staging); err != nil {
			m.logf("remove %s: %v", staging, err)
		}
		m.say("Nothing was left running: the publication on the source and database %s were removed.", staging)
		return fmt.Errorf("%w: %w", ErrNotSwitched, err)
	}

	// Publication: the leaf tables (partitions are published one by one,
	// which works from PostgreSQL 12 on).
	tables := make([]string, 0, len(m.srcInfo.tables))
	for _, t := range m.srcInfo.tables {
		tables = append(tables, t.qualified())
	}
	if _, err := src.Exec(ctx, "DROP PUBLICATION IF EXISTS "+ident(name)); err != nil {
		return fail(fmt.Errorf("remove an old publication %s on the source: %w", name, err))
	}
	dropSlot(ctx, src, name) // left by an earlier run that could not reach the source
	if _, err := src.Exec(ctx, "CREATE PUBLICATION "+ident(name)+" FOR TABLE "+strings.Join(tables, ", ")); err != nil {
		return fail(fmt.Errorf("create the publication on the source: %w", err))
	}
	m.say("  published %d %s on the source (publication %s)", len(tables), plural(len(tables), "table", "tables"), name)

	if err := m.sameMain(ctx, main0); err != nil {
		return fail(err)
	}
	c, err := m.o.Target.Admin(ctx, staging)
	if err != nil {
		return fail(err)
	}
	if c == nil {
		return fail(errors.New("this computer stopped being the main node"))
	}
	defer c.Close(context.WithoutCancel(ctx))
	// The statement holds the source password: keep it out of the log.
	for _, q := range []string{"SET log_statement = 'none'", "SET log_min_error_statement = 'panic'"} {
		if _, err := c.Exec(ctx, q); err != nil {
			return fail(err)
		}
	}
	q := "CREATE SUBSCRIPTION " + ident(name) + " CONNECTION " + quoteLiteral(m.src.withPassword()) +
		" PUBLICATION " + ident(name) + " WITH (copy_data = true, synchronous_commit = 'on')"
	if _, err := c.Exec(ctx, q); err != nil {
		return fail(fmt.Errorf("the main node could not subscribe to the source (it must reach the source over the network itself): %w", err))
	}
	comment := fmt.Sprintf("%s from %s, started %s. Not in use until the cutover.", liveComment, m.src.describe(), time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	if _, err := adm.Exec(ctx, "COMMENT ON DATABASE "+ident(staging)+" IS "+quoteLiteral(comment)); err != nil {
		m.logf("comment: %v", err)
	}
	m.say("  the main node subscribed to it (subscription %s)", name)
	m.say("")
	m.say("Copying the rows (the source stays in use)…")
	if err := m.followInitial(ctx, c, name); err != nil {
		if ctx.Err() != nil {
			m.say("Stopped watching. The live copy keeps running; see `coyote migrate --status`.")
			return nil
		}
		return err
	}
	m.say("")
	m.say("The live copy is running: every change on the source reaches the cluster within seconds.")
	m.say("When you are ready to switch:")
	m.say("  1. stop your app (or anything else that writes to the old database);")
	m.say("  2. on the main node's computer run:  coyote migrate --cutover%s", dbFlag(db))
	m.say("  3. point your app at the connection string it prints, and start it.")
	m.say("Check on it any time with `coyote migrate --status%s`; stop it with `coyote migrate --cancel%s`.", dbFlag(db), dbFlag(db))
	return nil
}

func dbFlag(db string) string {
	if db == "app" {
		return ""
	}
	return " --database " + db
}

// dropSlot removes an inactive replication slot of this name on the source.
func dropSlot(ctx context.Context, src *pgx.Conn, name string) {
	_, _ = src.Exec(ctx, `SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = $1 AND NOT active`, name)
}

// liveState is the subscription as the main node sees it.
type liveState struct {
	conninfo     string
	enabled      bool
	slot         bool // still has its replication slot on the source
	tables       int
	ready        int // tables in state r (streaming)
	copying      int
	workerPID    *int32
	lastMsg      *time.Time
	applyErrors  int64
	syncErrors   int64
	slotActive   bool
	behindBytes  *int64
	sourceErr    error
	sourceParams connParams
}

func readLiveState(ctx context.Context, c *pgx.Conn, name string) (*liveState, error) {
	st := &liveState{}
	var oid uint32
	err := c.QueryRow(ctx, "SELECT oid, subenabled, subslotname IS NOT NULL, subconninfo FROM pg_subscription WHERE subname = $1", name).Scan(&oid, &st.enabled, &st.slot, &st.conninfo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the subscription: %w", cleanErr(err))
	}
	rows, err := c.Query(ctx, "SELECT srsubstate::text FROM pg_subscription_rel WHERE srsubid = $1", oid)
	if err != nil {
		return nil, fmt.Errorf("read the subscription's tables: %w", cleanErr(err))
	}
	states, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	st.tables = len(states)
	for _, s := range states {
		if s == "r" {
			st.ready++
		} else {
			st.copying++
		}
	}
	// The apply worker (not the table-copy or parallel workers).
	err = c.QueryRow(ctx, `SELECT pid, last_msg_receipt_time FROM pg_stat_subscription WHERE subid = $1 AND worker_type = 'apply'`, oid).Scan(&st.workerPID, &st.lastMsg)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read the subscription's worker: %w", cleanErr(err))
	}
	err = c.QueryRow(ctx, `SELECT apply_error_count, sync_error_count FROM pg_stat_subscription_stats WHERE subid = $1`, oid).Scan(&st.applyErrors, &st.syncErrors)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("read the subscription's errors: %w", cleanErr(err))
	}
	st.sourceParams, err = parseConnString(st.conninfo)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// sourceLag reads, on the source, how far the subscription's slot is
// behind the source's newest change.
func sourceLag(ctx context.Context, src *pgx.Conn, name string) (active bool, behind *int64, err error) {
	err = src.QueryRow(ctx, `SELECT active, (pg_current_wal_lsn() - confirmed_flush_lsn)::int8
	   FROM pg_replication_slots WHERE slot_name = $1`, name).Scan(&active, &behind)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, fmt.Errorf("the source has no replication slot %s (was it removed there?)", name)
	}
	return active, behind, err
}

// followInitial prints the first copy's progress until every table streams.
func (m *mover) followInitial(ctx context.Context, c *pgx.Conn, name string) error {
	last := -1
	lastSay := time.Now()
	for {
		st, err := readLiveState(ctx, c, name)
		if err != nil {
			return err
		}
		if st == nil {
			return errors.New("the subscription disappeared")
		}
		if st.tables > 0 && st.ready == st.tables {
			m.say("  all %d %s copied; now following changes.", st.tables, plural(st.tables, "table", "tables"))
			return nil
		}
		if st.ready != last || time.Since(lastSay) > 30*time.Second {
			msg := fmt.Sprintf("  %d of %d tables copied", st.ready, st.tables)
			if st.syncErrors > 0 {
				msg += fmt.Sprintf(" (%d failed attempts so far; they are retried: see the PostgreSQL log on the main node)", st.syncErrors)
			}
			m.say("%s", msg)
			last, lastSay = st.ready, time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// liveSession is what --status, --cutover and --cancel work with.
type liveSession struct {
	m       *mover
	main    MainNode
	adm     *pgx.Conn // app role, "postgres" database, through the client port
	su      *pgx.Conn // administrator, staging database, this machine (nil without one)
	name    string
	staging string
	state   *liveState // nil: no subscription
	obs     liveObs
	step    liveStep
}

func (ls *liveSession) close(ctx context.Context) {
	if ls.adm != nil {
		_ = ls.adm.Close(context.WithoutCancel(ctx))
	}
	if ls.su != nil {
		_ = ls.su.Close(context.WithoutCancel(ctx))
	}
}

// openLive looks at the live copy for o.Target.Database on this machine:
// the databases, the subscription, and what is left to do (ls.step).
func openLive(ctx context.Context, o Options, verb string) (*liveSession, error) {
	if o.Target.Database == "" {
		o.Target.Database = "app"
	}
	m := &mover{o: o}
	db := o.Target.Database
	ls := &liveSession{m: m, name: liveName(db), staging: stagingName(db)}
	main, err := o.Target.Main(ctx)
	if err != nil {
		return nil, fmt.Errorf("ask the cluster for its main node: %w", err)
	}
	ls.main = main
	notMain := func() (*liveSession, error) {
		m.say("This computer is not the main node; the main node is %s. Run `coyote migrate --%s%s` on %s.", main.Name, verb, dbFlag(db), main.Name)
		return nil, ErrNotMain
	}
	if o.Target.Admin == nil {
		return notMain()
	}
	// First make sure this is the main node's computer (a witness or a
	// copy cannot see or change the subscription).
	probe, err := o.Target.Admin(ctx, "postgres")
	if err != nil {
		return nil, err
	}
	if probe == nil {
		return notMain()
	}
	_ = probe.Close(ctx)
	adm, err := o.Target.connect(ctx, "postgres")
	if err != nil {
		return nil, err
	}
	ls.adm = adm
	if ls.obs, err = readObs(ctx, adm, db); err != nil {
		ls.close(ctx)
		return nil, err
	}
	if ls.obs.staging {
		su, err := o.Target.Admin(ctx, ls.staging)
		if err != nil || su == nil {
			ls.close(ctx)
			if err != nil {
				return nil, err
			}
			return notMain()
		}
		ls.su = su
		if ls.state, err = readLiveState(ctx, su, ls.name); err != nil {
			ls.close(ctx)
			return nil, err
		}
		if ls.state != nil {
			ls.obs.sub, ls.obs.subSlot = true, ls.state.slot
		}
	}
	ls.step = decideLive(ls.obs)
	m.logf("%s: found %s", verb, ls.step)
	return ls, nil
}

// sayNoLive explains a step that has no live copy to work with.
func (ls *liveSession) sayNoLive() {
	m, db := ls.m, ls.m.o.Target.Database
	switch ls.step {
	case stepDone:
		m.say("The move into database %s is done; no live copy is running.", db)
	case stepDetached:
		m.say("The live copy into database %s was being stopped by `coyote migrate --cancel` when that stopped partway. Run `coyote migrate --cancel%s` again to finish removing it.", db, dbFlag(db))
	case stepNotLive:
		m.say("Database %s exists but has no live copy running (an unfinished one-time copy, or a --cancel that stopped partway). Run the move again, or remove it with `coyote migrate --cancel%s`.", ls.staging, dbFlag(db))
	default:
		m.say("No live copy is running into database %s.", db)
	}
}

// connectLiveSource opens the source with the subscription's own
// connection string (it was given once, at the start).
func (ls *liveSession) connectSource(ctx context.Context) (*pgx.Conn, error) {
	return connectSource(ctx, ls.state.sourceParams)
}

// Status prints how the live copy is doing.
func Status(ctx context.Context, o Options) error {
	ls, err := openLive(ctx, o, "status")
	if err != nil {
		return err
	}
	defer ls.close(ctx)
	db := ls.m.o.Target.Database
	switch ls.step {
	case stepFull:
		ls.printStatus(ctx)
		return nil
	case stepResume:
		ls.m.say("A cutover into database %s stopped partway, after the copy was checked. Finish it with `coyote migrate --cutover%s`, or stop it with `coyote migrate --cancel%s`.", db, dbFlag(db), dbFlag(db))
		return nil
	case stepDetached, stepNotLive:
		ls.sayNoLive()
		return ErrNotSwitched
	default:
		ls.sayNoLive()
		return nil
	}
}

func (ls *liveSession) printStatus(ctx context.Context) {
	m, st := ls.m, ls.state
	m.say("Live copy into database %s, on main node %s:", ls.m.o.Target.Database, ls.main.Name)
	m.say("  Source:        %s", st.sourceParams.describe())
	if st.ready == st.tables {
		m.say("  Tables:        %d, all copied; following changes", st.tables)
	} else {
		m.say("  Tables:        %d of %d copied (the rest are being copied)", st.ready, st.tables)
	}
	switch {
	case !st.enabled:
		m.say("  Worker:        stopped (the subscription is disabled)")
	case st.workerPID == nil:
		m.say("  Worker:        not running right now (it starts again by itself within seconds; after a change of main node it can take up to a minute)")
	default:
		w := "running"
		if st.lastMsg != nil {
			w += fmt.Sprintf(", last heard from the source %s ago", time.Since(*st.lastMsg).Round(time.Second))
		}
		m.say("  Worker:        %s", w)
	}
	src, err := ls.connectSource(ctx)
	if err != nil {
		m.say("  Behind:        unknown (%v)", err)
	} else {
		defer src.Close(context.WithoutCancel(ctx))
		active, behind, err := sourceLag(ctx, src, ls.name)
		switch {
		case err != nil:
			m.say("  Behind:        unknown (%v)", cleanErr(err))
		case behind == nil:
			m.say("  Behind:        unknown")
		default:
			s := humanBytes(max(0, *behind)) + " of changes"
			if *behind <= 0 {
				s = "nothing: caught up"
			}
			if !active {
				s += " (the source sees no connection from the cluster right now)"
			}
			m.say("  Behind:        %s", s)
		}
	}
	if st.applyErrors > 0 || st.syncErrors > 0 {
		m.say("  Errors:        %d while applying changes, %d while copying tables (retried; details in the PostgreSQL log on %s)", st.applyErrors, st.syncErrors, ls.main.Name)
	}
	m.say("")
	m.say("To switch: stop your app's writes, then run `coyote migrate --cutover%s` here.", dbFlag(ls.m.o.Target.Database))
}

// Cancel stops a live copy: subscription, publication and the staging
// database go. The target database is not touched (if a cutover stopped
// between its two renames, the old database is put back).
func Cancel(ctx context.Context, o Options) error {
	ls, err := openLive(ctx, o, "cancel")
	if err != nil {
		return err
	}
	defer ls.close(ctx)
	m := ls.m
	db := m.o.Target.Database
	if !ls.obs.staging {
		if ls.step == stepDone {
			m.say("The move into database %s is already done; there is nothing to cancel.", db)
		} else {
			ls.sayNoLive()
		}
		return nil
	}
	if ls.state != nil {
		if err := ls.dropSubscription(ctx); err != nil {
			return err
		}
	}
	_ = ls.su.Close(ctx)
	ls.su = nil
	if err := dropDatabase(ctx, ls.adm, ls.staging); err != nil {
		return fmt.Errorf("remove database %s: %w", ls.staging, cleanErr(err))
	}
	if err := m.settleReplaced(ctx, ls.adm, ls.obs, false); err != nil {
		return err
	}
	m.say("The live copy was stopped and removed. Database %s on the cluster was not touched; the old database is as it was.", db)
	return nil
}

// dropSubscription removes the subscription (which removes its slot on the
// source) and the publication on the source. The publication goes before
// the subscription, while the subscription still holds the source's
// connection string, so a run stopped partway can always finish it.
func (ls *liveSession) dropSubscription(ctx context.Context) error {
	m := ls.m
	src, srcErr := ls.connectSource(ctx)
	if src != nil {
		defer src.Close(context.WithoutCancel(ctx))
	}
	if _, err := ls.su.Exec(ctx, "ALTER SUBSCRIPTION "+ident(ls.name)+" DISABLE"); err != nil {
		return fmt.Errorf("stop the subscription: %w", cleanErr(err))
	}
	if srcErr != nil {
		// The source cannot be reached: keep its slot, say how to drop it.
		if _, err := ls.su.Exec(ctx, "ALTER SUBSCRIPTION "+ident(ls.name)+" SET (slot_name = NONE)"); err != nil {
			return fmt.Errorf("detach the subscription: %w", cleanErr(err))
		}
		m.say("  Note: the source could not be reached (%v). On the source, remove what the live copy left there:", srcErr)
		m.say("    SELECT pg_drop_replication_slot('%s'); DROP PUBLICATION %s;", ls.name, ls.name)
	} else if _, err := src.Exec(ctx, "DROP PUBLICATION IF EXISTS "+ident(ls.name)); err != nil {
		m.say("  Note: could not remove publication %s on the source (%v); remove it there with DROP PUBLICATION %s;", ls.name, cleanErr(err), ls.name)
	}
	// The worker may need a moment to let go of the slot after DISABLE.
	var err error
	for range 30 {
		if _, err = ls.su.Exec(ctx, "DROP SUBSCRIPTION IF EXISTS "+ident(ls.name)); err == nil || sqlState(err) != objectInUse {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return fmt.Errorf("remove the subscription: %w", cleanErr(err))
	}
	if src != nil {
		dropSlot(ctx, src, ls.name)
	}
	return nil
}

// confirm asks what every cutover asks before it changes anything: may it
// replace a database that is not empty, and have the writers stopped.
func (ls *liveSession) confirm(ctx context.Context) error {
	m := ls.m
	o := m.o
	db := o.Target.Database
	ti, err := inspectTarget(ctx, o.Target, ls.adm)
	if err != nil {
		return err
	}
	m.tgtInfo = ti
	if ob := ls.obs; len(ti.contents) == 0 && ob.target && ob.replaced && !isCutoverNote(ob.targetNote) && !strings.HasPrefix(ob.targetNote, movedComment) {
		// The old contents wait under the other name to be put back
		// (settleReplaced): they are what the copy replaces.
		c, err := o.Target.connect(ctx, replacedName(db))
		if err != nil {
			return err
		}
		ti.contents, ti.more, err = userObjects(ctx, c)
		_ = c.Close(context.WithoutCancel(ctx))
		if err != nil {
			return fmt.Errorf("look inside database %s: %w", replacedName(db), err)
		}
	}
	if len(ti.contents) > 0 {
		if !o.Replace {
			m.say("Database %s on the cluster is not empty (%s). Run --cutover with --replace to replace its contents with the copy.", db, strings.Join(ti.contents, ", "))
			return ErrBlocked
		}
		q := fmt.Sprintf("This deletes everything in database %s on the cluster and puts the copy in its place.\nType %s to go on: ", db, db)
		if o.Confirm == nil || !o.Confirm(q, db) {
			m.say("Not confirmed; nothing was changed.")
			return ErrBlocked
		}
	}
	if o.Confirm == nil || !o.Confirm("Stop every program that writes to the old database first (your app, workers, cron jobs).\nHave they stopped? Type yes to go on: ", "yes") {
		m.say("Not confirmed; nothing was changed.")
		return ErrBlocked
	}
	return nil
}

// Cutover finishes a live copy: once the source has stopped changing and
// the copy has every change, it checks the copy, copies the sequences,
// records the check, removes the subscription and the publication, and
// switches the copy in. Run again after it was stopped at any point, it
// continues from where it was (see resume.go).
func Cutover(ctx context.Context, o Options) (*Result, error) {
	ls, err := openLive(ctx, o, "cutover")
	if err != nil {
		return nil, err
	}
	defer ls.close(ctx)
	m := ls.m
	o = m.o
	db := o.Target.Database
	m.adm = ls.adm
	switch ls.step {
	case stepFull:
	case stepResume:
		rec, _ := parseCheckRecord(ls.obs.stagingNote)
		return ls.resumeCutover(ctx, rec)
	case stepDone:
		return ls.finishDone(ctx)
	default:
		ls.sayNoLive()
		return nil, ErrNotSwitched
	}
	st := ls.state
	if st.ready < st.tables {
		m.say("The first copy is not finished yet (%d of %d tables). Run --cutover again once `coyote migrate --status%s` says all are copied.", st.ready, st.tables, dbFlag(db))
		return nil, ErrNotSwitched
	}
	if err := ls.confirm(ctx); err != nil {
		return nil, err
	}

	src, err := ls.connectSource(ctx)
	if err != nil {
		return nil, err
	}
	defer src.Close(context.WithoutCancel(ctx))
	var published bool
	if err := src.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = $1)", ls.name).Scan(&published); err != nil {
		return nil, fmt.Errorf("read the publication: %w", cleanErr(err))
	}
	if !published {
		m.say("The live copy's publication %s is gone from the source (removed there, or by a --cancel that stopped partway). The live copy cannot finish; remove it with `coyote migrate --cancel%s` and start the move again.", ls.name, dbFlag(db))
		return nil, ErrNotSwitched
	}
	if !st.enabled {
		// Stopped by an earlier run (a --cancel or --cutover that stopped
		// partway): start it again, it carries on from where it was.
		m.say("Starting the live copy again (an earlier run had stopped it)…")
		if _, err := ls.su.Exec(ctx, "ALTER SUBSCRIPTION "+ident(ls.name)+" ENABLE"); err != nil {
			return nil, fmt.Errorf("start the subscription: %w", cleanErr(err))
		}
	}
	m.say("")
	m.say("Waiting for the last changes to arrive…")
	var target string
	if err := src.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&target); err != nil {
		return nil, fmt.Errorf("read the source's position: %w", cleanErr(err))
	}
	if err := m.waitCaughtUp(ctx, src, ls.name, target); err != nil {
		return nil, err
	}

	// The source is still: compare it with the copy table by table.
	m.say("Checking the copy against the source…")
	s, err := inspectSource(ctx, src, ls.state.sourceParams)
	if err != nil {
		return nil, err
	}
	if err := s.inspectObjects(ctx, src, nil); err != nil {
		return nil, err
	}
	pubTables, err := textColumn(ctx, src, `SELECT schemaname || '.' || tablename FROM pg_publication_tables WHERE pubname = $1`, ls.name)
	if err != nil {
		return nil, fmt.Errorf("read the publication: %w", cleanErr(err))
	}
	var tables []table
	for _, t := range s.tables {
		if slices.Contains(pubTables, t.label()) {
			tables = append(tables, t)
		}
	}
	if len(tables) != len(pubTables) || len(tables) == 0 {
		// Every published table must be compared; never switch in unchecked.
		return nil, fmt.Errorf("could only find %d of the %d published tables on the source to compare; nothing was changed (was a table renamed or dropped during the live copy?)", len(tables), len(pubTables))
	}
	cp, err := o.Target.connect(ctx, ls.staging)
	if err != nil {
		return nil, err
	}
	defer cp.Close(context.WithoutCancel(ctx))
	for _, q := range stableOutput {
		if _, err := src.Exec(ctx, q); err != nil {
			return nil, err
		}
		if _, err := cp.Exec(ctx, q); err != nil {
			return nil, err
		}
	}
	var bad []string
	rec := checkRecord{}
	for _, t := range tables {
		a, err := readStats(ctx, src.QueryRow(ctx, statsQuery(t)), t)
		if err != nil {
			return nil, err
		}
		b, err := readStats(ctx, cp.QueryRow(ctx, statsQuery(t)), t)
		if err != nil {
			return nil, err
		}
		if d := mismatch(t, a, b); d != "" {
			bad = append(bad, d)
		}
		rec.Rows += b.Rows
		rec.Tables = append(rec.Tables, checkedTable{Table: t.label(), How: b.How, Rows: b.Rows, Sum: b.Sum})
	}
	if len(bad) > 0 {
		m.say("The copy does not match the source yet:")
		for _, b := range limit(bad, 20) {
			m.say("  - %s", b)
		}
		m.say("The old database is probably still being written to. Stop everything that writes to it and run --cutover again. Nothing was changed; the live copy keeps running.")
		return nil, ErrNotSwitched
	}
	m.say("  %d %s the source; %s rows.", len(tables), plural(len(tables), "table matches", "tables match"), humanCount(float64(rec.Rows)))

	// Sequences are not carried by logical replication: copy their values.
	n, err := copySequences(ctx, src, cp)
	if err != nil {
		return nil, err
	}
	fixes, _, err := fixSequences(ctx, cp)
	if err != nil {
		return nil, err
	}
	m.say("  %d %s copied from the source; %d moved past the highest id in use.", n, plural(n, "sequence", "sequences"), len(fixes))
	rec.Fixed = len(fixes)

	// Record the check before anything is removed: from here on a rerun
	// continues from this record (the subscription may be gone by then).
	if rec.Sequences, err = readSequences(ctx, cp); err != nil {
		return nil, err
	}
	note, err := rec.comment()
	if err != nil {
		return nil, err
	}
	if err := setComment(ctx, ls.adm, ls.staging, note); err != nil {
		return nil, fmt.Errorf("record the check on the cluster: %w", cleanErr(err))
	}

	if err := m.sameMain(ctx, ls.main); err != nil {
		return nil, err
	}
	m.say("Removing the subscription and the publication…")
	if err := ls.dropSubscription(ctx); err != nil {
		return nil, err
	}
	return ls.finishSwitch(ctx, cp, &rec)
}

// waitCaughtUp waits until the subscription has confirmed every change up
// to target (an LSN on the source).
func (m *mover) waitCaughtUp(ctx context.Context, src *pgx.Conn, name, target string) error {
	deadline := time.Now().Add(10 * time.Minute)
	lastSay := time.Now()
	for {
		var done bool
		var behind *int64
		var active bool
		err := src.QueryRow(ctx, `SELECT confirmed_flush_lsn >= $2::pg_lsn, ($2::pg_lsn - confirmed_flush_lsn)::int8, active
		   FROM pg_replication_slots WHERE slot_name = $1`, name, target).Scan(&done, &behind, &active)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("the source has no replication slot %s any more", name)
		}
		if err != nil {
			return fmt.Errorf("read the subscription's progress on the source: %w", cleanErr(err))
		}
		if done {
			m.say("  every change has arrived.")
			return nil
		}
		if time.Since(lastSay) > 10*time.Second {
			s := "  still waiting"
			if behind != nil {
				s += fmt.Sprintf(" (%s behind)", humanBytes(max(0, *behind)))
			}
			if !active {
				s += "; the cluster is not connected to the source right now"
			}
			m.say("%s…", s)
			lastSay = time.Now()
		}
		if time.Now().After(deadline) {
			return errors.New("the copy did not catch up within 10 minutes; see `coyote migrate --status`. Nothing was changed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// copySequences sets every sequence of the copy to the source's value.
func copySequences(ctx context.Context, src, cp *pgx.Conn) (int, error) {
	names, err := textColumn(ctx, cp, `SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname)
	   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	  WHERE c.relkind = 'S' AND `+userSchemaFilter+`
	    AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')`)
	if err != nil {
		return 0, fmt.Errorf("list the copy's sequences: %w", cleanErr(err))
	}
	n := 0
	for _, s := range names {
		var last int64
		var called bool
		err := src.QueryRow(ctx, "SELECT last_value, is_called FROM "+s).Scan(&last, &called)
		if sqlState(err) == "42P01" {
			continue // not on the source (made on the cluster only)
		}
		if err != nil {
			return n, fmt.Errorf("read sequence %s on the source: %w", s, cleanErr(err))
		}
		if _, err := cp.Exec(ctx, "SELECT setval($1::regclass, $2, $3)", s, last, called); err != nil {
			return n, fmt.Errorf("set sequence %s: %w", s, cleanErr(err))
		}
		n++
	}
	return n, nil
}
