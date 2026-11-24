// The mock cluster: scenario presets, a small simulation (failover, copying,
// setup progress), and the /v1 handlers' logic. Shapes follow dashboard/API.md.
import { createHash, randomBytes } from 'node:crypto';
import { decodeInvite } from '../src/lib/invite.ts';
import { encodeInvite, garble } from './invite.mjs';
import { makeBackup, statusView, tickBackup } from './backups.mjs';
import { addressGone, addressUnreachable, afterSetAddress, answersAt } from './problems.mjs';

export const SPEED = Number(process.env.MOCK_SPEED || 1);
/** About 11 s from the main node's last lease renewal to a takeover (lease 10 s, D33). */
const TAKEOVER_MS = 11_000;
const MB = 1024 * 1024;
const LINUX = { os: 'linux', arch: 'amd64' };
const MAC = { os: 'darwin', arch: 'arm64' };
const CLUSTER_ID = '3fa9c2e17b4d4e0a9c1d2b3e4f5a6b7c';

const ago = (ms) => new Date(Date.now() - ms);
const iso = (d) => (d ? new Date(d).toISOString().replace(/\.\d{3}Z$/, 'Z') : undefined);
const id = (name) => createHash('sha256').update(name).digest('hex').slice(0, 32);

function node(name, o = {}) {
  return {
    id: id(name),
    name,
    role: 'data',
    platform: LINUX,
    addr: o.addr ?? `100.64.0.${40 + name.charCodeAt(name.length - 1) - 96}`,
    joinedAt: o.joinedAt ?? ago(3 * 86_400_000),
    up: true,
    phase: 'standby',
    progress: null,
    status: 'active',
    lastSeen: null,
    lagBytes: 0,
    lagSeconds: 0,
    sync: true,
    voter: true,
    message: '',
    version: '0.6.0',
    ...o,
  };
}

function baseWorld(o = {}) {
  return {
    scenario: o.scenario ?? 'custom',
    thisPlatform: LINUX,
    setup: { state: 'member', role: 'data', suggested_name: 'node-a', progress: null, error: null },
    clusterId: CLUSTER_ID,
    clusterName: 'Bakery',
    epoch: 4,
    mode: 'degrade_after',
    degradeAfter: '30s',
    nodes: [],
    self: '',
    primary: '',
    leader: '',
    standbyEligible: true,
    eligibleSince: ago(2 * 86_400_000),
    switching: null,
    writesBlocked: false,
    events: [],
    invites: [],
    remote: false,
    /** What check-invite's probe finds: 'ok', 'unreachable' or 'wrong_cluster'. */
    probe: 'ok',
    /** How a join fails at "contact": null, 'secret_used', 'mixed_os', 'unreachable' or 'wrong_cluster'. */
    joinError: null,
    /** PostgreSQL's program files are already on this computer (a later try skips the download). */
    downloaded: false,
    agentDown: false,
    appPassword: MOCK_PASSWORD,
    ...o,
  };
}

let eventSeq = 100;
export function addEvent(w, kind, severity, props = {}, at = new Date()) {
  const e = { id: String(++eventSeq).padStart(12, '0'), at: iso(at), kind, severity, message: props.message ?? kind.replace(/_/g, ' '), ...props };
  w.events.unshift(e);
  w.events = w.events.slice(0, 500);
  w.onEvent?.(e);
  return e;
}

function history(w, list) {
  // list: [msAgo, kind, severity, props], oldest first
  for (const [msAgo, kind, sev, props] of list) addEvent(w, kind, sev, props, ago(msAgo));
}

function member(w, nodes, selfName, primaryName, leaderName = primaryName) {
  w.nodes = nodes;
  w.self = nodes.find((n) => n.name === selfName).id;
  w.primary = primaryName ? nodes.find((n) => n.name === primaryName).id : '';
  w.leader = leaderName ? nodes.find((n) => n.name === leaderName).id : '';
  for (const n of nodes) if (n.id === w.primary) n.phase = 'primary';
  w.setup = { state: 'member', role: nodes.find((n) => n.id === w.self).role, suggested_name: selfName, progress: null, error: null };
  // Backups (§13): a data node reports them; a witness has none.
  if (w.setup.role === 'data' && w.backup === undefined) w.backup = makeBackup(w.backupKind ?? 'healthy', { node: selfName, checkFails: w.backupKind === 'check-fails' });
  if (w.setup.role !== 'data') w.backup = null;
  return w;
}

/** A healthy two-node cluster whose backups start in the given state. */
function backupScenario(id, label, kind) {
  return {
    id,
    label,
    make: () => member(baseWorld({ scenario: id, backupKind: kind }), [node('node-a'), node('node-b', { lagBytes: 0, voter: false })], 'node-a', 'node-a'),
  };
}

const witness = (name, o = {}) => node(name, { role: 'witness', phase: 'witness', platform: MAC, sync: false, lagBytes: -1, addr: '100.64.0.60', ...o });

// ---- scenarios ----

