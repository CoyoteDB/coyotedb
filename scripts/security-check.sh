#!/usr/bin/env bash
# Security and freshness check for a CoyoteDB release (SECURITY.md, D69).
# Run it before every release, and whenever PostgreSQL or Go announce a
# release. It changes nothing; it says what is current and what to update.
#
#   scripts/security-check.sh
#
# It checks:
#   1. the pinned PostgreSQL (internal/pg/version.go) against the newest
#      minor release of the same major version (postgresql.org), and
#      whether our binary source (theseus-rs/postgresql-binaries) already
#      has that version for every platform we ship;
#   2. known vulnerabilities in the Go code and the Go standard library it
#      uses (govulncheck; installed into the Go bin folder if missing, never
#      added to go.mod);
#   3. the Go version (go.mod, the Dockerfile's Go image) against the
#      newest Go patch release;
#   4. the Dockerfile's pinned base images (Go build image, Debian slim)
#      against what their tags point to today;
#   5. npm audit of the dashboard's production packages.
#
# Needs: curl, go; docker (with buildx) for check 4; npm for check 5.
# On macOS, Docker Desktop's tools are found even when not on the PATH.
#
# Exit code: 0 all OK, 1 something needs an update, 2 something could not
# be checked (and nothing needs an update).
set -uo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root" || exit 2

if ! command -v docker >/dev/null 2>&1 && [ -x /Applications/Docker.app/Contents/Resources/bin/docker ]; then
  PATH="/Applications/Docker.app/Contents/Resources/bin:$PATH"
fi

need_update=0
not_checked=0
ok()     { printf 'OK             %s\n' "$1"; }
update() { printf 'UPDATE NEEDED  %s\n' "$1"; printf '               What to do: %s\n' "$2"; need_update=1; }
skip()   { printf 'NOT CHECKED    %s\n' "$1"; not_checked=1; }
note()   { printf '               %s\n' "$1"; }
fetch()  { curl -fsSL --max-time 30 -H 'User-Agent: coyote-security-check' "$@"; }

echo "CoyoteDB security check ($(date -u +%Y-%m-%d))"
echo

# ---- 1. PostgreSQL ----------------------------------------------------------
pinned="$(sed -n 's/^const Version = "\([0-9.]*\)".*/\1/p' internal/pg/version.go)"
major="$(sed -n 's/^const MajorVersion = "\([0-9]*\)".*/\1/p' internal/pg/version.go)"
pinned_minor="$(echo "$pinned" | cut -d. -f2)"
if [ -z "$pinned" ] || [ -z "$major" ]; then
  skip "PostgreSQL: could not read the pinned version from internal/pg/version.go"
elif ! versions="$(fetch https://www.postgresql.org/versions.json)"; then
  skip "PostgreSQL $major.$pinned_minor: could not reach postgresql.org/versions.json"
