package pg

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/durable"
)

// ---- archive settings (D63) ----

// ArchiveSettings is <data-dir>/backup/archive.json: what coyote.conf's
// archive_command and archive_timeout say. Written by the agent when the
// cluster's backup settings change; read by every config render.
type ArchiveSettings struct {
	Command string `json:"command,omitempty"` // "" = backups off: a no-op command
	Timeout string `json:"timeout,omitempty"` // e.g. "60s"; "" = 0 (off)
}

// archiveNoop is archive_command while backups are off. archive_mode is
// always on (changing it needs a restart), so the command must always
// succeed and must not depend on our program's path: a moved binary must
// never hold WAL back. "exit 0" works for sh and for cmd.exe.
const archiveNoop = "exit 0"

// ArchivePath is where ArchiveSettings live.
func (n Node) ArchivePath() string { return filepath.Join(n.StateDir, "backup", "archive.json") }

// ReadArchive reads archive.json (zero value if missing).
func (n Node) ReadArchive() (ArchiveSettings, error) {
	var a ArchiveSettings
	b, err := os.ReadFile(n.ArchivePath())
	if errors.Is(err, fs.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return a, fmt.Errorf("%s: %w", n.ArchivePath(), err)
	}
	return a, nil
}

// WriteArchive stores archive.json.
func (n Node) WriteArchive(a ArchiveSettings) error {
	if err := os.MkdirAll(filepath.Dir(n.ArchivePath()), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return durable.WriteFile(n.ArchivePath(), append(b, '\n'), 0o600)
}

// ArchiveCommandFor builds archive_command for exe and this node: Postgres
// runs it through sh (Unix) or cmd.exe (Windows) with %p and %f filled in.
func (n Node) ArchiveCommandFor(exe string) (string, error) {
	for _, p := range []string{exe, n.StateDir} {
		if strings.ContainsAny(p, "\"$`\n\r") {
			return "", fmt.Errorf("backups need folder and program paths without \" $ ` characters (got %q)", p)
		}
	}
	esc := func(s string) string { return strings.ReplaceAll(s, "%", "%%") }
	return fmt.Sprintf(`"%s" backup wal-push --data-dir "%s" "%%p" "%%f"`, esc(exe), esc(n.StateDir)), nil
}

// ReloadConfig re-renders coyote.conf (for example after archive.json
// changed) and reloads the running Postgres, under the config lock.
func (n Node) ReloadConfig(ctx context.Context) error {
	c, err := n.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	_, err = n.changeRepl(ctx, c, func(*ReplState) error { return nil })
	return err
}

// ArchiveLive is what the running Postgres itself says about WAL
// archiving: its live settings and pg_stat_archiver. A full backup checks
// it before it starts (pg_basebackup -X none waits for the archive).
type ArchiveLive struct {
	Mode           string // archive_mode
	Command        string // archive_command
	Library        string // archive_library
	InRecovery     bool   // a copy (standby)
	ArchivedCount  int64
	LastArchived   string
	LastArchivedAt time.Time
	FailedCount    int64
	LastFailed     string
	LastFailedAt   time.Time
}

// ReadArchiveLive asks this node's running Postgres.
func (n Node) ReadArchiveLive(ctx context.Context) (ArchiveLive, error) {
	c, err := n.Connect(ctx)
	if err != nil {
		return ArchiveLive{}, err
	}
	defer c.Close(context.Background())
	var l ArchiveLive
	var lastAt, failAt *time.Time
	err = c.QueryRow(ctx, `SELECT current_setting('archive_mode'), current_setting('archive_command'),
		current_setting('archive_library'), pg_is_in_recovery(),
		archived_count, coalesce(last_archived_wal, ''), last_archived_time,
		failed_count, coalesce(last_failed_wal, ''), last_failed_time
		FROM pg_stat_archiver`).Scan(&l.Mode, &l.Command, &l.Library, &l.InRecovery,
		&l.ArchivedCount, &l.LastArchived, &lastAt, &l.FailedCount, &l.LastFailed, &failAt)
	if err != nil {
		return ArchiveLive{}, fmt.Errorf("read the archive state: %w", err)
	}
	if lastAt != nil {
		l.LastArchivedAt = *lastAt
	}
	if failAt != nil {
		l.LastFailedAt = *failAt
	}
	return l, nil
}

// ---- WAL a copy replayed from its own pg_wal (never given an archive status) ----

// walSegSize is the WAL segment size coyote's initdb uses (the default).
const walSegSize = 16 << 20

// UnarchivedWAL is the result of AdoptUnarchivedWAL.
type UnarchivedWAL struct {
	Adopted []string // segments given a .ready file (Postgres archives them now)
	// Pending is a segment the copy is replaying from its own pg_wal and
	// has not passed yet: it cannot be judged complete, so it is left
	// alone for now ("" = none). A full backup must not start on it.
	Pending string
}

// AdoptUnarchivedWAL gives segments a copy holds but Postgres never marked
// for archiving a .ready file, so its archiver saves them.
//
// Why (D63): a clone streams the WAL of its base backup into pg_wal;
// pg_basebackup marks the segments it completed as done but leaves the
// last one with no status at all. The main node ends a base backup with a
// segment switch, so that last segment is complete; the new copy replays
// it from pg_wal and starts streaming at the next segment, and no part of
// Postgres marks it (only a later restartpoint that removes it would).
// It is the very segment a copy's first full backup starts in, so
// pg_basebackup waited on it for ever and the copy's own archive missed it.
//
// Only segments that are safe to archive under their full name are
// adopted: files of a full segment's size on the timeline being replayed,
// below the segment replay is in (so recovery read past their end). A
// segment that is still being received, a partial one at a timeline fork
// and recycled future segments are left alone.
func (n Node) AdoptUnarchivedWAL(ctx context.Context) (UnarchivedWAL, error) {
	c, err := n.Connect(ctx)
	if err != nil {
		return UnarchivedWAL{}, err
	}
	defer c.Close(context.Background())
	var inRecovery bool
	var replay, receive *string
	var recvTLI *int64
	var ckptTLI int64
	err = c.QueryRow(ctx, `SELECT pg_is_in_recovery(), pg_last_wal_replay_lsn()::text, pg_last_wal_receive_lsn()::text,
		(SELECT received_tli::bigint FROM pg_stat_wal_receiver WHERE status = 'streaming'),
		(SELECT timeline_id::bigint FROM pg_control_checkpoint())`).Scan(&inRecovery, &replay, &receive, &recvTLI, &ckptTLI)
	if err != nil {
		return UnarchivedWAL{}, fmt.Errorf("read the replay position: %w", err)
	}
	if !inRecovery || replay == nil {
		return UnarchivedWAL{}, nil // a main node marks its own segments
	}
	rp, err := ParseLSN(*replay)
	if err != nil {
		return UnarchivedWAL{}, err
	}
	w := walPosition{TLI: uint32(ckptTLI), ReplaySeg: uint64(rp) / walSegSize}
	if recvTLI != nil && receive != nil {
		if rl, err := ParseLSN(*receive); err == nil && rl > 0 {
			w.TLI, w.Receiving, w.ReceiveSeg = uint32(*recvTLI), true, (uint64(rl)-1)/walSegSize
		}
	}
	dir := filepath.Join(n.PGData(), "pg_wal")
	files, status, err := readWALDir(dir)
	if err != nil {
		return UnarchivedWAL{}, err
	}
	plan := planUnarchived(files, status, w)
	var out UnarchivedWAL
	out.Pending = plan.Pending
	for _, name := range plan.Adopt {
		sd := filepath.Join(dir, "archive_status")
		if _, err := os.Stat(filepath.Join(sd, name+".done")); err == nil {
			continue // Postgres got to it meanwhile
		}
		f, err := os.OpenFile(filepath.Join(sd, name+".ready"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return out, fmt.Errorf("mark %s for archiving: %w", name, err)
		}
		if err := f.Close(); err != nil {
			return out, fmt.Errorf("mark %s for archiving: %w", name, err)
		}
		out.Adopted = append(out.Adopted, name)
	}
	return out, nil
}

// walPosition is where a copy is: the timeline it replays and the
// segments it replays and receives.
type walPosition struct {
	TLI        uint32
	ReplaySeg  uint64
	Receiving  bool
	ReceiveSeg uint64 // the segment the last received byte is in
}

type walPlan struct {
	Adopt   []string
	Pending string
}

// walFileInfo is a pg_wal entry: its name and size.
type walFileInfo struct {
	Name string
	Size int64
}

// planUnarchived decides which status-less segments to adopt (see
// AdoptUnarchivedWAL). status holds the names with a .ready or .done file.
func planUnarchived(files []walFileInfo, status map[string]bool, w walPosition) walPlan {
	var p walPlan
	for _, f := range files {
		tli, seg, ok := parseSegmentName(f.Name)
		if !ok || status[f.Name] || tli != w.TLI || f.Size != walSegSize {
			continue
		}
		switch {
		case seg < w.ReplaySeg:
			p.Adopt = append(p.Adopt, f.Name)
		case seg == w.ReplaySeg && !(w.Receiving && w.ReceiveSeg == seg):
			p.Pending = f.Name // replayed from pg_wal, not passed yet
		}
	}
	slices.Sort(p.Adopt)
	return p
}

func readWALDir(dir string) ([]walFileInfo, map[string]bool, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var files []walFileInfo
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		if _, _, ok := parseSegmentName(e.Name()); !ok {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue // removed meanwhile (recycled)
		}
		files = append(files, walFileInfo{Name: e.Name(), Size: fi.Size()})
	}
	st, err := os.ReadDir(filepath.Join(dir, "archive_status"))
	if err != nil {
		return nil, nil, err
	}
	status := map[string]bool{}
	for _, e := range st {
		if name, ok := strings.CutSuffix(e.Name(), ".ready"); ok {
			status[name] = true
		} else if name, ok := strings.CutSuffix(e.Name(), ".done"); ok {
			status[name] = true
		}
	}
	return files, status, nil
}

// parseSegmentName parses a 24-hex-digit WAL segment file name.
func parseSegmentName(name string) (tli uint32, seg uint64, ok bool) {
	if len(name) != 24 {
		return 0, 0, false
	}
	t, err1 := strconv.ParseUint(name[:8], 16, 32)
	l, err2 := strconv.ParseUint(name[8:16], 16, 32)
	s, err3 := strconv.ParseUint(name[16:], 16, 32)
	if err1 != nil || err2 != nil || err3 != nil || s >= 0x100 || t == 0 {
		return 0, 0, false
	}
	return uint32(t), l*0x100 + s, true
}

// ---- full backup to a stream ----

// BaseBackupResult is what pg_basebackup reported.
type BaseBackupResult struct {
	StartLSN LSN
	EndLSN   LSN
	Timeline uint32
}

var reLabelStart = regexp.MustCompile(`(?m)^START WAL LOCATION: ([0-9A-Fa-f]+/[0-9A-Fa-f]+) \(file [0-9A-F]+\)$`)
var reLabelTLI = regexp.MustCompile(`(?m)^START TIMELINE: (\d+)$`)

// parseBackupLabel reads the start point and timeline from backup_label.
func parseBackupLabel(b []byte) (LSN, uint32, error) {
	m := reLabelStart.FindSubmatch(b)
	t := reLabelTLI.FindSubmatch(b)
	if m == nil || t == nil {
		return 0, 0, fmt.Errorf("backup_label without START WAL LOCATION / START TIMELINE:\n%s", b)
	}
	lsn, err := ParseLSN(string(m[1]))
	if err != nil {
		return 0, 0, err
	}
	tli, err := strconv.ParseUint(string(t[1]), 10, 32)
	if err != nil || tli == 0 {
		return 0, 0, fmt.Errorf("backup_label timeline %q", t[1])
	}
	return lsn, uint32(tli), nil
}

// labelSniffer passes a tar stream through and keeps the backup_label
// member. It never fails the stream: a parse problem shows up as "no
// label" at the end.
type labelSniffer struct {
	pw    *io.PipeWriter
	done  chan struct{}
	label []byte
	err   error
}

func newLabelSniffer() *labelSniffer {
	pr, pw := io.Pipe()
	s := &labelSniffer{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		tr := tar.NewReader(pr)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				s.err = err
				break
			}
			if h.Name == "backup_label" || h.Name == "./backup_label" {
				if s.label, err = io.ReadAll(io.LimitReader(tr, 1<<20)); err != nil {
					s.err = err
					break
				}
			}
		}
		// Keep draining so the writer side never blocks.
		_, _ = io.Copy(io.Discard, pr)
	}()
	return s
}

