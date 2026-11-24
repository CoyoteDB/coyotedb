#!/bin/sh
# Runs inside a throwaway Linux container (as root, drops to a normal user).
set -eu
useradd -m tester >/dev/null 2>&1 || true
mkdir -p /work/step0 && cp /in/step0/*.sh /in/step0/*.sql /work/step0/ && cp /in/step0/pgdata-*.tar.gz /work/ 2>/dev/null || true
chown -R tester /work
su tester -c '
  set -eu
  cd /work
  if [ ! -d pg ]; then
    curl -fsSL -o pg.tgz https://github.com/theseus-rs/postgresql-binaries/releases/download/18.7.0/postgresql-18.7.0-x86_64-unknown-linux-gnu.tar.gz
    echo "53ca01952cb581145e4fb16b6b3b675da345a82879f350707b9f65863a0642f4  pg.tgz" | sha256sum -c - >/dev/null && echo "postgres build checksum ok"
    mkdir pg && tar -xzf pg.tgz -C pg --strip-components=1
  fi
  export PGBIN=/work/pg/bin WORK=/work
  sh step0/make.sh linux-x64 | tail -1
  for src in $(ls /work | sed -n "s/^pgdata-\(.*\)\.tar\.gz$/\1/p" | grep -v "^linux-x64$" | grep -v -- "-via-"); do
    sh step0/open.sh "$src" linux-x64 || echo "OPEN FAILED for $src (exit $?)"
  done
'
mkdir -p /out && cp /work/*.info /work/*.verify /out/ 2>/dev/null; cp /work/pgdata-linux-x64.tar.gz /work/pgdata-*-via-linux-x64.tar.gz /out/ 2>/dev/null || true
ls -la /out | awk '{print $5, $9}'
