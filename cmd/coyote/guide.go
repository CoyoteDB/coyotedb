package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"strconv"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/paths"
	"coyotedb.com/coyote/internal/pg"
)

// advertiseEnv is the environment variable that does what --advertise
// does (D51).
const advertiseEnv = brand.EnvPrefix + "ADVERTISE"

// guide is what `coyote` alone prints: the first steps in plain words.
// When this computer already runs a node in the default folder it says
// that instead of the first-run steps.
func guide(w io.Writer) {
	c := brand.Command
	fmt.Fprintf(w, "%s runs a PostgreSQL database for you, on one computer or several.\n\n", brand.Product)
	if d, err := paths.DefaultDataDir(); err == nil {
		if node, err := pg.NewNode(d); err == nil && isClusterNode(node) {
			fmt.Fprintf(w, "This computer is already set up (its data is in %s).\n\n", d)
			fmt.Fprintf(w, "  %s status       see the computers in the cluster and which one is the main node\n", c)
			fmt.Fprintf(w, "  %s dashboard    open the dashboard in your browser\n", c)
			fmt.Fprintf(w, "  %s conninfo     print the connection string for your app\n", c)
			fmt.Fprintf(w, "  %s invite       get a code to add another computer\n", c)
			fmt.Fprintf(w, "  %s up           start it again if it is not running\n\n", c)
			fmt.Fprintf(w, "All commands: %s help\n", c)
			return
		}
	}
	fmt.Fprintf(w, `Start on the first computer:

  %[1]s up        creates your database here and keeps it running
                   (or %[1]s setup: the same, step by step in your web browser)

Add more computers, so the database keeps running if one stops:

  1. On the first computer:  %[1]s invite    prints a code that works once
  2. On the new computer:    %[1]s join      paste the code when it asks

  With three computers (or two and a small "witness": %[1]s witness), any one
  of them can stop and the others carry on.

Connect your app:

  %[1]s conninfo  prints the connection string (any computer of the cluster works)

Which computers to use: Linux is the recommended system for database nodes.
macOS and Windows work too. A witness can run on anything.

All commands: %[1]s help
`, c)
}

// checkDashListen checks --dashboard-listen HOST:PORT and returns the port,
// which becomes the control port (one port serves both, D52).
func checkDashListen(s string, ctlPort int) (int, error) {
	host, ps, err := net.SplitHostPort(s)
	if err != nil {
		return 0, errors.New("give it as HOST:PORT, for example 0.0.0.0:7480")
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return 0, errors.New("HOST must be an IP address of this computer or container, for example 0.0.0.0")
	}
	p, err := strconv.Atoi(ps)
	if err != nil || p < 1 || p > 65535 {
		return 0, errors.New("PORT must be between 1 and 65535")
	}
	if ctlPort != 0 && ctlPort != p {
		return 0, fmt.Errorf("its port must be the control port (--control-port %d); the dashboard and the local control channel share one port", ctlPort)
	}
	return p, nil
}

// prepareHint is the next step after the data folder could not be made.
func prepareHint(dir string, err error) string {
	switch {
	case errors.Is(err, fs.ErrPermission):
		return fmt.Sprintf("This account may not write there. Pick a folder you own with --data-dir, or fix the folder's owner (it must be the account that runs %s).", brand.Command)
	default:
		return "Pick another folder with --data-dir, or fix the problem named above and run the command again."
	}
}
