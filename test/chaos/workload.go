//go:build chaos && unix

package chaos

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"coyotedb.com/coyote/test/chaos/check"
)

// Outcome of one client operation, as the client saw it (see check).
type Outcome = check.Outcome

const (
	OK      = check.OK
	Failed  = check.Failed
	Unknown = check.Unknown
)

// classify: only a server ERROR on an autocommit statement (or a
// transaction the server rolled back) means "did not happen".
func classify(err error) Outcome {
	if err == nil {
		return OK
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Severity == "ERROR" {
		return Failed
	}
	return Unknown
}

type AppendOp = check.Append

type RegOp struct {
	Client     int
	Key        int
	Write      bool
	Value      int64 // written, or read (ok reads)
	Start, End time.Time
	Outcome    Outcome
	Node       string
}

// History is everything the clients saw.
type History struct {
	mu      sync.Mutex
	Appends []AppendOp
	Regs    []RegOp
	BankOK  int
	// Probes: what each node's commit probe proved, on the harness'
	// monotonic clock as offsets from Base (see probe).
	Base   time.Time
	Probes []check.Probe
	// Primaries: main nodes as the harness saw them (proxy runs).
	Primaries []PrimaryChange
}

func (h *History) add(f func(h *History)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

const (
	regKeys   = 5
	accounts  = 10
	initialBk = 1000
)

// Workload runs the clients against the cluster until Stop.
type Workload struct {
	c      *Cluster
	H      *History
	stop   chan struct{}
	wg     sync.WaitGroup
	nextID atomic.Int64
	nextV  atomic.Int64
	seed   uint64
	// gate: every client operation holds it shared; Pause takes it
	// exclusively, so while paused no operation is in flight.
	gate sync.RWMutex
}

// Pause waits for the operations in flight to finish (each is bounded by
// opTimeout) and holds new ones back until Resume. While paused the
// database does not change, so nodes can be compared.
func (w *Workload) Pause() {
	w.gate.Lock()
	w.c.Log.Event("workload paused")
}

// Resume lets the clients go on after Pause.
func (w *Workload) Resume() {
	w.c.Log.Event("workload resumed")
	w.gate.Unlock()
}

// StartWorkload creates the tables on the primary and starts the clients.
func StartWorkload(c *Cluster, seed uint64) *Workload {
	w := &Workload{c: c, H: &History{Base: time.Now()}, stop: make(chan struct{}), seed: seed}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var conn *pgx.Conn
	var err error
	if c.ViaProxy {
		conn, err = c.appSchema(ctx)
		if p := c.PrimaryByCluster(); p != "" {
			w.H.Primaries = append(w.H.Primaries, PrimaryChange{At: time.Now(), Node: p})
		}
	} else {
		conn, _, err = c.FindPrimary(ctx, "")
	}
	if err != nil {
		c.Fail("workload: %v", err)
	}
	defer conn.Close(context.Background())
	for _, q := range []string{
		`CREATE TABLE chaos_append (id bigint PRIMARY KEY, tl text NOT NULL)`,
		`CREATE TABLE chaos_reg (k int PRIMARY KEY, v bigint NOT NULL)`,
		`INSERT INTO chaos_reg SELECT g, 0 FROM generate_series(0, ` + strconv.Itoa(regKeys-1) + `) g`,
		`CREATE TABLE chaos_bank (id int PRIMARY KEY, bal bigint NOT NULL CHECK (bal >= 0))`,
		`INSERT INTO chaos_bank SELECT g, ` + strconv.Itoa(initialBk) + ` FROM generate_series(1, ` + strconv.Itoa(accounts) + `) g`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			c.Fail("workload schema: %v", err)
		}
	}
	for i := range 3 {
		w.wg.Add(1)
		go w.appender(i)
	}
	for i := range 4 {
		w.wg.Add(1)
		go w.register(i)
	}
	for i := range 2 {
		w.wg.Add(1)
		go w.bank(i)
	}
	for _, n := range c.Data() {
		w.wg.Add(1)
		go w.probe(n)
	}
	c.Log.Event("workload started")
	return w
}

// Stop stops the clients and waits for them.
func (w *Workload) Stop() {
	close(w.stop)
	w.wg.Wait()
	w.c.Log.Event("workload stopped")
}

func (w *Workload) stopped() bool {
	select {
	case <-w.stop:
		return true
	default:
		return false
	}
}

// client holds one connection to whichever node it found read-write.
type client struct {
	w    *Workload
	conn *pgx.Conn
	node *Node
	rng  *rand.Rand
	idx  int // spreads application clients over the client ports
}

func (w *Workload) newClient(kind, i int) *client {
	return &client{w: w, idx: kind*10 + i, rng: rand.New(rand.NewPCG(w.seed, uint64(kind*100+i)))}
}

func (cl *client) get() bool {
	if cl.conn != nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	find := cl.w.c.FindPrimary
	if cl.w.c.ViaProxy {
		find = func(ctx context.Context, _ string) (*pgx.Conn, *Node, error) { return cl.w.c.AppConnect(ctx, cl.idx) }
	}
	conn, n, err := find(ctx, "")
	if err != nil {
		time.Sleep(100 * time.Millisecond) // nobody is read-write right now; try again soon
		return false
	}
	cl.conn, cl.node = conn, n
	return true
}

func (cl *client) drop() {
	if cl.conn != nil {
		cl.conn.Close(context.Background())
	}
	cl.conn, cl.node = nil, nil
}

// opTimeout bounds one operation; when it passes the connection is closed
// (outcome unknown). It is longer than a failover.
const opTimeout = 20 * time.Second

func (w *Workload) appender(i int) {
	defer w.wg.Done()
	cl := w.newClient(1, i)
	defer cl.drop()
	for !w.stopped() {
		if !cl.get() {
			continue
		}
		w.gate.RLock()
		id := w.nextID.Add(1)
		op := AppendOp{ID: id, Node: cl.node.Name, Start: time.Now()}
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		var tl string
		err := cl.conn.QueryRow(ctx, `INSERT INTO chaos_append (id, tl)
			VALUES ($1, substr(pg_walfile_name(pg_current_wal_insert_lsn()), 1, 8)) RETURNING tl`, id).Scan(&tl)
		cancel()
		op.End, op.Outcome, op.TL = time.Now(), classify(err), tl
		w.H.add(func(h *History) { h.Appends = append(h.Appends, op) })
		w.gate.RUnlock()
		if err != nil {
			cl.drop()
		}
		time.Sleep(time.Duration(10+cl.rng.IntN(20)) * time.Millisecond) // pacing
	}
}

// register: writes are UPDATE ... SET v = new; reads are UPDATE ... SET
// v = v RETURNING v, so a read is acknowledged only once it is on a copy
// and is linearizable like a write (a plain SELECT on a deposed primary
// can return stale data: a known limitation, see the report).
func (w *Workload) register(i int) {
	defer w.wg.Done()
	cl := w.newClient(2, i)
	defer cl.drop()
	for !w.stopped() {
		if !cl.get() {
			continue
		}
		w.gate.RLock()
		op := RegOp{Client: i, Key: cl.rng.IntN(regKeys), Write: cl.rng.IntN(2) == 0, Start: time.Now(), Node: cl.node.Name}
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		var err error
		if op.Write {
			op.Value = w.nextV.Add(1)
			var tag pgconn.CommandTag
			tag, err = cl.conn.Exec(ctx, `UPDATE chaos_reg SET v = $2 WHERE k = $1`, op.Key, op.Value)
			if err == nil && tag.RowsAffected() != 1 {
				err = fmt.Errorf("register %d missing", op.Key)
			}
		} else {
			err = cl.conn.QueryRow(ctx, `UPDATE chaos_reg SET v = v WHERE k = $1 RETURNING v`, op.Key).Scan(&op.Value)
		}
		cancel()
		op.End, op.Outcome = time.Now(), classify(err)
		w.H.add(func(h *History) { h.Regs = append(h.Regs, op) })
		w.gate.RUnlock()
		if err != nil {
			cl.drop()
		}
		time.Sleep(time.Duration(5+cl.rng.IntN(20)) * time.Millisecond) // pacing
	}
}

func (w *Workload) bank(i int) {
	defer w.wg.Done()
	cl := w.newClient(3, i)
	defer cl.drop()
	for !w.stopped() {
		if !cl.get() {
			continue
		}
		a, b := 1+cl.rng.IntN(accounts), 1+cl.rng.IntN(accounts)
		if a == b {
			continue
		}
		amt := 1 + cl.rng.IntN(20)
		w.gate.RLock()
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		err := pgx.BeginFunc(ctx, cl.conn, func(tx pgx.Tx) error {
			var bal int64
			if err := tx.QueryRow(ctx, `SELECT bal FROM chaos_bank WHERE id = $1 FOR UPDATE`, a).Scan(&bal); err != nil {
				return err
			}
			if bal < int64(amt) {
				return nil
			}
			if _, err := tx.Exec(ctx, `UPDATE chaos_bank SET bal = bal - $2 WHERE id = $1`, a, amt); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE chaos_bank SET bal = bal + $2 WHERE id = $1`, b, amt)
			return err
		})
		cancel()
		if err == nil {
			w.H.add(func(h *History) { h.BankOK++ })
		}
		w.gate.RUnlock()
		if err != nil {
			cl.drop()
		}
		time.Sleep(time.Duration(10+cl.rng.IntN(20)) * time.Millisecond) // pacing
	}
}

// probeEvery is the pause between two probes of one node.
const probeEvery = 20 * time.Millisecond

// probeTimeout bounds one probe; past it the probe is "no answer" and the
// connection is opened again.
const probeTimeout = 5 * time.Second

// probe is the per-node evidence for the two-mains check (check.TwoMains).
// It keeps one connection to the node's own Postgres and, every
// probeEvery, commits a transaction that holds a transaction id:
//
//	SELECT txid_current(), pg_postmaster_start_time(), pg_backend_pid()
//
// with synchronous_commit = local for this session only, so the commit
// never waits for a copy: a node that is read-write but cut off from its
// copies still proves it here (the workload's own writes would just hang).
// On a standby txid_current() fails with 25006. Each probe records the
// harness-monotonic instants it was sent and answered; the effect happened
// in between. A frozen or slow node simply gives "no answer" for that
// stretch, which the check treats as unknown, never as "not main".
func (w *Workload) probe(n *Node) {
	defer w.wg.Done()
	var conn *pgx.Conn
	defer func() {
		if conn != nil {
			conn.Close(context.Background())
		}
	}()
	now := func() time.Duration { return time.Since(w.H.Base) }
	rec := func(p check.Probe) {
		p.Node = n.Name
		w.H.add(func(h *History) { h.Probes = append(h.Probes, p) })
	}
	for !w.stopped() {
		if conn == nil {
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			p := check.Probe{Start: now()}
			c, err := w.c.Connect(ctx, n)
			if err == nil {
				_, err = c.Exec(ctx, "SET synchronous_commit = local")
				if err != nil {
					c.Close(context.Background())
				}
			}
			cancel()
			p.End = now()
			if err != nil {
				p.Kind, p.Detail = probeKind(err), err.Error()
				rec(p)
				time.Sleep(probeEvery) // pacing between probes
				continue
			}
			conn = c
		}
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		p := check.Probe{Start: now()}
		var xid, started string
		var pid int
		err := conn.QueryRow(ctx, `SELECT txid_current()::text, pg_postmaster_start_time()::text, pg_backend_pid()`).Scan(&xid, &started, &pid)
		p.End = now()
		cancel()
		switch {
		case err == nil:
			p.Kind, p.Conn, p.Detail = check.RW, fmt.Sprintf("%s/%d", started, pid), "xid "+xid
		default:
			p.Kind, p.Detail = probeKind(err), err.Error()
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Severity != "ERROR" {
				conn.Close(context.Background())
				conn = nil
			}
		}
		rec(p)
		time.Sleep(probeEvery) // pacing between probes
	}
}

// probeKind: what a failed probe or connection attempt proves.
func probeKind(err error) check.Kind {
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return check.Down
	case errors.As(err, &pe) && pe.Code == "25006":
		return check.NotRW // cannot execute txid_current() during recovery
	case errors.As(err, &pe) && pe.Code == "57P03" && !strings.Contains(pe.Message, "shutting down"):
		return check.NotRW // starting up / not yet accepting / in recovery mode
	}
	return check.NoAnswer
}

// RunLog is the timeline of one run (kept in the artifacts directory).
type RunLog struct {
	mu    sync.Mutex
	f     *os.File
	start time.Time
	marks map[string]time.Time
}

func NewRunLog(dir string) *RunLog {
	f, err := os.Create(filepath.Join(dir, "events.log"))
	if err != nil {
		panic(err)
	}
	return &RunLog{f: f, start: time.Now(), marks: map[string]time.Time{}}
}

func (l *RunLog) Event(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.f, "%s +%7.3fs  %s\n", time.Now().Format("15:04:05.000"), time.Since(l.start).Seconds(), fmt.Sprintf(format, args...))
}

// Mark remembers a named instant (fault times for the resume budget).
func (l *RunLog) Mark(name string) time.Time {
	now := time.Now()
	l.mu.Lock()
	l.marks[name] = now
	l.mu.Unlock()
	l.Event("mark %s", name)
	return now
}

func (l *RunLog) Close() { l.f.Close() }
