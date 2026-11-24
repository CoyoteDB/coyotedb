package pg

import (
	"fmt"
	"runtime"
)

// Version is the pinned PostgreSQL build (theseus-rs/postgresql-binaries
// release tag). Changing it requires updating every checksum below.
const Version = "18.7.0"

// MajorVersion is the PostgreSQL major version, as written in PG_VERSION.
const MajorVersion = "18"

const releaseBaseURL = "https://github.com/theseus-rs/postgresql-binaries/releases/download/"

// pinnedSHA256 holds the sha256 of every supported archive. These are pinned
// in source on purpose (DECISIONS.md D4, ARCHITECTURE.md "Security"): a
// checksum fetched at runtime proves nothing.
//
// Obtained 2026-11-20 from the release's published .sha256 files, and
// independently re-computed by hashing each downloaded archive.
var pinnedSHA256 = map[string]string{
	"aarch64-apple-darwin":      "86456c0ed99d2466524eaa074db6580cf7d748b91042e252c83eaca9a9d216e7",
	"x86_64-apple-darwin":       "9cd649259d6bf4af60a235e772c83853df215b63f4c1967c2eed7f9376aefb89",
	"x86_64-unknown-linux-gnu":  "53ca01952cb581145e4fb16b6b3b675da345a82879f350707b9f65863a0642f4",
	"aarch64-unknown-linux-gnu": "6fcf45163aa044fe58597885b0e27c4b1614c53ad088d4d0ebbbfdf5df510eaf",
	"x86_64-pc-windows-msvc":    "6e054adb854dbed1f78f9933a1c65737d85a47d2446ce0e9b5f055fb3794fe5c",
}

// Build identifies one downloadable Postgres archive.
type Build struct {
	Version string
	Target  string
	SHA256  string
}

// Name is the archive's base name without extension; it is also the single
// top-level directory inside the archive and the cache directory name.
func (b Build) Name() string { return "postgresql-" + b.Version + "-" + b.Target }

// URL is where the archive is downloaded from.
func (b Build) URL() string {
	return releaseBaseURL + b.Version + "/" + b.Name() + ".tar.gz"
}

// TargetFor maps a Go GOOS/GOARCH pair to the archive target triple.
func TargetFor(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "darwin/arm64":
		return "aarch64-apple-darwin", nil
	case "darwin/amd64":
		return "x86_64-apple-darwin", nil
	case "linux/amd64":
		return "x86_64-unknown-linux-gnu", nil
	case "linux/arm64":
		return "aarch64-unknown-linux-gnu", nil
	case "windows/amd64":
		return "x86_64-pc-windows-msvc", nil
	}
	return "", fmt.Errorf("no PostgreSQL build for %s/%s (supported: macOS arm64/x64, Linux x64/arm64, Windows x64)", goos, goarch)
}

// BuildFor returns the pinned build for a platform.
func BuildFor(goos, goarch string) (Build, error) {
	t, err := TargetFor(goos, goarch)
	if err != nil {
		return Build{}, err
	}
	sum, ok := pinnedSHA256[t]
	if !ok {
		return Build{}, fmt.Errorf("no pinned checksum for target %s", t)
	}
	return Build{Version: Version, Target: t, SHA256: sum}, nil
}

// CurrentBuild returns the pinned build for the running platform.
func CurrentBuild() (Build, error) { return BuildFor(runtime.GOOS, runtime.GOARCH) }