func (s *labelSniffer) Write(p []byte) (int, error) {
	_, _ = s.pw.Write(p)
	return len(p), nil
}

func (s *labelSniffer) finish() ([]byte, error) {
	_ = s.pw.Close()
	<-s.done
	return s.label, s.err
}

// BaseBackupOptions controls BaseBackupStream.
type BaseBackupOptions struct {
	Install Install
	Guard   *Guard
	Logger  *slog.Logger
	MaxRate string // pg_basebackup --max-rate, e.g. "64M"; "" = none
	// ArchiveWait bounds how long the server may wait, once the data is
	// sent, for the WAL the backup needs to be archived (pg_basebackup -X
	// none waits for that without limit). 0 = DefaultArchiveWait.
	ArchiveWait time.Duration
}

// DefaultArchiveWait is BaseBackupOptions.ArchiveWait's default. The main
// node's health record and archive_timeout (60 s by default) complete the
// last segment within about a minute; ten covers a slow destination.
const DefaultArchiveWait = 10 * time.Minute

// ErrArchiveWait: the backup's data was sent but the WAL it needs was not
// archived within ArchiveWait.
var ErrArchiveWait = errors.New("the database did not finish saving the changes (WAL) this backup needs to the backup place in time; WAL archiving on this node is stuck or failing (see `coyote backup`)")

