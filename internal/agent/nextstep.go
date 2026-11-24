package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strings"
	"syscall"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/pg"
)

// Every stop a person can hit while setting up ends with one plain sentence
// of what to do next.
// The errors that already carry their own next step (ExitError messages,
// friendlyJoinError, portMsg) do not come here.

// NextStep is the next step for an error that has none of its own, or "".
// dataDir, if known, is named in the sentence.
func NextStep(err error, dataDir string) string {
	if err == nil {
		return ""
	}
	where := "this node's data folder"
	if dataDir != "" {
		where = dataDir
	}
	switch {
	case diskFull(err):
		return fmt.Sprintf("The disk is full: free some space on the drive that holds %s, then run the same command again.", where)
	case readOnlyFS(err):
		return fmt.Sprintf("The disk that holds %s does not accept writes (it is read-only): fix the disk, or pick a folder on another disk with --data-dir, then run the same command again.", where)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Sprintf("This account may not use a file in %s: run %s as the account that owns that folder (not with sudo or as administrator unless it was set up that way), or pick another folder with --data-dir.", where, brand.Command)
	case addrInUse(err):
		return "Another program uses that port: stop it, or pick another port (see `" + brand.Command + " help` for the port options), then run the same command again."
	case isNetErr(err):
		return "Check this computer's network connection (and that a firewall or proxy does not block it), then run the same command again."
	}
	return ""
}

func isNetErr(err error) bool {
	var dnsErr *net.DNSError
	var oe *net.OpError
	return errors.As(err, &dnsErr) || errors.As(err, &oe) || errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "no such host") || strings.Contains(err.Error(), "connection refused")
}

// diskFull: the disk (or the user's quota) is full.
func diskFull(err error) bool {
	if errors.Is(err, syscall.ENOSPC) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no space left on device") || strings.Contains(s, "disk quota exceeded") ||
		strings.Contains(s, "there is not enough space on the disk")
}

// startFailure is a PostgreSQL start (or first set-up) that failed.
type startFailure struct {
	// permanent: trying again cannot help until the person changes
	// something, so the node stops and says what (Windows run, D50).
	permanent bool
	msg       string
}

// classifyStartError reads what PostgreSQL printed when it refused to
// start. Only failures that are certain to repeat are permanent; every
// other failure keeps the old behaviour (retry with backoff).
func classifyStartError(err error, dataDir string) (startFailure, bool) {
	if err == nil {
		return startFailure{}, false
	}
	s := err.Error()
	l := strings.ToLower(s)
	has := func(subs ...string) bool {
		for _, x := range subs {
			if strings.Contains(l, strings.ToLower(x)) {
				return true
			}
		}
		return false
	}
	switch {
	case has("by a user with administrative permissions is not permitted"):
		return startFailure{true, fmt.Sprintf("PostgreSQL refuses to run with administrator rights, and it could not be started without them here. "+
			"Open a normal PowerShell (not \"Run as administrator\") and run: %s up --data-dir %s", brand.Command, quoteArg(dataDir))}, true
	case has(`"root" execution of the postgresql server is not permitted`, "cannot be run as root"):
		return startFailure{true, fmt.Sprintf("PostgreSQL does not run as root. Run %s as a normal user (without sudo), or install the background service with `sudo %s service install`, which runs it as its own user.", brand.Command, brand.Command)}, true
	case has("invalid value for parameter", "unrecognized configuration parameter", "contains errors", "syntax error in file"):
		return startFailure{true, fmt.Sprintf("PostgreSQL refused its settings (%s). If you changed postgresql.conf or postgresql.auto.conf in %s, or used ALTER SYSTEM, undo that change; then run `%s up --data-dir %s` again.",
			pgLine(s, "invalid value for parameter", "unrecognized configuration parameter", "syntax error in file"), dataDir, brand.Command, quoteArg(dataDir))}, true
	case has("has wrong ownership", "has invalid permissions", "is not owned by the current user"):
		return startFailure{true, fmt.Sprintf("PostgreSQL will not use its data folder because of its owner or permissions (%s). Run %s as the account that created %s, or start over in a new folder with --data-dir.",
			firstPGError(s), brand.Command, dataDir)}, true
	case has("could not create lock file", "could not open file") && has("permission denied"):
		return startFailure{true, fmt.Sprintf("PostgreSQL may not write in its data folder (%s). Run %s as the account that owns %s, or start over in a new folder with --data-dir.",
			firstPGError(s), brand.Command, dataDir)}, true
	case has("database files are incompatible with server", "was initialized by postgresql version"):
		return startFailure{true, fmt.Sprintf("The database in %s was made by a different PostgreSQL version (%s), so this program cannot open it. Use the %s version that made it, or start over in a new folder with --data-dir.",
			dataDir, firstPGError(s), brand.Command)}, true
	case diskFull(err):
		return startFailure{false, fmt.Sprintf("The disk that holds %s is full. Free some space; the node keeps trying and starts by itself once there is room.", dataDir)}, true
	case has("could not bind", "address already in use"):
		return startFailure{false, fmt.Sprintf("PostgreSQL's port on 127.0.0.1 is taken by another program. Stop that program, or run `%s up --data-dir %s --pg-port N` with a free port; the node keeps trying meanwhile.", brand.Command, quoteArg(dataDir))}, true
	}
	return startFailure{}, false
}

// pgLine is the message of the first line PostgreSQL printed that holds
// one of keys (without the time and level), else firstPGError.
func pgLine(s string, keys ...string) string {
	for _, line := range strings.Split(s, "\n") {
		for _, k := range keys {
			if i := strings.Index(line, k); i >= 0 {
				return strings.TrimSpace(line[i:])
			}
		}
	}
	return firstPGError(s)
}

// firstPGError is the first FATAL/ERROR line PostgreSQL printed, or the
// first line of the error.
func firstPGError(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		for _, k := range []string{"FATAL:", "ERROR:", "PANIC:"} {
			if i := strings.Index(line, k); i >= 0 {
				return strings.TrimSpace(line[i+len(k):])
			}
		}
	}
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// permanentStartRepeats: how many failed starts in a row with the same
// permanent cause stop the node.
const permanentStartRepeats = 3

// startProblem looks at a failed reconcile step. For a PostgreSQL start
// (or first set-up) that can never work (the same answer permanentStartRepeats times in a row) it returns the stop; for a known
// passing problem it says the plain words once and returns nil.
func (a *Agent) startProblem(err error) error {
	s := err.Error()
	if !errors.Is(err, pg.ErrExitedDuringStartup) && !strings.Contains(s, "PostgreSQL did not start") && !strings.Contains(s, "initdb failed") {
		return nil
	}
	f, ok := classifyStartError(err, a.node.StateDir)
	if !ok {
		a.permRepeats = 0
		return nil
	}
	if f.permanent {
		// Only after the same answer several times in a row: one failed
		// start can be passing (on Windows a file briefly held by a virus
		// scanner reads as "permission denied").
		if f.msg == a.lastHint {
			a.permRepeats++
		} else {
			a.permRepeats = 1
		}
		if a.permRepeats >= permanentStartRepeats {
			a.log.Error("postgres cannot start; stopping the node", "err", err)
			return &ExitError{Code: 1, Msg: f.msg, Cause: err}
		}
	} else {
		a.permRepeats = 0
	}
	if f.msg != a.lastHint {
		a.lastHint = f.msg
		a.say("%s", f.msg)
	}
	return nil
}
