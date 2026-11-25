# Architecture

This file describes the target
design and the rules the code must follow.

## One-paragraph version

Each machine runs one program, `coyote` (the **agent**). The agent downloads a
pinned PostgreSQL 18 build, runs it as a child process, and owns its config.
Agents form a small voting group (Raft) that holds only cluster facts: who the
members are, which node is the main one (**primary**), and settings. User data
never goes through Raft; it is copied by Postgres streaming replication with
synchronous commit, so a write is acknowledged only once another node has it.
The Raft leader acts as **controller** and moves the primary role when a node
dies. Every node runs a TCP **proxy** on the Postgres port that forwards to the
current primary, so clients can connect to any node.

## Parts

```
            clients (psql, apps, ORMs, coding agents)
                 |  5432 (any node)
        +--------v---------+        one mTLS port         +------------------+
        |  coyote agent A  |<---------------------------->|  coyote agent B  |
        |  proxy  api  mcp |   raft + replication tunnel  |  proxy  api  mcp |
        |  controller*     |                              |                  |
        |  raft member     |                              |  raft member     |
        +--------+---------+                              +---------+--------+
                 | 127.0.0.1 only                                   |
           [ Postgres primary ] === streaming replication ===> [ Postgres standby ]
```

- **Agent (`cmd/coyote`)** — one process per machine. Everything below lives in it.
- **pg (`internal/pg`)** — fetch + verify the Postgres build, `initdb`, write
  config, start/stop, clone (`pg_basebackup`), rewind (`pg_rewind`), LSN helpers,
  OS process guards.
- **cluster (`internal/cluster`)** — hashicorp/raft with a boltdb store, the
  state machine (FSM), the mTLS transport, cluster CA (`internal/pki`), invite
  codes (`internal/invite`).
- **controller (`internal/controller`)** — runs on the Raft leader only. Leases,
  sync-set changes, failover, re-pointing standbys.
- **proxy (`internal/proxy`)** — plain TCP passthrough on 5432 to the primary.
- **api (`internal/api`)** — HTTP API + embedded dashboard. Contract in `API.md`.
- **mcp (`internal/mcp`)** — MCP server so coding agents can query the database.
- **netdetect (`internal/netdetect`)** — finds usable addresses (Tailscale
  100.64.0.0/10, LAN ranges) to suggest during setup.

## Cluster state (what is in Raft)

Small, and nothing else:

- cluster id, epoch (bumps on every primary change)
- members: id, name, addresses, role (data / witness), os, arch, Postgres build sha
- current primary id and its lease
- sync set **R** (members allowed to be promoted) and required count **k**
- settings (2-node mode, timeouts)
- one-time join secrets (hashed), API key hashes

## Node counts

| Setup | Raft voters | Behaviour |
|---|---|---|
| 1 node | 1 | Solo. No sync replication. |
| 2 nodes | 1 (second data node is a non-voter, D17) | Sync copy. Manual, forced promote only. See safety rule 9. |
| 2 + witness | 3 | Automatic failover. Witness votes, runs no Postgres. |
| 3+ nodes | 3+ | Automatic failover. |

## Normal write path

1. Client connects to any node's 5432. The proxy forwards to the primary.
2. Primary commits; with `synchronous_standby_names = 'ANY k (...)'` the commit
   returns only after k standbys have flushed it.
3. Standbys replay it.

## Failover path

1. Primary's lease stops being renewed (agent dead, machine dead, or cut off).
2. A cut-off primary stops its own Postgres before its lease runs out.
3. Controller waits out the full lease.
4. Controller cuts WAL receiving on enough standbys that the old primary can no
   longer get a write confirmed (rule 2).
5. Controller picks the member of R with the highest received LSN, bumps the
   epoch, promotes it, creates replication slots, re-points the others.
   A copy known to have damaged pages (D55) is never picked; if the only
   ones holding every write are damaged, nothing is promoted.
6. Proxies drop client connections and route to the new primary. A new
   connection opened while there is no usable primary is held, up to 3
   leases (30 s by default, D64, which covers steps 1-5), and released to
   the new primary once it is checked.
7. The old primary, when it returns, is rewound and rejoins as a standby.

**Main node alive but unreachable (D62).** Renewals travel from the main
node to the leader, and a follower's command is relayed through any member
it can dial, so a main node that can dial out but that nobody can dial in
(its address changed, a firewall drops incoming connections) could keep
its lease while no copy could stream from it. It now stops renewing once
no Raft leader has reached it for max(30 % of the lease, one election)
(3 s by default), and the failover path above takes over.

