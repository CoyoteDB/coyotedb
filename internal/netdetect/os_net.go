//go:build !windows

package netdetect

import (
	"net"
	"net/netip"
)

// fromNet converts net.Interfaces output (the portable part of the OS layer).
func fromNet(nifs []net.Interface) []Interface {
	out := make([]Interface, 0, len(nifs))
	for _, ni := range nifs {
		ifc := Interface{
			Name:         ni.Name,
			Up:           ni.Flags&net.FlagUp != 0 && ni.Flags&net.FlagRunning != 0,
			Loopback:     ni.Flags&net.FlagLoopback != 0,
			PointToPoint: ni.Flags&net.FlagPointToPoint != 0,
		}
		addrs, err := ni.Addrs()
		if err == nil {
			for _, a := range addrs {
				pn, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				ip, ok := netip.AddrFromSlice(pn.IP)
				if !ok {
					continue
				}
				ones, _ := pn.Mask.Size()
				ifc.Addrs = append(ifc.Addrs, IfAddr{Prefix: netip.PrefixFrom(ip.Unmap(), ones)})
			}
		}
		out = append(out, ifc)
	}
	return out
}
