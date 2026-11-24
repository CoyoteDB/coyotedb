package main

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/backup"
	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/pg"
)

// Environment variables for secrets (never on the command line).
var (
	s3SecretEnv  = brand.EnvPrefix + "S3_SECRET_KEY"
	backupKeyEnv = brand.EnvPrefix + "BACKUP_KEY"
)

func backupUsage(w io.Writer) {
	fmt.Fprintf(w, `Backups (full copies every night plus every change, kept 7 days by default).

Usage:
  %[1]s backup                     where backups go, the last one, how far back you can restore
  %[1]s backup list                the full backups that exist
  %[1]s backup now                 take a full backup now
  %[1]s backup restore --into DIR [--to TIME]
                                   restore into a NEW folder (never over the running cluster);
                                   TIME like "2026-11-24 14:05" (this computer's time) or "15m ago".
                                   Without --to: everything saved so far. Then start it as a new
                                   cluster with "%[1]s up --data-dir DIR"
  %[1]s backup check               restore the newest backup into a temporary folder, check every
                                   page, and delete it again
  %[1]s backup schedule [every=24h] [at=03:00] [keep=7d]
  %[1]s backup folder [PATH] [--encrypt]
                                   keep backups in PATH on every data node (default: next to the
                                   data folder; use another disk if you can)
  %[1]s backup s3 --endpoint URL --bucket NAME --access-key ID [--prefix P] [--region R]
                                   keep backups in S3-compatible storage (AWS S3, Backblaze B2,
                                   Cloudflare R2, MinIO). The secret key is read from %[2]s or
                                   asked for. Cloud backups are always encrypted
  %[1]s backup key                 show the backup key (keep a copy away from these computers)
  %[1]s backup on | off

Restore when the cluster is gone (on any computer):
  %[1]s backup restore --into DIR --from PATH            (a backup folder)
  %[1]s backup restore --into DIR --from s3://BUCKET/PREFIX --endpoint URL --access-key ID
      secret key from %[2]s, backup key from %[3]s (or asked for)
`, brand.Command, s3SecretEnv, backupKeyEnv)
}

func cmdBackup(args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "", "show", "status":
		return backupShow(args, stdout, stderr)
	case "list":
		return backupList(args, stdout, stderr)
	case "now":
		return backupNowCmd(args, stdout, stderr)
	case "schedule":
		return backupSchedule(args, stdout, stderr)
	case "on", "off":
		return backupOnOff(sub == "on", args, stdout, stderr)
	case "folder":
		return backupFolder(args, stdout, stderr)
	case "s3":
		return backupS3(args, stdout, stderr)
	case "key":
		return backupKey(args, stdout, stderr)
	case "restore":
		return backupRestore(args, stdout, stderr, false)
	case "check":
		return backupRestore(args, stdout, stderr, true)
	case "wal-push":
		return walPush(args, stderr)
	case "wal-fetch":
		return walFetch(args, stderr)
	case "help", "-h", "--help":
		backupUsage(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "Unknown backup command %q.\n\n", sub)
	backupUsage(stderr)
	return 2
}

func backupFlags(name string, stderr io.Writer) (*flag.FlagSet, *string, *int, *bool) {
	fs := flag.NewFlagSet("backup "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs, dataDirFlag(fs), controlPortFlag(fs), fs.Bool("json", false, "print JSON")
}

func backupShow(args []string, stdout, stderr io.Writer) int {
	fs, dataDir, ctlPort, asJSON := backupFlags("", stderr)
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
		return 2
	}
	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var v agent.BackupsView
	if err := c.Do(ctx, http.MethodGet, "/backups?refresh=1", nil, &v); err != nil {
		return apiFail(stderr, node, err)
	}
	if *asJSON {
		return printJSON(stdout, v)
	}
	printBackupOverview(stdout, v)
	return 0
}