**Addresses (D66, replaces D62's re-detection).** A node's address is
configured once: given with `--advertise`, or detected at the first start
and saved in `agent.json`. The node never changes it by itself; only
`coyote node set-address` (or the dashboard's Change addresses, the same
API call) does, after checking that the node answers there with its own
certificate. Every 5 s each node checks that this computer still has the
addresses the cluster records for it (`internal/agent/addrwatch.go`; only
addresses it was seen to have, so a router's or container host's
forwarded address is never reported). When one is gone it reports
`address_gone` with the previous and the new address and the exact
command, in `coyote status`, the log (once) and `/v1/status` `problems`,
and sends the finding to every other member (RPC `addr_report`, which it
can still send: connecting out works), so their status and dashboard show
it although they cannot reach it. A node that reaches others while no
leader reaches it for 10 s reports `address_unreachable` (one-way) the same
way. A Raft leader whose every recorded address is gone hands the
leadership over, so the lease rule below applies to it (a leader's contact
age is always 0). Kept from D62: when a member's record changes, every
connection dialed to an address it no longer has is closed; every second
every connection whose local IP the computer lost is closed
(`Node.DropDeadConns`); `--advertise` replaces a pinned record only with a
value the cluster has not recorded before; if every address changed while
the cluster was off, `node set-address --local NODE HOST:PORT` on each
computer (no leader needed; kept in `peers.json` until the cluster records
it) tells it where the others are.

**Status problems (D66).** Every problem in a status carries a stable code
(`internal/agent/problems.go`, listed in `dashboard/API.md` §17a and by
`coyote explain`): a short plain line, and a long form (details, evidence,
fix commands, a Markdown block for an AI agent, never with secrets). The
CLI, the API and the dashboard show the same texts, which live in Go only.

**Main node alive but unable to save (D58).** The lease only proves the
main node's agent is alive. Its agent also writes and flushes one tiny WAL
record every second (`internal/agent/mainhealth.go`, no table, no wait for
the copies) and reads its disk's free space. If no probe succeeded for 12 s
because each hung or hit a full disk, or the disk has had under 64 MB free
for 3 s, the main node passes on Raft leadership if it has it and asks for
a planned switch (D41) to the most caught-up healthy copy in R (for a full
disk: one with room). The request is conditional: the leader does nothing
unless it still names that node main node at that term. All of the switch's
rules apply unchanged (rule 2 cuts, target catches up, old main stops and
confirms or its lease is waited out). It stays, and says why, when the
cluster could not fail over by itself either (one data node, two without
a witness, running on one copy, not yet Served), or no copy qualifies
(e.g. every disk full). Writes on a nearly full disk are not refused early;
the node warns under 1 GB free.

## Safety rules (hard rules)

1. **Same platform only.** A cluster's data nodes share os, arch and Postgres
   build. These are stored in Raft and a mismatched join is rejected with a clear
   message. Physical replication across platforms corrupts indexes. `initdb`
   always uses `--locale-provider=builtin --builtin-locale=C.UTF-8
   --encoding=UTF8`; never libc locales. Witnesses may be any platform.
   **Only exception (D37), opt-in:** data nodes of different kinds may
   share a cluster when the cluster allows mixing and their database
   fingerprints and behaviour-probe digests are equal, no guard is blocked,
   and no Windows node is involved yet. The Raft state machine decides;
   see "Mixed operating systems" below. A node that stops matching (drift)
   never copies from, or takes over from, a node of another kind.
   **PostgreSQL minor updates (D70), the only other exception:** while a
   cluster is updated one computer at a time, data nodes on the same os
   and arch may run two different builds of the same PostgreSQL major, and
   only when both builds are in the cluster's Raft table of pinned builds
   (`State.PGBuilds`: builds whose sha256 a member's own coyote pins,
   written only once every member's program knows the table). A node's
   record moves only forward (a newer minor); a joining data node must be
   at or above the newest build recorded for any data node; a copy never
   copies a main node whose build is a newer minor than the one the copy
   runs (it stops its database and waits for its update); a planned
   switch never makes a node the main node while another copy (not the
   current main node) runs an older minor; a failover tie on WAL goes to
   the older minor. An unpinned build, another major, os or arch, and a
   downgrade are refused exactly as before; without the table the rule is
   exactly as before.
2. **Cut the stream before promoting.** A paused (not dead) primary cannot fence
   itself. Before promoting, clear `primary_conninfo` on at least N−k+1 standbys
   (as built, k is counted as 1, so all of them: D42),
   reload, and confirm `pg_stat_wal_receiver` is empty. Only then compare
   `pg_last_wal_receive_lsn()` (flushed, not replayed) and promote the highest.
3. **The sync set lives in Raft and changes in a fixed order.** Postgres' actual
   sync list S is always a subset of Raft's set R. Add: R first, then S. Remove:
   S first, reload, confirm via `pg_stat_replication.sync_state`, then R. k never
   exceeds live candidates. Each change is one Raft entry tagged with the epoch.
   Only members of R may be promoted. A member being removed or leaving
   (`Member.Removing`, D59) never enters R; it leaves R by the remove order.
4. **Leases use monotonic time only.** Never wall-clock timestamps. The primary
   counts its lease from when it *sent* the renew request. A newly elected
   controller waits a full lease plus margin before promoting anyone. Defaults
   (D33): lease 10 s, renew every 1 s (every 250 ms while one is unanswered),
   `pg_ctl stop -m fast` at 60%, `-m immediate` at 80%, the watchdog stops
   Postgres by itself at 85%, kill at 90%. **The lease covers a primary
   Postgres from before its process exists** (D32, D39): the watchdog is
   armed with the lease's absolute deadline right before the postmaster is
   spawned, and a node re-checks lease and term after every primary start
   and promote. "Monotonic" means `internal/mono` (D40): suspend-aware, the
   same for every process on the machine; watchdog deadlines are absolute
   readings of it, so a late message can never extend one. A planned switch
   records a new main node only after the old one confirmed it stopped or
   its lease ran out (D41). The final kill reaches every Postgres process,
   not only the postmaster (D42). **The lease proves contact both ways
   (D62):** a main node that is not the Raft leader renews only while a
   leader has reached it within max(30 % of the lease, one election), and
   a renewal counts from the earlier of its send time and that contact.
5. **Postgres never outlives its agent.** Linux: `Pdeathsig`. Windows: Job Object
   with `KILL_ON_JOB_CLOSE`. macOS: a pipe to the agent plus a watchdog that kills
   the postmaster when it closes. On start, the agent inspects `postmaster.pid`
   and adopts or stops any Postgres already running.
6. **An old primary never restarts as primary.** A client-cancelled sync wait is
   committed locally and would show up after a restart. So a former primary
   always goes through rewind and rejoins as a standby. Fallback: wipe and
   `pg_basebackup`. A promotion cut short (power cut after Postgres removed
   `standby.signal`) is never started as a primary: the agent's
   `promoting_epoch` sends it back to a standby start and a new promotion
   (D56). A rewind whose target wrote WAL as a primary on the source's own
   timeline is refused and re-clones; a copy whose WAL differs from the main
   node's on the same timeline is re-cloned (D56).
7. **Rewind preconditions.** Data checksums on, `wal_log_hints=on`,
   `full_page_writes=on`. `CHECKPOINT` on the new primary before rewinding. Keep
   enough WAL (`wal_keep_size`, or `pg_rewind -c` with a `restore_command` that
   fetches WAL through the agent). After rewind write `standby.signal`,
   `primary_conninfo`, `primary_slot_name`. Physical slots do not exist on a newly
   promoted primary: create them for all members right after promotion.
8. **Cap WAL kept for missing nodes.** `max_slot_wal_keep_size` is set. A slot
   that is invalidated, or a member gone too long, is dropped and the member is
   marked "needs re-clone".
9. **Down to one data node is an explicit setting in Raft.** Applies whenever
   only one data node is left with no sync standby: 2 nodes, 2 + witness, or a
   larger cluster that has lost all but one. `strict`: writes block while
   the standby is down. `degrade_after=30s` (default): the controller clears the
   sync list and records `standby_eligible=false` (retried until done, D34; at
   once at failover in 2 + witness when no copy is left, D31). While false, manual promote
   shows "may lose data since <time>". Eligibility returns only once caught up.
10. **Proxy.** On an epoch change, close every client connection. If the proxy
    cannot reach Raft for longer than the lease, refuse new connections. Each new
    upstream is checked with `pg_is_in_recovery() = false`, cached per epoch. TCP
    keepalives on both sides.
11. **Postgres listens on 127.0.0.1 only.** Replication, base backups and rewind
    run through the agent's single mTLS port. One firewall rule per node.

## Security

- `coyote up` on the first node creates a cluster CA. Every agent gets a cert.
  All agent-to-agent traffic is mutual TLS on one port.
- Invite code = `coyote-` + base32 of seed addresses, cluster id, a one-time
  join secret, the CA fingerprint and an expiry, with a checksum (D21). Single
  use, but the same machine may retry. The joiner pins the CA by fingerprint.
- Membership is the revocation list: a removed node's certificate is refused.
- Postgres uses scram-sha-256. The HTTP API is loopback only and needs the
  CLI's bearer token or a dashboard session from a one-time sign-in link
  (D36); there is no unauthenticated first-run mode: the setup page
  (`coyote setup`, D50) is behind the same guard and the same sign-in.
- Invite codes never need to be on a command line (D50): `join` and
  `witness` read them from a prompt or pipe, `COYOTE_INVITE` (removed from
  the environment before anything is started) or `--invite-file`. A code
  given as an argument still works and prints a warning.
- The downloaded Postgres archive is checked against a sha256 pinned in the
  source code, not one fetched at runtime.

## Ports (defaults)

| Port | Use | Bind |
|---|---|---|
| 5432 | Postgres proxy for clients | all interfaces chosen at setup |
| 7480 | HTTP API + dashboard | 127.0.0.1 only; `--dashboard-listen` (containers, D52) binds another address but lets only private-address browsers in with a session. The Docker image sets `COYOTE_DASHBOARD_LISTEN=0.0.0.0:7480`, used only in a container on its own network (D68); docs publish it as `-p 127.0.0.1:7480:7480` only |
| 7481 | Agent-to-agent (Raft + replication tunnel), mTLS | chosen interfaces |
| 7432 | The real Postgres | 127.0.0.1 only |

## Platform notes

- **Windows:** Postgres refuses to run as Administrator. The service runs as
  the virtual account `NT SERVICE\coyote`; the data dir ACL allows only it. Data
  lives in `%ProgramData%\CoyoteDB` (no spaces: `pg_ctl` goes through `cmd.exe`).
  There is no SIGTERM: always stop with `pg_ctl stop`. Add firewall rules at
  install for the agent only.
- **macOS:** run as a LaunchDaemon so it survives logout. The `coyote` binary
  must be signed and notarized for public release. Sleep breaks leases: warn
  that a laptop is a poor cluster member.
- **Linux:** dedicated `coyote` user. systemd unit with `KillMode=mixed` and a
  `TimeoutStopSec` longer than Postgres shutdown.

## Data dir layout (as built in M1)

```
<data-dir>/
  coyote.lock        node lock (flock / LockFileEx); one agent per data dir
  pgdata/            the Postgres cluster
  cache/             downloaded + unpacked Postgres builds, by version
  logs/              coyote.log, postgres log, watchdog.log
  node.json          what this node is (name, port, versions)
  cluster.secrets    superuser, replication and rewind passwords; 0600, never logged
  upstream.pgpass    passfile for replication / basebackup / rewind; 0600
  replication.json   upstream, slot, sync list; rendered into coyote.conf
  rejoin.json        "must rejoin as standby" marker (see D19)
  config.lock        taken for any config change on a running node
  agent.json         last-primary epoch, stream-cut hold, sync wanted, removed
  pagecheck.json     page check progress and damage found (D55); the damage
                     counts only while pgdata/coyote_page_damage exists
  control.token      bearer token for the local control channel; 0600
  backup/            config.json (0600, rendered from Raft for wal-push),
                     archive.json (what coyote.conf's archive settings say),
                     push.json (last saved WAL, failures, gaps),
                     identity.ok (destination identity checked), local.pgpass
                     (for pg_basebackup), check-*/ (a running `backup check`;
                     D63)
  control.json       where the local control channel listens
  cluster/           identity.json, node.key/crt, ca.crt, peers.json
                     (last known addresses and names; --local ones, D62),
                     join.pending.json (only mid-join), raft/ (log, snapshots)
```

Exact names are in `internal/pg/node.go` and `internal/pg/state.go`.

**Rule 6 in practice (automatic since M3 part B).** A node with `rejoin.json`
never starts Postgres as primary. A node holding primary data starts it as
primary only after a linearizable read of the cluster names it primary at the
epoch its data was last primary; otherwise it writes the marker and rejoins as
a standby (rewind or re-clone through the tunnel). A node that cannot reach a
quorum does not start Postgres as primary at all. The decision table is
`internal/agent/decide.go`.

**Follow check (standbys, D35).** After every change of main node a running
standby checks that it holds nothing the new main node lacks
(`pg.LocalWAL.CanFollow`); if it does, it is rewound like a former primary.
Every (timeline, LSN) pair it reports (WAL receiver, restartpoint, minimum
recovery point) must lie on the main node's timeline history. Replay and
receive positions carry no timeline: they are read first and checked on the
newest timeline the pairs show. Never read a timeline and a position in two
reads in the other order (that called correct copies diverged).

**A copy's stream never stays stuck (D57).** Every libpq connection to an
upstream (`pg.Conninfo`: WAL receiver, pg_basebackup, pg_rewind) carries
TCP keepalives (10 s idle, 5 s apart, 3 probes) and, on Linux,
`tcp_user_timeout=30000`; `wal_receiver_timeout`, `wal_sender_timeout` and
`wal_retrieve_retry_interval` are pinned in `coyote.conf`. The tunnel ends a
connection it cannot serve with a FIN (never a lone reset) and gives a
half-closed pipe 30 s. Behind that, the agent watches a copy that should
stream (another node is main, its stream is not cut, the tunnel points
there): one WAL receiver process that has not reached streaming for 30 s
is restarted (SIGTERM; `pg_ctl kill` on Windows), and a copy that has not
streamed for a minute says so in its output. A copy whose disk has less
than 64 MB free says that too (`internal/agent/recvwatch.go`). None of this
changes what is acknowledged: a copy that is not streaming is handled by
rules 3 and 9 as before.

**Damaged pages (D55).** Postgres checks a page's checksum only when it
reads it. So every copy reads all its pages in the background (the page
check, `internal/pg/verify_pages.go`): amcheck `verify_heapam` in 8 MiB
chunks and `bt_index_check`, paced to 32 MiB/s, resumable, a new pass
after every Postgres start and 24 h after the last full one. Every data
node also reads `pg_stat_database.checksum_failures` every 2 s. A copy
with damage reports it (`status`: `pages`), is never added to the sync
set or promoted, and retires itself like a drifted copy (sync_remove, then
"needs re-clone" once out of R), so it is re-cloned from the main node
(`internal/agent/pagehealth.go`). A strict cluster's only copy stays in R
(never promoted). A damaged main node is reported, not stopped.

## Dashboard and local API (M6, D36)

The agent's local port (default 7480) binds **127.0.0.1 only** and serves
both the CLI's control channel and the web dashboard (embedded from
`dashboard/dist`, `dashboard/embed.go`). Contract: `dashboard/API.md`
(§19 lists what is not built yet).

- **One guard** (`internal/control`) decides every request: loopback
  connection first, then the bearer token from `control.token` (CLI) or a
  dashboard session cookie with Host, Origin and CSRF checks. Handlers never
  do their own access checks.
- **Sign-in:** `coyote dashboard` mints a one-time link (120 s); the page
  trades it for an HttpOnly, SameSite=Strict cookie. Sessions live in
  memory only.
- **Actions go through the CLI's handlers** (planned switch, invites,
  remove, leave, settings, app password, set-address), so the dashboard
  passes exactly the same safety checks. The dashboard has no other way to
  change the main node.
