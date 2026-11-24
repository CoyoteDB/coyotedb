#!/bin/sh
# Build the Linux (x86_64, arm64) and Windows (x64) pgvector archives in the
# builder images (scripts/extensions/Dockerfile), on any Linux machine with
# Docker. Run from the repository root:
#
#   scripts/extensions/build-linux.sh OUT_DIR
#
# The PostgreSQL trees are fetched and checked by coyote's own code
# (extbuild pgtree, pinned sha256) into OUT_DIR/pgcache. Archives land in
# OUT_DIR; each one's sha256 is printed. The containers are named
# coyote-test-ext-build-* and removed afterwards.
set -eu
[ $# -eq 1 ] || { sed -n 2,11p "$0" >&2; exit 2; }
mkdir -p "$1"
out=$(cd "$1" && pwd)
for t in linux windows; do
	docker build -q --target "$t" -t "coyote-ext-builder:$t" -f scripts/extensions/Dockerfile scripts/extensions >/dev/null
done

# run IMAGE SCRIPT: SCRIPT runs in a copy of the repository, with tree
# GOOS GOARCH printing a verified PostgreSQL tree.
run() {
	docker run --rm --name "coyote-test-ext-build-$1" \
		--user "$(id -u):$(id -g)" -e HOME=/tmp -e GOCACHE=/tmp/gocache -e GOPATH=/tmp/go -e GOFLAGS=-buildvcs=false \
		-v "$PWD":/src:ro -v "$out":/out -w /tmp "coyote-ext-builder:$1" sh -euc '
		cp -R /src /tmp/repo && cd /tmp/repo
		tree() { go run ./scripts/extensions/extbuild pgtree -goos "$1" -goarch "$2" -cache /out/pgcache; }
		'"$2"
}

# shellcheck disable=SC2016
run linux '
	amd64=$(tree linux amd64)
	arm64=$(tree linux arm64)
	echo "== x86_64-unknown-linux-gnu"
	sh scripts/extensions/build.sh x86_64-unknown-linux-gnu "$amd64" /out
	echo "== aarch64-unknown-linux-gnu"
	HOST_PG_ROOT=$amd64 sh scripts/extensions/build.sh aarch64-unknown-linux-gnu "$arm64" /out
'
# shellcheck disable=SC2016
run windows '
	win=$(tree windows amd64)
	echo "== x86_64-pc-windows-msvc"
	sh scripts/extensions/build.sh x86_64-pc-windows-msvc "$win" /out
'