func backupList(args []string, stdout, stderr io.Writer) int {
	fs, dataDir, ctlPort, asJSON := backupFlags("list", stderr)
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
		return 2
	}
	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var v agent.BackupsView
	if err := c.Do(ctx, http.MethodGet, "/backups?refresh=1", nil, &v); err != nil {
		return apiFail(stderr, node, err)
	}
	if *asJSON {
		return printJSON(stdout, v.Backups)
	}
	if len(v.Backups) == 0 {
		fmt.Fprintln(stdout, "No full backups yet.")
		if v.Status.Enabled {
			fmt.Fprintf(stdout, "The first one is taken by itself soon after backups are turned on. Take one now: `%s backup now`.\n", brand.Command)
		} else {
			fmt.Fprintf(stdout, "Backups are off. Turn them on: `%s backup on`.\n", brand.Command)
		}
		return 0
	}
	now := time.Now()
	ready := 0
	for _, b := range v.Backups {
		if b.State == "done" {
			ready++
		}
	}
	fmt.Fprintf(stdout, "%d full %s, %d ready to restore from. Newest first.\n\n", len(v.Backups), plural(len(v.Backups), "backup", "backups"), ready)
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  FINISHED\tSTATE\tKIND\tNODE\tSIZE\tID")
	for _, b := range v.Backups {
		fin := "-"
		if b.FinishedAt != nil {
			fin = stamp(*b.FinishedAt, now)
		}
		state := map[string]string{"done": "ready", "running": "running", "incomplete": "incomplete"}[b.State]
		kind := map[string]string{"scheduled": "scheduled", "manual": "by hand"}[b.Kind]
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", fin, cmp.Or(state, b.State), cmp.Or(kind, b.Kind), b.Node, humanSize(b.SizeBytes), cmp.Or(b.ID, "-"))
	}
	tw.Flush()
	for _, b := range v.Backups {
		if b.State == "incomplete" {
			fmt.Fprintln(stdout, "\nIncomplete: finished, but some changes after it were not saved (or the computer taking it stopped). It is never used for a restore and is removed with the old ones.")
			break
		}
	}
	return 0
}

func backupNowCmd(args []string, stdout, stderr io.Writer) int {
	fs, dataDir, ctlPort, _ := backupFlags("now", stderr)
	wait := fs.Bool("wait", false, "wait until it is done")
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
		return 2
	}
	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var before agent.BackupsView
	_ = c.Do(ctx, http.MethodGet, "/backups?refresh=1", nil, &before)
	var r agent.BackupNowResult
	if err := c.Do(ctx, http.MethodPost, "/backups", struct{}{}, &r); err != nil {
		return apiFail(stderr, node, err)
	}
	fmt.Fprintln(stdout, r.Message)
	if !*wait {
		fmt.Fprintf(stdout, "It takes a few minutes for most databases; your database keeps running. Follow it with `%s backup`.\n", brand.Command)
		return 0
	}
	fmt.Fprintln(stdout, "Waiting for it to finish (Ctrl-C stops waiting; the backup carries on).")
	known := map[string]bool{}
	for _, b := range before.Backups {
		known[b.ID] = true
	}
	asked := time.Now()
	saidWaiting := ""
	deadline := time.Now().Add(24 * time.Hour)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		pctx, pcancel := context.WithTimeout(context.Background(), 60*time.Second)
		var v agent.BackupsView
		err := c.Do(pctx, http.MethodGet, "/backups?refresh=1", nil, &v)
		pcancel()
		if err != nil {
			continue
		}
		for _, b := range v.Backups {
			if !known[b.ID] && b.State == "done" {
				fmt.Fprintf(stdout, "Done: %s (%s).\n", b.ID, humanSize(b.SizeBytes))
				return 0
			}
		}
		if w := v.Status.Waiting; v.Status.Running == nil && w != "" {
			if w != saidWaiting {
				fmt.Fprintf(stdout, "Not started yet: %s. Trying again every few seconds.\n", w)
				saidWaiting = w
			}
			continue
		}
		if v.Status.Running == nil && v.Status.LastError != "" && (v.Status.LastErrorAt == nil || !v.Status.LastErrorAt.Before(asked.Add(-time.Second))) {
			fmt.Fprintf(stderr, "The backup failed: %s\n", v.Status.LastError)
			fmt.Fprintf(stderr, "Fix the cause above, then try again with `%s backup now --wait`.\n", brand.Command)
			return 1
		}
	}
	fmt.Fprintf(stderr, "Stopped waiting after a day. See `%s backup`.\n", brand.Command)
	return 1
}

