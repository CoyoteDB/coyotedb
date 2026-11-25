# CoyoteDB

**PostgreSQL 18 that stays up.** Run it on 2 or 3 machines and they stay in
sync. If the main one dies, another takes over by itself, and no saved write
is lost. Your apps connect as they always do.

Website: https://coyotedb.com

> **Early build.** This is the source of an early build, not a release. There
> are no version numbers and no release downloads yet. Try it, but do not put
> data on it that you cannot afford to lose.

## What it is

- One program, `coyote`. It runs real, unchanged PostgreSQL 18 for you.
- 1, 2, 3 or more machines. New machines join with a one-time invite code.
- A small witness (votes, keeps no data) lets 2 database machines fail over
  by themselves.
- Apps connect to port 5432 on any node with any Postgres driver or tool;
  the connection is passed to the current main node, over TLS.
- Cluster traffic between nodes uses mutual TLS.
- Works over a LAN, Tailscale, or another private network.
- Linux, macOS and Windows; Linux is recommended for database nodes, and the
  Docker image gives you Linux on any computer.

## Quick start with Docker

The image is on Docker Hub as `coyotedb/coyote`, for 64-bit Intel/AMD
(amd64) and ARM (arm64) hosts: Linux with Docker, or Docker Desktop on
Windows and macOS (Apple silicon and Intel). `edge` follows the main branch; `sha-<commit>` tags pin one
build. There is no `latest` tag until there is a release.

First machine (replace `192.168.1.10` with an address of this machine that
the others can reach):

```bash
docker run -d --name coyote --restart unless-stopped \
  --stop-timeout 120 --shm-size 256m \
  -p 7481:7481 -p 5432:5432 -v coyote-data:/data \
  coyotedb/coyote:edge up --name node1 --advertise 192.168.1.10:7481

docker exec coyote coyote invite     # one-time code for the next machine
```

Each other machine:

```bash
docker run -d --name coyote --restart unless-stopped \
  --stop-timeout 120 --shm-size 256m \
  -p 7481:7481 -p 5432:5432 -v coyote-data:/data \
  -e COYOTE_INVITE=coyote-XXXX... \
  coyotedb/coyote:edge join --name node2 --advertise 192.168.1.11:7481
```

Then:

```bash
docker exec coyote coyote status     # which node is main, copies healthy?
docker exec coyote coyote conninfo   # the connection string for your app
docker cp coyote:/data/coyote/ca.crt .
```

To try three nodes and a witness on one machine:

```bash
docker compose -f docker/docker-compose.yml up -d --build
```

The full guide (witness, Tailscale, the dashboard, things to know) is in
[`docker/README.md`](docker/README.md).

## Build from source

Needs Go (the version in `go.mod`). No C compiler.

```bash
go build -o bin/coyote ./cmd/coyote
./bin/coyote --help
```

`coyote` downloads the pinned PostgreSQL build on first start and checks it
against a checksum in the source.

The web dashboard is built separately with Node (`dashboard/README.md`) and
embedded into the binary; without it, a plain `go build` still works and the
program serves a short page instead.

Tests:

```bash
make test     # unit tests, no Postgres needed
make itest    # integration tests with real Postgres processes
make chaos    # kill / freeze / cut-off tests with a write checker
make check    # gofmt + vet + unit tests
```

## Docs

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md): how it works and the
  safety rules
- [`docs/MIGRATE.md`](docs/MIGRATE.md): moving an existing Postgres database in
- [`docs/MIXED_OS.md`](docs/MIXED_OS.md): clusters that mix operating systems
- [`docker/README.md`](docker/README.md): running nodes in Docker

## Licence

Apache License 2.0, see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
PostgreSQL is under the PostgreSQL License.
