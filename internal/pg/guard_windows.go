//go:build windows

package pg

// Safety rule 5 on Windows: the agent puts itself in a Job Object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE before starting Postgres. Child
// processes (the postmaster and everything it spawns, and the watchdog)
// inherit the job. The only handle to the job is held by the agent; when
// the agent exits for any reason the OS closes that handle and terminates
// every process in the job.
//
// We never close the handle ourselves: that would kill the agent too.
//
// Safety rule 4's second line of defence (the primary lease) needs a
// process that keeps running when the agent is frozen (suspended, stuck):
// the job only acts when the agent dies. So the agent also starts
// `coyote __watchdog`, a child in the same job, and talks to it over its
// stdin with the same line protocol as on Unix (watchdog.go). When a lease
// runs out without a renewal, the watchdog terminates the postmaster and
// then every postgres.exe whose parent is that postmaster (a backend left
// running could still commit and acknowledge on its own).
//
// If the watchdog exits while the agent lives, Guard.Lost fires and the
// agent stops Postgres and exits, as on Unix: a primary never runs without
// its lease enforced.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WatchdogCommand is the hidden subcommand the agent re-executes itself with.
const WatchdogCommand = "__watchdog"

// Guard keeps Postgres from outliving the agent and enforces the lease.
type Guard struct {
	job     windows.Handle
	pgToken windows.Token  // restricted token for Postgres programs; 0: the agent's own (token_windows.go)
	ctl     io.WriteCloser // the watchdog's stdin
	cmd     *exec.Cmd
	exited  chan struct{}
	closing atomic.Bool
	log     *slog.Logger
}

// NewGuard creates the kill-on-close Job Object, assigns this process to
// it and starts the watchdog. lock is unused on Windows (the job, not a
// lock hand-over, makes a new agent wait).
func NewGuard(n Node, _ *FileLock, log *slog.Logger) (*Guard, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("configure job object: %w", err)
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("join job object (is the agent inside a job that forbids nesting?): %w", err)
	}
	log.Info("job object guard installed")
	pgToken, err := postgresToken()
	if err != nil {
		// The job handle stays open (closing it would kill us).
		return nil, err
	}
	if pgToken != 0 {
		log.Info("running with administrative rights; Postgres programs get a restricted token without them")
		if err := grantUserDataDir(n.StateDir, pgToken); err != nil {
			pgToken.Close()
			return nil, err
		}
	}
	g := &Guard{job: job, pgToken: pgToken, exited: make(chan struct{}), log: log}
	if err := g.startWatchdog(n); err != nil {
		// The job handle stays open (closing it would kill us); the agent
		// refuses to run without the watchdog.
		return nil, err
	}
	return g, nil
}

func (g *Guard) startWatchdog(n Node) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find own executable: %w", err)
	}
	logf, err := os.OpenFile(filepath.Join(n.LogDir(), "watchdog.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, WatchdogCommand, "--pgdata", n.PGData())
	cmd.Stderr = logf
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.SysProcAttr = postmasterSysProcAttr() // own process group: no console Ctrl-C
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start watchdog: %w", err)
	}
	g.ctl, g.cmd = stdin, cmd
	go func() {
		err := cmd.Wait()
		if !g.closing.Load() {
			g.log.Error("watchdog exited unexpectedly", "err", err)
		}
		close(g.exited)
	}()
	ack := make(chan error, 1)
	go func() {
		buf := make([]byte, 3)
		_, err := io.ReadFull(stdout, buf)
		if err == nil && string(buf) != "ok\n" {
			err = errors.New("unexpected greeting")
		}
		ack <- err
	}()
	select {
	case err = <-ack:
	case <-time.After(15 * time.Second):
		err = errors.New("timed out")
	}
	if err != nil {
		g.closing.Store(true)
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		<-g.exited
		return fmt.Errorf("watchdog did not start (see %s): %v", filepath.Join(n.LogDir(), "watchdog.log"), err)
	}
	g.log.Info("watchdog running", "pid", cmd.Process.Pid)
	return nil
}

// Lost is closed if the watchdog exits while the agent still runs.
func (g *Guard) Lost() <-chan struct{} { return g.exited }

func (g *Guard) prepare(cmd *exec.Cmd) { cmd.SysProcAttr = g.postgresSysProcAttr() }

