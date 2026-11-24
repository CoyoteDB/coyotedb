//go:build unix

package pg

// Safety rule 5 on Unix: Postgres never outlives its agent.
//
// Process tree while running:
//
//	agent (coyote up)                       session S, pgid A
//	├── postgres -D pgdata (postmaster)     session S, own pgid (Setpgid)
//	│     └── backends …
//	└── coyote __watchdog --pgdata …        NEW session (Setsid)
//
// The agent creates a pipe and passes the read end to the watchdog as fd 3.
// Nobody else holds the write end (Go opens every fd close-on-exec, and the
// postmaster gets no extra files). When the agent exits for any reason,
// including kill -9, the kernel closes the write end and the watchdog's
// read returns EOF. The watchdog then stops the postmaster: fast (SIGINT),
// then immediate (SIGQUIT), then SIGKILL, each with a deadline.
//
// Why the watchdog survives the agent: it is in its own session and process
// group, ignores SIGINT/SIGHUP/SIGTERM, and is not the agent's thread or
// process group, so neither a terminal Ctrl-C/hangup nor kill -9 of the
// agent touches it. It is reparented to launchd/init and reaped there.
//
// The watchdog also inherits the agent's node lock (fd 5). flock locks
// belong to the open file description, so the lock stays held until the
// watchdog exits too. A new agent on the same data dir therefore waits until
// the old Postgres is fully stopped; and the watchdog may safely use the pid
// in postmaster.pid as a fallback (no one else can have started a Postgres
// on this data dir while the lock is held).
//
// If the watchdog dies while the agent lives, Guard.Lost fires and the agent
// stops Postgres itself and exits: we never run unguarded.
//
// On Linux the postmaster additionally gets Pdeathsig=SIGQUIT (see
// proc_linux.go), so the kernel starts an immediate shutdown at once and the
// watchdog is the escalation path.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// WatchdogCommand is the hidden subcommand the agent re-executes itself with.
const WatchdogCommand = "__watchdog"

const (
	wdCtlFD  = 3 // read end of the agent's pipe
	wdAckFD  = 4 // write end of the ack pipe
	wdLockFD = 5 // the node lock, held until the watchdog exits
)

// Guard keeps Postgres from outliving the agent.
type Guard struct {
	ctl     *os.File
	cmd     *exec.Cmd
	exited  chan struct{}
	closing atomic.Bool
	log     *slog.Logger
}

// NewGuard starts the watchdog. lock must be the held node lock.
func NewGuard(n Node, lock *FileLock, log *slog.Logger) (*Guard, error) {
	if lock == nil || lock.File() == nil {
		return nil, errors.New("guard needs the node lock")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find own executable: %w", err)
	}
	ctlR, ctlW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		ctlR.Close()
		ctlW.Close()
		return nil, err
	}
	defer ackR.Close()
	logf, err := os.OpenFile(filepath.Join(n.LogDir(), "watchdog.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		ctlR.Close()
		ctlW.Close()
		ackW.Close()
		return nil, err
	}

	cmd := exec.Command(exe, WatchdogCommand, "--pgdata", n.PGData())
	cmd.ExtraFiles = []*os.File{ctlR, ackW, lock.File()} // fds 3, 4, 5
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	// The child has its own copies now.
	ctlR.Close()
	ackW.Close()
	logf.Close()
	if err != nil {
		ctlW.Close()
		return nil, fmt.Errorf("start watchdog: %w", err)
	}

	g := &Guard{ctl: ctlW, cmd: cmd, exited: make(chan struct{}), log: log}
	go func() {
		err := cmd.Wait()
		if !g.closing.Load() {
			log.Error("watchdog exited unexpectedly", "err", err)
		}
		close(g.exited)
	}()

	// Wait for the watchdog to confirm it is set up.
	_ = ackR.SetReadDeadline(time.Now().Add(15 * time.Second))
	buf := make([]byte, 3)
	if _, err := io.ReadFull(ackR, buf); err != nil || string(buf) != "ok\n" {
		g.closing.Store(true)
		ctlW.Close()
		_ = cmd.Process.Kill()
		<-g.exited
		return nil, fmt.Errorf("watchdog did not start (see %s): %v", filepath.Join(n.LogDir(), "watchdog.log"), err)
	}
	log.Info("watchdog running", "pid", cmd.Process.Pid)
	return g, nil
}

// Lost is closed if the watchdog exits while the agent still runs.
func (g *Guard) Lost() <-chan struct{} { return g.exited }

func (g *Guard) prepare(cmd *exec.Cmd) { cmd.SysProcAttr = postmasterSysProcAttr() }

// disarm cancels an arm whose start did not happen.
func (g *Guard) disarm() {
	if g.closing.Load() {
		return
	}
	_, _ = io.WriteString(g.ctl, "disarm\n")
}

