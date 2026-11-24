// Package netdetect finds the addresses other nodes should dial to reach
// this machine, and labels them in words a non-technical person follows.
//
// The classification is pure (Classify) and table-tested with synthetic
// interface lists for Windows, macOS and Linux naming. Detect adds a thin
// OS layer on top of net.Interfaces (plus, where the OS tells us cheaply:
// Wi-Fi or not, and whether an address was leased by DHCP).
package netdetect

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// Class is what kind of network an address belongs to.
type Class string

// Classes.
const (
	ClassTailscale Class = "tailscale"  // 100.64.0.0/10 or fd7a:115c:a1e0::/48 on a Tailscale interface
	ClassVPN       Class = "vpn"        // WireGuard, ZeroTier, OpenVPN, other tunnels
	ClassLAN       Class = "lan"        // RFC 1918 or IPv6 ULA on a normal interface
	ClassLinkLocal Class = "link-local" // 169.254/16, fe80::/10: excluded by default
	ClassPublic    Class = "public"     // globally routable: flagged, never suggested
	ClassShared    Class = "shared"     // 100.64.0.0/10 not from Tailscale: carrier NAT
	ClassOther     Class = "other"      // reserved/benchmark ranges and the like
	ClassLoopback  Class = "loopback"   // tests only
)

// Interface is one network interface as the OS layer sees it.
type Interface struct {
	Name         string // OS name; on Windows the friendly name ("Wi-Fi", "vEthernet (WSL)")
	Description  string // Windows adapter description, macOS hardware port; may be ""
	Up           bool
	Loopback     bool
	PointToPoint bool // tunnel-like (IFF_POINTOPOINT, Windows tunnel type)
	Wireless     bool // the OS says it is Wi-Fi
	Addrs        []IfAddr
}

// IfAddr is one address on an interface.
type IfAddr struct {
	Prefix netip.Prefix
	// Dynamic: the OS says the address is leased (DHCP, or a finite
	// lifetime on Linux), so it may change. False also means "unknown".
	Dynamic bool
	// Temporary: an IPv6 privacy address that rotates.
	Temporary bool
}

// Address is one usable address with its classification.
type Address struct {
	Addr      netip.Addr `json:"addr"`
	Interface string     `json:"interface"`
	Class     Class      `json:"class"`
	Label     string     `json:"label"` // "Tailscale (100.64.0.43)"
	Suggested bool       `json:"suggested"`
	Wireless  bool       `json:"wireless,omitempty"`
	MayChange bool       `json:"may_change,omitempty"`
	Warnings  []string   `json:"warnings,omitempty"`
	rank      int
}

// Skipped is an address or interface left out, and why.
type Skipped struct {
	Interface string `json:"interface"`
	Addr      string `json:"addr,omitempty"`
	Reason    string `json:"reason"`
}

