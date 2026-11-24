package main

// `coyote dev …`: TEMPORARY one-shot replication tools for milestone M2
// (copying by hand). They are thin wrappers over internal/pg; from M3 the
// agent's controller calls the same functions in-process and these
// commands go away. Not a stable interface.
//
// Concurrency with a running agent:
//   - status, promote, cut-stream, set-upstream and set-sync talk to the
//     node's running Postgres over SQL. They change files only through
//     pg.Node methods that hold the node's config lock, which the agent
//     also takes while it writes config on start, so they cannot race.
//   - rewind, reclone and demote need the agent stopped: they take the node
//     lock themselves (and fail fast if the agent holds it), and run any
//     Postgres program under the same process guard as the agent.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/pg"
)

func devUsage(w io.Writer) {
	fmt.Fprintf(w, `%[1]s dev: TEMPORARY, UNSTABLE tools. They will be replaced by automatic
failover. Each acts through the node chosen with --data-dir.

On a cluster node (started with up / join), while its agent runs:
  %[1]s dev status          this node's own report as JSON
  %[1]s dev switch --to N   make node N the main node (safety rules 2, 3, 6)
  %[1]s dev set-sync        --standbys a,b [--count 1]   the sync set (empty: async)
  %[1]s dev cut-stream      disconnect this copy until the next switch
  %[1]s dev resume-stream   undo cut-stream

Manual mode (a node started with dev manual-up, not in a cluster):
  %[1]s dev manual-up       run a node by hand [--clone-from HOST:PORT
                           --upstream-password-file FILE]
  %[1]s dev status / cut-stream / set-sync (as above, on that node)
  %[1]s dev promote         [--slots-for a,b]   cut the stream, then promote
  %[1]s dev set-upstream    --upstream HOST:PORT  follow another primary
  With the agent stopped:
  %[1]s dev demote          mark a former primary: never start it as primary
  %[1]s dev rewind          --from HOST:PORT  rejoin as standby
  %[1]s dev reclone         --from HOST:PORT  replace the data with a fresh copy
`, brand.Command)
}

