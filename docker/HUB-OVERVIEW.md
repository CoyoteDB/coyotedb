<p align="center"><a href="https://coyotedb.com"><img src="https://coyotedb.com/brand/lockup-light.png" alt="CoyoteDB" width="360"></a></p>

# CoyoteDB

**PostgreSQL 18 that stays up.** Run it on 2 or 3 machines and they stay in sync. If the main one dies, another takes over by itself, and no saved write is lost. Your apps connect as they always do.

**[Website](https://coyotedb.com)** · **[Docker guide](https://coyotedb.com/docs/docker/)** · **[How we test](https://coyotedb.com/testing/)** · **[Support the project](https://coyotedb.com/support/)**

> **Early build.** `edge` follows the main branch; it is not a release yet. Each container is one node.

## Which tag

| Tag | What |
|---|---|
| `edge` | Latest build from the main branch |
| `sha-<commit>` | One fixed build. Use the same one on every machine |

There is no `latest` tag until the first release, so always give a tag.

**Runs on:** 64-bit Intel/AMD computers (amd64): Linux with Docker, or Windows with Docker Desktop. Not on ARM yet (Raspberry Pi, Apple silicon Macs).

## 1. First machine: start a new cluster

Replace `192.168.1.10` with an address of **this machine** that the other machines can reach (its LAN address, or its Tailscale address `100.x.y.z`).

```bash
docker run -d --name coyote --restart unless-stopped \
  --stop-timeout 120 --shm-size 256m \
  -p 7481:7481 -p 5432:5432 -v coyote-data:/data \
  coyotedb/coyote:edge up --name node1 --advertise 192.168.1.10:7481
```

Get an invite code for the next machine (it works once, for one hour):

```bash
docker exec coyote coyote invite
```

## 2. Each other machine: join

```bash
docker run -d --name coyote --restart unless-stopped \
  --stop-timeout 120 --shm-size 256m \
  -p 7481:7481 -p 5432:5432 -v coyote-data:/data \
  -e COYOTE_INVITE=coyote-XXXX... \
  coyotedb/coyote:edge join --name node2 --advertise 192.168.1.11:7481
```

Only two database machines? Add a **witness**: it votes and keeps no data, so the two can switch over by themselves. Any small computer will do:

```bash
docker run -d --name coyote-witness --restart unless-stopped \
  -p 7481:7481 -v coyote-witness:/data \
  -e COYOTE_INVITE=coyote-XXXX... \
  coyotedb/coyote:edge witness --name witness --advertise 192.168.1.12:7481
```

Leave the commands as they are: when Docker restarts a container, it sees the node has already joined and just starts it. Put the code in `COYOTE_INVITE`, not on the command line, where other programs could read it.

## 3. Use it

```bash
docker exec coyote coyote status       # which node is the main one, are the copies healthy
docker exec coyote coyote conninfo     # the connection string for your app
docker cp coyote:/data/coyote/ca.crt . # the certificate your app checks
```

Point your app at the connection string, with `sslrootcert` set to the `ca.crt` you copied. Any node's port 5432 passes connections to the main node.

## Things to know

- **Give the address** (`--advertise`, or `COYOTE_ADVERTISE`). Inside a container, coyote only sees Docker's private address, which other machines cannot reach. It notices this and refuses to guess.
- **Keep the ports the same** inside and outside (`-p 7481:7481 -p 5432:5432`).
- **Use a named volume** (`-v coyote-data:/data`) for the data, not a folder from your Mac or PC.
- **Let it stop cleanly.** `--stop-timeout 120` gives PostgreSQL time to shut down.
- PostgreSQL 18 is inside the image, checked against a pinned checksum, so nothing downloads when a container starts.

## Licence

Apache-2.0. PostgreSQL is under the PostgreSQL License.
