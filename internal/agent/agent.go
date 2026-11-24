// Package agent is the long-running node program behind `coyote up`,
// `coyote join` and `coyote witness`. One Agent owns the node's lifecycle:
//
//	node lock → process guard → cluster node (Bootstrap / Join / Start)
//	→ tunnel forwarder → agent RPC + local control channel
//	→ role reconcile loop (starts/stops/promotes/rewinds Postgres as the
//	  cluster state dictates)
//
// and tears it down in reverse order. Safety rule 5 holds throughout: no
// Postgres (or Postgres tool) is ever started without the guard, and the
// guard outlives every Postgres start.
package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/cluster/transport"
	"coyotedb.com/coyote/internal/control"
	"coyotedb.com/coyote/internal/controller"
	"coyotedb.com/coyote/internal/invite"
	"coyotedb.com/coyote/internal/netdetect"
	"coyotedb.com/coyote/internal/pg"
	"coyotedb.com/coyote/internal/proxy"
	"coyotedb.com/coyote/internal/tunnel"
)

// Mode is how the agent was started.
type Mode int

const (
	ModeUp      Mode = iota // create a cluster on a fresh data dir, otherwise resume
	ModeJoin                // join as a data node (or resume a join)
	ModeWitness             // join as a witness (or resume)
)

// DefaultListen is the cluster port's default bind address.
const DefaultListen = "0.0.0.0:7481"

// Config is what the CLI passes in.
type Config struct {
	DataDir     string
	Mode        Mode
	Invite      string // join / witness
	Name        string // "" = stored, else from the host name
	Listen      string // "" = stored, else DefaultListen
	Advertise   []string
	PGPort      int // 0 = stored, else pg.DefaultPort
	ControlPort int // 0 = stored, else control.DefaultPort
	// ClientPort is the client (proxy) port on data nodes; 0 = stored, else
	// DefaultClientPort.
	ClientPort int
	Archive    string
	Cache      string
	Out        io.Writer
	Logger     *slog.Logger
	LogPath    string
	Version    string
	// Service: run by the background service (`service run`). On a data
	// folder not in a cluster, wait for a ServiceRequest instead of
	// creating a cluster.
	Service bool
	// Timing of the cluster layer; zero means cluster.DefaultTiming().
	Timing cluster.Timing
	// AllowMixedOS (join): accept joining a cluster whose data nodes are a
	// different kind of machine, if the checks pass (`--allow-mixed-os`,
	// D37).
	AllowMixedOS bool
	// Wizard (`coyote setup`, D50): on a data folder that is not in a
	// cluster, serve the dashboard's setup page and wait for "create" or
	// "join" from it instead of creating a cluster at once.
	Wizard bool
	// OpenLink, if set, opens the wizard's one-time sign-in link in a
	// browser and reports whether it did.
	OpenLink func(url string) bool
	// DashboardListen (`--dashboard-listen HOST:PORT`, D52): bind the local
	// control port on HOST instead of 127.0.0.1 and let dashboard browsers
	// that are not on loopback in (session cookie only; the CLI's bearer
	// token stays loopback-only).
	DashboardListen string
}

// ExitError is a stop the person should read; Code is the exit code.
// Cause, if set, is the error behind it (for codes in the setup page).
type ExitError struct {
	Code  int
	Msg   string
	Cause error
}

func (e *ExitError) Error() string { return e.Msg }
func (e *ExitError) Unwrap() error { return e.Cause }

// fatalError ends the reconcile loop (and the agent).
type fatalError struct{ err error }

func (f *fatalError) Error() string { return f.err.Error() }
func (f *fatalError) Unwrap() error { return f.err }

func fatal(err error) error { return &fatalError{err} }

// errGuardLost is the M1 message, kept word for word.
var errGuardLost = errors.New("the process guard stopped unexpectedly, so the database was stopped for safety")

type lifecycle int

const (
	lcBootstrap lifecycle = iota
	lcJoin
	lcStart
)

