package pg

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Roles the agent creates on a primary. They replicate to every standby
// like any other catalog change, so their passwords are cluster-wide
// secrets (see Secrets).
const (
	// ReplUser streams WAL and takes base backups (REPLICATION LOGIN).
	ReplUser = "coyote_repl"
	// RewindUser is the pg_rewind source role: LOGIN plus exactly the
	// function grants pg_rewind documents, never superuser.
	RewindUser = "coyote_rewind"
	// HealthUser is the proxy's check that a node really is the primary
	// (SELECT pg_is_in_recovery()): LOGIN only, no other privilege.
	HealthUser = "coyote_health"
)

// HealthPassword derives the health role's password from the superuser
// password (HMAC-SHA256, one way): every data node can compute it and no
// second secret has to be stored or distributed.
func HealthPassword(superuser string) string {
	m := hmac.New(sha256.New, []byte(superuser))
	m.Write([]byte("coyote health role v1"))
	return hex.EncodeToString(m.Sum(nil))
}

// Upstream is where a standby gets its WAL from. A standby never knows a
// "remote host": in M2 this is another node's 127.0.0.1:<pg port>; from M3
// on it is a local listener of the agent's mTLS tunnel. Every connection
// string to an upstream is built by Conninfo.
type Upstream struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (u Upstream) String() string { return net.JoinHostPort(u.Host, strconv.Itoa(u.Port)) }

// Valid reports whether u names a host and a usable port.
func (u Upstream) Valid() error {
	if u.Host == "" {
		return errors.New("upstream host is empty")
	}
	if strings.ContainsAny(u.Host, " \t\r\n'\"\\=") {
		return fmt.Errorf("upstream host %q has characters that are not allowed", u.Host)
	}
	if u.Port <= 0 || u.Port > 65535 {
		return fmt.Errorf("upstream port %d is out of range", u.Port)
	}
	return nil
}

// ParseUpstream parses "host:port".
func ParseUpstream(s string) (Upstream, error) {
	h, p, err := net.SplitHostPort(strings.TrimSpace(s))
	if err != nil {
		return Upstream{}, fmt.Errorf("upstream %q: want host:port: %w", s, err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return Upstream{}, fmt.Errorf("upstream %q: bad port", s)
	}
	u := Upstream{Host: h, Port: port}
	return u, u.Valid()
}

// ConnParams describes one libpq connection to an upstream.
type ConnParams struct {
	Upstream Upstream
	User     string
	DBName   string // empty: not set (physical replication ignores it)
	AppName  string // application_name; for standbys, the node name
	Passfile string // libpq passfile path; empty: not set
}

// Conninfo renders a libpq key=value connection string. It is the single
// place that builds connection strings to an upstream (primary_conninfo,
// pg_basebackup, pg_rewind, slot management). Passwords never go in here:
// they live in a 0600 passfile.
func Conninfo(p ConnParams) string {
	kv := [][2]string{
		{"host", p.Upstream.Host},
		{"port", strconv.Itoa(p.Upstream.Port)},
		{"user", p.User},
	}
	if p.DBName != "" {
		kv = append(kv, [2]string{"dbname", p.DBName})
	}
	if p.Passfile != "" {
		kv = append(kv, [2]string{"passfile", p.Passfile})
	}
	if p.AppName != "" {
		kv = append(kv, [2]string{"application_name", p.AppName})
	}
	// The upstream is always on 127.0.0.1 (the agent's tunnel, which
	// provides TLS), so TLS here would only be in the way.
	kv = append(kv, [2]string{"sslmode", "disable"}, [2]string{"connect_timeout", "10"})
	kv = append(kv, connTimeouts...)
	parts := make([]string, len(kv))
	for i, e := range kv {
		parts[i] = e[0] + "=" + quoteConninfo(e[1])
	}
	return strings.Join(parts, " ")
}

// connTimeouts end a connection whose other end is gone (D57). The WAL
// receiver opens its connection without applying connect_timeout, and the
// kernel's own keepalive starts after two hours; if the tunnel's close of
// a half-open connection is lost (packet loss), the receiver would wait
// that long for a reply. With these, an idle connection to a socket that
// no longer exists is found within about 25 s (the probe is answered with
// a reset), and on Linux data unacknowledged for 30 s ends it too.
// libpq skips an option the operating system lacks (tcp_user_timeout off
// Linux, keepalives_count on older Windows), so they are safe everywhere.
// Streaming connections are never idle that long: the WAL receiver and
// sender exchange status messages at least every 10 s.
var connTimeouts = [][2]string{
	{"keepalives", "1"},
	{"keepalives_idle", "10"},
	{"keepalives_interval", "5"},
	{"keepalives_count", "3"},
	{"tcp_user_timeout", "30000"},
}

// quoteConninfo quotes a libpq conninfo value: single quotes, with
// backslash and single quote escaped by a backslash.
func quoteConninfo(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\r\n'\\=") {
		return v
	}
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `'`, `\'`)
	return "'" + v + "'"
}

