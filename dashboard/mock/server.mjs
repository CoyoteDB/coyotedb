#!/usr/bin/env node
// Mock CoyoteDB agent API for developing and testing the dashboard.
// Implements dashboard/API.md: sessions (one-time link → HttpOnly cookie,
// CSRF header + Origin check), Host check, security headers, SSE, and every
// /v1 endpoint, over switchable scenario presets. No dependencies.
//
//   node mock/server.mjs [--port 4330] [--scenario two-healthy] [--no-static] [--m7] [--real-features]
//
// --real-features (or MOCK_REAL_FEATURES=1, or POST /__mock/features
// {"real": true}) serves exactly the feature set of the real agent today
// (internal/agent dash.go dashFeatures): the routes of features that are off
// answer 501 not_yet_available like it does. Without it every feature is on.
//
// Mock-only routes (never part of the real API): GET /__mock/scenarios,
// POST /__mock/scenario {"id"}, GET /__mock/login-link, GET /__mock/codes,
// POST /__mock/expire-sessions, GET/POST /__mock/features {"real"}.
import { createHash, randomBytes, timingSafeEqual } from 'node:crypto';
import { existsSync, readFileSync, statSync } from 'node:fs';
import { createServer } from 'node:http';
import { extname, join, normalize } from 'node:path';
import * as W from './world.mjs';
import * as B from './backups.mjs';
import { PROBLEM_CODES } from './problems.mjs';

const args = process.argv.slice(2);
const flag = (n) => args.includes(n);
const opt = (n, d) => {
  const i = args.indexOf(n);
  return i >= 0 && args[i + 1] ? args[i + 1] : d;
};
const PORT = Number(opt('--port', process.env.PORT || 4330));
// In `npm run dev` the browser talks to Vite (UI_PORT), which proxies /v1 here.
const UI_PORT = Number(opt('--ui-port', PORT));
const SERVE_STATIC = !flag('--no-static');
const M7 = flag('--m7');
const DIST = join(import.meta.dirname, '..', 'dist');
const COOKIE = `coyote_session_${PORT}`;
const quiet = flag('--quiet');
let realFeatures = flag('--real-features') || process.env.MOCK_REAL_FEATURES === '1';

// What the real agent serves today (internal/agent/dash.go dashFeatures).
const REAL_FEATURES = {
  setup: true,
  invite_probe: true,
  invite_role: false,
  invite_cancel: true,
  rename: false,
  forced_promote: false,
  cluster_name: false,
  remote_dashboard: false,
  app_password: true,
  set_address: true,
  backups: true,
  agents: false,
};
function features() {
  if (realFeatures) return { ...REAL_FEATURES };
  const all = Object.fromEntries(Object.keys(REAL_FEATURES).map((k) => [k, true]));
  // Coding agents answer 501 unless --m7, so say so.
  return { ...all, agents: M7 };
}
const on = (k) => features()[k] === true;
const notYet = (res, msg, details) => fail(res, 501, 'not_yet_available', msg, details);

/** The real agent stores no cluster name yet (API.md §19). */
function applyFeatures(w) {
  if (realFeatures) w.clusterName = '';
}

let scenarioId = opt('--scenario', process.env.MOCK_SCENARIO || 'two-healthy');
let world;
const streams = new Set();

function load(idv) {
  const s = W.SCENARIOS.find((x) => x.id === idv);
  if (!s) throw new Error(`unknown scenario ${idv}`);
  scenarioId = idv;
  world = s.make();
  applyFeatures(world);
  world.onEvent = (e) => broadcast('activity', e, e.id);
  for (const c of streams) c.end();
  streams.clear();
}
load(scenarioId);

// ---- sessions (API.md §2) ----

const sha = (s) => createHash('sha256').update(s).digest('hex');
const loginTokens = new Map(); // sha → expires ms
const sessions = new Map(); // sha(id) → { csrf, expires, absolute }
const IDLE = 12 * 3600_000;
const ABSOLUTE = 7 * 86_400_000;

function mintLink() {
  const t = randomBytes(32).toString('base64url');
  loginTokens.set(sha(t), Date.now() + 120_000);
  return { url: `http://127.0.0.1:${UI_PORT}/login#t=${t}`, expires: new Date(Date.now() + 120_000).toISOString() };
}

