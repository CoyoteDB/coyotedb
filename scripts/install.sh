#!/bin/sh
# CoyoteDB installer for macOS and Linux (POSIX sh).
#
#   COYOTE_RELEASE_BASE=https://<release host>/<version> sh install.sh [--dry-run] [--no-service]
#
# What it does, in order:
#   1. works out this machine's system and processor (darwin/linux, amd64/arm64)
#   2. downloads  $COYOTE_RELEASE_BASE/coyote-<os>-<arch>  and  $COYOTE_RELEASE_BASE/SHA256SUMS
#   3. checks the program's sha256 against SHA256SUMS and stops if it differs
#   4. asks for administrator rights (sudo) only to copy it to /usr/local/bin
#      and to run `coyote service install` (the background service)
#
# Nothing is downloaded from a built-in address: there is no public release
# yet, so COYOTE_RELEASE_BASE must be set (or passed with --base).
#
# Options:
#   --dry-run       print what would be done, change nothing
#   --no-service    install the program only, not the background service
#   --base URL      same as COYOTE_RELEASE_BASE
#   --prefix DIR    install into DIR instead of /usr/local/bin (COYOTE_INSTALL_DIR)
#   -h, --help      this help

set -eu

# ---- settings -------------------------------------------------------------
# Release location. Deliberately no default: set it to the folder that holds
# coyote-<os>-<arch> and SHA256SUMS (https:// or, for testing, file://).
RELEASE_BASE="${COYOTE_RELEASE_BASE:-}"
INSTALL_DIR="${COYOTE_INSTALL_DIR:-/usr/local/bin}"
PROGRAM="coyote"
NEXT_STEP="Next: run 'coyote up' to start a new cluster on this machine, or 'coyote join <invite code>' to join one you already have."

DRY_RUN=0
NO_SERVICE=0

say() { printf '%s\n' "$*"; }
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
Usage: COYOTE_RELEASE_BASE=https://<release host>/<version> sh install.sh [options]

  --dry-run       print what would be done, change nothing
  --no-service    install the program only, not the background service
  --base URL      same as COYOTE_RELEASE_BASE
  --prefix DIR    install into DIR instead of /usr/local/bin (COYOTE_INSTALL_DIR)
  -h, --help      this help
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --no-service) NO_SERVICE=1 ;;
    --base) [ $# -ge 2 ] || fail "--base needs a URL"; RELEASE_BASE="$2"; shift ;;
    --base=*) RELEASE_BASE="${1#--base=}" ;;
    --prefix) [ $# -ge 2 ] || fail "--prefix needs a folder"; INSTALL_DIR="$2"; shift ;;
    --prefix=*) INSTALL_DIR="${1#--prefix=}" ;;
    -h | --help) usage; exit 0 ;;
    *) fail "unknown option: $1 (try --help)" ;;
  esac
  shift
done

# run prints the command in dry-run mode, otherwise runs it.
run() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '  would run:'
    printf ' %s' "$@"
    printf '\n'
  else
    "$@"
  fi
}

# ---- what to download ------------------------------------------------------
[ -n "$RELEASE_BASE" ] || fail "COYOTE_RELEASE_BASE is not set.
There is no public CoyoteDB release yet, so this script has no download address built in.
Set it to the folder that holds the release files, for example:
    COYOTE_RELEASE_BASE=https://example.invalid/coyote/1.0.0 sh install.sh"
case "$RELEASE_BASE" in
  https://* | file://*) ;;
  *) fail "COYOTE_RELEASE_BASE must start with https:// (got: $RELEASE_BASE)" ;;
