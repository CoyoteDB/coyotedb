# CoyoteDB in Docker

Run a CoyoteDB node as a Linux container on any computer that has Docker:
Linux, a Mac or a Windows PC. Every node then runs the same Linux and the
same PostgreSQL 18 build, whatever the host is. Linux is the recommended
system for database nodes (D38), and this is the easy way to get it on a Mac
or a Windows PC.

Design and reasons: D46 in `docs/DECISIONS.md`. Tests: `docs/TESTING.md`
("Docker image").

## 1. Get the image

The image is on Docker Hub as `coyotedb/coyote`, one image for 64-bit
Intel/AMD (amd64) and ARM (arm64); Docker picks the right one. Tested on
Linux (Intel/AMD) with Docker, Windows with Docker Desktop, and Apple
silicon Macs with Docker Desktop:

```bash
docker pull coyotedb/coyote:0.1.1
```

Tags: `0.1.1` is the newest release (use the same version on every machine),
`latest` the newest release, `edge` the main branch (not a release), and
`sha-<commit>` one fixed build.

To build it yourself, from the repository root:

```bash
docker build -f docker/Dockerfile -t coyotedb/coyote:local .
```

It takes a few minutes the first time. PostgreSQL is downloaded during the
build and checked against the checksum pinned in coyote's source, so the
container never downloads anything when it starts.

Ubuntu's `docker.io` package has no BuildKit; the build still works there
(the classic builder), for the processor of that machine.

## 2. Easiest: the setup page

On each machine, start the container with `setup`. It needs no address and
no invite code on the command line: you give those on the page.

```bash
docker run -d --name coyote --restart unless-stopped \
  --stop-timeout 120 --shm-size 256m \
  -p 127.0.0.1:7480:7480 -p 7481:7481 -p 5432:5432 -v coyote-data:/data \
  coyotedb/coyote:local setup
```

Then get the sign-in link and open it in a browser **on this machine**:

```bash
docker logs coyote                     # shows the link (works once, for 2 minutes)
docker exec coyote coyote dashboard    # a fresh link, any time
```

The page asks: start a new database, or join one (paste an invite code
made on a machine that is already in the cluster: "Add a computer" in its
control panel, or `docker exec coyote coyote invite`). Then a name, backups
(new database only) and **this machine's address**: coyote runs inside
Docker and cannot see it, so the page leaves Docker's own addresses
(`172.17.x.x`) out and asks you to type the machine's LAN address
(`192.168.1.10`) or its Tailscale address (`100.x.y.z`). It refuses
`127.0.0.1` and the container's own address, which other machines cannot
reach.

Leave the command as it is: when Docker restarts the container (or you run
the same command again on the same volume), `setup` sees the node is
already set up and just starts it.

**The control panel** stays at `http://127.0.0.1:7480` on this machine.
Sign in with a fresh link from `docker exec coyote coyote dashboard`.

How it is kept safe (D52, D68): the port is published on the host's
`127.0.0.1` only, so no other computer can reach it; nothing gets in
without a one-time sign-in link (single use, 2 minutes), which only a
command inside the container can make; the page is plain HTTP, which is
why it must never be published on `0.0.0.0` (`-p 7480:7480`). Inside the
container the page listens on `0.0.0.0:7480` because the image sets
`COYOTE_DASHBOARD_LISTEN=0.0.0.0:7480`; coyote uses that only inside a
container on Docker's own network (not with `--network host`). To switch
it off: `-e COYOTE_DASHBOARD_LISTEN=` (then there is no page from the host;
use the command line below).

**The panel from another computer:** do not publish the port wider. Use an
SSH tunnel from that computer, then open the link there:

```bash
ssh -L 7480:127.0.0.1:7480 you@this-machine
# in that SSH session: docker exec coyote coyote dashboard
```

With Tailscale on both computers, the same tunnel works over the Tailscale
address (`ssh -L 7480:127.0.0.1:7480 you@100.x.y.z`).

**The sign-in link is in `docker logs`.** Anyone who can read this
container's logs (Docker's log files on this machine, or a log collector
you set up) sees it. It is still of no use to them: it works once, for 2
minutes, and only from a browser on this machine. If you would rather no
link ever reached the logs, use the command line way below: `up`, `join`
and `witness` print none, and `docker exec coyote coyote dashboard` prints
one only to your terminal.

## 3. The command line way: first machine

Replace `192.168.1.10` with an address of **this machine** that the other
machines can reach (its LAN address, or its Tailscale address `100.x.y.z`).

```bash
docker run -d --name coyote --restart unless-stopped \
  --stop-timeout 120 --shm-size 256m \
  -p 7481:7481 -p 5432:5432 -v coyote-data:/data \
  coyotedb/coyote:local up --name node1 --advertise 192.168.1.10:7481
```

Then get an invite code for the next machine (it works once, for one hour):

```bash
docker exec coyote coyote invite
```

