// Command coyote runs a PostgreSQL node. See README.md and docs/.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/invite"
	"coyotedb.com/coyote/internal/paths"
	"coyotedb.com/coyote/internal/pg"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		guide(stdout)
		return 0
	}
	switch args[0] {
	case "up":
		return cmdAgent(agent.ModeUp, args[1:], stdout, stderr)
	case "setup":
		return cmdSetup(args[1:], stdout, stderr)
	case "join":
		return cmdAgent(agent.ModeJoin, args[1:], stdout, stderr)
	case "witness":
		return cmdAgent(agent.ModeWitness, args[1:], stdout, stderr)
	case "invite":
		return cmdInvite(args[1:], stdout, stderr)
	case "status":
		return cmdStatus(args[1:], stdout, stderr)
	case "leave":
		return cmdLeave(args[1:], stdout, stderr)
	case "remove":
		return cmdRemove(args[1:], stdout, stderr)
	case "conninfo":
		return cmdConninfo(args[1:], stdout, stderr)
	case "dashboard":
		return cmdDashboard(args[1:], stdout, stderr)
	case "migrate":
		return cmdMigrate(args[1:], stdout, stderr)
	case "node":
		return cmdNode(args[1:], stdout, stderr)
	case "explain":
		return cmdExplain(args[1:], stdout, stderr)
	case "service":
		return cmdService(args[1:], stdout, stderr)
	case "switch":
		return cmdSwitch(args[1:], stdout, stderr)
	case "settings":
		return cmdSettings(args[1:], stdout, stderr)
	case "backup":
		return cmdBackup(args[1:], stdout, stderr)
	case "dev":
		return cmdDev(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "%s %s (PostgreSQL %s, %s/%s)\n", brand.Command, version, pg.Version, runtime.GOOS, runtime.GOARCH)
		return 0
	case pg.WatchdogCommand:
		return pg.RunWatchdog(args[1:])
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "Unknown command %q.\n\n", args[0])
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `%[1]s runs a PostgreSQL database for you, on one machine or several.

Usage:
  %[2]s up                 start this node; on a fresh machine, create a new cluster
  %[2]s setup              the same first run in your web browser: create a cluster or join one
  %[2]s invite [--ttl 1h]  print a code another machine uses to join
  %[2]s join               join a cluster: copy the data and follow it live (asks for the code)
  %[2]s witness            join as a witness: votes, keeps no data (asks for the code)
  %[2]s status [--json]    who is in the cluster, who is the main node, replication health;
                           --details: the long form of every problem (causes, evidence, commands)
  %[2]s explain [CODE]     what a problem code means and how to fix it (works offline)
  %[2]s leave              take this node out of the cluster
  %[2]s remove <node>      take another node out of the cluster
  %[2]s conninfo           print the connection string for applications (with password)
  %[2]s dashboard          open the web dashboard (a one-time sign-in link, this computer only)
  %[2]s migrate            copy an existing PostgreSQL database into the cluster (asks for its
                           connection string; --dry-run checks first, --live for a short stop)
  %[2]s switch --to <node> make another node the main node (planned, no data lost)
  %[2]s node set-address <node> HOST:PORT,...
                           change the addresses other nodes use to reach <node>; checked
                           first (it must answer as <node>). Run it when status says
                           address_gone. --local: only on this computer, when the cluster
                           has no leader; --force: record it although it does not answer
  %[2]s settings [--json]  show the cluster settings; "settings set key=value ..." changes them
  %[2]s backup ...         backups: where they go, restore to a moment, check one ("backup help")
  %[2]s service ...         run this node in the background (install, uninstall, start, stop, status)
  %[2]s version            print the version
  %[2]s dev ...            TEMPORARY tools (manual switch, sync set; unstable)

Options for up, join and witness:
  --data-dir DIR           where this node keeps its data (default: %[3]s)
  --name NAME              this node's name (default: from the host name)
  --listen HOST:PORT       cluster port (default %[4]s)
  --advertise HOST:PORT,.. addresses other nodes use (default: detected at the first start,
                           Tailscale first, then the local network, and kept; change it
                           later with "node set-address")
  --client-port N          port applications connect to (default %[7]d)
  --pg-port N              Postgres port on 127.0.0.1 (default %[5]d)
  --control-port N         local control port on 127.0.0.1 (default %[6]d)
  --foreground             run in this terminal even if the background service is installed
  --invite-file FILE       (join, witness) read the invite code from FILE ("-" = standard
                           input); or set %[8]s; else the command asks for it
  --dashboard-listen HOST:PORT
                           (containers) let the browser on the container's host reach the
                           dashboard; publish it as -p 127.0.0.1:PORT:PORT. Off by default
  --allow-mixed-os         (join) accept a cluster whose data nodes are a different kind of
                           computer (operating system or processor); still refused unless
                           this machine stores and compares data exactly like them. In a
                           mixed cluster text sorts only with the built-in rules, and the
                           results of decimal maths like corr() may differ in the last
                           digits after a failover; the stored data itself never changes

Which computers to use: Linux is the recommended system for database nodes.
macOS and Windows are supported too. A witness can run on anything.

Environment: %[9]s is the same as --advertise.

Run "%[2]s <command> -h" for all options of a command.
`, brand.Product, brand.Command, defaultDataDirHint(), agent.DefaultListen, pg.DefaultPort, control.DefaultPort, agent.DefaultClientPort, inviteEnv, advertiseEnv)
}