// archiveWaitLine is the server's NOTICE when the data is sent and it
// starts waiting for the archive (C locale, see toolEnv).
const archiveWaitLine = "waiting for required WAL segments to be archived"

// BaseBackupStream runs pg_basebackup against this node's own Postgres
// (primary or standby) as the replication role and writes the tar stream
// of the data folder to w. No WAL is included (-X none): the WAL archive
// covers start to end (D63). Tablespaces are not supported (a stream
// holds one).
func (n Node) BaseBackupStream(ctx context.Context, o BaseBackupOptions, w io.Writer) (BaseBackupResult, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Guard == nil {
		return BaseBackupResult{}, errors.New("refusing to run pg_basebackup without a process guard (safety rule 5)")
	}
	info, err := n.ReadInfo()
	if err != nil {
		return BaseBackupResult{}, err
	}
	s, err := n.Secrets()
	if err != nil {
		return BaseBackupResult{}, err
	}
	dir := filepath.Join(n.StateDir, "backup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return BaseBackupResult{}, err
	}
	pass := filepath.Join(dir, "local.pgpass")
	port := strconv.Itoa(info.Port)
	if err := writeFileAtomic(pass, []byte(RenderPassfile([]PassEntry{
		{"127.0.0.1", port, "replication", ReplUser, s.Replication},
		{"127.0.0.1", port, "*", ReplUser, s.Replication},
	})), 0o600); err != nil {
		return BaseBackupResult{}, err
	}
	args := []string{
		"--pgdata", "-",
		"--format", "tar",
		"--wal-method", "none",
		"--checkpoint", "fast",
		"--no-manifest",
		"--verbose",
		"--no-password",
		"--dbname", fmt.Sprintf("host=127.0.0.1 port=%s user=%s application_name=%s-backup sslmode=disable", port, ReplUser, brand.Command),
	}
	if o.MaxRate != "" {
		args = append(args, "--max-rate", o.MaxRate)
	}
	wait := o.ArchiveWait
	if wait <= 0 {
		wait = DefaultArchiveWait
	}
	tctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var (
		waitMu    sync.Mutex
		waitTimer *time.Timer
	)
	defer func() {
		waitMu.Lock()
		if waitTimer != nil {
			waitTimer.Stop()
		}
		waitMu.Unlock()
	}()
	onLine := func(line string) {
		if !strings.Contains(line, archiveWaitLine) {
			return
		}
		waitMu.Lock()
		if waitTimer == nil {
			waitTimer = time.AfterFunc(wait, func() { cancel(ErrArchiveWait) })
		}
		waitMu.Unlock()
	}
	var res BaseBackupResult
	sniff := newLabelSniffer()
	out, err := runToolStream(tctx, o.Guard, o.Logger, toolSpec{
		Name:    "pg_basebackup (backup)",
		Bin:     o.Install.Bin("pg_basebackup"),
		Args:    args,
		Env:     toolEnv("PGPASSFILE=" + pass),
		LogFile: filepath.Join(n.LogDir(), "backup.log"),
		OnLine:  onLine,
	}, nil, io.MultiWriter(w, sniff))
	label, lerr := sniff.finish()
	if err != nil {
		if cause := context.Cause(tctx); errors.Is(cause, ErrArchiveWait) && ctx.Err() == nil {
			return res, fmt.Errorf("%w (waited %s)", ErrArchiveWait, wait)
		}
		if ctx.Err() != nil {
			return res, err
		}
		return res, fmt.Errorf("%w\n%s", err, out)
	}
	if label == nil {
		return res, fmt.Errorf("the backup stream has no backup_label (%v)", lerr)
	}
	if res.StartLSN, res.Timeline, err = parseBackupLabel(label); err != nil {
		return res, err
	}
	// With -X none pg_basebackup does not print its end point. The
	// server's position now is at or after it: waiting for WAL up to
	// here before calling the backup usable is safe, only a little later.
	c, err := n.Connect(ctx)
	if err != nil {
		return res, fmt.Errorf("read the end point: %w", err)
	}
	defer c.Close(context.Background())
	var end string
	if err := c.QueryRow(ctx, `SELECT CASE WHEN pg_is_in_recovery() THEN greatest(pg_last_wal_replay_lsn(), pg_last_wal_receive_lsn())
		ELSE pg_current_wal_insert_lsn() END::text`).Scan(&end); err != nil {
		return res, fmt.Errorf("read the end point: %w", err)
	}
	if res.EndLSN, err = ParseLSN(end); err != nil {
		return res, err
	}
	if res.EndLSN < res.StartLSN {
		return res, fmt.Errorf("end point %s before start %s", res.EndLSN, res.StartLSN)
	}
	return res, nil
}