func (g *Guard) started(pid int) error {
	_, err := fmt.Fprintf(g.ctl, "pid %d\n", pid)
	return err
}

// trackTool tells the watchdog about a running tool (pg_basebackup,
// pg_rewind, single-user postgres). Each line is one small write, so lines
// from different goroutines never interleave.
func (g *Guard) trackTool(pid int) error {
	_, err := fmt.Fprintf(g.ctl, "tool %d\n", pid)
	return err
}

// untrackTool tells the watchdog a tool has exited.
func (g *Guard) untrackTool(pid int) {
	if g.closing.Load() {
		return
	}
	_, _ = fmt.Fprintf(g.ctl, "untool %d\n", pid)
}

// Close tells the watchdog we are shutting down in order (Postgres already
// stopped) and waits for it to exit, which releases its copy of the lock.
func (g *Guard) Close() error {
	if g.closing.Swap(true) {
		return nil
	}
	_, _ = io.WriteString(g.ctl, "bye\n")
	g.ctl.Close()
	if waitClosed(g.exited, 20*time.Second) {
		return nil
	}
	return errors.New("watchdog did not exit")
}

// leaseStopTimeouts: when the lease runs out there is no time for a fast
// stop; go straight to immediate, then kill.
var leaseStopTimeouts = StopTimeouts{Fast: 0, Immediate: 300 * time.Millisecond, Kill: 5 * time.Second}

// RunWatchdog is the body of `coyote __watchdog`. It returns the exit code.
func RunWatchdog(args []string) int {
	fset := flag.NewFlagSet(WatchdogCommand, flag.ContinueOnError)
	pgdata := fset.String("pgdata", "", "Postgres data dir")
	if err := fset.Parse(args); err != nil || *pgdata == "" {
		fmt.Fprintln(os.Stderr, "watchdog: --pgdata is required")
		return 2
	}
	// Stay alive through terminal signals; we exit on our own once the
	// agent is gone and Postgres has stopped.
	signal.Ignore(syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGPIPE)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "watchdog", "watchdog_pid", os.Getpid())
	ctl := os.NewFile(wdCtlFD, "ctl")
	ack := os.NewFile(wdAckFD, "ack")
	lock := os.NewFile(wdLockFD, "lock")
	if ctl == nil || ack == nil || lock == nil {
		log.Error("watchdog started without its pipes")
		return 2
	}
	defer lock.Close() // released only when we are done

	if _, err := io.WriteString(ack, "ok\n"); err != nil {
		log.Error("cannot ack agent", "err", err)
		return 2
	}
	ack.Close()
	log.Info("watching", "pgdata", *pgdata, "agent", os.Getppid())

	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(ctl)
		for sc.Scan() {
			lines <- strings.TrimSpace(sc.Text())
		}
	}()
	// The primary lease (safety rule 4, second line of defence): while the
	// agent says Postgres is a primary it must refresh the lease; if it
	// does not (agent frozen or stuck), Postgres is stopped here.
	res := watchLoop(lines, func(pid int) {
		if pid == 0 { // armed start, pid never sent: the pid file names it
			if pf, err := ReadPidFile(*pgdata); err == nil && pf != nil {
				pid = pf.PID
			}
		}
		if pid <= 1 || !processAlive(pid) {
			return
		}
		if ok, err := isPostgresProcess(pid); err == nil && ok {
			if err := stopPID(context.Background(), Install{}, *pgdata, pid, leaseStopTimeouts, log); err != nil {
				log.Error("could not stop postgres", "postgres_pid", pid, "err", err)
			}
		}
	}, log)
	told, bye, tools := res.told, res.bye, res.tools
	// EOF (or read error): the agent has exited or closed us.
	if bye {
		log.Info("agent closed the guard in order")
	} else {
		log.Warn("agent is gone; making sure its postgres stops")
	}

	for pid := range tools {
		log.Warn("killing tool process group left by the agent", "tool_pid", pid)
		if err := killToolGroup(pid); err != nil {
			log.Error("could not kill tool", "tool_pid", pid, "err", err)
		}
	}

	cands := []int{}
	if told > 0 {
		cands = append(cands, told)
	}
	if pf, err := ReadPidFile(*pgdata); err == nil && pf != nil && pf.PID != told {
		cands = append(cands, pf.PID)
	}
	rc := 0
	for _, pid := range cands {
		if !processAlive(pid) {
			continue
		}
		if ok, err := isPostgresProcess(pid); err != nil || !ok {
			continue
		}
		if err := stopPID(context.Background(), Install{}, *pgdata, pid, guardStopTimeouts, log); err != nil {
			log.Error("could not stop postgres", "postgres_pid", pid, "err", err)
			rc = 1
		}
	}
	log.Info("watchdog done")
	return rc
}
