package agent

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/netdetect"
)

// The setup page and the control panel from a container's host (D68).
//
// The image sets COYOTE_DASHBOARD_LISTEN=0.0.0.0:7480. It is the same as
// --dashboard-listen (D52), but it is only used where it is needed and
// safe: inside a container whose own addresses are all on the container's
// private network (Docker's bridge), so the port is reachable only through
// what the host publishes (-p 127.0.0.1:7480:7480) and from the container
// network. Anywhere else (not a container, or a container sharing the
// host's network with --network host) it is ignored and the port stays on
// 127.0.0.1: there the host's browser reaches loopback directly, and
// 0.0.0.0 would put the port on the host's own network.

// DashboardListenEnv is the environment form of --dashboard-listen, for
// container images (D68).
const DashboardListenEnv = brand.EnvPrefix + "DASHBOARD_LISTEN"

// ContainerDashboardListen decides what COYOTE_DASHBOARD_LISTEN (env)
// turns into: the HOST:PORT to bind, or "" to stay on 127.0.0.1, and a
// sentence to print when it was set but not used. ctlPort, when given
// (--control-port), replaces the port in env.
func ContainerDashboardListen(env string, ctlPort int, inContainer func() (string, bool), addrs func() ([]netip.Addr, error)) (listen, note string) {
	env = strings.TrimSpace(env)
	if env == "" {
		return "", ""
	}
	host, port, err := net.SplitHostPort(env)
	if err != nil {
		return "", fmt.Sprintf("%s=%q is not HOST:PORT (for example 0.0.0.0:7480); the control panel stays on 127.0.0.1.", DashboardListenEnv, env)
	}
	if ctlPort != 0 {
		port = strconv.Itoa(ctlPort)
	}
	kind, ok := inContainer()
	if !ok {
		return "", fmt.Sprintf("%s is only used inside a container; the control panel stays on 127.0.0.1.", DashboardListenEnv)
	}
	as, err := addrs()
	if err != nil {
		return "", fmt.Sprintf("%s: could not list this container's addresses (%v); the control panel stays on 127.0.0.1.", DashboardListenEnv, err)
	}
	if a, shared := hostAddress(as); shared {
		return "", fmt.Sprintf("This %s container has the address %s, which is not on a container network (--network host?). "+
			"The control panel stays on 127.0.0.1:%s, which a browser on this computer reaches directly.", kind, a, port)
	}
	return net.JoinHostPort(host, port), ""
}

// hostAddress returns an address that shows the container shares the
// host's network (or has its own address on the LAN): one that is not
// loopback, link-local or on a container network.
func hostAddress(as []netip.Addr) (netip.Addr, bool) {
	for _, a := range as {
		a = a.Unmap()
		switch {
		case a.IsLoopback(), a.IsLinkLocalUnicast(), a.IsMulticast(), a.IsUnspecified():
			continue
		case a.Is4() && netdetect.ContainerPrivate(a):
			continue
		case a.Is6() && (a.IsPrivate() || !a.IsGlobalUnicast()):
			continue // Docker's IPv6 bridges use unique local addresses
		}
		return a, true
	}
	return netip.Addr{}, false
}

// localAddrs lists this computer's (or container's) interface addresses.
func localAddrs() ([]netip.Addr, error) {
	ias, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(ias))
	for _, ia := range ias {
		if pn, ok := ia.(*net.IPNet); ok {
			if a, ok := netip.AddrFromSlice(pn.IP); ok {
				out = append(out, a.Unmap())
			}
		}
	}
	return out, nil
}

// LocalAddrs is localAddrs for the command line.
func LocalAddrs() ([]netip.Addr, error) { return localAddrs() }

// ContainerView is GET /v1/addresses' "container": this runs inside a
// container, so the addresses Docker gave it are left out and the page
// asks for the host's address instead (D68).
type ContainerView struct {
	Kind string `json:"kind"`
	// HostAddressNeeded: no address found here is one other computers can
	// use; the person types the host's (LAN or Tailscale) address.
	HostAddressNeeded bool `json:"host_address_needed"`
	// Hidden is how many container-network addresses were left out.
	Hidden int `json:"hidden"`
}

// containerReason is the skipped reason of a container-network address.
const containerReason = "the container's own network: other computers cannot reach it"

