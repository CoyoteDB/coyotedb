// Command campaign drives the Step 3 proof campaign for mixed-system
// clusters (docs/MIXED_OS.md): a real cluster whose data nodes run on
// different operating systems and processors, a heavy workload, planned
// switches in every direction between every pair of nodes, and after
// every switch a full comparison of all nodes (fingerprint, behaviour
// probe, portability audit, integrity check, row-by-row content digests,
// and a ledger of acknowledged writes).
//
// It has two modes:
//
//	campaign peer --data-dir DIR --listen HOST:PORT --token-file FILE
//	    Runs on every data node, as the same OS user as the node's agent.
//	    It reads the node's own data folder, talks to the agent's
//	    local-only control channel, and runs the checks against the
//	    node's own Postgres (which listens on 127.0.0.1 only, rule 11).
//	    It serves them to the driver over HTTP with a bearer token. Put
//	    it on a private network (Tailscale) or behind an SSH tunnel: the
//	    token is the only protection.
//
//	campaign run --config nodes.json --out report.jsonl [--duration 72h]
//	    The driver. nodes.json lists the peers:
//	        {"token_file": "token", "nodes": [{"name": "mac", "peer": "http://100.64.0.1:7499"}, ...]}
//	    The node names are the cluster's node names (as `coyote status`
//	    shows them). Every result goes to the JSONL report, one record
//	    per line; the run stops at the first failed comparison unless
//	    --keep-going.
//
// It uses only what exists today: the agent's control API (/v1/status,
// /v1/switch, /v1/conninfo) and the checks in internal/pg. It cannot
// run end to end until the cluster allows mixed joins (Step 2 phase 2).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch args[0] {
	case "peer":
		err = cmdPeer(ctx, args[1:], stderr)
	case "run":
		err = cmdRun(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		usage(stderr)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "campaign: %v\n", err)
		if err == errUsage {
			return 2
		}
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  campaign peer --data-dir DIR --listen HOST:PORT --token-file FILE [--pg-cache DIR]
  campaign run  --config nodes.json --out report.jsonl [--duration D] [--keep-going]

Run "campaign peer -h" or "campaign run -h" for all options.
`)
}