// Agent is one running node.
type Agent struct {
	cfg  Config
	node pg.Node
	log  *slog.Logger
	out  *printer
	st   *stateStore

	role      cluster.Role
	lifecycle lifecycle
	name      string
	pgPort    int
	listen    string
	advertise []string
	// advertiseExplicit: --advertise was given on this command line.
	advertiseExplicit bool
	// addrs: the address check (addrwatch.go, D66).
	addrs addrWatch
	// clusterOpened: when this agent's cluster node started (status says
	// how long it has waited for the cluster).
	clusterOpened time.Time
	ctlPort       int
	clientPort    int
	proxy         atomic.Pointer[proxy.Proxy]
	peers         peerTable
	clusterLn     net.Listener // bound by reservePorts, owned by the cluster node once opened
	addrNotes     []string     // what plan found out about this machine's addresses

	lock  *pg.FileLock
	inst  pg.Install
	guard *pg.Guard
	cn    *cluster.Node
	fwd   *tunnel.Forwarder
	ctl   *control.Server

	// pgSem serialises everything that starts, stops or reconfigures
	// Postgres (the loop and the agent RPC handlers).
	pgSem chan struct{}

	pgMu   sync.Mutex
	srv    *pg.Server
	pgRole string // "primary" or "standby" while srv != nil
	// starting: a Postgres whose process exists but whose Start has not
	// returned yet (crash recovery can take long). The lease covers it
	// from its first instant: the lease loop renews and fences it like srv.
	starting *pg.Server
	// primaryStart: a primary start between taking its lease deadline
	// and spawning (lease.go, fenceTarget).
	primaryStart *primaryStart
	// cutHold measures this node's stream-cut hold (lease_cut.go).
	cutHold cutHoldClock
	// stepHold: stepped down for a planned switch (lease_stepdown.go).
	stepHold stepHoldClock
	// Test fault hooks (lease_testhooks.go); inert in normal use.
	stallSpawn atomic.Bool
	wallStep   *wallStep
	// slowStart (test fault slow-start-after-clear-fence): the next
	// primary start returns late. Guarded by pgSem (the loop).
	slowStart bool
	pgAddr    atomic.Pointer[string]

	phaseMu  sync.Mutex
	phase    string
	phaseMsg string
	shownMsg string
	progress int

	// switchMu is held by a planned switch and by every controller step.
	switchMu sync.Mutex
	leaveCh  chan leaveReq

	clock controller.Clock
	lease leaseState
	// health is the main-node health check (mainhealth.go, D58).
	health mainHealth
	ctrl   *controller.Controller

	kick   chan struct{}
	exitCh chan error

	ready         bool
	seenMember    bool
	lastProbe     time.Time
	following     string // primary id we last announced following
	lastDuties    time.Time
	lastDutyIdx   uint64
	lastErrMsg    string
	lastHint      string // the last plain-words start problem said (nextstep.go)
	permRepeats   int    // the same permanent start error seen this many times in a row (nextstep.go)
	promotedEpoch uint64 // the term this process last called Promote in (doPromote; a failed one is finished, not retried)
	// notPrimaryTries counts consecutive failed steps because the new main
	// node was still promoting (fast retries, see retryDelay).
	notPrimaryTries int
	// pgTLSSeen / pgTLSChecked: the host set the Postgres certificate was
	// last confirmed for, and when (reconcile goroutine only).
	pgTLSSeen    string
	pgTLSChecked time.Time
	// appSynced / appApplied: the application password last written to
	// the local secrets file / last applied to the role (reconcile
	// goroutine only).
	appSynced, appApplied string
	// Postgres crash restarts (backoff).
	crashes   int
	lastCrash time.Time
	// followChecked is the epoch at which this standby last confirmed it
	// can follow the primary without a rewind.
	followChecked uint64
	// diverge is the same-timeline divergence watch (divergewatch.go, D56).
	diverge divergeWatch
	// recloneSlotDropped: members whose slot this primary dropped for
	// "needs re-clone" (rule 8), so a new copy's slot is not dropped again.
	recloneSlotDropped map[string]bool
	// mixed is the mixed-system guard's state (mixedos.go).
	mixed mixedState
	// pages is the page health state (pagehealth.go, D55).
	pages pageHealth
	// recv is the copy's WAL receiver watch (recvwatch.go, D57).
	recv recvWatch
	// bk is the backup loop's state (backup.go, D63).
	bk backupState

	// early is the local control port when it is served before the node
	// is a member (the setup page, D50; setup.go); nil otherwise.
	early *earlyControl
	// setup is the first-run progress the setup page shows.
	setup *setupTracker
}

