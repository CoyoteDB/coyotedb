// Package paths picks OS-appropriate default locations.
//
// There is one machine-wide data folder per OS, shared by the background
// service and the command line (SystemDataDir). The command line uses it
// whenever it can: when a service is installed (the caller passes its
// folder; see cmd/coyote) or when it runs as an administrator. Only an
// unprivileged foreground run on a machine without the service falls back
// to a folder in the person's home (UserDataDir), because the machine-wide
// one is not writable for them.
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"coyotedb.com/coyote/internal/brand"
)

// Machine-wide data folders (SystemDataDir) on Linux and macOS. Windows
// uses %ProgramData%\CoyoteDB.
const (
	LinuxSystemDataDir  = "/var/lib/" + brand.DirNameLower
	DarwinSystemDataDir = "/Library/Application Support/" + brand.DirName
)

// SystemDataDir is the machine-wide data folder: the background service's.
func SystemDataDir() (string, error) { return systemDataDir(runtime.GOOS, os.Getenv) }

func systemDataDir(goos string, getenv func(string) string) (string, error) {
	switch goos {
	case "windows":
		pd := getenv("ProgramData")
		if pd == "" {
			return "", errors.New("ProgramData is not set")
		}
		return filepath.Join(pd, brand.DirName), nil
	case "darwin":
		return filepath.FromSlash(DarwinSystemDataDir), nil
	default:
		return filepath.FromSlash(LinuxSystemDataDir), nil
	}
}

// DefaultDataDir returns the default node data folder when no service
// folder applies: SystemDataDir for an administrator (always on Windows,
// where %ProgramData% is the only sensible place), else UserDataDir.
//
//   - Windows: %ProgramData%\CoyoteDB
//   - macOS:   /Library/Application Support/CoyoteDB as root, else
//     ~/Library/Application Support/CoyoteDB
//   - Linux:   /var/lib/coyote as root, else ~/.local/share/coyote
//     (or $XDG_DATA_HOME/coyote)
func DefaultDataDir() (string, error) {
	return defaultDataDir(runtime.GOOS, os.Getenv, os.UserHomeDir, os.Geteuid)
}

func defaultDataDir(goos string, getenv func(string) string, home func() (string, error), euid func() int) (string, error) {
	if goos == "windows" || euid() == 0 {
		return systemDataDir(goos, getenv)
	}
	return userDataDir(goos, getenv, home)
}

// UserDataDir is the per-person data folder (no administrator rights).
func UserDataDir() (string, error) { return userDataDir(runtime.GOOS, os.Getenv, os.UserHomeDir) }

func userDataDir(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	switch goos {
	case "windows":
		return systemDataDir(goos, getenv)
	case "darwin":
		h, err := home()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, "Library", "Application Support", brand.DirName), nil
	default:
		if x := getenv("XDG_DATA_HOME"); x != "" && filepath.IsAbs(x) {
			return filepath.Join(x, brand.DirNameLower), nil
		}
		h, err := home()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, ".local", "share", brand.DirNameLower), nil
	}
}
