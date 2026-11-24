// Package svc runs the agent in the background on Linux (systemd), macOS
// (a LaunchDaemon) and Windows (a service under the virtual account
// NT SERVICE\coyote), and installs / removes that background service.
//
//	m := svc.New(logger)
//	rep, err := m.Install(ctx, svc.Options{Start: true})
//	rep, err := m.Uninstall(ctx, svc.UninstallOptions{Purge: false})
//	m.Start(ctx); m.Stop(ctx); st, err := m.Status(ctx)
//	svc.Run(func(ctx context.Context) error { ... })  // the service entry point
//
// Install is idempotent (running it again converges the system to the
// same result and says what changed) and Uninstall reverses it. Uninstall
// never deletes the data folder unless Purge is set, and its report says
// where the data is. Every OS command runs with an argument list, never
// through a shell.
package svc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"coyotedb.com/coyote/internal/brand"
)

// Names used on every platform.
const (
	// Name is the service name (systemd unit coyote.service, Windows
	// service "coyote", virtual account NT SERVICE\coyote).
	Name = brand.Command
	// DisplayName is what service managers show.
	DisplayName = brand.Product
	// Description is the one-line description.
	Description = brand.Product + " database agent (PostgreSQL kept in sync across machines)"
)

// Default ports opened by the Windows firewall rule: the agent-to-agent
// port and the client (proxy) port. See ARCHITECTURE.md, Ports.
var DefaultFirewallPorts = []int{7481, 5432}

// ExitNoRestart is the exit code the agent should use for a permanent
// error (removed from the cluster, unusable config): systemd does not
// restart the service for it (RestartPreventExitStatus). launchd and the
// Windows service manager have no equivalent and restart with backoff.
const ExitNoRestart = 78

// Options configure Install.
type Options struct {
	// Executable is the absolute path of the coyote program the service
	// runs. Default: this program (os.Executable).
	Executable string
	// Args are the arguments of the service entry point. Default
	// DefaultArgs(DataDir).
	Args []string
	// DataDir is the node's data folder. Default DefaultDataDir().
	DataDir string
	// FirewallPorts (Windows only) are opened, for the agent program
	// only, on private and domain networks. Default DefaultFirewallPorts.
	FirewallPorts []int
	// Start the service now as well (otherwise it starts at next boot or
	// on Start).
	Start bool
}

// DefaultArgs is the service command line after the program path.
func DefaultArgs(dataDir string) []string {
	return []string{"service", "run", "--data-dir", dataDir}
}

// UninstallOptions configure Uninstall.
type UninstallOptions struct {
	// Purge also deletes the data folder (the database!) and the service
	// user. Never the default.
	Purge bool
}

// Status describes the installed service.
type Status struct {
	Installed  bool   `json:"installed"`
	Running    bool   `json:"running"`
	PID        int    `json:"pid,omitempty"`
	State      string `json:"state,omitempty"` // the service manager's own word
	Definition string `json:"definition,omitempty"`
	DataDir    string `json:"data_dir,omitempty"`
}

// Report lists, in plain words, what Install or Uninstall did.
type Report struct {
	Steps   []string `json:"steps"`
	DataDir string   `json:"data_dir,omitempty"`
	// Notes are things the person should know afterwards (where the data
	// is, a restart that is needed).
	Notes []string `json:"notes,omitempty"`
}

func (r *Report) step(format string, a ...any) { r.Steps = append(r.Steps, fmt.Sprintf(format, a...)) }
func (r *Report) note(format string, a ...any) { r.Notes = append(r.Notes, fmt.Sprintf(format, a...)) }

// ErrNotInstalled is returned by Start/Stop when there is no service.
var ErrNotInstalled = errors.New("the background service is not installed (run: " + brand.Command + " service install)")

// ErrUnsupported is returned on a system this package cannot manage.
var ErrUnsupported = errors.New("background service is not supported on this system")

// ElevationError means the command needs administrator rights.
type ElevationError struct{ Msg string }

func (e *ElevationError) Error() string { return e.Msg }

// IsElevationError reports whether err asks the person to re-run elevated.
func IsElevationError(err error) bool {
	var e *ElevationError
	return errors.As(err, &e)
}