// Run runs the agent until ctx is cancelled or a fatal condition.
func Run(ctx context.Context, cfg Config) error {
	node, err := pg.NewNode(cfg.DataDir)
	if err != nil {
		return err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	a := newAgent(cfg, node)
	a.setup = newSetupTracker()
	return a.run(ctx)
}

// newAgent is a fresh Agent for cfg (one per attempt of the setup page).
func newAgent(cfg Config, node pg.Node) *Agent {
	a := &Agent{
		cfg:      cfg,
		node:     node,
		log:      cfg.Logger,
		out:      &printer{w: cfg.Out},
		pgSem:    make(chan struct{}, 1),
		kick:     make(chan struct{}, 1),
		exitCh:   make(chan error, 1),
		leaveCh:  make(chan leaveReq),
		progress: -1,
		clock:    controller.NewSystemClock(),

		recloneSlotDropped: map[string]bool{},
		mixed:              mixedState{kick: make(chan struct{}, 1)},
	}
	if w := newWallStep(filepath.Join(node.StateDir, "cluster"), a.say); w != nil {
		a.wallStep, a.clock = w, steppedClock{Clock: a.clock, w: w}
	}
	return a
}

func (a *Agent) say(format string, args ...any) { a.out.say(format, args...) }

func (a *Agent) clusterDir() string { return filepath.Join(a.node.StateDir, "cluster") }

func (a *Agent) run(ctx context.Context) error {
	if err := a.node.Prepare(); err != nil {
		return fmt.Errorf("could not create the data folder %s: %w", a.node.StateDir, err)
	}
	// 1. Node lock: one agent per data dir (the watchdog inherits it).
	lockCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	lock, err := pg.AcquireLock(lockCtx, a.node.LockPath(), func() {
		a.say("Another %s is using this data folder (or is still stopping its database). Waiting up to a minute…", brand.Command)
	})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("another %s is already running for %s", brand.Command, a.node.StateDir)
	}
	a.lock = lock
	defer lock.Unlock()

	if a.st, err = loadStateStore(a.node.StateDir); err != nil {
		return err
	}
	if why := a.st.get().Removed; why != "" {
		return &ExitError{Code: 1, Msg: removedMessage(why, a.node.StateDir)}
	}
	if a.cfg.Service || a.cfg.Wizard {
		// Not in a cluster yet: serve the setup page until it (or, for the
		// service, a request file) says what to do (setup.go).
		return a.runSetup(ctx)
	}
	return a.runMember(ctx)
}

