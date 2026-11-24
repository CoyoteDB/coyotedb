package pg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"coyotedb.com/coyote/internal/mono"
)

// StopTimeouts bounds each step of the fast -> immediate -> kill escalation.
type StopTimeouts struct {
	Fast      time.Duration
	Immediate time.Duration
	Kill      time.Duration
}

// DefaultStopTimeouts is used for an orderly stop by the agent.
var DefaultStopTimeouts = StopTimeouts{Fast: 30 * time.Second, Immediate: 10 * time.Second, Kill: 10 * time.Second}

// guardStopTimeouts is used when Postgres must go away quickly because its
// agent is gone (safety rule 5).
var guardStopTimeouts = StopTimeouts{Fast: 4 * time.Second, Immediate: 3 * time.Second, Kill: 5 * time.Second}

// stopMode is a Postgres shutdown mode.
type stopMode int

const (
	modeFast stopMode = iota
	modeImmediate
)

func (m stopMode) String() string {
	if m == modeFast {
		return "fast"
	}
	return "immediate"
}

// StartOptions controls Node.Start.
type StartOptions struct {
	Install      Install
	Port         int
	Name         string // node name: application_name towards the upstream
	Guard        *Guard
	Logger       *slog.Logger
	ReadyTimeout time.Duration // default 2 minutes
	// PrimaryDeadline, if set, makes this start a primary (safety rule 4).
	// It is called right before the process is spawned (after every file
	// is written) and returns the lease's watchdog deadline (an
	// internal/mono reading), or false if the lease is not held any more:
	// the start is then refused with ErrLeaseExpired. The watchdog is armed
	// with the deadline before the process exists, so it covers Postgres
	// from its first instant (crash recovery included), whatever the agent
	// does meanwhile. A deadline already past is refused too.
	PrimaryDeadline func() (time.Duration, bool)
	// OnSpawn, if set, is called with the server as soon as the process
	// exists, before it is ready: the caller can then renew or fence a
	// Postgres that is still starting.
	OnSpawn func(*Server)
	// TestReadyDelay (tests only): Start returns this much later than
	// Postgres becomes ready, like an agent stalled in a slow start. Postgres
	// serves meanwhile, so only the lease (loop and watchdog) protects it.
	TestReadyDelay time.Duration
	// TestBeforeSpawn (tests only) runs after the config is written and
	// right before the watchdog is armed and Postgres is spawned: a slow
	// disk or a stalled agent in the window before the process exists.
	TestBeforeSpawn func()
}

// ErrExitedDuringStartup: the postmaster exited before it was ready (a
// crash, a fence, a program that cannot run).
var ErrExitedDuringStartup = errors.New("postgres exited during startup")

// ErrLeaseExpired: a primary start was refused because the main-node
// lease ran out before Postgres could be spawned (safety rule 4).
var ErrLeaseExpired = errors.New("the main-node lease ran out before PostgreSQL could start")

func exitedDuringStartup(waitErr error) error {
	if waitErr == nil {
		return ErrExitedDuringStartup
	}
	return fmt.Errorf("%w: %w", ErrExitedDuringStartup, waitErr)
}

// Server is a running postmaster that this agent started and owns.
type Server struct {
	node    Node
	inst    Install
	port    int
	log     *slog.Logger
	cmd     *exec.Cmd
	stderr  *os.File
	done    chan struct{}
	waitErr error

	stopOnce sync.Once
	stopErr  error
}

// PID is the postmaster's process id.
func (s *Server) PID() int { return s.cmd.Process.Pid }

// Port is the TCP port Postgres listens on (127.0.0.1 only).
func (s *Server) Port() int { return s.port }

// Done is closed when the postmaster exits for any reason.
func (s *Server) Done() <-chan struct{} { return s.done }

// ExitErr is the postmaster's exit status; valid after Done is closed.
func (s *Server) ExitErr() error { <-s.done; return s.waitErr }

