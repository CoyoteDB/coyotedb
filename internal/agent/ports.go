package agent

import (
	"fmt"
	"net"
	"strconv"

	"coyotedb.com/coyote/internal/cluster"
	"coyotedb.com/coyote/internal/control"
)

// localPorts are the listeners bound before the cluster is contacted.
// Each is handed over (take) to its user; whatever was not taken is closed.
type localPorts struct {
	cluster, client, control net.Listener
}

func (p *localPorts) take(l *net.Listener) net.Listener {
	ln := *l
	*l = nil
	return ln
}

func (p *localPorts) close() {
	for _, l := range []net.Listener{p.cluster, p.client, p.control} {
		if l != nil {
			l.Close()
		}
	}
}

// reservePorts binds the cluster, client and control ports, so that a port in use stops this command before anything
// changes in the cluster. The message names the port and the flag.
func (a *Agent) reservePorts() (*localPorts, error) {
	p := &localPorts{}
	fail := func(err error) (*localPorts, error) {
		p.close()
		return nil, err
	}
	var err error
	if a.early == nil {
		if p.control, err = a.listenControl(); err != nil {
			return fail(err)
		}
	}
	if p.cluster, err = cluster.Listen(a.cfg.Timing, a.listen); err != nil {
		_, port, _ := net.SplitHostPort(a.listen)
		n, _ := strconv.Atoi(port)
		return fail(&ExitError{Code: 1, Msg: portMsg(n, "the cluster port ("+a.listen+")", "--listen HOST:PORT", err), Cause: err})
	}
	if a.role != cluster.RoleData {
		return p, nil
	}
	if p.client, err = net.Listen("tcp", a.proxyListen()); err != nil {
		if addrInUse(err) {
			return fail(&ExitError{Code: 1, Msg: portInUseMessage(a.clientPort), Cause: err})
		}
		return fail(fmt.Errorf("open the client port %s: %w", a.proxyListen(), err))
	}
	return p, nil
}

// listenControl binds the local control port: 127.0.0.1, or the
// --dashboard-listen host (D52).
func (a *Agent) listenControl() (net.Listener, error) {
	host := "127.0.0.1"
	if a.cfg.DashboardListen != "" {
		h, _, err := net.SplitHostPort(a.cfg.DashboardListen)
		if err != nil {
			return nil, &ExitError{Code: 2, Msg: fmt.Sprintf("--dashboard-listen %q: %v", a.cfg.DashboardListen, err)}
		}
		host = h
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(a.ctlPort)))
	if err != nil {
		return nil, &ExitError{Code: 1, Msg: portMsg(a.ctlPort, "the local control port", "--control-port N", err), Cause: err}
	}
	return ln, nil
}

// controlOptions is how the guard is set up for this agent.
func (a *Agent) controlOptions(s *sessionStore) []control.Option {
	opts := []control.Option{control.WithBrowserSession(s)}
	if a.cfg.DashboardListen != "" {
		opts = append(opts, control.WithRemoteBrowsers())
	}
	return opts
}

// infoAddr is the address the CLI dials: loopback, also when the port is
// bound on every address (--dashboard-listen 0.0.0.0:PORT).
func infoAddr(addr string) string {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsUnspecified() {
		if ip.To4() == nil {
			return net.JoinHostPort("::1", p)
		}
		return net.JoinHostPort("127.0.0.1", p)
	}
	return addr
}

// checkPGPort: Postgres binds its own port later; check that it is free
// now. Our own leftover Postgres has been stopped by then, so a holder is
// another program.
func (a *Agent) checkPGPort() error {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(a.pgPort)))
	if err != nil {
		return &ExitError{Code: 1, Msg: portMsg(a.pgPort, "the PostgreSQL port on 127.0.0.1 (often another PostgreSQL)", "--pg-port N", err), Cause: err}
	}
	return l.Close()
}

func portMsg(port int, what, flag string, err error) string {
	if addrInUse(err) {
		return fmt.Sprintf("Port %d, %s, is already in use on this machine. Nothing was changed. Free it, or pick another port with %s.", port, what, flag)
	}
	return fmt.Sprintf("Could not open port %d, %s: %v. Nothing was changed. Pick another port with %s.", port, what, err, flag)
}