// PassEntry is one line of a libpq password file.
type PassEntry struct {
	Host, Port, DB, User, Password string
}

// RenderPassfile renders a libpq password file
// (hostname:port:database:username:password). ':' and '\' inside a field
// are escaped with a backslash. Newlines are rejected by the caller (our
// passwords are base64url).
func RenderPassfile(entries []PassEntry) string {
	esc := func(s string) string {
		if s == "*" {
			return s
		}
		s = strings.ReplaceAll(s, `\`, `\\`)
		return strings.ReplaceAll(s, `:`, `\:`)
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s:%s:%s:%s:%s\n", esc(e.Host), esc(e.Port), esc(e.DB), esc(e.User), esc(e.Password))
	}
	return b.String()
}

// upstreamPassEntries are the credentials this node uses towards up.
func upstreamPassEntries(up Upstream, s Secrets) []PassEntry {
	port := strconv.Itoa(up.Port)
	return []PassEntry{
		// "replication" matches physical replication connections.
		{up.Host, port, "replication", ReplUser, s.Replication},
		{up.Host, port, "*", ReplUser, s.Replication},
		{up.Host, port, "*", RewindUser, s.Rewind},
	}
}

// ---- node names, slots, sync list ----

const maxNameLen = 48

// ValidName checks a node name: 1–48 of [a-z0-9-], starting and ending
// with a letter or digit. No '_' so the slot name mapping is one-to-one.
func ValidName(s string) error {
	if s == "" || len(s) > maxNameLen {
		return fmt.Errorf("node name %q must be 1–%d characters", s, maxNameLen)
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '-' && i > 0 && i < len(s)-1)
		if !ok {
			return fmt.Errorf("node name %q may only use a-z, 0-9 and inner '-'", s)
		}
	}
	return nil
}

// SanitizeName turns a host name into a valid node name ("Alex's-MacBook.local"
// -> "alex-s-macbook"). It never returns an invalid name.
func SanitizeName(host string) string {
	host, _, _ = strings.Cut(strings.ToLower(strings.TrimSpace(host)), ".")
	var b strings.Builder
	dash := false
	for _, r := range host {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := b.String()
	if len(s) > maxNameLen {
		s = s[:maxNameLen]
	}
	s = strings.Trim(s, "-")
	if s == "" {
		return "node"
	}
	return s
}

// SlotName is the physical replication slot a node streams from, on
// whichever node is its upstream.
func SlotName(node string) string { return "coyote_" + strings.ReplaceAll(node, "-", "_") }

// RenderSyncNames renders synchronous_standby_names: `ANY k ("a", "b")`,
// or "" for none. Names are always double-quoted ("" escapes a quote), so
// keywords, digits-first and mixed-case names are taken literally.
func RenderSyncNames(count int, names []string) (string, error) {
	if len(names) == 0 {
		if count != 0 {
			return "", errors.New("sync count must be 0 when the list is empty")
		}
		return "", nil
	}
	if count < 1 || count > len(names) {
		return "", fmt.Errorf("sync count %d must be between 1 and %d", count, len(names))
	}
	seen := map[string]bool{}
	q := make([]string, len(names))
	for i, n := range names {
		if n == "" || strings.ContainsAny(n, "\x00\r\n") {
			return "", fmt.Errorf("bad standby name %q", n)
		}
		if seen[strings.ToLower(n)] {
			return "", fmt.Errorf("standby name %q listed twice", n)
		}
		seen[strings.ToLower(n)] = true
		q[i] = `"` + strings.ReplaceAll(n, `"`, `""`) + `"`
	}
	return fmt.Sprintf("ANY %d (%s)", count, strings.Join(q, ", ")), nil
}

// ParseSyncNames parses a synchronous_standby_names value as
// RenderSyncNames writes it ("ANY k (a, b)"), and also the plain forms
// Postgres accepts ("a, b" and "FIRST k (a, b)", "k (a, b)"). It returns
// the count of standbys a commit waits for and the names, or an error for
// anything else (so an unexpected value is never read as "no sync").
func ParseSyncNames(s string) (int, []string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil, nil
	}
	count := 1
	list := s
	if i := strings.IndexByte(s, '('); i >= 0 {
		if !strings.HasSuffix(s, ")") {
			return 0, nil, fmt.Errorf("synchronous_standby_names %q not understood", s)
		}
		head := strings.Fields(s[:i])
		if len(head) == 2 && (strings.EqualFold(head[0], "ANY") || strings.EqualFold(head[0], "FIRST")) {
			head = head[1:]
		}
		if len(head) != 1 {
			return 0, nil, fmt.Errorf("synchronous_standby_names %q not understood", s)
		}
		n, err := strconv.Atoi(head[0])
		if err != nil || n < 1 {
			return 0, nil, fmt.Errorf("synchronous_standby_names %q not understood", s)
		}
		count, list = n, s[i+1:len(s)-1]
	}
	var names []string
	var cur strings.Builder
	inQ := false
	flush := func() error {
		n := strings.TrimSpace(cur.String())
		cur.Reset()
		if n == "" {
			return fmt.Errorf("synchronous_standby_names %q has an empty name", s)
		}
		names = append(names, n)
		return nil
	}
	quoted := false
	for i := 0; i < len(list); i++ {
		ch := list[i]
		switch {
		case inQ && ch == '"' && i+1 < len(list) && list[i+1] == '"':
			cur.WriteByte('"')
			i++
		case ch == '"':
			inQ = !inQ
			quoted = true
		case !inQ && ch == ',':
			if err := flush(); err != nil {
				return 0, nil, err
			}
			quoted = false
		default:
			if !inQ && !quoted && (ch == ' ' || ch == '\t') && cur.Len() == 0 {
				continue
			}
			cur.WriteByte(ch)
		}
	}
	if inQ {
		return 0, nil, fmt.Errorf("synchronous_standby_names %q has an open quote", s)
	}
	if err := flush(); err != nil {
		return 0, nil, err
	}
	if count > len(names) {
		count = len(names)
	}
	return count, names, nil
}