else
  entry="$(echo "$versions" | tr '{' '\n' | grep "\"major\": *\"$major\"" | head -1)"
  latest="$(echo "$entry" | sed -n 's/.*"latestMinor": *"\([0-9]*\)".*/\1/p')"
  reldate="$(echo "$entry" | sed -n 's/.*"relDate": *"\([0-9-]*\)".*/\1/p')"
  newest_major="$(echo "$versions" | tr '{' '\n' | grep '"current": *true' | sed -n 's/.*"major": *"\([0-9]*\)".*/\1/p' | head -1)"
  if [ -z "$latest" ]; then
    skip "PostgreSQL $major: not found in postgresql.org/versions.json"
  elif [ "$pinned_minor" -ge "$latest" ]; then
    ok "PostgreSQL $major.$pinned_minor is the newest $major.x release ($major.$latest, released $reldate)."
  else
    want="$major.$latest.0"
    rel="$(fetch "https://api.github.com/repos/theseus-rs/postgresql-binaries/releases/tags/$want" 2>/dev/null || true)"
    missing=""
    for t in aarch64-apple-darwin x86_64-apple-darwin x86_64-unknown-linux-gnu aarch64-unknown-linux-gnu x86_64-pc-windows-msvc; do
      echo "$rel" | grep -q "\"postgresql-$want-$t.tar.gz\"" || missing="$missing $t"
    done
    if [ -z "$rel" ]; then
      update "PostgreSQL $major.$pinned_minor is pinned; $major.$latest is out (released $reldate) but our binary source has no $want release yet." \
        "watch https://github.com/theseus-rs/postgresql-binaries/releases for $want, then update internal/pg/version.go (see SECURITY.md)."
    elif [ -n "$missing" ]; then
      update "PostgreSQL $major.$pinned_minor is pinned; $major.$latest is out (released $reldate); our binary source has $want but not yet for:$missing." \
        "wait for those builds, then update internal/pg/version.go (see SECURITY.md)."
    else
      update "PostgreSQL $major.$pinned_minor is pinned; $major.$latest is out (released $reldate) and our binary source has $want for every platform." \
        "set Version = \"$want\" in internal/pg/version.go, pin the 5 new sha256 sums (from the release's .sha256 files, then re-hash each download), rebuild the pgvector archives (scripts/extensions), run the tests and make a patch release."
    fi
  fi
  if [ -n "$newest_major" ] && [ "$newest_major" != "$major" ]; then
    note "Note: PostgreSQL $newest_major is the newest major version. CoyoteDB supports $major only (moving to a new major version is a separate project, not a patch release)."
  fi
fi

# ---- 2. govulncheck ---------------------------------------------------------
gobin="$(go env GOBIN 2>/dev/null)"
[ -n "$gobin" ] || gobin="$(go env GOPATH 2>/dev/null)/bin"
vc="$gobin/govulncheck"
if ! command -v go >/dev/null 2>&1; then
  skip "Go vulnerabilities: go is not installed (scripts/dev-bootstrap.sh)"
else
  if [ ! -x "$vc" ]; then
    echo "(installing govulncheck into $gobin; go.mod is not touched)"
    # `go install pkg@version` builds outside this module: no go.mod change.
    if ! (cd "${TMPDIR:-/tmp}" && GOFLAGS= go install golang.org/x/vuln/cmd/govulncheck@latest) >/dev/null 2>&1; then
      vc=""
    fi
  fi
  if [ -z "$vc" ]; then
    skip "Go vulnerabilities: could not install govulncheck"
  else
    out="$("$vc" ./... 2>&1)"
    code=$?
    if [ $code -eq 0 ]; then
      ok "Go vulnerabilities: govulncheck finds none that our code reaches (checked with $(go env GOVERSION))."
    elif [ $code -eq 3 ]; then
      update "Go vulnerabilities: govulncheck finds $(echo "$out" | grep -c '^Vulnerability #' | tr -d ' ') that our code reaches:" \
        "for the standard library: a Go patch release (go.mod, the Dockerfile's Go image, scripts/dev-bootstrap.sh); for a module: go get MODULE@FIXED-VERSION. Details: $vc ./..."
      echo "$out" | grep -E '^Vulnerability #|Found in:|Fixed in:' | sed 's/^/               /'
    else
      skip "Go vulnerabilities: govulncheck failed to run (exit $code): $(echo "$out" | tail -1)"
    fi
  fi
fi

# ---- 3. Go version ----------------------------------------------------------
gomod="$(sed -n 's/^go \([0-9.]*\)$/\1/p' go.mod)"
goimg="$(sed -n 's/^ARG GO_IMAGE=golang:\([0-9.]*\)-.*/\1/p' docker/Dockerfile)"
boot="$(sed -n 's/^GO_VERSION="go\([0-9.]*\)"$/\1/p' scripts/dev-bootstrap.sh)"
if ! dl="$(fetch 'https://go.dev/dl/?mode=json')"; then
  skip "Go version: could not reach go.dev/dl"