## 4. The command line way: each other machine

```bash
docker run -d --name coyote --restart unless-stopped \
  --stop-timeout 120 --shm-size 256m \
  -p 7481:7481 -p 5432:5432 -v coyote-data:/data \
  -e COYOTE_INVITE=coyote-XXXX... \
  coyotedb/coyote:local join --name node2 --advertise 192.168.1.11:7481
```

A **witness** (votes, keeps no data; makes 2 database machines fail over by
themselves) needs only the cluster port:

```bash
docker run -d --name coyote-witness --restart unless-stopped \
  -p 7481:7481 -v coyote-witness:/data \
  -e COYOTE_INVITE=coyote-XXXX... \
  coyotedb/coyote:local witness --name witness --advertise 192.168.1.12:7481
```

Leave the command as it is: when Docker restarts the container, `join` and
`witness` see the node already joined and just start it. The code goes in
`COYOTE_INVITE` (or `--invite-file FILE` from a mounted file), not on the
command line, where other programs could read it.

## 5. Use it

```bash
docker exec coyote coyote status            # who is the main node, copies healthy?
docker exec coyote coyote conninfo          # the connection string for your app
docker cp coyote:/data/coyote/ca.crt .      # the certificate your app checks
```

Point your app at the connection string, with `sslrootcert` set to the
`ca.crt` you copied. Any node's port 5432 passes the connection to the main
node.

`docker exec -it coyote psql "$(docker exec coyote coyote conninfo --admin)"`
opens a superuser `psql` inside the container (`psql` is in the image).

## Try three nodes on one machine

`docker-compose.yml` runs 3 database nodes and a witness on this machine.
It is for trying CoyoteDB out; it does not survive this machine dying.

```bash
docker compose -f docker/docker-compose.yml up -d --build
docker compose -f docker/docker-compose.yml exec node1 coyote status
docker compose -f docker/docker-compose.yml exec node1 coyote conninfo
docker compose -f docker/docker-compose.yml cp node1:/data/coyote/ca.crt .
```

Apps on this machine connect to `localhost:5432`, `5433` or `5434`.
Remove it all, data included: `docker compose -f docker/docker-compose.yml down -v`.

## Updating to a new version

