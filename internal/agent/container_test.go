package agent

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"coyotedb.com/coyote/internal/netdetect"
)

func addrs(ss ...string) func() ([]netip.Addr, error) {
	return func() ([]netip.Addr, error) {
		var out []netip.Addr
		for _, s := range ss {
			out = append(out, netip.MustParseAddr(s))
		}
		return out, nil
	}
}

func TestContainerDashboardListen(t *testing.T) {
	docker := func() (string, bool) { return "docker", true }
	none := func() (string, bool) { return "", false }
	bridge := addrs("127.0.0.1", "::1", "172.17.0.3", "fe80::42:acff:fe11:3", "fd00:dead::3")
	cases := []struct {
		name       string
		env        string
		ctlPort    int
		in         func() (string, bool)
		addrs      func() ([]netip.Addr, error)
		want       string
		noteHas    string
		wantNoNote bool
	}{
		{name: "unset", env: "", in: docker, addrs: bridge, want: "", wantNoNote: true},
		{name: "bridge container", env: "0.0.0.0:7480", in: docker, addrs: bridge, want: "0.0.0.0:7480", wantNoNote: true},
		{name: "control port wins", env: "0.0.0.0:7480", ctlPort: 17480, in: docker, addrs: bridge, want: "0.0.0.0:17480", wantNoNote: true},
		{name: "podman", env: "0.0.0.0:7480", in: docker, addrs: addrs("10.88.0.5"), want: "0.0.0.0:7480", wantNoNote: true},
		{name: "not a container", env: "0.0.0.0:7480", in: none, addrs: bridge, want: "", noteHas: "only used inside a container"},
		{name: "host network", env: "0.0.0.0:7480", in: docker, addrs: addrs("127.0.0.1", "172.17.0.1", "192.168.1.10"), want: "", noteHas: "192.168.1.10"},
		{name: "host network tailscale", env: "0.0.0.0:7480", in: docker, addrs: addrs("100.64.0.7"), want: "", noteHas: "100.64.0.7"},
		{name: "host network global v6", env: "0.0.0.0:7480", in: docker, addrs: addrs("172.17.0.2", "2001:db8::5"), want: "", noteHas: "2001:db8::5"},
		{name: "bad value", env: "7480", in: docker, addrs: bridge, want: "", noteHas: "not HOST:PORT"},
		{name: "addrs fail", env: "0.0.0.0:7480", in: docker, addrs: func() ([]netip.Addr, error) { return nil, errors.New("boom") }, want: "", noteHas: "boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, note := ContainerDashboardListen(c.env, c.ctlPort, c.in, c.addrs)
			if got != c.want {
				t.Errorf("listen = %q, want %q", got, c.want)
			}
			if c.wantNoNote && note != "" {
				t.Errorf("unexpected note %q", note)
			}
			if c.noteHas != "" && !strings.Contains(note, c.noteHas) {
				t.Errorf("note %q does not say %q", note, c.noteHas)
			}
		})
	}
}

func TestContainerAddresses(t *testing.T) {
	mk := func(s string, sug bool) netdetect.Address {
		return netdetect.Address{Addr: netip.MustParseAddr(s), Interface: "eth0", Class: "lan", Suggested: sug}
	}
	def := "172.17.0.3"
	v := AddressesView{Report: netdetect.Report{Addresses: []netdetect.Address{mk("172.17.0.3", true)}}, Default: &def}
	containerAddresses(&v, "docker")
	if len(v.Addresses) != 0 || v.Default != nil {
		t.Fatalf("the container's own address is still offered: %+v default=%v", v.Addresses, v.Default)
	}
	if v.Container == nil || !v.Container.HostAddressNeeded || v.Container.Hidden != 1 || v.Container.Kind != "docker" {
		t.Fatalf("container = %+v", v.Container)
	}
	if len(v.Skipped) != 1 || v.Skipped[0].Addr != "172.17.0.3" || v.Skipped[0].Reason != containerReason {
		t.Fatalf("skipped = %+v", v.Skipped)
	}

	// --network host: the host's own LAN and Tailscale addresses stay,
	// docker0's bridge address goes; the default moves to a kept one.
	def = "172.17.0.1"
	v = AddressesView{Report: netdetect.Report{Addresses: []netdetect.Address{mk("172.17.0.1", true), mk("100.64.0.7", true), mk("192.168.1.10", false)}}, Default: &def}
	containerAddresses(&v, "docker")
	if len(v.Addresses) != 2 || v.Default == nil || *v.Default != "100.64.0.7" {
		t.Fatalf("addresses %+v default %v", v.Addresses, v.Default)
	}
	if v.Container.HostAddressNeeded || v.Container.Hidden != 1 {
		t.Fatalf("container = %+v", v.Container)
	}
}

func TestContainerAdvertiseProblem(t *testing.T) {
	own := []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("172.17.0.3")}
	for _, c := range []struct {
		hp     string
		refuse bool
	}{
		{"172.17.0.3:7481", true},
		{"127.0.0.1:7481", true},
		{"[::1]:7481", true},
		{"0.0.0.0:7481", true},
		{"172.17.0.9:7481", false}, // another container: the person's choice
		{"192.168.1.10:7481", false},
		{"100.64.0.7:7481", false},
		{"host.docker.internal:7481", false},
		{"node1:7481", false},
	} {
		msg := containerAdvertiseProblem(c.hp, own)
		if (msg != "") != c.refuse {
			t.Errorf("%s: refused=%v (%q), want %v", c.hp, msg != "", msg, c.refuse)
		}
		if c.refuse && !strings.Contains(msg, "computer that runs Docker") {
			t.Errorf("%s: message does not say what to type: %q", c.hp, msg)
		}
	}
}

func TestDockerJoin(t *testing.T) {
	d := dockerJoin("data", "coyote-ABC")
	for _, want := range []string{"-p 127.0.0.1:7480:7480", "-p 7481:7481", "-p 5432:5432", "--shm-size 256m", "coyotedb/coyote:edge setup"} {
		if !strings.Contains(d.SetupCommand, want) {
			t.Errorf("setup command %q lacks %q", d.SetupCommand, want)
		}
	}
	if strings.Contains(d.SetupCommand, "INVITE") || strings.Contains(d.SetupCommand, "-p 7480:7480") {
		t.Errorf("setup command: %q", d.SetupCommand)
	}
	if !strings.Contains(d.CLICommand, "-e COYOTE_INVITE=coyote-ABC coyotedb/coyote:edge join --name NAME --advertise ADDRESS:7481") {
		t.Errorf("cli command: %q", d.CLICommand)
	}
	w := dockerJoin("witness", "")
	if strings.Contains(w.SetupCommand, "5432") || !strings.Contains(w.CLICommand, "COYOTE_INVITE=PASTE-THE-CODE coyotedb/coyote:edge witness --name witness") {
		t.Errorf("witness: %+v", w)
	}
	if d.LinkCommand != "docker logs coyote" {
		t.Errorf("link: %q", d.LinkCommand)
	}
}