// Start writes the agent-owned config and starts Postgres under the guard,
// then waits until it accepts connections. The caller must already hold the
// node lock and have called StopLeftover.
func (n Node) Start(ctx context.Context, o StartOptions) (*Server, error) {
	if o.Guard == nil {
		return nil, errors.New("refusing to start Postgres without a process guard (safety rule 5)")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 2 * time.Minute
	}
	if err := n.Prepare(); err != nil {
		return nil, err
	}
	if l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(o.Port))); err != nil {
		return nil, fmt.Errorf("port %d on 127.0.0.1 is already in use; pick another one: %w", o.Port, err)
	} else {
		l.Close()
	}
	if err := n.writeConfigLocked(ctx, o.Port, o.Name); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}

	// Output before the logging collector takes over (e.g. "port in use")
	// lands here.
	stderrPath := filepath.Join(n.LogDir(), "postgres-stderr.log")
	ef, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	startOff, _ := ef.Seek(0, io.SeekEnd)

	// Safety rule 11 settings are also passed on the command line: they then
	// beat postgresql.auto.conf, so ALTER SYSTEM cannot open Postgres to the
	// network. "-D <pgdata>" stays last (tests and tools match on it).
	cmd := exec.Command(o.Install.Bin("postgres"),
		"-c", "listen_addresses=127.0.0.1",
		"-c", "port="+strconv.Itoa(o.Port),
		"-D", n.PGData())
	cmd.Dir = n.StateDir
	cmd.Env = cleanEnv()
	cmd.Stdout = ef
	cmd.Stderr = ef
	o.Guard.prepare(cmd)

	s := &Server{node: n, inst: o.Install, port: o.Port, log: o.Logger, cmd: cmd, stderr: ef, done: make(chan struct{})}
	if o.TestBeforeSpawn != nil {
		o.TestBeforeSpawn()
	}
	primary := o.PrimaryDeadline != nil
	if primary {
		dl, ok := o.PrimaryDeadline()
		if !ok || dl <= mono.Now() {
			ef.Close()
			return nil, ErrLeaseExpired
		}
		if err := o.Guard.armUntil(dl); err != nil {
			ef.Close()
			return nil, fmt.Errorf("arm the watchdog: %w", err)
		}
	}
	waitCh, err := startProcess(cmd)
	if err != nil {
		if primary {
			o.Guard.disarm()
		}
		ef.Close()
		return nil, fmt.Errorf("start postgres: %w", err)
	}
	go func() {
		s.waitErr = <-waitCh
		ef.Close()
		close(s.done)
	}()
	o.Logger.Info("postgres started", "pid", cmd.Process.Pid, "port", o.Port)
	if err := o.Guard.started(cmd.Process.Pid); err != nil {
		_ = s.Stop(guardStopTimeouts)
		return nil, fmt.Errorf("register postgres with guard: %w", err)
	}
	if o.OnSpawn != nil {
		o.OnSpawn(s)
	}

	if err := s.waitReady(ctx, o.ReadyTimeout); err != nil {
		_ = s.Stop(guardStopTimeouts)
		msg := tail(readFrom(stderrPath, startOff), 10)
		if srvLog := newestServerLog(n.LogDir()); srvLog != "" {
			msg += "\n" + tail(readFrom(srvLog, 0), 10)
		}
		return nil, fmt.Errorf("%w\n%s", err, msg)
	}
	if o.TestReadyDelay > 0 {
		select {
		case <-time.After(o.TestReadyDelay):
		case <-s.done:
			return nil, exitedDuringStartup(s.waitErr)
		case <-ctx.Done():
			_ = s.Stop(guardStopTimeouts)
			return nil, ctx.Err()
		}
	}
	return s, nil
}

// waitReady polls postmaster.pid for status "ready", or "standby" for a hot
// standby that accepts read-only connections (what pg_ctl -w does), and
// then confirms the TCP port accepts connections.
func (s *Server) waitReady(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port))
	for {
		select {
		case <-s.done:
			return exitedDuringStartup(s.waitErr)
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("postgres did not become ready within %s", timeout)
			}
			return ctx.Err()
		case <-tick.C:
		}
		pf, err := ReadPidFile(s.node.PGData())
		if err != nil || pf == nil || pf.PID != s.PID() || (pf.Status != "ready" && pf.Status != "standby") {
			continue
		}
		d := net.Dialer{Timeout: time.Second}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			continue
		}
		c.Close()
		return nil
	}
}

// Stop shuts Postgres down: fast, then immediate, then kill, each step
// bounded by its timeout. Safe to call more than once.
func (s *Server) Stop(t StopTimeouts) error {
	s.stopOnce.Do(func() { s.stopErr = s.stop(t) })
	return s.stopErr
}

func (s *Server) stop(t StopTimeouts) error {
	for _, step := range []struct {
		mode stopMode
		wait time.Duration
	}{{modeFast, t.Fast}, {modeImmediate, t.Immediate}} {
		select {
		case <-s.done:
			return nil
		default:
		}
		s.log.Info("stopping postgres", "mode", step.mode.String(), "pid", s.PID())
		if err := requestStop(s.inst, s.node.PGData(), s.PID(), step.mode); err != nil {
			s.log.Warn("stop request failed", "mode", step.mode.String(), "err", err)
		}
		if waitClosed(s.done, step.wait) {
			s.log.Info("postgres stopped", "mode", step.mode.String())
			return nil
		}
	}
	s.log.Warn("postgres did not stop; killing it", "pid", s.PID())
	return s.killAll(t.Kill)
}

