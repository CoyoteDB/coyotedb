package netdetect

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
)

// Linux: net.Interfaces, plus /sys/class/net/<if>/wireless for Wi-Fi and a
// netlink address dump for per-address flags: a finite valid lifetime
// (DHCP or SLAAC lease) marks an address as possibly changing, and
// IFA_F_TEMPORARY marks IPv6 privacy addresses. If netlink fails the
// flags are simply unknown.
func interfaces(context.Context) ([]Interface, error) {
	nifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	ifs := fromNet(nifs)
	flags := addrFlags()
	for i := range ifs {
		if _, err := os.Stat(filepath.Join("/sys/class/net", ifs[i].Name, "wireless")); err == nil {
			ifs[i].Wireless = true
		}
		for j := range ifs[i].Addrs {
			k := addrKey{nifs[i].Index, ifs[i].Addrs[j].Prefix.Addr()}
			if f, ok := flags[k]; ok {
				ifs[i].Addrs[j].Dynamic = f.dynamic
				ifs[i].Addrs[j].Temporary = f.temporary
			}
		}
	}
	return ifs, nil
}

type addrKey struct {
	index int
	addr  netip.Addr
}

type addrInfo struct{ dynamic, temporary bool }

const (
	ifaFlags     = 8 // IFA_FLAGS: u32 extended flags
	ifaCacheinfo = 6 // IFA_CACHEINFO
	ifaFTemp     = 0x01
	ifaFPerm     = 0x80
)

func addrFlags() map[addrKey]addrInfo {
	rib, err := syscall.NetlinkRIB(syscall.RTM_GETADDR, syscall.AF_UNSPEC)
	if err != nil {
		return nil
	}
	msgs, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		return nil
	}
	out := map[addrKey]addrInfo{}
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWADDR || len(m.Data) < syscall.SizeofIfAddrmsg {
			continue
		}
		ifam := (*syscall.IfAddrmsg)(unsafePointer(m.Data))
		attrs, err := syscall.ParseNetlinkRouteAttr(&m)
		if err != nil {
			continue
		}
		var local, address netip.Addr
		flags := uint32(ifam.Flags)
		valid := uint32(0xFFFFFFFF)
		for _, a := range attrs {
			switch a.Attr.Type {
			case syscall.IFA_ADDRESS:
				address, _ = netip.AddrFromSlice(a.Value)
			case syscall.IFA_LOCAL:
				local, _ = netip.AddrFromSlice(a.Value)
			case ifaFlags:
				if len(a.Value) >= 4 {
					flags = binary.NativeEndian.Uint32(a.Value)
				}
			case ifaCacheinfo:
				if len(a.Value) >= 8 {
					valid = binary.NativeEndian.Uint32(a.Value[4:8])
				}
			}
		}
		addr := address
		if local.IsValid() {
			addr = local // point-to-point: IFA_ADDRESS is the peer
		}
		if !addr.IsValid() {
			continue
		}
		out[addrKey{int(ifam.Index), addr.Unmap()}] = addrInfo{
			dynamic:   flags&ifaFPerm == 0 && valid != 0xFFFFFFFF,
			temporary: flags&ifaFTemp != 0,
		}
	}
	return out
}
