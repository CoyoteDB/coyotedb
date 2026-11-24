package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCode = "coyote-abcd efgh\n"

func TestReadInvite(t *testing.T) {
	file := filepath.Join(t.TempDir(), "invite")
	if err := os.WriteFile(file, []byte("  coyote-fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		src      inviteSource
		code     string
		warn     bool
		err      error
		anyErr   bool
		prompted bool
	}{
		{name: "pipe", src: inviteSource{Stdin: strings.NewReader(testCode)}, code: "coyote-abcd efgh"},
		{name: "prompt", src: inviteSource{Stdin: strings.NewReader("coyote-typed\nrest"), StdinTTY: true}, code: "coyote-typed", prompted: true},
		{name: "env", src: inviteSource{Env: " coyote-env ", Stdin: strings.NewReader("ignored")}, code: "coyote-env"},
		{name: "file", src: inviteSource{File: file}, code: "coyote-fromfile"},
		{name: "file dash is stdin", src: inviteSource{File: "-", Stdin: strings.NewReader("coyote-dash")}, code: "coyote-dash"},
		{name: "argument warns", src: inviteSource{Positional: []string{"coyote-arg"}}, code: "coyote-arg", warn: true},
		{name: "nothing piped", src: inviteSource{Stdin: strings.NewReader("")}, err: errNoInvite},
		{name: "empty prompt", src: inviteSource{Stdin: strings.NewReader("\n"), StdinTTY: true}, err: errNoInvite, prompted: true},
		{name: "two ways", src: inviteSource{Positional: []string{"coyote-a"}, Env: "coyote-b"}, anyErr: true},
		{name: "two arguments", src: inviteSource{Positional: []string{"a", "b"}}, anyErr: true},
		{name: "missing file", src: inviteSource{File: file + ".nope"}, anyErr: true},
		{name: "huge pipe", src: inviteSource{Stdin: strings.NewReader(strings.Repeat("x", maxInviteInput+5))}, anyErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var prompt bytes.Buffer
			c.src.Prompt, c.src.Cmd = &prompt, "join"
			code, warn, err := readInvite(c.src)
			switch {
			case c.err != nil && !errors.Is(err, c.err):
				t.Fatalf("err %v, want %v", err, c.err)
			case c.anyErr && err == nil:
				t.Fatal("no error")
			case c.err == nil && !c.anyErr && err != nil:
				t.Fatal(err)
			}
			if code != c.code || (warn != "") != c.warn {
				t.Fatalf("code %q warn %q", code, warn)
			}
			if c.warn && !strings.Contains(warn, "COYOTE_INVITE") {
				t.Fatalf("warning does not say what to do: %q", warn)
			}
			if (prompt.Len() > 0) != c.prompted {
				t.Fatalf("prompted %q", prompt.String())
			}
		})
	}
}

func TestCheckDashListen(t *testing.T) {
	if p, err := checkDashListen("0.0.0.0:7480", 0); err != nil || p != 7480 {
		t.Fatalf("%d %v", p, err)
	}
	for _, s := range []string{"7480", "localhost:7480", "0.0.0.0:0", "0.0.0.0:x"} {
		if _, err := checkDashListen(s, 0); err == nil {
			t.Errorf("%s accepted", s)
		}
	}
	if _, err := checkDashListen("0.0.0.0:7480", 7490); err == nil {
		t.Error("a port other than the control port accepted")
	}
}

func TestNoArgsGuide(t *testing.T) {
	var out, errb bytes.Buffer
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("ProgramData", t.TempDir())
	if code := run(nil, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{"coyote up", "coyote invite", "coyote join", "paste the code", "Linux is the recommended", "coyote help"} {
		if !strings.Contains(s, want) {
			t.Errorf("guide lacks %q:\n%s", want, s)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"--data-dir": "--data-dir", `C:\My Data`: `"C:\My Data"`, "": `""`} {
		if got := shellQuote(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
