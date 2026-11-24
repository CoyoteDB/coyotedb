package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/pg"
	"coyotedb.com/coyote/internal/svc"
)

// Exit codes of `service status` (LSB-style).
const (
	exitRunning      = 0
	exitNotRunning   = 3
	exitNotInstalled = 4
)

func serviceUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: %[1]s service <command>

  install [--data-dir DIR] [--no-start]  run this node in the background, also after a reboot
  uninstall [--purge]                    remove the background service (the data stays unless --purge)
  start | stop                           start or stop the background service
  status [--json]                        exit code 0 running, 3 installed but stopped, 4 not installed
  run --data-dir DIR                     the service's own entry point (not for people)
`, brand.Command)
}

func cmdService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		serviceUsage(stderr)
		return 2
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("service "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	m := svc.New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	switch sub {
	case "install":
		dataDir := fs.String("data-dir", "", "the node's data folder (default: "+m.DefaultDataDir()+")")
		noStart := fs.Bool("no-start", false, "install only; start at the next boot or with `service start`")
		if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
			return 2
		}
		dir := *dataDir
		if dir != "" {
			abs, err := filepath.Abs(dir)
			if err != nil {
				fmt.Fprintf(stderr, "Error: %v\n", err)
				return 1
			}
			dir = abs
		}
		rep, err := m.Install(ctx, svc.Options{DataDir: dir, Start: !*noStart})
		printReport(stdout, rep)
		if err != nil {
			return svcFailCmd(stderr, err, "install", args)
		}
		fmt.Fprintf(stdout, "\nNext: `%s up` to start a new cluster here, `%s join` to join one (it asks for the invite code), or `%s setup` to do either in your browser. The background service does the work.\n", brand.Command, brand.Command, brand.Command)
		return 0
	case "uninstall":
		purge := fs.Bool("purge", false, "also delete the data folder (the database!) and the service user")
		if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
			return 2
		}
		rep, err := m.Uninstall(ctx, svc.UninstallOptions{Purge: *purge})
		printReport(stdout, rep)
		if err != nil {
			return svcFailCmd(stderr, err, "uninstall", args)
		}
		return 0
	case "start", "stop":
		if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
			return 2
		}
		op := m.Start
		if sub == "stop" {
			op = m.Stop
		}
		if err := op(ctx); err != nil {
			return svcFail(stderr, err)
		}
		fmt.Fprintf(stdout, "Background service %sed.\n", map[string]string{"start": "start", "stop": "stopp"}[sub])
		return 0
	case "status":
		asJSON := fs.Bool("json", false, "print as JSON")
		if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
			return 2
		}
		st, err := m.Status(ctx)
		if err != nil && !errors.Is(err, svc.ErrNotInstalled) {
			return svcFail(stderr, err)
		}
		code := exitNotInstalled
		switch {
		case st.Installed && st.Running:
			code = exitRunning
		case st.Installed:
			code = exitNotRunning
		}
		if *asJSON {
			b, _ := json.MarshalIndent(st, "", "  ")
			fmt.Fprintln(stdout, string(b))
			return code
		}
		switch code {
		case exitRunning:
			fmt.Fprintf(stdout, "Running (pid %d). Data folder: %s\n", st.PID, st.DataDir)
		case exitNotRunning:
			fmt.Fprintf(stdout, "Installed but not running (%s). Start it with `%s service start`.\n", st.State, brand.Command)
		default:
			fmt.Fprintf(stdout, "Not installed. Install it with `%s service install`.\n", brand.Command)
		}
		return code
	case "run":
		dataDir := fs.String("data-dir", "", "the node's data folder")
		if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 || *dataDir == "" {
			return 2
		}
		return serviceRun(*dataDir, stdout, stderr)
	case "help", "-h", "--help":
		serviceUsage(stdout)
		return 0
	}
	serviceUsage(stderr)
	return 2
}

func printReport(w io.Writer, rep svc.Report) {
	for _, s := range rep.Steps {
		fmt.Fprintf(w, "  %s\n", s)
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "%s\n", n)
	}
}

// svcFailCmd is svcFail for a command whose options matter: an
// "administrator rights needed" message repeats the whole command, with
// the person's own --data-dir and other options, ready to paste.
func svcFailCmd(stderr io.Writer, err error, sub string, args []string) int {
	if svc.IsElevationError(err) && len(args) > 0 {
		short := brand.Command + " service " + sub
		full := short
		for _, a := range args {
			full += " " + shellQuote(a)
		}
		msg := err.Error()
		if i := strings.LastIndex(msg, short); i >= 0 && strings.TrimSpace(msg[i+len(short):]) == "" {
			msg = msg[:i] + full
		}
		fmt.Fprintln(stderr, msg)
		return 1
	}
	return svcFail(stderr, err)
}

// shellQuote quotes an argument for pasting into a shell or PowerShell
// when it needs it.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'`$&|;<>()*?[]{}!#~") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func svcFail(stderr io.Writer, err error) int {
	if svc.IsElevationError(err) {
		fmt.Fprintln(stderr, err.Error())
		return 1
	}
	fmt.Fprintf(stderr, "Error: %v\n", err)
	return 1
}

