package netdetect

import "testing"

func TestParseHardwarePorts(t *testing.T) {
	in := `
Hardware Port: Ethernet Adapter (en4)
Device: en4
Ethernet Address: 1a:2b:3c:4d:5e:6f

Hardware Port: Wi-Fi
Device: en0
Ethernet Address: aa:bb:cc:dd:ee:ff

Hardware Port: Thunderbolt Bridge
Device: bridge0
Ethernet Address: N/A

VLAN Configurations
===================
`
	m := parseHardwarePorts(in)
	if m["en0"] != "Wi-Fi" || m["bridge0"] != "Thunderbolt Bridge" || m["en4"] != "Ethernet Adapter (en4)" || len(m) != 3 {
		t.Fatalf("%v", m)
	}
}