- **Activity log:** `logs/events.jsonl`, written by each agent from the Raft
  state it applies and its own status checks.
- **First run in the browser (D50).** `coyote setup` (and the background
  service on a folder not in a cluster) binds the local port first and
  serves only the setup routes (sign-in, version, setup, addresses,
  check-invite, create, join, reset, stream; every other route 409
  `not_set_up`). "Create" and "join" run exactly what `up` and `join` run,
  on a fresh agent that shares the node lock, the port and the sign-ins;
  once the cluster is open the same port serves the whole API. A create or
  join that fails before the machine is a member is shown on the page and
  can be tried again (or reset, which forgets an unfinished join);
  after that, failures end the agent as before.
- **Invite check** (`check-invite` with `probe`) opens the join stream as a
  join does (TLS, the server chain checked against the code's CA
  fingerprint) and sends nothing, so the code is not used up.
- **Cancel an invite** uses the existing "consume join secret" command
  with the marker `cancelled` as the user: no new Raft command, so every
  version of the state machine reads it.

## Mixed operating systems (D29, D37, D48, D49)

Plan and evidence: `MIXED_OS.md`. Code: `internal/cluster/fsm.go`
(`checkPlatform`; PostgreSQL minor updates: `internal/cluster/pgbuilds.go`,
`internal/agent/pgminor.go`, D70), `internal/agent/{mixedos,builtinonly}.go`,
`internal/pg/{fingerprint,probe,portability,verify,builtinonly}.go`,
`internal/pg/builtinonly.sql`.

