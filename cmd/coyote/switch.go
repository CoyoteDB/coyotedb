package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"coyotedb.com/coyote/internal/agent"
)

// `coyote switch --to <node>`: a planned switch (the M4 rules).
func cmdSwitch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("switch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	ctlPort := controlPortFlag(fs)
	to := fs.String("to", "", "the node that becomes the main node")
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
		return 2
	}
	if *to == "" {
		fmt.Fprintln(stderr, "Usage: switch --to <node>")
		return 2
	}
	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	var r agent.SwitchResult
	if err := c.Do(ctx, http.MethodPost, "/switch", map[string]string{"to": *to}, &r); err != nil {
		return apiFail(stderr, node, err)
	}
	fmt.Fprintln(stdout, r.Message)
	return 0
}

// `coyote settings [--json]` and `coyote settings set key=value ...`.
func cmdSettings(args []string, stdout, stderr io.Writer) int {
	set := len(args) > 0 && args[0] == "set"
	if set {
		args = args[1:]
	}
	fs := flag.NewFlagSet("settings", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	ctlPort := controlPortFlag(fs)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parseArgs(fs, args)
	if err != nil || (!set && len(pos) > 0) || (set && len(pos) == 0) {
		fmt.Fprintf(stderr, "Usage: settings [--json]\n       settings set key=value ...   (keys: %s)\n", strings.Join(agent.SettingKeys, ", "))
		return 2
	}
	c, node, err := client(*dataDir, *ctlPort)
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var v agent.SettingsView
	if set {
		req := agent.SetSettingsRequest{Set: map[string]string{}}
		for _, kv := range pos {
			k, val, ok := strings.Cut(kv, "=")
			if !ok || !slices.Contains(agent.SettingKeys, k) {
				fmt.Fprintf(stderr, "%q: expected key=value with key one of %s\n", kv, strings.Join(agent.SettingKeys, ", "))
				return 2
			}
			req.Set[k] = val
		}
		err = c.Do(ctx, http.MethodPost, "/settings", req, &v)
	} else {
		err = c.Do(ctx, http.MethodGet, "/settings", nil, &v)
	}
	if err != nil {
		return apiFail(stderr, node, err)
	}
	if *asJSON {
		return printJSON(stdout, v)
	}
	tw := tabwriter.NewWriter(stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "lease_duration\t%s\tthe main node stops its database if it cannot renew its lease for this long\n", v.LeaseDuration)
	fmt.Fprintf(tw, "lease_renew_interval\t%s\thow often it renews\n", v.LeaseRenewInterval)
	fmt.Fprintf(tw, "single_data_node_mode\t%s\tstrict: writes wait for a copy; degrade_after: carry on alone after degrade_after\n", v.SingleDataNodeMode)
	fmt.Fprintf(tw, "degrade_after\t%s\thow long writes wait for a lost copy before the main node carries on alone\n", v.DegradeAfter)
	fmt.Fprintf(tw, "gone_after\t%s\ta node away this long must copy the data again\n", v.GoneAfter)
	tw.Flush()
	if set {
		fmt.Fprintln(stdout, "Saved. A new lease length applies from the next change of main node.")
	}
	return 0
}
