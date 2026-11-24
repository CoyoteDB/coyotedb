#!/usr/bin/env bash
# Sets up a dev machine (macOS or Linux) with no admin rights.
# Installs Go and Node 22 into ~/.local, then the npm packages of the web
# folders (dashboard/, and site/ when present). Safe to re-run.
set -euo pipefail

GO_VERSION="go1.27.4"
NODE_MIN="22.23.4"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
case "$arch" in
  x86_64 | amd64) goarch="amd64"; nodearch="x64" ;;
  arm64 | aarch64) goarch="arm64"; nodearch="arm64" ;;
  *) echo "Unsupported CPU: $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin | linux) ;;
  *) echo "Unsupported OS: $os (on Windows use WSL)" >&2; exit 1 ;;
esac

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }
# true if version $1 >= version $2
ver_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" = "$2" ]; }

mkdir -p "$HOME/.local/bin"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# --- Go ---------------------------------------------------------------------
if [ "$("$HOME/.local/go/bin/go" version 2>/dev/null | awk '{print $3}')" = "$GO_VERSION" ]; then
  echo "Go $GO_VERSION already installed"
else
  file="$GO_VERSION.$os-$goarch.tar.gz"
  echo "Installing $GO_VERSION ..."
  curl -fsSL -o "$tmp/$file" "https://go.dev/dl/$file"
  want="$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' |
    python3 -c "import sys,json; print([f['sha256'] for r in json.load(sys.stdin) if r['version']=='$GO_VERSION' for f in r['files'] if f['filename']=='$file'][0])")"
  [ "$want" = "$(sha256 "$tmp/$file")" ] || { echo "Go checksum mismatch" >&2; exit 1; }
  rm -rf "$HOME/.local/go"
  tar -C "$HOME/.local" -xzf "$tmp/$file"
  ln -sf "$HOME/.local/go/bin/go" "$HOME/.local/bin/go"
  ln -sf "$HOME/.local/go/bin/gofmt" "$HOME/.local/bin/gofmt"
fi

# --- Node (for site/ and dashboard/) ----------------------------------------
node_bin=""
for cand in "$HOME/.local/node/bin/node" "$(command -v node || true)"; do
  [ -x "$cand" ] || continue
  if ver_ge "$("$cand" --version | sed 's/^v//')" "$NODE_MIN"; then node_bin="$cand"; break; fi
done
if [ -n "$node_bin" ]; then
  echo "Node OK: $("$node_bin" --version) at $node_bin"
else
  ver="$(curl -fsSL https://nodejs.org/dist/index.json |
    python3 -c "import sys,json; print([r['version'] for r in json.load(sys.stdin) if r['version'].startswith('v22.')][0])")"
  file="node-$ver-$os-$nodearch.tar.gz"
  echo "Installing Node $ver ..."
  curl -fsSL -o "$tmp/$file" "https://nodejs.org/dist/$ver/$file"
  want="$(curl -fsSL "https://nodejs.org/dist/$ver/SHASUMS256.txt" | grep " $file\$" | cut -d' ' -f1)"
  [ -n "$want" ] && [ "$want" = "$(sha256 "$tmp/$file")" ] || { echo "Node checksum mismatch" >&2; exit 1; }
  rm -rf "$HOME/.local/node"
  mkdir -p "$HOME/.local/node"
  tar -C "$HOME/.local/node" --strip-components=1 -xzf "$tmp/$file"
  node_bin="$HOME/.local/node/bin/node"
fi
node_dir="$(dirname "$node_bin")"

# --- Web deps ---------------------------------------------------------------
repo="$(cd "$(dirname "$0")/.." && pwd)"
for web in site dashboard; do
  if [ -f "$repo/$web/package.json" ]; then
    echo "Installing $web/ packages ..."
    (cd "$repo/$web" && PATH="$node_dir:$PATH" npm install --no-fund --no-audit)
  fi
done

case ":$PATH:" in
  *":$HOME/.local/bin:"*) ;;
  *) echo; echo "Add this to your shell profile:  export PATH=\"\$HOME/.local/bin:\$PATH\"" ;;
esac
echo
echo "Done. Next: make check"
