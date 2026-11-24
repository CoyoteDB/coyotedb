package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/migrate"
	"coyotedb.com/coyote/internal/pg"
)

// listFlag is a flag that may be given more than once.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*l = append(*l, s)
		}
	}
	return nil
}

func migrateUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: %[1]s migrate [options]           (asks for the connection string)
       COYOTE_MIGRATE_SOURCE=<connection string> %[1]s migrate [options]
       %[1]s migrate --live | --status | --cutover | --cancel

Copies an existing PostgreSQL database (12 or newer: Supabase, Neon, RDS,
Heroku, another server, a laptop) into this cluster. The source is only
read. The copy is checked table by table against the source before it is
switched in; if anything goes wrong, the cluster's database is unchanged
and the command can simply be run again.

The connection string (postgresql://user:password@host:5432/dbname) is read
from COYOTE_MIGRATE_SOURCE, or typed or piped in when the command asks, so
the password never shows in the list of running programs. Giving it as an
argument also works, but other users of this computer can see it then.

A live copy (--live) keeps the copy in step with the source while your app
runs, until --cutover switches it in with only a short stop. It needs
wal_level = logical on the source and runs on the main node's computer.

Options:
`, brand.Command)
}

func cmdMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { migrateUsage(stderr); fs.PrintDefaults() }
	dataDir := dataDirFlag(fs)
	ctlPort := controlPortFlag(fs)
	database := fs.String("database", pg.AppDatabase, "the cluster database that receives the copy (created if missing)")
	dryRun := fs.Bool("dry-run", false, "only check the source and the cluster and report; copy nothing")
	replace := fs.Bool("replace", false, "replace the contents of a database that is not empty (asks first)")
	yes := fs.Bool("yes", false, "answer the questions with yes (for scripts)")
	keepGoing := fs.Bool("keep-going", false, "switch the copy in even if some non-table parts (for example a policy or trigger) could not be created; they are listed")
	jobs := fs.Int("jobs", 0, "tables copied at the same time (default: up to 4, by CPU count)")
	workDir := fs.String("work-dir", "", "folder for the temporary copy (default: the system's temporary folder)")
	live := fs.Bool("live", false, "start a live copy that follows the source until --cutover")
	status := fs.Bool("status", false, "show how the live copy is doing")
	cutover := fs.Bool("cutover", false, "finish the live copy and switch it in (stop your app's writes first)")
	cancelLive := fs.Bool("cancel", false, "stop and remove the live copy (the cluster's database is not touched)")
	var excl, skip listFlag
	fs.Var(&excl, "exclude-schema", "leave a schema out (may be repeated)")
	fs.Var(&skip, "skip-extension", "leave an extension out (may be repeated); anything that uses it fails to copy")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	modes := 0
	for _, b := range []bool{*live, *status, *cutover, *cancelLive} {
		if b {
			modes++
		}
	}
	if modes > 1 || len(pos) > 1 || (*dryRun && (*status || *cutover || *cancelLive)) {
		fs.Usage()
		return 2
	}
	stdin := bufio.NewReader(os.Stdin)
	source := ""
	if !*status && !*cutover && !*cancelLive {
		switch {
		case len(pos) == 1 && pos[0] != "-":
			source = pos[0]
			fmt.Fprintf(stderr, "Tip: a connection string given as an argument is visible to other users of this computer while the command runs. Use COYOTE_MIGRATE_SOURCE, or run `%s migrate` without it and paste it when asked.\n", brand.Command)
		case os.Getenv(brand.EnvPrefix+"MIGRATE_SOURCE") != "" && len(pos) == 0:
			source = os.Getenv(brand.EnvPrefix + "MIGRATE_SOURCE")
		default:
			if isTerminal(os.Stdin) {
				fmt.Fprint(stderr, "Connection string of the database to move (postgresql://user:password@host:5432/dbname): ")
			}
			line, err := stdin.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				fmt.Fprintf(stderr, "Error: read the connection string: %v\n", err)
				return 1
			}
			source = strings.TrimSpace(line)
			if source == "" {
				fmt.Fprintln(stderr, "No connection string given. Set COYOTE_MIGRATE_SOURCE, or type it when asked.")
				return 2
			}
		}
	} else if len(pos) > 0 {
		fmt.Fprintln(stderr, "--status, --cutover and --cancel take no connection string (the live copy remembers it).")
		return 2
	}

	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	var ci agent.ConnInfo
	err = c.Do(qctx, http.MethodGet, "/conninfo", nil, &ci)
	cancel()
	if err != nil {
		return apiFail(stderr, node, err)
	}
	if ci.Pending {
		fmt.Fprintf(stderr, "The cluster is not ready yet: %s.\n", ci.Note)
		return 1
	}
	mainNode := func(ctx context.Context) (migrate.MainNode, error) {
		var st agent.Status
		if err := c.Do(ctx, http.MethodGet, "/status", nil, &st); err != nil {
			return migrate.MainNode{}, err
		}
		if st.Cluster.PrimaryName == "" {
			return migrate.MainNode{}, errors.New("the cluster has no main node right now")
		}
		return migrate.MainNode{Name: st.Cluster.PrimaryName, Epoch: st.Cluster.Epoch}, nil
	}
	// The administrator logs in only on this machine's own Postgres; it is
	// used (for extensions the app role may not create) only while this
	// machine is the main node.
	admin := func(ctx context.Context, db string) (*pgx.Conn, error) {
		if _, err := node.ReadInfo(); err != nil {
			return nil, nil // a witness: no database here
		}
		conn, err := node.ConnectDB(ctx, db)
		if err != nil {
			return nil, nil
		}
		var standby bool
		if err := conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&standby); err != nil || standby {
			_ = conn.Close(context.WithoutCancel(ctx))
			return nil, nil
		}
		return conn, nil
	}

	cache := os.Getenv(brand.EnvPrefix + "PG_CACHE")
	if cache == "" {
		cache = filepath.Join(node.StateDir, "cache")
	}
	inst, err := pg.Ensure(ctx, pg.FetchOptions{CacheDir: cache})
	if err != nil {
		fmt.Fprintf(stderr, "Error: get the PostgreSQL tools: %v\n", err)
		return 1
	}

	var logW io.Writer
	logPath := ""
	if err := os.MkdirAll(node.LogDir(), 0o700); err == nil {
		logPath = filepath.Join(node.LogDir(), "migrate-"+time.Now().Format("20060102-150405")+".log")
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			defer f.Close()
			logW = f
		} else {
			logPath = ""
		}
	}

	confirm := func(question, answer string) bool {
		if *yes {
			return true
		}
		if !isTerminal(os.Stdin) {
			fmt.Fprintln(stderr, "--replace needs a confirmation; add --yes to give it without a prompt.")
			return false
		}
		fmt.Fprint(stderr, "\n"+question)
		line, _ := stdin.ReadString('\n')
		return strings.TrimSpace(line) == answer
	}

	opts := migrate.Options{
		Source: source,
		Target: migrate.Target{
			AppURL: ci.URL, Password: ci.Password, Database: *database,
			Main: mainNode, Admin: admin,
		},
		Install: inst, Jobs: *jobs, DryRun: *dryRun, Replace: *replace, KeepGoing: *keepGoing,
		ExcludeSchemas: excl, SkipExtensions: skip, WorkDir: *workDir, Live: *live,
		Out: stdout, Confirm: confirm, Log: logW,
	}
	var res *migrate.Result
	switch {
	case *status:
		err = migrate.Status(ctx, opts)
	case *cancelLive:
		err = migrate.Cancel(ctx, opts)
	case *cutover:
		res, err = migrate.Cutover(ctx, opts)
	default:
		res, err = migrate.Run(ctx, opts)
	}
	if err != nil {
		switch {
		case errors.Is(err, migrate.ErrBlocked), errors.Is(err, migrate.ErrMainChanged), errors.Is(err, migrate.ErrNotSwitched):
		case errors.Is(err, migrate.ErrNotMain): // already said where to run it
		case errors.Is(err, context.Canceled):
			fmt.Fprintln(stderr, "Stopped. Nothing was switched in; run the same command again.")
		default:
			fmt.Fprintf(stderr, "%s\n", upperFirst(err.Error()))
		}
		if logPath != "" {
			fmt.Fprintf(stderr, "Details: %s\n", logPath)
		}
		return 1
	}
	if res == nil { // dry run, status, cancel, or a live copy started
		return 0
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Connection string for your app:")
	fmt.Fprintln(stdout, res.URL)
	fmt.Fprintf(stderr, "\nUser %s, database %s, TLS required. Any of the hosts works: each one passes the connection to the main node.\n"+
		"On another machine, copy %s there and point sslrootcert at it.\n", ci.User, res.Database, ci.CAFile)
	return 0
}

// isTerminal reports whether f is an interactive terminal: a character
// device that is not the null device (a program started without input gets
// /dev/null, which is a character device too).
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(st, null) {
		return false
	}
	return true
}
