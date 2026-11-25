# Changelog

## 0.1.1 (2026-11-25)

Carries PostgreSQL 18.7, the same as 0.1.0.

**Fixed**
- PostgreSQL minor updates (for example 18.7 to 18.8) now work one computer
  at a time, with the database up and no saved write lost. Before, a node
  on the new PostgreSQL release was refused as "database programs changed",
  and a new computer on it could not join. Now a copy never copies a main
  computer that runs a newer PostgreSQL (it waits for its own update), a new
  computer must run the newest PostgreSQL in the cluster, and going back to
  an older one, another major version or an unknown build stays refused.
  The rule switches on once every computer runs 0.1.1.
- `coyote status --json` shows each data node's PostgreSQL version.

**Updating from 0.1.0**
- One computer at a time, copies first: docker/README.md, "Updating to a
  new version".

## 0.1.0 (2026-11-24)

The first release.

**What it does**
- Runs real, unchanged PostgreSQL 18 on 1, 2, 3 or more computers and keeps
  them in sync. A write counts as saved only once a copy has it too.
- When the main computer dies, another takes over by itself, usually in 12
  to 17 seconds in our tests. No saved write is lost. Two computers need a
  small witness for this; three computers do it on their own.
- Apps connect to port 5432 on any computer with any Postgres driver; the
  connection goes to the current main computer, over TLS. While there is no
  main computer, a new connection waits up to 30 seconds instead of failing.
- New computers join with a one-time invite code. Traffic between computers
  uses mutual TLS. Works on a LAN, over Tailscale or another private network.
- A setup page and a control panel in the browser, and the same in the
  terminal: status, add a computer, switch over, change addresses, backups.
- Every problem has a code, a plain sentence and the exact fix
  (`coyote status --details`, `coyote explain CODE`), plus "Copy for your AI
  agent" in the control panel.
- Backups are on by default: every change within about a minute and a full
  copy each night, kept 7 days, to a folder or S3-compatible storage
  (encrypted). Restore to any moment into a new folder; `coyote backup check`
  proves a backup restores.
- `coyote migrate` moves an existing PostgreSQL 12 to 18 database in, once or
  as a live copy.
- An address never changes by itself: if a computer gets a new address,
  CoyoteDB says so and shows the one command (or button) that fixes it.

**How to run it**
- The Docker image `coyotedb/coyote:0.1.0`, one image for 64-bit Intel/AMD
  and ARM. Tested on Linux with Docker, Windows with Docker Desktop and Apple
  silicon Macs with Docker Desktop. See README.md and docker/README.md.
- From source: `go build -o bin/coyote ./cmd/coyote` (Go 1.27).

**Known limits**
- No installers yet; Docker or a build from source.
- Test it with your own app before you trust it with data you cannot lose.
  Copies are not backups: keep backups on another disk or in cloud storage.
