package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A cutover can be stopped at any point (a closed terminal, a lost SSH
// session, kill -9). Once the copy is checked, the cutover records the
// result of that check in the staging database's comment, before it
// removes anything. The comment lives in the cluster, so it survives the
// terminal, a restart and a change of main node, and it cannot be taken
// for a one-time copy's comment or a running live copy's. A rerun that
// finds it continues from there: it stops the subscription if it is still
// there, checks that the copy is still exactly what was checked (same
// tables, same row counts and checksums, same sequence values), and only
// then removes the subscription and switches the copy in. A copy that
// changed is never switched in.

// cutoverComment starts the staging database's comment once the copy was
// checked. It must not start with liveComment.
const cutoverComment = "coyote migrate: cutover under way"

// movedComment starts the comment of a database a move switched in.
const movedComment = "Moved in with coyote migrate on "

// checkRecord is what the cutover's check saw in the copy.
type checkRecord struct {
	V         int            `json:"v"`
	Tables    []checkedTable `json:"tables"`
	Rows      int64          `json:"rows"`
	Fixed     int            `json:"fixed"` // sequences moved past the highest id
	Sequences []seqValue     `json:"seqs"`
}

type checkedTable struct {
	Table string `json:"t"` // schema.name
	How   string `json:"how"`
	Rows  int64  `json:"rows"`
	Sum   string `json:"sum"`
}

type seqValue struct {
	Name   string `json:"s"`
	Last   int64  `json:"last"`
	Called bool   `json:"called"`
}

const checkRecordVersion = 1

// comment is the staging database's comment that carries r.
func (r checkRecord) comment() (string, error) {
	r.V = checkRecordVersion
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return cutoverComment + ". The copy was checked; finish with coyote migrate --cutover, or stop with coyote migrate --cancel. Do not drop. " + string(b), nil
}

func isCutoverNote(note string) bool { return strings.HasPrefix(note, cutoverComment) }

// parseCheckRecord reads the record from a cutover comment. ok is false
// when note is not a cutover comment or its record cannot be trusted.
func parseCheckRecord(note string) (*checkRecord, bool) {
	if !isCutoverNote(note) {
		return nil, false
	}
	i := strings.IndexByte(note, '{')
	if i < 0 {
		return nil, false
	}
	var r checkRecord
	if err := json.Unmarshal([]byte(note[i:]), &r); err != nil || r.V != checkRecordVersion || len(r.Tables) == 0 {
		return nil, false
	}
	return &r, true
}

// liveObs is what a --status, --cutover or --cancel finds on the cluster.
type liveObs struct {
	staging     bool   // <db>__migrating exists
	stagingNote string // its comment
	sub         bool   // the subscription exists
	subSlot     bool   // it still has its slot on the source
	target      bool   // <db> exists
	targetNote  string // its comment
	replaced    bool   // <db>__replaced exists
}

// liveStep is what --cutover does with what it found.
type liveStep int

const (
	stepNoLive   liveStep = iota // no live copy and no move to finish
	stepDone                     // the copy was switched in already; only tidying up may be left
	stepFull                     // a live copy runs: the whole cutover
	stepResume                   // the copy was checked: continue from the record
	stepDetached                 // a subscription without its slot and no record: a stopped --cancel
	stepNotLive                  // a staging database with no live copy: a one-time copy, or a stopped --cancel
)

func (s liveStep) String() string {
	return [...]string{"no live copy", "done", "full cutover", "resume", "detached", "not live"}[s]
}

func decideLive(o liveObs) liveStep {
	switch {
	case !o.staging:
		if o.target && (isCutoverNote(o.targetNote) || strings.HasPrefix(o.targetNote, movedComment)) {
			return stepDone
		}
		return stepNoLive
	}
	if _, ok := parseCheckRecord(o.stagingNote); ok {
		return stepResume
	}
	switch {
	case o.sub && o.subSlot:
		return stepFull
	case o.sub:
		return stepDetached
	default:
		return stepNotLive
	}
}

// readObs reads the databases' side of liveObs (the subscription is read
// by readLiveState).
func readObs(ctx context.Context, c *pgx.Conn, db string) (liveObs, error) {
	var o liveObs
	rows, err := c.Query(ctx, `SELECT datname::text, coalesce(shobj_description(oid, 'pg_database'), '')
	   FROM pg_database WHERE datname IN ($1, $2, $3)`, db, stagingName(db), replacedName(db))
	if err != nil {
		return o, fmt.Errorf("list the cluster's databases: %w", cleanErr(err))
	}
	var name, note string
	_, err = pgx.ForEachRow(rows, []any{&name, &note}, func() error {
		switch name {
		case db:
			o.target, o.targetNote = true, note
		case stagingName(db):
			o.staging, o.stagingNote = true, note
		case replacedName(db):
			o.replaced = true
		}
		return nil
	})
	if err != nil {
		return o, fmt.Errorf("list the cluster's databases: %w", cleanErr(err))
	}
	return o, nil
}

// seqListQuery lists the user sequences of a database (not an extension's).
const seqListQuery = `SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname)
	   FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	  WHERE c.relkind = 'S' AND ` + userSchemaFilter + `
	    AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
	  ORDER BY 1`

func readSequences(ctx context.Context, c *pgx.Conn) ([]seqValue, error) {
	names, err := textColumn(ctx, c, seqListQuery)
	if err != nil {
		return nil, fmt.Errorf("list the copy's sequences: %w", cleanErr(err))
	}
	out := make([]seqValue, 0, len(names))
	for _, s := range names {
		v := seqValue{Name: s}
		if err := c.QueryRow(ctx, "SELECT last_value, is_called FROM "+s).Scan(&v.Last, &v.Called); err != nil {
			return nil, fmt.Errorf("read sequence %s: %w", s, cleanErr(err))
		}
		out = append(out, v)
	}
	return out, nil
}

// changedSince compares the copy now (tables as found in it, sequences)
// with the record of the check; it returns the differences in plain words.
func changedSince(rec *checkRecord, tables map[string]table, stats func(table) (tableStats, error), seqs []seqValue) ([]string, error) {
	var diff []string
	for _, ct := range rec.Tables {
		t, ok := tables[ct.Table]
		if !ok {
			diff = append(diff, fmt.Sprintf("%s: no longer in the copy", ct.Table))
			continue
		}
		// Check it exactly as it was checked then.
		if ct.How == "every row" {
			t.Size = 0
		} else {
			t.Size = fullChecksumBytes + 1
		}
		if checkMode(t) != ct.How {
			diff = append(diff, fmt.Sprintf("%s: its primary key changed", ct.Table))
			continue
		}
		st, err := stats(t)
		if err != nil {
			return nil, err
		}
		switch {
		case st.Rows != ct.Rows:
			diff = append(diff, fmt.Sprintf("%s: %d rows when checked, %d now", ct.Table, ct.Rows, st.Rows))
		case st.Sum != ct.Sum:
			diff = append(diff, fmt.Sprintf("%s: same number of rows (%d) but different contents", ct.Table, st.Rows))
		}
	}
	was := map[string]seqValue{}
	for _, s := range rec.Sequences {
		was[s.Name] = s
	}
	for _, s := range seqs {
		w, ok := was[s.Name]
		switch {
		case !ok:
			diff = append(diff, fmt.Sprintf("sequence %s: new since the check", s.Name))
		case w != s:
			diff = append(diff, fmt.Sprintf("sequence %s: at %d when checked, %d now", s.Name, w.Last, s.Last))
		}
		delete(was, s.Name)
	}
	gone := make([]string, 0, len(was))
	for n := range was {
		gone = append(gone, n)
	}
	slices.Sort(gone)
	for _, n := range gone {
		diff = append(diff, fmt.Sprintf("sequence %s: no longer in the copy", n))
	}
	return diff, nil
}

// recheck compares the staging database with rec.
func recheck(ctx context.Context, cp *pgx.Conn, rec *checkRecord) ([]string, error) {
	for _, q := range stableOutput {
		if _, err := cp.Exec(ctx, q); err != nil {
			return nil, err
		}
	}
	s := &source{}
	if err := s.inspectObjects(ctx, cp, nil); err != nil {
		return nil, fmt.Errorf("read the copy's tables: %w", err)
	}
	tables := make(map[string]table, len(s.tables))
	for _, t := range s.tables {
		tables[t.label()] = t
	}
	seqs, err := readSequences(ctx, cp)
	if err != nil {
		return nil, err
	}
	return changedSince(rec, tables, func(t table) (tableStats, error) {
		return readStats(ctx, cp.QueryRow(ctx, statsQuery(t)), t)
	}, seqs)
}

// stopWorker disables the subscription and waits until its worker has
// stopped, so nothing more is applied to the copy.
func (ls *liveSession) stopWorker(ctx context.Context) error {
	if _, err := ls.su.Exec(ctx, "ALTER SUBSCRIPTION "+ident(ls.name)+" DISABLE"); err != nil {
		return fmt.Errorf("stop the subscription: %w", cleanErr(err))
	}
	deadline := time.Now().Add(time.Minute)
	for {
		var n int
		err := ls.su.QueryRow(ctx, `SELECT count(*) FROM pg_stat_subscription s JOIN pg_subscription p ON p.oid = s.subid
		   WHERE p.subname = $1 AND s.pid IS NOT NULL`, ls.name).Scan(&n)
		if err != nil {
			return fmt.Errorf("read the subscription's worker: %w", cleanErr(err))
		}
		if n == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the subscription's worker did not stop within a minute; nothing was switched in. Run --cutover again")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// setComment sets database db's comment.
func setComment(ctx context.Context, c *pgx.Conn, db, comment string) error {
	_, err := c.Exec(ctx, "COMMENT ON DATABASE "+ident(db)+" IS "+quoteLiteral(comment))
	return err
}

// settleReplaced deals with a <db>__replaced left by a switch that
// stopped partway, before a cutover switches in or after a --cancel:
//   - no <db>: the switch stopped between its two renames; the old
//     database is put back under its name;
//   - <db> is a moved-in copy: an earlier move finished but its old
//     contents were not deleted; they are deleted when dropLeftover;
//   - <db> is an empty database the cluster made while the name was free
//     (it makes its app database when it starts as main): it is removed
//     and the old database put back;
//   - anything else: two databases, neither known; nothing is touched.
func (m *mover) settleReplaced(ctx context.Context, c *pgx.Conn, o liveObs, dropLeftover bool) error {
	db := m.o.Target.Database
	old := replacedName(db)
	if !o.replaced {
		return nil
	}
	if o.target {
		if isCutoverNote(o.targetNote) || strings.HasPrefix(o.targetNote, movedComment) {
			if !dropLeftover {
				return nil
			}
			if err := dropDatabase(ctx, c, old); err != nil {
				return fmt.Errorf("remove %s left by an earlier move: %w", old, cleanErr(err))
			}
			return nil
		}
		empty, err := m.isEmpty(ctx, db)
		if err != nil {
			return err
		}
		if !empty {
			m.say("Databases %s and %s both exist on the cluster, and neither is the copy. Nothing was switched in. Keep the one that holds your data under the name %s, remove the other (DROP DATABASE), then run the command again.", db, old, db)
			return ErrNotSwitched
		}
		m.say("  removing the empty database %s the cluster made while the name was free", db)
		if err := dropDatabase(ctx, c, db); err != nil {
			return fmt.Errorf("remove the empty database %s: %w", db, cleanErr(err))
		}
	}
	m.say("  putting back database %s (an earlier run stopped while switching)", db)
	if err := renameDatabase(ctx, c, old, db); err != nil {
		return fmt.Errorf("put back database %s: %w", db, cleanErr(err))
	}
	return nil
}

// isEmpty reports whether database db holds nothing beyond a fresh one.
func (m *mover) isEmpty(ctx context.Context, db string) (bool, error) {
	c, err := m.o.Target.connect(ctx, db)
	if err != nil {
		return false, err
	}
	defer c.Close(context.WithoutCancel(ctx))
	objs, more, err := userObjects(ctx, c)
	if err != nil {
		return false, fmt.Errorf("look inside database %s: %w", db, err)
	}
	return len(objs) == 0 && more == 0, nil
}

// resumeCutover continues a cutover that stopped after its check.
func (ls *liveSession) resumeCutover(ctx context.Context, rec *checkRecord) (*Result, error) {
	m := ls.m
	db := m.o.Target.Database
	if err := ls.confirm(ctx); err != nil {
		return nil, err
	}
	m.say("")
	m.say("Finishing the cutover that was stopped partway (the copy was already checked).")
	if ls.state != nil {
		if err := ls.stopWorker(ctx); err != nil {
			return nil, err
		}
	}
	m.say("Checking that the copy has not changed since…")
	cp, err := m.o.Target.connect(ctx, ls.staging)
	if err != nil {
		return nil, err
	}
	defer cp.Close(context.WithoutCancel(ctx))
	diff, err := recheck(ctx, cp, rec)
	if err != nil {
		return nil, err
	}
	if len(diff) > 0 {
		m.say("The copy changed after it was checked:")
		for _, d := range limit(diff, 20) {
			m.say("  - %s", d)
		}
		if ls.state != nil && ls.state.slot && ls.publicationLeft(ctx) {
			comment := fmt.Sprintf("%s from %s. Not in use until the cutover.", liveComment, ls.state.sourceParams.describe())
			if err := setComment(ctx, ls.adm, ls.staging, comment); err != nil {
				return nil, fmt.Errorf("mark the live copy as running again: %w", cleanErr(err))
			}
			if _, err := ls.su.Exec(ctx, "ALTER SUBSCRIPTION "+ident(ls.name)+" ENABLE"); err != nil {
				return nil, fmt.Errorf("start the live copy again: %w", cleanErr(err))
			}
			m.say("Nothing was switched in; the live copy runs again. Is something writing to the old database? Stop it, then run --cutover again.")
			return nil, ErrNotSwitched
		}
		m.say("Nothing was switched in. Start over: run `coyote migrate --cancel%s`, then the move again. The old database is as it was.", dbFlag(db))
		return nil, ErrNotSwitched
	}
	m.say("  unchanged: %d %s, %s rows.", len(rec.Tables), plural(len(rec.Tables), "table", "tables"), humanCount(float64(rec.Rows)))
	if err := m.sameMain(ctx, ls.main); err != nil {
		return nil, err
	}
	if ls.state != nil {
		m.say("Removing the subscription and the publication…")
		if err := ls.dropSubscription(ctx); err != nil {
			return nil, err
		}
	}
	return ls.finishSwitch(ctx, cp, rec)
}

// finishSwitch is the cutover's end, once the subscription is gone:
// ANALYZE, then the copy takes the database's name.
func (ls *liveSession) finishSwitch(ctx context.Context, cp *pgx.Conn, rec *checkRecord) (*Result, error) {
	m := ls.m
	db := m.o.Target.Database
	if ls.su != nil {
		_ = ls.su.Close(ctx)
		ls.su = nil
	}
	if _, err := cp.Exec(ctx, "ANALYZE"); err != nil {
		return nil, fmt.Errorf("update the copy's statistics: %w", cleanErr(err))
	}
	_ = cp.Close(ctx)
	if err := m.sameMain(ctx, ls.main); err != nil {
		return nil, err
	}
	obs, err := readObs(ctx, ls.adm, db)
	if err != nil {
		return nil, err
	}
	if err := m.settleReplaced(ctx, ls.adm, obs, true); err != nil {
		return nil, err
	}
	if err := m.switchIn(ctx, ls.adm); err != nil {
		return nil, err
	}
	m.say("")
	m.say("Done. Database %s on the cluster now holds the copy. Point your app at the connection string below and start it.", db)
	return &Result{Database: db, URL: m.o.Target.AppURLFor(db), Tables: len(rec.Tables), Rows: rec.Rows, Sequences: rec.Fixed}, nil
}

// finishDone tidies up after a cutover that had already switched the copy
// in, and says so.
func (ls *liveSession) finishDone(ctx context.Context) (*Result, error) {
	m := ls.m
	db := m.o.Target.Database
	res := &Result{Database: db, URL: m.o.Target.AppURLFor(db)}
	if rec, ok := parseCheckRecord(ls.obs.targetNote); ok {
		res.Tables, res.Rows, res.Sequences = len(rec.Tables), rec.Rows, rec.Fixed
	}
	if isCutoverNote(ls.obs.targetNote) {
		if err := setComment(ctx, ls.adm, db, movedComment+time.Now().UTC().Format("2006-01-02")+"."); err != nil {
			m.logf("comment on database: %v", err)
		}
	}
	if ls.obs.replaced {
		if err := dropDatabase(ctx, ls.adm, replacedName(db)); err != nil {
			m.say("Note: the old contents are still in database %s (it could not be deleted: %v). Delete it later with: DROP DATABASE %s;", replacedName(db), cleanErr(err), ident(replacedName(db)))
		}
	}
	m.say("The move is done: database %s on the cluster holds the copy. There is no live copy left to finish.", db)
	return res, nil
}

// publicationLeft reports whether the live copy's publication is still on
// the source (false when the source cannot be reached).
func (ls *liveSession) publicationLeft(ctx context.Context) bool {
	src, err := ls.connectSource(ctx)
	if err != nil {
		return false
	}
	defer src.Close(context.WithoutCancel(ctx))
	var ok bool
	if err := src.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = $1)", ls.name).Scan(&ok); err != nil {
		return false
	}
	return ok
}