export const SCENARIOS = [
  { id: 'fresh', label: 'Fresh install', make: () => fresh({}) },
  {
    id: 'single',
    label: 'Single node',
    make: () => {
      const w = member(baseWorld({ scenario: 'single', standbyEligible: false }), [node('node-a')], 'node-a', 'node-a');
      history(w, [[3 * 86_400_000, 'cluster_created', 'good', { node: 'node-a' }]]);
      return w;
    },
  },
  {
    id: 'two-healthy',
    label: '2 nodes, healthy',
    make: () => {
      const w = member(baseWorld({ scenario: 'two-healthy' }), [node('node-a'), node('node-b', { lagBytes: 2048, voter: false })], 'node-a', 'node-a');
      history(w, [
        [3 * 86_400_000, 'cluster_created', 'good', { node: 'node-a' }],
        [3 * 86_400_000 - 600_000, 'member_joined', 'info', { node: 'node-b', role: 'data' }],
        [3 * 86_400_000 - 300_000, 'copy_finished', 'good', { node: 'node-b' }],
      ]);
      return w;
    },
  },
  {
    id: 'two-copy-down',
    label: '2 nodes, copy down (one copy)',
    make: () => {
      const w = member(
        baseWorld({ scenario: 'two-copy-down', standbyEligible: false, eligibleSince: ago(6 * 60_000 - 30_000) }),
        [node('node-a'), node('node-b', { up: false, lastSeen: ago(6 * 60_000), voter: false })],
        'node-a',
        'node-a',
      );
      history(w, [
        [86_400_000, 'cluster_created', 'good', { node: 'node-a' }],
        [6 * 60_000, 'node_unreachable', 'warning', { node: 'node-b' }],
        [6 * 60_000 - 30_000, 'one_copy_started', 'warning', { node: 'node-a' }],
      ]);
      return w;
    },
  },
  {
    id: 'two-main-down',
    label: '2 nodes, main down (manual switch)',
    make: () => {
      const w = member(
        baseWorld({ scenario: 'two-main-down' }),
        [node('node-a', { up: false, lastSeen: ago(4 * 60_000) }), node('node-b', { voter: false })],
        'node-b',
        'node-a',
        '', // node-a held the only vote (D17), so nothing can be decided without it
      );
      history(w, [[4 * 60_000, 'node_unreachable', 'problem', { node: 'node-a' }]]);
      return w;
    },
  },
  {
    id: 'two-witness',
    label: '2 nodes + witness',
    make: () => {
      const w = member(baseWorld({ scenario: 'two-witness' }), [node('node-a'), node('node-b', { lagBytes: 0 }), witness('pi-witness')], 'node-a', 'node-a');
      history(w, [
        [2 * 86_400_000, 'member_joined', 'info', { node: 'pi-witness', role: 'witness' }],
        [3 * 3_600_000, 'main_changed', 'info', { node: 'node-a', from: 'node-b', reason: 'planned' }],
      ]);
      return w;
    },
  },
  {
    id: 'three-healthy',
    label: '3 nodes, healthy',
    make: () => member(baseWorld({ scenario: 'three-healthy' }), [node('node-a'), node('node-b', { lagBytes: 512 }), node('node-c', { lagBytes: 1024, sync: true })], 'node-a', 'node-a'),
  },
  {
    id: 'address-changed',
    label: 'Address: a copy got a new address (address_gone)',
    make: () => {
      const w = member(
        baseWorld({ scenario: 'address-changed' }),
        [node('node-a'), node('node-b', { addr: '192.168.1.20', up: false, lastSeen: ago(4 * 60_000) }), node('node-c', { lagBytes: 1024 })],
        'node-a',
        'node-a',
      );
      w.problems = [addressGone('node-b', '192.168.1.20:7481', '192.168.1.35:7481', ['node-a', 'node-c'])];
      return w;
    },
  },
  {
    id: 'address-one-way',
    label: 'Address: others cannot connect in (address_unreachable)',
    make: () => {
      const w = member(
        baseWorld({ scenario: 'address-one-way' }),
        [node('node-a'), node('node-b'), node('node-c', { addr: '192.168.1.23', up: false, lastSeen: ago(90_000) })],
        'node-a',
        'node-a',
      );
      w.problems = [addressUnreachable('node-c', '192.168.1.23:7481', ['node-a', 'node-b'])];
      return w;
    },
  },
  {
    id: 'failover',
    label: 'Failover in progress',
    make: () => {
      const w = member(
        baseWorld({ scenario: 'failover' }),
        [node('node-a', { up: false, lastSeen: ago(2500) }), node('node-b'), node('node-c', { lagBytes: 0 })],
        'node-b',
        'node-a',
        'node-b',
      );
      // Lease 10 s (D33): a copy takes over about 11 s after the main node's
      // last renewal; node-a last renewed 2.5 s ago.
      w.switching = { kind: 'automatic', from: 'node-a', to: '', startedAt: ago(2500), step: 'waiting_lease', takeoverAt: Date.now() + (TAKEOVER_MS - 2500) / SPEED, next: 'node-c' };
      addEvent(w, 'node_unreachable', 'problem', { node: 'node-a' }, ago(2500));
      return w;
    },
  },
  {
    id: 'rejoining',
    label: 'Node copying with progress',
    make: () => {
      const w = member(
        baseWorld({ scenario: 'rejoining', standbyEligible: false, eligibleSince: ago(25 * 60_000) }),
        [node('node-a'), node('node-b', { phase: 'cloning', progress: 34, lagBytes: -1 }), witness('pi-witness')],
        'node-a',
        'node-a',
      );
      history(w, [
        [40 * 60_000, 'node_unreachable', 'warning', { node: 'node-b' }],
        [39 * 60_000, 'one_copy_started', 'warning', { node: 'node-a' }],
        [3 * 60_000, 'node_back', 'good', { node: 'node-b' }],
        [3 * 60_000, 'needs_reclone', 'warning', { node: 'node-b' }],
        [2 * 60_000, 'copy_started', 'info', { node: 'node-b' }],
      ]);
      return w;
    },
  },
  {
    id: 'needs-reclone',
    label: 'Node needs re-clone',
    make: () => {
      const w = member(
        baseWorld({ scenario: 'needs-reclone' }),
        [node('node-a'), node('node-b'), node('node-c', { status: 'needs_reclone', phase: 'waiting', message: 'This node was away too long; waiting to reach node-a to copy the data again…', lagBytes: -1, sync: false })],
        'node-a',
        'node-a',
      );
      history(w, [[50 * 60_000, 'node_unreachable', 'warning', { node: 'node-c' }], [60_000, 'needs_reclone', 'warning', { node: 'node-c' }]]);
      return w;
    },
  },
  {
    id: 'needs-attention',
    label: 'Node needs attention (database keeps stopping)',
    make: () => {
      const w = member(
        baseWorld({ scenario: 'needs-attention' }),
        [
          node('node-a'),
          node('node-b', {
            phase: 'needs-attention',
            message:
              "Needs attention: PostgreSQL keeps stopping (5 times in a row, last: postgres exited: exit status 1). Trying again in 1m0s. Check this machine's disk and memory; the details are in this node's log.",
            lagBytes: -1,
            sync: false,
          }),
          node('node-c', { lagBytes: 0 }),
        ],
        'node-a',
        'node-a',
      );
      history(w, [[20 * 60_000, 'node_unreachable', 'warning', { node: 'node-b' }], [18 * 60_000, 'node_back', 'good', { node: 'node-b' }]]);
      return w;
    },
  },
  {
    id: 'no-quorum',
    label: 'No quorum (cluster unreachable)',
    make: () => {
      const w = member(
        baseWorld({ scenario: 'no-quorum' }),
        [node('node-a'), node('node-b', { up: false, lastSeen: ago(90_000) }), node('node-c', { up: false, lastSeen: ago(95_000) })],
        'node-a',
        'node-a',
        '',
      );
      w.nodes[0].phase = 'waiting';
      w.nodes[0].message = 'Waiting to reach the other nodes…';
      history(w, [[95_000, 'node_unreachable', 'problem', { node: 'node-c' }], [90_000, 'node_unreachable', 'problem', { node: 'node-b' }]]);
      return w;
    },
  },
  backupScenario('backups-first', 'Backups: just turned on, no full backup yet', 'first'),
  backupScenario('backups-attention', 'Backups: saving fails (needs attention)', 'attention'),
  backupScenario('backups-off', 'Backups: off', 'off'),
  backupScenario('backups-s3', 'Backups: cloud storage (encrypted)', 's3'),
  backupScenario('backups-incomplete', 'Backups: one incomplete backup', 'incomplete'),
  backupScenario('backups-check-fails', 'Backups: a check finds damage', 'check-fails'),
  {
    id: 'backups-witness',
    label: 'Backups: dashboard opened on the witness',
    make: () => member(baseWorld({ scenario: 'backups-witness' }), [node('node-a'), node('node-b', { lagBytes: 0 }), witness('pi-witness')], 'pi-witness', 'node-a'),
  },
  { id: 'platform-mismatch', label: 'Join: different kind of computer (mixed OS)', make: () => fresh({ scenario: 'platform-mismatch', thisPlatform: MAC, joinError: 'mixed_os' }) },
  { id: 'invite-bad', label: 'Join: expired / garbled / used invite', make: () => fresh({ scenario: 'invite-bad', joinError: 'secret_used' }) },
  { id: 'join-unreachable', label: 'Join: cluster not reachable', make: () => fresh({ scenario: 'join-unreachable', probe: 'unreachable', joinError: 'unreachable' }) },
  { id: 'join-wrong-cluster', label: 'Join: another cluster answers', make: () => fresh({ scenario: 'join-wrong-cluster', probe: 'wrong_cluster', joinError: 'wrong_cluster' }) },
  {
    id: 'unfinished-join',
    label: 'Join: an earlier join did not finish',
    make: () => {
      const w = fresh({ scenario: 'unfinished-join', downloaded: true });
      w.setup = {
        ...w.setup,
        state: 'failed',
        error: { error: 'This computer started joining a cluster but did not finish. Join again with the same invite code to finish, or start over.', code: 'unfinished_join' },
      };
      return w;
    },
  },
  { id: 'agent-down', label: 'Agent unreachable', make: () => ({ ...SCENARIOS.find((s) => s.id === 'two-healthy').make(), agentDown: true, scenario: 'agent-down' }) },
];

