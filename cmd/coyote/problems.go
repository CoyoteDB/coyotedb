package main

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/brand"
)

// printProblems prints the status problems (D66): by default one plain
// line, the code and the first fix command each; with details the long
// form (what happened, causes, evidence, every command). Then the
// warnings that have no code.
func printProblems(w io.Writer, s agent.Status, details bool) {
	if len(s.Problems) == 0 && len(s.Warnings) == 0 {
		return
	}
	fmt.Fprintln(w)
	for i, p := range s.Problems {
		if details {
			if i > 0 {
				fmt.Fprintln(w)
			}
			printProblemDetails(w, p)
			continue
		}
		fmt.Fprintf(w, "! %s [%s]\n", p.Summary, p.Code)
		if len(p.Commands) > 0 {
			fmt.Fprintf(w, "    Fix: %s\n", p.Commands[0])
		}
	}
	for _, m := range s.Warnings {
		if !slices.ContainsFunc(s.Problems, func(p agent.Problem) bool { return p.Summary == m }) {
			fmt.Fprintf(w, "! %s\n", m)
		}
	}
	if !details && len(s.Problems) > 0 {
		fmt.Fprintf(w, "\nFor the long form (causes, evidence, commands): %s status --details. For a code: %s explain CODE.\n", brand.Command, brand.Command)
	}
}

func printProblemDetails(w io.Writer, p agent.Problem) {
	about := ""
	if p.Node != "" {
		about = " (" + p.Node + ")"
	}
	fmt.Fprintf(w, "== %s %s%s\n", strings.ToUpper(p.Severity), p.Code, about)
	fmt.Fprintf(w, "%s\n", p.Summary)
	if p.Fix != "" {
		fmt.Fprintf(w, "Fix: %s\n", p.Fix)
	}
	for _, c := range p.Commands {
		fmt.Fprintf(w, "    %s\n", c)
	}
	if p.Details != "" {
		fmt.Fprintln(w)
		for _, para := range strings.Split(p.Details, "\n\n") {
			fmt.Fprintf(w, "%s\n", para)
		}
	}
	if !p.Since.IsZero() || len(p.Evidence) > 0 {
		fmt.Fprintln(w, "\nEvidence:")
		if !p.Since.IsZero() {
			fmt.Fprintf(w, "  - Since %s (%s ago).\n", p.Since.Local().Format("2006-01-02 15:04:05 MST"), time.Since(p.Since).Round(time.Second))
		}
		for _, e := range p.Evidence {
			fmt.Fprintf(w, "  - %s\n", e)
		}
	}
}

// cmdExplain is `coyote explain [CODE]`: what a code means and how to fix
// it. It needs no running node.
func cmdExplain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--list" {
		fmt.Fprintf(stdout, "Codes shown by `%s status`, the API and the dashboard (`%s explain CODE` for one):\n\n", brand.Command, brand.Command)
		for _, c := range agent.ProblemCodes() {
			fmt.Fprintf(stdout, "  %-24s %s\n", c.Code, c.Title)
		}
		return 0
	}
	if len(args) > 1 {
		fmt.Fprintf(stderr, "Usage: %s explain [CODE]\n", brand.Command)
		return 2
	}
	c, ok := agent.LookupCode(strings.TrimSpace(args[0]))
	if !ok {
		fmt.Fprintf(stderr, "No code %q. `%s explain` lists them.\n", args[0], brand.Command)
		return 2
	}
	fmt.Fprint(stdout, agent.ExplainText(c))
	return 0
}
