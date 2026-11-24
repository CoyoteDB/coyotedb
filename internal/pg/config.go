package pg

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/durable"
)

// AgentConfFile is the agent-owned include file inside the data dir.
const AgentConfFile = "coyote.conf"

// includeLine is the only line we ever add to postgresql.conf.
const includeLine = "include = '" + AgentConfFile + "'"

// Config is the agent-owned Postgres configuration.
type Config struct {
	Port   int
	LogDir string // absolute path; Postgres server logs go here

	// PrimaryConninfo and PrimarySlot make a standby stream (empty: none).
	PrimaryConninfo string
	PrimarySlot     string
	// SyncStandbyNames is synchronous_standby_names (see RenderSyncNames).
	SyncStandbyNames string
	// MaxSlotWALKeep caps WAL kept for slots (safety rule 8). Default
	// DefaultMaxSlotWALKeep.
	MaxSlotWALKeep string
	// TLSCert and TLSKey (absolute paths, outside pgdata so they never
	// travel with a base backup) turn on TLS for client connections. Both
	// empty: ssl=off.
	TLSCert, TLSKey string
	// Archive is the WAL archive setup (D63): archive_mode is always on,
	// the command is a no-op while backups are off.
	Archive ArchiveSettings
}

// WAL retention defaults. wal_keep_size keeps recent WAL around for
// rewinds and short standby outages even without a slot (rule 7);
// max_slot_wal_keep_size stops a dead member's slot from filling the disk
// (rule 8): past it the slot is invalidated and the member needs a re-clone.
const (
	DefaultWALKeep        = "512MB"
	DefaultMaxSlotWALKeep = "8GB"
)

// DefaultPort is where the real Postgres listens (ARCHITECTURE.md, Ports).
const DefaultPort = 7432

// RenderConf renders coyote.conf. It is rewritten on every start, so
// anything in it wins over postgresql.conf (the include is the last line).
//
// Settings here back safety rules 7 (rewind preconditions) and 11 (Postgres
// listens on 127.0.0.1 only).
func RenderConf(c Config) string {
	var b strings.Builder
	w := func(k, v string) { fmt.Fprintf(&b, "%s = %s\n", k, v) }
	fmt.Fprintf(&b, "# Managed by %s. Do not edit: this file is rewritten on every start.\n\n", brand.Command)

	b.WriteString("# Connections (safety rule 11: local only)\n")
	w("listen_addresses", quoteConf("127.0.0.1"))
	w("port", fmt.Sprint(c.Port))
	// No Unix sockets: TCP on 127.0.0.1 is the one way in, the same on every
	// OS, and long data-dir paths cannot break the socket path limit.
	w("unix_socket_directories", quoteConf(""))
	w("password_encryption", quoteConf("scram-sha-256"))
	// TLS for clients (they come through the proxy, which passes TLS
	// through untouched). The agent's own connections stay on plain TCP on
	// 127.0.0.1; pg_hba tells the two apart by TLS.
	if c.TLSCert != "" && c.TLSKey != "" {
		w("ssl", "on")
	} else {
		w("ssl", "off")
	}
	w("ssl_cert_file", quoteConf(confPath(c.TLSCert)))
	w("ssl_key_file", quoteConf(confPath(c.TLSKey)))
	w("ssl_min_protocol_version", quoteConf("TLSv1.2"))

	b.WriteString("\n# Replication and rewind (safety rule 7)\n")
	w("wal_level", "replica")
	w("wal_log_hints", "on")
	w("full_page_writes", "on")
	w("max_wal_senders", "10")
	w("max_replication_slots", "10")
	w("hot_standby", "on")
	w("wal_keep_size", quoteConf(DefaultWALKeep))
	maxKeep := c.MaxSlotWALKeep
	if maxKeep == "" {
		maxKeep = DefaultMaxSlotWALKeep
	}
	w("max_slot_wal_keep_size", quoteConf(maxKeep))

	b.WriteString("\n# Backups (D63): every data node archives what it has\n")
	w("archive_mode", "always")
	cmd := c.Archive.Command
	if cmd == "" {
		cmd = archiveNoop
	}
	w("archive_command", quoteConf(cmd))
	timeout := c.Archive.Timeout
	if timeout == "" || c.Archive.Command == "" {
		timeout = "0"
	}
	w("archive_timeout", quoteConf(timeout))

	b.WriteString("\n# This machine (never inherited from the node a copy was made from:\n")
	b.WriteString("# postgresql.conf travels with base backups and rewinds, this file wins)\n")
	dsm, sync := platformSettings(runtime.GOOS)
	w("dynamic_shared_memory_type", dsm)
	// The default on macOS and Windows does not force the drive's own cache
	// to write: a power cut could lose a commit that was acknowledged.
	w("wal_sync_method", sync)
	w("jit", "off")

	b.WriteString("\n# Standby and sync settings (from replication.json; reloaded on change)\n")
	w("recovery_target_timeline", quoteConf("latest"))
	w("primary_conninfo", quoteConf(c.PrimaryConninfo))
	w("primary_slot_name", quoteConf(c.PrimarySlot))
	w("synchronous_standby_names", quoteConf(c.SyncStandbyNames))
	// Pinned at Postgres' defaults so no inherited postgresql.conf can turn
	// them off (0): a silent stream ends after 60 s, and a WAL receiver
	// that stopped is started again 5 s later (D57).
	w("wal_receiver_timeout", quoteConf("60s"))
	w("wal_sender_timeout", quoteConf("60s"))
	w("wal_retrieve_retry_interval", quoteConf("5s"))

	b.WriteString("\n# Logging\n")
	w("logging_collector", "on")
	w("log_directory", quoteConf(confPath(c.LogDir)))
	w("log_filename", quoteConf("postgresql-%Y-%m-%d.log"))
	w("log_file_mode", "0600")
	w("log_rotation_age", quoteConf("1d"))
	w("log_rotation_size", quoteConf("50MB"))
	w("log_truncate_on_rotation", "off")
	w("log_line_prefix", quoteConf("%m [%p] %q%u@%d "))
	w("log_checkpoints", "on")
	return b.String()
}

