# CoyoteDB dashboard

The web dashboard that ships inside the `coyote` binary. The local agent serves
it. It is written for people who are not database experts. Every screen
answers one question: *is my data safe right now, and what should I do?*

- **Stack:** Preact + TypeScript (strict), built with Vite. No UI kit, no CDN,
  and no requests to anything except the local agent. Fonts are self-hosted
  (Aleo, Host Grotesk and Azeret Mono, all OFL; see
  `src/assets/fonts/LICENSES.txt`).
- **Contract:** [`API.md`](API.md) is the full `/v1` HTTP API: auth, SSE,
  error codes, which endpoints exist today and which are new.
  `src/api/types.ts` and `src/api/client.ts` mirror it, so change all three
  together.

Node ≥ 22.23.4 is required. On this machine, prefix commands with
`PATH="$HOME/.local/node/bin:$PATH"`.

## Run against the mock

```sh
npm ci
npm run dev        # Vite on http://127.0.0.1:4330, mock API on :4331
```

Open the sign-in link printed in the terminal. It is single-use and lasts
120 s, like the real `coyote dashboard`. To get a new one:
`curl http://127.0.0.1:4331/__mock/login-link`. In dev mode, a bar at the
bottom of the page switches scenarios.

To try the production build exactly as the agent will serve it (same CSP and
headers):

```sh
npm run build && npm run mock   # http://127.0.0.1:4330
```

Mock options: `--port`, `--scenario <id>`, `--m7` (coding agents return
sample data instead of 501), `--quiet`. `MOCK_SPEED=4` makes timed processes
(setup, copying, failover) run faster.

`--real-features` (or `MOCK_REAL_FEATURES=1`) makes the mock serve exactly the
`features` of today's real agent (`GET /v1/version`): `setup`,
`invite_probe`, `invite_cancel`, `app_password`, `set_address` and `backups` are on;
the routes of the others answer 501 `not_yet_available` like the agent does
(rename, invite role, forced promote, cluster name, remote dashboard,
agents; invites are listed as `data`, and a create with a
`cluster_name` is refused). Without it every feature is on (agents follow `--m7`).

In both modes the setup routes match the real agent (`internal/agent/setup.go`,
`dash_invite.go`): before this computer is a member only session, version,
setup, addresses, check-invite, create, join, reset and the stream answer,
and every other `/v1` route is 409 `not_set_up`. The step order is the
agent's (create: download, verify, prepare, start; join: download, verify,
contact, copy, follow; witness: contact, start; a later try skips the
download), the invite probe has the real shape (`reachable`, `via`,
`cluster_verified`, `wrong_cluster`, `tried`, `message`; no cluster name or
system), a stream opened before membership carries `setup` events only,
and `DELETE /v1/invites/{id}` answers 204 / 404 `unknown_invite` / 409
`secret_used` / `secret_expired` and lists the invite as `cancelled`. A
join named `node-a` stops with `duplicate_name`.

### Scenario presets

| id | What it shows |
|---|---|
| `fresh` | New install, setup wizard (start new or join) |
| `single` | One computer, no copy |
| `two-healthy` | Main node + copy, in sync |
| `two-copy-down` | Copy not responding: "running on one copy" |
| `two-main-down` | 2 nodes, main gone: forced switch-over with typed confirmation |
| `two-witness` | 2 data computers + witness |
| `three-healthy` | 3 data computers |
| `failover` | Automatic takeover in progress; finishes by itself |
| `rejoining` | A copy re-copying data, with progress |
| `needs-reclone` | A copy that was away too long |
| `needs-attention` | A copy whose database keeps stopping (agent state `needs-attention`) |
| `no-quorum` | Not enough computers to agree; database paused |
| `platform-mismatch` | Join from a different OS/CPU: `mixed_os_off`, "join anyway" ends in `platform_mismatch`, witness works |
| `invite-bad` | Expired/garbled codes caught on paste; join fails "already used" |
| `join-unreachable` | The invite check gets no answer; the join stops with `unreachable` |
| `join-wrong-cluster` | Another machine answers on the invite's address (`wrong_cluster`) |
| `unfinished-join` | An earlier join did not finish (state `failed`, `unfinished_join`) |
| `agent-down` | The agent stops answering (lost-contact and unreachable states) |
| `backups-first` | Backups just on, no full backup yet (a fresh create starts here too) |
| `backups-attention` | Saving changes fails (a USB drive that is gone): "needs attention" |
| `backups-off` | Backups turned off |
| `backups-s3` | Cloud storage, encrypted, with a backup key |
| `backups-incomplete` | One full backup is incomplete |
| `backups-check-fails` | A backup check finds damage |
| `backups-witness` | The dashboard opened on a witness (no backups there) |

Backups in the mock (`mock/backups.mjs`): folder paths starting `/mnt/`,
`/Volumes/` or `/media/` are another disk, `/readonly…` cannot be written,
`/full…` is too full; the S3 secret `wrong` and the bucket `missing` are
refused, an endpoint with `nowhere` cannot be reached. A restore into
`/var/lib/coyote`, `/etc…` or `/home/sam/notes` is "not empty".

Mock-only routes (`/__mock/*`): `scenarios`, `scenario` (POST `{id}`),
`login-link`, `codes` (sample valid, expired, garbled and messy invites),
`expire-sessions`, `features` (GET, or POST `{"real": true|false}` to switch
real-features mode).