- **Record in Raft.** `Member.Platform` holds OS, arch, Postgres build sha,
  plus the fingerprint hash and canonical text and the probe digest per
  group. `Member.Guard` holds the node's latest audit, integrity check and
  drift. `Settings.MixedOS` says whether mixing is on.
- **Join.** A joining data node measures itself on a scratch database
  (throwaway `initdb`, under the guard) before anything is copied. The
  leader refuses joiners with an older `cluster.JoinProtocol`. For a mixed
  join it has every reachable data node audit now, then the state machine
  applies safety rule 1 with its mixed exception. Refusals are plain words.
- **Running.** Each data node re-measures after every Postgres start and
  announces it; the state machine refuses an update that breaks a mixed
  match (drift). It audits after every start, role change and every 10
  min, and runs the quick integrity check hourly. On a main node it drops
  imported OS/ICU collations and installs `amcheck`.
- **Becoming a copy or the main node.** A drifted node never starts,
  clones, rewinds or re-clones from a main node of another kind; as such a
  copy it stops, refuses stream cuts (so a failover never chooses it),
  leaves the sync set by the rule-3 path and is marked `needs_reclone`. A
  planned switch between kinds needs equal fingerprints and no blocked
  guard. Failover between kinds only happens between nodes that matched.
- **Upgrades.** New state fields are written only once every member
  answers the `caps` agent RPC. A node whose state machine halts on an
  entry it cannot read stops its database and exits with a message.