esac
RELEASE_BASE="${RELEASE_BASE%/}"
case "$INSTALL_DIR" in
  /*) ;;
  *) fail "install folder must be an absolute path (got: $INSTALL_DIR)" ;;
esac

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "this installer is for macOS and Linux; on Windows use install.ps1" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported processor: $(uname -m) (CoyoteDB runs on 64-bit Intel/AMD and ARM)" ;;
esac
# A Terminal running under Rosetta reports x86_64 on Apple silicon.
if [ "$os" = darwin ] && [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
  arch=arm64
fi
asset="$PROGRAM-$os-$arch"

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL --proto '=https,file' --tlsv1.2 --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  case "$RELEASE_BASE" in file://*) fail "wget cannot read file:// addresses; install curl" ;; esac
  fetch() { wget -q --https-only -O "$2" "$1"; }
else
  fail "curl or wget is needed to download CoyoteDB"
fi

if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  fail "sha256sum or shasum is needed to check the download"
fi

# Administrator rights: only for the copy into $INSTALL_DIR and the service.
if [ "$(id -u)" -eq 0 ]; then
  as_root() { run "$@"; }
elif command -v sudo >/dev/null 2>&1; then
  as_root() { run sudo "$@"; }
elif command -v doas >/dev/null 2>&1; then
  as_root() { run doas "$@"; }
else
  as_root() { fail "administrator rights are needed (no sudo or doas found); run this script as root"; }
fi

say "Installing CoyoteDB for $os/$arch"
say "  from: $RELEASE_BASE/$asset"
say "  to:   $INSTALL_DIR/$PROGRAM"
[ "$DRY_RUN" -eq 1 ] && say "  (dry run: nothing will be changed)"

# ---- download + verify -----------------------------------------------------
tmp="$(mktemp -d 2>/dev/null || mktemp -d -t coyote)"
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' HUP INT TERM

if [ "$DRY_RUN" -eq 1 ]; then
  say "  would download $RELEASE_BASE/$asset and $RELEASE_BASE/SHA256SUMS"
  say "  would check the sha256 of $asset against SHA256SUMS and stop on a mismatch"
else
  fetch "$RELEASE_BASE/SHA256SUMS" "$tmp/SHA256SUMS" || fail "could not download $RELEASE_BASE/SHA256SUMS"
  fetch "$RELEASE_BASE/$asset" "$tmp/$asset" || fail "could not download $RELEASE_BASE/$asset"
  # Lines look like "<sha256>  <file>" (or "<sha256> *<file>").
  want="$(awk -v f="$asset" '$2 == f || $2 == "*" f { print tolower($1); exit }' "$tmp/SHA256SUMS")"
  [ -n "$want" ] || fail "SHA256SUMS has no entry for $asset; refusing to install"
  case "$want" in
    *[!0-9a-f]* | "") fail "SHA256SUMS entry for $asset is malformed; refusing to install" ;;
  esac
  [ "${#want}" -eq 64 ] || fail "SHA256SUMS entry for $asset is malformed; refusing to install"
  got="$(sha256 "$tmp/$asset")"
  if [ "$got" != "$want" ]; then
    fail "the downloaded program does not match its checksum; refusing to install.
  expected: $want
  got:      $got
The download may be damaged or tampered with. Nothing was installed."
  fi
  say "Checksum OK ($want)"
  chmod 0755 "$tmp/$asset"
fi

# ---- install (needs administrator rights) ----------------------------------
say ""
if [ -d "$INSTALL_DIR" ] && [ -w "$INSTALL_DIR" ]; then
  copy_as() { run "$@"; }
  copy_needs_root=0
else
  copy_as() { as_root "$@"; }
  copy_needs_root=1
fi
if [ "$(id -u)" -ne 0 ]; then
  if [ "$NO_SERVICE" -eq 0 ]; then
    say "Administrator rights are needed to set CoyoteDB up as a background service"
    say "(so it keeps running after you log out and starts with the computer)."
    [ "$copy_needs_root" -eq 1 ] && say "They are also needed to copy coyote into $INSTALL_DIR."
    say "You may be asked for your password."
  elif [ "$copy_needs_root" -eq 1 ]; then
    say "Administrator rights are needed to copy coyote into $INSTALL_DIR. You may be asked for your password."
  fi
fi
# Upgrading while the service runs: stop it (the database shuts down
# cleanly), replace the program, start it again. `coyote service status`
# exits 0 only while the service is running.
was_running=0
if [ -x "$INSTALL_DIR/$PROGRAM" ] && "$INSTALL_DIR/$PROGRAM" service status >/dev/null 2>&1; then
  was_running=1
  say "The CoyoteDB service is running; it will be stopped for the upgrade and started again."
  as_root "$INSTALL_DIR/$PROGRAM" service stop
fi
copy_as mkdir -p "$INSTALL_DIR"
copy_as install -m 0755 "$tmp/$asset" "$INSTALL_DIR/$PROGRAM"
[ "$DRY_RUN" -eq 1 ] || say "Installed $INSTALL_DIR/$PROGRAM"

if [ "$NO_SERVICE" -eq 0 ]; then
  as_root "$INSTALL_DIR/$PROGRAM" service install
fi
if [ "$was_running" -eq 1 ]; then
  as_root "$INSTALL_DIR/$PROGRAM" service start
fi

say ""
if [ "$DRY_RUN" -eq 1 ]; then
  say "Dry run finished; nothing was changed."
  exit 0
fi
say "Done. CoyoteDB is installed."
case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *) say "Note: $INSTALL_DIR is not on your PATH; run it as $INSTALL_DIR/$PROGRAM." ;;
esac
say "$NEXT_STEP"
