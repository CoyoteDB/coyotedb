# Builder images for the Linux and Windows extension archives (D53).
#
# Two targets: `linux` and `windows` (docker build --target).
# Debian 12 (bookworm), like the pinned Linux PostgreSQL builds (built on
# Debian 12 with gcc 12.2, glibc 2.36): an extension built here needs
# nothing newer than PostgreSQL itself does. Go comes with the base image
# (extbuild fetches the trees and packs the archives).
#
#   linux:   gcc 12 (x86_64), gcc-aarch64-linux-gnu 12 (arm64, cross).
#   windows: Debian 13 (trixie; bookworm has no UCRT MinGW), MinGW-w64
#            against the UCRT, the C runtime of the MSVC PostgreSQL build.
#            glibc does not matter for a Windows DLL.
#
# Built and run by scripts/extensions/build-linux.sh.
FROM golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS linux
# hadolint ignore=DL3008
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc-aarch64-linux-gnu libc6-dev-arm64-cross file \
 && rm -rf /var/lib/apt/lists/*

FROM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS windows
# hadolint ignore=DL3008
RUN apt-get update \
 && apt-get install -y --no-install-recommends gcc-mingw-w64-ucrt64 binutils-mingw-w64-x86-64 file \
 && rm -rf /var/lib/apt/lists/*
