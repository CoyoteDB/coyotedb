package pg

import (
	"fmt"
	"maps"
	"runtime"
	"slices"
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

// earlierBuilds holds the sha256 of every earlier pinned release of the
// same major, newest first (D70). They are never downloaded: they let a
// cluster being updated one computer at a time recognise the build its
// not-yet-updated nodes run, so safety rule 1 can accept the update
// (KnownBuilds). Add the outgoing release here when Version changes.
//
// 18.6.0: the release's published .sha256 files, independently
// re-computed by hashing each downloaded archive (2026-11-25).
var earlierBuilds = []struct {
	Version string
	SHA256  map[string]string
}{
	{"18.6.0", map[string]string{
		"aarch64-apple-darwin":      "a257bcdb8aa3301a13d6a5bcec48f8c9517045b7cbae71e50788b2615539e95b",
		"x86_64-apple-darwin":       "f8918fbe747e0d79bda58ad8f8afcf23ac384b847ff3af18856f0825ba54b031",
		"x86_64-unknown-linux-gnu":  "bb3d09f876b2383e25a8c9ce09e32d185a03656a23d074f5195542b1b1b3ca61",
		"aarch64-unknown-linux-gnu": "5d2b5e8be9e96bfc3d7f22090057a836ec36d5d092542d8d3b93cc145cf4ee13",
		"x86_64-pc-windows-msvc":    "7da44c2dbcda3b49688ea08ce8cf99cfe677adf565f53a9145bf9002c74db7d5",
	}},
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

// GoPlatform maps an archive target triple back to Go's GOOS/GOARCH.
func GoPlatform(target string) (goos, goarch string, ok bool) {
	for _, p := range [][2]string{{"darwin", "arm64"}, {"darwin", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}, {"windows", "amd64"}} {
		if t, _ := TargetFor(p[0], p[1]); t == target {
			return p[0], p[1], true
		}
	}
	return "", "", false
}

// KnownBuilds lists every build this program pins: the current release
// for every target, then the earlier ones (earlierBuilds). Only the
// current one is ever downloaded or run.
func KnownBuilds() []Build {
	var out []Build
	add := func(version string, sums map[string]string) {
		for _, t := range slices.Sorted(maps.Keys(sums)) {
			out = append(out, Build{Version: version, Target: t, SHA256: sums[t]})
		}
	}
	add(Version, pinnedSHA256)
	for _, e := range earlierBuilds {
		add(e.Version, e.SHA256)
	}
	return out
}

// CurrentBuild returns the pinned build for the running platform.
func CurrentBuild() (Build, error) { return BuildFor(runtime.GOOS, runtime.GOARCH) }
