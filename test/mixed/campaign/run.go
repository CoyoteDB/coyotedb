package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Config is the driver's nodes.json.
type Config struct {
	TokenFile string     `json:"token_file"`
	Nodes     []NodeSpec `json:"nodes"`
	// Schema is an optional SQL file run once on the main node before
	// the first round (psql meta-commands such as \set are skipped),
	// e.g. test/mixed/step0/workload.sql.
	Schema string `json:"schema,omitempty"`
}

// NodeSpec is one data node and where its peer listens.
type NodeSpec struct {
	Name string `json:"name"`
	Peer string `json:"peer"` // http://host:port
}

func loadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.TokenFile != "" && !filepath.IsAbs(c.TokenFile) {
		c.TokenFile = filepath.Join(filepath.Dir(path), c.TokenFile)
	}
	if c.Schema != "" && !filepath.IsAbs(c.Schema) {
		c.Schema = filepath.Join(filepath.Dir(path), c.Schema)
	}
	return c, c.validate()
}

func (c Config) validate() error {
	if c.TokenFile == "" {
		return errors.New("config: token_file is required")
	}
	if len(c.Nodes) < 2 {
		return errors.New("config: at least two data nodes are needed")
	}
	seen := map[string]bool{}
	for _, n := range c.Nodes {
		if n.Name == "" || n.Peer == "" {
			return errors.New("config: every node needs a name and a peer address")
		}
		if seen[n.Name] {
			return fmt.Errorf("config: node %q is listed twice", n.Name)
		}
		seen[n.Name] = true
		u, err := url.Parse(n.Peer)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("config: node %q: peer must be http(s)://host:port", n.Name)
		}
	}
	return nil
}

// switchPlan returns a cycle of planned switches, starting from the
// current main node, that moves the main role along every ordered pair
// of nodes exactly once (an Euler circuit of the complete directed
// graph, which exists because every node has as many ways in as out).
func switchPlan(names []string, start string) [][2]string {
	if !slices.Contains(names, start) || len(names) < 2 {
		return nil
	}
	// Hierholzer's algorithm with a deterministic edge order.
	next := map[string][]string{}
	for _, a := range names {
		for _, b := range names {
			if a != b {
				next[a] = append(next[a], b)
			}
		}
	}
	stack := []string{start}
	var circuit []string
	for len(stack) > 0 {
		v := stack[len(stack)-1]
		if len(next[v]) > 0 {
			w := next[v][0]
			next[v] = next[v][1:]
			stack = append(stack, w)
			continue
		}
		circuit = append(circuit, v)
		stack = stack[:len(stack)-1]
	}
	slices.Reverse(circuit)
	plan := make([][2]string, 0, len(circuit)-1)
	for i := 1; i < len(circuit); i++ {
		plan = append(plan, [2]string{circuit[i-1], circuit[i]})
	}
	return plan
}

