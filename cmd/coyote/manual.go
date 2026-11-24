package main

// `coyote dev manual-up`: the M1/M2 single-node agent, kept for the
// manual replication tools (`dev cut-stream/promote/rewind/...` without a
// cluster). It runs Postgres from a data dir that is not a cluster member,
// optionally cloning from --clone-from first. Not a stable interface; the
// cluster agent (`up`, `join`) is the real path.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/pg"
)

func cmdManualUp(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev manual-up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	port := fs.Int("pg-port", pg.DefaultPort, "port for Postgres on 127.0.0.1")
	name := fs.String("name", "", "this node's name: a-z, 0-9, '-' (default: from the host name; stored on first start)")
	cloneFrom := fs.String("clone-from", "", "first start only: copy the database from this node (HOST:PORT of its Postgres) and run as its standby")
	upPassFile := fs.String("upstream-password-file", "", "with --clone-from: the other node's cluster.secrets file")
	archive := fs.String("pg-archive", "", "use this Postgres .tar.gz instead of downloading (still checksum-verified)")
	cacheDir := fs.String("pg-cache", "", "where to keep the Postgres program files (default: $"+brand.EnvPrefix+"PG_CACHE or <data-dir>/cache)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "Unexpected argument %q.\n", fs.Arg(0))
		return 2
	}
	if *port <= 0 || *port > 65535 {
		fmt.Fprintf(stderr, "--pg-port must be between 1 and 65535.\n")
		return 2
	}
	node, err := resolveDataDir(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if isClusterNode(node) {
		fmt.Fprintf(stderr, "%s is a cluster node; start it with `%s up`.\n", node.StateDir, brand.Command)
		return 1
	}
	if err := node.Prepare(); err != nil {
		fmt.Fprintf(stderr, "Error: could not create the data folder %s: %v\n", node.StateDir, err)
		return 1
	}
	logPath := filepath.Join(node.LogDir(), brand.Command+".log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "Error: could not open log file: %v\n", err)
		return 1
	}
	defer lf.Close()
	log := slog.New(slog.NewTextHandler(lf, &slog.HandlerOptions{Level: slog.LevelInfo})).With("agent_pid", os.Getpid())
	log.Info("up", "version", version, "data_dir", node.StateDir, "port", *port)

	cache := *cacheDir
	if cache == "" {
		cache = os.Getenv(brand.EnvPrefix + "PG_CACHE")
	}
	if cache == "" {
		cache = filepath.Join(node.StateDir, "cache")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	u := &upRun{node: node, port: *port, name: *name, archive: *archive, cache: cache, log: log, out: stdout, logPath: logPath,
		cloneFrom: *cloneFrom, upPassFile: *upPassFile}
	if (*cloneFrom == "") != (*upPassFile == "") {
		fmt.Fprintln(stderr, "--clone-from and --upstream-password-file go together.")
		return 2
	}
	err = u.run(ctx)
	if err != nil {
		log.Error("up failed", "err", err)
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stdout, "Cancelled.")
			return 130
		}
		fmt.Fprintf(stderr, "\nError: %v\n\nMore detail in %s\n", err, logPath)
		return 1
	}
	return 0
}

type upRun struct {
	node       pg.Node
	port       int
	name       string
	cloneFrom  string
	upPassFile string
	archive    string
	cache      string
	log        *slog.Logger
	out        io.Writer
	logPath    string
}

func (u *upRun) say(format string, a ...any) { fmt.Fprintf(u.out, format+"\n", a...) }

