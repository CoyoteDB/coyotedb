package netdetect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

func up(name string, addrs ...string) Interface {
	ifc := Interface{Name: name, Up: true}
	for _, a := range addrs {
		ifc.Addrs = append(ifc.Addrs, IfAddr{Prefix: netip.MustParsePrefix(a)})
	}
	return ifc
}

func with(ifc Interface, f func(*Interface)) Interface { f(&ifc); return ifc }

func labels(r Report, suggestedOnly bool) []string {
	var out []string
	for _, a := range r.Addresses {
		if !suggestedOnly || a.Suggested {
			out = append(out, a.Label)
		}
	}
	return out
}

func skippedReason(r Report, ifName string) string {
	for _, s := range r.Skipped {
		if s.Interface == ifName {
			return s.Reason
		}
	}
	return ""
}

func TestClassify(t *testing.T) {
	type tc struct {
		name      string
		goos      string
		ifs       []Interface
		opt       Options
		suggested []string          // labels, in order
		listed    []string          // all labels, in order (nil = don't check)
		skipped   map[string]string // interface -> reason
		warnings  []string
		def       string // default addr ("" = none)
	}
	tests := []tc{
		{
			name: "linux server: tailscale first, docker skipped",
			goos: "linux",
			ifs: []Interface{
				with(up("lo", "127.0.0.1/8", "::1/128"), func(i *Interface) { i.Loopback = true }),
				up("enp3s0", "192.168.1.20/24", "fe80::1/64"),
				up("docker0", "172.17.0.1/16"),
				up("br-1a2b3c", "172.18.0.1/16"),
				up("veth12ab", "fe80::2/64"),
				up("tailscale0", "100.64.0.43/32", "fd7a:115c:a1e0::1234/128"),
				up("virbr0", "192.168.122.1/24"),
			},
			suggested: []string{"Tailscale (100.64.0.43)", "Tailscale (fd7a:115c:a1e0::1234)", "Home/office network (192.168.1.20)"},
			skipped:   map[string]string{"docker0": ReasonVirtual, "br-1a2b3c": ReasonVirtual, "virbr0": ReasonVirtual, "lo": ReasonLoopback, "enp3s0": ReasonLinkLocal},
			def:       "100.64.0.43",
		},
		{
			name: "linux wifi only, dhcp",
			goos: "linux",
			ifs: []Interface{
				with(up("wlp2s0"), func(i *Interface) {
					i.Wireless = true
					i.Addrs = []IfAddr{{Prefix: netip.MustParsePrefix("10.0.0.7/24"), Dynamic: true}}
				}),
			},
			suggested: []string{"Home/office Wi-Fi (10.0.0.7)"},
			warnings:  []string{WarnOnlyWiFi},
			def:       "10.0.0.7",
		},
		{
			name: "linux wired beats wifi; vpn after lan",
			goos: "linux",
			ifs: []Interface{
				with(up("wlan0", "192.168.1.30/24"), func(i *Interface) { i.Wireless = true }),
				up("wg0", "10.8.0.2/24"),
				up("eth0", "192.168.1.31/24"),
				up("zt7nnig26", "10.147.17.5/24"),
			},
			suggested: []string{"Home/office network (192.168.1.31)", "Home/office Wi-Fi (192.168.1.30)", "WireGuard (10.8.0.2)", "ZeroTier (10.147.17.5)"},
			def:       "192.168.1.31",
		},
		{
			name: "public address flagged, never suggested",
			goos: "linux",
			ifs: []Interface{
				up("eth0", "203.0.113.9/24", "8.8.4.4/24", "2001:4860::5/64"),
			},
			suggested: nil,
			listed:    []string{"Public internet address (8.8.4.4)", "Public internet address (2001:4860::5)", "Reserved-range address (203.0.113.9)"},
			warnings:  []string{WarnNone},
		},
		{
			name:     "cgnat on ethernet is not tailscale",
			goos:     "linux",
			ifs:      []Interface{up("eth0", "100.64.0.72/10")},
			listed:   []string{"Shared provider address (100.64.0.72)"},
			warnings: []string{WarnNone},
		},
		{
			name: "down interface and temporary v6 skipped",
			goos: "linux",
			ifs: []Interface{
				with(up("eth1", "192.168.5.5/24"), func(i *Interface) { i.Up = false }),
				with(up("eth0"), func(i *Interface) {
					i.Addrs = []IfAddr{
						{Prefix: netip.MustParsePrefix("fd00::aaaa/64"), Temporary: true},
						{Prefix: netip.MustParsePrefix("fd00::1/64")},
					}
				}),
			},
			suggested: []string{"Home/office network (fd00::1)"},
			skipped:   map[string]string{"eth1": ReasonDown, "eth0": ReasonTemporary},
			def:       "fd00::1",
		},
		{
			name: "macos laptop: utun tailscale, awdl and vm bridge skipped",
			goos: "darwin",
			ifs: []Interface{
				with(up("lo0", "127.0.0.1/8"), func(i *Interface) { i.Loopback = true }),
				with(up("en0", "192.168.1.44/24"), func(i *Interface) { i.Wireless = true; i.Description = "Wi-Fi" }),
				up("awdl0", "fe80::3/64"),
				up("llw0", "fe80::4/64"),
				up("utun0", "fe80::5/64"),
				up("utun4", "100.64.0.88/32"),
				up("bridge100", "192.168.64.1/24"),
				up("bridge0"),
				up("anpi0"),
			},
			suggested: []string{"Tailscale (100.64.0.88)", "Home/office Wi-Fi (192.168.1.44)"},
			skipped:   map[string]string{"bridge100": ReasonVirtual, "awdl0": ReasonLinkLocal, "bridge0": ReasonNoAddress},
			def:       "100.64.0.88",
		},
		{
			name: "macos: non-tailscale utun is a vpn",
			goos: "darwin",
			ifs: []Interface{
				up("en0", "10.1.1.4/24"),
				up("utun3", "10.200.0.4/32"),
			},
			suggested: []string{"Home/office network (10.1.1.4)", `VPN "utun3" (10.200.0.4)`},
			def:       "10.1.1.4",
		},
		{
			name: "windows: WSL and Default Switch skipped, external switch is LAN",
			goos: "windows",
			ifs: []Interface{
				with(up("Wi-Fi", "192.168.0.12/24"), func(i *Interface) {
					i.Wireless = true
					i.Description = "Intel(R) Wi-Fi 6 AX201"
					i.Addrs[0].Dynamic = true
				}),
				with(up("vEthernet (WSL (Hyper-V firewall))", "172.29.80.1/20"), func(i *Interface) { i.Description = "Hyper-V Virtual Ethernet Adapter" }),
				with(up("vEthernet (Default Switch)", "172.20.0.1/20"), func(i *Interface) { i.Description = "Hyper-V Virtual Ethernet Adapter #2" }),
				with(up("vEthernet (External)", "192.168.0.13/24"), func(i *Interface) { i.Description = "Hyper-V Virtual Ethernet Adapter #3" }),
				with(up("Tailscale", "100.64.0.43/32"), func(i *Interface) { i.Description = "Tailscale Tunnel"; i.PointToPoint = true }),
				with(up("Loopback Pseudo-Interface 1", "127.0.0.1/8"), func(i *Interface) { i.Loopback = true }),
				with(up("Ethernet 3", "192.168.56.1/24"), func(i *Interface) { i.Description = "VirtualBox Host-Only Ethernet Adapter" }),
				with(up("Local Area Connection* 10", "2001:0:2851::1/64"), func(i *Interface) { i.Description = "Teredo Tunneling Pseudo-Interface" }),
				with(up("OpenVPN Connect", "10.9.0.6/24"), func(i *Interface) { i.Description = "TAP-Windows Adapter V9" }),
			},
			suggested: []string{"Tailscale (100.64.0.43)", "Home/office network (192.168.0.13)", "Home/office Wi-Fi (192.168.0.12)", `VPN "OpenVPN Connect" (10.9.0.6)`},
			skipped: map[string]string{
				"vEthernet (WSL (Hyper-V firewall))": ReasonVirtual,
				"vEthernet (Default Switch)":         ReasonVirtual,
				"Ethernet 3":                         ReasonVirtual,
				"Local Area Connection* 10":          ReasonTransition,
			},
			def: "100.64.0.43",
		},
		{
			name:      "loopback only with option (tests)",
			goos:      "linux",
			ifs:       []Interface{with(up("lo", "127.0.0.1/8"), func(i *Interface) { i.Loopback = true })},
			opt:       Options{IncludeLoopback: true},
			suggested: []string{"This computer only (127.0.0.1)"},
			warnings:  []string{WarnNone},
			def:       "127.0.0.1",
		},
		{
			name:     "virtual listed with option, never suggested",
			goos:     "linux",
			ifs:      []Interface{up("docker0", "172.17.0.1/16")},
			opt:      Options{IncludeVirtual: true},
			listed:   []string{"VM/Docker network (172.17.0.1)"},
			warnings: []string{WarnNone},
		},
		{
			name:      "duplicates collapse",
			goos:      "linux",
			ifs:       []Interface{up("eth0", "192.168.1.2/24"), up("eth0:1", "192.168.1.2/24")},
			suggested: []string{"Home/office network (192.168.1.2)"},
			def:       "192.168.1.2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Classify(tt.goos, tt.ifs, tt.opt)
			if got := labels(r, true); !slices.Equal(got, tt.suggested) {
				t.Errorf("suggested:\n got  %q\n want %q", got, tt.suggested)
			}
			if tt.listed != nil {
				if got := labels(r, false); !slices.Equal(got, tt.listed) {
					t.Errorf("listed:\n got  %q\n want %q", got, tt.listed)
				}
			}
			for ifn, want := range tt.skipped {
				if got := skippedReason(r, ifn); got != want {
					t.Errorf("skipped[%s] = %q, want %q", ifn, got, want)
				}
			}
			if !slices.Equal(r.Warnings, tt.warnings) {
				t.Errorf("warnings = %q, want %q", r.Warnings, tt.warnings)
			}
			d, ok := r.Default()
			switch {
			case tt.def == "" && ok:
				t.Errorf("default = %v, want none", d.Addr)
			case tt.def != "" && (!ok || d.Addr.String() != tt.def):
				t.Errorf("default = %v (%v), want %s", d.Addr, ok, tt.def)
			}
		})
	}
}

