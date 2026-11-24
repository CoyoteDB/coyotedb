package svc

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"coyotedb.com/coyote/internal/brand"
	"coyotedb.com/coyote/internal/paths"
)

// macOS / launchd. A LaunchDaemon in the system domain (runs at boot,
// survives logout) as the hidden, non-admin user _coyote. Controlled with
// launchctl bootstrap / bootout / kickstart / print (never the deprecated
// load / unload). OS-independent logic; platform_darwin.go wires the host.

const (
	darwinUser     = "_" + brand.DirNameLower // "_coyote" (Apple's convention for daemon users)
	darwinLabel    = "com.coyotedb." + brand.Command
	darwinPlist    = "/Library/LaunchDaemons/" + darwinLabel + ".plist"
	darwinDataDir  = paths.DarwinSystemDataDir
	darwinLogDir   = "/Library/Logs/" + brand.DirName
	darwinTarget   = "system/" + darwinLabel
	darwinAdminGID = 80 // "admin": lets administrators read the logs without sudo
	// macOS refuses resource limits above OPEN_MAX for launchd jobs on
	// some versions, so the file limit stays at 10240 here.
	darwinFileLimit = 10240
	// launchctl print exits 113 for "Could not find service".
	launchctlNotFound = 113
)

type darwinPlatform struct {
	h     host
	log   *slog.Logger
	sleep func(time.Duration)
}