func backupSchedule(args []string, stdout, stderr io.Writer) int {
	fs, dataDir, ctlPort, _ := backupFlags("schedule", stderr)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	var req agent.BackupScheduleRequest
	for _, kv := range pos {
		k, v, ok := strings.Cut(kv, "=")
		switch {
		case ok && k == "every":
			req.Every = v
		case ok && k == "at":
			req.At = v
		case ok && k == "keep":
			req.Keep = v
		case ok && k == "archive_timeout":
			req.ArchiveTimeout = v
		default:
			fmt.Fprintf(stderr, "%q: expected every=24h, at=03:00, keep=7d or archive_timeout=60s\n", kv)
			return 2
		}
	}
	return backupPut(*dataDir, *ctlPort, "/backups/schedule", req, stdout, stderr)
}

func backupOnOff(on bool, args []string, stdout, stderr io.Writer) int {
	fs, dataDir, ctlPort, _ := backupFlags("on", stderr)
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
		return 2
	}
	if !on {
		fmt.Fprintln(stdout, "Turning backups off: no new full backups and no saved changes from now on. Existing backups stay where they are.")
	}
	return backupPut(*dataDir, *ctlPort, "/backups/schedule", agent.BackupScheduleRequest{Enabled: &on}, stdout, stderr)
}

func backupPut(dataDir string, ctlPort int, path string, req any, stdout, stderr io.Writer) int {
	c, node, err := client(dataDir, ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var v agent.BackupScheduleView
	if err := c.Do(ctx, http.MethodPut, path, req, &v); err != nil {
		return apiFail(stderr, node, err)
	}
	if !v.Enabled {
		fmt.Fprintf(stdout, "Saved. Backups are off. Turn them on again with `%s backup on`.\n", brand.Command)
		return 0
	}
	fmt.Fprintf(stdout, "Saved. Backups are on: %s.\n", scheduleText(v))
	return 0
}

func backupFolder(args []string, stdout, stderr io.Writer) int {
	fs, dataDir, ctlPort, _ := backupFlags("folder", stderr)
	encrypt := fs.Bool("encrypt", false, "encrypt the backups (keep the key: without it they cannot be read)")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) > 1 {
		return 2
	}
	req := agent.BackupDestinationRequest{Kind: cluster.BackupKindFolder, Encrypt: *encrypt}
	if len(pos) == 1 {
		p, err := filepath.Abs(pos[0])
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		req.Folder = p
	}
	return backupSetDest(*dataDir, *ctlPort, req, stdout, stderr)
}

func backupS3(args []string, stdout, stderr io.Writer) int {
	fs, dataDir, ctlPort, _ := backupFlags("s3", stderr)
	endpoint := fs.String("endpoint", "", "the storage address, e.g. https://s3.us-west-002.backblazeb2.com")
	bucket := fs.String("bucket", "", "bucket name")
	prefix := fs.String("prefix", "coyote", "folder inside the bucket")
	region := fs.String("region", "", "region (default us-east-1; Cloudflare R2: auto)")
	access := fs.String("access-key", "", "access key id")
	pathStyle := fs.String("path-style", "", "on or off (default: automatic)")
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 || *endpoint == "" || *bucket == "" || *access == "" {
		fmt.Fprintf(stderr, "Usage: %s backup s3 --endpoint URL --bucket NAME --access-key ID [--prefix P] [--region R]\n", brand.Command)
		return 2
	}
	secret, err := readSecret(os.Getenv(s3SecretEnv), "Secret access key for "+*access+": ", stderr)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v (or set %s)\n", err, s3SecretEnv)
		return 1
	}
	req := agent.BackupDestinationRequest{Kind: cluster.BackupKindS3, S3: &cluster.BackupS3{
		Endpoint: *endpoint, Bucket: *bucket, Prefix: *prefix, Region: *region, AccessKey: *access, SecretKey: secret, PathStyle: *pathStyle,
	}}
	return backupSetDest(*dataDir, *ctlPort, req, stdout, stderr)
}