func waitClosed(ch <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}

// StopLeftover implements "adopt or stop" from safety rule 5: if a Postgres
// that we did not start in this process still owns the data dir, stop it
// cleanly (fast, immediate, kill) so we can start it fresh under our guard.
// A pid file whose process is gone is left for Postgres to clean up. A pid
// file whose pid now belongs to an unrelated process is removed. The caller
// must hold the node lock.
func (n Node) StopLeftover(ctx context.Context, inst Install, log *slog.Logger) (bool, error) {
	pf, err := ReadPidFile(n.PGData())
	if err != nil {
		return false, fmt.Errorf("read postmaster.pid: %w", err)
	}
	if pf == nil {
		return false, nil
	}
	if !processAlive(pf.PID) {
		log.Info("stale postmaster.pid: process is gone", "pid", pf.PID)
		return false, nil
	}
	isPG, err := isPostgresProcess(pf.PID)
	if err != nil {
		return false, fmt.Errorf("postmaster.pid names pid %d, which is running but could not be inspected: %w", pf.PID, err)
	}
	if !isPG {
		log.Warn("postmaster.pid names a live process that is not postgres (pid reused); removing it", "pid", pf.PID)
		if err := os.Remove(PostmasterPidPath(n.PGData())); err != nil {
			return false, fmt.Errorf("remove stale postmaster.pid: %w", err)
		}
		return false, nil
	}
	log.Warn("a Postgres from an earlier run is still running on this data dir; stopping it", "pid", pf.PID)
	if err := stopPID(ctx, inst, n.PGData(), pf.PID, DefaultStopTimeouts, log); err != nil {
		return true, err
	}
	return true, nil
}

// stopPID stops a postmaster that is not our child, escalating fast ->
// immediate -> kill, and polls until the process is gone.
func stopPID(ctx context.Context, inst Install, pgdata string, pid int, t StopTimeouts, log *slog.Logger) error {
	for _, step := range []struct {
		mode stopMode
		wait time.Duration
	}{{modeFast, t.Fast}, {modeImmediate, t.Immediate}} {
		if !processAlive(pid) {
			return nil
		}
		if step.wait <= 0 {
			continue // no time for this step (a lease ran out): escalate
		}
		log.Info("stopping postgres", "mode", step.mode.String(), "postgres_pid", pid)
		if err := requestStop(inst, pgdata, pid, step.mode); err != nil {
			log.Warn("stop request failed", "mode", step.mode.String(), "err", err)
		}
		if waitGone(ctx, pid, step.wait) {
			log.Info("postgres stopped", "mode", step.mode.String(), "postgres_pid", pid)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	log.Warn("postgres did not stop; killing it and every process it started", "postgres_pid", pid)
	stop, kill := rawStopKill(pid)
	kids := killServerTree(pid, stop, kill)
	if waitGone(ctx, pid, t.Kill) && waitProcsGone(kids, t.Kill) {
		return nil
	}
	return fmt.Errorf("postgres (pid %d) is still running after kill", pid)
}

// waitGone polls until pid is no longer alive or d elapses.
func waitGone(ctx context.Context, pid int, d time.Duration) bool {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if !processAlive(pid) {
			return true
		}
		select {
		case <-ctx.Done():
			return !processAlive(pid)
		case <-deadline.C:
			return !processAlive(pid)
		case <-tick.C:
		}
	}
}

// Status describes what is running on a node's data dir.
type Status struct {
	Running bool
	PID     int
	Port    int
	State   string // from postmaster.pid: starting, ready, stopping, standby
}

// Status reports whether a Postgres is running on this node's data dir.
func (n Node) Status() (Status, error) {
	pf, err := ReadPidFile(n.PGData())
	if err != nil || pf == nil {
		return Status{}, err
	}
	if !processAlive(pf.PID) {
		return Status{}, nil
	}
	if ok, err := isPostgresProcess(pf.PID); err != nil || !ok {
		return Status{}, err
	}
	return Status{Running: true, PID: pf.PID, Port: pf.Port, State: pf.Status}, nil
}

// newestServerLog returns the most recently modified postgresql-*.log.
func newestServerLog(dir string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "postgresql-*.log"))
	var best string
	var bestT time.Time
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().After(bestT) {
			best, bestT = m, fi.ModTime()
		}
	}
	return best
}

// readFrom returns the content of path from byte offset off (at most the
// last 64 KiB).
func readFrom(path string, off int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size()-off > 64<<10 {
		off = fi.Size() - 64<<10
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(f, 64<<10))
	return string(b)
}
