package netdetect

import (
	"bufio"
	"strings"
)

func isWiFiPort(p string) bool {
	p = strings.ToLower(p)
	return p == "wi-fi" || p == "airport"
}

// parseHardwarePorts reads "Hardware Port: X\nDevice: enN" blocks.
func parseHardwarePorts(s string) map[string]string {
	m := map[string]string{}
	port := ""
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "Hardware Port:"):
			port = strings.TrimSpace(strings.TrimPrefix(line, "Hardware Port:"))
		case strings.HasPrefix(line, "Device:"):
			if dev := strings.TrimSpace(strings.TrimPrefix(line, "Device:")); dev != "" && port != "" {
				m[dev] = port
			}
			port = ""
		}
	}
	return m
}