// postgresSysProcAttr is postmasterSysProcAttr plus, when the agent has
// administrative rights, the restricted token postgres.exe requires.
func (g *Guard) postgresSysProcAttr() *syscall.SysProcAttr {
	a := postmasterSysProcAttr()
	a.Token = syscall.Token(g.pgToken)
	return a
}

func (g *Guard) send(format string, args ...any) error {
	if g.closing.Load() {
		return nil
	}
	_, err := fmt.Fprintf(g.ctl, format, args...)
	return err
}

func (g *Guard) started(pid int) error { return g.send("pid %d\n", pid) }

// disarm cancels an arm whose start did not happen.
func (g *Guard) disarm() { _ = g.send("disarm\n") }

// Close tells the watchdog we are shutting down in order (Postgres already
// stopped) and waits for it to exit. The job handle stays open: closing it
// would terminate this process.
func (g *Guard) Close() error {
	if g.closing.Swap(true) {
		return nil
	}
	_, _ = io.WriteString(g.ctl, "bye\n")
	_ = g.ctl.Close()
	if waitClosed(g.exited, 20*time.Second) {
		return nil
	}
	return errors.New("watchdog did not exit")
}

// RunWatchdog is the body of `coyote __watchdog` on Windows: it reads the
// control protocol on stdin and enforces the primary lease. When the agent
// dies, the job terminates this process together with Postgres; when the
// pipe ends without "bye" while we still run, Postgres is stopped here too.
func RunWatchdog(args []string) int {
	pgdata := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--pgdata" {
			pgdata = args[i+1]
		}
	}
	if pgdata == "" {
		fmt.Fprintln(os.Stderr, "watchdog: --pgdata is required")
		return 2
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil)).With("component", "watchdog", "watchdog_pid", os.Getpid())
	if _, err := io.WriteString(os.Stdout, "ok\n"); err != nil {
		log.Error("cannot ack agent", "err", err)
		return 2
	}
	log.Info("watching", "pgdata", pgdata, "agent", os.Getppid())
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			lines <- strings.TrimSpace(sc.Text())
		}
	}()
	res := watchLoop(lines, func(pid int) {
		if pid == 0 { // armed start, pid never sent: the pid file names it
			if pf, err := ReadPidFile(pgdata); err == nil && pf != nil {
				pid = pf.PID
			}
		}
		if pid > 0 {
			killPostgresTree(pid, log)
		}
	}, log)
	if res.bye {
		log.Info("agent closed the guard in order")
		return 0
	}
	log.Warn("agent is gone; making sure its postgres stops")
	if res.told > 0 {
		killPostgresTree(res.told, log)
	}
	if pf, err := ReadPidFile(pgdata); err == nil && pf != nil && pf.PID != res.told {
		killPostgresTree(pf.PID, log)
	}
	log.Info("watchdog done")
	return 0
}

// killPostgresTree terminates postmaster pid (if it still is a postgres)
// and then every postgres.exe whose parent it was. The postmaster goes
// first so it cannot start new children while we look for them; Windows
// keeps the parent pid in each child's record after the parent exits.
func killPostgresTree(pid int, log *slog.Logger) {
	if !processAlive(pid) {
		return
	}
	if ok, err := isPostgresProcess(pid); err != nil || !ok {
		return
	}
	if err := killPID(pid); err != nil {
		log.Error("could not terminate postgres", "postgres_pid", pid, "err", err)
	}
	children, err := childProcesses(uint32(pid))
	if err != nil {
		log.Error("could not list postgres children", "postgres_pid", pid, "err", err)
		return
	}
	for _, c := range children {
		if ok, err := isPostgresProcess(int(c)); err == nil && ok {
			if err := killPID(int(c)); err != nil {
				log.Warn("could not terminate a postgres child", "pid", c, "err", err)
			}
		}
	}
	log.Info("postgres terminated", "postgres_pid", pid, "children", len(children))
}

// childProcesses lists the pids whose parent is ppid.
func childProcesses(ppid uint32) ([]uint32, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var out []uint32
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ParentProcessID == ppid && e.ProcessID != ppid {
			out = append(out, e.ProcessID)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return out, err
	}
	return out, nil
}
