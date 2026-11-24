package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/brand"
)

// `coyote dev …` on a cluster node: the operations go through the
// node's running agent (control channel), which routes them through the
// cluster state. The per-node manual tools (promote, set-upstream,
// rewind, reclone, demote) are refused: in a cluster the agent does those
// steps itself, in the right order.
func cmdDevCluster(sub string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	ctlPort := controlPortFlag(fs)
	var to, standbys *string
	var count *int
	switch sub {
	case "switch":
		to = fs.String("to", "", "the node that becomes the main node")
	case "set-sync":
		standbys = fs.String("standbys", "", "comma-separated node names; empty clears the sync set (writes stop waiting for a copy)")
		count = fs.Int("count", 1, "how many of them must confirm each commit")
	case "status", "cut-stream", "resume-stream":
	case "promote", "set-upstream", "rewind", "reclone", "demote":
		fmt.Fprintf(stderr, "This node is part of a cluster: its agent does %q by itself. To move the main node, use `%s dev switch --to <node>`.\n", sub, brand.Command)
		return 2
	default:
		fmt.Fprintf(stderr, "Unknown dev command %q.\n\n", sub)
		devUsage(stderr)
		return 2
	}
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
		return 2
	}
	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	switch sub {
	case "status":
		var r agent.NodeReport
		if err := c.Do(ctx, http.MethodGet, "/dev/status", nil, &r); err != nil {
			return apiFail(stderr, node, err)
		}
		return printJSON(stdout, r)
	case "switch":
		if *to == "" {
			fmt.Fprintf(stderr, "Usage: %s dev switch --to <node>\n", brand.Command)
			return 2
		}
		fmt.Fprintf(stdout, "Switching the main node to %s…\n", *to)
		var r agent.SwitchResult
		if err := c.Do(ctx, http.MethodPost, "/dev/switch", map[string]string{"to": *to}, &r); err != nil {
			return apiFail(stderr, node, err)
		}
		fmt.Fprintln(stdout, r.Message)
		return 0
	case "set-sync":
		body := map[string]any{"standbys": splitList(*standbys), "count": *count}
		var r map[string]string
		if err := c.Do(ctx, http.MethodPost, "/dev/set-sync", body, &r); err != nil {
			return apiFail(stderr, node, err)
		}
		fmt.Fprintln(stdout, r["message"])
		return 0
	}
	var r map[string]string
	if err := c.Do(ctx, http.MethodPost, "/dev/"+sub, nil, &r); err != nil {
		return apiFail(stderr, node, err)
	}
	fmt.Fprintln(stdout, r["message"])
	return 0
}

// peekDataDir finds --data-dir in args without parsing the rest.
func peekDataDir(args []string) string {
	for i, a := range args {
		for _, p := range []string{"--data-dir", "-data-dir"} {
			switch {
			case a == p && i+1 < len(args):
				return args[i+1]
			case strings.HasPrefix(a, p+"="):
				return a[len(p)+1:]
			}
		}
	}
	return ""
}