func elevationError(goos, action string) error {
	cmd := brand.Command + " service " + action
	if goos == "windows" {
		return &ElevationError{Msg: "This needs administrator rights.\n" +
			"Open PowerShell as administrator (right-click PowerShell, choose \"Run as administrator\") and run:\n\n    " + cmd}
	}
	return &ElevationError{Msg: "This needs administrator rights.\nRun it again with sudo:\n\n    sudo " + cmd}
}

// platform is one OS's implementation.
type platform interface {
	install(ctx context.Context, o Options) (Report, error)
	uninstall(ctx context.Context, o UninstallOptions) (Report, error)
	start(ctx context.Context) error
	stop(ctx context.Context) error
	status(ctx context.Context) (Status, error)
	defaultDataDir() string
}

// Manager installs and controls the background service.
type Manager struct {
	p   platform
	log *slog.Logger
}

// New returns a Manager for this OS.
func New(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{p: newPlatform(log), log: log}
}

// DefaultDataDir is where the service keeps its data on this OS:
// /var/lib/coyote, /Library/Application Support/CoyoteDB, or
// %ProgramData%\CoyoteDB.
func (m *Manager) DefaultDataDir() string { return m.p.defaultDataDir() }

// Install installs (or updates) the service. See Options.
func (m *Manager) Install(ctx context.Context, o Options) (Report, error) {
	if err := m.normalize(&o); err != nil {
		return Report{}, err
	}
	return m.p.install(ctx, o)
}

// Uninstall removes the service. The data folder stays unless o.Purge.
func (m *Manager) Uninstall(ctx context.Context, o UninstallOptions) (Report, error) {
	return m.p.uninstall(ctx, o)
}

// Start starts the installed service.
func (m *Manager) Start(ctx context.Context) error { return m.p.start(ctx) }

// Stop stops the service and waits for it (and its Postgres) to exit.
func (m *Manager) Stop(ctx context.Context) error { return m.p.stop(ctx) }

// Status reports the service's state.
func (m *Manager) Status(ctx context.Context) (Status, error) { return m.p.status(ctx) }

func (m *Manager) normalize(o *Options) error {
	if o.Executable == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("find this program's path: %w", err)
		}
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		o.Executable = exe
	}
	if o.DataDir == "" {
		o.DataDir = m.p.defaultDataDir()
	}
	if len(o.Args) == 0 {
		o.Args = DefaultArgs(o.DataDir)
	}
	if len(o.FirewallPorts) == 0 {
		o.FirewallPorts = DefaultFirewallPorts
	}
	return validate(*o)
}

func validate(o Options) error {
	for _, s := range append([]string{o.Executable, o.DataDir}, o.Args...) {
		if strings.ContainsAny(s, "\x00\r\n") {
			return fmt.Errorf("invalid character in %q", s)
		}
	}
	if !isAbs(o.Executable) {
		return fmt.Errorf("program path %q must be absolute", o.Executable)
	}
	if !isAbs(o.DataDir) {
		return fmt.Errorf("data folder %q must be absolute", o.DataDir)
	}
	for _, p := range o.FirewallPorts {
		if p <= 0 || p > 65535 {
			return fmt.Errorf("bad firewall port %d", p)
		}
	}
	return nil
}

// isAbs accepts both OS path styles so rendering can be tested anywhere.
func isAbs(p string) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}
	return len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// safeToPurge refuses to delete obviously wrong folders.
func safeToPurge(dir string) error {
	// Slash-style paths are cleaned as such: filepath.Clean on Windows would
	// turn "/var/lib/coyote" into "\var\lib\coyote", which isAbs rejects
	// (the Linux and macOS rules are tested on every OS).
	clean := filepath.Clean(dir)
	if strings.HasPrefix(dir, "/") {
		clean = path.Clean(dir)
	}
	if !isAbs(clean) {
		return fmt.Errorf("refusing to delete %q: not an absolute path", dir)
	}
	parts := strings.FieldsFunc(clean, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) < 2 || (len(parts) == 2 && strings.HasSuffix(parts[0], ":")) {
		return fmt.Errorf("refusing to delete %q: too close to the root of the disk", dir)
	}
	return nil
}

// homeLike reports whether p is inside a user's home folder, which the
// service user cannot (or, under systemd's ProtectHome, may not) read.
func homeLike(p string) bool {
	for _, pre := range []string{"/home/", "/root/", "/Users/", "/run/user/"} {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}
