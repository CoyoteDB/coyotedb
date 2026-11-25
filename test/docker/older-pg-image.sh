#!/usr/bin/env bash
# Builds a start image for TestDockerUpdate across a PostgreSQL minor (D70):
# the working tree as an older release would be, with an earlier pinned
# PostgreSQL (internal/pg/version.go earlierBuilds) as its only build.
# Nothing in the repository is changed: the build runs on a copy.
#
#   test/docker/older-pg-image.sh [VERSION] [TAG]
#   COYOTE_DOCKER_FROM=TAG go test -tags docker -count=1 -run TestDockerUpdate -v ./test/docker/
#
# VERSION defaults to the newest earlier pinned release (18.6.0), TAG to
# coyote-test/coyote:pg<digits of VERSION> (coyote-test/coyote:pg1860).
#
# theseus-rs 18.6.0 for Linux is linked against system libraries that
# 18.7.0 ships in its own lib folder (docs/TESTING.md), so the image adds
# them. Its ecpg, uuid-ossp and plpython parts still lack theirs; coyote
# uses none of them, so the image's library check skips those four only.
# Test use only: this image is never published.
set -euo pipefail

repo="$(cd "$(dirname "$0")/../.." && pwd)"
version="${1:-}"
if [ -z "$version" ]; then
	version="$(grep -oE '^[[:space:]]*\{"[0-9]+\.[0-9]+\.[0-9]+", map' "$repo/internal/pg/version.go" | head -1 | grep -oE '[0-9]+\.[0-9]+\.[0-9]+')"
fi
tag="${2:-coyote-test/coyote:pg${version//./}}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cd "$repo"
mkdir -p "$work/dashboard" "$work/docker"
cp -R go.mod go.sum cmd internal LICENSE NOTICE "$work/"
cp dashboard/embed.go "$work/dashboard/"
cp -R dashboard/dist "$work/dashboard/"
cp -R docker/pgfetch docker/extensions docker/Dockerfile docker/Dockerfile.dockerignore "$work/docker/"

python3 - "$work" "$version" <<'PY'
import re, sys
work, version = sys.argv[1], sys.argv[2]
p = work + '/internal/pg/version.go'
s = open(p).read()
m = re.search(r'\t\{"' + re.escape(version) + r'", map\[string\]string\{(.*?)\n\t\}\},\n', s, re.S)
if not m:
    sys.exit(version + ' is not an earlier pinned release in internal/pg/version.go')
s = re.sub(r'const Version = "[0-9.]+"', 'const Version = "' + version + '"', s)
s = re.sub(r'var pinnedSHA256 = map\[string\]string\{.*?\n\}\n',
           lambda _: 'var pinnedSHA256 = map[string]string{' + m.group(1).replace('\t\t"', '\t"') + '\n}\n', s, count=1, flags=re.S)
s = s.replace(m.group(0), '')  # an older release does not know newer builds
open(p, 'w').write(s)
d = work + '/docker/Dockerfile'
s = open(d).read()
s = s.replace('apt-get install -y --no-install-recommends tini',
              'apt-get install -y --no-install-recommends tini libgssapi-krb5-2 libxml2 libreadline8t64 libxslt1.1 libpq5')
s = s.replace("grep 'not found'; done",
              "grep 'not found' | grep -v -e libecpg -e libossp-uuid -e libpgtypes -e libpython; done")
open(d, 'w').write(s)
PY
gofmt -w "$work/internal/pg/version.go"
grep -q "const Version = \"$version\"" "$work/internal/pg/version.go"

cd "$work"
docker build -f docker/Dockerfile \
	--build-arg COYOTE_VERSION="0.0.0-pg${version}" --build-arg COYOTE_IMAGE="$tag" \
	-t "$tag" .
echo "built $tag (PostgreSQL $version)"