func TestAddressWarnings(t *testing.T) {
	r := Classify("linux", []Interface{
		with(up("eth0"), func(i *Interface) {
			i.Addrs = []IfAddr{{Prefix: netip.MustParsePrefix("192.168.1.5/24"), Dynamic: true}, {Prefix: netip.MustParsePrefix("8.8.8.8/24")}}
		}),
	}, Options{})
	for _, a := range r.Addresses {
		switch a.Addr.String() {
		case "192.168.1.5":
			if !a.MayChange || !slices.Contains(a.Warnings, WarnMayChange) {
				t.Errorf("dhcp address: %+v", a)
			}
		case "8.8.8.8":
			if a.Suggested || !slices.Contains(a.Warnings, WarnPublic) {
				t.Errorf("public address: %+v", a)
			}
		}
	}
}

func TestHostPorts(t *testing.T) {
	r := Classify("linux", []Interface{up("tailscale0", "100.64.0.9/32", "fd7a:115c:a1e0::9/128"), up("eth0", "10.0.0.2/8")}, Options{})
	got := r.HostPorts(7481, 2)
	want := []string{"100.64.0.9:7481", "[fd7a:115c:a1e0::9]:7481"}
	if !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	if n := len(r.HostPorts(7481, 0)); n != 3 {
		t.Fatalf("all = %d", n)
	}
}

