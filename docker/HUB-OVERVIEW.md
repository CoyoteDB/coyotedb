<p align="center"><a href="https://coyotedb.com"><img src="https://coyotedb.com/brand/lockup-light.png" alt="CoyoteDB" width="360"></a></p>

# CoyoteDB

**PostgreSQL 18 that stays up.** Run it on 2 or 3 machines and they stay in sync. If the main one dies, another takes over by itself, and no saved write is lost. Your apps connect as they always do.

**[Website](https://coyotedb.com)** · **[Docker guide](https://coyotedb.com/docs/docker/)** · **[How we test](https://coyotedb.com/testing/)** · **[Support the project](https://coyotedb.com/support/)**

> **Version 0.1.0**, the first release. Each container is one node. Test it with your own app before you trust it with data you cannot lose, and keep backups.

## Which tag

| Tag | What |
|---|---|
| `0.1.0` | This release. Use the same version on every machine |
| `latest` | The newest release (now 0.1.0) |
| `edge` | The newest build from the main branch, not a release |
| `sha-<commit>` | One fixed build |

**Runs on:** 64-bit Intel/AMD (amd64) and ARM (arm64), one image: Linux with Docker, Windows with Docker Desktop, or a Mac with Docker Desktop (Apple silicon or Intel). Tested on Linux (Intel/AMD), Windows and Apple silicon Macs.

## Easiest: the setup page

Run this on each machine:

```bash
docker run -d --name coyote --restart unless-stopped \
  --stop-timeout 120 --shm-size 256m \
  -p 127.0.0.1:7480:7480 -p 7481:7481 -p 5432:5432 -v coyote-data:/data \
  coyotedb/coyote:0.1.0 setup
```

Get the sign-in link and open it in a browser on the same machine:

```bash
docker logs coyote                    # the link (works once, for 2 minutes)
docker exec coyote coyote dashboard   # a fresh link, any time
```

The page lets you start a new database, or join one with an invite code from a machine already in it. It asks for **this machine's address** (its LAN address like `192.168.1.10`, or its Tailscale address `100.x.y.z`): inside Docker coyote cannot see it, so it does not guess.

Afterwards the same container keeps running the node, also after restarts. The control panel stays at `http://127.0.0.1:7480`; sign in with `docker exec coyote coyote dashboard`.

**Keep `-p 127.0.0.1:7480:7480` exactly like that.** The page is plain HTTP behind a one-time sign-in link; never publish it as `-p 7480:7480`. From another computer, use an SSH tunnel: `ssh -L 7480:127.0.0.1:7480 you@this-machine`.

## Or the command line: first machine

Replace `192.168.1.10` with an address of **this machine** that the other machines can reach (its LAN address, or its Tailscale address `100.x.y.z`).

```bash
docker run -d --name coyote --restart unless-stopped \
  --stop-timeout 120 --shm-size 256m \
  -p 7481:7481 -p 5432:5432 -v coyote-data:/data \
  coyotedb/coyote:0.1.0 up --name node1 --advertise 192.168.1.10:7481
```

Get an invite code for the next machine (it works once, for one hour):

```bash
docker exec coyote coyote invite
```

## Each other machine: join

```bash
docker run -d --name coyote --restart unless-stopped \
  --stop-timeout 120 --shm-size 256m \
  -p 7481:7481 -p 5432:5432 -v coyote-data:/data \
  -e COYOTE_INVITE=coyote-XXXX... \
  coyotedb/coyote:0.1.0 join --name node2 --advertise 192.168.1.11:7481
```

Only two database machines? Add a **witness**: it votes and keeps no data, so the two can switch over by themselves. Any small computer will do:

```bash
docker run -d --name coyote-witness --restart unless-stopped \
  -p 7481:7481 -v coyote-witness:/data \
  -e COYOTE_INVITE=coyote-XXXX... \
  coyotedb/coyote:0.1.0 witness --name witness --advertise 192.168.1.12:7481
```

Leave the commands as they are: when Docker restarts a container, it sees the node has already joined and just starts it. Put the code in `COYOTE_INVITE`, not on the command line, where other programs could read it.

## Use it

```bash
docker exec coyote coyote status       # which node is the main one, are the copies healthy
docker exec coyote coyote conninfo     # the connection string for your app
docker cp coyote:/data/coyote/ca.crt . # the certificate your app checks
```

Point your app at the connection string, with `sslrootcert` set to the `ca.crt` you copied. Any node's port 5432 passes connections to the main node.

## Things to know

- **Give the address** (`--advertise`, or `COYOTE_ADVERTISE`, or on the setup page). Inside a container, coyote only sees Docker's private address, which other machines cannot reach. It notices this and refuses to guess.
- **Keep the ports the same** inside and outside (`-p 7481:7481 -p 5432:5432`). Port 7480 (setup page, control panel) on `127.0.0.1` only.
- **Use a named volume** (`-v coyote-data:/data`) for the data, not a folder from your Mac or PC.
- **Let it stop cleanly.** `--stop-timeout 120` gives PostgreSQL time to shut down.
- PostgreSQL 18 is inside the image, checked against a pinned checksum, so nothing downloads when a container starts.

## Licence

Apache-2.0. PostgreSQL is under the PostgreSQL License.
