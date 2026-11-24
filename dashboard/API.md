# CoyoteDB local HTTP API, version 1

This is the HTTP API every `coyote` agent serves under `/v1`. The dashboard
(`dashboard/`, embedded in the binary) uses it, the CLI uses it, and it is
the product's public local API. The typed client in `src/api/types.ts` is
kept in lockstep with this file; the mock server in `mock/` implements it.

Each endpoint is marked:

- **exists** — served by `internal/agent/api.go` today. Its shape here matches
  the Go code exactly. Some get **additive** fields (marked *new field*); old
  clients keep working.
- **new** — designed for the dashboard (M6, or M7 where noted).

**As built (M6, 2026-11-21, D36).** The Go side is `internal/agent/dash*.go`
behind the guard in `internal/control`. The *Built* column of §3 says what is
real; everything not built answers **501 `not_yet_available`**, and
`GET /v1/version` lists it in `features` so the dashboard hides it. Where
this file and the code disagree, §19 lists the deviations.

Words: the *main node* is the Postgres primary, a *copy* is a data node
following it (standby), a *witness* votes but holds no data. "Switch-over"
moves the main role.

## Contents

1. [Conventions](#1-conventions)
2. [Authentication and sessions](#2-authentication-and-sessions)
3. [Endpoint summary](#3-endpoint-summary)
4. [Setup (first run)](#4-setup-first-run)
5. [Status](#5-status)
6. [Live stream (Server-Sent Events)](#6-live-stream-server-sent-events)
7. [Activity](#7-activity)
8. [Invites](#8-invites)
9. [Members, leaving, renaming](#9-members-leaving-renaming)
10. [Switch-over and forced promote](#10-switch-over-and-forced-promote)
11. [Settings and this node](#11-settings-and-this-node)
12. [Connection info](#12-connection-info)
13. [Backups (M7)](#13-backups-m7)
14. [Coding agents / MCP (M7)](#14-coding-agents--mcp-m7)
15. [Version](#15-version)
16. [Static files](#16-static-files)
17. [Error codes](#17-error-codes)
18. [Go-side checklist](#18-go-side-checklist)
19. [As built in M6: deviations](#19-as-built-in-m6-deviations-2026-11-21)

---

## 1. Conventions

- Base URL: the agent's API port, default `http://127.0.0.1:7480`. The
  dashboard is served from the same origin, so every call is same-origin.
- Request and response bodies are JSON (`Content-Type: application/json`),
  UTF-8. Request bodies are decoded strictly (`DisallowUnknownFields`, as
  `control.ReadJSON` does today): **clients must not send fields this
  document does not list.** Response objects may gain fields at any time;
  clients ignore what they do not know.
- Request bodies are capped at 1 MiB (as today).
- Times are RFC 3339 strings in UTC (`"2026-11-20T14:03:11Z"`). A zero time is
  omitted (Go `omitzero`), never sent as `"0001-01-01T00:00:00Z"`.
- Durations in settings are Go duration strings (`"30s"`, `"5m"`), as today.
  Durations in new read-only fields are integer milliseconds with an `_ms`
  suffix. Existing controller fields keep their nanosecond `_ns` suffix.
- Node references in paths (`{node}`) accept a member's name or id, as
  `DELETE /v1/members/{node}` does today.
- Every response to `/v1/*` carries `Cache-Control: no-store`.

### Error shape (exists)

Every non-2xx response is:

```json
{ "error": "Plain-words message a person can read.", "code": "machine_code", "details": {} }
```

| Field | Type | Meaning |
|---|---|---|
| `error` | string | Human message. Safe to show as-is. |
| `code` | string | Stable machine code (see [§17](#17-error-codes)). Clients branch on this, never on `error`. |
| `details` | object, optional, *new field* | Extra structured facts for some codes (e.g. both platforms for `platform_mismatch`). |

HTTP status: 400 bad input, 401 no/expired session, 403 refused (wrong host,
CSRF, not loopback), 404 unknown thing, 409 conflict with cluster state
(today's default for failures), 422 invite code invalid, 501 not yet
available, 503 no leader / agent busy, 504 timeout.

---

## 2. Authentication and sessions

Two kinds of credential. Both are checked by one guard in front of every
`/v1` route.

| Credential | Who | Where accepted | CSRF check |
|---|---|---|---|
| `Authorization: Bearer <control.token>` (**exists**) | the CLI on this machine | loopback connections only | no (not an ambient credential) |
| Session cookie (**new**) | the dashboard in a browser | loopback only (remote access is not built, D36) | yes, on every state-changing request |

`/v1/dev/*`, `POST /v1/session/links` and `GET /v1/conninfo` (it returns
the password by GET) accept the bearer token only; a session cookie is
refused there with 403 `bearer_only`.

**One guard owns access** (`internal/control`, D36), in this order:

1. The connection must come from loopback, whatever the credential
   (403 `forbidden`). The port is bound on 127.0.0.1 only anyway.
2. A request with an `Authorization` header takes the CLI's path, unchanged:
   the bearer token or 401 `unauthorized`. No Host, Origin or CSRF check.
3. Otherwise: Host check (§2.4, 403 `bad_host`); then public requests (static
   files, `POST /v1/session`) pass, with a matching `Origin` if one is sent;
   everything else needs a valid session cookie (401 `unauthenticated`), is
   refused on bearer-only routes (403 `bearer_only`), and on every method but
   GET/HEAD needs a matching `Origin` and `X-Coyote-CSRF` (403 `csrf_failed`).

### 2.1 Getting a session: the one-time link

```
person            coyote CLI                   agent (API)                     browser
  | coyote dashboard |                               |                              |
  |----------------->| POST /v1/session/links (Bearer)                              |
  |                  |------------------------------>| mint login token (one use,   |
  |                  |<------------------------------|  120 s), store sha256 only   |
  |                  |  {url, expires}               |                              |
  |                  | print url, open browser ---------------------------------->  |
  |                  |                               |   GET /login                 |
  |                  |                               |<-----------------------------|  (#t=… is never sent)
  |                  |                               |   index.html                 |
  |                  |                               |----------------------------->|
  |                  |                               |   JS reads #t, replaceState('/') removes it
  |                  |                               |   POST /v1/session {token}   |
  |                  |                               |<-----------------------------|
  |                  |                               |   Set-Cookie + {csrf_token}  |
  |                  |                               |----------------------------->|
```

1. `coyote dashboard` calls `POST /v1/session/links` with the control
   token. The agent creates a **login token**: 32 random bytes, base64url, valid
   for 120 seconds, usable once. It keeps only its SHA-256 in memory (never on
   disk, never in Raft, never logged).
2. The CLI prints the URL and opens the default browser (`--no-open` only
   prints). The token sits in the **URL fragment**
   (`http://127.0.0.1:7480/login#t=<token>`), so it never reaches server logs,
   proxies or `Referer` headers.
3. The page's script reads `#t`, immediately calls
   `history.replaceState(null, "", "/")` so the token leaves the address bar
   and history, then sends `POST /v1/session {"token": "<token>"}`.
4. The agent checks the hash in constant time, deletes it (single use),
   creates a **session**, and answers with `Set-Cookie` and the CSRF token.
5. A link that is expired, already used or unknown gets 401
   `login_link_invalid`. The dashboard then says: "This sign-in link has
   expired or was already used. Run `coyote dashboard` again."

### 2.2 The session cookie

```
Set-Cookie: coyote_session_7480=<session id>; Path=/; HttpOnly; SameSite=Strict; Max-Age=43200
```

- The name carries the API port (`coyote_session_<port>`). Browsers share
  cookies across ports of one host, and several agents on one machine (local
  test clusters) must not overwrite each other's session.
- Session id: 32 random bytes, base64url. The agent stores its SHA-256 in
  memory. Sessions do not survive an agent restart; the dashboard then shows
  "Your session ended" with the command to run.
- Idle timeout 12 h (sliding, refreshed by any authenticated request), absolute
  lifetime 7 days. `Max-Age` tracks the idle timeout.
- `Secure` is added when the API is served over HTTPS. Plain HTTP is used only
  on loopback and (when enabled) over the person's private network or VPN.
- Known limit: other programs listening on other ports of 127.0.0.1 receive
  the cookie if the browser talks to them. The cookie alone cannot change
  anything (CSRF token needed) and cannot reveal the database password
  (that is a POST, see §12), which keeps this acceptable.

### 2.3 CSRF and origin protection

For a request authenticated by the session cookie, every method other than
`GET`/`HEAD` must:

1. carry `X-Coyote-CSRF: <csrf_token>` equal to the session's CSRF token
   (constant-time compare), **and**
2. carry an `Origin` header equal to the API's own origin
   (`http://<Host>`). Browsers send `Origin` on every fetch with these
   methods.

Failure: 403 `csrf_failed`. The CSRF token is random per session (32 bytes,
base64url), returned by `POST /v1/session` and `GET /v1/session` (a
cross-origin page cannot read either response: the API sends **no CORS
headers at all**).

`SameSite=Strict` already stops cross-site requests from carrying the
cookie; the header and `Origin` check are the second lock.

### 2.4 Host check (DNS rebinding)

Every request (static files too) must have a `Host` header that is one of
`127.0.0.1:<port>`, `localhost:<port>`, `[::1]:<port>` (remote access, which
would add the advertised addresses, is not built). Bearer requests from the
CLI are exempt (they never come from a browser). Anything else gets 403 `bad_host` before
routing. This stops a malicious site from pointing its own domain at
127.0.0.1 and talking to the agent.

### 2.5 Headers on every response

```
Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
X-Frame-Options: DENY
Cross-Origin-Opener-Policy: same-origin
Cross-Origin-Resource-Policy: same-origin
Permissions-Policy: camera=(), microphone=(), geolocation=()
```

The dashboard build has no inline scripts or styles, so this CSP needs no
hashes or nonces. The mock server sends the same headers, so the
end-to-end tests prove it.

### 2.6 First run

A freshly installed agent (service running, no cluster yet) serves the same
API. Sessions work the same way: the installer's last step runs
`coyote dashboard`, which mints a link. There is no unauthenticated
"first-run from localhost" mode: any local program or web page could use
one.

### 2.7 Session endpoints

#### `POST /v1/session/links` — new, bearer only

Request:

```json
{ "host": "100.64.0.43" }
```

| Field | Type | Meaning |
|---|---|---|
| `host` | string, optional | Host to put in the link. Default `127.0.0.1`. Must be `127.0.0.1`, `localhost` or `::1`; else 400 `bad_host`. |

Response 200:

```json
{ "url": "http://127.0.0.1:7480/login#t=Zk3…", "expires": "2026-11-20T14:05:11Z" }
```

#### `POST /v1/session` — new

Request `{"token": "<login token from #t>"}`. No session or CSRF header needed;
`Origin`, if present, must match.

Response 200 (with `Set-Cookie`):

```json
{
  "csrf_token": "q8V…",
  "expires_at": "2026-11-21T02:03:11Z",
  "absolute_expires_at": "2026-11-27T14:03:11Z",
  "idle_timeout_s": 43200
}
```

Errors: 401 `login_link_invalid`.

#### `GET /v1/session` — new

Response 200: same body as `POST /v1/session`. 401 `unauthenticated` when there
is no valid session. The dashboard calls this at start-up.

#### `DELETE /v1/session` — new

Ends the session. 204 and `Set-Cookie: coyote_session_<port>=; Max-Age=0`.

---

## 3. Endpoint summary

| Method | Path | Status | Built (M6) | Purpose |
|---|---|---|---|---|
| POST | `/v1/session/links` | new | yes | Mint a one-time sign-in link (CLI, bearer only) |
| POST | `/v1/session` | new | yes | Exchange a sign-in token for a session cookie |
| GET | `/v1/session` | new | yes | Current session + CSRF token |
| DELETE | `/v1/session` | new | yes | Sign out |
| GET | `/v1/version` | new | yes (+ `features`) | Program and Postgres versions, what is built |
| GET | `/v1/setup` | new | partly: always `member` | Fresh / creating / joining / member / failed / left / removed |
| GET | `/v1/addresses` | new | yes | Address suggestions (netdetect) |
| POST | `/v1/setup/check-invite` | new | decode only, `probe` always `null` | Validate an invite code |
| POST | `/v1/setup/create` | new | 409 `already_set_up` | Start a new database on this computer |
| POST | `/v1/setup/join` | new | 409 `already_set_up` | Join a cluster with an invite code |
| POST | `/v1/setup/reset` | new | 409 `not_failed` | Start over after a failed join or leaving |
| GET | `/v1/status` | **exists** (+ fields) | yes | Cluster, members, health, warnings, problems (D66) |
| GET | `/v1/problem-codes` | new (D66) | yes | Every status problem code with its texts (§17a) |
| GET | `/v1/stream` | new | yes | SSE: live status, setup, activity |
| GET | `/v1/events` | new | yes | Activity log, newest first |
| POST | `/v1/invites` | **exists** (+ fields) | yes (`role` not enforced) | Create an invite code |
| GET | `/v1/invites` | new | yes (`role` always `data`) | Open invites |
| DELETE | `/v1/invites/{id}` | new | 501 | Cancel an invite |
| DELETE | `/v1/members/{node}` | **exists** | yes | Remove another member |
| PATCH | `/v1/members/{node}` | new | 501 | Rename a member |
| POST | `/v1/members/{node}/addresses` | **exists** | yes | Change the addresses other nodes use for a node (§9) |
| POST | `/v1/leave` | **exists** | yes | This node leaves the cluster |
| POST | `/v1/switch` | **exists** | yes | Planned switch-over |
| POST | `/v1/promote` | new | 501 | Forced promote of this node (2-node, main down) |
| GET | `/v1/settings` | **exists** (+ field) | yes (`cluster_name` always `""`) | Cluster settings |
| POST | `/v1/settings` | **exists** (+ key) | yes; `cluster_name` → 501 | Change cluster settings |
| GET | `/v1/node` | new | yes | This node: name, role, ports, addresses, dashboard access |
| PATCH | `/v1/node` | new | `false` only; `true` → 501 | Dashboard access from other devices |
| GET | `/v1/connection` | new | yes (+ `ca_file`, `ca_pem`) | How apps connect (no password) |
| POST | `/v1/connection/reveal` | new | yes | The database password, on explicit request |
| GET | `/v1/conninfo` | **exists** | yes, bearer only | Connection info with password, for the CLI (§12) |
| POST | `/v1/app-password` | **exists** | yes | New application password (§12) |
| GET | `/v1/backups` | new, M7 | yes | Backups, schedule, destination, saving status |
| POST | `/v1/backups` | new, M7 | yes | Take a full backup now |
| PUT | `/v1/backups/schedule` | new, M7 | yes | On/off, schedule, retention |
| PUT | `/v1/backups/destination` | new, M7 | yes | Folder or S3 destination |
| POST | `/v1/backups/key/reveal` | new, M7 | yes | Show the backup key |
| POST | `/v1/backups/check-destination` | new, M7 | yes (also before setup) | Test a folder or S3 place without saving (D65) |
| POST | `/v1/backups/check` | new, M7 | yes | Prove the newest backup restores (a job) |
| POST | `/v1/backups/restore` | new, M7 | yes | Restore to a moment into a new folder (dry run or a job) |
| POST | `/v1/backups/{id}/restore` | new, M7 | 501 | In-place restore: never served |
| GET | `/v1/agents` | new, M7 | 501 | MCP endpoint and agent keys |
| POST | `/v1/agents/keys` | new, M7 | 501 | Create a key for a coding agent |
| DELETE | `/v1/agents/keys/{id}` | new, M7 | 501 | Revoke an agent key |
| GET | `/v1/dev/status` and `POST /v1/dev/*` | **exists** | yes, bearer only | CLI test scaffolding; not used by the dashboard |

When this node is not a cluster member yet (setup state `fresh`, `creating`,
`joining`, `failed`, `left`, `removed`), every cluster endpoint (`/v1/status`
through `/v1/agents/*`) answers 409 `not_set_up`. `/v1/session*`,
`/v1/version`, `/v1/setup*`, `/v1/addresses` and `/v1/stream` always work.

**As built (D50):** `coyote setup`, and a background service whose folder
is not in a cluster, serve the local port before the cluster exists, with
only the setup routes (`/v1/session*`, `/v1/version`, `/v1/setup*`,
`/v1/addresses`, `/v1/stream`); every other `/v1` route answers 409
`not_set_up`. `coyote up` / `join` / `witness` still do the first run from
the command line and serve the port once the cluster is open.

---

## 4. Setup (first run)

### `GET /v1/setup` — new

What this computer is right now. The dashboard routes on `state`.

```json
{
  "state": "joining",
  "role": "data",
  "suggested_name": "alex-macbook",
  "platform": { "os": "darwin", "arch": "arm64" },
  "progress": {
    "operation": "join",
    "role": "data",
    "cluster_name": "Bakery",
    "from": "node-a",
    "started_at": "2026-11-20T14:01:00Z",
    "done": false,
    "steps": [
      { "id": "contact",  "state": "done" },
      { "id": "download", "state": "done", "bytes_done": 94371840, "bytes_total": 94371840 },
      { "id": "verify",   "state": "done" },
      { "id": "copy",     "state": "active", "percent": 42, "bytes_done": 1288490188, "bytes_total": 3067833856 },
      { "id": "follow",   "state": "pending" }
    ]
  }
}
```

| Field | Type | Meaning |
|---|---|---|
| `state` | `"fresh" \| "creating" \| "joining" \| "member" \| "failed" \| "left" \| "removed"` | `fresh`: never set up. `creating`/`joining`: work in progress (see `progress`). `member`: part of a cluster, use `/v1/status`. `failed`: the last create/join stopped; see `error`. `left`: this node left; `removed`: another node removed it (its data folder is kept). |
| `role` | `"data" \| "witness" \| null` | Role being set up, or held. |
| `suggested_name` | string | Valid node name from the host name (`pg.SanitizeName`). |
| `platform` | `{os, arch}` | This machine: Go `GOOS` / `GOARCH`. |
| `progress` | Progress, optional | Present while `creating`/`joining`, and kept after it finishes until the agent restarts (so a reloaded page still shows success). |
| `error` | Error body, optional | When `state` is `failed`: the same `{error, code, details}` shape as HTTP errors. |
| `removed_reason` | string, optional | When `left`/`removed`: plain words (the `agent.json` `removed` text). |

**Progress**

| Field | Type | Meaning |
|---|---|---|
| `operation` | `"create" \| "join"` | |
| `role` | `"data" \| "witness"` | |
| `cluster_name` | string, optional | Known once contact is made (join). |
| `from` | string, optional | Name of the node data is copied from. |
| `started_at` | time | |
| `done` | bool | All steps done. |
| `steps` | Step[] | In order. The set depends on the operation (below). |

**Step**

| Field | Type | Meaning |
|---|---|---|
| `id` | string | `contact`, `download`, `verify`, `prepare`, `start`, `copy`, `follow`. The UI owns the wording per id. |
| `state` | `"pending" \| "active" \| "done" \| "failed" \| "skipped"` | `skipped`: e.g. `download` when the Postgres build is already cached. |
| `percent` | int 0–100, optional | When the step can measure it. |
| `bytes_done`, `bytes_total` | int, optional | For `download` and `copy`. |
| `detail` | string, optional | Plain words, e.g. `"Trying 100.64.0.43:7481"`. |

Step sets: create (data) = `download, verify, prepare, start`; join (data) =
`contact, download, verify, copy, follow`; join (witness) = `contact, start`.

### `GET /v1/addresses` — new

The netdetect report, exactly as `netdetect.Report` marshals today, plus the
default pick.

```json
{
  "addresses": [
    { "addr": "100.64.0.43", "interface": "utun4", "class": "tailscale", "label": "Tailscale (100.64.0.43)", "suggested": true },
    { "addr": "192.168.1.20", "interface": "en0", "class": "lan", "label": "Home/office Wi-Fi (192.168.1.20)", "suggested": true, "wireless": true, "may_change": true,
      "warnings": ["This address was handed out automatically and may change. Reserve it in your router, or use Tailscale."] },
    { "addr": "203.0.113.7", "interface": "en1", "class": "public", "label": "Public internet address (203.0.113.7)", "suggested": false,
      "warnings": ["This address looks public: anyone on the internet could try to reach it. Use Tailscale or your home/office network instead."] }
  ],
  "skipped": [ { "interface": "bridge100", "addr": "192.168.64.1", "reason": "virtual machine network" } ],
  "warnings": [],
  "default": "100.64.0.43"
}
```

| Field | Type | Meaning |
|---|---|---|
| `addresses[]` | netdetect `Address` | Best first. `class`: `tailscale`, `vpn`, `lan`, `link-local`, `public`, `shared`, `other`, `loopback`. `suggested`: safe to pick. `warnings`: plain sentences to show next to it. |
| `skipped[]` | `{interface, addr?, reason}` | Left out, and why. |
| `warnings` | string[] | Report-level (`WarnOnlyWiFi`, `WarnNone`). |
| `default` | string \| null, *new field* | `Report.Default()`; `null` when nothing is suggested. |

### `POST /v1/setup/check-invite` — new

Request `{"code": "<pasted text>", "probe": true}`. `code` may contain spaces,
line breaks, quotes and dashes (decoded with `invite.Decode`). `probe`
(optional, default false) also contacts the cluster.

Response 200:

```json
{
  "valid": true,
  "cluster_id": "3fa9c2e17b4d4e0a9c1d2b3e4f5a6b7c",
  "expires": "2026-11-20T15:00:00Z",
  "addresses": ["100.64.0.43:7481"],
  "probe": {
    "reachable": true,
    "cluster_name": "Bakery",
    "platform": { "os": "linux", "arch": "amd64" },
    "data_nodes": 1,
    "witnesses": 0,
    "compatible": true
  }
}
```

`probe` is `null` when not asked or not supported. `probe.reachable: false`
means none of the addresses answered within 5 s (`probe` then has only
`reachable` and `tried: ["host:port", …]`). `compatible` is safety rule 1 for
a data node: same `os` and `arch` (a witness is always compatible).

Probe needs a new join-stream request: the joiner proves it holds the secret
(HMAC of a server nonce with the secret) and gets the public cluster facts
above **without** using up the secret. The CA fingerprint in the code is
pinned as for a real join.

Errors 422: `not_invite`, `invite_corrupt`, `invite_expired`
(`details.expired_at`), `invite_version`.

### `POST /v1/setup/create` — new

Request:

```json
{ "name": "node-a", "advertise": "100.64.0.43", "cluster_name": "Bakery" }
```

| Field | Type | Meaning |
|---|---|---|
| `name` | string | This node's name, `pg.ValidName` (1–48 of `a-z 0-9 -`, no leading/trailing `-`). |
| `advertise` | string | Address other nodes dial, normally one from `/v1/addresses`. IP or host name, no port (port is the agent port, 7481). |
| `cluster_name` | string, optional | Friendly name shown in dashboards, 1–64 printable characters. Default: `""` (dashboard shows "Your database"). |

Response 202: the `GET /v1/setup` body (`state: "creating"`). Progress
continues on the stream (`setup` events) and in `GET /v1/setup`.

Errors: 400 `bad_request` (with the rule broken), 409 `already_set_up`.

### `POST /v1/setup/join` — new

```json
{ "code": "coyote-04zt…", "name": "node-b", "advertise": "100.64.0.44", "role": "data" }
```

`role`: `"data"` (default) or `"witness"`. Same as `coyote join` / `coyote
witness`. Response 202 with the setup body (`state: "joining"`).

Failures are reported **asynchronously** (the join retries with backoff, D21)
in `state: "failed"` + `error`, except checks that can be done before
starting, which return at once:

| Code | HTTP | When |
|---|---|---|
| `not_invite`, `invite_corrupt`, `invite_version` | 422 | Code does not decode. |
| `invite_expired` | 422 | Past its expiry (local clock). |
| `bad_request` | 400 | Bad name or address. |
| `already_set_up` | 409 | Not `fresh`/`failed`. |

Asynchronous `error.code` values: `secret_unknown`, `secret_used`,
`secret_expired`, `wrong_role` (the code was made for the other role),
`duplicate_name`, `platform_mismatch`, `unreachable` (`details.tried`),
`copy_failed`, `disk_full`.

`platform_mismatch` details:

```json
{ "cluster": { "os": "linux", "arch": "amd64" }, "this": { "os": "darwin", "arch": "arm64" } }
```

### `POST /v1/setup/reset` — new

Allowed in states `failed`, `left` and `removed`; returns to `fresh`.

- `failed`: deletes the partial join (`join.pending.json`, half-copied data
  in its temp folder).
- `left` / `removed`: moves the old data folder aside (renamed with a
  timestamp, for example `data.old-20261120-142301`), never deletes it, so
  the person can recover it by hand. The response's `details.moved_to`
  names the folder.

Never deletes the data of a node that was a member. Response 200 with the
setup body. 409 `not_failed` in any other state.

---

## 5. Status

### `GET /v1/status` — exists (with additive fields)

This is `agent.Status` from `internal/agent/status.go`. Fields marked *new*
are additive.

```json
{
  "self": "9b1c…",
  "cluster": {
    "id": "3fa9…",
    "epoch": 7,
    "standby_eligible": true,
    "single_data_node_mode": "degrade_after",
    "primary_id": "9b1c…",
    "primary_name": "node-a",
    "leader_id": "9b1c…",
    "leader_name": "node-a",
    "sync_set": ["node-b"],
    "sync_k": 1,
    "name": "Bakery",
    "platform": { "os": "linux", "arch": "amd64" },
    "stale": false
  },
  "nodes": [
    {
      "id": "9b1c…", "name": "node-a", "role": "data", "suffrage": "voter", "status": "active",
      "addresses": ["100.64.0.43:7481"], "primary": true, "leader": true, "this": true,
      "in_sync_set": false, "reachable": true, "lag_bytes": -1,
      "report": { "id": "9b1c…", "name": "node-a", "role": "data", "state": "primary", "epoch": 7, "lease": true,
                  "postgres": { "running": true, "port": 7432, "role": "primary", "streaming": false } },
      "platform": { "os": "linux", "arch": "amd64" },
      "joined_at": "2026-11-18T09:12:00Z",
      "last_seen": "2026-11-20T14:03:10Z",
      "lag_seconds": null,
      "eligible": false,
      "agent_version": "0.6.0"
    }
  ],
  "warnings": [],
  "controller": { "leader": true, "view": { "lease_healthy": true, "lease_age_ns": 812000000, "writes_waiting": false, "one_copy": false } },
  "health": {
    "main_node": "node-a",
    "lease": "renewed",
    "copies": "Writes wait for a copy on node-b before they are confirmed.",
    "writes_wait": true,
    "one_copy": false,
    "failover_code": "",
    "writes_blocked": false,
    "switching": null,
    "promote": { "allowed": false }
  }
}
```

**Top level** (exists): `self` (this node's id), `cluster`, `nodes`,
`warnings` (plain sentences, server wording), `controller` (leader's
`controller.Report`, omitted when there is no leader or it did not answer),
`health`, and `waiting` (exists, D62): present only while this node has no
copy of the cluster state (no leader has reached it since it started):
`{ "for_ms": 42000, "peers": [ { "name": "n2", "addresses": ["192.168.1.12:7481"], "answers": false } ] }`
from its saved address book. Then `cluster`, `nodes` and `health` are
empty and `warnings` say what to do (`node set-address --local`).

**`problems`** (*new*, D66): always present (`[]` when fine). Each is a
problem with a stable code (§17a); its `summary` is also in `warnings`, so
a client showing both skips warnings equal to a problem's summary.

```json
{
  "code": "address_gone", "severity": "problem", "node": "node-b",
  "summary": "node-b's address changed: it was 192.168.1.20, it is now 192.168.1.35. The cluster still records 192.168.1.20, so the other computers cannot reach it.",
  "fix": "Record node-b's new address, from any computer of the cluster (it checks first that node-b answers there).",
  "details": "What happened…\n\nWhat it means…\n\nLikely causes: …",
  "evidence": ["The cluster records node-b at 192.168.1.20:7481 (the previous address).", "node-b's computer has now: 192.168.1.35:7481 …"],
  "commands": ["coyote node set-address node-b 192.168.1.35:7481"],
  "since": "2026-11-24T14:03:10Z",
  "address": { "recorded": ["192.168.1.20:7481"], "now": ["192.168.1.35:7481"], "suggested": ["192.168.1.35:7481"],
               "unreachable_from": ["node-a"], "one_way": false },
  "markdown": "### CoyoteDB problem `address_gone` on node-b …"
}
```

| Field | Meaning |
|---|---|
| `code` | Stable (§17a). Clients branch on it. |
| `severity` | `problem` (something does not work) or `warning`. |
| `node` | The computer it is about; absent for the whole cluster. |
| `summary` | One plain line for everyone. |
| `fix` | What to do, one plain sentence. |
| `details` | Long form, paragraphs split by a blank line: what happened, what it means, likely causes. |
| `evidence` | Facts: addresses, times, which node saw what. |
| `commands` | Exact CLI commands that fix it, in order. |
| `since` | When it started (as the node that noticed it says). |
| `address` | Address codes only: `recorded` (what the others dial, the previous address), `now` (what that computer has), `suggested` (what to record; the dashboard's "Use …" button), `unreachable_from` (node names), `one_way`. |
| `markdown` | Self-contained block for an AI agent: code, meaning, causes, evidence, commands. Never contains invite codes, passwords or keys. |

The texts are made in Go only (`internal/agent/problems.go`); `coyote
status` prints the same (`--details` for the long form).

**`cluster`** — `ClusterView`

| Field | Type | Status | Meaning |
|---|---|---|---|
| `id` | string | exists | Cluster id (32 hex). |
| `epoch` | int | exists | Bumps on every main-node change. |
| `standby_eligible` | bool | exists | Rule 9 flag. `false` = running on one copy. |
| `standby_eligible_since` | time, optional | exists | When it last changed. |
| `single_data_node_mode` | `"strict" \| "degrade_after"` | exists | |
| `primary_id`, `primary_name` | string | exists | `""` when no main node. |
| `leader_id`, `leader_name` | string | exists | Raft leader; `""` = no quorum. |
| `sync_set` | string[] | exists | Names in R. (Go marshals a nil slice as `null`; clients treat `null` as `[]`.) |
| `sync_k` | int | exists | |
| `name` | string | *new* | Cluster display name (`""` if unset). |
| `platform` | `{os, arch}` \| null | *new* | The data nodes' platform (rule 1). |
| `stale` | bool | *new* | `true` when this node cannot reach a leader and the members shown are its last known copy. **Go side:** status must then still list the last known members with `reachable: false` (today it shows an empty cluster, D22). |

**`nodes[]`** — `MemberView`

| Field | Type | Status | Meaning |
|---|---|---|---|
| `id`, `name` | string | exists | |
| `role` | `"data" \| "witness"` | exists | |
| `suffrage` | `"voter" \| "nonvoter"` | exists | |
| `status` | `"active" \| "needs_reclone"` | exists | `needs_reclone`: away too long (rule 8); copies everything again when back. |
| `addresses` | string[] | exists | `host:port` of the agent port. |
| `primary`, `leader`, `this` | bool | exists | |
| `in_sync_set` | bool | exists | |
| `reachable` | bool | exists | This node got its report just now. |
| `report` | NodeReport, optional | exists | The node's own report (below). |
| `replication` | string, optional | exists | `"streaming (sync)"`, `"streaming"`, `"not connected"` (copies only). |
| `lag_bytes` | int | exists | Flushed-WAL distance behind the main node; `-1` unknown. |
| `platform` | `{os, arch}` | *new* | From `Member.Platform`. |
| `joined_at` | time | *new* | From `Member.JoinedAt`. |
| `last_seen` | time \| null | *new* | Wall-clock time this agent last got any answer from that node (status RPC, ping, Raft). For `this`, now. |
| `lag_seconds` | number \| null | *new* | `pg_stat_replication.replay_lag` in seconds, copies only. |
| `eligible` | bool | *new* | May become the main node in a planned switch right now (in R, streaming, caught up enough). |
| `eligible_reason` | string, optional | *new* | Why not, in plain words, when `eligible` is false for a data copy. |
| `agent_version` | string, optional | *new* | That node's `coyote` version. |

**`nodes[].report`** — `NodeReport` (exists, unchanged). The dashboard uses:
`state` (`primary`, `standby`, `witness`, `cloning`, `rejoining`,
`waiting`, `starting`, `""`), `message` (server words for `waiting`),
`copy_progress` (0–100 while `cloning`), `rejoin_marker`, `lease`,
`postgres.running`, `postgres.streaming`, `postgres.stream_cut`.

**`health`** — `Health`

| Field | Type | Status | Meaning |
|---|---|---|---|
| `main_node` | string | exists | Name, or `"none"`. |
| `lease` | string | exists | `"renewed"`, `"not renewed for 7s"`, `"unknown"`. |
| `copies` | string | exists | Server sentence about where writes are saved. |
| `writes_wait` | bool | exists | A commit returns only after a copy has it (good). |
| `one_copy` | bool | exists | Running on one data node… |
| `one_copy_since` | time, optional | exists | …since. |
| `failover` | string, optional | exists | Server sentence: why the main node is not being replaced. |
| `failover_code` | string | *new* | Machine form of `failover`: `""`, `"two_nodes"` (no witness: manual only), `"no_quorum"`, `"no_eligible_copy"` (no copy is safe to promote: running on one copy, or behind), `"waiting_lease"` (main stopped answering; waiting out its lease before taking over), `"strict_waiting"` (strict mode, writes wait for the copy). |
| `writes_blocked` | bool | *new* | Commits are waiting for a copy that is not there (`controller.view.writes_waiting`). |
| `writes_blocked_since` | time, optional | *new* | |
| `degrade_in_ms` | int, optional | *new* | In `degrade_after` mode while blocked: ms until the main node carries on alone. |
| `switching` | Switching \| null | *new* | A switch-over or failover in progress (below). |
| `promote` | Promote | *new* | Whether a forced promote of **this** node is offered (§10). |

**`health.switching`** — new

```json
{ "kind": "automatic", "from": "node-a", "to": "node-c", "started_at": "2026-11-20T14:03:00Z",
  "step": "waiting_lease", "takeover_in_ms": 3800 }
```

`kind`: `planned`, `automatic`, `forced`. `to` may be `""` while the
controller is still waiting out the lease (it picks the most up-to-date copy
afterwards). `step`: `waiting_lease`, `cutting_streams`, `catching_up`,
`promoting`, `reconnecting`. `takeover_in_ms` only during `waiting_lease`.

**`health.promote`** — new

```json
{ "allowed": true, "since": "2026-11-20T13:58:41Z", "may_lose_since": "2026-11-20T13:40:02Z", "reason": "" }
```

`allowed` is true only when all hold: exactly two data nodes and no voting
witness (D17) — or a quorum is otherwise permanently gone — this node is a
data node, not the main node, and the main node has been unreachable for
longer than the lease. `since`: when the main node was last seen.
`may_lose_since`: rule 9's "may lose data since <time>": set when
`standby_eligible` is false (this copy stopped being kept in step at that
time). `reason`: why not, when `allowed` is false and it is relevant.

---

## 6. Live stream (Server-Sent Events)

### `GET /v1/stream` — new

`Content-Type: text/event-stream`. Works with the session cookie
(`EventSource` sends cookies; it cannot send headers, which is fine for a
GET). Query: `topics` (comma list of `status`, `setup`, `activity`; default
all available).

```
retry: 3000

event: status
data: {…the GET /v1/status body…}

event: setup
data: {…the GET /v1/setup body…}

event: activity
id: 000000000142
data: {…one Event (§7)…}

: ping
```

- On connect: one `setup` event, then one `status` event if a member.
- `status`: re-sent when anything in it changes (compare the JSON), at most
  once per second, and at least every 10 s.
- `setup`: on every progress change while `creating`/`joining`, at most 4 per
  second, plus the final state.
- `activity`: each new event. A reconnect with `Last-Event-ID` replays newer
  events first (up to 200).
- `: ping` comment every 15 s so proxies and the browser keep it open.
- When the session expires the server ends the stream; the reconnect gets
  401, the client then checks `GET /v1/session`.
- At most 8 open streams per session; the oldest is closed.

The dashboard falls back to polling `GET /v1/status` every 3 s if the
stream cannot be opened.

---

## 7. Activity

### `GET /v1/events` — new

Query: `limit` (1–200, default 50), `before` (an event id; page backwards).

```json
{
  "events": [
    { "id": "000000000142", "at": "2026-11-20T14:03:04Z", "kind": "main_changed", "severity": "warning",
      "node": "node-c", "from": "node-a", "reason": "automatic",
      "message": "node-c became the main node (node-a stopped responding)." }
  ],
  "more": true
}
```

| Field | Type | Meaning |
|---|---|---|
| `id` | string | Increasing, zero-padded, sortable. |
| `at` | time | When this node recorded it. |
| `kind` | string | See below. The dashboard words each kind itself and falls back to `message`. |
| `severity` | `"info" \| "good" \| "warning" \| "problem"` | |
| `node` | string, optional | Main subject (name). |
| `from` | string, optional | Previous main node (`main_changed`, `switch_*`). |
| `reason` | string, optional | `planned`, `automatic`, `forced` for main changes; free words otherwise. |
| `message` | string | Server sentence. |

Kinds: `cluster_created`, `member_joined` (with `role`), `member_left`,
`member_removed`, `member_renamed` (`from` = old name), `node_unreachable`,
`node_back`, `switch_started`, `switch_failed`, `main_changed`,
`forced_promote`, `one_copy_started`, `one_copy_ended`, `writes_blocked`,
`writes_resumed`, `copy_started`, `copy_finished`, `needs_reclone`,
`invite_created` (with `role`), `invite_cancelled`, `settings_changed`,
`agent_started`.

Storage (Go side): each agent appends to `<data-dir>/logs/events.jsonl` what it
observes; cluster-wide changes (members, main node, settings) are recorded by
every node when it applies the Raft entry, so every node shows them. Keep the
last 2000 entries or 30 days.

---

## 8. Invites

### `POST /v1/invites` — exists (with additive fields)

Request (exists: `ttl`; *new*: `role`):

```json
{ "ttl": "1h", "role": "witness" }
```

| Field | Type | Meaning |
|---|---|---|
| `ttl` | string, optional | Go duration, > 0, ≤ 168h. Default `1h`. (exists) |
| `role` | `"data" \| "witness"`, optional | *new*. Default `"data"`. Recorded with the secret; a join with the other role is refused (`wrong_role`). |

Response 200 (exists: `code`, `expires`):

```json
{
  "code": "coyote-04ztkgq1…",
  "expires": "2026-11-20T15:03:11Z",
  "id": "a1b2c3d4e5f6",
  "role": "witness",
  "command": "coyote witness coyote-04ztkgq1…",
  "platform": { "os": "linux", "arch": "amd64" }
}
```

| Field | Status | Meaning |
|---|---|---|
| `code`, `expires` | exists | |
| `id` | *new* | First 12 hex chars of the secret's SHA-256 (the `JoinSecrets` key). |
| `role` | *new* | |
| `command` | *new* | The exact line to run on the other computer. |
| `platform` | *new* | What a data node must match (rule 1), or null before the first data node exists. |

Errors (exists): 400 `bad_request` (ttl), 409/503 when there is no leader.

### `GET /v1/invites` — new

```json
{ "invites": [ { "id": "a1b2c3d4e5f6", "role": "data", "created_at": "…", "expires": "…", "used": false } ] }
```

Open (unused, unexpired) invites, newest first; also used ones from the last
24 h with `used: true` and `used_by` (node name). The code itself is never
returned again (only its hash is stored).

### `DELETE /v1/invites/{id}` — new

Cancels an open invite (removes the secret from Raft). 200
`{"message": "Invite cancelled."}`. 404 `unknown_invite`.

---

## 9. Members, leaving, renaming

### `DELETE /v1/members/{node}` — exists

Removes another member. Response 200 `{"message": "Removed node-c. Its database stops and it can no longer connect."}`.

Errors (exist): 404 `unknown_member`, 409 `is_self` (use leave), 409
`is_primary` (switch first), plus cluster errors (`no_leader`,
`last_voter`, …).

### `PATCH /v1/members/{node}` — new

Request `{"name": "kitchen-pc"}` (`pg.ValidName`). Response 200: the updated
`MemberView`. Errors: 400 `bad_request`, 409 `duplicate_name`.

**Go side note:** the member name is used as Postgres `application_name` and
for slot names today, so a rename must re-render `primary_conninfo` on that
node and keep the slot (slots are keyed per member). If that is not safe for
M6, return 501 `not_yet_available` and the dashboard shows rename as
unavailable.

### `POST /v1/members/{node}/addresses` — exists

What `coyote node set-address <node> HOST:PORT,... [--force]` does. Sets the
addresses other members use to reach `{node}` (name or id). Each address is
checked from this node first: a TCP connect plus the cluster's mutual-TLS
handshake, which must present **that node's own certificate** (a typo that
reaches another machine is caught). The record is pinned: the node keeps it
over what it detects itself, until it is started with an explicit
`--advertise`.

Request:

```json
{ "addresses": ["100.64.0.44:7481"], "force": false }
```

| Field | Type | Meaning |
|---|---|---|
| `addresses` | string[] | 1–8 `HOST:PORT` (duplicates and blanks dropped). |
| `force` | bool, optional | Record them even if none answers from here (the node is off, or only other nodes can reach it). |
| `local` | bool, optional | `node set-address --local` (D62): keep them on **this machine only**, without the cluster. For when no leader can be elected because the computers got new addresses. Needs no leader; `{node}` may be any member this node knows (from its saved address book). They are dialed before the recorded ones and forgotten once the cluster records them. Message: "This machine now reaches …". |

Response 200 (`agent.SetAddressResponse`):

```json
{
  "node": "node-b",
  "probes": [ { "addr": "100.64.0.44:7481", "ok": true, "rtt_ms": 3.1 } ],
  "recorded": true,
  "message": "Other nodes now reach node-b at 100.64.0.44:7481."
}
```

`recorded: false` means nothing changed: no address answered as that node
and `force` was not set; then `code` (D66) says why: `address_no_answer`
(nothing accepted the connection) or `address_not_this_node` (something
answered without that node's certificate), and each failed probe has the
same `code` (`message` is CLI wording that mentions `--force`;
the dashboard writes its own sentence). Errors: 400 `bad_request`
(not `HOST:PORT`, none or more than 8), 404 `unknown_member`, cluster codes
(`no_leader`, …). Without `local`, a node that has not heard from the
cluster since it started answers 409 `no_leader` (not `unknown_member`) and
names the `--local` form.

### `POST /v1/leave` — exists

This node leaves the cluster; its database stops; data stays on disk.
Response 200 `{"message": "Left the cluster."}`. Errors (exist): 409 when this
is the main node ("move that role first"), cluster errors.

---

## 10. Switch-over and forced promote

### `POST /v1/switch` — exists

Planned switch-over to a copy. Request `{"to": "node-b"}`. Blocks until done
(up to 5 minutes; progress is visible on the stream as `health.switching`).

Response 200 — `SwitchResult`:

```json
{ "from": "node-a", "to": "node-b", "epoch": 8, "message": "node-b is now the main node." }
```

Errors (exist): 400 `bad_request`, 409 `not_eligible`, `epoch_mismatch`,
`no_leader`, 504 `timeout`. A failure before the switch commits changes
nothing (D22).

### `POST /v1/promote` — new

Forced promote of **this** node, for two data nodes and no witness when the
main node is gone (D17, rule 9). Uses `ForceNewCluster` internally.

```json
{ "confirm": "node-b", "accept_data_loss": true }
```

| Field | Type | Meaning |
|---|---|---|
| `confirm` | string | Must equal this node's name exactly (the person typed it). |
| `accept_data_loss` | bool | Must be `true`: the person read that writes the old main node took alone (after `health.promote.may_lose_since`, or while cut off) are lost. |

Response 200:

```json
{ "from": "node-a", "to": "node-b", "epoch": 9, "forced": true,
  "message": "node-b is now the main node. node-a must copy the data again when it comes back." }
```

Errors: 400 `not_confirmed`, 409 `promote_not_allowed` (with
`health.promote.reason` as `error`), 409 `is_primary`.

After it, the old main node, when it returns, sees the higher forced epoch,
demotes itself and re-clones (D17 follow-up).

---

## 11. Settings and this node

### `GET /v1/settings` — exists (with an additive field)

```json
{
  "lease_duration": "10s",
  "lease_renew_interval": "1s",
  "single_data_node_mode": "degrade_after",
  "degrade_after": "30s",
  "gone_after": "30m0s",
  "cluster_name": "Bakery"
}
```

`cluster_name` is *new*. The rest exists (`agent.SettingsView`).

### `POST /v1/settings` — exists (with an additive key)

Request `{"set": {"single_data_node_mode": "strict"}}`. Keys (exist):
`lease_duration`, `lease_renew_interval`, `single_data_node_mode`
(`strict` | `degrade_after`), `degrade_after`, `gone_after`; *new*:
`cluster_name` (1–64 printable characters; `""` clears). Response 200: the
settings. Errors (exist): 400 `bad_request`, 400 `unsafe_timing`.

The dashboard only offers `single_data_node_mode`, `degrade_after` (as a
choice of 10 s / 30 s / 1 min / 5 min) and `cluster_name`. Lease timings stay
CLI-only.

### `GET /v1/node` — new

```json
{
  "id": "9b1c…",
  "name": "node-a",
  "role": "data",
  "platform": { "os": "linux", "arch": "amd64" },
  "data_dir": "/var/lib/coyote",
  "advertise": ["100.64.0.43:7481"],
  "ports": { "postgres": 5432, "api": 7480, "peer": 7481 },
  "service": true,
  "dashboard": { "remote": false, "urls": ["http://127.0.0.1:7480/"] }
}
```

`service`: running as the installed background service. `dashboard.urls`:
where this dashboard can be opened (loopback, plus advertised addresses when
`remote` is on).

### `PATCH /v1/node` — new

Request `{"dashboard_remote": true}`. Turns on serving the dashboard and API
on this node's advertised addresses (never a `public` or `shared` class
address; refused with 409 `unsafe_address`). Sign-in still needs a one-time
link (`coyote dashboard --host <address>`). Response 200: the node body.

---

## 12. Connection info

### `GET /v1/connection` — new

How apps connect. **No password in this response.**

```json
{
  "database": "app",
  "user": "app",
  "sslmode": "verify-full",
  "hosts": [
    { "node": "node-a", "host": "100.64.0.43", "port": 5432, "this": true, "reachable": true },
    { "node": "node-b", "host": "100.64.0.44", "port": 5432, "this": false, "reachable": true }
  ],
  "local": { "host": "127.0.0.1", "port": 5432 },
  "ca_file": "/var/lib/coyote/ca.crt",
  "ca_pem": "-----BEGIN CERTIFICATE-----\n…"
}
```

As built (D31): apps use role `app`, database `app`, over TLS only, and
verify the server against the cluster CA (`sslmode=verify-full`,
`sslrootcert=<ca file>`). `hosts`: every **data** node's addresses with its
client port; any one of them works (each forwards to the main node).
`local`: from this computer (`null` on a witness). `ca_file`: the CA
certificate on this computer; `ca_pem`: its text (public), for apps on other
computers. `pending: true` + `note` while the application role is not set
up yet.

The dashboard builds connection strings and driver snippets from these parts
(`src/lib/snippets.ts`); the multi-host form adds
`target_session_attrs=read-write`.

### `POST /v1/connection/reveal` — new

State-changing method on purpose (needs CSRF), so a stolen cookie alone
cannot read the password. Request `{}`. Response 200:

```json
{ "password": "…" }
```

Logged as an activity event? No: it is not a cluster change. The agent logs
"connection password revealed in the dashboard" at info level, without the
value. 409 `not_ready` while the application role is not set up.

### `GET /v1/conninfo` — exists, bearer only

The CLI's `coyote conninfo [--json]`: `agent.ConnInfo`.

```json
{
  "url": "postgresql://app:…@100.64.0.43:5432,100.64.0.44:5432/app?sslmode=verify-full&sslrootcert=%2Fvar%2Flib%2Fcoyote%2Fca.crt&target_session_attrs=read-write&connect_timeout=10",
  "hosts": ["100.64.0.43:5432", "100.64.0.44:5432"],
  "user": "app", "database": "app", "password": "…", "ca_file": "/var/lib/coyote/ca.crt"
}
```

`pending: true` and `note` (no `password`, no `url`) while the application
role is not set up. It carries the password in a GET, so a session cookie is
refused (403 `bearer_only`); the dashboard uses `/v1/connection` and
`/v1/connection/reveal`.

### `POST /v1/app-password` — exists

`coyote conninfo --new-password`. Request `{}`. Gives the application role a
new random password, committed in the cluster state; the main node applies
it within a few seconds; open sessions stay up, new logins need the new
password. Response 200: `ConnInfo` (above) with the new password. Errors:
409 `not_ready` (no database yet), cluster codes (`no_leader`, …).

---

## 13. Backups (M7, D63)

Served, and `features.backups` is **true**: the setup page's backup step,
the Backups page and the overview card use it (D65). Full design: `docs/DECISIONS.md` D63, D65
and `docs/ARCHITECTURE.md` "Backups and point-in-time restore". Short
version: every data node saves every change (WAL) to the backup place as it
happens, and a full copy is taken on a schedule (default every 24 h at
03:00 this computer's time, kept 7 days). A **folder** destination is per
node (each data node keeps a complete set in that folder on its own disk); an
**S3** destination is shared (one full backup for the cluster, taken by a
copy). Restores never touch the running cluster: they go into a new folder
(`POST /v1/backups/restore`, or `coyote backup restore`).

Secrets never appear in these answers: the S3 secret key is write-only, and
the backup key is only in `PUT /v1/backups/destination` (once, when it is
made) and `POST /v1/backups/key/reveal`.

### `GET /v1/backups`

`?refresh=1` re-reads the catalog from the destination now (otherwise it is
at most 5 minutes old).

```json
{
  "schedule": { "enabled": true, "every": "24h0m0s", "at": "03:00", "keep": "168h0m0s", "archive_timeout": "1m0s" },
  "destination": { "kind": "folder", "label": "/srv/coyote-backups", "per_node": true },
  "status": { "…": "same object as status.backup below" },
  "backups": [
    { "id": "20261124T090000Z-node-b", "kind": "scheduled", "state": "done", "node": "node-b",
      "started_at": "…", "finished_at": "…", "size_bytes": 32800000, "data_bytes": 98000000 }
  ],
  "check": { "…": "the last backup check started from the dashboard on this node (a job, below)" },
  "restore": { "…": "the last restore started from the dashboard on this node (a job, below)" }
}
```

`destination.same_disk` (folder only, omitted when false): the folder is on
the disk that holds this node's data folder. It protects from mistakes, not
from that disk failing; the dashboard and `coyote backup` say so.
`check` / `restore`: missing until one was started on this node; kept until
the agent restarts.

`backups`: newest first, from this node's view of the destination (for a
folder destination: this node's own folder). `kind`: `scheduled` | `manual`.
`state`: `running` (another taker's heartbeat is fresh) | `done` (finished
and every WAL segment from its start to its end is saved: restorable) |
`incomplete` (finished but WAL is missing, or a taker died; never used for a
restore, removed by retention). `size_bytes` is what is stored (compressed,
encrypted); `data_bytes` the data folder size it holds. Durations are Go
duration strings.

The same `status` object is `backup` in `GET /v1/status` (and the
dashboard status):

```json
{
  "enabled": true,
  "destination": { "kind": "s3", "label": "s3://my-bucket/coyote (https://s3.us-west-002.backblazeb2.com)", "per_node": false },
  "encrypted": true,
  "last_backup": { "id": "…", "kind": "scheduled", "state": "done", "node": "node-b", "started_at": "…", "finished_at": "…", "size_bytes": 1 },
  "restorable": { "from": "2026-11-17T09:00:41Z", "to": "2026-11-24T15:03:00Z", "gap_after": "" },
  "running": null,
  "next_backup_at": "2026-11-25T09:00:00Z",
  "archive": { "last_archived": "00000003000000000000002A", "last_archived_at": "…", "waiting_bytes": 0, "lag_ms": 0,
               "failing_since": null, "conflicts": 0 },
  "gap": null,
  "waiting": "", "waiting_since": null,
  "last_error": "", "last_error_at": null,
  "problem": ""
}
```

- `restorable`: the window `backup restore --to` accepts, from the oldest
  usable full backup's end to the newest saved change. Missing when nothing
  is restorable yet. `gap_after`: set when a WAL segment is missing after
  the newest backup (the window ends before it).
- `archive`: this node's own saving. `waiting_bytes` / `lag_ms`: WAL not yet
  saved and how old the oldest is. `failing_since`: saving has failed since
  then (Postgres keeps the WAL and retries). `conflicts`: segments another
  node had saved with different content (a dead branch after a failover;
  kept under `conflicts/`, not a problem by itself).
- `waiting`: why a due or asked-for full backup has not started yet, in
  plain words (WAL archiving not live on this node's database, a copy still
  replaying WAL from its clone); retried every 5 s; after 5 minutes it
  becomes `last_error`. `waiting_since`: when it started waiting.
- `gap`: this node gave up saving some WAL to protect its disk (more than
  2 GiB waiting, or under 1 GiB free) and recorded it here instead of
  letting Postgres fill the disk. `closed: true` once a full backup was
  taken after it. Restores never cross a gap.
- `problem`: one plain sentence when something needs the user (the
  dashboard shows it as a warning): an open gap, saving failing for over
  2 minutes, the last full backup failed, or changes waiting over 10
  minutes.

### `POST /v1/backups`

Take a full backup now. Request `{}`. 202:

```json
{ "message": "Full backup started on node-a, node-b, node-c.", "nodes": ["node-a", "node-b", "node-c"] }
```

Folder destination: every data node takes its own (forwarded to the other
data nodes). S3: the first node in the taker order (copies first, then the
main node). 409 `backups_off`, `no_backup_node`; 409 `not_data_node` when
called on a witness.

### `PUT /v1/backups/schedule`

Request: any of `{"enabled": true, "every": "24h", "at": "03:00", "keep":
"7d", "archive_timeout": "60s"}` (`keep` also takes `Nd`). 200: the
`schedule` object. Limits: `every` 1 hour…31 days, `keep` at least
`every`, `archive_timeout` 10s…1h. Needs every member on a version
with backups (otherwise 409 from the cluster).

### `PUT /v1/backups/destination`

```json
{ "kind": "folder", "folder": "/mnt/usb/coyote-backups", "encrypt": false }
{ "kind": "s3", "s3": { "endpoint": "https://s3.eu-central-003.backblazeb2.com", "bucket": "b", "prefix": "coyote",
                        "region": "", "access_key": "…", "secret_key": "…", "path_style": "" } }
```

`folder`: a full path, or `""` for "next to each node's data folder"
(`<data-dir>-backups`). Folder backups are encrypted only with
`encrypt: true`; S3 backups always are. Before saving, this node checks it
can write, read, list and delete there (400 `destination_failed` with the
reason otherwise). 200:

```json
{ "destination": { "kind": "s3", "label": "…", "per_node": false }, "encrypted": true, "new_key": "cybk-…" }
```

`new_key`: only when a backup key was made by this call. Show it once,
with "save this somewhere that is not one of these computers".

### `POST /v1/backups/key/reveal`

Request `{}`. 200 `{"key": "cybk-…"}`; 404 `no_key` when backups are not
encrypted. The dashboard asks for confirmation first, like
`/v1/connection/reveal`.

### `POST /v1/backups/check-destination`

Checks a destination **without saving it**, from this computer: the same
request body as `PUT /v1/backups/destination`. Also served by the setup page
before this computer is in a cluster (its backup step). 200 always:

```json
{ "ok": true, "kind": "folder", "label": "/mnt/usb/coyote-backups", "folder": "/mnt/usb/coyote-backups",
  "exists": false, "free_bytes": 1870000000000, "total_bytes": 2000000000000, "same_disk": false,
  "encrypted": false, "warnings": ["The folder does not exist yet; it is created when you save."] }
{ "ok": false, "kind": "s3", "label": "s3://b/coyote (https://…)", "encrypted": true,
  "problem": "The storage refused these keys. Check the access key ID and the secret key, … (403 SignatureDoesNotMatch)" }
```

Folder: refused when not a full path, inside the data folder (or holding
it), a file, not writable (a real test file is written and removed), or
with under 2 GiB free; warned when over 90% full or not there yet
(`folder: ""` checks the default, next to the data folder). S3: lists,
writes and deletes a test object. In a cluster, the S3 secret key may be
left out to test with the saved one. `problem` and `warnings` are plain
sentences that say what to do.

### `POST /v1/backups/check`

Proves the newest backup restores: restores it into a temporary folder
under the data folder's `backup/`, reads every page with `amcheck`, then
deletes it. Request `{}`. 202 with the job; 409 `job_running` (one check or
restore at a time on a node), `backups_off`, `not_data_node`. The job:

```json
{ "kind": "check", "state": "running", "started_at": "…", "lines": ["Restoring the newest backup into a temporary folder on node-a."] }
{ "kind": "check", "state": "done", "started_at": "…", "finished_at": "…", "backup_id": "20261124T190859Z-node-a",
  "last_change": "2026-11-24 13:09:34.832352-06", "databases": 3, "relations": 810, "took_ms": 8371, "lines": ["…"] }
{ "kind": "check", "state": "failed", "error": "…", "damage": ["table public.orders: page 812 …"], "stop_line": "…", "lines": ["…"] }
```

Follow it in `GET /v1/backups` (`check`). `lines`: the same progress lines
the CLI prints.

### `POST /v1/backups/restore`

Restore to a moment into a **new** folder on this computer. The running
database is never touched. Request:

```json
{ "to": "2026-11-24T14:05:00Z", "into": "/srv/coyote-restore-20261124-1405", "dry_run": true }
```

`to`: optional (empty: the newest saved change). `into`: a full path that
does not exist or is empty. With `dry_run: true`, 200 with the plan and no
change (`into` may be left out to get `suggested_into`):

```json
{ "ok": true, "into": "/srv/coyote-restore-20261124-1405", "suggested_into": "/srv/coyote-restore-20261124-1405",
  "free_bytes": 655851659264, "need_bytes": 32835072, "backup_id": "20261124T190859Z-node-a", "target": "…" }
{ "ok": false, "suggested_into": "…", "problem": "That is this database's own folder (or holds it). …" }
```

Refused: a moment outside `status.restorable`, a relative path, the data
folder or the backup folder (or inside them), a folder with files in it,
under `need_bytes × 1.1 + 256 MB` free. Without `dry_run`: 202 with the
job (`kind: "restore"`), 400 `restore_refused` with the plan's `problem`,
409 `job_running`. When done, the job has `into`, `backup_id`,
`last_change` and `next_command`: the exact `coyote up --data-dir …` line,
with ports free on this computer, that starts the copy as a new, separate
database. The dashboard shows it to copy.

`POST /v1/backups/{id}/restore` (in place) is not served (501): a restore
never replaces the running database.

---

## 14. Coding agents / MCP (M7)

Same 501 `not_yet_available` until M7.

### `GET /v1/agents`

```json
{
  "mcp": { "url": "http://127.0.0.1:7480/mcp", "transport": "streamable-http" },
  "keys": [ { "id": "k_7f3a", "name": "Claude Code on laptop", "access": "read_only", "created_at": "…", "last_used_at": "…" } ]
}
```

### `POST /v1/agents/keys`

Request `{"name": "Claude Code on laptop", "access": "read_only"}` (`read_only`
| `read_write`). 200: `{"key": {…as above…}, "secret": "cyk_…"}`; the secret
is shown once and stored hashed (`State.APIKeys`).

### `DELETE /v1/agents/keys/{id}`

200 `{"message": "Key revoked."}`.

---

## 15. Version

### `GET /v1/version` — new

```json
{ "product": "CoyoteDB", "version": "0.6.0", "commit": "4fcccd2", "built_at": "2026-11-20T12:00:00Z",
  "go_version": "go1.27.4", "os": "linux", "arch": "amd64", "postgres_version": "18.7.0", "api": 1,
  "features": { "setup": true, "invite_probe": true, "invite_role": false, "invite_cancel": true, "rename": false,
                "forced_promote": false, "cluster_name": false, "remote_dashboard": false,
                "app_password": true, "set_address": true, "backups": true, "agents": false } }
```

`version` is `"dev"` for local builds. `api` is this document's version.
`commit` and `built_at` are empty/omitted until release builds stamp them.
`features` (*new field*, M6): which parts of this document the agent really
serves; the dashboard hides a control whose feature is `false` (its route
answers 501 `not_yet_available`). A missing key means `false`.

---

## 16. Static files

- `GET /assets/*` and other files present in the embedded `dist/`: served
  with the right `Content-Type`; `/assets/*` (hashed names) get
  `Cache-Control: public, max-age=31536000, immutable`; everything else
  `no-cache`.
- Any other `GET` not under `/v1/` (for example `/login`, `/connect`,
  `/nodes`): `index.html` (the app routes in the browser).
- If the binary was built without the dashboard (empty `dist/`), every such
  path serves the fallback page from `dashboard.Fallback`.
- The headers in §2.5 apply to static files too.

---

## 17. Error codes

| Code | HTTP | Status | Meaning |
|---|---|---|---|
| `bad_request` | 400 | exists | Input wrong; `error` says which rule. |
| `unsafe_timing` | 400 | exists | Settings would break lease rules. |
| `forbidden` | 403 | exists | Not a loopback connection (bearer). |
| `unauthorized` | 401 | exists | Wrong/missing bearer token. |
| `failed` | 409 | exists | Generic failure (`apiErr` default). |
| `timeout` | 504 | exists | |
| `unknown_member`, `is_self`, `is_primary`, `no_primary` | 404/409 | exists | |
| cluster codes: `epoch_mismatch`, `not_bootstrapped`, `duplicate_name`, `platform_mismatch`, `secret_unknown`, `secret_used`, `secret_expired`, `not_eligible`, `in_sync_set`, `no_leader`, `not_leader`, `not_confirmed`, `last_voter`, `lease_renewed`, `fsm_halted` | 409 | exists (`internal/cluster/errors.go`) | |
| `unauthenticated` | 401 | new | No valid session. |
| `login_link_invalid` | 401 | new | Sign-in link expired, used or unknown. |
| `csrf_failed` | 403 | new | Missing/wrong `X-Coyote-CSRF` or `Origin`. |
| `bad_host` | 403 | new | `Host` header not allowed. |
| `bearer_only` | 403 | new | Endpoint is CLI-only. |
| `not_set_up` | 409 | new | Cluster endpoint called before this node is a member. |
| `already_set_up` | 409 | new | Create/join when already a member or busy. |
| `not_failed` | 409 | new | Reset when not failed. |
| `not_invite`, `invite_corrupt`, `invite_expired`, `invite_version` | 422 | new | Invite decode errors (`invite.Err*`). |
| `wrong_role`, `unreachable`, `copy_failed`, `disk_full` | (async) | new | Join failures in `setup.error`. |
| `unknown_invite` | 404 | new | |
| `promote_not_allowed` | 409 | new | |
| `unsafe_address` | 409 | new | Remote dashboard on a public address. |
| `backups_off`, `no_backup_node`, `not_data_node` | 409 | new (M7) | Backup now refused. |
| `destination_failed` | 400 | new (M7) | The backup destination does not work from this node. |
| `no_key` | 404 | new (M7) | Backups are not encrypted. |
| `not_yet_available` | 501 | new | Feature arrives later (`details.milestone`). |

---

### 17a. Status problem codes (D66)

Carried in `problems[].code` of `/v1/status`, printed by `coyote status`
as `[code]`, explained offline by `coyote explain CODE`, listed by
`GET /v1/problem-codes` (`{"codes":[{code, severity, title, meaning, causes[], fix, commands[], where}]}`).
`where` is `status` (a status problem) or `request` (an answer to a command).

| Code | Where | In plain words | How to fix |
|---|---|---|---|
| `address_gone` | status | This computer no longer has the address the cluster records for it (it was X, it is now Y); the others cannot reach it. | `coyote node set-address NODE NEW:PORT` from any computer (dashboard: "Use NEW"). No leader: `--local` on each other computer first. |
| `address_unreachable` | status | The others cannot connect to it, though it connects to them (one-way). | Open the cluster port (7481) on that computer, or record an address they can reach. |
| `address_no_answer` | request | `set-address`: nothing answered at the address, nothing changed. | Check the address and that the node runs; `--force` if only others reach it. |
| `address_not_this_node` | request | `set-address`: something answered, but not that node of this cluster. | Find which computer has that address now; use the right one. |
| `peers_unreachable` | status | No other node answers at any address this one knows. | Start the others; if addresses changed, `set-address --local` on each computer. |
| `waiting_for_cluster` | status | Not heard from the cluster since it started. | Wait; start the others. |
| `no_leader` | status, request | Not enough voting nodes reach each other: nothing can change. (`health.failover_code` `no_quorum`.) | Start or reconnect computers. |
| `no_main` | status | No node is the main node. | Wait for the failover; see the other problems. |
| `main_unreachable` | status | The main node does not answer this node. | Check its computer. |
| `node_unreachable` | status | A node does not answer this node. | Check its computer. |
| `writes_waiting` | status | Writes wait for a copy that is not connected. (`failover_code` `strict_waiting` in strict mode.) | Bring a copy back. |
| `one_copy` | status | Writes are saved on the main node only. | Bring the copy back. |
| `waiting_lease` | status | The main node stopped renewing; another takes over once its lease has surely run out. | Nothing; then bring the old one back. |
| `no_eligible_copy` | status | Main gone, no copy is safe to take over. | Bring the old main node back. |
| `two_nodes` | status | Main gone, two data nodes and no witness: nobody takes over by itself. | Bring it back; add a witness. |
| `needs_attention` | status | A node stopped and cannot fix it by itself. | Follow its message. |
| `node_waiting` | status | A node waits for something. | Usually nothing. |

Not coded yet (plain sentences in `warnings` only): mixed-system warnings,
the controller's notes, the backup problem line.

## 18. Go-side checklist

What `internal/api` (or today's `internal/agent/api.go` + `internal/control`)
must add for the dashboard. In rough order of need:

**Status after M6 (2026-11-21):** built: 1, 2, 3, 5, 6 (last-known members
come from this node's own copy of the state, marked `stale`), 7 (partly),
10 (`GET` only), 11, 12 (`GET /v1/version` only), and 13's 501s. Since
2026-11-22 (D50) also 4, and of 8 the invite probe (partly) and cancel. Not
built: invite roles (8), 9. Details in §19.

1. **Serve the dashboard**: `dashboard.FS()` at `/`, SPA fallback, cache and
   security headers (§2.5, §16).
2. **Auth guard**: accept the session cookie besides the bearer token; host
   check; CSRF + `Origin` check; `bearer_only` routes; no CORS.
3. **Session endpoints** + `coyote dashboard [--no-open] [--host H]` command.
4. **Agent runs before setup**: a fresh agent (service installed, no cluster)
   serves the API in `fresh` state; `GET /v1/setup`, `/v1/addresses`,
   `POST /v1/setup/{check-invite,create,join,reset}` drive what `coyote up` /
   `join` / `witness` do today, with step progress.
5. **`GET /v1/stream`** (SSE).
6. **Status additions**: `cluster.{name,platform,stale}`,
   `nodes[].{platform,joined_at,last_seen,lag_seconds,eligible,eligible_reason,agent_version}`,
   `health.{failover_code,writes_blocked,writes_blocked_since,degrade_in_ms,switching,promote}`;
   last-known members when there is no quorum.
7. **Activity log**: `events.jsonl` + `GET /v1/events` + stream topic.
8. **Invites**: `role` + response fields; `GET`/`DELETE /v1/invites`;
   `wrong_role` on join; invite probe for `check-invite`.
9. **`POST /v1/promote`** (forced, D17).
10. **`GET/PATCH /v1/node`**, remote dashboard access.
11. **`GET /v1/connection`**, `POST /v1/connection/reveal`.
12. **`cluster_name` setting**, `PATCH /v1/members/{node}` (rename),
    `GET /v1/version`.
13. **M7**: backups and agents endpoints; until then 501
    `not_yet_available` with `details.milestone`.

---

## 19. As built in M6: deviations (2026-11-21)

Where the Go side (`internal/agent/dash*.go`, D36) differs from the design
above. Each is either honest 501 or a documented narrowing; none weakens a
safety rule.

- **Setup (D50, 2026-11-22):** served before the cluster by `coyote setup`
  and by a waiting service. States sent: `fresh`, `creating`, `joining`,
  `member`, `failed` (never `left`/`removed`: such a folder stops the agent
  with a message, as before). Step order as it really happens: create =
  `download, verify, prepare, start`; join (data) = `download, verify,
  contact, copy, follow`; join (witness) = `contact, start`. `download` is
  `skipped` ("already on this computer") when the program files are cached.
  `cluster_name` in create: 501 `not_yet_available` when not empty.
  Join takes an extra optional `allow_mixed_os` (bool). Extra `error.code`
  values: `wrong_cluster`, `port_in_use`, `unfinished_join` (a join started
  earlier did not finish; join again with the same code, or reset),
  `failed`; cluster codes as in §17 (e.g. `mixed_os_off`). `error.error`
  is the plain message the command line prints. Reset (`failed` only) also
  forgets an unfinished join. `GET /v1/stream` before membership sends only
  `setup` events (on connect and on each change, at most 4 a second).
- **`check-invite` probe (D50):** `probe` is `{reachable, via?,
  cluster_verified, wrong_cluster?, tried?, message}`: the join stream is
  opened as a join does (TLS, the code's CA fingerprint checked) and nothing
  is sent, so the code is not used up. No `cluster_name`, `platform`,
  `data_nodes`, `witnesses` or `compatible` (needs a new join-protocol
  request); whether the secret is still unused is known only on join.
- **Invites:** `role` is accepted and changes `command`, but is not recorded
  with the secret, so it is not enforced (`wrong_role` is never sent) and
  `GET /v1/invites` reports every invite as `data`. `DELETE /v1/invites/{id}`
  cancels an unused invite (204; 204 again if already cancelled; 404
  `unknown_invite`; 409 `secret_used` / `secret_expired`). It marks the
  secret used by `cancelled` with the existing Raft command (D50);
  `GET /v1/invites` then shows it with `used: true, cancelled: true`; a
  joiner with that code is told it was already used.
- **Rename** (`PATCH /v1/members/{node}`): 501, as §9 allows.
- **Forced promote** (`POST /v1/promote`): 501. The program has no forced
  promote that runs on a live node (`cluster.ForceNewCluster` needs the node
  stopped), and M6 adds no new way to change the main node.
  `health.promote.allowed` is always `false`.
- **`cluster_name`:** not stored (no field in the Raft settings);
  `GET /v1/settings` sends `""`, setting it is 501, `cluster.name` is `""`.
- **Remote dashboard** (`PATCH /v1/node {"dashboard_remote": true}`): 501.
  Loopback only (D36).
- **Status:** `lag_seconds` is `0` when a copy's flushed WAL equals the main
  node's and `null` otherwise (replay lag is not collected yet).
  `agent_version` is set for this node only. `last_seen` is when *this* agent
  last got a status answer from that node (`null` until the first one since
  it started). `health.switching` shows a planned switch only while this
  agent runs it (a switch started on another node is not shown here), and an
  automatic takeover only during `waiting_lease`, with `to: ""`.
  `cluster.stale` is `true` whenever there is no Raft leader.
- **Activity log:** each agent records what it sees: Raft state changes
  (members, main node, one copy, settings, invites) and, from its own status
  check every 10 s (or the stream's, when one is open), `node_unreachable`,
  `node_back`, `writes_blocked`, `writes_resumed`. Not recorded yet:
  `cluster_created`, `member_left` (a leave shows as `member_removed`),
  `switch_started`, `switch_failed`, `copy_started`, `copy_finished`,
  `invite_cancelled`. Events from before this agent started are not back-filled.
- **`/v1/conninfo`** is bearer only (it returns the password by GET).
- **Extra fields:** `ConnectionView.{ca_file, ca_pem, pending, note}`,
  `version.features`.

