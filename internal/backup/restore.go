package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/pg"
)

// RestoreOptions controls Restore.
type RestoreOptions struct {
	Config  Config // where the backups are (and the key)
	Into    string // a new or empty folder; never a live node's folder
	Target  time.Time
	Install pg.Install
	Exe     string // this program, for restore_command
	Port    int    // Postgres port during the restore (0 = a free one)
	Logger  *slog.Logger
	Say     func(string) // progress for people
	Timeout time.Duration
}

// RestoreResult says what was restored.
type RestoreResult struct {
	Backup   Info
	Dir      string
	Port     int
	StopLine string // Postgres' "recovery stopping before ..." line, if any
	LastTx   string // time of the last transaction replayed
}

// ErrNotRestorable: no backup can reach the target.
var ErrNotRestorable = errors.New("no backup can restore to that time")

// Restore restores the newest backup ending before o.Target (or the newest,
// replaying all archived WAL) into o.Into. The live cluster, its folders
// and its backups are never written (D63).
func Restore(ctx context.Context, o RestoreOptions) (RestoreResult, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	say := o.Say
	if say == nil {
		say = func(string) {}
	}
	dir, err := filepath.Abs(o.Into)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := emptyNewDir(dir); err != nil {
		return RestoreResult{}, err
	}
	st, err := o.Config.Store()
	if err != nil {
		return RestoreResult{}, err
	}
	codec, err := o.Config.Codec()
	if err != nil {
		return RestoreResult{}, err
	}
	if id, err := ReadIdentity(ctx, st); err == nil && id.KeyID != "" && id.KeyID != KeyID(codec.Key) {
		return RestoreResult{}, fmt.Errorf("these backups were written with a different backup key: %w", ErrWrongKey)
	}
	cat, err := LoadCatalog(ctx, st)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("read the backup list from %s: %w", st.Label(), err)
	}
	var cands []Info
	for i := len(cat.Backups) - 1; i >= 0; i-- {
		b := cat.Backups[i]
		if !cat.Usable(b) {
			continue
		}
		if !o.Target.IsZero() && !b.FinishedAt.Before(o.Target) {
			continue
		}
		cands = append(cands, b)
	}
	if len(cands) == 0 {
		w := cat.RestoreWindow()
		if w.From.IsZero() {
			return RestoreResult{}, fmt.Errorf("%w: %s holds no finished full backup yet", ErrNotRestorable, st.Label())
		}
		return RestoreResult{}, fmt.Errorf("%w: the oldest moment these backups reach is %s", ErrNotRestorable, w.From.Local().Format(time.DateTime))
	}

	n, err := pg.NewNode(dir)
	if err != nil {
		return RestoreResult{}, err
	}
	if err := n.Prepare(); err != nil {
		return RestoreResult{}, err
	}
	lctx, cancel := context.WithTimeout(ctx, time.Second)
	lock, err := pg.AcquireLock(lctx, n.LockPath(), nil)
	cancel()
	if err != nil {
		return RestoreResult{}, fmt.Errorf("%s is in use by another program: %w", dir, err)
	}
	defer lock.Unlock()
	g, err := pg.NewGuard(n, lock, o.Logger)
	if err != nil {
		return RestoreResult{}, err
	}
	defer g.Close()

	port := o.Port
	if port == 0 {
		if port, err = freePort(); err != nil {
			return RestoreResult{}, err
		}
	}
	// wal-fetch reads where the backups are from this file (0600), which
	// is removed when the restore ends.
	srcFile := filepath.Join(StateDir(dir), "source.json")
	if err := WriteConfig(srcFile, o.Config); err != nil {
		return RestoreResult{}, err
	}
	defer os.Remove(srcFile)
	restoreCmd, err := fetchCommand(o.Exe, srcFile)
	if err != nil {
		return RestoreResult{}, err
	}
	target := ""
	if !o.Target.IsZero() {
		target = o.Target.UTC().Format("2006-01-02 15:04:05.999999") + "+00"
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 24 * time.Hour
	}
	rctx, rcancel := context.WithTimeout(ctx, timeout)
	defer rcancel()

	var lastErr error
	for i, b := range cands {
		if i == 3 {
			break
		}
		say(fmt.Sprintf("Unpacking the full backup from %s (%s, node %s)...", b.FinishedAt.Local().Format(time.DateTime), humanBytes(b.DataSize), b.Node))
		if err := os.RemoveAll(n.PGData()); err != nil {
			return RestoreResult{}, err
		}
		if err := unpack(rctx, st, codec, b, n.PGData()); err != nil {
			return RestoreResult{}, err
		}
		if err := n.WriteRecoveryConfig(port, pg.RecoverySettings{RestoreCommand: restoreCmd, TargetTime: target}); err != nil {
			return RestoreResult{}, err
		}
		say("Replaying saved changes" + map[bool]string{true: " up to " + o.Target.Local().Format(time.DateTime), false: ""}[target != ""] + "...")
		logs, _ := os.ReadDir(n.LogDir())
		for _, l := range logs { // a retry reads only its own log
			if strings.HasPrefix(l.Name(), "postgresql-") {
				_ = os.Remove(filepath.Join(n.LogDir(), l.Name()))
			}
		}
		srv, err := n.StartRecovery(rctx, o.Install, g, o.Logger, port)
		if err != nil {
			return RestoreResult{}, err
		}
		werr := srv.WaitPromoted(rctx, nil)
		facts := pg.ParseRecoveryLog(n.ServerLog())
		if werr == nil {
			if err := srv.StopFast(); err != nil {
				return RestoreResult{}, err
			}
			pw, err := pg.GeneratePassword()
			if err != nil {
				return RestoreResult{}, err
			}
			if err := n.SetSuperuserPassword(ctx, o.Install, g, o.Logger, pw); err != nil {
				return RestoreResult{}, err
			}
			if err := n.WriteNodeConfig(port); err != nil {
				return RestoreResult{}, err
			}
			if err := n.WriteInfo(pg.NodeInfo{Port: port, PGVersion: o.Install.Build.Version, Target: o.Install.Build.Target}); err != nil {
				return RestoreResult{}, err
			}
			return RestoreResult{Backup: b, Dir: dir, Port: port, StopLine: facts.StoppedAt, LastTx: facts.LastTxTime}, nil
		}
		_ = srv.StopFast()
		switch {
		case facts.BeforeConsistent:
			lastErr = fmt.Errorf("%w: the backup from %s ends after that time", ErrNotRestorable, b.FinishedAt.Local().Format(time.DateTime))
			say("That backup ends after the chosen time; trying an older one.")
			continue
		case facts.EndedEarly:
			w := cat.RestoreWindow()
			return RestoreResult{}, fmt.Errorf("%w: the saved changes end before that time (the newest restorable moment is about %s)", ErrNotRestorable, w.To.Local().Format(time.DateTime))
		case facts.Fatal != "":
			return RestoreResult{}, fmt.Errorf("the restore failed: %s (log: %s)", facts.Fatal, n.LogDir())
		}
		return RestoreResult{}, fmt.Errorf("the restore failed: %w (log: %s)", werr, n.LogDir())
	}
	return RestoreResult{}, lastErr
}