// runMember is the node's life from planning to stop: bootstrap, join or
// start, then the reconcile loop. The node lock is held.
func (a *Agent) runMember(ctx context.Context) error {
	if err := a.plan(); err != nil {
		return err
	}
	for _, l := range a.addrNotes {
		a.say("%s", l)
	}
	a.setup.begin(a.lifecycle, a.role)
	a.log.Info("agent starting", "version", a.cfg.Version, "data_dir", a.node.StateDir, "role", a.role,
		"lifecycle", a.lifecycle, "listen", a.listen, "pg_port", a.pgPort, "control_port", a.ctlPort)

	// 2. Every local port, before anything else and long before the
	// cluster is contacted: a port that is taken must stop a join before it
	// uses the invite and makes this machine a member, and say which port
	// in plain words. (The Postgres port is checked once a leftover
	// Postgres of ours is stopped, below.)
	ports, err := a.reservePorts()
	if err != nil {
		return err
	}
	defer ports.close()

	// 3. Postgres program files and the process guard (data nodes only).
	var plat cluster.Platform
	if a.role == cluster.RoleData {
		if err := pg.CheckProgramPath(a.cacheDir()); err != nil {
			return &ExitError{Code: 2, Msg: "--pg-cache: " + err.Error() + "."}
		}
		a.setup.step(stepDownload, stepActive, "")
		inst, err := pg.Ensure(ctx, pg.FetchOptions{
			CacheDir: a.cacheDir(), Archive: a.cfg.Archive, Logger: a.log, Progress: a.downloadProgress(),
		})
		if err != nil {
			a.setup.step(stepDownload, stepFailed, "")
			if errors.Is(err, pg.ErrChecksumMismatch) {
				return fmt.Errorf("the PostgreSQL download did not match its expected fingerprint, so it was not used. This can mean a broken download or tampering: %w", err)
			}
			return fmt.Errorf("could not get PostgreSQL: %w", err)
		}
		a.setup.downloaded()
		a.inst = inst
		plat = cluster.Platform{OS: platformOS(), Arch: runtime.GOARCH, PGBuildSHA: inst.Build.SHA256}
		// Stopping a leftover never starts anything, so it is always safe.
		if a.node.Initialized() {
			stopped, err := a.node.StopLeftover(ctx, inst, a.log)
			if err != nil {
				return fmt.Errorf("a database from an earlier run is still running and could not be stopped: %w", err)
			}
			if stopped {
				a.say("Stopped a database left running by an earlier run; starting it fresh.")
			}
		}
		if err := a.checkPGPort(); err != nil {
			return err
		}
		g, err := pg.NewGuard(a.node, a.lock, a.log)
		if err != nil {
			return fmt.Errorf("could not set up the process guard: %w", err)
		}
		a.guard = g
		defer func() {
			if err := g.Close(); err != nil {
				a.log.Warn("guard close", "err", err)
			}
		}()
		// A joining data node is measured before it holds any data, so
		// the cluster can refuse a machine that cannot share its files
		// before anything is copied (rule 1, MIXED_OS.md item 4).
		if a.lifecycle == lcJoin {
			if plat, err = a.joinPlatform(ctx); err != nil {
				return err
			}
		}
	} else {
		plat = cluster.Platform{OS: platformOS(), Arch: runtime.GOARCH}
		if b, err := pg.CurrentBuild(); err == nil {
			plat.PGBuildSHA = b.SHA256
		}
	}

	// 4. Cluster node.
	a.clusterLn = ports.take(&ports.cluster)
	if err := a.openCluster(ctx, plat); err != nil {
		return err
	}
	a.clusterOpened = time.Now()
	a.setup.clusterOpen(a.lifecycle == lcJoin)
	defer func() {
		if err := a.cn.Shutdown(); err != nil {
			a.log.Warn("cluster shutdown", "err", err)
		}
	}()

	// 5. Tunnel forwarder: this node's only way to reach another Postgres.
	if a.role == cluster.RoleData {
		if err := a.openForwarder(); err != nil {
			return err
		}
		defer a.fwd.Close()
		// The client port (rule 10). Stopped before Postgres, below.
		if err := a.startProxy(ctx, ports.take(&ports.client)); err != nil {
			return err
		}
		defer a.stopProxy()
	}

	// 6. Agent RPC (members only, over the mTLS port) and the local
	// control channel (127.0.0.1, token).
	a.cn.Handle(transport.StreamAgent, a.serveAgent)
	if a.early != nil {
		// Served since before the cluster (setup page): from now on the
		// same port, guard and sign-ins serve the whole API.
		dash := a.newDash(ctx, a.early.sessions)
		a.early.serve(dash)
		a.ctl = a.early.srv
	} else {
		tok, err := control.EnsureToken(a.node.StateDir)
		if err != nil {
			return err
		}
		dash := a.newDash(ctx, nil) // the CLI's routes plus the dashboard's (D36)
		ctl, err := control.Serve(ports.take(&ports.control), tok, dash, a.log, a.controlOptions(dash.sessions)...)
		if err != nil {
			return err
		}
		a.ctl = ctl
		if err := control.WriteInfo(a.node.StateDir, control.Info{Addr: infoAddr(ctl.Addr()), PID: os.Getpid()}); err != nil {
			ctl.Close()
			return err
		}
		defer func() {
			control.RemoveInfo(a.node.StateDir)
			ctl.Close()
		}()
	}
	a.setup.watch(ctx, a)

	// 7. The controller (acts only while this node is the Raft leader) and,
	// on data nodes, the primary lease loop.
	a.ctrl = controller.New(controller.Config{
		Cluster: a.cn, Agents: agentsAdapter{a}, Timing: a.cn.Timing(), Busy: &a.switchMu,
		Log:   a.log.With("component", "controller"),
		Fault: func(ctx context.Context, point string) error { return fault(ctx, a, point) },
	})
	bg, stopBG := context.WithCancel(ctx)
	var bgWG sync.WaitGroup
	bgWG.Add(1)
	go func() { defer bgWG.Done(); a.ctrl.Run(bg) }()
	bgWG.Add(1)
	go func() { defer bgWG.Done(); a.peerLoop(bg) }()
	bgWG.Add(1)
	go func() { defer bgWG.Done(); a.addrLoop(bg) }()
	if a.role == cluster.RoleData {
		bgWG.Add(1)
		go func() { defer bgWG.Done(); a.leaseLoop(bg) }()
		bgWG.Add(1)
		go func() { defer bgWG.Done(); a.healthLoop(bg) }()
		bgWG.Add(1)
		go func() { defer bgWG.Done(); a.guardLoop(bg) }()
		bgWG.Add(1)
		go func() { defer bgWG.Done(); a.pageLoop(bg) }()
		bgWG.Add(1)
		go func() { defer bgWG.Done(); a.backupLoop(bg) }()
	}
	defer func() { stopBG(); bgWG.Wait() }()

	// 8. Postgres is stopped first on the way out (deferred last), while
	// the tunnel still carries the final WAL to the standbys; the client
	// port closes just before it, so no client is cut mid-statement by
	// the shutdown when it could have finished.
	defer func() {
		a.stopProxy()
		if err := a.stopPG(); err != nil {
			a.log.Warn("stop postgres", "err", err)
		}
	}()

	err = a.loop(ctx)
	switch {
	case err == nil && ctx.Err() != nil:
		a.say("\nStopping…")
		return nil
	case errors.Is(err, errLeft):
		return nil
	}
	return err
}

