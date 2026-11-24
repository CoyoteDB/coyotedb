package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"coyotedb.com/coyote/internal/brand"
)

// An invite code is a secret: whoever holds it can join the cluster once.
// A command-line argument is visible to every user of the computer (`ps`,
// Task Manager, /proc/<pid>/cmdline) for as long as the program runs, and
// `join` runs for as long as the node does. So `join` and `witness` read
// the code from a prompt or a pipe, from COYOTE_INVITE, or from a file
// (--invite-file). A code given as an argument still works, with a warning.

// inviteEnv is the environment variable that may carry the invite code.
const inviteEnv = brand.EnvPrefix + "INVITE"

// maxInviteInput bounds what is read from a file or a pipe (a code is
// about 150 characters; line breaks and spaces are allowed in it).
const maxInviteInput = 64 << 10

// inviteSource is where readInvite looks.
type inviteSource struct {
	Positional []string // arguments after the command
	File       string   // --invite-file ("-" = standard input)
	Env        string   // value of COYOTE_INVITE
	Stdin      io.Reader
	StdinTTY   bool // standard input is a terminal: prompt for the code
	Prompt     io.Writer
	Cmd        string // "join" or "witness", for messages
}

// errNoInvite: nothing was given anywhere.
var errNoInvite = errors.New("no invite code given")

// readInvite returns the invite code and a warning to print (or "").
func readInvite(s inviteSource) (code, warning string, err error) {
	n := 0
	if len(s.Positional) > 0 {
		n++
	}
	if s.File != "" {
		n++
	}
	if s.Env != "" {
		n++
	}
	switch {
	case len(s.Positional) > 1:
		return "", "", fmt.Errorf("expected one invite code, got %d arguments", len(s.Positional))
	case n > 1:
		return "", "", fmt.Errorf("the invite code was given more than one way (argument, --invite-file, %s); give it one way only", inviteEnv)
	case len(s.Positional) == 1:
		return s.Positional[0], fmt.Sprintf("Tip: an invite code typed after `%s %s` can be seen by other users of this computer while the node runs. "+
			"Next time run `%s %s` without it and paste the code when asked (or use --invite-file FILE or %s).", brand.Command, s.Cmd, brand.Command, s.Cmd, inviteEnv), nil
	case s.Env != "":
		return strings.TrimSpace(s.Env), "", nil
	case s.File == "-":
		return readAllInvite(s.Stdin, "standard input")
	case s.File != "":
		f, err := os.Open(s.File)
		if err != nil {
			return "", "", fmt.Errorf("could not read the invite file: %w", err)
		}
		defer f.Close()
		return readAllInvite(f, s.File)
	}
	if s.Stdin == nil {
		return "", "", errNoInvite
	}
	if s.StdinTTY {
		fmt.Fprint(s.Prompt, "Paste the invite code (from `"+brand.Command+" invite` on a computer already in the cluster) and press Enter: ")
		line, err := bufio.NewReaderSize(s.Stdin, 4096).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", "", fmt.Errorf("read the invite code: %w", err)
		}
		if strings.TrimSpace(line) == "" {
			return "", "", errNoInvite
		}
		return strings.TrimSpace(line), "", nil
	}
	return readAllInvite(s.Stdin, "standard input")
}

func readAllInvite(r io.Reader, what string) (string, string, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxInviteInput+1))
	if err != nil {
		return "", "", fmt.Errorf("read the invite code from %s: %w", what, err)
	}
	if len(b) > maxInviteInput {
		return "", "", fmt.Errorf("%s is too large to be an invite code", what)
	}
	code := strings.TrimSpace(string(b))
	if code == "" {
		return "", "", errNoInvite
	}
	return code, "", nil
}

// noInviteHelp is printed when no code was found.
func noInviteHelp(cmd string) string {
	return fmt.Sprintf("No invite code given. On a computer that is already in the cluster run `%s invite`, then here run `%s %s` and paste the code when asked "+
		"(or use --invite-file FILE, or set %s).", brand.Command, brand.Command, cmd, inviteEnv)
}