function fresh(o) {
  const w = baseWorld({ scenario: 'fresh', ...o });
  w.setup = { state: 'fresh', role: null, suggested_name: o.thisPlatform === MAC ? 'alex-macbook' : 'office-mini', progress: null, error: null };
  w.thisAddr = '100.64.0.44';
  return w;
}

export function sampleCodes() {
  const addrs = ['100.64.0.41:7481'];
  const valid = encodeInvite({ clusterId: CLUSTER_ID, addrs, expires: new Date(Date.now() + 3_600_000) });
  return {
    valid,
    expired: encodeInvite({ clusterId: CLUSTER_ID, addrs, expires: new Date(Date.now() - 600_000) }),
    garbled: garble(valid),
    // A pasted code split over lines with surrounding spaces and quotes still works.
    messy: `  "${valid.slice(0, 60)}\n${valid.slice(60, 120)}\n  ${valid.slice(120)}"  `,
  };
}

// ---- status (API.md §5) ----

const byId = (w, i) => w.nodes.find((n) => n.id === i);
const dataNodes = (w) => w.nodes.filter((n) => n.role === 'data');

export function status(w) {
  const now = new Date();
  const primary = byId(w, w.primary);
  const leader = byId(w, w.leader);
  const witnessCount = w.nodes.filter((n) => n.role === 'witness').length;
  const dataCount = dataNodes(w).length;
  const primaryUp = !!primary && (primary.up || primary.id === w.self);
  const inR = (n) => n.role === 'data' && n.id !== w.primary && n.sync && n.status === 'active';
  const nodes = w.nodes.map((n) => {
    const reachable = n.id === w.self || n.up;
    const streaming = reachable && primaryUp && n.phase === 'standby';
    const v = {
      id: n.id,
      name: n.name,
      role: n.role,
      suffrage: n.voter ? 'voter' : 'nonvoter',
      status: n.status,
      addresses: n.addresses ?? [`${n.addr}:7481`],
      primary: n.id === w.primary,
      leader: n.id === w.leader,
      this: n.id === w.self,
      in_sync_set: inR(n),
      reachable,
      lag_bytes: -1,
      platform: n.platform,
      joined_at: iso(n.joinedAt),
      last_seen: reachable ? iso(now) : n.lastSeen ? iso(n.lastSeen) : null,
      lag_seconds: null,
      agent_version: n.version,
    };
    if (reachable) {
      v.report = {
        id: n.id,
        name: n.name,
        role: n.role,
        state: n.phase,
        epoch: w.epoch,
        ...(n.message ? { message: n.message } : {}),
        ...(n.progress != null && n.phase === 'cloning' ? { copy_progress: Math.floor(n.progress) } : {}),
        ...(n.status === 'needs_reclone' ? { rejoin_marker: 'recloning' } : {}),
        ...(n.role === 'data'
          ? { postgres: { running: n.phase === 'primary' || n.phase === 'standby', port: 7432, role: n.phase === 'primary' ? 'primary' : 'standby', streaming, ...(n.phase === 'needs-attention' ? { error: 'postgres exited: exit status 1' } : {}) } }
          : {}),
        ...(n.id === w.primary && n.phase === 'primary' ? { lease: true } : {}),
      };
    }
    if (n.role === 'data' && n.id !== w.primary && primaryUp) {
      v.replication = streaming ? (inR(n) && w.standbyEligible ? 'streaming (sync)' : 'streaming') : 'not connected';
      if (streaming) {
        v.lag_bytes = Math.max(0, n.lagBytes);
        v.lag_seconds = n.lagBytes > 0 ? n.lagSeconds : 0;
      }
    }
    if (n.role === 'data' && n.id !== w.primary) {
      v.eligible = streaming && inR(n) && w.standbyEligible;
      if (!v.eligible) v.eligible_reason = !reachable ? `${n.name} is not responding.` : n.phase === 'needs-attention' ? `${n.name} needs attention: its database is not running.` : n.phase === 'cloning' ? `${n.name} is still copying the data.` : `${n.name} is not caught up yet.`;
    }
    return v;
  });

  const sync = nodes.filter((n) => n.replication === 'streaming (sync)').map((n) => n.name);
  const health = {
    main_node: primary?.name ?? 'none',
    lease: !w.leader ? 'unknown' : primaryUp ? 'renewed' : `not renewed for ${Math.round((Date.now() - (primary?.lastSeen?.getTime() ?? Date.now())) / 1000)}s`,
    copies: sync.length
      ? `Writes wait for a copy on ${sync.join(' or ')} before they are confirmed.`
      : dataCount <= 1
        ? `One data node: writes are saved on ${primary?.name} only.`
        : `Running on one copy: writes are saved on ${primary?.name} only.`,
    writes_wait: sync.length > 0,
    one_copy: !w.standbyEligible,
    ...(w.standbyEligible ? {} : { one_copy_since: iso(w.eligibleSince) }),
    failover_code: '',
    writes_blocked: w.writesBlocked,
    switching: null,
    promote: { allowed: false },
  };
  if (!w.leader && w.nodes.filter((n) => n.voter).length > 1) {
    health.failover_code = 'no_quorum';
    health.failover = 'Not enough voting nodes can reach each other.';
  } else if (!primaryUp && dataCount === 2 && witnessCount === 0) {
    health.failover_code = 'two_nodes';
    health.failover = 'Two data nodes and no witness: if the main node dies, no other node takes over by itself. Add a witness to make that automatic.';
    const self = byId(w, w.self);
    if (self.role === 'data' && self.id !== w.primary) {
      health.promote = { allowed: true, since: iso(primary.lastSeen), ...(w.standbyEligible ? {} : { may_lose_since: iso(w.eligibleSince) }) };
    }
  } else if (primaryUp && dataCount === 2 && witnessCount === 0) {
    health.failover = 'Two data nodes and no witness: if the main node dies, no other node takes over by itself. Add a witness to make that automatic.';
  }
  if (w.switching) {
    const s = w.switching;
    health.switching = {
      kind: s.kind,
      from: s.from,
      to: s.to,
      started_at: iso(s.startedAt),
      step: s.step,
      ...(s.step === 'waiting_lease' ? { takeover_in_ms: Math.max(0, Math.round(s.takeoverAt - Date.now())) } : {}),
    };
    if (s.kind === 'automatic' && s.step === 'waiting_lease') health.failover_code = 'waiting_lease';
  }

  const warnings = [];
  if (!w.leader) warnings.push('The cluster has no leader right now: not enough voting nodes can reach each other. Nothing can change until they do.');
  if (primary && !primaryUp) warnings.push(`The main node (${primary.name}) is not reachable from here.`);
  for (const n of nodes) if (!n.reachable && !n.this) warnings.push(`${n.name} is not reachable.`);
  for (const n of w.nodes) if ((n.phase === 'waiting' || n.phase === 'needs-attention') && n.message && (n.up || n.id === w.self)) warnings.push(`${n.name}: ${n.message}`);
  if (health.failover && !warnings.includes(health.failover)) warnings.push(health.failover);
  // §5.1: every problem's summary is also a warning.
  const problems = w.problems ?? [];
  for (const p of problems) if (!warnings.includes(p.summary)) warnings.push(p.summary);

  return {
    self: w.self,
    cluster: {
      id: w.clusterId,
      epoch: w.epoch,
      standby_eligible: w.standbyEligible,
      ...(w.eligibleSince ? { standby_eligible_since: iso(w.eligibleSince) } : {}),
      single_data_node_mode: w.mode,
      primary_id: w.primary,
      primary_name: primary?.name ?? '',
      leader_id: w.leader,
      leader_name: leader?.name ?? '',
      sync_set: w.nodes.filter(inR).map((n) => n.name),
      sync_k: w.nodes.some(inR) ? 1 : 0,
      name: w.clusterName,
      platform: LINUX,
      stale: !w.leader,
    },
    nodes,
    warnings,
    problems,
    ...(w.backup ? { backup: statusView(w.backup) } : {}),
    ...(w.leader
      ? {
          controller: {
            leader: true,
            view: {
              lease_healthy: primaryUp,
              lease_age_ns: primaryUp ? 800_000_000 : (Date.now() - (primary?.lastSeen?.getTime() ?? Date.now())) * 1e6,
              writes_waiting: w.writesBlocked,
              one_copy: !w.standbyEligible,
            },
          },
        }
      : {}),
    health,
  };
}