// plan decides bootstrap / join / start and fills role, name and ports.
func (a *Agent) plan() error {
	has, err := cluster.HasState(a.clusterDir())
	if err != nil {
		return err
	}
	pending, err := cluster.HasPendingJoin(a.clusterDir())
	if err != nil {
		return err
	}
	saved := a.st.get()
	var id cluster.Identity
	if has {
		if id, err = cluster.ReadIdentity(a.clusterDir()); err != nil {
			return err
		}
	}
	switch a.cfg.Mode {
	case ModeUp:
		switch {
		case has:
			a.lifecycle, a.role = lcStart, id.Role
		case pending:
			return &ExitError{Code: 1, Msg: "This machine started joining a cluster but did not finish. Run `" +
				brand.Command + " join` (or `" + brand.Command + " witness`) again with the same invite code to finish it."}
		default:
			a.lifecycle, a.role = lcBootstrap, cluster.RoleData
			if err := a.checkBootstrapData(); err != nil {
				return err
			}
		}
	case ModeJoin, ModeWitness:
		want := cluster.RoleData
		if a.cfg.Mode == ModeWitness {
			want = cluster.RoleWitness
		}
		if a.cfg.Invite == "" && has {
			// Already joined, started again without a code (D62): resume.
			if id.Role != want {
				return &ExitError{Code: 1, Msg: fmt.Sprintf("This machine already joined this cluster as a %s node; run `%s up` to start it.", id.Role, brand.Command)}
			}
			a.lifecycle, a.role = lcStart, id.Role
			break
		}
		inv, err := invite.Decode(a.cfg.Invite)
		if err != nil {
			return &ExitError{Code: 1, Msg: friendlyJoinError(err), Cause: err}
		}
		switch {
		case has && id.ClusterID != inv.ClusterID:
			return &ExitError{Code: 1, Msg: "This data folder already belongs to another cluster. To join a different one, use a new --data-dir."}
		case has && id.Role != want:
			return &ExitError{Code: 1, Msg: fmt.Sprintf("This machine already joined this cluster as a %s node; run `%s up` to start it.", id.Role, brand.Command)}
		case has:
			a.say("This machine already joined this cluster; resuming.")
			a.lifecycle, a.role = lcStart, id.Role
		default:
			if a.node.Initialized() {
				return &ExitError{Code: 1, Msg: fmt.Sprintf("%s already holds a database. Joining copies the data from the cluster, so it needs an empty data folder (use --data-dir).", a.node.StateDir)}
			}
			if !pending {
				if err := inv.Check(time.Now()); err != nil {
					return &ExitError{Code: 1, Msg: friendlyJoinError(err), Cause: err}
				}
			}
			a.lifecycle, a.role = lcJoin, want
		}
	}
	if saved.Role != "" && has && saved.Role != string(a.role) {
		return fmt.Errorf("agent.json says this node is a %s node but its cluster identity says %s", saved.Role, a.role)
	}

	// Name: fixed once the node is in a cluster.
	switch {
	case has && a.cfg.Name != "" && a.cfg.Name != id.Name:
		return &ExitError{Code: 2, Msg: fmt.Sprintf("this node is called %q; it cannot be renamed to %q", id.Name, a.cfg.Name)}
	case has:
		a.name = id.Name
	case a.cfg.Name != "":
		a.name = a.cfg.Name
	default:
		if info, err := a.node.ReadInfo(); err == nil && info.Name != "" {
			a.name = info.Name
		} else {
			h, _ := os.Hostname()
			a.name = pg.SanitizeName(h)
		}
	}
	if err := pg.ValidName(a.name); err != nil {
		return &ExitError{Code: 2, Msg: err.Error()}
	}

	// Ports and addresses: flag, else what was used last time, else default.
	a.pgPort = a.cfg.PGPort
	if a.pgPort == 0 {
		if info, err := a.node.ReadInfo(); err == nil && info.Port > 0 {
			a.pgPort = info.Port
		} else {
			a.pgPort = pg.DefaultPort
		}
	}
	a.listen = firstNonEmpty(a.cfg.Listen, saved.Listen, DefaultListen)
	a.advertise = a.cfg.Advertise
	if len(a.advertise) == 0 {
		a.advertise = saved.Advertise
	}
	// Only addresses given on this command line replace ones an operator
	// pinned with `node set-address` (which also updates the saved ones),
	// and only a value the cluster has not recorded before: the same
	// COYOTE_ADVERTISE at every container start must not undo a later
	// set-address (D62).
	a.advertiseExplicit = len(a.cfg.Advertise) > 0 && !slices.Equal(a.cfg.Advertise, saved.AdvertiseRecorded)
	var detected []string
	for _, ad := range a.advertise {
		if err := validHostPort(ad); err != nil {
			return &ExitError{Code: 2, Msg: fmt.Sprintf("--advertise %q: %v", ad, err)}
		}
	}
	if len(a.advertise) == 0 {
		adv, rep, err := advertiseFor(context.Background(), a.listen)
		if err != nil {
			return &ExitError{Code: 1, Msg: err.Error()}
		}
		cerr := containerAddressCheck(a.listen, adv, netdetect.Container)
		if cerr != nil && a.lifecycle != lcStart {
			return cerr
		}
		a.advertise = adv // nil: the listener's own address
		// Detected once, here, and saved below: from now on this is the
		// node's configured address, like one given with --advertise. It
		// is not detected again while running or at the next start; the
		// address check says when the computer no longer has it (D66).
		detected = adv
		a.addrNotes = describeAddresses(adv, rep)
		var ee *ExitError
		if errors.As(cerr, &ee) {
			// A member keeps running as before; it is only told.
			a.addrNotes = append(a.addrNotes, "Note: "+ee.Msg)
		}
	} else {
		a.addrNotes = describeAddresses(a.advertise, nil)
	}
	a.clientPort = cmp.Or(a.cfg.ClientPort, saved.ClientPort, DefaultClientPort)
	a.ctlPort = a.cfg.ControlPort
	if a.ctlPort == 0 {
		a.ctlPort = saved.ControlPort
	}
	if a.ctlPort == 0 {
		a.ctlPort = control.DefaultPort
	}
	return a.st.update(func(f *agentFile) {
		if a.cfg.Listen != "" {
			f.Listen = a.cfg.Listen
		}
		if len(a.cfg.Advertise) > 0 {
			f.Advertise = slices.Clone(a.cfg.Advertise)
		} else if len(detected) > 0 {
			f.Advertise = slices.Clone(detected)
			// Found on this computer's interfaces, so they can be
			// reported as gone later (addrwatch.go).
			for _, ad := range detected {
				if !slices.Contains(f.AddrSeenLocal, ad) {
					f.AddrSeenLocal = append(f.AddrSeenLocal, ad)
				}
			}
		}
		if a.cfg.ControlPort != 0 {
			f.ControlPort = a.cfg.ControlPort
		}
		if a.cfg.ClientPort != 0 {
			f.ClientPort = a.cfg.ClientPort
		}
	})
}