func fetchCommand(exe, src string) (string, error) {
	for _, p := range []string{exe, src} {
		if p == "" || strings.ContainsAny(p, "\"$`\n\r") {
			return "", fmt.Errorf("restore needs folder and program paths without \" $ ` characters (got %q)", p)
		}
	}
	esc := func(s string) string { return strings.ReplaceAll(s, "%", "%%") }
	return fmt.Sprintf(`"%s" backup wal-fetch --source "%s" "%%f" "%%p"`, esc(exe), esc(src)), nil
}

// emptyNewDir accepts a folder that does not exist (it is created) or is
// empty. Anything else (a live node, an old restore) is refused.
func emptyNewDir(dir string) error {
	ents, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.MkdirAll(dir, 0o700)
	case err != nil:
		return err
	case len(ents) > 0:
		return fmt.Errorf("%s is not empty; restore only goes into a new or empty folder, so nothing is ever overwritten", dir)
	}
	return nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// unpack writes a full backup's data folder into dest.
func unpack(ctx context.Context, st Store, codec Codec, b Info, dest string) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	r := &partsReader{ctx: ctx, st: st, codec: codec, prefix: "base/" + b.ID + "/", n: b.Parts}
	zr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("backup %s: %w", b.ID, err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("backup %s: %w", b.ID, err)
		}
		name := filepath.FromSlash(strings.TrimPrefix(h.Name, "./"))
		if name == "" || name == "." {
			continue
		}
		if filepath.IsAbs(name) || strings.HasPrefix(filepath.Clean(name), "..") {
			return fmt.Errorf("backup %s holds a bad path %q", b.ID, h.Name)
		}
		p := filepath.Join(dest, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if err == nil {
				err = f.Sync()
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("backup %s holds %q of a kind restore does not handle (tablespaces are not supported)", b.ID, h.Name)
		}
	}
	// pg_wal is empty in the backup (-X none) but must exist.
	return os.MkdirAll(filepath.Join(dest, "pg_wal", "archive_status"), 0o700)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", n>>10)
}