export function setupView(w) {
  return {
    state: w.setup.state,
    role: w.setup.role,
    suggested_name: w.setup.suggested_name,
    platform: w.thisPlatform,
    ...(w.setup.progress ? { progress: w.setup.progress } : {}),
    ...(w.setup.error ? { error: w.setup.error } : {}),
    ...(w.setup.removed_reason ? { removed_reason: w.setup.removed_reason } : {}),
  };
}

// ---- simulation ----

/** Advance timed processes by dt milliseconds. */
export function tick(w, dt) {
  const k = (dt / 1000) * SPEED;
  tickBackup(w.backup, dt);
  // setup progress (the step order and fields of internal/agent/setup.go)
  const p = w.setup.progress;
  if (p && !p.done && (w.setup.state === 'creating' || w.setup.state === 'joining')) {
    const s = p.steps.find((x) => x.state === 'active');
    if (s) {
      s.elapsed = (s.elapsed ?? 0) + dt * SPEED;
      let finished;
      if (s.id === 'download') {
        s.bytes_done = Math.min(s.bytes_total, (s.bytes_done ?? 0) + 32 * MB * k);
        s.percent = Math.floor((s.bytes_done / s.bytes_total) * 100);
        finished = s.bytes_done >= s.bytes_total;
      } else if (s.id === 'copy') {
        // The agent reports the copy as a percentage only.
        s.percent = Math.min(100, Math.floor(s.elapsed / 40));
        finished = s.percent >= 100;
      } else {
        finished = s.elapsed >= (s.id === 'contact' ? 1400 : 1000);
      }
      if (finished) {
        delete s.elapsed;
        if (s.id === 'contact') {
          const e = joinFailure(w, p);
          if (e) {
            s.state = 'failed';
            w.setup.state = 'failed';
            w.setup.error = e;
            return;
          }
        }
        s.state = 'done';
        if (s.id === 'download') {
          w.downloaded = true;
          const v = p.steps.find((x) => x.id === 'verify');
          if (v) v.state = 'done'; // the agent checks the files as one step with the download
        }
        const next = p.steps.find((x) => x.state === 'pending');
        if (next) next.state = 'active';
        else finishSetup(w);
      }
    }
  }

  // switch-over / failover
  const sw = w.switching;
  if (sw) {
    sw.elapsed = (sw.elapsed ?? 0) + dt * SPEED;
    if (sw.kind === 'automatic' && sw.step === 'waiting_lease' && Date.now() >= sw.takeoverAt) {
      sw.step = 'promoting';
      sw.to = sw.next;
      sw.elapsed = 0;
    } else if (sw.step !== 'waiting_lease' && sw.elapsed > 900) {
      const order = ['cutting_streams', 'catching_up', 'promoting', 'reconnecting'];
      const i = order.indexOf(sw.step);
      sw.elapsed = 0;
      if (i < order.length - 1) sw.step = order[i + 1];
      else completeSwitch(w);
    }
  }

  // copying and rejoining nodes
  for (const n of w.nodes) {
    if (n.phase === 'cloning' && n.up) {
      n.progress = Math.min(100, (n.progress ?? 0) + 2.2 * k);
      if (n.progress >= 100) {
        n.phase = 'standby';
        n.progress = null;
        n.status = 'active';
        n.lagBytes = 0;
        addEvent(w, 'copy_finished', 'good', { node: n.name });
        if (!w.standbyEligible) {
          w.standbyEligible = true;
          w.eligibleSince = new Date();
          addEvent(w, 'one_copy_ended', 'good', { node: n.name });
        }
      }
    } else if (n.phase === 'rejoining') {
      n.rejoinMs = (n.rejoinMs ?? 0) + dt * SPEED;
      if (n.rejoinMs > 2500) {
        n.phase = 'standby';
        delete n.rejoinMs;
      }
    }
    // a little lag jitter on healthy copies
    if (n.phase === 'standby' && n.up && n.lagBytes >= 0) n.lagBytes = Math.random() < 0.7 ? 0 : Math.floor(Math.random() * 6000);
  }
}

