package pg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The behaviour probe (docs/MIXED_OS.md condition 1): a fixed set of
// calculations whose answers must be identical on every data node. Physical
// replication copies bytes; the copy then re-computes things from those
// bytes (index positions, hash buckets, partition routing, text
// conversions). If two machines compute differently from the same bytes,
// an index can silently disagree with its table. The fingerprint
// (fingerprint.go) proves the files are laid out the same; the probe proves
// the code reading them behaves the same.
//
// Every statement is deterministic (no clock, no random, no catalog OIDs),
// uses only built-in functions, runs read-only (so it works on a standby),
// and returns text in a fixed order. A statement that fails records its
// SQLSTATE, which must then match too.
//
// The "libm" group is informational: those functions call the operating
// system's maths library and are known to differ in the last digit between
// macOS and Linux (Step 0). The portability audit keeps them out of stored
// structures instead (portability.go).

// ProbeVersion versions the statement set. Change it whenever a statement
// is added, removed or edited: results of different versions never match.
const ProbeVersion = 1

// ProbeStatement is one probe calculation.
type ProbeStatement struct {
	Name  string   // stable id, unique in its group
	What  string   // plain words
	Setup []string // SET LOCAL ... run first (undone afterwards)
	SQL   string
}

// ProbeGroup is a set of statements about one subject.
type ProbeGroup struct {
	Name string
	What string // plain words: "turning decimal numbers into text"
	// MustMatch: a difference blocks mixing the two machines. False: the
	// group is reported only.
	MustMatch  bool
	Statements []ProbeStatement
}

// ProbeGroups returns the probe (shared; do not modify).
func ProbeGroups() []ProbeGroup { return probeGroups }

// ProbeOptions controls RunProbe.
type ProbeOptions struct {
	// StatementTimeout per statement (default 60 s).
	StatementTimeout time.Duration
	// KeepOutput stores each statement's text output (up to 64 KiB) in the
	// result, for diagnosing a difference. Off by default: results are
	// then small enough to keep in the cluster state.
	KeepOutput bool
	// Groups limits the run to these group names (empty: all).
	Groups []string
}

// ProbeResult is the outcome of one probe run.
type ProbeResult struct {
	Version int                `json:"version"`
	OS      string             `json:"os"`
	Arch    string             `json:"arch"`
	Groups  []ProbeGroupResult `json:"groups"`
}

// ProbeGroupResult is one group's digest and its statements.
type ProbeGroupResult struct {
	Name       string                 `json:"name"`
	MustMatch  bool                   `json:"must_match"`
	Digest     string                 `json:"digest"`
	Statements []ProbeStatementResult `json:"statements"`
}

// ProbeStatementResult is one statement's digest.
type ProbeStatementResult struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Rows   int    `json:"rows"`
	// SQLState is set when the statement failed; the failure is part of
	// the digest.
	SQLState string `json:"sqlstate,omitempty"`
	// Output and Message (the error text) are kept only with
	// ProbeOptions.KeepOutput, and are not part of the digest.
	Output  string `json:"output,omitempty"`
	Message string `json:"message,omitempty"`
}