In the mock, "Change addresses" succeeds for an address on port 7481 of a
computer that is up, and fails for one in a documentation range
(`192.0.2.x`, `198.51.100.x`, `203.0.113.x`).

## Checks

```sh
npm run typecheck   # tsc --noEmit, strict
npm run lint        # eslint (typescript-eslint strict + react-hooks)
npm test            # vitest: health sentences per scenario, invite codec (Go test vector), formatting, snippets, wording, setup failures
npm run build       # typecheck, vite build to dist/, gzip size report (fails above 400 KB)
npm run e2e         # Playwright against the mock serving dist/ (run build first)
```

`npm run e2e` goes through the wizard (create, join, bad codes, try again /
start over, mixed OS and witness, unreachable and wrong cluster, duplicate
name, unfinished join), every scenario in light and dark, every screen, and the
switch-over, promote, remove, settings, new password, CA certificate
download, change addresses and session flows, plus one pass in
real-features mode (screens and the setup page), and cancelling an invite. Each page gets axe
WCAG 2.1 AA checks, a sideways-scroll check and a CSP-violation check.
Screenshots at 360, 768 and 1440 px go to `screenshots/` (git-ignored). The
first run needs `npx playwright install --only-shell chromium`.

## How the embed works

`embed.go` is `package dashboard`:

- `//go:embed all:dist` embeds the build output.
- `FS()` returns it rooted at `dist/`.
- `Built()` reports whether `index.html` is there.
- `Fallback` (with `FallbackCSP`) is a self-contained page saying the
  dashboard was not included in this build.

`dist/` is git-ignored except for `dist/.gitkeep`, so `go build ./...` works
without Node and serves the fallback page. A release build runs
`npm ci && npm run build` in this folder before `go build`.

`npm install` writes `node_modules/go.mod` (`scripts/go-ignore.mjs`) so that
`go vet ./...` skips a `.go` file that ships inside one npm package.

Serving rules (API.md §16 and §2.5):

- Files come from `FS()`, and any other GET outside `/v1/` gets
  `index.html`.
- `assets/*` (hashed names) is cached immutable; everything else is
  `no-cache`.
- Every response gets the strict CSP and security headers. The build has no
  inline scripts, so the CSP needs no `unsafe-inline`.

## What the Go side still has to implement

Exists today (`internal/agent/api.go`): `GET /v1/status`,
`POST /v1/invites`, `DELETE /v1/members/{node}`, `POST /v1/leave`,
`POST /v1/switch`, `GET`/`POST /v1/settings` and `/v1/dev/*`.

New (details in API.md, checklist in API.md §18):

1. Serve the dashboard (`dashboard.FS()`, SPA fallback, headers).
2. Session auth:
   - `POST /v1/session/links` (bearer only), the `coyote dashboard` command,
     and `POST`/`GET`/`DELETE /v1/session`.
   - Cookie accepted alongside the bearer token.
   - CSRF + Origin check and the Host allowlist.
3. Agent runs before setup:
   - `GET /v1/setup` and `GET /v1/addresses`.
   - `POST /v1/setup/check-invite`, `create`, `join` and `reset`, with step
     progress.
4. `GET /v1/stream` (SSE: status, setup, activity).
5. Additive status fields:
   - `cluster.{name,platform,stale}`
   - `nodes[].{platform,joined_at,last_seen,lag_seconds,eligible,eligible_reason,agent_version}`
   - `health.{failover_code,writes_blocked,writes_blocked_since,degrade_in_ms,switching,promote}`
6. `GET /v1/events` (activity log).
7. Invites: the `role` field and response fields; `GET /v1/invites` and
   `DELETE /v1/invites/{id}`.
8. `POST /v1/promote` (forced promote with `accept_data_loss`).
9. `GET`/`PATCH /v1/node` (dashboard access from other devices).
10. `GET /v1/connection` and `POST /v1/connection/reveal`.
11. The `cluster_name` setting, `PATCH /v1/members/{node}` (rename) and
    `GET /v1/version`.
12. M7: `/v1/backups*` is served (D63, D65; `mock/backups.mjs` plays every
    state). `/v1/agents*`: until M7 it returns 501
    `not_yet_available`, and the UI shows a calm "coming in a later version"
    page.

The UI tolerates the fields that are not there yet: with only today's
`/v1/status`, the overview still works.

## Layout

```
API.md              the HTTP contract
embed.go            Go embed of dist/
index.html          entry (no inline scripts)
public/             favicon, theme-init.js (theme before first paint)
src/api/            types + typed client (mirror of API.md)
src/lib/            pure logic: health summary, invite codec, formatting, snippets, wording, problems
src/components/     UI pieces, node diagram, icons, ProblemBanner (problems with a code, API.md §5.1:
                    plain line, Show details, Copy for your AI agent, Use <new address>)
src/screens/        wizard, overview, add, connect, switch over, settings, activity, backups, agents, states
src/styles/         tokens (from the site) + app CSS
mock/               dependency-free mock agent (node mock/server.mjs); problems.mjs: problem
                    texts for the address-changed and address-one-way scenarios
tests/unit, tests/e2e
scripts/            dev runner, dist cleaner, size budget, go-ignore
```