A new CoyoteDB release (and with it a new PostgreSQL 18 minor version,
which brings PostgreSQL's security fixes) goes on one machine at a time.
The database stays up the whole time and no saved write is lost: the
copies first, then a planned switch-over, then the old main machine. Our
test does exactly this with writes running all the time
(`TestDockerUpdate`, `docs/TESTING.md`).

The release notes (`CHANGELOG.md`) say when a release needs anything more.
Never go back to an older version on a machine that already ran the newer
one.

**0. Before you start.** Every machine healthy, and the new image on every
machine (pulling takes the longest; do it first):

```bash
docker exec coyote coyote status          # main node, copies "following", no "!" lines
docker pull coyotedb/coyote:0.1.1         # on every machine
```

(`0.1.1` stands for the new version's tag throughout.)

**1. Each copy, one at a time.** On a machine that `coyote status` shows as
a copy (not the main node):

```bash
docker stop coyote        # stops PostgreSQL cleanly (give it the time it needs)
docker rm coyote          # the container only; the data stays in the volume
docker run -d --name coyote ...same options and command as before... \
  coyotedb/coyote:0.1.1 ...same command as before...
```

Use exactly the same `docker run` line as when you set the machine up (the
same volume, ports, name and `--advertise`); only the image tag changes.
`join` and `setup` need no invite code here: a volume that already joined
just starts. Then wait until `docker exec coyote coyote status` shows this
machine as a copy that is following again, before you go on to the next
machine. That takes seconds when the copy was only briefly away.

With only two database machines, writes wait while their one copy
restarts (a few seconds) and then carry on. If it stays away longer than
30 seconds, writes carry on saved on the main machine alone until the copy
is back (`coyote status` says so); wait for it before step 3.

**2. The witness**, if you have one: the same three commands on its
machine (its own `docker run` line, `witness`, no client port).

**3. Switch the main node to an updated copy.** On any machine:

```bash
docker exec coyote coyote switch --to node2      # a copy you already updated
```

The switch waits until that copy has every saved write, stops the old main
node and only then lets the new one take writes. Apps see their
connections closed once and reconnect; a new connection waits for the new
main node instead of failing.

**4. The old main machine,** now a copy: step 1 on it. When the new
release brings a newer PostgreSQL, this machine does not copy the new main
node until it is updated (a copy must never run an older PostgreSQL than
the main node); `coyote status` says it waits. Update it right away; it
then catches up by itself.

Copies first matters: a switch to an updated copy is refused while another
copy still runs the older PostgreSQL ("Update node3 first"), and a new
machine must run the newest PostgreSQL in the cluster to join.

**5. Check.** Every machine on the new version, and healthy:

```bash
docker exec coyote coyote version
docker exec coyote coyote status
```

**One database machine only:** there is no copy to take over, so the
database is down while the container restarts (seconds): `docker stop`,
`docker rm`, `docker run` with the new tag. No saved write is lost.

## Things to know

**The address (`--advertise`, or `COYOTE_ADVERTISE`) must be given** (the
setup page asks for it instead).
Inside a container, coyote only sees the container's private Docker address
(like `172.17.0.5`), which other machines cannot reach; it notices it runs
in a container and refuses to start a new node on such an address, with
this advice. Give the host's address and the cluster port.
The ports inside and outside must be the same (`-p 7481:7481`,
`-p 5432:5432`): the address you give is also what apps on other machines
are told to use. If the host's address changes, remove the container and
run it again with the new `--advertise` (the data stays in the volume). The
same `join` command works for that without an invite code: a volume that
already joined just starts. Or keep the container and run
`coyote node set-address NODE NEWHOST:7481` on any node: a container started
again with its old `--advertise` keeps that new address. coyote never
changes an address by itself: when a computer no longer has the address the
cluster records, `coyote status` and the dashboard say `address_gone`, show
the old and the new address, and give that exact command.

**On Linux you can skip the ports and the address** with `--network host`:
then coyote sees the real network and picks the address itself at the
first start (Tailscale first) and keeps it. The container then also uses this machine's 127.0.0.1 ports 7432
(PostgreSQL) and 7480 (control), so do not also run coyote natively on it.
Docker Desktop (Mac, Windows) needs the `-p` form.

**Keep the data in a named volume** (`-v coyote-data:/data`), not in a
folder of your computer (`-v ./data:/data`). On Docker Desktop a folder of
your Mac or PC is shared into Docker's Linux machine through a file-sharing
layer: much slower for a database, and it does not give PostgreSQL the
file permissions, locks and flush-to-disk guarantees it relies on. A named
volume lives on Docker's own Linux disk. On a Linux host a folder works if
it belongs to user 10001 (`sudo chown 10001:10001 ./data`), but a named
volume is still simpler.

**Stopping takes a few seconds; let it.** `docker stop` asks coyote to stop
PostgreSQL cleanly. Docker's default is to kill the container after 10
seconds, which is why the commands above say `--stop-timeout 120`. Docker
Desktop on a laptop: a sleeping computer cannot keep its lease, so another
node takes over while it sleeps; it rejoins when it wakes.

**Tailscale.** Easiest: Tailscale on the host, and `--advertise` the host's
Tailscale address (`100.x.y.z:7481`); the `-p` ports are reachable on it.
Without Tailscale on the host, run the official `tailscale/tailscale`
container next to coyote and share its network:

```bash
docker run -d --name coyote-ts --hostname node1 \
  -e TS_AUTHKEY=tskey-... -e TS_STATE_DIR=/var/lib/tailscale \
  -v coyote-ts:/var/lib/tailscale tailscale/tailscale
docker run -d --name coyote --network container:coyote-ts --restart unless-stopped \
  --stop-timeout 120 --shm-size 256m -v coyote-data:/data \
  coyotedb/coyote:local up --name node1 --advertise node1:7481
```

(`node1` is then the machine's Tailscale name; no `-p` needed. This
sidecar setup has not been through our tests yet; the host-Tailscale way
uses the tested `-p` path.)

## What is in the image

- Debian 13 slim, pinned by digest; `tini` as the first process.
- `coyote` (static, no cgo) in `/usr/local/bin`.
- PostgreSQL 18.7 from `/opt/coyote/pg` (`postgres`, `psql`, `pg_*` on the
  `PATH`), read-only for the database user.
- pgvector (`CREATE EXTENSION vector`) when the image was built with the
  pgvector archives in `docker/extensions/` (built by
  `scripts/extensions/build-linux.sh`, checked against the sha256 pinned
  in coyote's source, D53). Without them the build prints a warning and the
  image has no pgvector. The archives are not in the repository.
- Runs as user `coyote` (uid 10001), never root. Data folder
  `/data/coyote` (the `/data` volume).
- Ports: 7481 (cluster, mutual TLS), 5432 (apps, TLS) and 7480 (setup page
  and control panel, plain HTTP with a one-time sign-in; publish on
  `127.0.0.1` only). PostgreSQL itself listens on 127.0.0.1 inside the
  container only.
- Default command: `setup` (D68): the setup page on an empty volume, the
  node itself on a volume that is already set up.
- Health check: `coyote status` (healthy while waiting for the setup page).

**The control panel with the command line way.** Add
`-p 127.0.0.1:7480:7480` to the `up` / `join` / `witness` run lines above
and sign in with `docker exec coyote coyote dashboard`. The image already
listens on the container's port 7480 (`COYOTE_DASHBOARD_LISTEN`, see
section 2). Publish it on `127.0.0.1` only, never `-p 7480:7480`. The
command-line control of the node stays inside the container (D52).