else
  stable="$(echo "$dl" | sed -n 's/^ *"version": *"go\([0-9.]*\)",*$/\1/p' | awk '!seen[$0]++')"
  line="$(echo "$gomod" | cut -d. -f1-2)"
  newest_patch="$(echo "$stable" | grep "^$line\." | sort -V | tail -1)"
  newest="$(echo "$stable" | sort -V | tail -1)"
  if [ -z "$newest_patch" ]; then
    update "Go $gomod: Go $line is no longer supported by the Go team (newest: $newest)." \
      "move to Go $newest (go.mod, Dockerfile GO_IMAGE tag and digest, scripts/dev-bootstrap.sh)."
  else
    bad=""
    for v in "go.mod=$gomod" "Dockerfile=$goimg" "dev-bootstrap=$boot"; do
      [ "${v#*=}" = "$newest_patch" ] || bad="$bad ${v%%=*} has ${v#*=};"
    done
    if [ -z "$bad" ]; then
      ok "Go $gomod is the newest Go $line patch release (go.mod, Dockerfile, dev-bootstrap agree)."
    else
      update "Go: the newest $line patch release is $newest_patch;$bad" \
        "set $newest_patch in go.mod, the Dockerfile's GO_IMAGE (tag and digest) and scripts/dev-bootstrap.sh."
    fi
    if [ "$newest" != "$newest_patch" ]; then
      note "Note: Go $newest is out; Go $line is still supported. Move at a planned release."
    fi
  fi
fi

# ---- 4. Base images ---------------------------------------------------------
if ! command -v docker >/dev/null 2>&1 || ! docker buildx version >/dev/null 2>&1; then
  skip "Base images: docker with buildx is not available here"
else
  for arg in GO_IMAGE BASE_IMAGE; do
    ref="$(sed -n "s/^ARG $arg=\(.*\)$/\1/p" docker/Dockerfile)"
    tag="${ref%@*}"
    pinned_digest="${ref#*@}"
    if ! insp="$(docker buildx imagetools inspect "$tag" 2>&1)"; then
      skip "Base image $tag: could not look it up ($(echo "$insp" | tail -1))"
      continue
    fi
    cur="$(echo "$insp" | sed -n 's/^Digest: *\(sha256:[0-9a-f]*\).*/\1/p' | head -1)"
    if [ "$cur" = "$pinned_digest" ]; then
      ok "Base image $tag: the pinned digest is what the tag points to today."
    else
      update "Base image $tag: the tag now points to $cur (pinned: $pinned_digest). Upstream rebuilt it, usually for security fixes." \
        "set the new digest in docker/Dockerfile ($arg), rebuild, run the Docker tests (docs/TESTING.md)."
    fi
  done
  if [ -n "${newest_patch:-}" ] && [ -n "$goimg" ] && [ "$goimg" != "$newest_patch" ]; then
    note "Note: the Go image tag should become golang:$newest_patch-trixie (see the Go check above)."
  fi
fi

# ---- 5. Dashboard packages --------------------------------------------------
if ! command -v npm >/dev/null 2>&1; then
  skip "Dashboard packages: npm is not installed (scripts/dev-bootstrap.sh)"
elif [ ! -f dashboard/package-lock.json ]; then
  skip "Dashboard packages: dashboard/package-lock.json is missing"
else
  out="$(cd dashboard && npm audit --omit=dev 2>&1)"
  code=$?
  if [ $code -eq 0 ]; then
    ok "Dashboard packages: npm audit finds no known vulnerability in the production packages."
  elif echo "$out" | grep -q 'vulnerabilit'; then
    update "Dashboard packages: npm audit reports: $(echo "$out" | grep -i 'vulnerabilit' | tail -1)" \
      "cd dashboard && npm audit --omit=dev; update the named packages, rebuild (npm run build), run the dashboard tests."
  else
    skip "Dashboard packages: npm audit could not run: $(echo "$out" | tail -1)"
  fi
fi

echo
if [ $need_update -eq 1 ]; then
  echo "Result: UPDATE NEEDED (see above)."
  exit 1
elif [ $not_checked -eq 1 ]; then
  echo "Result: everything checked is current, but some checks could not run."
  exit 2
fi
echo "Result: all OK."