// ---- roles ----

// rewindGrants are the function grants the pg_rewind docs list for a
// non-superuser source role (PostgreSQL 18, "pg_rewind", Notes).
var rewindGrants = []string{
	"pg_catalog.pg_ls_dir(text, boolean, boolean)",
	"pg_catalog.pg_stat_file(text, boolean)",
	"pg_catalog.pg_read_binary_file(text)",
	"pg_catalog.pg_read_binary_file(text, bigint, bigint, boolean)",
}

// EnsureRoles creates or updates the replication and rewind roles on a
// primary so their passwords match s. Postgres stores them as SCRAM
// verifiers; statement logging is switched off for this transaction so the
// plain text never reaches the server log.
func EnsureRoles(ctx context.Context, c *pgx.Conn, s Secrets) error {
	if s.Replication == "" || s.Rewind == "" || s.Superuser == "" {
		return errors.New("superuser/replication/rewind passwords missing")
	}
	var inRecovery bool
	if err := c.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return err
	}
	if inRecovery {
		return errors.New("roles can only be created on the primary")
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Never let a password reach the server log, and do not wait for a sync
	// standby that may be down: the next start re-applies all of this.
	for _, q := range []string{
		"SET LOCAL log_statement = 'none'",
		"SET LOCAL log_min_error_statement = 'panic'",
		"SET LOCAL synchronous_commit = 'local'",
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	type role struct{ name, attrs, pw string }
	for _, r := range []role{
		{ReplUser, "LOGIN REPLICATION NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS", s.Replication},
		{RewindUser, "LOGIN NOREPLICATION NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS", s.Rewind},
		{HealthUser, "LOGIN NOREPLICATION NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOINHERIT CONNECTION LIMIT 20", HealthPassword(s.Superuser)},
	} {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", r.name).Scan(&exists); err != nil {
			return err
		}
		verb := "ALTER"
		if !exists {
			verb = "CREATE"
		}
		// Utility statements take no bind parameters: quote as a literal.
		q := fmt.Sprintf("%s ROLE %s WITH %s PASSWORD %s", verb, pgx.Identifier{r.name}.Sanitize(), r.attrs, quoteLiteral(r.pw))
		if _, err := tx.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s role %s: %w", strings.ToLower(verb), r.name, err)
		}
	}
	rw := pgx.Identifier{RewindUser}.Sanitize()
	for _, f := range rewindGrants {
		if _, err := tx.Exec(ctx, "GRANT EXECUTE ON FUNCTION "+f+" TO "+rw); err != nil {
			return fmt.Errorf("grant %s: %w", f, err)
		}
	}
	// pg_checkpoint lets the rewinding node run CHECKPOINT on the new
	// primary before pg_rewind (safety rule 7) without superuser.
	if _, err := tx.Exec(ctx, "GRANT pg_checkpoint TO "+rw); err != nil {
		return fmt.Errorf("grant pg_checkpoint: %w", err)
	}
	return tx.Commit(ctx)
}

// AppUser and AppDatabase are what clients use: a normal role (no
// superuser, no replication) that owns its own database. They reach it
// only over TLS through the proxy (see RenderHBA).
const (
	AppUser     = "app"
	AppDatabase = "app"
)