func backupSetDest(dataDir string, ctlPort int, req agent.BackupDestinationRequest, stdout, stderr io.Writer) int {
	c, node, err := client(dataDir, ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var r agent.BackupDestinationResult
	if err := c.Do(ctx, http.MethodPut, "/backups/destination", req, &r); err != nil {
		code := apiFail(stderr, node, err)
		var ae *control.APIError
		if errors.As(err, &ae) && ae.Code == "destination_failed" {
			if req.Kind == cluster.BackupKindS3 {
				fmt.Fprintln(stderr, "Nothing was changed: backups still go where they went before. Check the address, the bucket and the keys, then run the command again.")
			} else {
				fmt.Fprintln(stderr, "Nothing was changed: backups still go where they went before. Pick a folder this computer can write to (on another disk if you can), then run the command again.")
			}
		}
		return code
	}
	fmt.Fprintf(stdout, "Saved. Backups now go to %s.\n", r.Destination.Label)
	if r.Destination.SameDisk {
		fmt.Fprintln(stdout, "Note: that is the same disk as the database. It protects you from mistakes, not from this disk failing.")
	}
	if r.NewKey != "" {
		printKeyWarning(stdout, r.NewKey)
	}
	fmt.Fprintf(stdout, "The next full backup goes there by itself within about 10 minutes. Or take one now: `%s backup now`.\n", brand.Command)
	return 0
}

func printKeyWarning(w io.Writer, key string) {
	fmt.Fprintf(w, "\nBackup key. You need it to restore these backups:\n\n    %s\n\n", key)
	fmt.Fprintln(w, "  Save it now somewhere that is not one of these computers: a password manager, or on paper.")
	fmt.Fprintln(w, "  The backups are encrypted with it. If every computer is lost and nobody has this key,")
	fmt.Fprintln(w, "  nobody can read the backups, not even us.")
	fmt.Fprintf(w, "  While a cluster computer runs, `%s backup key` shows it again.\n\n", brand.Command)
}

func backupKey(args []string, stdout, stderr io.Writer) int {
	fs, dataDir, ctlPort, _ := backupFlags("key", stderr)
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
		return 2
	}
	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var k agent.BackupKey
	if err := c.Do(ctx, http.MethodPost, "/backups/key/reveal", struct{}{}, &k); err != nil {
		return apiFail(stderr, node, err)
	}
	printKeyWarning(stdout, k.Key)
	return 0
}