func (d *darwinPlatform) defaultDataDir() string { return darwinDataDir }

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// renderPlist renders the LaunchDaemon definition.
//
//	KeepAlive.SuccessfulExit=false  restart only after a failure
//	ThrottleInterval 10             at most one restart every 10 s
//	ExitTimeOut 120                 SIGTERM → SIGKILL grace (default is 20 s,
//	                                too short for Postgres' stop escalation)
//	Umask 077, file limit, logs in /Library/Logs/CoyoteDB
func renderPlist(o Options) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s); b.WriteByte('\n') }
	str := func(k, v string) { w("\t<key>" + k + "</key>\n\t<string>" + xmlText(v) + "</string>") }
	num := func(k string, v int) { w("\t<key>" + k + "</key>\n\t<integer>" + strconv.Itoa(v) + "</integer>") }
	w(`<?xml version="1.0" encoding="UTF-8"?>`)
	w(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`)
	w(`<!-- Installed by "` + brand.Command + ` service install"; reinstalling overwrites this file. -->`)
	w(`<plist version="1.0">`)
	w(`<dict>`)
	str("Label", darwinLabel)
	w("\t<key>ProgramArguments</key>")
	w("\t<array>")
	for _, a := range append([]string{o.Executable}, o.Args...) {
		w("\t\t<string>" + xmlText(a) + "</string>")
	}
	w("\t</array>")
	str("UserName", darwinUser)
	str("GroupName", darwinUser)
	str("WorkingDirectory", o.DataDir)
	w("\t<key>RunAtLoad</key>\n\t<true/>")
	w("\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>")
	num("ThrottleInterval", 10)
	num("ExitTimeOut", StopTimeoutSeconds)
	num("Umask", 0o077)
	str("ProcessType", "Standard")
	w("\t<key>SoftResourceLimits</key>\n\t<dict>\n\t\t<key>NumberOfFiles</key>\n\t\t<integer>" + strconv.Itoa(darwinFileLimit) + "</integer>\n\t</dict>")
	w("\t<key>HardResourceLimits</key>\n\t<dict>\n\t\t<key>NumberOfFiles</key>\n\t\t<integer>" + strconv.Itoa(darwinFileLimit) + "</integer>\n\t</dict>")
	str("StandardOutPath", darwinLogDir+"/coyote.out.log")
	str("StandardErrorPath", darwinLogDir+"/coyote.err.log")
	w(`</dict>`)
	w(`</plist>`)
	return b.String()
}

// plistString returns the string value of a top-level key.
func plistString(data []byte, key string) string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	depth, wantNext := 0, false
	for {
		tok, err := dec.Token()
		if err == io.EOF || err != nil {
			return ""
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 3 && t.Name.Local == "key" {
				var k string
				if dec.DecodeElement(&k, &t) == nil {
					depth--
					wantNext = k == key
				}
				continue
			}
			if depth == 3 && wantNext && t.Name.Local == "string" {
				var v string
				if dec.DecodeElement(&v, &t) == nil {
					return v
				}
				return ""
			}
			if depth == 3 {
				wantNext = false
			}
		case xml.EndElement:
			depth--
		}
	}
}

func (d *darwinPlatform) loaded(ctx context.Context) (bool, string, error) {
	out, err := d.h.Run(ctx, "launchctl", "print", darwinTarget)
	if err != nil {
		if exitCode(err) == launchctlNotFound {
			return false, "", nil
		}
		return false, "", err
	}
	return true, out, nil
}

func (d *darwinPlatform) install(ctx context.Context, o Options) (Report, error) {
	rep := Report{DataDir: o.DataDir}
	if !d.h.Elevated() {
		return rep, elevationError("darwin", "install")
	}
	if err := unixPathChecks(o, "darwin"); err != nil {
		return rep, err
	}
	if !exists(d.h, o.Executable) {
		return rep, fmt.Errorf("program %s not found", o.Executable)
	}
	uid, gid, err := d.ensureUser(ctx, &rep)
	if err != nil {
		return rep, err
	}
	if err := prepareDir(d.h, o.DataDir, uid, gid, 0o700, &rep); err != nil {
		return rep, err
	}
	if err := prepareDir(d.h, darwinLogDir, uid, darwinAdminGID, 0o750, &rep); err != nil {
		return rep, err
	}
	plist := renderPlist(o)
	old, _ := d.h.ReadFile(darwinPlist)
	changed := string(old) != plist
	if changed {
		if err := d.h.WriteFileAtomic(darwinPlist, []byte(plist), 0o644); err != nil {
			return rep, fmt.Errorf("write %s: %w", darwinPlist, err)
		}
		if old == nil {
			rep.step("Created the service definition %s (starts when the computer starts)", darwinPlist)
		} else {
			rep.step("Updated the service definition %s", darwinPlist)
		}
	}
	// Clear a "disabled" override left by an earlier launchctl disable.
	if _, err := d.h.Run(ctx, "launchctl", "enable", darwinTarget); err != nil {
		return rep, err
	}
	loaded, _, err := d.loaded(ctx)
	if err != nil {
		return rep, err
	}
	switch {
	case o.Start && loaded && changed:
		if err := d.bootout(ctx); err != nil {
			return rep, err
		}
		if _, err := d.h.Run(ctx, "launchctl", "bootstrap", "system", darwinPlist); err != nil {
			return rep, err
		}
		rep.step("Restarted the service with the new definition")
	case o.Start && loaded:
		if _, err := d.h.Run(ctx, "launchctl", "kickstart", darwinTarget); err != nil {
			return rep, err
		}
	case o.Start:
		if _, err := d.h.Run(ctx, "launchctl", "bootstrap", "system", darwinPlist); err != nil {
			return rep, err
		}
		rep.step("Started the service")
	case loaded && changed:
		rep.note("The service is running with the old definition; restart it to apply: sudo %s service stop, then sudo %s service start", brand.Command, brand.Command)
	}
	if len(rep.Steps) == 0 {
		rep.step("Already installed; nothing to change")
	}
	rep.note("Data folder: %s", o.DataDir)
	rep.note("Logs: %s", darwinLogDir)
	return rep, nil
}

// ensureUser creates the hidden daemon user and group _coyote (same
// number for both, picked free in 400–499, below the 500 where login
// users start) with no shell, no password and /var/empty as home.
func (d *darwinPlatform) ensureUser(ctx context.Context, rep *Report) (int, int, error) {
	uid, gid, ok, err := lookupIDs(ctx, d.h, darwinUser)
	if err != nil {
		return 0, 0, err
	}
	if ok {
		if uid == 0 {
			return 0, 0, fmt.Errorf("user %q exists but is root; refusing to run the database as root", darwinUser)
		}
		return uid, gid, nil
	}
	used := map[int]bool{}
	for _, args := range [][]string{{".", "-list", "/Users", "UniqueID"}, {".", "-list", "/Groups", "PrimaryGroupID"}} {
		out, err := d.h.Run(ctx, "dscl", args...)
		if err != nil {
			return 0, 0, err
		}
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				if n, err := strconv.Atoi(f[len(f)-1]); err == nil {
					used[n] = true
				}
			}
		}
	}
	id := 0
	for n := 400; n < 500; n++ {
		if !used[n] {
			id = n
			break
		}
	}
	if id == 0 {
		return 0, 0, fmt.Errorf("no free system user id between 400 and 499 for %q", darwinUser)
	}
	ids := strconv.Itoa(id)
	g, u := "/Groups/"+darwinUser, "/Users/"+darwinUser
	steps := [][]string{
		{".", "-create", g},
		{".", "-create", g, "PrimaryGroupID", ids},
		{".", "-create", g, "RealName", brand.Product + " service"},
		{".", "-create", g, "Password", "*"},
		{".", "-create", u},
		{".", "-create", u, "UniqueID", ids},
		{".", "-create", u, "PrimaryGroupID", ids},
		{".", "-create", u, "UserShell", "/usr/bin/false"},
		{".", "-create", u, "NFSHomeDirectory", "/var/empty"},
		{".", "-create", u, "RealName", brand.Product + " service"},
		{".", "-create", u, "Password", "*"},
		{".", "-create", u, "IsHidden", "1"},
	}
	for _, a := range steps {
		if _, err := d.h.Run(ctx, "dscl", a...); err != nil {
			return 0, 0, err
		}
	}
	rep.step("Created the hidden system user %q (not an administrator, cannot log in)", darwinUser)
	return id, id, nil
}

// bootout stops and unloads the job and waits until launchd has let go.
func (d *darwinPlatform) bootout(ctx context.Context) error {
	if _, err := d.h.Run(ctx, "launchctl", "bootout", darwinTarget); err != nil {
		if loaded, _, lerr := d.loaded(ctx); lerr == nil && !loaded {
			return nil // it went away anyway (bootout reports EIO on slow exits)
		}
		if exitCode(err) != 5 { // 5: "Input/output error" while still exiting
			return err
		}
	}
	deadline := time.Now().Add((StopTimeoutSeconds + 15) * time.Second)
	for {
		loaded, _, err := d.loaded(ctx)
		if err != nil {
			return err
		}
		if !loaded {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service did not stop within %d seconds", StopTimeoutSeconds+15)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.sleep(250 * time.Millisecond)
	}
}

func (d *darwinPlatform) status(ctx context.Context) (Status, error) {
	data, err := d.h.ReadFile(darwinPlist)
	if err != nil {
		return Status{State: "not installed"}, nil
	}
	st := Status{Installed: true, Definition: darwinPlist, DataDir: plistString(data, "WorkingDirectory"), State: "not loaded"}
	loaded, out, err := d.loaded(ctx)
	if err != nil {
		return st, err
	}
	if !loaded {
		return st, nil
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch k {
		case "state":
			if st.State == "not loaded" {
				st.State = v
			}
		case "pid":
			if st.PID == 0 {
				st.PID, _ = strconv.Atoi(v)
			}
		}
	}
	st.Running = st.State == "running"
	return st, nil
}

func (d *darwinPlatform) start(ctx context.Context) error {
	if !d.h.Elevated() {
		return elevationError("darwin", "start")
	}
	if !exists(d.h, darwinPlist) {
		return ErrNotInstalled
	}
	if _, err := d.h.Run(ctx, "launchctl", "enable", darwinTarget); err != nil {
		return err
	}
	loaded, _, err := d.loaded(ctx)
	if err != nil {
		return err
	}
	if loaded {
		_, err = d.h.Run(ctx, "launchctl", "kickstart", darwinTarget)
		return err
	}
	_, err = d.h.Run(ctx, "launchctl", "bootstrap", "system", darwinPlist)
	return err
}

// stop unloads the job (it stays installed and loads again at boot or on
// start). A plain SIGTERM would not do: KeepAlive restarts on failure.
func (d *darwinPlatform) stop(ctx context.Context) error {
	if !d.h.Elevated() {
		return elevationError("darwin", "stop")
	}
	if !exists(d.h, darwinPlist) {
		return ErrNotInstalled
	}
	return d.bootout(ctx)
}

func (d *darwinPlatform) uninstall(ctx context.Context, o UninstallOptions) (Report, error) {
	rep := Report{}
	if !d.h.Elevated() {
		return rep, elevationError("darwin", "uninstall")
	}
	dataDir := darwinDataDir
	if data, err := d.h.ReadFile(darwinPlist); err == nil {
		if v := plistString(data, "WorkingDirectory"); v != "" {
			dataDir = v
		}
	}
	loaded, _, err := d.loaded(ctx)
	if err != nil {
		return rep, err
	}
	if loaded {
		if err := d.bootout(ctx); err != nil {
			return rep, err
		}
		rep.step("Stopped the service (the database shut down cleanly)")
	}
	if exists(d.h, darwinPlist) {
		if err := d.h.Remove(darwinPlist); err != nil {
			return rep, err
		}
		rep.step("Removed %s", darwinPlist)
	} else if !loaded {
		rep.step("The background service was not installed")
	}
	rep.DataDir = dataDir
	if !o.Purge {
		if exists(d.h, dataDir) {
			rep.note("Your data is still in %s. To delete it for good: sudo %s service uninstall --purge", dataDir, brand.Command)
		}
		return rep, nil
	}
	if err := purgeDir(d.h, dataDir, &rep); err != nil {
		return rep, err
	}
	if exists(d.h, darwinLogDir) {
		if err := d.h.RemoveAll(darwinLogDir); err != nil {
			return rep, err
		}
		rep.step("Deleted the logs in %s", darwinLogDir)
	}
	if _, _, ok, err := lookupIDs(ctx, d.h, darwinUser); err == nil && ok {
		if _, err := d.h.Run(ctx, "dscl", ".", "-delete", "/Users/"+darwinUser); err != nil {
			return rep, err
		}
		_, _ = d.h.Run(ctx, "dscl", ".", "-delete", "/Groups/"+darwinUser)
		rep.step("Removed the system user %q", darwinUser)
	}
	return rep, nil
}