// EnsureAppRole creates the application role and its database, or sets the
// role's password, on the primary. It is idempotent.
func EnsureAppRole(ctx context.Context, c *pgx.Conn, password string) error {
	if len(password) < 16 {
		return errors.New("application password missing or too short")
	}
	var inRecovery bool
	if err := c.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err != nil {
		return err
	}
	if inRecovery {
		return errors.New("roles can only be created on the primary")
	}
	// Session settings: no password in the server log, and no wait for a
	// sync copy that may be down (the next run re-applies all of this).
	for _, q := range []string{
		"SET log_statement = 'none'",
		"SET log_min_error_statement = 'panic'",
		"SET synchronous_commit = 'local'",
	} {
		if _, err := c.Exec(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	defer func() {
		_, _ = c.Exec(context.WithoutCancel(ctx), "RESET ALL")
	}()
	var exists bool
	if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", AppUser).Scan(&exists); err != nil {
		return err
	}
	verb := "ALTER"
	if !exists {
		verb = "CREATE"
	}
	role := pgx.Identifier{AppUser}.Sanitize()
	q := fmt.Sprintf("%s ROLE %s WITH LOGIN NOSUPERUSER NOREPLICATION NOBYPASSRLS CREATEDB NOCREATEROLE PASSWORD %s", verb, role, quoteLiteral(password))
	if _, err := c.Exec(ctx, q); err != nil {
		return fmt.Errorf("%s role %s: %w", strings.ToLower(verb), AppUser, err)
	}
	if err := c.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", AppDatabase).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		// CREATE DATABASE cannot run in a transaction block.
		if _, err := c.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{AppDatabase}.Sanitize()+" OWNER "+role); err != nil {
			return fmt.Errorf("create database %s: %w", AppDatabase, err)
		}
	}
	return nil
}

// quoteLiteral quotes s as a standard SQL string literal.
func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ---- slots ----

// Slot is one physical replication slot.
type Slot struct {
	Name       string `json:"name"`
	Active     bool   `json:"active"`
	ActivePID  int    `json:"active_pid,omitempty"`
	RestartLSN *LSN   `json:"restart_lsn,omitempty"`
	WALStatus  string `json:"wal_status,omitempty"`
}

// ListSlots lists physical replication slots.
func ListSlots(ctx context.Context, c *pgx.Conn) ([]Slot, error) {
	rows, err := c.Query(ctx, `SELECT slot_name, active, coalesce(active_pid, 0), restart_lsn::text, coalesce(wal_status, '')
		FROM pg_replication_slots WHERE slot_type = 'physical' ORDER BY slot_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Slot
	for rows.Next() {
		var s Slot
		var restart *string
		if err := rows.Scan(&s.Name, &s.Active, &s.ActivePID, &restart, &s.WALStatus); err != nil {
			return nil, err
		}
		if restart != nil {
			l, err := ParseLSN(*restart)
			if err != nil {
				return nil, err
			}
			s.RestartLSN = &l
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CreateSlot makes sure a usable physical slot called name exists. An
// existing slot whose WAL was removed (wal_status "lost") is dropped and
// made again. It reports whether a slot was created.
func CreateSlot(ctx context.Context, c *pgx.Conn, name string) (bool, error) {
	slots, err := ListSlots(ctx, c)
	if err != nil {
		return false, err
	}
	for _, s := range slots {
		if s.Name != name {
			continue
		}
		if s.WALStatus != "lost" {
			return false, nil
		}
		if s.Active {
			return false, fmt.Errorf("slot %s lost its WAL but is still in use (pid %d)", name, s.ActivePID)
		}
		if err := DropSlot(ctx, c, name); err != nil {
			return false, err
		}
	}
	if _, err := c.Exec(ctx, "SELECT pg_create_physical_replication_slot($1, true)", name); err != nil {
		// Made by someone else since the list (another step of this
		// agent): fine, as long as it is usable.
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "42710" {
			slots, lerr := ListSlots(ctx, c)
			if lerr != nil {
				return false, lerr
			}
			for _, s := range slots {
				if s.Name == name && s.WALStatus != "lost" {
					return false, nil
				}
			}
		}
		return false, fmt.Errorf("create slot %s: %w", name, err)
	}
	return true, nil
}

// DropSlot drops a physical slot if it exists. An active slot is refused.
func DropSlot(ctx context.Context, c *pgx.Conn, name string) error {
	var active *bool
	err := c.QueryRow(ctx, "SELECT active FROM pg_replication_slots WHERE slot_name = $1", name).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if active != nil && *active {
		return fmt.Errorf("slot %s is in use", name)
	}
	if _, err := c.Exec(ctx, "SELECT pg_drop_replication_slot($1)", name); err != nil {
		return fmt.Errorf("drop slot %s: %w", name, err)
	}
	return nil
}
