package svc

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"coyotedb.com/coyote/internal/brand"
)

// Windows rules that do not need Windows to compute, so they are tested
// everywhere. The calls into the service manager are in windows.go.

const (
	// winAccount is the virtual account the service runs as. Postgres
	// refuses to run as an administrator; a virtual account is not one,
	// needs no password and is created by the service manager.
	winAccount      = `NT SERVICE\` + Name
	winFirewallRule = brand.Product
	winResetPeriod  = 24 * time.Hour
)

// winRecovery is restart after 5 s, 15 s, then 60 s (failure count resets
// after a day without failures).
var winRecovery = []time.Duration{5 * time.Second, 15 * time.Second, 60 * time.Second}

// serviceSID computes the per-service SID (S-1-5-80-…) that Windows gives
// a service and its virtual account: the SHA-1 of the upper-case service
// name in UTF-16LE, read as five little-endian 32-bit numbers.
func serviceSID(name string) string {
	u := utf16.Encode([]rune(strings.ToUpper(name)))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	h := sha1.Sum(b)
	parts := []string{"S-1-5-80"}
	for i := 0; i < 5; i++ {
		parts = append(parts, strconv.FormatUint(uint64(binary.LittleEndian.Uint32(h[4*i:])), 10))
	}
	return strings.Join(parts, "-")
}

// dataDirSDDL is the data folder's security descriptor: owned by
// Administrators; inherited permissions cut off (P = protected, AI =
// auto-inherited); full control, inherited by every file and folder below
// (OICI), for SYSTEM, Administrators and the service's own account only.
func dataDirSDDL(serviceName string) string {
	return "O:BAG:SYD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + serviceSID(serviceName) + ")"
}

// escapeArg quotes one argument exactly like Go's syscall.EscapeArg (which
// mgr.CreateService uses), so an installed command line compares equal to
// a freshly built one.
func escapeArg(s string) string {
	if s == "" {
		return `""`
	}
	needsBackslash := strings.ContainsAny(s, `"\`)
	hasSpace := strings.ContainsAny(s, " \t")
	if !needsBackslash && !hasSpace {
		return s
	}
	if !needsBackslash {
		return `"` + s + `"`
	}
	var b strings.Builder
	if hasSpace {
		b.WriteByte('"')
	}
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteByte(c)
	}
	if hasSpace {
		b.WriteString(strings.Repeat(`\`, slashes))
		b.WriteByte('"')
	}
	return b.String()
}

// winBinaryPath is the service's command line.
func winBinaryPath(exe string, args []string) string {
	parts := []string{escapeArg(exe)}
	for _, a := range args {
		parts = append(parts, escapeArg(a))
	}
	return strings.Join(parts, " ")
}

// dataDirFromArgs finds --data-dir in a service command line.
func dataDirFromArgs(args []string) string {
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--data-dir="); ok {
			return v
		}
		if a == "--data-dir" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// netshCmdLine builds the exact command line for netsh, which parses
// key=value itself: values with spaces must be written key="value" (a
// whole-argument quote, as escapeArg would produce, is not understood).
// Quotes inside values are refused instead of being escaped.
func netshCmdLine(exe string, args []string) (string, error) {
	parts := []string{escapeArg(exe)}
	for _, a := range args {
		if strings.ContainsAny(a, "\"\r\n\x00") {
			return "", fmt.Errorf("netsh argument %q contains a quote or control character", a)
		}
		k, v, ok := strings.Cut(a, "=")
		switch {
		case ok && strings.ContainsAny(v, " \t&|<>^()%!"):
			parts = append(parts, k+`="`+v+`"`)
		case !ok && strings.ContainsAny(a, " \t"):
			return "", errors.New("netsh argument without key contains a space")
		default:
			parts = append(parts, a)
		}
	}
	return strings.Join(parts, " "), nil
}

func joinPorts(ports []int) string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

// firewallAddArgs: inbound TCP to the given ports, for the agent program
// only, on private and domain networks (never public ones).
func firewallAddArgs(exe string, ports []int) []string {
	return []string{
		"advfirewall", "firewall", "add", "rule",
		"name=" + winFirewallRule,
		"description=" + brand.Product + " cluster and database ports (added by " + brand.Command + " service install)",
		"dir=in", "action=allow", "protocol=TCP",
		"localport=" + joinPorts(ports),
		"program=" + exe,
		"profile=private,domain",
		"enable=yes",
	}
}

func firewallDeleteArgs() []string {
	return []string{"advfirewall", "firewall", "delete", "rule", "name=" + winFirewallRule}
}

// firewallShowArgs lists the rule; netsh exits non-zero when there is
// none (the exit code, unlike the message, does not depend on the
// Windows display language).
func firewallShowArgs() []string {
	return []string{"advfirewall", "firewall", "show", "rule", "name=" + winFirewallRule}
}
