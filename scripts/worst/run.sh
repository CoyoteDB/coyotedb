#!/bin/sh
# Run worst-case chaos scenarios (or the soak) on Linux, in a container
# that gives the faults what they need without root on the host:
#   - /disks/a, /disks/b: 1 GiB tmpfs volumes (disk full: WORST_DISKS)
#   - CAP_SYS_PTRACE: strace injects EROFS / fsync delays (disk read-only,
#     fsync stall)
#   - CAP_NET_ADMIN: tc netem on the container's own loopback (packet loss)
# Nothing outside the container changes. See docs/TESTING.md "Worst cases".
#
# Usage (on the Linux host, from the folder holding the tarball):
#   sh run.sh <tag> <tarball> '<CHAOS_ONLY regexp>' [KEY=VALUE ...]
#   sh run.sh soak  <tarball> '' SOAK_SEGMENTS=16 SOAK_SEGMENT=30m
# Output: ./out/<tag>.log (and ./out/<tag>-artifacts/ for failed runs,
# ./out/<tag>.jsonl for the soak). Container: coyote-test-worst-<tag>.
set -eu
TAG="$1"; TAR="$2"; ONLY="$3"; shift 3
W="$(pwd)"
mkdir -p "$W/out" "$W/pgcache" "$W/gocache" "$W/gopath"
rm -rf "$W/src-$TAG" && mkdir -p "$W/src-$TAG" && tar -xzf "$TAR" -C "$W/src-$TAG"
ENVS=""
for kv in "$@"; do ENVS="$ENVS -e $kv"; done
RUN='-run TestScenarios'
[ "$TAG" = soak ] || [ "${TAG#soak}" != "$TAG" ] && RUN='-run TestSoak'
docker rm -f "coyote-test-worst-$TAG" >/dev/null 2>&1 || true
# shellcheck disable=SC2086
docker run -d --name "coyote-test-worst-$TAG" \
  --cap-add NET_ADMIN --cap-add SYS_PTRACE --cap-add SYS_ADMIN --security-opt apparmor=unconfined \
  --mount type=tmpfs,destination=/disks/a,tmpfs-size=1073741824,tmpfs-mode=1777 \
  --mount type=tmpfs,destination=/disks/b,tmpfs-size=1073741824,tmpfs-mode=1777 \
  --shm-size=1g -v "$W:/w" \
  -e WORST_SUDO=1 -e WORST_DISKS=/disks -e CHAOS_ONLY="$ONLY" \
  -e COYOTE_PG_CACHE=/w/pgcache -e GOCACHE=/w/gocache -e GOPATH=/w/gopath \
  -e CHAOS_ARTIFACTS="/w/out/$TAG-artifacts" -e SOAK_OUT="/w/out/$TAG.jsonl" $ENVS \
  coyote-test-worst:2 sh -c "cd /w/src-$TAG/test/chaos && go test -tags 'chaos worst' -count=1 -timeout 1000m $RUN -v . > /w/out/$TAG.log 2>&1; echo EXIT=\$? >> /w/out/$TAG.log; echo DONE >> /w/out/$TAG.log"
echo "started coyote-test-worst-$TAG; log: $W/out/$TAG.log"
