package svc

import (
	"syscall"
	"testing"
)

func TestEscapeArgMatchesSyscall(t *testing.T) {
	for _, s := range []string{"", "a", "a b", `a"b`, `a\b`, `a\"b`, `C:\x y\`, `\\server\share dir\`, "tab\there", `""`, `a\\"b c`} {
		if got, want := escapeArg(s), syscall.EscapeArg(s); got != want {
			t.Errorf("escapeArg(%q) = %s, syscall.EscapeArg = %s", s, got, want)
		}
	}
}