// checkBootstrapData: a new cluster may start from an empty data dir or
// from an M1 single node's primary data, never from anything that was a
// copy or a cluster member (D19: no fresh empty primary in its place).
func (a *Agent) checkBootstrapData() error {
	m, err := a.node.ReadMarker()
	if err != nil {
		return err
	}
	_, secErr := os.Stat(a.node.SecretsPath())
	switch {
	case m != nil:
		return &ExitError{Code: 1, Msg: pg.MarkerError{M: *m}.Error()}
	case a.node.Initialized() && a.node.IsStandbyDir():
		return &ExitError{Code: 1, Msg: "This data folder holds a copy made with the manual dev tools, not a cluster node; it cannot start a new cluster."}
	case !a.node.Initialized() && secErr == nil:
		return &ExitError{Code: 1, Msg: "This node's database folder is missing but the node was part of a cluster; a new, empty database will not be created in its place."}
	}
	return nil
}

func (a *Agent) cacheDir() string {
	c := a.cfg.Cache
	if c == "" {
		c = os.Getenv(brand.EnvPrefix + "PG_CACHE")
	}
	if c == "" {
		c = filepath.Join(a.node.StateDir, "cache")
	}
	return c
}

func (a *Agent) downloadProgress() func(done, total int64) {
	last := -1
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		step := int(done*100/total) / 10 * 10
		if step == last {
			return
		}
		if last == -1 {
			a.say("Downloading PostgreSQL %s (%d MB, first run only)…", pg.Version, total>>20)
		}
		last = step
		a.say("  %3d%%", step)
	}
}