- **Built-in sorting only (D48).** A mixed cluster sorts text only with
  Postgres' built-in provider (every cluster is created with C.UTF-8). At the
  first mixed join the main node refuses if a database uses ICU or OS
  rules, else drops every non-built-in collation in every database
  (template0 included) and installs the `coyote_guard` event triggers, which
  refuse `CREATE COLLATION` and any DDL using a non-built-in collation, and
  log DDL. Each guard tick every data node runs the quick check (database
  providers, triggers on, no leftover collations, DDL log position) and
  audits at once when the DDL log moved. ICU versions are not compared
  (fingerprint format 2); the built-in Unicode version is.
- **No recomputed float maths (D49).** In a mixed cluster the float and
  geometry probe groups are informational (one build fuses multiply-add).
  The same guard refuses structures that would recompute float or
  geometric maths on a new main node: expression and partial indexes,
  CHECK and exclusion constraints, stored generated columns, partition
  keys, indexes with a geometric operator class. Plain B-tree, hash and BRIN
  indexes on float columns are allowed (`float_compare` and `hash` probe
  groups stay must-match). The quick check finds such objects too, and a
  mixed join is refused while any exist.
- **Promotion-time check (D48, rule 1).** A failover candidate or switch
  target of another kind than the main node checks inside its cut, after
  applying everything it received: its own recorded guard result, the quick
  check, and a fresh audit if the DDL log moved. It refuses the cut if
  anything blocks, so the controller waits (no promotion onto it).
- **Drifted main node (D48).** It refuses client writes at once
  (`default_transaction_read_only`, reads still work) and hands over by a
  planned switch to a healthy copy in the sync set when one exists; the
  refusal is lifted when the drift is gone.