// readSecret returns env if set, else reads one line from standard input
// (asking for it on a terminal). Never from the command line.
func readSecret(env, prompt string, stderr io.Writer) (string, error) {
	if env != "" {
		return strings.TrimSpace(env), nil
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(stderr, prompt)
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if s := strings.TrimSpace(line); s != "" {
		return s, nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		err = errors.New("nothing on standard input")
	}
	return "", err
}

// ---- restore and check ----

func backupRestore(args []string, stdout, stderr io.Writer, check bool) int {
	name := "restore"
	if check {
		name = "check"
	}
	fs := flag.NewFlagSet("backup "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	into := fs.String("into", "", "a new or empty folder to restore into")
	to := fs.String("to", "", `the moment to restore: "2026-11-24 14:05", RFC 3339, or "15m ago" (default: everything saved)`)
	from := fs.String("from", "", "where the backups are when this computer is not in the cluster: a folder or s3://BUCKET/PREFIX")
	endpoint := fs.String("endpoint", "", "(s3) the storage address")
	region := fs.String("region", "", "(s3) region")
	access := fs.String("access-key", "", "(s3) access key id")
	clusterID := fs.String("cluster", "", "which cluster's backups, when the place holds several")
	port := fs.Int("pg-port", 0, "Postgres port on 127.0.0.1 while restoring (default: a free one)")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) > 0 || (!check && *into == "") {
		fmt.Fprintf(stderr, "Usage: %s backup restore --into NEW-FOLDER [--to TIME] [--from PLACE]\n", brand.Command)
		return 2
	}
	var target time.Time
	if *to != "" {
		if target, err = parseWhen(*to, time.Now()); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 2
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var cfg backup.Config
	var cacheBase string
	if *from == "" {
		node, err := resolveDataDir(*dataDir)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		cfg, err = backup.ReadConfig(backup.ConfigFile(node.StateDir))
		if err != nil || cfg.Kind == "" {
			fmt.Fprintf(stderr, "Error: no backup settings on this computer (%s). Give --from PLACE to restore from a backup folder or bucket.\n", node.StateDir)
			return 1
		}
		cacheBase = node.StateDir
		if check && *into == "" {
			*into = filepath.Join(backup.StateDir(node.StateDir), "check-"+time.Now().UTC().Format("20060102T150405Z"))
		}
	} else {
		cfg, err = restoreSource(ctx, *from, *endpoint, *region, *access, *clusterID, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		if check && *into == "" {
			fmt.Fprintln(stderr, "Error: give --into SCRATCH-FOLDER for a check from --from.")
			return 2
		}
	}
	intoAbs, err := filepath.Abs(*into)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if cacheBase == "" {
		cacheBase = intoAbs
	}
	cache := os.Getenv(brand.EnvPrefix + "PG_CACHE")
	if cache == "" {
		cache = filepath.Join(cacheBase, "cache")
	}
	inst, err := pg.Ensure(ctx, pg.FetchOptions{CacheDir: cache})
	if err != nil {
		fmt.Fprintf(stderr, "Error: get the PostgreSQL programs: %v\n", err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	o := backup.RestoreOptions{
		Config: cfg, Into: intoAbs, Target: target, Install: inst, Exe: exe, Port: *port,
		Logger: slog.New(slog.DiscardHandler),
		Say:    func(s string) { fmt.Fprintln(stdout, s) },
	}
	if check {
		fmt.Fprintf(stdout, "Checking the newest backup in %s.\nIt is restored into a temporary folder, every page is read, then it is deleted. Your database keeps running.\n", cfg.Label())
	} else {
		fmt.Fprintf(stdout, "Restoring from %s into %s. The running database is not touched.\n", cfg.Label(), intoAbs)
	}
	if check {
		res, err := backup.Check(ctx, o)
		if err == nil && len(res.Damage) > 0 {
			err = fmt.Errorf("the restored copy has damage: %s", strings.Join(res.Damage, "; "))
		}
		if err != nil {
			fmt.Fprintf(stderr, "Backup check failed: %v\n%s\n", err, restoreNext(err, true))
			return 1
		}
		fmt.Fprintln(stdout, "\nBackup check passed: the newest backup restores, and every page reads without damage.")
		rows(stdout, [][2]string{
			{"Full backup", res.Restore.Backup.ID},
			{"Last change in it", orUnknown(res.Restore.LastTx)},
			{"Read", fmt.Sprintf("%d %s, %d tables and indexes", res.Databases, plural(res.Databases, "database", "databases"), res.Relations)},
			{"Took", res.Took.Round(time.Second).String()},
		})
		fmt.Fprintln(stdout, "The temporary copy was deleted.")
		return 0
	}
	res, err := backup.Restore(ctx, o)
	if err != nil {
		fmt.Fprintf(stderr, "The restore did not finish: %v\n%s\n", err, restoreNext(err, false))
		return 1
	}
	fmt.Fprintf(stdout, "\nRestored into %s.\n", res.Dir)
	last := res.LastTx
	if res.StopLine != "" {
		last = res.StopLine
	}
	rows(stdout, [][2]string{{"From full backup", res.Backup.ID}, {"Last change in it", orUnknown(last)}})
	if *from == "" {
		fmt.Fprintln(stdout, "Nothing in the running cluster was changed.")
	}
	fmt.Fprintf(stdout, "\nStart it as a new, separate database (ports picked that are free here now):\n\n    %s\n\n", agent.RestoredStartCommand(res.Dir))
	fmt.Fprintln(stdout, "Look at it, copy out what you need, or move your app to it. Your running database stays as it is.")
	return 0
}

func orUnknown(s string) string {
	if s == "" {
		return "time unknown"
	}
	return s
}

// restoreSource builds a backup config for --from.
func restoreSource(ctx context.Context, from, endpoint, region, access, clusterID string, stderr io.Writer) (backup.Config, error) {
	var cfg backup.Config
	if rest, ok := strings.CutPrefix(from, "s3://"); ok {
		bucket, prefix, _ := strings.Cut(rest, "/")
		if endpoint == "" || access == "" || bucket == "" {
			return cfg, errors.New("restoring from s3:// needs --endpoint and --access-key")
		}
		secret, err := readSecret(os.Getenv(s3SecretEnv), "Secret access key for "+access+": ", stderr)
		if err != nil {
			return cfg, fmt.Errorf("%w (or set %s)", err, s3SecretEnv)
		}
		cfg = backup.Config{Kind: backup.KindS3, S3: backup.S3Settings{Endpoint: endpoint, Region: region, Bucket: bucket, Prefix: prefix, AccessKey: access, SecretKey: secret}}
	} else {
		p, err := filepath.Abs(from)
		if err != nil {
			return cfg, err
		}
		cfg = backup.Config{Kind: backup.KindFolder, Folder: p}
	}
	base, err := cfg.BaseStore()
	if err != nil {
		return cfg, err
	}
	if clusterID == "" {
		objs, err := base.List(ctx, "")
		if err != nil {
			return cfg, fmt.Errorf("list %s: %w", base.Label(), err)
		}
		var ids []string
		for _, o := range objs {
			if id, ok := strings.CutSuffix(o.Key, "/"+backup.IdentityKey); ok && !strings.Contains(id, "/") {
				ids = append(ids, id)
			}
		}
		switch len(ids) {
		case 0:
			return cfg, fmt.Errorf("no backups found in %s", base.Label())
		case 1:
			clusterID = ids[0]
		default:
			return cfg, fmt.Errorf("%s holds backups of several clusters (%s); pick one with --cluster", base.Label(), strings.Join(ids, ", "))
		}
	}
	cfg.ClusterID = clusterID
	st, _ := cfg.Store()
	id, err := backup.ReadIdentity(ctx, st)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", st.Label(), err)
	}
	if id.KeyID != "" {
		k, err := readSecret(os.Getenv(backupKeyEnv), "These backups are encrypted. Backup key (cybk-...): ", stderr)
		if err != nil {
			return cfg, fmt.Errorf("%w (or set %s)", err, backupKeyEnv)
		}
		kb, err := backup.DecodeKey(k)
		if err != nil {
			return cfg, err
		}
		if backup.KeyID(kb) != id.KeyID {
			return cfg, backup.ErrWrongKey
		}
		cfg.Key = k
	}
	return cfg, nil
}

// parseWhen reads a restore target.
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if d, ok := strings.CutSuffix(s, " ago"); ok {
		dur, err := time.ParseDuration(strings.TrimSpace(d))
		if err != nil || dur < 0 {
			return time.Time{}, fmt.Errorf("%q: expected something like \"15m ago\"", s)
		}
		return now.Add(-dur), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q: expected a time like \"2026-11-24 14:05\" (this computer's time), RFC 3339, or \"15m ago\"", s)
}

// ---- archive_command and restore_command ----

// walPush is archive_command. Exit 0 = saved (or deliberately given up,
// D63); anything else makes Postgres try again later.
func walPush(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup wal-push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", "", "the node's data folder")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 2 || *dataDir == "" {
		fmt.Fprintln(stderr, "usage: backup wal-push --data-dir DIR PATH NAME")
		return 2
	}
	cfg, err := backup.ReadConfig(backup.ConfigFile(*dataDir))
	if err != nil {
		fmt.Fprintf(stderr, "coyote wal-push: %v\n", err)
		return 1
	}
	if !cfg.Enabled {
		return 0 // backups were turned off after Postgres read its config
	}
	node, err := pg.NewNode(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "coyote wal-push: %v\n", err)
		return 1
	}
	_, serr := os.Stat(filepath.Join(node.PGData(), "standby.signal"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := backup.Push(ctx, cfg, backup.PushOptions{
		StateDir: node.StateDir, Path: pos[0], Name: pos[1],
		Primary: errors.Is(serr, os.ErrNotExist),
		Backlog: func() int64 {
			ents, _ := os.ReadDir(filepath.Join(node.PGData(), "pg_wal", "archive_status"))
			var n int64
			for _, e := range ents {
				if strings.HasSuffix(e.Name(), ".ready") {
					n += backup.SegmentSize
				}
			}
			return n
		},
		DiskFree: func() (uint64, error) { f, _, err := pg.DiskSpace(node.PGData()); return f, err },
	})
	if res.Note != "" {
		fmt.Fprintf(stderr, "coyote wal-push: %s\n", res.Note)
	}
	if err != nil {
		fmt.Fprintf(stderr, "coyote wal-push: could not save %s: %v\n", pos[1], err)
		return 1
	}
	return 0
}

// walFetch is restore_command during a restore. Exit 1 = not in the
// archive (Postgres takes that as the end); 126 makes Postgres stop the
// restore (a real failure must never look like "the end of the backup").
func walFetch(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup wal-fetch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	source := fs.String("source", "", "the restore's source settings file")
	pos, err := parseArgs(fs, args)
	if err != nil || len(pos) != 2 || *source == "" {
		fmt.Fprintln(stderr, "usage: backup wal-fetch --source FILE NAME PATH")
		return 126
	}
	cfg, err := backup.ReadConfig(*source)
	if err != nil {
		fmt.Fprintf(stderr, "coyote wal-fetch: %v\n", err)
		return 126
	}
	st, err := cfg.Store()
	if err != nil {
		fmt.Fprintf(stderr, "coyote wal-fetch: %v\n", err)
		return 126
	}
	codec, err := cfg.Codec()
	if err != nil {
		fmt.Fprintf(stderr, "coyote wal-fetch: %v\n", err)
		return 126
	}
	for attempt := range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err = backup.Fetch(ctx, st, codec, pos[0], pos[1])
		cancel()
		switch {
		case err == nil:
			return 0
		case errors.Is(err, backup.ErrNotFound):
			return 1
		case errors.Is(err, backup.ErrWrongKey):
			fmt.Fprintf(stderr, "coyote wal-fetch: %s: %v\n", pos[0], err)
			return 126
		}
		fmt.Fprintf(stderr, "coyote wal-fetch: %s: %v (try %d of 5)\n", pos[0], err, attempt+1)
		time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
	}
	return 126
}

// ---- words ----

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

func humanDur(s string) string {
	d, err := time.ParseDuration(s)
	if err != nil {
		return s
	}
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		n := int(d / (24 * time.Hour))
		if n == 1 {
			return "day"
		}
		return fmt.Sprintf("%d days", n)
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	}
	return d.String()
}

func humanAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}