// containerAddresses leaves the container's own network addresses out of
// what the setup page offers (they are listed under skipped), the same
// rule `up` and `join` use (D51).
func containerAddresses(v *AddressesView, kind string) {
	cv := &ContainerView{Kind: kind}
	kept := v.Addresses[:0:0]
	for _, a := range v.Addresses {
		if a.Addr.Is4() && netdetect.ContainerPrivate(a.Addr) {
			cv.Hidden++
			v.Skipped = append(v.Skipped, netdetect.Skipped{Interface: a.Interface, Addr: a.Addr.String(), Reason: containerReason})
			continue
		}
		kept = append(kept, a)
	}
	v.Addresses = kept
	if v.Default != nil {
		if d, err := netip.ParseAddr(*v.Default); err == nil && d.Is4() && netdetect.ContainerPrivate(d) {
			v.Default = nil
		}
	}
	if v.Default == nil {
		for _, a := range kept {
			if a.Suggested {
				s := a.Addr.String()
				v.Default = &s
				break
			}
		}
	}
	cv.HostAddressNeeded = !anySuggested(kept)
	v.Container = cv
}

func anySuggested(as []netdetect.Address) bool {
	for _, a := range as {
		if a.Suggested {
			return true
		}
	}
	return false
}

// containerAdvertiseProblem: inside a container, the setup page refuses an
// address only this container has (loopback, or its own container-network
// address): other computers could never reach it. A host name, or another
// machine's address, is the person's choice. "" when it is fine.
func containerAdvertiseProblem(hostport string, own []netip.Addr) string {
	h := hostport
	if hh, _, err := net.SplitHostPort(hostport); err == nil {
		h = hh
	}
	a, err := netip.ParseAddr(strings.Trim(h, "[]"))
	if err != nil {
		return "" // a host name
	}
	a = a.Unmap()
	const tail = "Type the address of the computer that runs Docker: its address on your network (like 192.168.1.10) or its Tailscale address (100.x.y.z)."
	if a.IsLoopback() || a.IsUnspecified() {
		return fmt.Sprintf("%s only reaches this container itself, so other computers cannot use it. %s", a, tail)
	}
	for _, o := range own {
		if o.Unmap() == a && netdetect.ContainerPrivate(a) {
			return fmt.Sprintf("%s is this container's address on Docker's private network: other computers cannot reach it. %s", a, tail)
		}
	}
	return ""
}

// inContainerBridge: running in a container with its own network, where
// the control port is reached from the host only through a published port.
func (a *Agent) inContainerBridge() (kind string, ok bool) {
	kind, ok = netdetect.Container()
	return kind, ok && a.cfg.DashboardListen != ""
}

// DockerJoin is what a node that runs in a container tells people to run
// on the next computer (D68): the same image, with the setup page, or the
// command line with the code in COYOTE_INVITE.
type DockerJoin struct {
	// SetupCommand starts the next computer's container with its setup
	// page; the code is pasted there.
	SetupCommand string `json:"setup_command"`
	// LinkCommand prints that page's sign-in link.
	LinkCommand string `json:"link_command"`
	// CLICommand joins without the page: the code in COYOTE_INVITE, the
	// name and the address filled in by the person (NAME, ADDRESS).
	CLICommand string `json:"cli_command"`
}

// dockerJoin builds DockerJoin for a data node or a witness. code may be
// "" (then a placeholder stands in its place).
func dockerJoin(role, code string) DockerJoin {
	witness := role == "witness"
	ports := "-p 127.0.0.1:7480:7480 -p 7481:7481 -p 5432:5432"
	shm := " --shm-size 256m"
	if witness {
		ports, shm = "-p 127.0.0.1:7480:7480 -p 7481:7481", ""
	}
	run := fmt.Sprintf("docker run -d --name %s --restart unless-stopped --stop-timeout 120%s %s -v %s-data:/data",
		brand.Command, shm, ports, brand.Command)
	if code == "" {
		code = "PASTE-THE-CODE"
	}
	cmd, name := "join", "NAME"
	if witness {
		cmd, name = "witness", "witness"
	}
	return DockerJoin{
		SetupCommand: run + " " + brand.DockerImage + " setup",
		LinkCommand:  fmt.Sprintf("docker logs %s", brand.Command),
		CLICommand:   fmt.Sprintf("%s -e %sINVITE=%s %s %s --name %s --advertise ADDRESS:7481", run, brand.EnvPrefix, code, brand.DockerImage, cmd, name),
	}
}

// DockerJoinFor is dockerJoin for the command line.
func DockerJoinFor(role, code string) DockerJoin { return dockerJoin(role, code) }

// containerKind is netdetect.Container's kind, "" outside a container.
func containerKind() string {
	kind, ok := netdetect.Container()
	if !ok {
		return ""
	}
	return kind
}