// Report is the result of a detection.
type Report struct {
	// Addresses are usable ones, best first; suggested ones come first.
	Addresses []Address `json:"addresses"`
	Skipped   []Skipped `json:"skipped,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
}

// Options widen what is reported.
type Options struct {
	IncludeLoopback  bool // list (and suggest, last) loopback addresses: tests only
	IncludeLinkLocal bool // list link-local addresses (never suggested)
	IncludeVirtual   bool // list VM/Docker network addresses (never suggested)
}

// Default returns the default pick (the first suggested address).
func (r Report) Default() (Address, bool) {
	for _, a := range r.Addresses {
		if a.Suggested {
			return a, true
		}
	}
	return Address{}, false
}

// Suggested returns the suggested addresses, best first.
func (r Report) Suggested() []Address {
	var out []Address
	for _, a := range r.Addresses {
		if a.Suggested {
			out = append(out, a)
		}
	}
	return out
}

// HostPorts returns up to max suggested addresses as host:port (max <= 0
// means all), for --advertise.
func (r Report) HostPorts(port, max int) []string {
	var out []string
	for _, a := range r.Suggested() {
		if max > 0 && len(out) == max {
			break
		}
		out = append(out, net.JoinHostPort(a.Addr.String(), strconv.Itoa(port)))
	}
	return out
}

// Detect inspects this machine's interfaces.
func Detect(ctx context.Context, opt Options) (Report, error) {
	ifs, err := interfaces(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list network interfaces: %w", err)
	}
	return Classify(runtime.GOOS, ifs, opt), nil
}

var (
	tailscaleV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
	ula         = netip.MustParsePrefix("fc00::/7")
	rfc1918     = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
	}
	otherV4 = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"), // benchmark; also "fake IP" VPN apps
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
	otherV6 = []netip.Prefix{
		netip.MustParsePrefix("2001:db8::/32"),
		netip.MustParsePrefix("64:ff9b::/96"),
	}
)

// Messages (shown to people).
const (
	ReasonDown        = "interface is down"
	ReasonVirtual     = "address is from a VM/Docker network on this machine; other machines cannot reach it"
	ReasonSystem      = "system interface (not a real network)"
	ReasonLinkLocal   = "link-local address: works only on the same cable or Wi-Fi and is not stable"
	ReasonLoopback    = "loopback: only this machine can use it"
	ReasonTemporary   = "temporary IPv6 privacy address: it changes by itself"
	ReasonNoAddress   = "no address"
	ReasonTransition  = "IPv6 transition tunnel (Teredo/6to4/ISATAP)"
	ReasonUnspecified = "not a unicast address"

	WarnPublic    = "This address looks public: anyone on the internet could try to reach it. Use Tailscale or your home/office network instead."
	WarnShared    = "This is a shared provider address (carrier NAT); other machines usually cannot reach it."
	WarnVirtual   = "This address is from a VM/Docker network on this machine; other machines cannot reach it."
	WarnMayChange = "This address was handed out automatically and may change. Reserve it in your router, or use Tailscale."
	WarnOnlyWiFi  = "Only a Wi-Fi address was found. It works, but a network cable is more reliable for a database."
	WarnNone      = "No address was found that other machines can reach. Connect this machine to your network, or install Tailscale."
	WarnLinkLocal = "Link-local addresses work only on the same cable or Wi-Fi and are not stable."
	WarnOther     = "This address is from a reserved range; other machines usually cannot reach it."
	WarnLoopback  = "Loopback: only this machine can use it."
)

// Classify turns an interface list into a ranked report. goos selects the
// naming rules ("windows", "darwin", anything else is Linux-like).
func Classify(goos string, ifs []Interface, opt Options) Report {
	var r Report
	for _, ifc := range ifs {
		kind, why := interfaceKind(goos, ifc)
		if len(ifc.Addrs) == 0 {
			if kind != kindSystem && kind != kindVirtual && ifc.Up && !ifc.Loopback {
				r.Skipped = append(r.Skipped, Skipped{Interface: ifc.Name, Reason: ReasonNoAddress})
			}
			continue
		}
		if !ifc.Up {
			r.Skipped = append(r.Skipped, Skipped{Interface: ifc.Name, Reason: ReasonDown})
			continue
		}
		for _, ia := range ifc.Addrs {
			a := ia.Prefix.Addr().Unmap()
			skip := func(reason string) {
				r.Skipped = append(r.Skipped, Skipped{Interface: ifc.Name, Addr: a.String(), Reason: reason})
			}
			if !a.IsValid() || a.IsUnspecified() || a.IsMulticast() {
				skip(ReasonUnspecified)
				continue
			}
			if a.IsLoopback() || ifc.Loopback {
				if opt.IncludeLoopback {
					r.Addresses = append(r.Addresses, mk(ifc, ia, a, ClassLoopback))
				} else {
					skip(ReasonLoopback)
				}
				continue
			}
			if a.IsLinkLocalUnicast() {
				if opt.IncludeLinkLocal {
					r.Addresses = append(r.Addresses, mk(ifc, ia, a, ClassLinkLocal))
				} else {
					skip(ReasonLinkLocal)
				}
				continue
			}
			switch kind {
			case kindSystem:
				skip(why)
				continue
			case kindTransition:
				skip(ReasonTransition)
				continue
			case kindVirtual:
				if opt.IncludeVirtual {
					ad := mk(ifc, ia, a, classOfAddr(a))
					ad.Suggested = false
					ad.Warnings = append(ad.Warnings, WarnVirtual)
					ad.Label = "VM/Docker network (" + a.String() + ")"
					ad.rank = 100
					r.Addresses = append(r.Addresses, ad)
				} else {
					skip(why)
				}
				continue
			}
			if ia.Temporary {
				skip(ReasonTemporary)
				continue
			}
			var c Class
			switch {
			case (kind == kindTailscale || darwinUtun(goos, ifc)) && (tailscaleV4.Contains(a) || tailscaleV6.Contains(a)):
				c = ClassTailscale
			case kind == kindTailscale || kind == kindVPN:
				c = ClassVPN
			default:
				c = classOfAddr(a)
			}
			r.Addresses = append(r.Addresses, mk(ifc, ia, a, c))
		}
	}
	r.Addresses = dedupe(r.Addresses)
	slices.SortStableFunc(r.Addresses, func(x, y Address) int { return cmp.Compare(x.rank, y.rank) })
	r.Warnings = reportWarnings(r.Addresses)
	return r
}

func classOfAddr(a netip.Addr) Class {
	switch {
	case tailscaleV4.Contains(a):
		return ClassShared
	case a.Is4() && slices.ContainsFunc(rfc1918, func(p netip.Prefix) bool { return p.Contains(a) }):
		return ClassLAN
	case a.Is6() && ula.Contains(a):
		return ClassLAN
	case a.Is4() && slices.ContainsFunc(otherV4, func(p netip.Prefix) bool { return p.Contains(a) }):
		return ClassOther
	case a.Is6() && slices.ContainsFunc(otherV6, func(p netip.Prefix) bool { return p.Contains(a) }):
		return ClassOther
	case a.IsGlobalUnicast():
		return ClassPublic
	}
	return ClassOther
}

func mk(ifc Interface, ia IfAddr, a netip.Addr, c Class) Address {
	ad := Address{Addr: a, Interface: ifc.Name, Class: c, Wireless: ifc.Wireless, MayChange: ia.Dynamic}
	s := a.String()
	v6 := 0
	if a.Is6() {
		v6 = 1
	}
	switch c {
	case ClassTailscale:
		ad.Label, ad.Suggested, ad.rank = "Tailscale ("+s+")", true, 0+v6
	case ClassLAN:
		ad.Suggested = true
		ad.rank = 10 + 5*v6
		if ifc.Wireless {
			ad.Label = "Home/office Wi-Fi (" + s + ")"
			ad.rank++
		} else {
			ad.Label = "Home/office network (" + s + ")"
		}
	case ClassVPN:
		ad.Label, ad.Suggested, ad.rank = vpnName(ifc)+" ("+s+")", true, 20+v6
	case ClassLoopback:
		ad.Label, ad.Suggested, ad.rank = "This computer only ("+s+")", true, 30+v6
		ad.Warnings = append(ad.Warnings, WarnLoopback)
	case ClassShared:
		ad.Label, ad.rank = "Shared provider address ("+s+")", 40
		ad.Warnings = append(ad.Warnings, WarnShared)
	case ClassPublic:
		ad.Label, ad.rank = "Public internet address ("+s+")", 50+v6
		ad.Warnings = append(ad.Warnings, WarnPublic)
	case ClassOther:
		ad.Label, ad.rank = "Reserved-range address ("+s+")", 60
		ad.Warnings = append(ad.Warnings, WarnOther)
	case ClassLinkLocal:
		ad.Label, ad.rank = "Link-local ("+s+")", 70
		ad.Warnings = append(ad.Warnings, WarnLinkLocal)
	}
	if ad.MayChange && ad.Suggested && c != ClassTailscale {
		ad.Warnings = append(ad.Warnings, WarnMayChange)
	}
	return ad
}

func dedupe(as []Address) []Address {
	seen := map[netip.Addr]bool{}
	out := as[:0]
	for _, a := range as {
		if seen[a.Addr] {
			continue
		}
		seen[a.Addr] = true
		out = append(out, a)
	}
	return out
}

func reportWarnings(as []Address) []string {
	var sug []Address
	for _, a := range as {
		if a.Suggested && a.Class != ClassLoopback {
			sug = append(sug, a)
		}
	}
	if len(sug) == 0 {
		return []string{WarnNone}
	}
	var w []string
	onlyWiFi := true
	for _, a := range sug {
		if a.Class != ClassLAN || !a.Wireless {
			onlyWiFi = false
		}
	}
	if onlyWiFi {
		w = append(w, WarnOnlyWiFi)
	}
	return w
}

// ---- interface naming rules ----

type ifKind int

const (
	kindNormal ifKind = iota
	kindTailscale
	kindVPN
	kindVirtual
	kindSystem
	kindTransition
)

func hasPrefix(s string, ps ...string) bool {
	for _, p := range ps {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// interfaceKind applies the per-OS naming rules.
func interfaceKind(goos string, ifc Interface) (ifKind, string) {
	name := strings.ToLower(ifc.Name)
	desc := strings.ToLower(ifc.Description)
	if containsAny(name, "tailscale") || containsAny(desc, "tailscale") {
		return kindTailscale, ""
	}
	switch goos {
	case "windows":
		switch {
		case hasPrefix(name, "vethernet (wsl", "vethernet (default switch)", "vethernet (nat)"),
			containsAny(desc, "virtualbox host-only", "vmware virtual ethernet", "vmware network adapter", "docker"),
			hasPrefix(name, "vmware network adapter"):
			return kindVirtual, ReasonVirtual
		case containsAny(desc, "teredo", "isatap", "6to4", "ip-https") || hasPrefix(name, "teredo", "isatap", "6to4"):
			return kindTransition, ReasonTransition
		case containsAny(desc, "loopback", "bluetooth", "kernel debug", "wan miniport"):
			return kindSystem, ReasonSystem
		case containsAny(desc, "wireguard", "wintun", "zerotier", "tap-windows", "openvpn", "twingate",
			"nordlynx", "anyconnect", "fortinet", "globalprotect", "pangp", "juniper"):
			return kindVPN, ""
		case ifc.PointToPoint:
			return kindVPN, ""
		}
		return kindNormal, ""
	case "darwin":
		switch {
		case hasPrefix(name, "awdl", "llw", "anpi", "ap", "gif", "stf", "xhc", "iptap", "pktap"):
			return kindSystem, ReasonSystem
		case hasPrefix(name, "vmenet", "vmnet", "vboxnet"), isNumbered(name, "bridge", 100):
			return kindVirtual, ReasonVirtual
		case hasPrefix(name, "utun", "ipsec", "ppp", "zt", "feth", "wg", "tun", "tap"):
			return kindVPN, ""
		case ifc.PointToPoint:
			return kindVPN, ""
		}
		return kindNormal, ""
	default:
		switch {
		case name == "docker0" || hasPrefix(name, "docker", "br-", "veth", "virbr", "vnet", "lxcbr", "lxdbr",
			"cni", "flannel", "cali", "vxlan", "weave", "kube-", "podman", "vmnet", "vboxnet", "cilium",
			"genev_sys", "nerdctl", "incusbr", "tunl0", "dummy"):
			return kindVirtual, ReasonVirtual
		case hasPrefix(name, "sit", "ip6tnl", "teredo"):
			return kindTransition, ReasonTransition
		case hasPrefix(name, "wg", "tun", "tap", "zt", "ppp", "ipsec", "nordlynx", "proton", "sdptun", "twingate", "nebula", "utun"):
			return kindVPN, ""
		case ifc.PointToPoint:
			return kindVPN, ""
		}
		return kindNormal, ""
	}
}

// darwinUtun: the Tailscale app on macOS uses a utunN interface with no
// name hint, so a Tailscale-range address there is taken as Tailscale.
func darwinUtun(goos string, ifc Interface) bool {
	return goos == "darwin" && strings.HasPrefix(strings.ToLower(ifc.Name), "utun")
}

// isNumbered reports name == prefix+N with N >= from.
func isNumbered(name, prefix string, from int) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	n, err := strconv.Atoi(name[len(prefix):])
	return err == nil && n >= from
}

func vpnName(ifc Interface) string {
	n := strings.ToLower(ifc.Name + " " + ifc.Description)
	switch {
	case strings.Contains(n, "zerotier") || strings.HasPrefix(strings.ToLower(ifc.Name), "zt"):
		return "ZeroTier"
	case strings.Contains(n, "wireguard") || strings.HasPrefix(strings.ToLower(ifc.Name), "wg"):
		return "WireGuard"
	case strings.Contains(n, "twingate"):
		return "Twingate"
	case strings.Contains(n, "tailscale"):
		return "Tailscale (non-standard address)"
	}
	return "VPN \"" + ifc.Name + "\""
}
