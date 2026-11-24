package netdetect

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows: GetAdaptersAddresses gives the friendly name ("Wi-Fi"), the
// adapter description ("Hyper-V Virtual Ethernet Adapter", "WireGuard
// Tunnel"), the interface type (Wi-Fi, tunnel, loopback), the operational
// state, and for every address whether its prefix came from DHCP.
func interfaces(context.Context) ([]Interface, error) {
	aas, err := adapterAddresses()
	if err != nil {
		return nil, err
	}
	var out []Interface
	for aa := aas; aa != nil; aa = aa.Next {
		ifc := Interface{
			Name:         windows.UTF16PtrToString(aa.FriendlyName),
			Description:  windows.UTF16PtrToString(aa.Description),
			Up:           aa.OperStatus == windows.IfOperStatusUp,
			Loopback:     aa.IfType == windows.IF_TYPE_SOFTWARE_LOOPBACK,
			PointToPoint: aa.IfType == windows.IF_TYPE_TUNNEL || aa.IfType == windows.IF_TYPE_PPP,
			Wireless:     aa.IfType == windows.IF_TYPE_IEEE80211,
		}
		for ua := aa.FirstUnicastAddress; ua != nil; ua = ua.Next {
			sa, err := ua.Address.Sockaddr.Sockaddr()
			if err != nil {
				continue
			}
			var ip netip.Addr
			switch s := sa.(type) {
			case *syscall.SockaddrInet4:
				ip = netip.AddrFrom4(s.Addr)
			case *syscall.SockaddrInet6:
				ip = netip.AddrFrom16(s.Addr)
			default:
				continue
			}
			ifc.Addrs = append(ifc.Addrs, IfAddr{
				Prefix:  netip.PrefixFrom(ip.Unmap(), int(ua.OnLinkPrefixLength)),
				Dynamic: ua.PrefixOrigin == windows.IpPrefixOriginDhcp,
			})
		}
		out = append(out, ifc)
	}
	return out, nil
}

// adapterAddresses is the usual grow-and-retry call (as in package net).
func adapterAddresses() (*windows.IpAdapterAddresses, error) {
	var b []byte
	l := uint32(15000)
	for {
		b = make([]byte, l)
		const flags = windows.GAA_FLAG_SKIP_ANYCAST | windows.GAA_FLAG_SKIP_MULTICAST | windows.GAA_FLAG_SKIP_DNS_SERVER
		err := windows.GetAdaptersAddresses(windows.AF_UNSPEC, flags, 0, (*windows.IpAdapterAddresses)(unsafe.Pointer(&b[0])), &l)
		if err == nil {
			if l == 0 {
				return nil, nil
			}
			return (*windows.IpAdapterAddresses)(unsafe.Pointer(&b[0])), nil
		}
		if !errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			return nil, os.NewSyscallError("getadaptersaddresses", err)
		}
		if l <= uint32(len(b)) {
			return nil, os.NewSyscallError("getadaptersaddresses", err)
		}
	}
}