function completeSwitch(w) {
  const sw = w.switching;
  const to = w.nodes.find((n) => n.name === sw.to);
  const from = w.nodes.find((n) => n.name === sw.from);
  w.epoch++;
  w.primary = to.id;
  to.phase = 'primary';
  to.voter = true;
  if (sw.kind === 'planned') {
    from.phase = 'rejoining';
    if (w.nodes.filter((n) => n.role === 'data').length === 2 && !w.nodes.some((n) => n.role === 'witness')) {
      from.voter = false;
      w.leader = to.id;
    }
  } else if (sw.kind === 'forced') {
    from.status = 'needs_reclone';
    from.voter = false;
    w.leader = to.id;
    w.standbyEligible = false;
    w.eligibleSince = new Date();
  }
  addEvent(w, 'main_changed', sw.kind === 'planned' ? 'info' : 'warning', { node: to.name, from: from.name, reason: sw.kind });
  w.switching = null;
  if (sw.resolve) sw.resolve({ from: from.name, to: to.name, epoch: w.epoch, message: `${to.name} is now the main node.`, ...(sw.kind === 'forced' ? { forced: true } : {}) });
}

function finishSetup(w) {
  const p = w.setup.progress;
  p.done = true;
  const name = w.pendingName;
  const thisNode = node(name, { addr: w.pendingAddr, platform: w.thisPlatform, joinedAt: new Date() });
  if (p.operation === 'create') {
    w.clusterName = w.pendingClusterName ?? '';
    w.standbyEligible = false;
    // A new cluster starts with backups on, next to the data folder, none taken yet (D63).
    w.backup = makeBackup('first', { node: name });
    member(w, [thisNode], name, name);
    addEvent(w, 'cluster_created', 'good', { node: name });
  } else {
    const a = node('node-a', { addr: '100.64.0.41', joinedAt: ago(5 * 86_400_000) });
    w.backup = undefined;
    if (p.role === 'witness') {
      thisNode.role = 'witness';
      thisNode.phase = 'witness';
      thisNode.sync = false;
      member(w, [a, node('node-b', { addr: '100.64.0.42' }), thisNode], name, 'node-a');
    } else {
      thisNode.voter = false;
      member(w, [a, thisNode], name, 'node-a');
    }
    addEvent(w, 'member_joined', 'info', { node: name, role: p.role });
    if (p.role === 'data') addEvent(w, 'copy_finished', 'good', { node: name });
  }
  w.setup.progress = p;
}

// ---- actions ----