## Running in a container (D46)

`docker/Dockerfile` builds one image for data nodes and witnesses: Debian 13
slim, `tini` as PID 1, the static `coyote`, and the pinned PostgreSQL build
in `/opt/coyote/pg` (fetched at image build time by `docker/pgfetch`, which
calls `pg.Ensure`: same URL, same pinned sha256, same manifest). User
`coyote` (uid 10001); data folder `/data/coyote` on a named volume (found
through `XDG_DATA_HOME=/data`, so `docker exec` commands need no flags).
Nothing in `internal/` knows it runs in a container.

- **Rule 5 in a container.** tini forwards `docker stop`'s SIGTERM to
  coyote only (not to the group); coyote stops Postgres itself. Postgres'
  Pdeathsig and the watchdog work as on any Linux machine. Orphans (backends
  of a killed postmaster) are reparented to tini and reaped. `docker kill`
  or the container dying kills every process in it: no Postgres outlives
  its agent.
- **Stop time.** coyote's orderly stop can take up to 50 s (fast 30 s,
  immediate 10 s, kill 10 s); run lines use `--stop-timeout 120`, compose
  `stop_grace_period: 2m` (Docker's default 10 s would SIGKILL a slow
  shutdown, which is safe but forces crash recovery).
- **Addresses.** In bridge networking coyote sees only the container's
  private address, so `--advertise HOST:7481` is required and the published
  ports equal the container ports (the advertised host is also the host
  apps are given by `conninfo`). Rule 11 is unchanged: Postgres listens on
  the container's 127.0.0.1.
