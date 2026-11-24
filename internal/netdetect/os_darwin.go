package netdetect

import (
	"context"
	"net"
	"os/exec"
	"time"
)

// On macOS the interface name says little (en0 may be Wi-Fi or Ethernet).
// `networksetup -listallhardwareports` maps devices to hardware ports
// ("Wi-Fi", "Thunderbolt Bridge", "USB 10/100/1000 LAN"). It is run with
// an argument list and a short timeout; if it fails, nothing is marked
// as Wi-Fi rather than guessing. DHCP vs static is not detected here.
func interfaces(ctx context.Context) ([]Interface, error) {
	nifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	ifs := fromNet(nifs)
	ports := hardwarePorts(ctx)
	for i := range ifs {
		if p, ok := ports[ifs[i].Name]; ok {
			ifs[i].Description = p
			ifs[i].Wireless = isWiFiPort(p)
		}
	}
	return ifs, nil
}

func hardwarePorts(ctx context.Context) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/networksetup", "-listallhardwareports").Output()
	if err != nil {
		return nil
	}
	return parseHardwarePorts(string(out))
}