function sessionBody(s) {
  return { csrf_token: s.csrf, expires_at: new Date(s.expires).toISOString(), absolute_expires_at: new Date(s.absolute).toISOString(), idle_timeout_s: IDLE / 1000 };
}

function getSession(req) {
  const m = new RegExp(`(?:^|;\\s*)${COOKIE}=([^;]+)`).exec(req.headers.cookie ?? '');
  if (!m) return null;
  const key = sha(m[1]);
  const s = sessions.get(key);
  if (!s) return null;
  if (Date.now() > s.expires || Date.now() > s.absolute) {
    sessions.delete(key);
    return null;
  }
  s.expires = Math.min(Date.now() + IDLE, s.absolute);
  return s;
}

const eq = (a, b) => {
  const x = Buffer.from(String(a));
  const y = Buffer.from(String(b));
  return x.length === y.length && timingSafeEqual(x, y);
};

// ---- HTTP helpers ----

const SECURITY = {
  'Content-Security-Policy':
    "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
  'X-Content-Type-Options': 'nosniff',
  'Referrer-Policy': 'no-referrer',
  'X-Frame-Options': 'DENY',
  'Cross-Origin-Opener-Policy': 'same-origin',
  'Cross-Origin-Resource-Policy': 'same-origin',
  'Permissions-Policy': 'camera=(), microphone=(), geolocation=()',
};

function send(res, status, body, headers = {}) {
  const data = body === undefined ? '' : JSON.stringify(body, null, 2);
  res.writeHead(status, { ...SECURITY, 'Cache-Control': 'no-store', ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}), ...headers });
  res.end(data);
}

const fail = (res, status, code, error, details) => send(res, status, { error, code, ...(details ? { details } : {}) });

async function readBody(req) {
  const chunks = [];
  let size = 0;
  for await (const c of req) {
    size += c.length;
    if (size > 1 << 20) throw new W.HttpError(400, 'bad_request', 'request body too large');
    chunks.push(c);
  }
  const text = Buffer.concat(chunks).toString('utf8').trim();
  if (!text) return {};
  try {
    return JSON.parse(text);
  } catch {
    throw new W.HttpError(400, 'bad_request', 'request body is not valid JSON');
  }
}

/** Strict decoding like control.ReadJSON's DisallowUnknownFields. */
function only(body, allowed) {
  for (const k of Object.keys(body)) if (!allowed.includes(k)) throw new W.HttpError(400, 'bad_request', `json: unknown field "${k}"`);
  return body;
}

const ALLOWED_HOSTS = new Set([PORT, UI_PORT].flatMap((p) => [`127.0.0.1:${p}`, `localhost:${p}`, `[::1]:${p}`]));

// ---- SSE (API.md §6) ----

let lastStatus = '';
let lastSetup = '';
let lastBeat = 0;