// runToolStream is runTool with stdin and stdout of the caller's choosing
// (stderr still goes to the log and OnLine).
func runToolStream(ctx context.Context, g *Guard, log *slog.Logger, sp toolSpec, stdin io.Reader, stdout io.Writer) (string, error) {
	if g == nil {
		return "", fmt.Errorf("refusing to run %s without a process guard (safety rule 5)", sp.Name)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var lf *os.File
	if sp.LogFile != "" {
		f, err := os.OpenFile(sp.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return "", err
		}
		defer f.Close()
		lf = f
	}
	errOut := &lineLog{log: log, tool: sp.Name, raw: lf, onLine: sp.OnLine}
	cmd := exec.Command(sp.Bin, sp.Args...)
	cmd.Env = sp.Env
	cmd.Stdin = stdin
	if stdout != nil {
		cmd.Stdout = stdout
	} else {
		cmd.Stdout = errOut
	}
	cmd.Stderr = errOut
	g.prepareTool(cmd)
	log.Info("running "+sp.Name, "bin", sp.Bin)
	waitCh, err := startProcess(cmd)
	if err != nil {
		return "", fmt.Errorf("start %s: %w", sp.Name, err)
	}
	pid := cmd.Process.Pid
	if err := g.trackTool(pid); err != nil {
		killTool(cmd.Process)
		<-waitCh
		return "", fmt.Errorf("register %s with guard: %w", sp.Name, err)
	}
	defer g.untrackTool(pid)
	var werr error
	select {
	case werr = <-waitCh:
	case <-ctx.Done():
		killTool(cmd.Process)
		<-waitCh
		errOut.flush()
		return errOut.tail(), ctx.Err()
	}
	errOut.flush()
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			return errOut.tail(), fmt.Errorf("%s failed (%s)", sp.Name, ee.ProcessState.String())
		}
		return errOut.tail(), fmt.Errorf("%s: %w", sp.Name, werr)
	}
	return errOut.tail(), nil
}

