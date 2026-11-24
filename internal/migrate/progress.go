package migrate

import (
	"context"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// progress turns pg_dump / pg_restore --verbose lines into a few plain
// lines: the share of the data done, by table size, every 10% (or every
// 15 s when nothing crosses a step), then the indexes and keys.
type progress struct {
	m        *mover
	verb     string
	mu       sync.Mutex
	total    int64            // bytes of all tables
	byName   map[string]int64 // table name (no schema) -> bytes
	byLabel  map[string]int64 // schema.name -> bytes
	data     map[int]tocEntry // restore: dump id -> TABLE DATA entry
	doneSize int64
	tables   int
	ntables  int
	post     int // restore: items after the data (indexes, keys, ...)
	postDone int
	lastPct  int
	lastAt   time.Time
	postSaid bool
}

func newProgress(m *mover, verb string, tables []table, data map[int]tocEntry) *progress {
	p := &progress{m: m, verb: verb, byName: map[string]int64{}, byLabel: map[string]int64{}, data: data,
		ntables: len(tables), lastAt: time.Now()}
	for _, t := range tables {
		w := t.Size + 8192 // empty tables still take a step
		p.total += w
		p.byName[t.Name] += w
		p.byLabel[t.label()] = w
	}
	return p
}

// finishedItem parses "<tool>: finished item 3912 TABLE DATA items".
func finishedItem(line string) (id int, desc string, ok bool) {
	_, rest, found := strings.Cut(line, ": finished item ")
	if !found {
		return 0, "", false
	}
	idStr, desc, _ := strings.Cut(rest, " ")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return 0, "", false
	}
	return id, desc, true
}

func (p *progress) dumpLine(line string) {
	id, desc, ok := finishedItem(line)
	if !ok || !strings.HasPrefix(desc, "TABLE DATA ") {
		return
	}
	_ = id
	name := strings.TrimPrefix(desc, "TABLE DATA ")
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tables++
	p.doneSize += p.byName[name] / max(1, int64(countSameName(p.byLabel, name)))
	p.report()
}

func countSameName(byLabel map[string]int64, name string) int {
	n := 0
	for l := range byLabel {
		if strings.HasSuffix(l, "."+name) {
			n++
		}
	}
	return n
}

func (p *progress) restoreLine(line string) {
	id, _, ok := finishedItem(line)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, isData := p.data[id]; isData {
		p.tables++
		p.doneSize += p.byLabel[e.Schema+"."+e.Name]
		p.report()
		return
	}
	p.postDone++
	if p.tables >= len(p.data) && p.post > 0 && len(p.data) > 0 {
		if !p.postSaid {
			p.postSaid = true
			p.m.say("  building indexes and keys…")
		}
	}
}

// report prints a line when the share crosses a 10% step, or 15 s passed.
func (p *progress) report() {
	if p.total <= 0 {
		return
	}
	pct := int(min(100, p.doneSize*100/p.total))
	if pct/10 > p.lastPct/10 || (time.Since(p.lastAt) > 15*time.Second && pct > p.lastPct) {
		p.lastPct, p.lastAt = pct, time.Now()
		if pct < 100 {
			p.m.say("  %s %d%% (%d of %d %s)", p.verb, pct, p.tables, p.ntables, plural(p.ntables, "table", "tables"))
		}
	}
}

// restoreLog collects pg_restore's errors in plain words, and notices a
// lost connection (the main node went away).
type restoreLog struct {
	mu       sync.Mutex
	errs     []string
	lastTOC  string
	lostLine string
}

// connectionLost: libpq and pg_restore messages meaning the server went
// away, not that a statement failed.
var connectionLost = []string{
	"server closed the connection unexpectedly",
	"terminating connection",
	"could not receive data from server",
	"could not send data to server",
	"no connection to the server",
	"connection to server",
	"SSL SYSCALL error",
	"SSL error",
	"EOF detected",
	"a worker process died unexpectedly",
	"lost synchronization with server",
	"Connection refused",
	"connection timed out",
	"the database system is shutting down",
	"the database system is in recovery mode",
	"in a read-only transaction",
}

func (r *restoreLog) line(l string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.HasPrefix(l, "pg_restore: from TOC entry ") {
		// "pg_restore: from TOC entry 3; 3079 16423 EXTENSION pg_stat_statements (no owner)"
		r.lastTOC = tocWhat(strings.TrimPrefix(l, "pg_restore: from TOC entry "))
		return
	}
	msg, ok := strings.CutPrefix(l, "pg_restore: error: ")
	if !ok {
		return
	}
	for _, s := range connectionLost {
		if strings.Contains(msg, s) {
			if r.lostLine == "" {
				r.lostLine = msg
			}
			return
		}
	}
	msg = strings.TrimPrefix(msg, "could not execute query: ")
	msg = strings.TrimPrefix(msg, "ERROR:  ")
	if r.lastTOC != "" {
		msg = r.lastTOC + ": " + msg
		r.lastTOC = ""
	}
	r.errs = append(r.errs, msg)
}

func (r *restoreLog) lost() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lostLine != ""
}

// tocWhat names the object of pg_restore's "from TOC entry" line, whose
// form is "<id>; <tableoid> <oid> <DESC> <tag> <owner>":
// "3; 3079 16423 EXTENSION pg_stat_statements (no owner)" becomes
// "extension pg_stat_statements".
func tocWhat(line string) string {
	_, rest, ok := strings.Cut(line, ";")
	if !ok {
		return ""
	}
	rest = strings.TrimSuffix(strings.TrimSpace(rest), " (no owner)")
	f := strings.Fields(rest)
	if len(f) < 4 {
		return ""
	}
	if !strings.HasSuffix(line, " (no owner)") {
		f = f[:len(f)-1] // the owner
	}
	f = f[2:]
	for i, w := range f {
		if w != strings.ToUpper(w) || strings.ToUpper(w) == strings.ToLower(w) {
			break
		}
		f[i] = strings.ToLower(w)
	}
	return strings.Join(f, " ")
}

// heartbeat prints how much has been written so far whenever the tool has
// been quiet for a while (one big table gives no other sign of life).
// size must be safe to call from another goroutine. stop waits for it.
func (p *progress) heartbeat(ctx context.Context, label string, size func(context.Context) (int64, error)) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			p.mu.Lock()
			quiet := time.Since(p.lastAt) >= 15*time.Second
			p.mu.Unlock()
			if !quiet {
				continue
			}
			// Not cancelled by stop: cancelling a query closes the session,
			// and the caller uses it again afterwards.
			qctx, qcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			n, err := size(qctx)
			qcancel()
			if err != nil || ctx.Err() != nil {
				continue
			}
			p.mu.Lock()
			p.lastAt = time.Now()
			p.mu.Unlock()
			p.m.say("  %s %s so far…", label, humanBytes(n))
		}
	}()
	return func() { cancel(); <-done }
}

// dirSize adds up the sizes of the files under dir.
func dirSize(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a file being written may come and go
		}
		if !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n, err
}