// ---- check ----

// CheckResult is what a backup check found.
type CheckResult struct {
	Restore   RestoreResult
	Databases int
	Relations int
	Damage    []string
	Took      time.Duration
}

// Check restores the newest backup into scratch (a folder that must not
// exist yet), starts it, reads every page with amcheck, and removes it.
func Check(ctx context.Context, o RestoreOptions) (CheckResult, error) {
	start := time.Now()
	o.Target = time.Time{}
	res, err := Restore(ctx, o)
	defer os.RemoveAll(o.Into)
	if err != nil {
		return CheckResult{}, err
	}
	n, err := pg.NewNode(res.Dir)
	if err != nil {
		return CheckResult{}, err
	}
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	lock, err := pg.AcquireLock(lctx, n.LockPath(), nil)
	cancel()
	if err != nil {
		return CheckResult{}, err
	}
	defer lock.Unlock()
	g, err := pg.NewGuard(n, lock, o.Logger)
	if err != nil {
		return CheckResult{}, err
	}
	defer g.Close()
	srv, err := n.Start(ctx, pg.StartOptions{Install: o.Install, Port: res.Port, Guard: g, Logger: o.Logger})
	if err != nil {
		return CheckResult{}, err
	}
	defer srv.StopFast()
	if o.Say != nil {
		o.Say("Restored. Reading every page with amcheck...")
	}
	connect := func(ctx context.Context, db string) (pgxConn, error) { return n.ConnectDB(ctx, db) }
	dbs, err := prepareAmcheck(ctx, connect)
	if err != nil {
		return CheckResult{}, err
	}
	readOnly := func(ctx context.Context, db string) (pgxConn, error) {
		c, err := n.ConnectDB(ctx, db)
		if err == nil {
			_, err = c.Exec(ctx, "SET default_transaction_read_only = on")
		}
		return c, err
	}
	p, err := pg.NewPageScan(ctx, readOnly, time.Now())
	if err != nil {
		return CheckResult{}, err
	}
	p, err = pg.ScanPages(ctx, readOnly, pg.PageScanOptions{BytesPerSecond: -1}, p)
	if err != nil {
		return CheckResult{}, fmt.Errorf("page check: %w", err)
	}
	return CheckResult{Restore: res, Databases: dbs, Relations: p.Relations, Damage: p.Damage, Took: time.Since(start)}, nil
}