func (a *Agent) openCluster(ctx context.Context, plat cluster.Platform) error {
	ccfg := cluster.Config{
		Dir:            a.clusterDir(),
		ListenAddr:     a.listen,
		AdvertiseAddrs: a.advertise,
		Name:           a.name,
		Role:           a.role,
		Platform:       plat,
		Timing:         a.cfg.Timing,
		Logger:         a.log.With("component", "cluster"),
		DialRedirect:   testDialMap(a.log),
		Listener:       a.clusterLn,
		// Announced so the cluster's addresses pinned with `node
		// set-address` are replaced only by an operator's --advertise.
		AdvertiseExplicit: a.advertiseExplicit,
		// Mixed systems (rule 1 extension, D37).
		AllowMixedOS:    a.cfg.AllowMixedOS,
		MixedJoinCheck:  a.mixedJoinCheck,
		ExplainPlatform: explainPlatform,
		MixedFieldsOK:   a.mixedFieldsOK,
		Version:         a.cfg.Version,
	}
	if a.role == cluster.RoleData {
		ccfg.PGAddr = a.localPGAddr
		ccfg.ClientPort = a.clientPort
	}
	var err error
	switch a.lifecycle {
	case lcBootstrap:
		// Remembered before the cluster exists: a crash right after
		// Bootstrap must still allow the first initdb, and a fresh cluster
		// always starts at epoch 1 with this node as primary.
		if err := a.st.update(func(f *agentFile) {
			f.Role = string(cluster.RoleData)
			f.FreshPrimary = !a.node.Initialized()
			f.LastPrimaryEpoch = 1
		}); err != nil {
			return err
		}
		a.setup.step(stepPrepare, stepActive, "")
		a.say("Creating a new cluster in %s …", a.node.StateDir)
		if platformOS() != "linux" {
			a.say("%s", linuxRecommended)
		}
		a.cn, err = cluster.Bootstrap(ctx, ccfg)
		if err != nil {
			return fmt.Errorf("could not create the cluster: %w", err)
		}
	case lcJoin:
		if err := a.st.update(func(f *agentFile) { f.Role = string(a.role) }); err != nil {
			return err
		}
		a.setup.step(stepContact, stepActive, "")
		a.say("Joining the cluster as %q …", a.name)
		if a.role == cluster.RoleData && platformOS() != "linux" {
			a.say("%s", linuxRecommended)
		}
		a.cn, err = cluster.Join(ctx, ccfg, a.cfg.Invite)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.log.Error("join failed", "err", err)
			return &ExitError{Code: 1, Msg: friendlyJoinError(err), Cause: err}
		}
		if a.role == cluster.RoleWitness {
			a.say("Joined as a witness: this node votes but keeps no data.")
		} else {
			a.say("Joined.")
		}
	case lcStart:
		a.cn, err = cluster.Start(ctx, ccfg)
		if err != nil {
			return fmt.Errorf("could not start the cluster node: %w", err)
		}
	}
	return nil
}

func (a *Agent) openForwarder() error {
	cfg := tunnel.ForwarderConfig{Dial: a.cn.PGDialer(), Logger: a.log.With("component", "tunnel")}
	port := a.st.get().TunnelPort
	var err error
	if port > 0 {
		cfg.ListenAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		a.fwd, err = tunnel.NewForwarder(cfg)
		if err != nil {
			a.log.Warn("tunnel port taken; picking another", "port", port, "err", err)
		}
	}
	if a.fwd == nil {
		cfg.ListenAddr = "127.0.0.1:0"
		if a.fwd, err = tunnel.NewForwarder(cfg); err != nil {
			return err
		}
	}
	return a.st.update(func(f *agentFile) { f.TunnelPort = a.fwd.Addr().Port })
}

// fwdUpstream is this node's Postgres upstream: always the local end of
// the tunnel (rule 11), whichever node is primary.
func (a *Agent) fwdUpstream() pg.Upstream {
	return pg.Upstream{Host: "127.0.0.1", Port: a.fwd.Addr().Port}
}

// localPGAddr is the pg stream backend's target (only while Postgres runs).
func (a *Agent) localPGAddr() string {
	if p := a.pgAddr.Load(); p != nil {
		return *p
	}
	return ""
}

// ---- Postgres process ----