// platformSettings returns dynamic_shared_memory_type and wal_sync_method
// for an operating system. Linux keeps its own default (fdatasync), set
// explicitly so a value from another machine can never be inherited.
//
// Windows: the pinned PostgreSQL 18 build offers only fsync, fdatasync and
// open_datasync (fsync_writethrough is rejected; found on the first
// Windows test run). Its default, open_datasync, relies
// on write-through, which many consumer drives do not honour. fsync is
// _commit(), i.e. FlushFileBuffers, which makes the drive flush its cache:
// the same strict flush fsync_writethrough gives on macOS.
func platformSettings(goos string) (dsm, walSync string) {
	switch goos {
	case "windows":
		return "windows", "fsync"
	case "darwin":
		return "posix", "fsync_writethrough"
	case "linux":
		return "posix", "fdatasync"
	default:
		return "posix", "fsync"
	}
}

// InternalRoles are the roles only the agent uses. They log in over plain
// TCP from 127.0.0.1 (the agent, and the tunnel on behalf of other nodes)
// and never over TLS, which is the only way clients reach Postgres (the
// proxy refuses a client that does not start TLS).
var InternalRoles = []string{Superuser, ReplUser, RewindUser, HealthUser}

// RenderHBA renders pg_hba.conf. The agent owns it entirely; first match
// wins:
//
//   - plain TCP from 127.0.0.1: internal roles only (scram), everything
//     else rejected. This is the agent itself and the tunnel (replication,
//     base backups and rewinds for other nodes), plus whoever on this
//     machine can read cluster.secrets.
//   - TLS from 127.0.0.1: internal roles rejected, every other role with
//     scram. This is every client, local or remote, through the proxy.
func RenderHBA() string {
	var b strings.Builder
	internal := strings.Join(InternalRoles, ",")
	fmt.Fprintf(&b, "# Managed by %s. Do not edit: this file is rewritten on every start.\n", brand.Command)
	b.WriteString("# TYPE    DATABASE     USER  ADDRESS        METHOD\n")
	b.WriteString("hostnossl replication  " + ReplUser + "  127.0.0.1/32   scram-sha-256\n")
	b.WriteString("hostnossl all          " + internal + "  127.0.0.1/32   scram-sha-256\n")
	b.WriteString("hostnossl all          all   127.0.0.1/32   reject\n")
	b.WriteString("hostssl   all          " + internal + "  127.0.0.1/32   reject\n")
	b.WriteString("hostssl   replication  all   127.0.0.1/32   reject\n")
	b.WriteString("hostssl   all          all   127.0.0.1/32   scram-sha-256\n")
	return b.String()
}

// quoteConf quotes a postgresql.conf string value. Inside single quotes,
// Postgres treats backslash as an escape and two single quotes stand for one.
func quoteConf(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return "'" + s + "'"
}

// confPath makes a path safe for postgresql.conf. Postgres accepts forward
// slashes on Windows, which avoids backslash escaping surprises.
func confPath(p string) string { return filepath.ToSlash(p) }