// ---- restore into a new folder ----

// RecoverySettings are what a restore adds to coyote.conf.
type RecoverySettings struct {
	RestoreCommand string
	TargetTime     string // "" = replay everything in the archive
}

// WriteRecoveryConfig prepares a restored data folder for archive
// recovery: coyote.conf for this folder (local port, no TLS, no
// replication, archiving off), a pg_hba.conf that refuses every
// connection, and recovery.signal. Nobody can connect while it recovers.
func (n Node) WriteRecoveryConfig(port int, r RecoverySettings) error {
	if err := os.MkdirAll(n.LogDir(), 0o700); err != nil {
		return err
	}
	conf := RenderConf(Config{Port: port, LogDir: n.LogDir()})
	var b strings.Builder
	b.WriteString(conf)
	w := func(k, v string) { fmt.Fprintf(&b, "%s = %s\n", k, v) }
	b.WriteString("\n# Restore (D63): never archive into the source's backups\n")
	w("archive_mode", "off")
	w("restore_command", quoteConf(r.RestoreCommand))
	w("recovery_target_timeline", quoteConf("latest"))
	w("recovery_target_action", quoteConf("promote"))
	if r.TargetTime != "" {
		w("recovery_target_time", quoteConf(r.TargetTime))
		w("recovery_target_inclusive", "on")
	}
	enc, err := encodeConfFile(b.String())
	if err != nil {
		return err
	}
	pgdata := n.PGData()
	if err := writeFileAtomic(filepath.Join(pgdata, AgentConfFile), enc, 0o600); err != nil {
		return err
	}
	hba := "# Restore in progress: no connections.\nhost all all 0.0.0.0/0 reject\nhost all all ::/0 reject\n"
	if err := writeFileAtomic(filepath.Join(pgdata, "pg_hba.conf"), []byte(hba), 0o600); err != nil {
		return err
	}
	if err := stripAutoConf(pgdata); err != nil {
		return err
	}
	if err := ensureInclude(filepath.Join(pgdata, "postgresql.conf")); err != nil {
		return err
	}
	for _, f := range []string{standbySignal, "postmaster.pid", "postmaster.opts", "coyote_page_damage"} {
		if err := os.Remove(filepath.Join(pgdata, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return writeFileAtomic(filepath.Join(pgdata, "recovery.signal"), nil, 0o600)
}

// WriteNodeConfig writes the normal agent-owned config (coyote.conf with
// archiving a no-op, the usual pg_hba.conf) for a restored folder that is
// not running.
func (n Node) WriteNodeConfig(port int) error {
	return WriteConfig(n.PGData(), Config{Port: port, LogDir: n.LogDir()})
}

// StartRecovery starts Postgres on a prepared restore folder under the
// guard. It does not wait: follow it with WaitPromoted.
func (n Node) StartRecovery(ctx context.Context, inst Install, g *Guard, log *slog.Logger, port int) (*Server, error) {
	if g == nil {
		return nil, errors.New("refusing to start Postgres without a process guard (safety rule 5)")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ef, err := os.OpenFile(filepath.Join(n.LogDir(), "postgres-stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(inst.Bin("postgres"),
		"-c", "listen_addresses=127.0.0.1",
		"-c", "port="+strconv.Itoa(port),
		"-D", n.PGData())
	cmd.Dir = n.StateDir
	cmd.Env = cleanEnv()
	cmd.Stdout = ef
	cmd.Stderr = ef
	g.prepare(cmd)
	s := &Server{node: n, inst: inst, port: port, log: log, cmd: cmd, stderr: ef, done: make(chan struct{})}
	waitCh, err := startProcess(cmd)
	if err != nil {
		ef.Close()
		return nil, fmt.Errorf("start postgres: %w", err)
	}
	go func() {
		s.waitErr = <-waitCh
		ef.Close()
		close(s.done)
	}()
	if err := g.started(cmd.Process.Pid); err != nil {
		_ = s.Stop(guardStopTimeouts)
		return nil, fmt.Errorf("register postgres with guard: %w", err)
	}
	return s, nil
}

// WaitPromoted waits until a recovering server has finished recovery and
// accepts connections as a primary (postmaster.pid says "ready"), or
// exits. onTick, if set, is called every second (progress).
func (s *Server) WaitPromoted(ctx context.Context, onTick func()) error {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-s.done:
			return exitedDuringStartup(s.waitErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		if onTick != nil && time.Since(last) >= time.Second {
			last = time.Now()
			onTick()
		}
		// "ready" alone also means a read-only server still in recovery
		// (hot standby); Postgres removes recovery.signal when archive
		// recovery has ended and it was promoted.
		pf, err := ReadPidFile(s.node.PGData())
		if err == nil && pf != nil && pf.PID == s.PID() && pf.Status == "ready" {
			if _, err := os.Stat(filepath.Join(s.node.PGData(), "recovery.signal")); errors.Is(err, os.ErrNotExist) {
				return nil
			}
		}
	}
}

// StopFast stops a server started by StartRecovery.
func (s *Server) StopFast() error { return s.Stop(guardStopTimeouts) }

// ServerLog returns the text of the newest Postgres log in this folder.
func (n Node) ServerLog() string {
	p := newestServerLog(n.LogDir())
	if p == "" {
		return ""
	}
	return readFrom(p, 0)
}

// SetSuperuserPassword sets the superuser's password in single-user mode
// on a cleanly stopped data folder (no connection, no authentication; the
// password reaches Postgres on standard input only) and writes it to the
// node's password file.
func (n Node) SetSuperuserPassword(ctx context.Context, inst Install, g *Guard, log *slog.Logger, pw string) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if strings.ContainsAny(pw, "\n\r") {
		return errors.New("bad password")
	}
	sql := "ALTER ROLE " + Superuser + " WITH PASSWORD " + quoteLiteral(pw) + ";\n"
	out, err := runToolStream(ctx, g, log, toolSpec{
		Name: "postgres --single (set password)",
		Bin:  inst.Bin("postgres"),
		Args: []string{"--single", "-D", n.PGData(),
			"-c", "archive_mode=off", "-c", "log_statement=none", "-c", "restore_command=",
			"postgres"},
		Env: toolEnv(),
	}, strings.NewReader(sql), io.Discard)
	if err != nil {
		return fmt.Errorf("set the superuser password: %w\n%s", err, out)
	}
	return writeFileAtomic(n.PasswordFile(), []byte(pw+"\n"), 0o600)
}

// RecoveryLogFacts pulls what a restore needs to tell people from the
// server log: where recovery stopped, and the error if it failed.
type RecoveryLogFacts struct {
	StoppedAt        string // "recovery stopping before commit of transaction 123, time ..."
	LastTxTime       string // "last completed transaction was at log time ..."
	BeforeConsistent bool
	EndedEarly       bool // target was after the end of the archive
	Fatal            string
}

// ParseRecoveryLog reads a Postgres server log written during recovery.
func ParseRecoveryLog(s string) RecoveryLogFacts {
	var f RecoveryLogFacts
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.Contains(l, "recovery stopping"):
			f.StoppedAt = l[strings.Index(l, "recovery stopping"):]
		case strings.Contains(l, "last completed transaction was at log time"):
			i := strings.Index(l, "log time ")
			f.LastTxTime = strings.TrimSpace(l[i+len("log time "):])
		case strings.Contains(l, "before consistent recovery point"):
			f.BeforeConsistent = true
		case strings.Contains(l, "recovery ended before configured recovery target was reached"):
			f.EndedEarly = true
		}
		if strings.Contains(l, "FATAL:") || strings.Contains(l, "PANIC:") {
			f.Fatal = strings.TrimSpace(l[strings.Index(l, ":")+1:])
			if i := strings.Index(l, "FATAL:"); i >= 0 {
				f.Fatal = strings.TrimSpace(l[i:])
			}
		}
	}
	return f
}