// lockPG takes the Postgres lifecycle lock, giving up when ctx ends.
func (a *Agent) lockPG(ctx context.Context) (func(), error) {
	select {
	case a.pgSem <- struct{}{}:
		return func() { <-a.pgSem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("this node is busy (copying, rewinding or starting its database); try again shortly: %w", ctx.Err())
	}
}

func (a *Agent) currentPG() (*pg.Server, string) {
	a.pgMu.Lock()
	defer a.pgMu.Unlock()
	return a.srv, a.pgRole
}

// leasePG is the Postgres the lease applies to: the running one, or the
// one still starting.
func (a *Agent) leasePG() *pg.Server {
	a.pgMu.Lock()
	defer a.pgMu.Unlock()
	if a.srv != nil {
		return a.srv
	}
	return a.starting
}

func (a *Agent) setPGRole(role string) {
	a.pgMu.Lock()
	a.pgRole = role
	a.pgMu.Unlock()
}

// startPG starts Postgres from the data dir as it is (standby.signal
// decides the role) under the guard.
func (a *Agent) startPG(ctx context.Context) error {
	info, _ := a.node.ReadInfo()
	info.Name, info.Port = a.name, a.pgPort
	info.PGVersion, info.Target = a.inst.Build.Version, a.inst.Build.Target
	if err := a.node.WriteInfo(info); err != nil {
		return fmt.Errorf("write node info: %w", err)
	}
	if _, err := a.ensurePGTLS(a.cn.State()); err != nil {
		return fmt.Errorf("client TLS certificate: %w", err)
	}
	opts := pg.StartOptions{Install: a.inst, Port: a.pgPort, Name: a.name, Guard: a.guard, Logger: a.log}
	if !a.node.IsStandbyDir() {
		// Rule 4 from the first instant: the watchdog is armed with the
		// lease's absolute deadline right before the process exists (after
		// every file is written), and the lease loop renews (or fences) it
		// while it is still starting. A fence before the spawn vetoes it.
		if _, ok := a.leaseDeadline(); !ok {
			return pg.ErrLeaseExpired
		}
		ps := a.beginPrimaryStart()
		defer a.endPrimaryStart(ps)
		opts.PrimaryDeadline = func() (time.Duration, bool) { return a.startDeadline(ps) }
		opts.OnSpawn = func(s *pg.Server) { a.spawnedPrimary(ps, s) }
		opts.TestBeforeSpawn = a.stallBeforeSpawn(ctx)
		if a.slowStart {
			a.slowStart = false
			opts.TestReadyDelay = slowStartDelay
			a.say("Test fault point reached: slow-start-after-clear-fence (the start returns %s late).", slowStartDelay)
		}
	}
	srv, err := a.node.Start(ctx, opts)
	role := "primary"
	if a.node.IsStandbyDir() {
		role = "standby"
	}
	// From starting to running in one step: a fence never finds neither.
	a.pgMu.Lock()
	a.starting = nil
	if err == nil {
		a.srv, a.pgRole = srv, role
	}
	a.pgMu.Unlock()
	if err != nil {
		return fmt.Errorf("PostgreSQL did not start: %w", err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(a.pgPort))
	a.pgAddr.Store(&addr)
	a.log.Info("postgres running", "role", role, "pid", srv.PID(), "port", a.pgPort)
	return nil
}

// stopPG stops our Postgres (if running) and waits for it. The node is not
// a primary afterwards, so it drops its lease.
func (a *Agent) stopPG() error {
	defer a.dropLease()
	a.pgMu.Lock()
	srv := a.srv
	a.srv, a.pgRole = nil, ""
	a.pgMu.Unlock()
	empty := ""
	a.pgAddr.Store(&empty)
	if srv == nil {
		return nil
	}
	return srv.Stop(pg.DefaultStopTimeouts)
}

func (a *Agent) pgDone() <-chan struct{} {
	if a.isFencing() {
		return nil // the lease loop is stopping it on purpose
	}
	srv, _ := a.currentPG()
	if srv == nil {
		return nil
	}
	return srv.Done()
}

func (a *Agent) guardLost() <-chan struct{} {
	if a.guard == nil {
		return nil
	}
	return a.guard.Lost()
}

// ---- phases (what status shows, and what the person sees) ----

// setPhase records what the node is doing; msg (if new) is printed once.
func (a *Agent) setPhase(phase, msg string) {
	a.phaseMu.Lock()
	a.phase, a.phaseMsg = phase, msg
	show := msg != "" && msg != a.shownMsg
	if show {
		a.shownMsg = msg
	}
	a.phaseMu.Unlock()
	if show {
		a.say("%s", msg)
	}
}

func (a *Agent) phaseInfo() (string, string, int) {
	a.phaseMu.Lock()
	defer a.phaseMu.Unlock()
	return a.phase, a.phaseMsg, a.progress
}

func (a *Agent) setProgress(p int) {
	a.phaseMu.Lock()
	a.progress = p
	a.phaseMu.Unlock()
}

// requestExit ends the loop with err (nil = clean stop).
func (a *Agent) requestExit(err error) {
	select {
	case a.exitCh <- err:
	default:
	}
}

func (a *Agent) nudge() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// testDialEnv maps cluster addresses to others ("from=to,from=to"): the
// chaos tests route every connection between two agents through a proxy
// of their own. Unset in normal use.
const testDialEnv = "COYOTE_TEST_DIAL_MAP"

func testDialMap(log *slog.Logger) func(string) string {
	v := os.Getenv(testDialEnv)
	if v == "" {
		return nil
	}
	m := map[string]string{}
	for _, kv := range strings.Split(v, ",") {
		from, to, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if ok && from != "" && to != "" {
			m[from] = to
		}
	}
	log.Warn("test dial map in use", "entries", len(m))
	return func(addr string) string {
		if to, ok := m[addr]; ok {
			return to
		}
		return addr
	}
}

// ---- helpers ----

type printer struct {
	mu sync.Mutex
	w  io.Writer
}

func (p *printer) say(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.w, format+"\n", args...)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func quoteArg(s string) string {
	for _, r := range s {
		if r == ' ' || r == '\t' {
			return `"` + s + `"`
		}
	}
	return s
}

func secretsFrom(s cluster.PGSecrets) pg.Secrets {
	return pg.Secrets{Superuser: s.Superuser, Replication: s.Replication, Rewind: s.Rewind, App: s.App}
}

func secretsTo(s pg.Secrets) cluster.PGSecrets {
	return cluster.PGSecrets{Superuser: s.Superuser, Replication: s.Replication, Rewind: s.Rewind, App: s.App}
}
