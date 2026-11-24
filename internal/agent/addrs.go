package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/netdetect"
)

// maxAdvertise is how many detected addresses a node announces.
const maxAdvertise = 4

// errNoAddress means the machine has no address another node could dial.
var errNoAddress = errors.New(netdetect.WarnNone + " Or pass --advertise HOST:PORT.")

// advertiseFor returns what other nodes should dial for a listener on
// listen, and the detection report behind it. A specific listen host is
// used as is (nil, nil: the cluster layer advertises the listener itself);
// an unspecified one ("0.0.0.0:7481", ":7481") becomes this machine's
// suggested addresses (internal/netdetect: Tailscale first, then the local
// network; public, carrier-NAT, VM and Docker addresses are never chosen).
func advertiseFor(ctx context.Context, listen string) ([]string, *netdetect.Report, error) {
	return detectAdvertise(ctx, listen, netdetect.Options{})
}

// testAddrEnv (tests only) names a file whose comma-separated HOST:PORT
// list stands in for this machine's detected addresses, read again at
// every detection, so a test can give a running node "a new address".
const testAddrEnv = "COYOTE_TEST_ADDR_FILE"

func detectAdvertise(ctx context.Context, listen string, opt netdetect.Options) ([]string, *netdetect.Report, error) {
	if f := os.Getenv(testAddrEnv); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, err
		}
		var out []string
		for _, a := range strings.Split(strings.TrimSpace(string(b)), ",") {
			if a = strings.TrimSpace(a); a != "" {
				out = append(out, a)
			}
		}
		if len(out) == 0 {
			return nil, nil, errNoAddress
		}
		return out, nil, nil
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, nil, err
	}
	if host != "" {
		if ip, err := netip.ParseAddr(host); err != nil || !ip.IsUnspecified() {
			return nil, nil, nil
		}
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return nil, nil, err
	}
	rep, err := netdetect.Detect(ctx, opt)
	if err != nil {
		return nil, nil, err
	}
	out := rep.HostPorts(p, maxAdvertise)
	if len(out) == 0 {
		return nil, &rep, errNoAddress
	}
	return out, &rep, nil
}

// describeAddresses is what `up` / `join` print about how other machines
// will reach this one: the chosen address's label and every warning.
func describeAddresses(adv []string, rep *netdetect.Report) []string {
	if rep == nil {
		if len(adv) == 0 {
			return nil
		}
		return []string{"Other machines will reach this one at " + strings.Join(adv, ", ") + "."}
	}
	var lines []string
	sug := rep.Suggested()
	if len(sug) > 0 {
		l := "Other machines will reach this one at " + sug[0].Label
		if len(sug) > 1 {
			var rest []string
			for _, a := range sug[1:min(len(sug), maxAdvertise)] {
				rest = append(rest, a.Label)
			}
			l += "; also " + strings.Join(rest, ", ")
		}
		lines = append(lines, l+".")
	}
	seen := map[string]bool{}
	for _, a := range sug[:min(len(sug), maxAdvertise)] {
		for _, w := range a.Warnings {
			if !seen[w] {
				seen[w] = true
				lines = append(lines, "Note: "+w)
			}
		}
	}
	for _, w := range rep.Warnings {
		if !seen[w] {
			seen[w] = true
			lines = append(lines, "Note: "+w)
		}
	}
	return lines
}

// validHostPort checks a --advertise entry.
func validHostPort(s string) error {
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		return err
	}
	if h == "" {
		return errors.New("host is empty")
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 || n > 65535 {
		return errors.New("bad port")
	}
	return nil
}

// containerAddressCheck refuses, inside a container, to announce an address
// of the container's own private network that nobody gave explicitly
// (D51): other computers could not reach it, and the cluster would look
// broken only later. adv is what detection picked (nil: the listener's own
// host); an address given with --advertise, COYOTE_ADVERTISE or saved
// earlier never comes here.
func containerAddressCheck(listen string, adv []string, inContainer func() (string, bool)) error {
	kind, ok := inContainer()
	if !ok {
		return nil
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return nil
	}
	hosts := adv
	if len(hosts) == 0 {
		hosts = []string{listen}
	}
	var private []string
	for _, hp := range hosts {
		h, _, err := net.SplitHostPort(hp)
		if err != nil {
			continue
		}
		ip, err := netip.ParseAddr(h)
		if err != nil || !netdetect.ContainerPrivate(ip) {
			return nil // at least one address other computers may reach
		}
		private = append(private, h)
	}
	if len(private) == 0 {
		return nil
	}
	return &ExitError{Code: 2, Msg: fmt.Sprintf(
		"This is running inside a container (%s), and the address found here, %s, belongs to the container's private network: other computers cannot reach it. "+
			"Give the address other computers should use with --advertise HOST:%s (or set %sADVERTISE=HOST:%s), and publish that port from the container (for example docker run -p %s:%s ...).",
		kind, strings.Join(private, ", "), port, brand.EnvPrefix, port, port, port)}
}
