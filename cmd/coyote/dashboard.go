package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"coyotedb.com/coyote/internal/agent"
	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/control"
)

// dataDirHint is " --data-dir DIR" when one was given.
func dataDirHint(d string) string {
	if d == "" {
		return ""
	}
	return " --data-dir " + quoteArg(d)
}

// cmdDashboard asks the running agent for a one-time sign-in link and
// opens it in the default browser (dashboard/API.md §2.1, D36).
func cmdDashboard(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := dataDirFlag(fs)
	ctlPort := controlPortFlag(fs)
	noOpen := fs.Bool("no-open", false, "only print the sign-in link, do not open a browser")
	host := fs.String("host", "", "host name in the link: 127.0.0.1 (default), localhost or ::1")
	if pos, err := parseArgs(fs, args); err != nil || len(pos) > 0 {
		return 2
	}
	c, node, err := client(*dataDir, *ctlPort)
	if errors.Is(err, control.ErrNotRunning) && !isClusterNode(node) {
		fmt.Fprintf(stderr, "This computer is not set up yet. Run `%s setup%s` to set it up in your browser (or `%s up` to create a cluster right here).\n",
			brand.Command, dataDirHint(*dataDir), brand.Command)
		return 3
	}
	if err != nil {
		return apiFail(stderr, node, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var link agent.SessionLink
	if err := c.Do(ctx, http.MethodPost, "/session/links", agent.SessionLinkRequest{Host: *host}, &link); err != nil {
		return apiFail(stderr, node, err)
	}
	opened := false
	if !*noOpen {
		if err := openBrowser(link.URL); err == nil {
			opened = true
		} else {
			fmt.Fprintf(stderr, "Could not open a browser (%v). Open the link below yourself.\n", err)
		}
	}
	if opened {
		fmt.Fprintf(stdout, "Opening the %s dashboard in your browser.\nIf it does not open, use this link on this computer (it works once, for 2 minutes):\n\n%s\n", brand.Product, link.URL)
	} else {
		fmt.Fprintf(stdout, "Open this link in a browser on this computer (it works once, for 2 minutes):\n\n%s\n", link.URL)
	}
	return 0
}

// openBrowser starts the system's URL opener directly (never through a
// shell) and does not wait for the browser.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "linux", "freebsd", "openbsd", "netbsd":
		p, err := exec.LookPath("xdg-open")
		if err != nil {
			return errors.New("xdg-open is not installed")
		}
		cmd = exec.Command(p, url)
	default:
		return fmt.Errorf("no browser opener known for %s", runtime.GOOS)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