// splitSQL turns a psql script into statements: meta-command lines
// (starting with a backslash) and comment-only lines are dropped, and
// statements end at a semicolon at the end of a line.
func splitSQL(script string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(script, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, `\`) || strings.HasPrefix(t, "--") || (t == "" && cur.Len() == 0) {
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
		if strings.HasSuffix(t, ";") {
			s := strings.TrimSpace(cur.String())
			out = append(out, strings.TrimSuffix(s, ";"))
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// Ledger tracks the writes the workload sent. A write is acknowledged
// only once its COMMIT returned success; one whose COMMIT failed or got
// no answer is "in doubt" (it may or may not be there, both are fine).
type Ledger struct {
	mu      sync.Mutex
	next    int64
	acked   map[int64]bool
	inDoubt map[int64]bool
}

func newLedger(start int64) *Ledger {
	return &Ledger{next: start, acked: map[int64]bool{}, inDoubt: map[int64]bool{}}
}

func (l *Ledger) take() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	return l.next
}

func (l *Ledger) result(id int64, acked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if acked {
		l.acked[id] = true
	} else {
		l.inDoubt[id] = true
	}
}

// LedgerCheck is the result of comparing the ledger with what is in the
// database.
type LedgerCheck struct {
	Acked    int     `json:"acked"`
	InDoubt  int     `json:"in_doubt"`
	Present  int     `json:"present"`
	Lost     []int64 `json:"lost,omitempty"`     // acknowledged but missing: a lost write
	Unknown  []int64 `json:"unknown,omitempty"`  // present but never sent by this run
	Repeated bool    `json:"repeated,omitempty"` // an id came back twice
}

// check compares the acknowledged ids with the ids found in the table.
func (l *Ledger) check(present []int64) LedgerCheck {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := LedgerCheck{Acked: len(l.acked), InDoubt: len(l.inDoubt), Present: len(present)}
	have := make(map[int64]bool, len(present))
	for _, id := range present {
		if have[id] {
			r.Repeated = true
		}
		have[id] = true
		if !l.acked[id] && !l.inDoubt[id] {
			r.Unknown = append(r.Unknown, id)
		}
	}
	for id := range l.acked {
		if !have[id] {
			r.Lost = append(r.Lost, id)
		}
	}
	slices.Sort(r.Lost)
	slices.Sort(r.Unknown)
	return r
}

func (r LedgerCheck) ok() bool { return len(r.Lost) == 0 && len(r.Unknown) == 0 && !r.Repeated }

// Record is one line of the JSONL report.
type Record struct {
	At         time.Time    `json:"at"`
	Kind       string       `json:"kind"` // start, switch, round, stop
	Round      int          `json:"round,omitempty"`
	From       string       `json:"from,omitempty"`
	To         string       `json:"to,omitempty"`
	OK         bool         `json:"ok"`
	Message    string       `json:"message,omitempty"`
	Took       string       `json:"took,omitempty"`
	Comparison *Comparison  `json:"comparison,omitempty"`
	Ledger     *LedgerCheck `json:"ledger,omitempty"`
	Nodes      []Inspection `json:"nodes,omitempty"`
}

// reportWriter appends records, one JSON object per line, flushed to
// disk after each so a crash leaves a readable report.
type reportWriter struct {
	mu sync.Mutex
	f  *os.File
	w  io.Writer
}

func (r *reportWriter) write(rec Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := r.w.Write(append(b, '\n')); err != nil {
		return err
	}
	if r.f != nil {
		return r.f.Sync()
	}
	return nil
}

// readReport reads a JSONL report back (for summaries and tests).
func readReport(rd io.Reader) ([]Record, error) {
	dec := json.NewDecoder(rd)
	var out []Record
	for {
		var r Record
		err := dec.Decode(&r)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
}

// Summary counts a report.
type Summary struct {
	Rounds, Switches, FailedRounds, FailedSwitches int
	Pairs                                          map[string]int // "from>to" -> successful switches
}

func summarize(recs []Record) Summary {
	s := Summary{Pairs: map[string]int{}}
	for _, r := range recs {
		switch r.Kind {
		case "switch":
			s.Switches++
			if !r.OK {
				s.FailedSwitches++
			} else {
				s.Pairs[r.From+">"+r.To]++
			}
		case "round":
			s.Rounds++
			if !r.OK {
				s.FailedRounds++
			}
		}
	}
	return s
}

// peerClient calls one peer.
type peerClient struct {
	name, base, token string
	http              *http.Client
}

func (p *peerClient) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(p.base, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", p.name, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", p.name, err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		return fmt.Errorf("%s: %s %s: %d %s", p.name, method, path, resp.StatusCode, e.Error)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

type driver struct {
	cfg    Config
	peers  []*peerClient
	byName map[string]*peerClient
	report *reportWriter
	ledger *Ledger
	stderr io.Writer
	tmp    string
	level  string
	amchk  bool
	runID  string
}

func cmdRun(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "nodes.json")
	out := fs.String("out", "", "report file (JSONL, appended)")
	dur := fs.Duration("duration", 72*time.Hour, "how long to run")
	keepGoing := fs.Bool("keep-going", false, "continue after a failed comparison")
	level := fs.String("level", "standard", "integrity check level after each switch: quick, standard, deep")
	amchk := fs.Bool("pg-amcheck", false, "also run pg_amcheck on every node after each switch")
	settle := fs.Duration("settle", 30*time.Second, "how long the workload runs between switches")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *cfgPath == "" || *out == "" {
		fs.Usage()
		return errUsage
	}
	if _, err := parseLevel(*level); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	tok, err := readToken(cfg.TokenFile)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	tmp, err := os.MkdirTemp("", "coyote-campaign-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	idb := make([]byte, 6)
	if _, err := rand.Read(idb); err != nil {
		return err
	}
	d := &driver{cfg: cfg, byName: map[string]*peerClient{}, report: &reportWriter{f: f, w: f}, stderr: stderr,
		tmp: tmp, level: *level, amchk: *amchk, runID: hex.EncodeToString(idb)}
	for _, n := range cfg.Nodes {
		p := &peerClient{name: n.Name, base: n.Peer, token: tok, http: &http.Client{Timeout: 3 * time.Hour}}
		d.peers = append(d.peers, p)
		d.byName[n.Name] = p
	}
	err = d.loop(ctx, *dur, *settle, *keepGoing)
	msg := "finished"
	if err != nil {
		msg = err.Error()
	}
	_ = d.report.write(Record{Kind: "stop", OK: err == nil, Message: msg})
	recs, rerr := func() ([]Record, error) {
		rf, err := os.Open(*out)
		if err != nil {
			return nil, err
		}
		defer rf.Close()
		return readReport(rf)
	}()
	if rerr == nil {
		s := summarize(recs)
		fmt.Fprintf(stdout, "rounds %d (%d failed), switches %d (%d failed), pairs %v\n", s.Rounds, s.FailedRounds, s.Switches, s.FailedSwitches, s.Pairs)
	}
	return err
}

func (d *driver) logf(format string, a ...any) {
	fmt.Fprintf(d.stderr, time.Now().Format("15:04:05 ")+format+"\n", a...)
}

// mainNode asks every peer for the cluster status until one answers.
func (d *driver) mainNode(ctx context.Context) (string, error) {
	var last error
	for _, p := range d.peers {
		var st struct {
			Health struct {
				MainNode string `json:"main_node"`
			} `json:"health"`
		}
		if err := p.do(ctx, http.MethodGet, "/v1/status", nil, &st); err != nil {
			last = err
			continue
		}
		if st.Health.MainNode != "" {
			return st.Health.MainNode, nil
		}
		last = fmt.Errorf("%s: no main node right now", p.name)
	}
	return "", last
}

// appConn connects to the application port the way an application does
// (connection string from conninfo, verified TLS).
func (d *driver) appConn(ctx context.Context) (*pgx.Conn, error) {
	var last error
	for _, p := range d.peers {
		var ci ConnInfo
		if err := p.do(ctx, http.MethodGet, "/v1/conninfo", nil, &ci); err != nil {
			last = err
			continue
		}
		if ci.Pending || ci.URL == "" {
			last = fmt.Errorf("%s: application role not ready", p.name)
			continue
		}
		u, err := localCA(ci, filepath.Join(d.tmp, "ca-"+p.name+".crt"))
		if err != nil {
			return nil, err
		}
		return pgx.Connect(ctx, u)
	}
	return nil, last
}

// localCA writes the cluster CA next to the driver and points the
// connection string at it (the peer's path is on the peer's machine).
func localCA(ci ConnInfo, path string) (string, error) {
	u, err := url.Parse(ci.URL)
	if err != nil {
		return "", fmt.Errorf("conninfo url: %w", err)
	}
	q := u.Query()
	if ci.CAPEM != "" {
		if err := os.WriteFile(path, []byte(ci.CAPEM), 0o600); err != nil {
			return "", err
		}
		q.Set("sslrootcert", path)
	}
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	return u.String(), nil
}

func (d *driver) loop(ctx context.Context, dur, settle time.Duration, keepGoing bool) error {
	ctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	names := make([]string, len(d.peers))
	for i, p := range d.peers {
		names[i] = p.name
		var info map[string]string
		if err := p.do(ctx, http.MethodGet, "/v1/info", nil, &info); err != nil {
			return err
		}
		if info["name"] != p.name {
			return fmt.Errorf("peer %s says it is node %q", p.name, info["name"])
		}
		d.logf("peer %s: %s/%s", p.name, info["os"], info["arch"])
	}
	main, err := d.mainNode(ctx)
	if err != nil {
		return err
	}
	if _, ok := d.byName[main]; !ok {
		return fmt.Errorf("the main node %q is not in the config", main)
	}
	var stmts []string
	if d.cfg.Schema != "" {
		b, err := os.ReadFile(d.cfg.Schema)
		if err != nil {
			return err
		}
		stmts = splitSQL(string(b))
	}
	ac, err := d.appConn(ctx)
	if err != nil {
		return err
	}
	appUser, appDB := ac.Config().User, ac.Config().Database
	ac.Close(ctx)
	if err := d.byName[main].do(ctx, http.MethodPost, "/v1/setup", SetupRequest{Database: appDB, AppUser: appUser, Statements: stmts}, nil); err != nil {
		return err
	}
	// Ids from this run start above anything already in the ledger.
	ac, err = d.appConn(ctx)
	if err != nil {
		return err
	}
	var maxID int64
	err = ac.QueryRow(ctx, "SELECT coalesce(max(id), 0) FROM public.campaign_ledger").Scan(&maxID)
	ac.Close(ctx)
	if err != nil {
		return err
	}
	d.ledger = newLedger(maxID)
	_ = d.report.write(Record{Kind: "start", OK: true, Message: fmt.Sprintf("run %s, nodes %v, main node %s", d.runID, names, main)})

	round := 0
	if fail := d.inspectRound(ctx, round, "", main); fail != nil && !keepGoing {
		return fail
	}
	for ctx.Err() == nil {
		for _, sw := range switchPlan(names, main) {
			if err := d.workFor(ctx, settle); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			round++
			start := time.Now()
			var res map[string]any
			err := d.byName[sw[0]].do(ctx, http.MethodPost, "/v1/switch", map[string]string{"to": sw[1]}, &res)
			rec := Record{Kind: "switch", Round: round, From: sw[0], To: sw[1], OK: err == nil, Took: time.Since(start).Round(time.Millisecond).String()}
			if err != nil {
				rec.Message = shortErr(err)
			}
			_ = d.report.write(rec)
			if err != nil {
				d.logf("switch %s -> %s failed: %v", sw[0], sw[1], err)
				if !keepGoing {
					return err
				}
				if main, err = d.mainNode(ctx); err != nil {
					return err
				}
				break // re-plan from the actual main node
			}
			main = sw[1]
			if fail := d.inspectRound(ctx, round, sw[0], sw[1]); fail != nil && !keepGoing {
				return fail
			}
		}
	}
	return nil
}

// workFor runs the write workload for a while: ledger inserts, each in
// its own transaction, through the application connection string.
func (d *driver) workFor(ctx context.Context, dur time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var c *pgx.Conn
			defer func() {
				if c != nil {
					c.Close(context.WithoutCancel(ctx))
				}
			}()
			for wctx.Err() == nil {
				if c == nil {
					var err error
					if c, err = d.appConn(wctx); err != nil {
						c = nil
						select {
						case <-wctx.Done():
						case <-time.After(time.Second):
						}
						continue
					}
				}
				id := d.ledger.take()
				_, err := c.Exec(wctx, "INSERT INTO public.campaign_ledger (id, body) VALUES ($1, md5($1::text) || $2)", id, d.runID)
				d.ledger.result(id, err == nil)
				if err != nil {
					c.Close(context.WithoutCancel(ctx))
					c = nil
				}
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// inspectRound waits until every copy has replayed the main node's
// changes, inspects all nodes (main node first), compares, and checks
// the ledger. It returns an error when the round failed.
func (d *driver) inspectRound(ctx context.Context, round int, from, main string) error {
	start := time.Now()
	rec := Record{Kind: "round", Round: round, From: from, To: main}
	fail := func(msg string) error {
		rec.OK, rec.Message, rec.Took = false, msg, time.Since(start).Round(time.Millisecond).String()
		_ = d.report.write(rec)
		d.logf("round %d failed: %s", round, msg)
		return errors.New(msg)
	}
	var lsn LSNReport
	if err := d.byName[main].do(ctx, http.MethodGet, "/v1/lsn", nil, &lsn); err != nil {
		return fail(shortErr(err))
	}
	if lsn.InRecovery {
		return fail(main + " is not the main node")
	}
	for _, p := range d.peers {
		if p.name == main {
			continue
		}
		deadline := time.Now().Add(10 * time.Minute)
		for {
			var r LSNReport
			err := p.do(ctx, http.MethodGet, "/v1/lsn?at_least="+url.QueryEscape(lsn.LSN), nil, &r)
			if err == nil && !r.Behind {
				break
			}
			if time.Now().After(deadline) {
				return fail(fmt.Sprintf("%s did not catch up to %s in 10 minutes", p.name, lsn.LSN))
			}
			select {
			case <-ctx.Done():
				return fail("stopped")
			case <-time.After(time.Second):
			}
		}
	}
	ac, err := d.appConn(ctx)
	if err != nil {
		return fail(shortErr(err))
	}
	ins := make([]Inspection, len(d.peers))
	order := append([]*peerClient{d.byName[main]}, slices.DeleteFunc(slices.Clone(d.peers), func(p *peerClient) bool { return p.name == main })...)
	var wg sync.WaitGroup
	errs := make([]error, len(order))
	req := InspectRequest{Database: ac.Config().Database, Level: d.level, Amcheck: d.amchk, Seed: int64(round) + 1}
	for i, p := range order {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = p.do(ctx, http.MethodPost, "/v1/inspect", req, &ins[i])
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			ac.Close(ctx)
			return fail(shortErr(err))
		}
	}
	rows, err := ac.Query(ctx, "SELECT id FROM public.campaign_ledger WHERE body LIKE '%' || $1", d.runID)
	var present []int64
	if err == nil {
		present, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	}
	ac.Close(ctx)
	if err != nil {
		return fail("read ledger: " + shortErr(err))
	}
	cmp := compareInspections(ins)
	lc := d.ledger.check(present)
	rec.Comparison, rec.Ledger, rec.Nodes = &cmp, &lc, ins
	rec.OK = cmp.OK && lc.ok()
	rec.Took = time.Since(start).Round(time.Millisecond).String()
	if !rec.OK {
		rec.Message = fmt.Sprintf("%d problems, %d lost writes", len(cmp.Problems), len(lc.Lost))
	}
	if err := d.report.write(rec); err != nil {
		return err
	}
	d.logf("round %d (main %s): ok=%v acked=%d lost=%d problems=%d", round, main, rec.OK, lc.Acked, len(lc.Lost), len(cmp.Problems))
	if !rec.OK {
		return errors.New(rec.Message)
	}
	return nil
}
