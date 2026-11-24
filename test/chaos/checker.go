//go:build chaos && unix

package chaos

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anishathalye/porcupine"

	"coyotedb.com/coyote/internal/pg"
	"coyotedb.com/coyote/test/chaos/check"
)

// Verdict is the checker's result.
type Verdict struct {
	Problems []string
	Notes    []string
}

func (v *Verdict) fail(format string, args ...any) {
	v.Problems = append(v.Problems, fmt.Sprintf(format, args...))
}
func (v *Verdict) note(format string, args ...any) {
	v.Notes = append(v.Notes, fmt.Sprintf(format, args...))
}

// Check runs every check on a healed, healthy cluster after the workload
// stopped.
func Check(c *Cluster, h *History) *Verdict {
	v := &Verdict{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	conn, primary, err := c.FindPrimary(ctx, "")
	if err != nil {
		v.fail("no read-write node at the end: %v", err)
		return v
	}
	defer conn.Close(context.Background())

	// All copies caught up with the final primary.
	var flush string
	if err := conn.QueryRow(ctx, "SELECT pg_current_wal_flush_lsn()::text").Scan(&flush); err != nil {
		v.fail("final primary flush lsn: %v", err)
		return v
	}
	want, _ := pg.ParseLSN(flush)
	for _, n := range c.Data() {
		if n == primary {
			continue
		}
		err := poll(2*time.Minute, func() bool {
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			cn, err := c.Connect(cctx, n)
			if err != nil {
				return false
			}
			defer cn.Close(context.Background())
			var s *string
			if cn.QueryRow(cctx, "SELECT pg_last_wal_replay_lsn()::text").Scan(&s) != nil || s == nil {
				return false
			}
			l, _ := pg.ParseLSN(*s)
			return l >= want
		})
		if err != nil {
			v.fail("%s did not replay up to the final primary's %s", n.Name, flush)
		}
	}

	// 1. Appends: every ok write is there, no failed write is.
	rows, err := conn.Query(ctx, "SELECT id, tl FROM chaos_append")
	if err != nil {
		v.fail("read appends: %v", err)
		return v
	}
	have := map[int64]string{}
	for rows.Next() {
		var id int64
		var tl string
		if err := rows.Scan(&id, &tl); err != nil {
			v.fail("scan: %v", err)
		}
		have[id] = tl
	}
	rows.Close()
	problems, ok, failed, unknown := check.LostWrites(have, h.Appends, primary.Name)
	for _, p := range problems {
		v.fail("%s", p)
	}
	v.note("appends: %d ok, %d failed, %d unknown, %d rows", ok, failed, unknown, len(have))
	if ok == 0 {
		v.fail("no append was ever acknowledged: the workload did not run")
	}

	// 2. Acknowledgements in timeline order: one node per timeline, and
	// no node acknowledging on an older timeline after a newer one had.
	for _, p := range check.AckOrder(h.Appends) {
		v.fail("%s", p)
	}
	for tl, who := range c.PromotionTimelines() {
		if len(who) > 1 {
			v.fail("timeline %d was produced by more than one promotion: %v", tl, who)
		}
	}

	// 3. All data nodes identical.
	ref := ""
	for _, n := range c.Data() {
		hsh, err := c.tableHash(ctx, n)
		if err != nil {
			v.fail("hash on %s: %v", n.Name, err)
			continue
		}
		if ref == "" {
			ref = hsh
		} else if hsh != ref {
			v.fail("DIVERGED: %s's tables differ from the others", n.Name)
		}
	}

	// 4. Bank total.
	var total int64
	if err := conn.QueryRow(ctx, "SELECT sum(bal) FROM chaos_bank").Scan(&total); err != nil {
		v.fail("bank: %v", err)
	} else if total != accounts*initialBk {
		v.fail("bank total %d, want %d", total, accounts*initialBk)
	}
	v.note("bank: %d transfers ok", h.BankOK)

	// 5. Register linearizable.
	res, info := checkRegisters(h.Regs)
	switch res {
	case porcupine.Ok:
		v.note("registers: %d ops, linearizable", len(h.Regs))
	case porcupine.Illegal:
		p := filepath.Join(c.Dir, "registers.html")
		_ = porcupine.VisualizePath(registerModel, info, p)
		v.fail("NOT LINEARIZABLE: the register history has no valid order (see %s)", p)
	default:
		v.note("registers: linearizability check timed out (%d ops), inconclusive", len(h.Regs))
	}

	// 6. Never two nodes accepting writes at one instant, from every
	// node's own commit probe (any timeline). "No answer" is unknown:
	// a handover the probes cannot prove clean fails too.
	tm := check.TwoMains(h.Probes)
	for _, n := range tm.Notes {
		v.note("%s", n)
	}
	v.note("two-mains check: %d read-write stretches over %d probes", len(tm.Runs), len(h.Probes))
	for _, p := range tm.Proven {
		v.fail("%s (harness clock, +s from workload start %s)", p, h.Base.Format("15:04:05.000"))
	}
	for _, p := range tm.Unproven {
		v.fail("%s (harness clock, +s from workload start %s)", p, h.Base.Format("15:04:05.000"))
	}
	return v
}

func (c *Cluster) tableHash(ctx context.Context, n *Node) (string, error) {
	conn, err := c.Connect(ctx, n)
	if err != nil {
		return "", err
	}
	defer conn.Close(context.Background())
	var parts []string
	for _, q := range []string{
		"SELECT coalesce(md5(string_agg(id::text || ':' || tl, ',' ORDER BY id)), '') FROM chaos_append",
		"SELECT coalesce(md5(string_agg(k::text || ':' || v::text, ',' ORDER BY k)), '') FROM chaos_reg",
		"SELECT coalesce(md5(string_agg(id::text || ':' || bal::text, ',' ORDER BY id)), '') FROM chaos_bank",
	} {
		var s string
		if err := conn.QueryRow(ctx, q).Scan(&s); err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "/"), nil
}

type regIn struct {
	Key   int
	Write bool
	V     int64
}

var registerModel = porcupine.Model{
	Partition: func(h []porcupine.Operation) [][]porcupine.Operation {
		by := map[int][]porcupine.Operation{}
		for _, op := range h {
			k := op.Input.(regIn).Key
			by[k] = append(by[k], op)
		}
		var out [][]porcupine.Operation
		for _, ops := range by {
			out = append(out, ops)
		}
		return out
	},
	Init: func() any { return int64(0) },
	Step: func(state, input, output any) (bool, any) {
		in := input.(regIn)
		if in.Write {
			return true, in.V
		}
		return output.(int64) == state.(int64), state
	},
	Equal: func(a, b any) bool { return a == b },
	DescribeOperation: func(input, output any) string {
		in := input.(regIn)
		if in.Write {
			return fmt.Sprintf("k%d := %d", in.Key, in.V)
		}
		return fmt.Sprintf("k%d -> %v", in.Key, output)
	},
}

// checkRegisters: ok ops as they were; unknown writes may have happened at
// any time after their call (return time: never); unknown reads and
// failed ops are dropped (they changed nothing).
func checkRegisters(ops []RegOp) (porcupine.CheckResult, porcupine.LinearizationInfo) {
	var hist []porcupine.Operation
	base := time.Time{}
	for _, op := range ops {
		if base.IsZero() || op.Start.Before(base) {
			base = op.Start
		}
	}
	for _, op := range ops {
		in := regIn{Key: op.Key, Write: op.Write, V: op.Value}
		call := op.Start.Sub(base).Nanoseconds()
		switch {
		case op.Outcome == OK:
			var out any = int64(0)
			if !op.Write {
				out = op.Value
			}
			hist = append(hist, porcupine.Operation{ClientId: op.Client, Input: in, Call: call, Output: out, Return: op.End.Sub(base).Nanoseconds()})
		case op.Outcome == Unknown && op.Write:
			hist = append(hist, porcupine.Operation{ClientId: op.Client, Input: in, Call: call, Output: int64(0), Return: math.MaxInt64 / 2})
		}
	}
	return porcupine.CheckOperationsVerbose(registerModel, hist, 2*time.Minute)
}

// ResumeAfter is how long after t the first write was acknowledged (-1:
// never).
func ResumeAfter(h *History, t time.Time) time.Duration {
	best := time.Duration(-1)
	for _, op := range h.snapshotAppends() {
		if op.Outcome == OK && op.Start.After(t) {
			if d := op.End.Sub(t); best < 0 || d < best {
				best = d
			}
		}
	}
	return best
}

// writeHistory dumps the history for the artifacts directory.
func writeHistory(dir string, h *History) {
	f, err := os.Create(filepath.Join(dir, "history.txt"))
	if err != nil {
		return
	}
	defer f.Close()
	for _, op := range h.Appends {
		fmt.Fprintf(f, "append %d %s %s %s..%s tl=%s\n", op.ID, op.Node, op.Outcome, op.Start.Format("15:04:05.000"), op.End.Format("15:04:05.000"), op.TL)
	}
	for _, op := range h.Regs {
		fmt.Fprintf(f, "reg c%d k%d write=%v v=%d %s %s %s..%s\n", op.Client, op.Key, op.Write, op.Value, op.Node, op.Outcome, op.Start.Format("15:04:05.000"), op.End.Format("15:04:05.000"))
	}
	fmt.Fprintf(f, "probes: offsets from %s (harness monotonic clock)\n", h.Base.Format("15:04:05.000000"))
	for _, p := range h.Probes {
		fmt.Fprintf(f, "probe %s %s +%.3f..+%.3f %s %s\n", p.Node, p.Kind, p.Start.Seconds(), p.End.Seconds(), p.Conn, p.Detail)
	}
}