// WriteConfig writes coyote.conf and pg_hba.conf and makes sure
// postgresql.conf includes coyote.conf as its final directive.
func WriteConfig(pgdata string, c Config) error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("invalid port %d", c.Port)
	}
	if !filepath.IsAbs(c.LogDir) {
		return fmt.Errorf("log dir must be absolute: %q", c.LogDir)
	}
	conf, err := encodeConfFile(RenderConf(c)) // Windows: ANSI code page (confenc_windows.go)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(pgdata, AgentConfFile), conf, 0o600); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(pgdata, "pg_hba.conf"), []byte(RenderHBA()), 0o600); err != nil {
		return err
	}
	if err := stripAutoConf(pgdata); err != nil {
		return err
	}
	return ensureInclude(filepath.Join(pgdata, "postgresql.conf"))
}

// autoConfFile is where ALTER SYSTEM writes. Postgres reads it after
// postgresql.conf and its includes, so anything in it beats coyote.conf.
const autoConfFile = "postgresql.auto.conf"

// alwaysStrip are settings removed from postgresql.auto.conf even though
// coyote.conf does not set them: a stale value from a base backup, pg_rewind
// or `pg_basebackup -R` must never point a standby somewhere else or
// change how recovery ends.
var alwaysStrip = []string{
	"restore_command", "archive_cleanup_command", "recovery_end_command",
	"recovery_target", "recovery_target_lsn", "recovery_target_name",
	"recovery_target_time", "recovery_target_xid", "recovery_target_inclusive",
	"recovery_target_action", "recovery_min_apply_delay", "primary_conninfo",
	"primary_slot_name", "synchronous_standby_names", "synchronous_commit",
}

// ownedKeys returns every key that coyote.conf sets plus alwaysStrip,
// lower-cased.
func ownedKeys() map[string]bool {
	keys := map[string]bool{}
	for _, l := range strings.Split(RenderConf(Config{Port: 1, LogDir: "/"}), "\n") {
		if k := confKey(l); k != "" {
			keys[k] = true
		}
	}
	for _, k := range alwaysStrip {
		keys[k] = true
	}
	return keys
}

// confKey returns the lower-cased setting name of a config line, or "".
func confKey(line string) string {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return ""
	}
	end := strings.IndexFunc(t, func(r rune) bool {
		return !(r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
	// "name = value" or "name value" (the '=' is optional in Postgres).
	if end <= 0 {
		return ""
	}
	if c := t[end]; c != '=' && c != ' ' && c != '\t' {
		return ""
	}
	return strings.ToLower(t[:end])
}

// StripAutoConf removes agent-owned settings from postgresql.auto.conf
// content. Other lines are kept as they are.
func StripAutoConf(conf string) (string, bool) {
	owned := ownedKeys()
	var out []string
	changed := false
	for _, l := range strings.SplitAfter(conf, "\n") {
		if k := confKey(l); k != "" && owned[k] {
			changed = true
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, ""), changed
}

// stripAutoConf rewrites pgdata/postgresql.auto.conf without agent-owned
// settings (no-op if the file is missing or clean).
func stripAutoConf(pgdata string) error {
	p := filepath.Join(pgdata, autoConfFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	s, changed := StripAutoConf(string(b))
	if !changed {
		return nil
	}
	return writeFileAtomic(p, []byte(s), 0o600)
}

// ensureInclude appends the include line to postgresql.conf if it is not
// already the last directive. Nothing else in the file is touched.
func ensureInclude(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read postgresql.conf: %w", err)
	}
	updated, changed := withInclude(b)
	if !changed {
		return nil
	}
	return writeFileAtomic(path, updated, 0o600)
}

// withInclude returns conf with the include line as the last directive.
func withInclude(conf []byte) ([]byte, bool) {
	lines := strings.Split(strings.ReplaceAll(string(conf), "\r\n", "\n"), "\n")
	last := ""
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		last = t
	}
	if last == includeLine {
		return conf, false
	}
	out := bytes.TrimRight(conf, "\r\n")
	out = append(out, []byte(fmt.Sprintf("\n\n# Added by %s: agent-owned settings, keep this last.\n%s\n", brand.Command, includeLine))...)
	return out, true
}

// WriteFileDurable replaces path durably: temp file, fsync, rename, fsync
// of the directory.
func WriteFileDurable(path string, data []byte, perm fs.FileMode) error {
	return durable.WriteFile(path, data, perm)
}

// writeFileAtomic replaces path durably (see package durable).
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return durable.WriteFile(path, data, perm)
}

// syncDir makes entries created, renamed or removed in dir durable.
func syncDir(dir string) error { return durable.SyncDir(dir) }