- **Durability.** Named volumes live on the engine's Linux filesystem, so
  `fdatasync` (Linux's `wal_sync_method`) reaches the engine's disk. On
  Docker Desktop that disk is a virtual disk file on the host; a host power
  cut is as safe as that VM disk's flushing, not tested yet.
- **Address refusal (D51).** In a container (`/.dockerenv`,
  `/run/.containerenv`, `container=`, cgroup or mount hints), a node that is
  not yet a member refuses to announce a detected address of a container
  network (172.16/12, 10.88/16, 10.0.2/24) and says to give `--advertise`
  or `COYOTE_ADVERTISE`. A member keeps running and is only warned.
  `COYOTE_ADVERTISE` is the same as `--advertise` everywhere.
- **Dashboard from the host (D52):** `--dashboard-listen 0.0.0.0:7480`
  with `-p 127.0.0.1:7480:7480`. The guard then also admits browsers from
  private addresses (the bridge gateway), on the session path only: Host
  must still be a loopback name on that port, a session from a one-time
  link is required, the bearer token stays loopback-only.

## Updating a cluster to a new release (D69)

One computer at a time, the database up throughout: **the copies first,
then the witness, then a planned switch-over (D41) to an updated copy,
then the old main node.** Each computer: stop cleanly (`docker stop`,
or stop the service), replace the program (the image tag), start it with
the same data folder and command; wait until it is a following copy in R
again before the next. Steps for people: `docker/README.md` "Updating to
a new version". Proof: `TestDockerUpdate` (TESTING.md).

Why this order: PostgreSQL's own rule for minor releases is standbys
before the primary (a newer primary may write WAL an older standby cannot
read); the switch moves the main role without a failover and without the
lease wait; the old main node is then just a copy.

Rules for every release, because a cluster runs two versions at once
during the roll:

- **Raft entries and snapshots are decoded strictly** (unknown fields are
  an error, `DecodeCommand`); an older program that meets a newer field
  halts its state machine. So a release may add a field to a Raft command
  or to `State` only behind a capability every member has confirmed (the
  `caps` RPC and `JoinProtocol` pattern of D37's mixed-system fields), and
  never in a patch release unless gated that way.
- **The state machine's apply logic must give the same answer on every
  version in the cluster.** A command one version accepts and another
  refuses splits the cluster state. A change to what `applyCommand` accepts
  (for example `checkPlatform`) is gated like a new field.
- **No downgrades.** A computer that ran the newer release is never started
  again on the older one.
- **A PostgreSQL minor update changes `Platform.PGBuildSHA`** (rule 1
  compares it). Rule 1's minor-update exception (D70) records it:

  - **Pinned builds in Raft.** `internal/pg` keeps every earlier pinned
    release of the major (`earlierBuilds`, never downloaded) next to the
    current one (`pg.KnownBuilds`). Each data node's guard loop adds the
    builds its program pins for its own os/arch to `State.PGBuilds` with
    `RegisterPGBuilds`, once every member answers the `caps` RPC with
    `pg_builds` (an older member's strict decoder would halt on it). A
    joining data node sends its own build's entry with the join; the
    leader registers it before admitting (a computer on a newer release
    names a build the members' programs do not know). Once the table
    exists the leader refuses joiners below `JoinProtocol` 3.
  - **The state machine decides** (`checkPlatform`, `checkPGLine`): a data
    node not alike to another but on the same line (both builds in the
    table, same major, os, arch: `State.SamePGLine`) matches it, with a
    direction: a joiner must be at or above every same-line record, and a
    member's record moves only forward. Going back to the build every
    other data node has is "alike" and passes as before (undoing an
    update). The decision reads only the state, so every member, of any
    version that has the table, decides the same; while the table is
    empty nothing is on a line and rule 1 is the original one.
  - **A copy never copies a newer main node** (`minorFollowBlocked`): it
    compares the build it runs (not its record, which moves only after
    its database started) with the main node's record. When the main
    node is newer it stops its database and waits, saying so, until it is
    updated. This is the old main node's state between the switch and its
    own update; nothing it holds is lost, and the cluster runs on the
    other copies meanwhile (with one copy left, on its own, rule 9).
  - **Switch and failover.** A planned switch is refused while a copy
    other than the current main node runs an older minor than the target
    (`minorSwitchBlocked`: "Update n3 first"). The other direction (a
    target on an older minor than the current main node) is safe: the old
    main node then copies an older one. `ChooseTarget` breaks a WAL tie
    toward the older minor, so the remaining copies can follow; a higher
    WAL end still always wins (rule 2).
  - **A refused announcement is tried again** when the table grows, so a
    roll that started with a member on a release without the table (no
    table, old rule, records do not move and updated nodes warn) clears
    by itself once the last member is updated: the table is written, each
    updated node announces again, the warnings go.
  - **Not covered:** a main node restarted in place on a newer minor while
    copies run the older one (not the documented order): its copies stop
    copying it as soon as its record moves, seconds after its database
    started; in those seconds an older copy may receive its WAL, the case
    PostgreSQL warns about. And a mixed-OS cluster (D37): its nodes'
    fingerprints include the exact PostgreSQL version, so a minor update
    there is still refused as drift, as before D70 (no write is lost; an
    updated copy whose main node is another kind stops copying it and is
    marked for a fresh copy, as for any drift).

## Moving a database in (M8, D47)

Code: `internal/migrate`, `cmd/coyote/migrate.go`. Guide: `MIGRATE.md`.

- The command runs on any member. It asks the local agent for the app
  connection string (`/conninfo`) and the main node and epoch
  (`/status`); it needs no new API and touches no cluster state.
- `pg_dump` (directory format, parallel, one exported snapshot) →
  `pg_restore` as the app role through the client port, into
  `<db>__migrating`. The data reaches the copies by normal replication.
- The copy is compared with the source's snapshot (counts and row
  hashes), sequences are moved past their columns' maxima, and only then
  is it renamed to `<db>`. Any change of main node (epoch) or lost
  connection stops it before the rename.
- The superuser is used only to create extensions the app role may not,
  and only from the main node's own machine (rule 11 unchanged).
- pg_dump and pg_restore run in their own process group, killed with the
  command (Linux: parent-death signal).
- The source connection string comes from `COYOTE_MIGRATE_SOURCE` or
  stdin by default, never from the command line of a child process
  (pg_dump gets the password through `PGPASSWORD`).
- **Live copy** (`--live`, phase 2): schema-only dump/restore into
  `<db>__migrating`, then `CREATE PUBLICATION coyote_migrate_<db> FOR TABLE
  <leaf tables>` on the source and `CREATE SUBSCRIPTION` (same name,
  `synchronous_commit = on`) in the staging database, made by the local
  administrator on the main node's machine (statement logging off for
  that session: it holds the source password). The subscription is
  catalog state, so it reaches the copies by physical replication, and
  its replication origin's progress rides in the commit records. After a
  failover the new main's Postgres starts the apply worker itself and
  resumes from the last commit it holds; because the worker's commits
  wait for the synchronous copy, the source's slot never moves past what
  the new main has. No agent or controller change was needed.
  `--status`/`--cutover`/`--cancel` read the source conninfo back from
  `pg_subscription` and must run on the main node's machine (they say
  where otherwise). `--cutover` waits until the slot's `confirmed_flush_lsn`
  passes the source's current LSN, compares every published table,
  copies sequence values (logical replication does not), fixes them past
  column maxima, drops subscription and publication, and swaps as above.

## Extensions outside the PostgreSQL build (D53)

Code: `internal/pg/ext.go`; build scripts: `scripts/extensions/`.

- Today only pgvector 0.8.7. Each target has one archive,
  `pgvector-0.8.7-pg18.7.0-<target>.tar.gz`, whose sha256 is pinned in
  source. It is built by us against that target's pinned PostgreSQL tree.
- Where from: `COYOTE_EXTENSIONS_FROM`, a folder or an `https://` base URL.
  Unset: no extension is installed and nothing is downloaded (the archives
  are not hosted yet; release blocker, D53).
- When: `pg.Ensure`, after the PostgreSQL tree is ready, under the same
  cache lock. Errors are logged and shown once; the node runs without the
  extension.
- How (power-cut safe, like D34): download to a temporary file, check the
  pin, unpack into `cache/.extract-*`, refuse any file that is not the
  extension's own or would replace a PostgreSQL file, fsync, remove the old
  marker, rename the library first and the control file last (Postgres
  only offers an extension once its control file is there), then write
  `.coyote-ext-pgvector` (a manifest with every file's sha256). An install
  that does not match its marker is reinstalled or removed, control file
  first.
- The tree check (`CheckInstall`) still covers only PostgreSQL's own files;
  extension files are checked against their own marker.
- Mixed clusters: `vector` is refused (audit deny list), and so is any
  index of a non-core access method built on float maths (`builtinonly.sql`).
- Not yet: the cluster does not check that every node has the same
  extensions (D53 "Not checked yet").

## Backups and point-in-time restore (M7, D63)

Code: `internal/backup` (stores, object format, S3 client, catalog,
wal-push/fetch, full backup, restore, check), `internal/pg/backup.go`
(pg_basebackup to a stream, recovery runs), `internal/agent/backup*.go`
(settings, scheduler, status, API), `cmd/coyote/backup.go`.

- **What is kept:** full backups (`pg_basebackup -X none` from the node's
  own Postgres, a tar stream gzipped into 32 MiB parts, `info.json` last)
  and every WAL segment. Together they restore the database as it was at
  any moment from the oldest kept full backup to about a minute ago.
  A full backup counts (`done`) only once the archive holds every WAL
  segment from its start (`backup_label` in the stream) to its end (the
  server's position right after the backup); while waiting, the taker saves
  needed segments it holds complete in its own `pg_wal` but never archived
  (WAL a copy received while being cloned).
- **Before a full backup starts** (D67) the node checks its running
  Postgres: `archive_mode` is `always` (or `on` on a main node),
  `archive_command` is the one rendered for it, `pg_stat_archiver` shows
  no failure newer than its last success, and no WAL segment the copy is
  replaying came from its own `pg_wal` without an archive status. If not,
  the backup does not start: status shows `waiting` with the reason, it is
  tried again every 5 s, and after 5 minutes it is an error (a manual
  request is then given up). The server's own wait inside pg_basebackup
  for the archive is bounded too (`WaitWAL`, 10 minutes); a backup that
  fails or is cancelled has its parts and `running.json` removed and never
  gets `info.json`.
- **WAL a copy replayed from its own `pg_wal`** (D67): a clone's last
  segment can be left with no archive status (pg_basebackup marks it done
  only when its stream got the whole segment). Each backup tick a copy
  gives `.ready` to every status-less segment of a full segment's size on
  the timeline it replays, below the segment replay is in (recovery read
  past its end), and reloads Postgres to wake the archiver. Postgres
  itself would do the same only at a later restartpoint that removes it.
- **Who archives:** every data node (`archive_mode = always`,
  `archive_command` = `coyote backup wal-push`, `archive_timeout = 60s`).
  An object already stored with the same content is a success; a copy
  never replaces a stored object, a primary replaces one that differs
  (only a dead branch can differ) and keeps the old one in `conflicts/`.
  A failover leaves no gap: the new main node and the other copies push
  what they received. Backups off: `archive_command` is a no-op (`exit 0`),
  never our program, so a moved binary cannot hold WAL back.
- **Where:** a folder (per node: each data node keeps a complete set) or
  S3-style storage (one shared set; full backups by one copy). Layout under
  `<root>/<cluster-id>/`: `coyote-backup.json`, `wal/<name>`,
  `base/<id>/part-NNNNN`, `base/<id>/info.json`, `conflicts/`.
- **Objects:** header `CYBK0001`, flags (gzip, AES-256-GCM), a content id
  (sha256 of the raw file; HMAC-SHA256 with the key when encrypted), then
  the payload. Encrypted payloads use the object key as associated data.
- **Bounded:** a failing push with more than 2 GiB waiting or under 1 GiB
  free disk is recorded as a gap and released; the next full backup closes
  it. Nothing is on the write path.
- **Restore** only into a new, empty folder, under that folder's node lock
  and the process guard, with every connection refused and archiving off.
  The live cluster, its data folders and its archive are never written.
  The result is a node folder `coyote up --data-dir DIR` starts as a new
  cluster. Live nodes never get a `restore_command`.
- **Check:** `coyote backup check` restores the newest backup into a
  scratch folder under `<data-dir>/backup/`, reads every page (amcheck,
  page checksums), and deletes it.
- **From the dashboard (D65):** `POST /v1/backups/check-destination` tests
  a place without saving it (also served by the setup page before the node
  is in a cluster); `POST /v1/backups/restore` (dry run or a job) and
  `POST /v1/backups/check` run the same `backup.Restore` / `backup.Check`
  inside the agent (`internal/agent/backup_jobs.go`), one job per node at a
  time, under the backup loop's context. A dashboard restore also refuses
  the data folder and the backup folder, checks free space, and answers
  with the `coyote up --data-dir …` line (free ports) that starts the copy.
  The setup wizard's backup choice is applied after the cluster is created
  (the D63 default is in force until then).

## Not in v1

Mixed-platform clusters by default (they are opt-in, D37), major-version upgrades (pinned to PG18), read routing,
connection pooling, protocol-aware proxying, automatic membership changes,
WAN/multi-region, SQL editor, auto REST API, auth, realtime, file storage.