// dataDirFlag adds --data-dir with the OS default.
func dataDirFlag(fs *flag.FlagSet) *string {
	return fs.String("data-dir", "", "where this node keeps its data (default: "+defaultDataDirHint()+")")
}

func controlPortFlag(fs *flag.FlagSet) *int {
	return fs.Int("control-port", 0, fmt.Sprintf("the node's local control port (default: as saved, else %d)", control.DefaultPort))
}

func defaultDataDirHint() string {
	d, err := paths.DefaultDataDir()
	if err != nil {
		return "OS default"
	}
	return d
}

func resolveDataDir(s string) (pg.Node, error) {
	if s == "" {
		if st, ok := installedService(""); ok {
			return pg.NewNode(st.DataDir) // the service's folder
		}
		d, err := paths.DefaultDataDir()
		if err != nil {
			return pg.Node{}, fmt.Errorf("pick a default data dir: %w", err)
		}
		s = d
	}
	return pg.NewNode(s)
}

// alreadyJoined reports whether the data folder already belongs to a
// cluster (its node identity exists).
func alreadyJoined(dataDir string) bool {
	node, err := resolveDataDir(dataDir)
	if err != nil {
		return false
	}
	has, err := cluster.HasState(filepath.Join(node.StateDir, "cluster"))
	return err == nil && has
}

// parseArgs parses flags that may come before or after positional
// arguments (`join <code> --name b` and `join --name b <code>`).
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func isClusterNode(node pg.Node) bool {
	has, err := cluster.HasState(filepath.Join(node.StateDir, "cluster"))
	return err == nil && has
}

func cmdAgent(mode agent.Mode, args []string, stdout, stderr io.Writer) int {
	return runAgentCmd(map[agent.Mode]string{agent.ModeUp: "up", agent.ModeJoin: "join", agent.ModeWitness: "witness"}[mode], mode, args, stdout, stderr)
}

// cmdSetup is `coyote setup`: start this computer's node and finish the
// first run in the browser (create a cluster, or join one with an invite
// code; D50).
func cmdSetup(args []string, stdout, stderr io.Writer) int {
	return runAgentCmd("setup", agent.ModeUp, args, stdout, stderr)
}