// Digest of the whole run (must-match groups only): equal digests mean
// the probe matched.
func (r ProbeResult) Digest() string {
	h := sha256.New()
	fmt.Fprintf(h, "version=%d\n", r.Version)
	for _, g := range r.Groups {
		if g.MustMatch {
			fmt.Fprintf(h, "%s=%s\n", g.Name, g.Digest)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// MustMatchGroups lists "group=digest" for each must-match group, in run
// order (kept in the cluster state so a refusal can name the group).
func (r ProbeResult) MustMatchGroups() []string {
	var out []string
	for _, g := range r.Groups {
		if g.MustMatch {
			out = append(out, g.Name+"="+g.Digest)
		}
	}
	return out
}

// probeSession fixes every setting that changes text output or
// comparison. Locale settings are "C" everywhere: they exist on every OS.
var probeSession = []string{
	"SET LOCAL TimeZone = 'UTC'",
	"SET LOCAL DateStyle = 'ISO, MDY'",
	"SET LOCAL IntervalStyle = 'postgres'",
	"SET LOCAL extra_float_digits = 1",
	"SET LOCAL bytea_output = 'hex'",
	"SET LOCAL standard_conforming_strings = on",
	"SET LOCAL search_path = pg_catalog",
	"SET LOCAL lc_monetary = 'C'",
	"SET LOCAL lc_numeric = 'C'",
	"SET LOCAL lc_time = 'C'",
	"SET LOCAL timezone_abbreviations = 'Default'",
	"SET LOCAL default_text_search_config = 'pg_catalog.simple'",
	"SET LOCAL max_parallel_workers_per_gather = 0",
	"SET LOCAL jit = off",
	"SET LOCAL xmloption = content",
	"SET LOCAL array_nulls = on",
	"SET LOCAL lock_timeout = '5s'",
}

// RunProbe runs the probe on c in one read-only transaction (rolled back).
// It returns an error only when the probe could not run (connection,
// cancellation, timeout); a failing statement is a result, not an error.
func RunProbe(ctx context.Context, c *pgx.Conn, o ProbeOptions) (ProbeResult, error) {
	if o.StatementTimeout <= 0 {
		o.StatementTimeout = 60 * time.Second
	}
	res := ProbeResult{Version: ProbeVersion, OS: runtime.GOOS, Arch: runtime.GOARCH}
	if _, err := c.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
		return res, fmt.Errorf("probe: %w", err)
	}
	defer func() {
		// Rollback even when ctx is done, so the connection stays usable.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = c.Exec(rctx, "ROLLBACK")
	}()
	setup := append(append([]string{}, probeSession...), fmt.Sprintf("SET LOCAL statement_timeout = %d", o.StatementTimeout.Milliseconds()))
	for _, s := range setup {
		if _, err := c.Exec(ctx, s); err != nil {
			return res, fmt.Errorf("probe setup %q: %w", s, err)
		}
	}
	for _, g := range probeGroups {
		if len(o.Groups) > 0 && !contains(o.Groups, g.Name) {
			continue
		}
		gr := ProbeGroupResult{Name: g.Name, MustMatch: g.MustMatch}
		gh := sha256.New()
		for _, st := range g.Statements {
			sr, err := runProbeStatement(ctx, c, st, o.KeepOutput)
			if err != nil {
				return res, fmt.Errorf("probe %s/%s: %w", g.Name, st.Name, err)
			}
			fmt.Fprintf(gh, "%s=%s\n", sr.Name, sr.Digest)
			gr.Statements = append(gr.Statements, sr)
		}
		gr.Digest = hex.EncodeToString(gh.Sum(nil))
		res.Groups = append(res.Groups, gr)
	}
	return res, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// runProbeStatement runs one statement inside a savepoint, always rolled
// back (that also undoes its SET LOCALs).
func runProbeStatement(ctx context.Context, c *pgx.Conn, st ProbeStatement, keep bool) (ProbeStatementResult, error) {
	sr := ProbeStatementResult{Name: st.Name}
	if _, err := c.Exec(ctx, "SAVEPOINT probe"); err != nil {
		return sr, err
	}
	var out strings.Builder
	failed := func(err error) error {
		state := sqlState(err)
		if state == "" || state == "57014" || state == "55P03" || ctx.Err() != nil {
			// Not a property of the calculation: a lost connection, a
			// cancel, a timeout or a lock wait.
			return err
		}
		sr.SQLState = state
		if keep {
			sr.Message = err.Error()
		}
		out.Reset()
		out.WriteString("ERROR " + state + "\n")
		return nil
	}
	err := func() error {
		for _, s := range st.Setup {
			if _, err := c.Exec(ctx, s); err != nil {
				return failed(err)
			}
		}
		rows, err := c.Query(ctx, st.SQL, pgx.QueryExecModeSimpleProtocol)
		if err != nil {
			return failed(err)
		}
		defer rows.Close()
		for rows.Next() {
			sr.Rows++
			for i, v := range rows.RawValues() {
				if i > 0 {
					out.WriteByte('\x1f')
				}
				if v == nil {
					out.WriteString(`\N`)
				} else {
					out.Write(v)
				}
			}
			out.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			sr.Rows = 0
			return failed(err)
		}
		return nil
	}()
	if err != nil {
		return sr, err
	}
	if _, err := c.Exec(ctx, "ROLLBACK TO SAVEPOINT probe"); err != nil {
		return sr, err
	}
	sum := sha256.Sum256([]byte(out.String()))
	sr.Digest = hex.EncodeToString(sum[:])
	if keep {
		s := out.String()
		if len(s) > 64<<10 {
			s = s[:64<<10]
		}
		sr.Output = s
	}
	return sr, nil
}

// ProbeDiff is one difference between two probe results.
type ProbeDiff struct {
	Group     string `json:"group"`
	Statement string `json:"statement,omitempty"`
	MustMatch bool   `json:"must_match"`
	Text      string `json:"text"`
}

// CompareProbes lists the statements whose answers differ, must-match
// groups first. a is this machine, b the other.
func CompareProbes(a, b ProbeResult) []ProbeDiff {
	if a.Version != b.Version {
		return []ProbeDiff{{Group: "version", MustMatch: true,
			Text: fmt.Sprintf("The two machines run different versions of the behaviour check (%d here, %d there); update this program on both.", a.Version, b.Version)}}
	}
	var must, info []ProbeDiff
	bg := map[string]ProbeGroupResult{}
	for _, g := range b.Groups {
		bg[g.Name] = g
	}
	for _, ga := range a.Groups {
		def := probeGroupByName(ga.Name)
		what := ga.Name
		if def != nil {
			what = def.What
		}
		gb, ok := bg[ga.Name]
		if !ok {
			must = append(must, ProbeDiff{Group: ga.Name, MustMatch: ga.MustMatch, Text: fmt.Sprintf("The other machine did not run the check of %s.", what)})
			continue
		}
		if ga.Digest == gb.Digest {
			continue
		}
		bs := map[string]ProbeStatementResult{}
		for _, s := range gb.Statements {
			bs[s.Name] = s
		}
		for _, sa := range ga.Statements {
			sb, ok := bs[sa.Name]
			if ok && sa.Digest == sb.Digest {
				continue
			}
			d := ProbeDiff{Group: ga.Name, Statement: sa.Name, MustMatch: ga.MustMatch}
			sw := sa.Name
			if def != nil {
				for _, st := range def.Statements {
					if st.Name == sa.Name && st.What != "" {
						sw = st.What
					}
				}
			}
			switch {
			case !ok:
				d.Text = fmt.Sprintf("The other machine did not run %q (%s).", sw, what)
			case sa.SQLState != sb.SQLState:
				d.Text = fmt.Sprintf("When %s, %s: %s here, %s on the other machine.", what, sw, outcome(sa), outcome(sb))
			default:
				d.Text = fmt.Sprintf("When %s, the machines give different answers for %s.", what, sw)
			}
			if ga.MustMatch {
				must = append(must, d)
			} else {
				info = append(info, d)
			}
		}
	}
	return append(must, info...)
}

func outcome(s ProbeStatementResult) string {
	if s.SQLState != "" {
		return "an error (" + s.SQLState + ")"
	}
	return "an answer"
}

// ProbeMismatch reports whether any difference is in a must-match group.
func ProbeMismatch(d []ProbeDiff) bool {
	for _, x := range d {
		if x.MustMatch {
			return true
		}
	}
	return false
}

func probeGroupByName(name string) *ProbeGroup {
	for i := range probeGroups {
		if probeGroups[i].Name == name {
			return &probeGroups[i]
		}
	}
	return nil
}

// ValidateProbe checks the statement set itself (unique names, no
// forbidden functions). Used by tests.
// bareVersion matches a call of version() itself.
var bareVersion = regexp.MustCompile(`(^|[^a-z0-9_])version\(\)`)

func ValidateProbe() error {
	var errs []error
	groups := map[string]bool{}
	for _, g := range probeGroups {
		if groups[g.Name] {
			errs = append(errs, fmt.Errorf("duplicate group %q", g.Name))
		}
		groups[g.Name] = true
		names := map[string]bool{}
		for _, s := range g.Statements {
			if names[s.Name] {
				errs = append(errs, fmt.Errorf("%s: duplicate statement %q", g.Name, s.Name))
			}
			names[s.Name] = true
			low := strings.ToLower(s.SQL)
			// version() is the server's build string (it differs between
			// machines by design); unicode_version() is fine.
			if bareVersion.MatchString(low) {
				errs = append(errs, fmt.Errorf("%s/%s: not deterministic (version())", g.Name, s.Name))
			}
			for _, bad := range []string{"now()", "random(", "clock_timestamp", "current_timestamp", "current_date", "localtime", "statement_timestamp", "transaction_timestamp", "timeofday", "gen_random_uuid", "uuidv4", "uuidv7(", "pg_backend_pid", "txid_current", "pg_current", "inet_server", "inet_client", "current_user", "session_user", "pg_postmaster_start_time", "'now'", "'today'", "'tomorrow'", "'yesterday'", "setseed"} {
				if strings.Contains(low, bad) {
					errs = append(errs, fmt.Errorf("%s/%s: not deterministic (%s)", g.Name, s.Name, bad))
				}
			}
		}
	}
	return errors.Join(errs...)
}
