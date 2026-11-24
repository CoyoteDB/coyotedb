#!/bin/sh
# Run the pgvector integration tests (test/integration/ext_test.go) on a
# Linux machine with Docker, inside the linux builder image
# (scripts/extensions/Dockerfile), as a non-root user (initdb refuses root).
# Run from the repository root:
#
#   scripts/extensions/itest-linux.sh EXT_DIR PG_CACHE LOG
#
# EXT_DIR holds the Linux pgvector archives (build-linux.sh), PG_CACHE a
# coyote PostgreSQL cache with the pinned Linux tree (build-linux.sh leaves
# one in OUT_DIR/pgcache). Both are copied; nothing outside the container
# changes but LOG. The container is named coyote-test-ext-itest.
set -eu
[ $# -eq 3 ] || { sed -n 2,12p "$0" >&2; exit 2; }
ext=$(cd "$1" && pwd)
cache=$(cd "$2" && pwd)
log=$3
docker build -q --target linux -t coyote-ext-builder:linux -f scripts/extensions/Dockerfile scripts/extensions >/dev/null
docker run --rm --name coyote-test-ext-itest -e U="$(id -u)" \
	-v "$PWD":/src:ro -v "$ext":/ext:ro -v "$cache":/pgcache:ro coyote-ext-builder:linux sh -euc '
		useradd -u "$U" -m tester
		mkdir /work && cp -R /src /work/src && cp -R /pgcache /work/pgcache && rm -f /work/pgcache/*.tar.gz*
		chown -R tester /work
		su tester -c "cd /work/src && export PATH=/usr/local/go/bin:\$PATH HOME=/home/tester GOFLAGS=-buildvcs=false COYOTE_PG_CACHE=/work/pgcache COYOTE_TEST_EXT_DIR=/ext && go test -tags \"integration ext\" -count=1 -timeout 40m -run TestExt -v ./test/integration/"
	' >"$log" 2>&1
