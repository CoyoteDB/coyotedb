package cluster

import (
	"fmt"
	"strconv"
	"strings"
)

// PostgreSQL minor updates (D70, safety rule 1).
//
// Rule 1 records each data node's PostgreSQL build (Platform.PGBuildSHA)
// and required them equal. A PostgreSQL minor update changes the build, so
// a cluster being updated one computer at a time holds two builds for a
// while. PostgreSQL supports physical replication between minor releases
// of one major version, as long as no copy (standby) runs an older minor
// than the main node (primary) it copies.
//
// State.PGBuilds is the cluster's table of pinned builds: every build is
// one that a member's own program pins (internal/pg), registered by that
// member's agent with RegisterPGBuilds once every member's agent knows the
// command (the "caps" RPC says "pg_builds"). The state machine allows a
// data node of another build only through this table, so its decision
// depends on the state alone and is the same on every member: while the
// table is empty (a member still runs a release without this rule) rule 1
// is exactly as before.

// PGBuild is one pinned PostgreSQL build: the theseus-rs release (Version,
// "18.7.0") and the operating system and processor it is for, in Go's
// names (Platform.OS, Platform.Arch).
type PGBuild struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// MaxPGBuilds caps the table so the cluster state stays small (five
// targets per release; this is decades of quarterly releases).
const MaxPGBuilds = 512

// RegisterPGBuilds adds pinned builds to State.PGBuilds (key: the
// archive's sha256). Written only when every member's agent knows it
// (caps "pg_builds"), so an older state machine never halts on it. An
// entry already present must be identical; a different one refuses the
// whole command (a sha256 names one archive).
type RegisterPGBuilds struct {
	Builds map[string]PGBuild `json:"builds"`
}

func (*RegisterPGBuilds) commandType() string { return "register_pg_builds" }

func applyRegisterPGBuilds(st *State, c *RegisterPGBuilds) error {
	if len(c.Builds) == 0 {
		return errorf(ErrInvalid, "no builds")
	}
	add := 0
	for sha, b := range c.Builds {
		if !isHexSHA256(sha) {
			return errorf(ErrInvalid, "build checksum %q is not a hex sha256", sha)
		}
		if _, ok := ParsePGVersion(b.Version); !ok || b.OS == "" || b.Arch == "" {
			return errorf(ErrInvalid, "build %s: version %q, os %q, arch %q", short(sha), b.Version, b.OS, b.Arch)
		}
		if old, ok := st.PGBuilds[sha]; ok {
			if old != b {
				return errorf(ErrInvalid, "build %s is already known as %s %s/%s", short(sha), old.Version, old.OS, old.Arch)
			}
			continue
		}
		add++
	}
	if len(st.PGBuilds)+add > MaxPGBuilds {
		return errorf(ErrInvalid, "at most %d PostgreSQL builds", MaxPGBuilds)
	}
	if st.PGBuilds == nil {
		st.PGBuilds = make(map[string]PGBuild, len(c.Builds))
	}
	for sha, b := range c.Builds {
		st.PGBuilds[sha] = b
	}
	return nil
}

func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// PGVersion is a parsed theseus-rs release: PostgreSQL major and minor,
// and the release's own revision of that build.
type PGVersion struct{ Major, Minor, Rev int }

// ParsePGVersion parses "18.7.0".
func ParsePGVersion(s string) (PGVersion, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return PGVersion{}, false
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return PGVersion{}, false
		}
		n[i] = v
	}
	if n[0] == 0 {
		return PGVersion{}, false
	}
	return PGVersion{n[0], n[1], n[2]}, true
}

// Compare is -1, 0 or 1 (major, then minor, then revision).
func (v PGVersion) Compare(o PGVersion) int {
	for _, d := range [3]int{v.Major - o.Major, v.Minor - o.Minor, v.Rev - o.Rev} {
		if d < 0 {
			return -1
		}
		if d > 0 {
			return 1
		}
	}
	return 0
}

// Short is "18.7" (the PostgreSQL version people know).
func (v PGVersion) Short() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// PGVersionOf is the pinned PostgreSQL version of a platform record: its
// build is in the cluster's table, for the record's own OS and processor.
func (s *State) PGVersionOf(p Platform) (PGVersion, bool) {
	b, ok := s.PGBuilds[p.PGBuildSHA]
	if !ok || b.OS != p.OS || b.Arch != p.Arch {
		return PGVersion{}, false
	}
	return ParsePGVersion(b.Version)
}

// SamePGLine reports whether p and o are two pinned builds of the same
// PostgreSQL major version on the same OS and processor (D70): their data
// files are the same format and one can copy the other's WAL, standby on
// the same or a newer minor. It also holds for two identical builds.
func (s *State) SamePGLine(p, o Platform) bool {
	if p.OS != o.OS || p.Arch != o.Arch {
		return false
	}
	pv, ok1 := s.PGVersionOf(p)
	ov, ok2 := s.PGVersionOf(o)
	return ok1 && ok2 && pv.Major == ov.Major
}

// ComparePG compares the pinned versions of two records on the same line
// (SamePGLine): -1 when p is older than o. 0 when either is not pinned.
func (s *State) ComparePG(p, o Platform) int {
	pv, ok1 := s.PGVersionOf(p)
	ov, ok2 := s.PGVersionOf(o)
	if !ok1 || !ok2 {
		return 0
	}
	return pv.Compare(ov)
}

// Compatible reports whether two data nodes' files can be shared without
// the mixed-system rules: alike (rule 1), or the same PostgreSQL line
// (D70). Which of them may copy which is NewerPG's business.
func (s *State) Compatible(p, o Platform) bool {
	return p.Alike(o) || s.SamePGLine(p, o)
}

// NewerPG reports whether p is a newer pinned build than o on the same
// line: a copy on o must not follow a main node on p (D70).
func (s *State) NewerPG(p, o Platform) bool {
	return !p.Alike(o) && s.SamePGLine(p, o) && s.ComparePG(p, o) > 0
}

// describePG is "PostgreSQL 18.7" for a pinned record, else the platform.
func (s *State) describePG(p Platform) string {
	if v, ok := s.PGVersionOf(p); ok {
		return "PostgreSQL " + v.Short()
	}
	return describePlatform(p)
}