func TestDetectRuns(t *testing.T) {
	r, err := Detect(context.Background(), Options{IncludeLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range r.Addresses {
		if a.Class == ClassLoopback {
			found = true
		}
		if a.Label == "" || !strings.Contains(a.Label, a.Addr.String()) {
			t.Errorf("bad label %q for %v", a.Label, a.Addr)
		}
	}
	if !found {
		t.Fatalf("no loopback address on this machine? %+v", r)
	}
	t.Logf("default: %v; %d addresses, %d skipped", firstLabel(r), len(r.Addresses), len(r.Skipped))
}

func firstLabel(r Report) string {
	if d, ok := r.Default(); ok {
		return d.Label
	}
	return "(none)"
}

func TestProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := closed.Addr().String()
	closed.Close()
	good := ln.Addr().String()

	// A blackholed address: a dial that never answers.
	hang := "192.0.2.1:7481"
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		if addr == hang {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	t0 := time.Now()
	rs := Probe(context.Background(), []string{hang, dead, good, good}, 200*time.Millisecond, dial)
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("probes not parallel/bounded: %v", d)
	}
	if len(rs) != 4 || rs[0].OK || rs[1].OK || !rs[2].OK || !rs[3].OK {
		t.Fatalf("%+v", rs)
	}
	if rs[0].Err == nil || !strings.Contains(rs[0].Err.Error(), "no answer") {
		t.Fatalf("timeout error = %v", rs[0].Err)
	}
	if f, ok := Fastest(rs); !ok || f.Addr != good {
		t.Fatalf("fastest = %+v %v", f, ok)
	}
	if got := Reachable(rs); !slices.Equal(got, []string{good, good}) {
		t.Fatalf("reachable = %q", got)
	}
	if _, ok := Fastest([]ProbeResult{{Addr: "x", Err: errors.New("no")}}); ok {
		t.Fatal("fastest of none")
	}
}