func cmdDev(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		devUsage(stdout)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	sub, rest := args[0], args[1:]
	if sub == "manual-up" {
		return cmdManualUp(rest, stdout, stderr)
	}
	if node, err := resolveDataDir(peekDataDir(rest)); err == nil && (isClusterNode(node) || sub == "switch" || sub == "resume-stream") {
		return cmdDevCluster(sub, rest, stdout, stderr)
	}
	fs := flag.NewFlagSet("dev "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	d := &devCmd{out: stdout}
	var upstream, from, standbys, slotsFor, cacheDir *string
	var count *int
	switch sub {
	case "set-upstream":
		upstream = fs.String("upstream", "", "HOST:PORT of the new primary's Postgres")
	case "set-sync":
		standbys = fs.String("standbys", "", "comma-separated node names; empty clears the list (async)")
		count = fs.Int("count", 1, "how many of them must confirm each commit")
	case "promote":
		slotsFor = fs.String("slots-for", "", "comma-separated names of the other nodes (a slot is made for each)")
	case "rewind", "reclone":
		from = fs.String("from", "", "HOST:PORT of the current primary's Postgres")
		cacheDir = fs.String("pg-cache", "", "Postgres program files (default: $"+brand.EnvPrefix+"PG_CACHE or <data-dir>/cache)")
	case "demote":
		cacheDir = fs.String("pg-cache", "", "Postgres program files (default: $"+brand.EnvPrefix+"PG_CACHE or <data-dir>/cache)")
	case "status", "cut-stream":
	default:
		fmt.Fprintf(stderr, "Unknown dev command %q.\n\n", sub)
		devUsage(stderr)
		return 2
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "Unexpected argument %q.\n", fs.Arg(0))
		return 2
	}
	node, err := resolveDataDir(*dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	d.node = node
	if err := node.Prepare(); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	lf, err := os.OpenFile(filepath.Join(node.LogDir(), brand.Command+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	defer lf.Close()
	d.log = slog.New(slog.NewTextHandler(lf, nil)).With("dev_command", sub, "pid", os.Getpid())
	d.log.Info("start", "args", rest)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch sub {
	case "status":
		err = d.status(ctx)
	case "cut-stream":
		err = d.cutStream(ctx)
	case "promote":
		err = d.promote(ctx, splitList(*slotsFor))
	case "set-upstream":
		err = d.setUpstream(ctx, *upstream)
	case "set-sync":
		err = d.setSync(ctx, splitList(*standbys), *count)
	case "demote":
		err = d.withStoppedNode(ctx, *cacheDir, func(inst pg.Install, _ *pg.Guard) error {
			if err := node.Demote(ctx, inst, "dev demote"); err != nil {
				return err
			}
			d.say("Marked: this node will not start as a primary until it is rewound or re-cloned.")
			return nil
		})
	case "rewind", "reclone":
		err = d.rejoin(ctx, sub, *from, *cacheDir)
	}
	if err != nil {
		d.log.Error("failed", "err", err)
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(stderr, "Cancelled.")
			return 130
		}
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	d.log.Info("done")
	return 0
}

type devCmd struct {
	node pg.Node
	log  *slog.Logger
	out  io.Writer
}

func (d *devCmd) say(format string, a ...any) { fmt.Fprintf(d.out, format+"\n", a...) }

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// devStatus is the JSON printed by `dev status`.
type devStatus struct {
	Name      string         `json:"name"`
	DataDir   string         `json:"data_dir"`
	Port      int            `json:"port"`
	Running   bool           `json:"running"`
	Marker    *pg.Marker     `json:"rejoin_marker"`
	Persisted pg.ReplState   `json:"persisted"`
	Postgres  *pg.ReplStatus `json:"postgres,omitempty"`
}

func (d *devCmd) status(ctx context.Context) error {
	info, err := d.node.ReadInfo()
	if err != nil {
		return err
	}
	s := devStatus{Name: info.Name, DataDir: d.node.StateDir, Port: info.Port}
	if s.Marker, err = d.node.ReadMarker(); err != nil {
		return err
	}
	if s.Persisted, err = d.node.ReadReplState(); err != nil {
		return err
	}
	st, err := d.node.Status()
	if err != nil {
		return err
	}
	s.Running = st.Running
	if st.Running {
		c, err := d.node.Connect(ctx)
		if err != nil {
			return err
		}
		defer c.Close(context.Background())
		rs, err := pg.ReplicationStatus(ctx, c)
		if err != nil {
			return err
		}
		s.Postgres = &rs
	}
	enc := json.NewEncoder(d.out)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

func (d *devCmd) cutStream(ctx context.Context) error {
	lsn, err := d.node.CutStream(ctx, 30*time.Second)
	if err != nil {
		return err
	}
	d.say("Stream cut. Received (flushed) up to %s.", lsn)
	return nil
}

func (d *devCmd) promote(ctx context.Context, members []string) error {
	lsn, err := d.node.CutStream(ctx, 30*time.Second)
	if err != nil {
		return fmt.Errorf("cut stream: %w", err)
	}
	d.say("Stream cut at %s.", lsn)
	tli, err := d.node.Promote(ctx, pg.PromoteOptions{Members: members})
	if err != nil {
		return err
	}
	d.say("Promoted: this node is now the primary, on timeline %d.", tli)
	return nil
}

func (d *devCmd) setUpstream(ctx context.Context, s string) error {
	up, err := pg.ParseUpstream(s)
	if err != nil {
		return err
	}
	if err := d.node.SetUpstream(ctx, up, 60*time.Second); err != nil {
		return err
	}
	d.say("Streaming from %s.", up)
	return nil
}

func (d *devCmd) setSync(ctx context.Context, names []string, count int) error {
	if len(names) == 0 {
		count = 0
	}
	if err := d.node.SetSync(ctx, count, names, 30*time.Second); err != nil {
		return err
	}
	if len(names) == 0 {
		d.say("Synchronous list cleared: commits no longer wait for a standby.")
	} else {
		d.say("Commits now wait for %d of: %s.", count, strings.Join(names, ", "))
	}
	return nil
}

// withStoppedNode takes the node lock (the agent must be stopped), stops
// any leftover Postgres, starts a process guard and runs f.
func (d *devCmd) withStoppedNode(ctx context.Context, cacheDir string, f func(pg.Install, *pg.Guard) error) error {
	lock, err := d.node.LockNode(ctx, 5*time.Second)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	cache := cacheDir
	if cache == "" {
		cache = os.Getenv(brand.EnvPrefix + "PG_CACHE")
	}
	if cache == "" {
		cache = filepath.Join(d.node.StateDir, "cache")
	}
	inst, err := pg.Ensure(ctx, pg.FetchOptions{CacheDir: cache, Logger: d.log})
	if err != nil {
		return err
	}
	if d.node.Initialized() {
		if _, err := d.node.StopLeftover(ctx, inst, d.log); err != nil {
			return err
		}
	}
	guard, err := pg.NewGuard(d.node, lock, d.log)
	if err != nil {
		return err
	}
	defer func() {
		if err := guard.Close(); err != nil {
			d.log.Warn("guard close", "err", err)
		}
	}()
	return f(inst, guard)
}

func (d *devCmd) rejoin(ctx context.Context, sub, from, cacheDir string) error {
	if from == "" {
		return errors.New("--from HOST:PORT is required")
	}
	up, err := pg.ParseUpstream(from)
	if err != nil {
		return err
	}
	return d.withStoppedNode(ctx, cacheDir, func(inst pg.Install, g *pg.Guard) error {
		o := pg.RewindOptions{Install: inst, Guard: g, Source: up, Logger: d.log}
		if sub == "reclone" {
			d.say("Re-cloning from %s …", up)
			if err := d.node.Reclone(ctx, o); err != nil {
				return err
			}
			d.say("Re-cloned. Start the node with `%s up`; it runs as a standby of %s.", brand.Command, up)
			return nil
		}
		d.say("Rewinding from %s …", up)
		recloned, err := d.node.RewindOrReclone(ctx, o)
		if err != nil {
			return err
		}
		if recloned {
			d.say("Rewind was not possible; re-cloned instead.")
		} else {
			d.say("Rewound.")
		}
		d.say("Start the node with `%s up`; it runs as a standby of %s.", brand.Command, up)
		return nil
	})
}
