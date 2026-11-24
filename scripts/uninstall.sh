#!/bin/sh
# CoyoteDB uninstaller for macOS and Linux (POSIX sh).
#
#   sh uninstall.sh [--dry-run] [--purge] [--yes]
#
# Stops and removes the background service, then removes the program.
# Your data is kept unless you pass --purge (which deletes the database
# for good and asks you to confirm first, unless --yes is given).

set -eu

INSTALL_DIR="${COYOTE_INSTALL_DIR:-/usr/local/bin}"
PROGRAM="coyote"
DRY_RUN=0
PURGE=0
YES=0

say() { printf '%s\n' "$*"; }
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
Usage: sh uninstall.sh [options]

  --dry-run       print what would be done, change nothing
  --purge         also delete the data folder (the database itself!) and the service user
  --yes           do not ask before --purge
  --prefix DIR    the program is in DIR instead of /usr/local/bin (COYOTE_INSTALL_DIR)
  -h, --help      this help
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --purge) PURGE=1 ;;
    --yes | -y) YES=1 ;;
    --prefix) [ $# -ge 2 ] || fail "--prefix needs a folder"; INSTALL_DIR="$2"; shift ;;
    --prefix=*) INSTALL_DIR="${1#--prefix=}" ;;
    -h | --help) usage; exit 0 ;;
    *) fail "unknown option: $1 (try --help)" ;;
  esac
  shift
done

run() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf '  would run:'
    printf ' %s' "$@"
    printf '\n'
  else
    "$@"
  fi
}

if [ "$(id -u)" -eq 0 ]; then
  as_root() { run "$@"; }
elif command -v sudo >/dev/null 2>&1; then
  as_root() { run sudo "$@"; }
elif command -v doas >/dev/null 2>&1; then
  as_root() { run doas "$@"; }
else
  as_root() { fail "administrator rights are needed (no sudo or doas found); run this script as root"; }
fi

bin="$INSTALL_DIR/$PROGRAM"

if [ "$PURGE" -eq 1 ] && [ "$YES" -eq 0 ] && [ "$DRY_RUN" -eq 0 ]; then
  say "--purge deletes this machine's copy of the database for good."
  say "If this is the only copy, the data is gone. Type 'delete' to continue:"
  read -r answer || answer=""
  [ "$answer" = "delete" ] || fail "not confirmed; nothing was changed"
fi

if [ "$(id -u)" -ne 0 ]; then
  say "Administrator rights are needed to stop and remove the background service and"
  say "to remove $bin. You may be asked for your password."
fi

if [ -x "$bin" ] || [ "$DRY_RUN" -eq 1 ]; then
  if [ "$PURGE" -eq 1 ]; then
    as_root "$bin" service uninstall --purge
  else
    as_root "$bin" service uninstall
  fi
else
  say "$bin not found; skipping the service step (if the service is still installed,"
  say "run '<path to coyote> service uninstall' first)."
fi

if [ -e "$bin" ] || [ "$DRY_RUN" -eq 1 ]; then
  as_root rm -f "$bin"
  [ "$DRY_RUN" -eq 1 ] || say "Removed $bin"
fi

say ""
if [ "$DRY_RUN" -eq 1 ]; then
  say "Dry run finished; nothing was changed."
elif [ "$PURGE" -eq 1 ]; then
  say "Done. CoyoteDB and its data were removed from this machine."
else
  say "Done. CoyoteDB was removed; your data was kept (see the message above for where)."
fi