func (u *upRun) run(ctx context.Context) error {
	lockCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	lock, err := pg.AcquireLock(lockCtx, u.node.LockPath(), func() {
		u.say("Another %s is using this data folder (or is still stopping its database). Waiting up to a minute…", brand.Command)
	})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("another %s is already running for %s", brand.Command, u.node.StateDir)
	}
	defer lock.Unlock()

	inst, err := pg.Ensure(ctx, pg.FetchOptions{
		CacheDir: u.cache,
		Archive:  u.archive,
		Logger:   u.log,
		Progress: u.progress(),
	})
	if err != nil {
		if errors.Is(err, pg.ErrChecksumMismatch) {
			return fmt.Errorf("the PostgreSQL download did not match its expected fingerprint, so it was not used. This can mean a broken download or tampering: %w", err)
		}
		return fmt.Errorf("could not get PostgreSQL: %w", err)
	}
	u.log.Info("postgres install ready", "root", inst.Root)

	name, err := u.resolveName()
	if err != nil {
		return err
	}
	u.log.Info("node", "name", name)

	// Stopping a leftover never starts anything, so it is always safe.
	if u.node.Initialized() {
		stopped, err := u.node.StopLeftover(ctx, inst, u.log)
		if err != nil {
			return fmt.Errorf("a database from an earlier run is still running and could not be stopped: %w", err)
		}
		if stopped {
			u.say("Stopped a database left running by an earlier run; starting it fresh.")
		}
	}

	// Safety rule 6: decide before any Postgres starts.
	marker, err := u.node.ReadMarker()
	if err != nil {
		return err
	}
	_, secErr := os.Stat(u.node.SecretsPath())
	plan, err := pg.PlanStart(u.node.Initialized(), secErr == nil, marker, u.cloneFrom != "")
	if err != nil {
		u.log.Warn("refusing to start", "marker", marker, "err", err)
		return err
	}

	guard, err := pg.NewGuard(u.node, lock, u.log)
	if err != nil {
		return fmt.Errorf("could not set up the process guard: %w", err)
	}
	defer func() {
		if err := guard.Close(); err != nil {
			u.log.Warn("guard close", "err", err)
		}
	}()

	switch plan {
	case pg.PlanInit:
		u.say("Creating a new database in %s …", u.node.StateDir)
		if _, err := u.node.InitDB(ctx, inst, u.log); err != nil {
			return err
		}
	case pg.PlanClone:
		up, err := pg.ParseUpstream(u.cloneFrom)
		if err != nil {
			return err
		}
		secrets, err := pg.ReadSecretsFile(u.upPassFile)
		if err != nil {
			return err
		}
		u.say("Copying the database from %s into %s …", up, u.node.StateDir)
		if err := u.node.Clone(ctx, pg.CloneOptions{
			Install: inst, Guard: guard, Source: up, Secrets: secrets,
			Name: name, Port: u.port, Reclone: u.node.Initialized(), Logger: u.log,
		}); err != nil {
			return fmt.Errorf("could not copy the database from %s: %w", up, err)
		}
		u.say("Copy complete.")
	case pg.PlanResume:
		if u.cloneFrom != "" {
			u.say("This node already has a database; --clone-from is ignored.")
		}
		if _, err := u.node.InitDB(ctx, inst, u.log); err != nil { // version check only
			return err
		}
	}

	info, _ := u.node.ReadInfo()
	info.Name, info.Port = name, u.port
	info.PGVersion, info.Target = inst.Build.Version, inst.Build.Target
	if err := u.node.WriteInfo(info); err != nil {
		return fmt.Errorf("write node info: %w", err)
	}

	srv, err := u.node.Start(ctx, pg.StartOptions{Install: inst, Port: u.port, Name: name, Guard: guard, Logger: u.log})
	if err != nil {
		return fmt.Errorf("PostgreSQL did not start: %w", err)
	}
	role, err := u.afterStart(ctx)
	if err != nil {
		_ = srv.Stop(pg.DefaultStopTimeouts)
		return err
	}

	u.say("")
	u.say("PostgreSQL %s is running on 127.0.0.1:%d.", pg.MajorVersion, u.port)
	u.say("  Node:      %s, %s", name, role)
	u.say("  Connect:   %s", pg.ConnString(u.port, "", false))
	u.say("  Password:  %s conninfo --data-dir %s", brand.Command, quoteArg(u.node.StateDir))
	u.say("  Logs:      %s", u.node.LogDir())
	u.say("Press Ctrl-C to stop.")
	u.log.Info("ready", "postgres_pid", srv.PID(), "port", u.port)

	var runErr error
	select {
	case <-ctx.Done():
		u.say("\nStopping PostgreSQL…")
	case <-srv.Done():
		runErr = fmt.Errorf("PostgreSQL stopped unexpectedly (%v); see the logs", srv.ExitErr())
	case <-guard.Lost():
		runErr = errors.New("the process guard stopped unexpectedly, so the database was stopped for safety")
	}
	if err := srv.Stop(pg.DefaultStopTimeouts); err != nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	u.say("Stopped.")
	return nil
}

// resolveName picks the node name: --name, else the stored one, else the
// host name. A stored name cannot be changed: slots and the sync list use it.
func (u *upRun) resolveName() (string, error) {
	info, _ := u.node.ReadInfo()
	name := u.name
	switch {
	case name != "" && info.Name != "" && name != info.Name:
		return "", fmt.Errorf("this node is called %q; it cannot be renamed to %q", info.Name, name)
	case name == "" && info.Name != "":
		name = info.Name
	case name == "":
		h, err := os.Hostname()
		if err != nil {
			h = ""
		}
		name = pg.SanitizeName(h)
	}
	return name, pg.ValidName(name)
}

// afterStart finishes setup by role: a primary makes sure the replication
// and rewind roles and their secrets exist.
func (u *upRun) afterStart(ctx context.Context) (string, error) {
	c, err := u.node.Connect(ctx)
	if err != nil {
		return "", err
	}
	defer c.Close(context.Background())
	var standby bool
	if err := c.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&standby); err != nil {
		return "", err
	}
	if standby {
		st, err := u.node.ReadReplState()
		if err != nil {
			return "", err
		}
		if st.Upstream == nil {
			return "standby (read-only), not streaming (stream cut)", nil
		}
		return "standby (read-only) of " + st.Upstream.String(), nil
	}
	s, err := u.node.EnsureSecrets()
	if err != nil {
		return "", err
	}
	if err := pg.EnsureRoles(ctx, c, s); err != nil {
		return "", fmt.Errorf("set up replication roles: %w", err)
	}
	return "primary", nil
}

func (u *upRun) progress() func(done, total int64) {
	last := -1
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		pct := int(done * 100 / total)
		step := pct / 10 * 10
		if step == last {
			return
		}
		if last == -1 {
			u.say("Downloading PostgreSQL %s (%d MB, first run only)…", pg.Version, total>>20)
		}
		last = step
		u.say("  %3d%%", step)
	}
}