func runAgentCmd(cmd string, mode agent.Mode, args []string, stdout, stderr io.Writer) int {
	wizard := cmd == "setup"
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	name := fs.String("name", "", "this node's name: a-z, 0-9, '-' (default: from the host name; fixed once in a cluster)")
	listen := fs.String("listen", "", "cluster port to listen on (default: as saved, else "+agent.DefaultListen+")")
	advertise := fs.String("advertise", "", "comma-separated HOST:PORT addresses other nodes use to reach this one (default: $"+advertiseEnv+", else detected)")
	port := fs.Int("pg-port", 0, fmt.Sprintf("port for Postgres on 127.0.0.1 (default: as saved, else %d)", pg.DefaultPort))
	ctlPort := controlPortFlag(fs)
	clientPort := fs.Int("client-port", 0, fmt.Sprintf("port applications connect to (data nodes; default: as saved, else %d)", agent.DefaultClientPort))
	archive := fs.String("pg-archive", "", "use this Postgres .tar.gz instead of downloading (still checksum-verified)")
	foreground := fs.Bool("foreground", false, "run in this terminal even if the background service is installed")
	cacheDir := fs.String("pg-cache", "", "where to keep the Postgres program files (default: $"+brand.EnvPrefix+"PG_CACHE or <data-dir>/cache)")
	dashListen := fs.String("dashboard-listen", "", "also accept dashboard browsers that are not on this computer's loopback, on HOST:PORT "+
		"(for a container whose port is published to its host as -p 127.0.0.1:PORT:PORT; the sign-in link is still required). Default: off")
	allowMixed := new(bool)
	inviteFile := new(string)
	if mode == agent.ModeJoin {
		allowMixed = fs.Bool("allow-mixed-os", false, "join although the cluster's data nodes are a different kind of computer (operating system or processor); "+
			"refused anyway unless this machine stores and compares data exactly like them. Linux is the recommended system for database nodes")
	}
	if mode != agent.ModeUp {
		inviteFile = fs.String("invite-file", "", "read the invite code from this file (\"-\" = standard input) instead of asking for it")
	}
	noOpen := new(bool)
	if wizard {
		noOpen = fs.Bool("no-open", false, "only print the sign-in link, do not open a browser")
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	var code string
	switch {
	case mode == agent.ModeUp && len(pos) > 0:
		fmt.Fprintf(stderr, "Unexpected argument %q. To join an existing cluster, use `%s join` (it asks for the invite code).\n", pos[0], brand.Command)
		return 2
	case mode != agent.ModeUp:
		c, warn, err := readInvite(inviteSource{
			Positional: pos, File: *inviteFile, Env: os.Getenv(inviteEnv),
			Stdin: os.Stdin, StdinTTY: isTerminal(os.Stdin), Prompt: stderr, Cmd: cmd,
		})
		// The code must not reach anything this process starts.
		_ = os.Unsetenv(inviteEnv)
		switch {
		case errors.Is(err, errNoInvite) && alreadyJoined(*dataDir):
			// A folder that already joined needs no code: `join` just
			// starts it, like `up` (D62; Docker re-creates a container
			// with the same command line).
			fmt.Fprintln(stderr, "This machine already joined the cluster; starting it (no invite code needed).")
		case errors.Is(err, errNoInvite):
			fmt.Fprintln(stderr, noInviteHelp(cmd))
			return 2
		case err != nil:
			fmt.Fprintf(stderr, "%s.\n%s\n", upperFirst(err.Error()), noInviteHelp(cmd))
			return 2
		}
		if warn != "" {
			fmt.Fprintln(stderr, warn)
		}
		code = c
		// Checked here, before any setup or network use.
		if _, err := invite.Decode(code); err != nil && code != "" {
			fmt.Fprintf(stderr, "That invite code does not work: %v. Copy the whole code again from `%s invite`.\n", err, brand.Command)
			return 1
		}
	}
	if *port < 0 || *port > 65535 || *ctlPort < 0 || *ctlPort > 65535 || *clientPort < 0 || *clientPort > 65535 {
		fmt.Fprintln(stderr, "Ports must be between 1 and 65535.")
		return 2
	}
	advText := *advertise
	if advText == "" {
		// COYOTE_ADVERTISE is the same as --advertise (containers, D51).
		advText = os.Getenv(advertiseEnv)
	}
	var adv []string
	for a := range strings.SplitSeq(advText, ",") {
		if a = strings.TrimSpace(a); a != "" {
			adv = append(adv, a)
		}
	}
	if *dashListen != "" {
		p, err := checkDashListen(*dashListen, *ctlPort)
		if err != nil {
			fmt.Fprintf(stderr, "--dashboard-listen %s: %v.\n", *dashListen, err)
			return 2
		}
		*ctlPort = p
	}
	if !*foreground {
		if st, ok := installedService(*dataDir); ok {
			if wizard {
				// The service already serves the setup page (D50): sign in.
				return cmdDashboard(append(dataDirArgs(st.DataDir), noOpenArgs(*noOpen)...), stdout, stderr)
			}
			return handOver(st, agent.ServiceRequest{
				Mode: cmd, Invite: code, Name: *name, Listen: *listen, Advertise: adv,
				PGPort: *port, ClientPort: *clientPort, ControlPort: *ctlPort, AllowMixedOS: *allowMixed,
			}, stdout, stderr)
		}
	}
	node, err := resolveDataDir(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if err := node.Prepare(); err != nil {
		fmt.Fprintf(stderr, "Error: could not create the data folder %s: %v\n%s\n", node.StateDir, err, prepareHint(node.StateDir, err))
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
	log.Info(cmd, "version", version, "data_dir", node.StateDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := agent.Config{
		DataDir: node.StateDir, Mode: mode, Invite: code, Name: *name,
		Listen: *listen, Advertise: adv, PGPort: *port, ControlPort: *ctlPort, ClientPort: *clientPort,
		Archive: *archive, Cache: *cacheDir, Out: stdout, Logger: log, LogPath: logPath, Version: version,
		AllowMixedOS: *allowMixed, Wizard: wizard, DashboardListen: *dashListen,
	}
	if wizard && !*noOpen {
		cfg.OpenLink = func(url string) bool {
			if err := openBrowser(url); err != nil {
				log.Warn("could not open a browser", "err", err)
				return false
			}
			return true
		}
	}
	err = agent.Run(ctx, cfg)
	var ee *agent.ExitError
	switch {
	case err == nil:
		fmt.Fprintln(stdout, "Stopped.")
		return 0
	case errors.As(err, &ee):
		log.Error(cmd+" stopped", "err", err)
		fmt.Fprintf(stderr, "\n%s\n", ee.Msg)
		return ee.Code
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(stdout, "Cancelled.")
		return 130
	}
	log.Error(cmd+" failed", "err", err)
	fmt.Fprintf(stderr, "\nError: %v\n%s\nMore detail in %s\n", err, agent.NextStep(err, node.StateDir), logPath)
	return 1
}

func dataDirArgs(dir string) []string { return []string{"--data-dir", dir} }

func noOpenArgs(noOpen bool) []string {
	if noOpen {
		return []string{"--no-open"}
	}
	return nil
}

// client connects to the running agent of --data-dir.
func client(dataDir string, port int) (*control.Client, pg.Node, error) {
	node, err := resolveDataDir(dataDir)
	if err != nil {
		return nil, node, err
	}
	c, err := control.NewClient(node.StateDir, port)
	return c, node, err
}

// apiFail prints a control API error in plain words and returns the exit code.
func apiFail(stderr io.Writer, node pg.Node, err error) int {
	if errors.Is(err, control.ErrNotRunning) {
		fmt.Fprintf(stderr, "This node is not running. Start it with `%s up --data-dir %s`.\n", brand.Command, quoteArg(node.StateDir))
		return 3
	}
	var ae *control.APIError
	if errors.As(err, &ae) {
		if ae.Status == http.StatusUnauthorized {
			fmt.Fprintln(stderr, "The node refused the request: the control token in the data folder does not match (was the folder copied?).")
			return 1
		}
		fmt.Fprintf(stderr, "%s\n", upperFirst(ae.Message))
		return 1
	}
	fmt.Fprintf(stderr, "Error: %v\n", err)
	return 1
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func cmdInvite(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	ctlPort := controlPortFlag(fs)
	ttl := fs.Duration("ttl", time.Hour, "how long the code stays valid (it works once)")
	asJSON := fs.Bool("json", false, "print JSON")
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
		return 2
	}
	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var r agent.InviteResponse
	if err := c.Do(ctx, http.MethodPost, "/invites", agent.InviteRequest{TTL: ttl.String()}, &r); err != nil {
		return apiFail(stderr, node, err)
	}
	if *asJSON {
		return printJSON(stdout, r)
	}
	fmt.Fprintf(stdout, "Invite code (works once, valid until %s):\n\n", r.Expires.Local().Format("15:04 Mon 2 Jan"))
	fmt.Fprintln(stdout, r.Code)
	fmt.Fprintf(stdout, "\nOn the new machine run `%s join` (or `%s witness` for a voter without data) and paste the code when it asks.\n", brand.Command, brand.Command)
	fmt.Fprintln(stdout, "Linux is the recommended system for database nodes; macOS and Windows are supported too.")
	return 0
}

func printJSON(w io.Writer, v any) int {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	if err := e.Encode(v); err != nil {
		return 1
	}
	return 0
}

func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	ctlPort := controlPortFlag(fs)
	asJSON := fs.Bool("json", false, "print JSON")
	details := fs.Bool("details", false, "the long form of every problem: what happened, likely causes, evidence, fix commands")
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
		return 2
	}
	node, err := resolveDataDir(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	c, err := control.NewClient(node.StateDir, *ctlPort)
	var st agent.Status
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err = c.Do(ctx, http.MethodGet, "/status", nil, &st)
	}
	if errors.Is(err, control.ErrNotRunning) && !isClusterNode(node) {
		return legacyStatus(node, stdout, stderr)
	}
	if err != nil {
		if *asJSON && errors.Is(err, control.ErrNotRunning) {
			printJSON(stdout, map[string]any{"running": false})
			return 3
		}
		if errors.Is(err, control.ErrNotRunning) {
			fmt.Fprintln(stdout, "Not running.")
			return 3
		}
		return apiFail(stderr, node, err)
	}
	if *asJSON {
		return printJSON(stdout, st)
	}
	printStatus(stdout, st, *details)
	return 0
}

// legacyStatus is the M1 answer for a node that is not in a cluster.
func legacyStatus(node pg.Node, stdout, stderr io.Writer) int {
	st, err := node.Status()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if !st.Running {
		fmt.Fprintln(stdout, "Not running.")
		return 3
	}
	fmt.Fprintf(stdout, "Running: pid %d, 127.0.0.1:%d, state %s.\n", st.PID, st.Port, st.State)
	return 0
}

// printMixed shows, for a cluster that mixes kinds of machines (D37), each
// data node's system, fingerprint and latest checks.
func printMixed(w io.Writer, s agent.Status) {
	if !s.Cluster.MixedOS {
		return
	}
	fmt.Fprintln(w, "\nMixed systems: on. Each data node checks the data can be shared between kinds of machines (after every change of the database's structure, and every 10 minutes).")
	fmt.Fprintln(w, "  Text sorts with the built-in rules only. After a failover, results of decimal maths like corr() may differ in the last digits; the stored data never changes.")
	for _, n := range s.Nodes {
		if n.Role != string(cluster.RoleData) {
			continue
		}
		fp := n.Fingerprint
		if fp == "" {
			fp = "not measured yet"
		}
		checks := "no checks yet"
		if g := n.Guard; g != nil {
			var parts []string
			if !g.AuditAt.IsZero() {
				r := "clean"
				if g.AuditBlocking {
					r = "PROBLEM"
				}
				parts = append(parts, fmt.Sprintf("audit %s %s ago", r, time.Since(g.AuditAt).Round(time.Second)))
			}
			if !g.VerifyAt.IsZero() {
				r := "clean"
				if g.VerifyDamaged {
					r = "DAMAGE"
				}
				parts = append(parts, fmt.Sprintf("integrity %s %s ago", r, time.Since(g.VerifyAt).Round(time.Second)))
			}
			if g.Drift != "" {
				parts = append(parts, "CHANGED since joining")
			}
			if len(parts) > 0 {
				checks = strings.Join(parts, ", ")
			}
		}
		fmt.Fprintf(w, "  %s: %s, fingerprint %s; %s\n", n.Name, n.System, fp, checks)
	}
}

func printStatus(w io.Writer, s agent.Status, details bool) {
	main := s.Cluster.PrimaryName
	if main == "" {
		main = "none"
	}
	leader := s.Cluster.LeaderName
	if leader == "" {
		leader = "none"
	}
	if s.Waiting != nil {
		// No copy of the cluster state yet (D62): nothing below is known.
		fmt.Fprintln(w, "Waiting to reach the other nodes: this node has not heard from the cluster since it started.")
		printProblems(w, s, details)
		return
	}
	fmt.Fprintf(w, "Cluster %s, term %d. Main node: %s. Leader: %s.\n", short(s.Cluster.ID), s.Cluster.Epoch, main, leader)
	h := s.Health
	if s.Cluster.PrimaryName != "" {
		switch h.Lease {
		case "renewed":
			fmt.Fprintf(w, "Main node lease: healthy (%s keeps renewing it).\n", main)
		case "unknown":
			fmt.Fprintln(w, "Main node lease: unknown (the leader did not answer).")
		default:
			fmt.Fprintf(w, "Main node lease: %s.\n", h.Lease)
		}
	}
	if h.Copies != "" {
		fmt.Fprintln(w, h.Copies)
	}
	if len(s.Cluster.SyncSet) > 0 {
		fmt.Fprintf(w, "Sync set (nodes that may take over): %s; each write needs %d of them.\n", strings.Join(s.Cluster.SyncSet, ", "), max(s.Cluster.SyncK, 1))
	}
	if s.Cluster.Mode == cluster.ModeStrict {
		fmt.Fprintln(w, "Mode: strict (writes wait rather than run on one copy).")
	}
	if h.Failover != "" {
		fmt.Fprintf(w, "Automatic failover: %s\n", h.Failover)
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tKIND\tVOTES\tDOING\tREPLICATION\tBEHIND\t")
	for _, n := range s.Nodes {
		name := n.Name
		if n.This {
			name += " (this)"
		}
		kind := "data"
		if n.Role == string(cluster.RoleWitness) {
			kind = "witness"
		}
		votes := "yes"
		if n.Suffrage != string(cluster.Voter) {
			votes = "no"
		}
		doing := "unreachable"
		if n.Report != nil {
			doing = describe(*n.Report)
		}
		repl := n.Replication
		if repl == "" {
			repl = "-"
		}
		behind := "-"
		if n.LagBytes >= 0 && n.Replication != "" {
			behind = humanBytes(n.LagBytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t\n", name, kind, votes, doing, repl, behind)
	}
	tw.Flush()
	printMixed(w, s)
	printBackupStatus(w, s.Backup) // backup.go (D63)
	printProblems(w, s, details)
}

func humanBytes(n int64) string {
	switch {
	case n == 0:
		return "0 (caught up)"
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
}

func describe(r agent.NodeReport) string {
	switch r.State {
	case "primary":
		return "main node (read-write)"
	case "standby":
		if r.PG != nil && r.PG.Streaming {
			return "copy, following live"
		}
		return "copy, not streaming"
	case "witness":
		return "witness"
	case "cloning":
		if r.Progress != nil {
			return fmt.Sprintf("copying data %d%%", *r.Progress)
		}
		return "copying data"
	case "":
		return "starting"
	case "waiting":
		if r.Marker != "" {
			return "stopped, will rejoin as a copy"
		}
		return "waiting"
	}
	return r.State
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func cmdLeave(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("leave", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	ctlPort := controlPortFlag(fs)
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
		return 2
	}
	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var r map[string]string
	if err := c.Do(ctx, http.MethodPost, "/leave", nil, &r); err != nil {
		return apiFail(stderr, node, err)
	}
	fmt.Fprintln(stdout, r["message"])
	return 0
}

func cmdRemove(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	ctlPort := controlPortFlag(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintf(stderr, "Usage: %s remove <node name>\n", brand.Command)
		return 2
	}
	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var r map[string]string
	if err := c.Do(ctx, http.MethodDelete, "/members/"+pos[0], nil, &r); err != nil {
		return apiFail(stderr, node, err)
	}
	fmt.Fprintln(stdout, r["message"])
	return 0
}

func cmdConninfo(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("conninfo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	ctlPort := controlPortFlag(fs)
	asJSON := fs.Bool("json", false, "print as JSON")
	admin := fs.Bool("admin", false, "print the superuser connection string instead: this machine only, straight to the local PostgreSQL (not through the client port)")
	newPW := fs.Bool("new-password", false, "give the application role a new random password (the old one stops working within seconds; open sessions stay)")
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
		return 2
	}
	if *admin {
		node, err := resolveDataDir(*dataDir)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		info, err := node.ReadInfo()
		if err != nil {
			fmt.Fprintf(stderr, "This node has no database (a witness, or not started yet): %v\n", err)
			return 1
		}
		pw, err := node.Password()
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, pg.ConnString(info.Port, pw, true))
		return 0
	}
	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var ci agent.ConnInfo
	method, path := http.MethodGet, "/conninfo"
	if *newPW {
		method, path = http.MethodPost, "/app-password"
	}
	if err := c.Do(ctx, method, path, nil, &ci); err != nil {
		return apiFail(stderr, node, err)
	}
	if *asJSON {
		return printJSON(stdout, ci)
	}
	if ci.Pending {
		fmt.Fprintf(stderr, "Not ready yet: %s.\n", ci.Note)
		return 1
	}
	fmt.Fprintln(stdout, ci.URL)
	fmt.Fprintf(stderr, "\nUser %s, database %s, TLS required. Any of the hosts works: each one passes the connection to the main node.\n"+
		"On another machine, copy %s there and point sslrootcert at it.\n", ci.User, ci.Database, ci.CAFile)
	if ci.CANote != "" {
		fmt.Fprintf(stderr, "Note: %s\n", ci.CANote)
	}
	if *newPW {
		fmt.Fprintln(stderr, "The password was changed; applications must use the new one.")
	}
	return 0
}

func cmdNode(args []string, stdout, stderr io.Writer) int {
	usageLine := func() {
		fmt.Fprintf(stderr, `Usage: %[1]s node set-address <node> HOST:PORT[,HOST:PORT...] [--local] [--force]

Changes the addresses the other computers use to reach <node>. Each address
is checked first from this computer: it must answer as <node> of this
cluster, or nothing changes. Run it on any computer of the cluster when
`+"`%[1]s status`"+` shows address_gone, with the address it names:

  %[1]s node set-address node2 192.168.1.35:7481

  --local  only tell this computer where <node> is, without the cluster: for
           when the cluster has no leader because the computers got new
           addresses. Run it on each computer, for each other node.
  --force  record the address although it does not answer from here (the
           node is down, or only other computers can reach it).
`, brand.Command)
	}
	if len(args) == 0 || args[0] != "set-address" {
		usageLine()
		return 2
	}
	fs := flag.NewFlagSet("node set-address", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	ctlPort := controlPortFlag(fs)
	force := fs.Bool("force", false, "record the addresses even if none answers from this machine (the node is down)")
	local := fs.Bool("local", false, "only tell this machine where <node> is, without the cluster (when the cluster has no leader because the computers got new addresses)")
	asJSON := fs.Bool("json", false, "print as JSON")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return 2
	}
	if len(pos) != 2 {
		usageLine()
		return 2
	}
	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req := agent.SetAddressRequest{Addresses: strings.Split(pos[1], ","), Force: *force, Local: *local}
	var resp agent.SetAddressResponse
	if err := c.Do(ctx, http.MethodPost, "/members/"+pos[0]+"/addresses", req, &resp); err != nil {
		var ae *control.APIError
		if errors.As(err, &ae) && ae.Code != "" && ae.Status != http.StatusUnauthorized {
			// Code, plain sentence, next command (D66).
			fmt.Fprintf(stderr, "Not changed [%s]: %s\n", ae.Code, upperFirst(ae.Message))
			switch ae.Code {
			case "no_leader", "not_leader":
				fmt.Fprintf(stderr, "Next: %s node set-address --local %s %s   (on each other computer; no leader needed)\n", brand.Command, pos[0], pos[1])
			case "unknown_member":
				fmt.Fprintf(stderr, "Next: %s status   (lists the node names)\n", brand.Command)
			case "bad_request":
				fmt.Fprintf(stderr, "Next: %s node set-address %s HOST:PORT   (for example 192.168.1.35:7481)\n", brand.Command, pos[0])
			default:
				fmt.Fprintf(stderr, "More: %s explain %s\n", brand.Command, ae.Code)
			}
			return 1
		}
		return apiFail(stderr, node, err)
	}
	if *asJSON {
		if code := printJSON(stdout, resp); code != 0 || resp.Recorded {
			return code
		}
		return 1
	}
	fmt.Fprintf(stdout, "Checking from this computer that %s answers there:\n", resp.Node)
	for _, p := range resp.Probes {
		if p.OK {
			fmt.Fprintf(stdout, "  %-28s answers as %s (%.1f ms)\n", p.Addr, resp.Node, p.RTTMS)
		} else {
			fmt.Fprintf(stdout, "  %-28s no [%s]: %s\n", p.Addr, p.Code, p.Error)
		}
	}
	if !resp.Recorded {
		fmt.Fprintf(stderr, "Not changed [%s]: %s\n", cmp.Or(resp.Code, "address_no_answer"), resp.Message)
		switch {
		case resp.Code == "address_not_this_node":
			fmt.Fprintf(stderr, "Next: check which computer has %s now, then %s node set-address %s HOST:PORT. More: %s explain %s\n", pos[1], brand.Command, pos[0], brand.Command, resp.Code)
		case *local:
			fmt.Fprintf(stderr, "Next: start %s, or check the address; to keep it anyway: %s node set-address --local --force %s %s\n", resp.Node, brand.Command, pos[0], pos[1])
		default:
			fmt.Fprintf(stderr, "Next: check the address and that %s runs; to record it anyway: %s node set-address --force %s %s\n", resp.Node, brand.Command, pos[0], pos[1])
		}
		return 1
	}
	fmt.Fprintln(stdout, resp.Message)
	return 0
}

// quoteArg quotes a path for display if it contains spaces.
func quoteArg(s string) string {
	for _, r := range s {
		if r == ' ' || r == '\t' {
			return `"` + s + `"`
		}
	}
	return s
}