function sse(res, event, data, idv) {
  res.write(`${idv ? `id: ${idv}\n` : ''}event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
}

function broadcast(event, data, idv) {
  for (const c of streams) if (!c.setupOnly) sse(c, event, data, idv);
}

setInterval(() => {
  W.tick(world, 500);
  if (world.agentDown) return;
  const setupJson = JSON.stringify(W.setupView(world));
  if (setupJson !== lastSetup) {
    lastSetup = setupJson;
    for (const c of streams) if (c.setupOnly) c.write(`event: setup\ndata: ${setupJson}\n\n`);
  }
  if (world.setup.state === 'member') {
    const st = W.status(world);
    // Compare without volatile timestamps so "changed" means something changed.
    const key = JSON.stringify(st, (k, v) => (k === 'last_seen' || k === 'takeover_in_ms' ? undefined : v));
    const now = Date.now();
    if (key !== lastStatus || now - lastBeat > 10_000 || world.switching) {
      lastStatus = key;
      lastBeat = now;
      const json = JSON.stringify(st);
      for (const c of streams) if (!c.setupOnly) c.write(`event: status\ndata: ${json}\n\n`);
    }
  }
}, 500).unref();

setInterval(() => {
  for (const c of streams) c.write(': ping\n\n');
}, 15_000).unref();

// ---- routes ----

async function api(req, res, url) {
  const path = url.pathname.slice(3); // after /v1
  const method = req.method;
  const unsafe = method !== 'GET' && method !== 'HEAD';

  // Auth: session cookie (the mock has no bearer token).
  if (path === '/session/links') return fail(res, 403, 'bearer_only', 'this endpoint is for the coyote program on this computer only');
  if (path === '/session' && method === 'POST') {
    const origin = req.headers.origin;
    if (origin && origin !== `http://${req.headers.host}`) return fail(res, 403, 'csrf_failed', 'request came from another site');
    const b = only(await readBody(req), ['token']);
    const key = sha(String(b.token ?? ''));
    const exp = loginTokens.get(key);
    loginTokens.delete(key);
    if (!exp || Date.now() > exp) return fail(res, 401, 'login_link_invalid', 'this sign-in link has expired or was already used');
    const sid = randomBytes(32).toString('base64url');
    const s = { csrf: randomBytes(32).toString('base64url'), expires: Date.now() + IDLE, absolute: Date.now() + ABSOLUTE };
    sessions.set(sha(sid), s);
    return send(res, 200, sessionBody(s), { 'Set-Cookie': `${COOKIE}=${sid}; Path=/; HttpOnly; SameSite=Strict; Max-Age=${IDLE / 1000}` });
  }
  const s = getSession(req);
  if (!s) return fail(res, 401, 'unauthenticated', 'not signed in: run `coyote dashboard` on this computer');
  if (unsafe) {
    if (!eq(req.headers['x-coyote-csrf'] ?? '', s.csrf) || req.headers.origin !== `http://${req.headers.host}`)
      return fail(res, 403, 'csrf_failed', 'missing or wrong CSRF token');
  }
  if (path === '/session' && method === 'GET') return send(res, 200, sessionBody(s));
  if (path === '/session' && method === 'DELETE') {
    for (const [k, v] of sessions) if (v === s) sessions.delete(k);
    return send(res, 204, undefined, { 'Set-Cookie': `${COOKIE}=; Path=/; HttpOnly; SameSite=Strict; Max-Age=0` });
  }

  const w = world;
  const member = w.setup.state === 'member';
  const route = `${method} ${path}`;
  const port = PORT;

  switch (route) {
    case 'GET /version':
      return send(res, 200, { product: 'CoyoteDB', version: '0.6.0-mock', commit: 'mock', go_version: 'go1.27.4', os: w.thisPlatform.os, arch: w.thisPlatform.arch, postgres_version: '18.7.0', api: 1, features: features() });
    case 'GET /setup':
      return send(res, 200, W.setupView(w));
    case 'GET /addresses':
      return send(res, 200, {
        addresses: [
          { addr: '100.64.0.44', interface: w.thisPlatform.os === 'darwin' ? 'utun4' : 'tailscale0', class: 'tailscale', label: 'Tailscale (100.64.0.44)', suggested: true },
          { addr: '192.168.1.24', interface: w.thisPlatform.os === 'darwin' ? 'en0' : 'wlan0', class: 'lan', label: 'Home/office Wi-Fi (192.168.1.24)', suggested: true, wireless: true, may_change: true, warnings: ['This address was handed out automatically and may change. Reserve it in your router, or use Tailscale.'] },
          { addr: '203.0.113.7', interface: 'eth1', class: 'public', label: 'Public internet address (203.0.113.7)', suggested: false, warnings: ['This address looks public: anyone on the internet could try to reach it. Use Tailscale or your home/office network instead.'] },
        ],
        skipped: [{ interface: 'docker0', addr: '172.17.0.1', reason: 'container network' }],
        warnings: [],
        default: '100.64.0.44',
      });
    case 'POST /setup/check-invite': {
      const b = only(await readBody(req), ['code', 'probe']);
      // Without invite_probe the agent decodes the code but cannot ask the cluster.
      return send(res, 200, W.checkInvite(w, on('invite_probe') ? b : { ...b, probe: false }));
    }
    case 'POST /setup/create': {
      const b = only(await readBody(req), ['name', 'advertise', 'cluster_name']);
      if (b.cluster_name && !on('cluster_name')) return notYet(res, 'Naming the cluster is not available yet; leave the name empty.');
      W.create(w, b);
      return send(res, 202, W.setupView(w));
    }
    case 'POST /setup/join':
      W.join(w, only(await readBody(req), ['code', 'name', 'advertise', 'role', 'allow_mixed_os']));
      return send(res, 202, W.setupView(w));
    case 'POST /setup/reset':
      W.reset(w);
      return send(res, 200, W.setupView(w));
    case 'POST /backups/check-destination':
      // Served before setup too: the setup page's backup step (backup_jobs.go).
      if (!on('backups')) return notYet(res, 'Backups arrive in a later version.');
      return send(res, 200, B.checkDestination(only(await readBody(req), ['kind', 'folder', 's3', 'encrypt', 'skip_check'])));
    case 'GET /stream': {
      res.writeHead(200, { ...SECURITY, 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store', Connection: 'keep-alive' });
      res.write('retry: 3000\n\n');
      sse(res, 'setup', W.setupView(w));
      // Like the agent: a stream opened before this computer is a member is
      // the setup page's stream (setup events only, also after it becomes
      // one); a member's stream sends setup once, then status and activity.
      res.setupOnly = !member;
      if (member) {
        sse(res, 'status', W.status(w));
        const lastId = req.headers['last-event-id'];
        if (lastId) for (const e of [...w.events].reverse()) if (e.id > lastId) sse(res, 'activity', e, e.id);
      }
      streams.add(res);
      req.on('close', () => streams.delete(res));
      return;
    }
  }

  if (!member) return fail(res, 409, 'not_set_up', 'this computer is not in a cluster yet; finish the setup page first (or run `coyote up` / `coyote join`)');

  if (path === '/conninfo') return fail(res, 403, 'bearer_only', 'this endpoint is for the coyote program on this computer only');
  if (route === 'GET /problem-codes') return send(res, 200, { codes: PROBLEM_CODES });
  const addrRoute = /^\/members\/([^/]+)\/addresses$/.exec(path);
  if (addrRoute && method === 'POST') {
    if (!on('set_address')) return notYet(res, 'Changing addresses is not available yet.');
    return send(res, 200, W.setAddresses(w, decodeURIComponent(addrRoute[1]), only(await readBody(req), ['addresses', 'force'])));
  }
  if (method === 'DELETE' && path.startsWith('/members/')) return send(res, 200, W.removeMember(w, decodeURIComponent(path.slice(9))));
  if (method === 'PATCH' && path.startsWith('/members/')) {
    if (!on('rename')) return notYet(res, 'Renaming a computer is not available yet.');
    return send(res, 200, W.renameMember(w, decodeURIComponent(path.slice(9)), only(await readBody(req), ['name']).name));
  }
  if (method === 'DELETE' && path.startsWith('/invites/')) {
    if (!on('invite_cancel')) return notYet(res, 'Cancelling an invite is not available yet. An unused invite stops working when it expires.');
    // As internal/agent/dash_invite.go apiCancelInvite.
    const iv = w.invites.find((x) => x.id === decodeURIComponent(path.slice(9)));
    if (!iv) return fail(res, 404, 'unknown_invite', 'there is no such invite (it may have expired and been cleared)');
    if (iv.cancelled) return send(res, 204);
    if (iv.used) return fail(res, 409, 'secret_used', 'that invite was already used; to take that computer out, remove it from the cluster');
    if (Date.parse(iv.expires) <= Date.now()) return fail(res, 409, 'secret_expired', 'that invite has already expired; it cannot be used any more');
    iv.used = true;
    iv.cancelled = true;
    W.addEvent(w, 'invite_cancelled', 'info', {});
    return send(res, 204);
  }
  if (path.startsWith('/backups')) {
    if (!on('backups')) return fail(res, 501, 'not_yet_available', 'Backups arrive in a later version.', { milestone: 'M7' });
    if (!w.backup) return fail(res, 409, 'not_data_node', 'This node holds no data; ask a data node.');
    const b = w.backup;
    try {
      switch (route) {
        case 'GET /backups':
          return send(res, 200, B.backupsView(b));
        case 'POST /backups':
          only(await readBody(req), []);
          return send(res, 202, B.backupNow(b));
        case 'PUT /backups/schedule':
          return send(res, 200, B.setSchedule(b, only(await readBody(req), ['enabled', 'every', 'at', 'keep', 'archive_timeout'])));
        case 'PUT /backups/destination':
          return send(res, 200, B.setDestination(b, only(await readBody(req), ['kind', 'folder', 's3', 'encrypt', 'skip_check'])));
        case 'POST /backups/key/reveal':
          only(await readBody(req), []);
          return send(res, 200, B.revealKey(b));
        case 'POST /backups/check':
          only(await readBody(req), []);
          return send(res, 202, B.startCheck(b));
        case 'POST /backups/restore': {
          const r = only(await readBody(req), ['to', 'into', 'dry_run']);
          const out = B.restore(b, r);
          return send(res, r.dry_run ? 200 : 202, out);
        }
      }
      if (method === 'POST' && /^\/backups\/[^/]+\/restore$/.test(path))
        return fail(res, 501, 'not_yet_available', 'A restore never replaces the running database. Restore into a new folder (POST /v1/backups/restore, or `coyote backup restore --to TIME --into FOLDER`), check it, then start it with `coyote up --data-dir FOLDER`.', { milestone: 'M7' });
    } catch (e) {
      if (e instanceof B.BackupError) return fail(res, e.status, e.code, e.message);
      throw e;
    }
    return fail(res, 404, 'not_found', `no route ${method} /v1${path}`);
  }
  if (path.startsWith('/agents')) {
    if (!on('agents')) return fail(res, 501, 'not_yet_available', 'Coding agent access arrives in a later version.', { milestone: 'M7' });
    if (route === 'GET /agents') return send(res, 200, W.sampleAgents());
    if (route === 'POST /agents/keys') {
      const b = only(await readBody(req), ['name', 'access']);
      return send(res, 200, { key: { id: 'k_new', name: b.name, access: b.access, created_at: new Date().toISOString() }, secret: 'cyk_' + randomBytes(18).toString('base64url') });
    }
    return send(res, 200, { message: 'OK (mock).' });
  }

  switch (route) {
    case 'GET /status':
      return send(res, 200, W.status(w));
    case 'GET /events': {
      const limit = Math.min(200, Math.max(1, Number(url.searchParams.get('limit') ?? 50)));
      const before = url.searchParams.get('before');
      const list = before ? w.events.filter((e) => e.id < before) : w.events;
      return send(res, 200, { events: list.slice(0, limit), more: list.length > limit });
    }
    case 'POST /invites': {
      const inv = W.createInvite(w, only(await readBody(req), ['ttl', 'role']));
      // Without invite_role the role is not recorded: the list says "data" for every invite.
      if (!on('invite_role')) w.invites[0].role = 'data';
      return send(res, 200, inv);
    }
    case 'GET /invites':
      return send(res, 200, { invites: W.openInvites(w) });
    case 'POST /leave':
      return send(res, 200, W.leave(w));
    case 'POST /switch': {
      const b = only(await readBody(req), ['to']);
      if (!b.to) return fail(res, 400, 'bad_request', 'usage: switch --to <node>');
      return send(res, 200, await W.startSwitch(w, b.to, 'planned'));
    }
    case 'POST /promote':
      if (!on('forced_promote')) return notYet(res, 'Making a computer the main node by force is not available yet.');
      return send(res, 200, await W.promote(w, only(await readBody(req), ['confirm', 'accept_data_loss'])));
    case 'GET /settings':
      return send(res, 200, W.settingsView(w));
    case 'POST /settings': {
      const b = only(await readBody(req), ['set']);
      if (!on('cluster_name') && b.set && typeof b.set === 'object' && 'cluster_name' in b.set) return notYet(res, 'Naming the cluster is not available yet.');
      return send(res, 200, W.setSettings(w, b));
    }
    case 'GET /node':
      return send(res, 200, W.nodeView(w, port));
    case 'PATCH /node': {
      const b = only(await readBody(req), ['dashboard_remote']);
      if (!on('remote_dashboard') && b.dashboard_remote) return notYet(res, 'The dashboard is only served on this computer. Opening it from other devices is not available yet.');
      w.remote = !!b.dashboard_remote;
      return send(res, 200, W.nodeView(w, port));
    }
    case 'GET /connection':
      return send(res, 200, W.connection(w));
    case 'POST /connection/reveal':
      only(await readBody(req), []);
      return send(res, 200, { password: w.appPassword });
    case 'POST /app-password':
      only(await readBody(req), []);
      if (!on('app_password')) return notYet(res, 'Making a new password is not available yet.');
      return send(res, 200, W.newAppPassword(w));
  }
  return fail(res, 404, 'not_found', `no route ${method} /v1${path}`);
}

const TYPES = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8', '.svg': 'image/svg+xml', '.woff2': 'font/woff2', '.png': 'image/png', '.ico': 'image/x-icon', '.txt': 'text/plain; charset=utf-8', '.json': 'application/json' };

function serveStatic(req, res, url) {
  if (!SERVE_STATIC) return send(res, 404, { error: 'static files are served by Vite in dev', code: 'not_found' });
  let p = normalize(decodeURIComponent(url.pathname)).replace(/^(\.\.[/\\])+/, '');
  let file = join(DIST, p);
  if (!file.startsWith(DIST) || !existsSync(file) || statSync(file).isDirectory() || p.endsWith('.gitkeep')) file = join(DIST, 'index.html');
  if (!existsSync(file)) {
    res.writeHead(503, { ...SECURITY, 'Content-Type': 'text/plain' });
    return res.end('dist/ is empty: run `npm run build` first.');
  }
  const immutable = p.startsWith('/assets/');
  res.writeHead(200, { ...SECURITY, 'Content-Type': TYPES[extname(file)] ?? 'application/octet-stream', 'Cache-Control': immutable ? 'public, max-age=31536000, immutable' : 'no-cache' });
  res.end(readFileSync(file));
}

const server = createServer(async (req, res) => {
  const url = new URL(req.url, `http://${req.headers.host ?? 'x'}`);
  if (!ALLOWED_HOSTS.has(req.headers.host ?? '')) return fail(res, 403, 'bad_host', 'this address is not allowed');
  try {
    if (url.pathname.startsWith('/__mock/')) return await mockRoute(req, res, url);
    if (url.pathname.startsWith('/v1/')) {
      if (world.agentDown) return req.socket.destroy();
      if (!quiet) console.log(req.method, url.pathname);
      return await api(req, res, url);
    }
    if (req.method !== 'GET' && req.method !== 'HEAD') return fail(res, 405, 'bad_request', 'method not allowed');
    return serveStatic(req, res, url);
  } catch (e) {
    if (e instanceof W.HttpError) return fail(res, e.status, e.code, e.message, e.details);
    console.error(e);
    return fail(res, 500, 'failed', String(e?.message ?? e));
  }
});

async function mockRoute(req, res, url) {
  switch (`${req.method} ${url.pathname}`) {
    case 'GET /__mock/scenarios':
      return send(res, 200, { current: scenarioId, scenarios: W.SCENARIOS.map((s) => ({ id: s.id, label: s.label })) });
    case 'POST /__mock/scenario': {
      const b = await readBody(req);
      load(b.id);
      return send(res, 200, { current: scenarioId });
    }
    case 'GET /__mock/login-link':
      return send(res, 200, mintLink());
    case 'GET /__mock/codes':
      return send(res, 200, W.sampleCodes());
    case 'GET /__mock/features':
      return send(res, 200, { real: realFeatures, features: features() });
    case 'POST /__mock/features': {
      const b = await readBody(req);
      realFeatures = !!b.real;
      load(scenarioId);
      return send(res, 200, { real: realFeatures, features: features() });
    }
    case 'POST /__mock/expire-sessions':
      sessions.clear();
      for (const c of streams) c.end();
      streams.clear();
      return send(res, 200, { ok: true });
  }
  return fail(res, 404, 'not_found', 'unknown mock route');
}

server.listen(PORT, '127.0.0.1', () => {
  const link = mintLink();
  console.log(`CoyoteDB mock API on http://127.0.0.1:${PORT} · scenario "${scenarioId}"${SERVE_STATIC ? ' · serving dist/' : ''}${realFeatures ? ' · real agent features' : ''}`);
  console.log(`Sign in: ${link.url}`);
  console.log(`More links: curl http://127.0.0.1:${PORT}/__mock/login-link`);
});
