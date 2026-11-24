# CoyoteDB node image: `coyote` plus the pinned PostgreSQL 18 build, on
# Debian 13 slim. One image for data nodes and witnesses. Design: D46 in
# docs/DECISIONS.md; how to run it: docker/README.md.
#
# Build from the repository root (the context must be the root):
#
#   docker build -f docker/Dockerfile -t coyotedb/coyote:local .
#   docker buildx build -f docker/Dockerfile --platform linux/amd64,linux/arm64 .
#
# With BuildKit (Docker Desktop, Docker CE) the Go and PostgreSQL stages run
# natively on the build machine and only the last stage runs as the target
# processor. The classic builder (Ubuntu's docker.io without docker-buildx)
# has no BUILDPLATFORM/TARGETARCH; it then builds for the machine it runs on.

# Base images, pinned by digest (checked 2026-11-21). Update tag and digest
# together.
ARG GO_IMAGE=golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d
ARG BASE_IMAGE=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2

# ---- 1. coyote and pgfetch, cross-compiled on the build machine --------
FROM --platform=${BUILDPLATFORM:-linux/amd64} ${GO_IMAGE} AS build
ARG TARGETARCH
ARG COYOTE_VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
COPY dashboard/embed.go ./dashboard/embed.go
COPY dashboard/dist ./dashboard/dist
COPY docker/pgfetch ./docker/pgfetch
# No cgo anywhere in coyote: one static binary, no libc dependency.
RUN arch="${TARGETARCH:-$(go env GOARCH)}" \
 && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath \
      -ldflags "-s -w -X main.version=${COYOTE_VERSION}" -o /out/coyote ./cmd/coyote \
 && go build -trimpath -o /out/pgfetch ./docker/pgfetch \
 && echo "$arch" > /out/arch

# ---- 2. PostgreSQL, fetched and verified by coyote's own code ----------
# pgfetch runs on the build machine and fetches the archive for the TARGET
# processor, checked against the sha256 pinned in internal/pg/version.go.
# Extensions (pgvector, D53): the archives in docker/extensions (built by
# scripts/extensions, checked against the sha256 pinned in
# internal/pg/ext.go) are installed into the same tree; with none there the
# image has no pgvector and pgfetch says so.
FROM build AS postgres
COPY docker/extensions /ext
RUN /out/pgfetch -arch "$(cat /out/arch)" -cache /out/pg -extensions-from /ext > /out/pg-root \
 && ln -s "$(basename "$(cat /out/pg-root)")" /out/pg/current \
 && rm -f /out/pg/.lock

# ---- 3. The image ------------------------------------------------------
FROM ${BASE_IMAGE}
ARG COYOTE_VERSION=dev

# tini is PID 1: it forwards `docker stop`'s SIGTERM to coyote and reaps
# processes that lose their parent (Postgres children of a killed
# postmaster, a watchdog outliving its agent). The PostgreSQL build needs
# nothing else from the system (checked with ldd, D46).
# hadolint ignore=DL3008
RUN apt-get update \
 && apt-get install -y --no-install-recommends tini \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --system --gid 10001 coyote \
 && useradd --system --uid 10001 --gid coyote --home-dir /data --no-create-home \
      --shell /usr/sbin/nologin coyote \
 && mkdir -p /data /invites \
 && chown coyote:coyote /data /invites \
 && chmod 0700 /data /invites

# /invites: only for docker-compose.yml, which hands each joining node its
# one-time invite code through a small shared volume mounted there.

COPY --from=build /out/coyote /usr/local/bin/coyote
# Root-owned and read-only for the coyote user: the program files cannot be
# changed by the database. coyote only reads them (it re-checks sizes on
# every start and every file's sha256 after a crash).
COPY --from=postgres /out/pg /opt/coyote/pg
# The licence and the third-party notices travel with the program.
COPY LICENSE NOTICE /usr/share/doc/coyote/

# /data/coyote is the node's data folder: Linux's per-user default
# ($XDG_DATA_HOME/coyote), so `docker exec <c> coyote status` and every
# other command find the node without --data-dir.
ENV XDG_DATA_HOME=/data \
    HOME=/data \
    COYOTE_PG_CACHE=/opt/coyote/pg \
    PATH=/usr/local/bin:/opt/coyote/pg/current/bin:/usr/sbin:/usr/bin:/sbin:/bin

LABEL org.opencontainers.image.title="CoyoteDB" \
      org.opencontainers.image.description="PostgreSQL 18 on one or more machines, with automatic failover" \
      org.opencontainers.image.version="${COYOTE_VERSION}" \
      org.opencontainers.image.licenses="Apache-2.0"

VOLUME ["/data"]
# 7481: cluster port (node to node, mutual TLS). 5432: client port (TLS).
EXPOSE 7481 5432
# coyote stops Postgres cleanly on SIGTERM (fast shutdown, up to 30 s, then
# immediate). Give it time: `docker run --stop-timeout 120`, compose
# `stop_grace_period: 2m`. Docker's default of 10 s is too short.
STOPSIGNAL SIGTERM
USER coyote
WORKDIR /data
HEALTHCHECK --interval=30s --timeout=20s --start-period=5m --retries=3 \
  CMD ["/usr/local/bin/coyote", "status", "--json"]
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/coyote"]
CMD ["up"]