export class HttpError extends Error {
  constructor(status, code, message, details) {
    super(message);
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

const nameRe = /^[a-z0-9](?:[a-z0-9-]{0,46}[a-z0-9])?$/;
function checkName(n) {
  if (typeof n !== 'string' || !nameRe.test(n)) throw new HttpError(400, 'bad_request', `node name "${n}" may only use a-z, 0-9 and inner '-' (1–48 characters)`);
}

/** The error a join stops with at "contact", in the agent's words (internal/agent/messages.go), or null. */
function joinFailure(w, p) {
  // The mock cluster a join reaches has node-a (and node-b, for a witness): see finishSetup.
  if ((p.role === 'witness' ? ['node-a', 'node-b'] : ['node-a']).includes(p.name)) return { error: 'Another node in the cluster already has this name. Choose a different one with --name.', code: 'duplicate_name' };
  switch (w.joinError) {
    case 'secret_used':
      return { error: 'That invite code was already used by another machine. Each code works once: run `coyote invite` on a machine in the cluster for a new one.', code: 'secret_used' };
    case 'unreachable':
      return {
        error: 'Could not reach the cluster. Is the machine that made the invite running and reachable from here? Progress is saved: run the same command again to continue. (dial tcp 100.64.0.41:7481: i/o timeout)',
        code: 'unreachable',
      };
    case 'wrong_cluster':
      return { error: 'The machine that answered is not the cluster this invite was made for, so nothing was sent to it. Check the address, or create a new invite.', code: 'wrong_cluster' };
    case 'mixed_os':
      if (p.role !== 'data') return null;
      if (!p.allowMixed)
        return {
          error:
            "This machine is a different kind of computer (operating system or processor) than the cluster's data nodes (this machine is darwin/arm64, but the cluster's data nodes (e.g. \"node-a\") are linux/amd64; mixing kinds of machines is off for this cluster). Mixing kinds of computers is new and off unless you ask for it: run the same command again with --allow-mixed-os. The cluster then checks that this machine reads and writes the data exactly like the others, and refuses it if not. It can always help the cluster as a witness (a voter that keeps no data): `coyote witness` with an invite code.",
          code: 'mixed_os_off',
        };
      return {
        error:
          "This machine cannot hold a copy of the data: this machine's database gives different answers than \"node-a\" (linux/amd64) in the behaviour check (text sorting). Copies need database programs that store and compare data exactly like the other data nodes'. It can still help the cluster as a witness (a voter that keeps no data): `coyote witness` with an invite code.",
        code: 'platform_mismatch',
      };
  }
  return null;
}

function pickName(w, n) {
  if (n === undefined || n === '') return w.setup.suggested_name;
  checkName(n);
  return n;
}

function checkAdvertise(a) {
  if (typeof a !== 'string') throw new HttpError(400, 'bad_request', 'json: cannot unmarshal advertise');
  if (a && (a.length > 253 || !/^[A-Za-z0-9.:_[\]-]+$/.test(a))) throw new HttpError(400, 'bad_request', `address ${a}: the address must be an IP address or a host name`);
}

/** Steps the agent sends, in the order they really happen. */
function stepsFor(w, op, role) {
  const download = w.downloaded
    ? { id: 'download', state: 'skipped', detail: 'already on this computer' }
    : { id: 'download', state: 'active', bytes_done: 0, bytes_total: 94 * MB, percent: 0 };
  const verify = { id: 'verify', state: w.downloaded ? 'done' : 'pending' };
  if (op === 'create') return [download, verify, { id: 'prepare', state: w.downloaded ? 'active' : 'pending' }, { id: 'start', state: 'pending' }];
  if (role === 'witness') return [{ id: 'contact', state: 'active' }, { id: 'start', state: 'pending' }];
  return [download, verify, { id: 'contact', state: w.downloaded ? 'active' : 'pending' }, { id: 'copy', state: 'pending' }, { id: 'follow', state: 'pending' }];
}

export function create(w, b) {
  const name = pickName(w, b.name);
  checkAdvertise(b.advertise ?? '');
  if (w.setup.state !== 'fresh' && w.setup.state !== 'failed') throw new HttpError(409, 'already_set_up', 'this computer is already being set up, or is set up');
  w.pendingName = name;
  w.pendingAddr = b.advertise || w.thisAddr;
  w.pendingClusterName = b.cluster_name;
  w.setup = {
    ...w.setup,
    state: 'creating',
    role: 'data',
    error: null,
    progress: { operation: 'create', role: 'data', started_at: iso(new Date()), done: false, steps: stepsFor(w, 'create', 'data') },
  };
}

export function checkInvite(w, b) {
  const r = decodeInvite(String(b.code ?? ''));
  if (!r.ok) {
    const code = r.problem === 'not_invite' || r.problem === 'empty' ? 'not_invite' : r.problem === 'version' ? 'invite_version' : 'invite_corrupt';
    throw new HttpError(422, code, code === 'not_invite' ? 'This is not an invite code (it should start with "coyote-").' : 'The invite code is incomplete or has a typo; copy the whole code again.');
  }
  if (r.expired) throw new HttpError(422, 'invite_expired', 'The invite code has expired; create a new one on a machine that is already in the cluster.', { expired_at: iso(r.invite.expires) });
  const addrs = r.invite.addresses;
  // Exactly what internal/agent/dash_invite.go probeInvite answers.
  let probe = null;
  if (b.probe) {
    if (w.probe === 'ok') {
      probe = { reachable: true, via: addrs[0], cluster_verified: true, message: `The cluster answered at ${addrs[0]}, and it is the cluster this code was made for.` };
    } else if (w.probe === 'wrong_cluster') {
      probe = {
        reachable: false,
        cluster_verified: false,
        wrong_cluster: true,
        tried: addrs,
        message: 'A machine answered, but it is not the cluster this code was made for, so nothing was sent to it. Check that the code is the newest one, or create a new invite.',
      };
    } else {
      const port = /:(\d+)$/.exec(addrs[0] ?? '')?.[1] ?? '7481';
      probe = {
        reachable: false,
        cluster_verified: false,
        tried: addrs,
        message: `No answer from ${addrs.join(', ')}. Check that the computer that made the code is running, that both computers are on the same network (or Tailscale), and that its firewall lets port ${port} in.`,
      };
    }
  }
  return { valid: true, cluster_id: r.invite.clusterId, expires: iso(r.invite.expires), addresses: addrs, probe };
}

export function join(w, b) {
  checkInvite(w, { code: b.code });
  const role = b.role === undefined || b.role === '' ? 'data' : b.role;
  if (role !== 'data' && role !== 'witness') throw new HttpError(400, 'bad_request', 'role must be "data" or "witness"');
  const name = pickName(w, b.name);
  checkAdvertise(b.advertise ?? '');
  if (w.setup.state !== 'fresh' && w.setup.state !== 'failed') throw new HttpError(409, 'already_set_up', 'this computer is already being set up, or is set up');
  w.pendingName = name;
  w.pendingAddr = b.advertise || w.thisAddr;
  const progress = { operation: 'join', role, started_at: iso(new Date()), done: false, steps: stepsFor(w, 'join', role) };
  // Mock-only bookkeeping, kept off the wire.
  Object.defineProperty(progress, 'name', { value: name, enumerable: false });
  Object.defineProperty(progress, 'allowMixed', { value: role === 'data' && b.allow_mixed_os === true, enumerable: false });
  w.setup = { ...w.setup, state: 'joining', role, error: null, progress };
}

export function reset(w) {
  if (!['failed', 'left', 'removed'].includes(w.setup.state)) throw new HttpError(409, 'not_failed', 'there is no failed setup to start over');
  const keep = { thisPlatform: w.thisPlatform, joinError: w.joinError, probe: w.probe, downloaded: w.downloaded };
  Object.assign(w, fresh(keep), { onEvent: w.onEvent, scenario: w.scenario });
}

export function createInvite(w, b) {
  if (!w.leader) throw new HttpError(503, 'no_leader', 'could not create an invite (the cluster must have a leader): the cluster has no leader right now');
  let ttl = 3_600_000;
  if (b.ttl) {
    const m = /^(\d+)(m|h)$/.exec(b.ttl);
    if (!m) throw new HttpError(400, 'bad_request', '--ttl must be a duration like 30m or 2h, at most 168h');
    ttl = Number(m[1]) * (m[2] === 'h' ? 3_600_000 : 60_000);
    if (ttl <= 0 || ttl > 168 * 3_600_000) throw new HttpError(400, 'bad_request', '--ttl must be a duration like 30m or 2h, at most 168h');
  }
  const role = b.role === 'witness' ? 'witness' : 'data';
  const expires = new Date(Math.floor((Date.now() + ttl) / 1000) * 1000);
  const secret = randomBytes(32);
  const code = encodeInvite({ clusterId: w.clusterId, addrs: w.nodes.slice(0, 3).map((n) => `${n.addr}:7481`), expires, secret });
  const inviteId = createHash('sha256').update(secret).digest('hex').slice(0, 12);
  w.invites.unshift({ id: inviteId, role, created_at: iso(new Date()), expires: iso(expires), used: false });
  addEvent(w, 'invite_created', 'info', { role });
  return { code, expires: iso(expires), id: inviteId, role, command: `coyote ${role === 'witness' ? 'witness' : 'join'}`, platform: LINUX };
}

/** GET /v1/invites: unused unexpired ones, and used (or cancelled) ones from the last 24 h, newest first. */
export function openInvites(w) {
  const now = Date.now();
  return w.invites.filter((iv) => (iv.used ? now - Date.parse(iv.created_at) <= 86_400_000 : Date.parse(iv.expires) > now));
}

export function findNode(w, ref) {
  const n = w.nodes.find((x) => x.name === ref || x.id === ref);
  if (!n) throw new HttpError(404, 'unknown_member', `no node called "${ref}" in this cluster`);
  return n;
}

export function removeMember(w, ref) {
  const n = findNode(w, ref);
  if (n.id === w.self) throw new HttpError(409, 'is_self', 'that is this node; use `coyote leave` instead');
  if (n.id === w.primary) throw new HttpError(409, 'is_primary', `${n.name} is the main node; move that role first (coyote switch --to <node>)`);
  if (!w.leader) throw new HttpError(409, 'no_leader', 'the cluster has no leader right now');
  w.nodes = w.nodes.filter((x) => x !== n);
  if (dataNodes(w).length === 1) {
    w.standbyEligible = false;
    w.eligibleSince = new Date();
  }
  addEvent(w, 'member_removed', 'warning', { node: n.name });
  return { message: `Removed ${n.name}. Its database stops and it can no longer connect.` };
}

export function renameMember(w, ref, name) {
  const n = findNode(w, ref);
  checkName(name);
  if (w.nodes.some((x) => x.name === name && x !== n)) throw new HttpError(409, 'duplicate_name', 'a member with this name already exists');
  const old = n.name;
  n.name = name;
  if (w.switching?.to === old) w.switching.to = name;
  addEvent(w, 'member_renamed', 'info', { node: name, from: old });
  return status(w).nodes.find((x) => x.id === n.id);
}

export function leave(w) {
  if (w.self === w.primary) throw new HttpError(409, 'failed', 'this node is the main node; move that role first (coyote dev switch --to <node>), then leave');
  w.setup = { state: 'left', role: null, suggested_name: byId(w, w.self).name, progress: null, error: null, removed_reason: 'left the cluster' };
  return { message: 'Left the cluster.' };
}

export function startSwitch(w, to, kind) {
  return new Promise((resolve, reject) => {
    const t = w.nodes.find((n) => n.name === to);
    if (!t) return reject(new HttpError(400, 'bad_request', `no node called "${to}"`));
    const st = status(w).nodes.find((n) => n.id === t.id);
    if (kind === 'planned' && !st.eligible) return reject(new HttpError(409, 'not_eligible', `node may not become primary: ${st.eligible_reason ?? ''}`.trim()));
    const from = byId(w, w.primary);
    w.switching = { kind, from: from.name, to: t.name, startedAt: new Date(), step: kind === 'forced' ? 'promoting' : 'cutting_streams', resolve };
    addEvent(w, 'switch_started', 'info', { node: t.name, from: from.name });
  });
}

export function promote(w, b) {
  const st = status(w);
  const self = byId(w, w.self);
  if (!st.health.promote?.allowed) throw new HttpError(409, 'promote_not_allowed', 'a forced promote is only possible with two data nodes and no witness, from the copy, while the main node is down');
  if (b.confirm !== self.name || b.accept_data_loss !== true) throw new HttpError(400, 'not_confirmed', 'confirmation does not match');
  addEvent(w, 'forced_promote', 'warning', { node: self.name });
  return startSwitch(w, self.name, 'forced');
}

const SETTINGS_KEYS = ['lease_duration', 'lease_renew_interval', 'single_data_node_mode', 'degrade_after', 'gone_after', 'cluster_name'];
export function settingsView(w) {
  return { lease_duration: '10s', lease_renew_interval: '1s', single_data_node_mode: w.mode, degrade_after: w.degradeAfter, gone_after: '30m0s', cluster_name: w.clusterName };
}

export function setSettings(w, b) {
  if (!b.set || typeof b.set !== 'object' || !Object.keys(b.set).length) throw new HttpError(400, 'bad_request', 'usage: settings set key=value ...');
  for (const [k, v] of Object.entries(b.set)) {
    if (!SETTINGS_KEYS.includes(k)) throw new HttpError(400, 'bad_request', `unknown setting "${k}" (known: ${SETTINGS_KEYS.join(', ')})`);
    if (k === 'single_data_node_mode' && v !== 'strict' && v !== 'degrade_after') throw new HttpError(400, 'bad_request', 'single_data_node_mode must be "strict" or "degrade_after"');
    if (k === 'cluster_name' && String(v).length > 64) throw new HttpError(400, 'bad_request', 'cluster_name must be at most 64 characters');
  }
  if (!w.leader) throw new HttpError(409, 'no_leader', 'could not change the settings (the cluster must have a leader): the cluster has no leader right now');
  if (b.set.single_data_node_mode) w.mode = b.set.single_data_node_mode;
  if (b.set.degrade_after) w.degradeAfter = b.set.degrade_after;
  if (b.set.cluster_name !== undefined) w.clusterName = b.set.cluster_name;
  addEvent(w, 'settings_changed', 'info', {});
  return settingsView(w);
}

export function nodeView(w, port) {
  const n = byId(w, w.self);
  return {
    id: n.id,
    name: n.name,
    role: n.role,
    platform: n.platform,
    data_dir: n.platform.os === 'darwin' ? '/Library/Application Support/CoyoteDB' : '/var/lib/coyote',
    advertise: [`${n.addr}:7481`],
    ports: { postgres: 5432, api: 7480, peer: 7481 },
    service: true,
    dashboard: { remote: w.remote, urls: [`http://127.0.0.1:${port}/`, ...(w.remote ? [`http://${n.addr}:7480/`] : [])] },
  };
}

// A real, self-signed CA certificate made for the mock (its key was thrown
// away). Like the cluster CA on a real agent, it is public.
export const MOCK_CA_PEM = `-----BEGIN CERTIFICATE-----
MIICejCCAiCgAwIBAgIJAOKi/n82Blc7MAoGCCqGSM49BAMCMDYxETAPBgNVBAoM
CENveW90ZURCMSEwHwYDVQQDDBhDb3lvdGVEQiBtb2NrIGNsdXN0ZXIgQ0EwHhcN
MjYxMTIyMDE1NTM0WhcNMzYxMTE5MDE1NTM0WjA2MREwDwYDVQQKDAhDb3lvdGVE
QjEhMB8GA1UEAwwYQ295b3RlREIgbW9jayBjbHVzdGVyIENBMIIBSzCCAQMGByqG
SM49AgEwgfcCAQEwLAYHKoZIzj0BAQIhAP////8AAAABAAAAAAAAAAAAAAAA////
////////////MFsEIP////8AAAABAAAAAAAAAAAAAAAA///////////////8BCBa
xjXYqjqT57PrvVV2mIa8ZR0GsMxTsPY7zjw+J9JgSwMVAMSdNgiG5wSTamZ44ROd
JreBn36QBEEEaxfR8uEsQkf4vOblY6RA8ncDfYEt6zOg9KE5RdiYwpZP40Li/hp/
m47n60p8D54WK84zV2sxXs7LtkBoN79R9QIhAP////8AAAAA//////////+85vqt
pxeehPO5ysL8YyVRAgEBA0IABAw4WgezSpofRQhdXrrkFj1vRkUnS8rRNZdxdZ9q
Iuz4tXnvHTWeHsGpsMoK2xW7jcEs+RxezAjXs0lK1zx9QGajIzAhMA8GA1UdEwEB
/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgEGMAoGCCqGSM49BAMCA0gAMEUCIQDNRxJ7
LnZLchwQ4I3B0AnUrOnuwERFQreOLyxGk5tNfgIga8CFi9dU6lSgOLzc9/ehejST
JUfn5CZ3bAl1a718SuQ=
-----END CERTIFICATE-----
`;

const caFile = (w) => (w.thisPlatform.os === 'darwin' ? '/Library/Application Support/CoyoteDB/ca.crt' : w.thisPlatform.os === 'windows' ? 'C:\\ProgramData\\CoyoteDB\\ca.crt' : '/var/lib/coyote/ca.crt');
const dataHosts = (w) => w.nodes.filter((n) => n.role === 'data');

/** GET /v1/connection (shape of internal/agent ConnectionView). */
export function connection(w) {
  const self = byId(w, w.self);
  return {
    database: 'app',
    user: 'app',
    sslmode: 'verify-full',
    hosts: dataHosts(w).map((n) => ({ node: n.name, host: n.addr, port: 5432, this: n.id === w.self, reachable: n.id === w.self || n.up })),
    local: self?.role === 'data' ? { host: '127.0.0.1', port: 5432 } : null,
    ca_file: caFile(w),
    ca_pem: MOCK_CA_PEM,
  };
}

/** POST /v1/app-password: a new password for the application role (ConnInfo). */
export function newAppPassword(w) {
  if (!w.leader) throw new HttpError(503, 'no_leader', 'could not change the password (the cluster must have a leader): the cluster has no leader right now');
  w.appPassword = randomBytes(18).toString('base64url');
  addEvent(w, 'settings_changed', 'info', { message: 'The application password was changed.' });
  const hosts = dataHosts(w).map((n) => `${n.addr}:5432`);
  const esc = (v) => encodeURIComponent(v);
  return {
    url: `postgresql://app:${esc(w.appPassword)}@${hosts.join(',')}/app?sslmode=verify-full&sslrootcert=${esc(caFile(w))}&target_session_attrs=read-write&connect_timeout=10`,
    hosts,
    user: 'app',
    database: 'app',
    password: w.appPassword,
    ca_file: caFile(w),
  };
}

// The mock's stand-in for the mutual-TLS check: an address answers as the
// node when the node is up and the address is not in a documentation range
// (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24) and uses port 7481.
const UNREACHABLE = /^(192\.0\.2|198\.51\.100|203\.0\.113)\./;

/** POST /v1/members/{node}/addresses (internal/agent setaddr.go). */
export function setAddresses(w, ref, b) {
  const addrs = [];
  for (const raw of Array.isArray(b.addresses) ? b.addresses : []) {
    const a = String(raw).trim();
    if (!a || addrs.includes(a)) continue;
    const m = /^(\[[0-9a-fA-F:.]+\]|[^\s:[\]]+):(\d{1,5})$/.exec(a);
    if (!m || Number(m[2]) < 1 || Number(m[2]) > 65535) throw new HttpError(400, 'bad_request', `"${a}": address ${a}: missing port in address (use HOST:PORT)`);
    addrs.push(a);
  }
  if (addrs.length === 0 || addrs.length > 8) throw new HttpError(400, 'bad_request', 'give 1 to 8 addresses as HOST:PORT');
  const n = findNode(w, ref);
  const up = n.id === w.self || n.up;
  const movedTo = answersAt(w.problems, n.name);
  const probes = addrs.map((a) => {
    const host = a.replace(/:\d+$/, '').replace(/^\[|\]$/g, '');
    const ok = movedTo.includes(a) || (up && !UNREACHABLE.test(host) && a.endsWith(':7481'));
    return ok ? { addr: a, ok: true, rtt_ms: 2.4 } : { addr: a, ok: false, error: `dial tcp ${a}: i/o timeout` };
  });
  if (!probes.some((p) => p.ok) && !b.force) {
    return { node: n.name, probes, recorded: false, message: `None of these addresses answered as ${n.name} from this machine, so nothing was changed. If ${n.name} is down, or only other machines can reach it, run the command again with --force.` };
  }
  if (!w.leader) throw new HttpError(503, 'no_leader', 'record the addresses (the cluster must have a leader): the cluster has no leader right now');
  n.addresses = addrs;
  if (movedTo.some((a) => addrs.includes(a))) {
    // The new address is recorded: the others reach it again.
    w.problems = afterSetAddress(w.problems, n.name, addrs);
    n.up = true;
    n.lastSeen = null;
  }
  return { node: n.name, probes, recorded: true, message: `Other nodes now reach ${n.name} at ${addrs.join(', ')}.` };
}

export const MOCK_PASSWORD = 'kq3V-mock-password_9zLw';

export function sampleAgents() {
  return {
    mcp: { url: 'http://127.0.0.1:7480/mcp', transport: 'streamable-http' },
    keys: [{ id: 'k_7f3a', name: 'Claude Code on laptop', access: 'read_only', created_at: iso(ago(5 * 86_400_000)), last_used_at: iso(ago(3_600_000)) }],
  };
}