// serviceRun is what the service manager runs.
func serviceRun(dataDir string, stdout, stderr io.Writer) int {
	node, err := pg.NewNode(dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return svc.ExitNoRestart
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
	log.Info("service run", "version", version, "data_dir", node.StateDir)
	err = svc.Run(func(ctx context.Context) error {
		return agent.Run(ctx, agent.Config{
			DataDir: node.StateDir, Mode: agent.ModeUp, Service: true,
			Out: stdout, Logger: log, LogPath: logPath, Version: version,
		})
	})
	var ee *agent.ExitError
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		return 0
	case errors.As(err, &ee):
		// A refusal a restart cannot fix (removed from the cluster, a data
		// folder from another cluster): systemd does not restart for it.
		log.Error("service stopped", "err", err)
		fmt.Fprintln(stderr, ee.Msg)
		return svc.ExitNoRestart
	}
	log.Error("service failed", "err", err)
	fmt.Fprintf(stderr, "Error: %v\n", err)
	return 1
}

// ---- up / join / witness with the background service installed ----

// installedService returns the service's status if it is installed and
// runs the data folder dir ("" = the default folder).
func installedService(dir string) (svc.Status, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := svc.New(nil).Status(ctx)
	if err != nil || !st.Installed || st.DataDir == "" {
		return st, false
	}
	if dir == "" {
		return st, true
	}
	a, err1 := filepath.Abs(dir)
	b, err2 := filepath.Abs(st.DataDir)
	return st, err1 == nil && err2 == nil && filepath.Clean(a) == filepath.Clean(b)
}

// handOver gives the command to the background service instead of running
// a second agent in this terminal.
func handOver(st svc.Status, req agent.ServiceRequest, stdout, stderr io.Writer) int {
	node, err := pg.NewNode(st.DataDir)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	member := isClusterNode(node)
	if _, err := os.Stat(filepath.Join(node.StateDir, "cluster")); errors.Is(err, fs.ErrPermission) {
		fmt.Fprintf(stderr, "The background service runs this node, and its data folder %s needs administrator rights to open. Run the command again with sudo (Windows: an administrator PowerShell).\n", node.StateDir)
		return 1
	}
	m := svc.New(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	switch {
	case member && req.Mode != "up":
		fmt.Fprintf(stderr, "This machine is already in a cluster; the background service runs it. See `%s status`.\n", brand.Command)
		return 1
	case !member:
		if err := writeServiceRequest(node.StateDir, req); err != nil {
			if errors.Is(err, fs.ErrPermission) {
				fmt.Fprintf(stderr, "The background service runs this node; handing it the request needs administrator rights. Run the command again with sudo (Windows: an administrator PowerShell).\n")
				return 1
			}
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
	}
	if !st.Running {
		if err := m.Start(ctx); err != nil {
			return svcFail(stderr, err)
		}
	}
	fmt.Fprintf(stdout, "The background service runs this node (data folder %s).\n", node.StateDir)
	if member {
		fmt.Fprintf(stdout, "It is running. See `%s status`.\n", brand.Command)
		return 0
	}
	// Wait until the service took the request and the node answers.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if s, err := m.Status(ctx); err == nil && !s.Running {
			fmt.Fprintf(stderr, "The background service stopped. Details: %s\n", filepath.Join(node.LogDir(), brand.Command+".log"))
			return 1
		}
		_, reqErr := os.Stat(agent.ServiceRequestPath(node.StateDir))
		if errors.Is(reqErr, fs.ErrNotExist) && isClusterNode(node) {
			if c, err := control.NewClient(node.StateDir, 0); err == nil {
				var s agent.Status
				cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
				err := c.Do(cctx, "GET", "/status", nil, &s)
				ccancel()
				if err == nil {
					fmt.Fprintf(stdout, "Done: this machine is in the cluster. Follow it with `%s status`.\n", brand.Command)
					return 0
				}
			}
		}
		time.Sleep(time.Second)
	}
	fmt.Fprintf(stdout, "The background service is still working on it. Follow it with `%s status`; details in %s\n",
		brand.Command, filepath.Join(node.LogDir(), brand.Command+".log"))
	return 0
}

func writeServiceRequest(dir string, req agent.ServiceRequest) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	p := agent.ServiceRequestPath(dir)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	// The service runs as its own user; the file must be that user's.
	if err := chownLike(tmp, dir); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}
