// From a /v1/status body to the plain-words answer to "is my data safe right
// now, and what should I do?". Pure: everything comes from the arguments,
// so every scenario is unit-tested (tests/unit/health.test.ts).
import type { MemberView, Status } from '../api/types';
import { ago, duration, lagText, plural } from './format';

export type Tone = 'ok' | 'one-copy' | 'switching' | 'attention' | 'down';

export const TONE_LABEL: Record<Tone, string> = {
  ok: 'All good',
  'one-copy': 'Running on one copy',
  switching: 'Switching over',
  attention: 'Needs attention',
  down: 'Down',
};

/** The shape of the cluster, which decides what protection it gives. */
export type SetupKind = 'one' | 'one_witness' | 'two' | 'two_witness' | 'three_plus';

export type NodeKind = 'main' | 'copy' | 'witness';
export type NodeCondition = 'ok' | 'down' | 'attention' | 'copying' | 'rejoining' | 'behind' | 'needs_reclone' | 'waiting' | 'starting';

export interface NodeSummary {
  id: string;
  name: string;
  kind: NodeKind;
  isThis: boolean;
  condition: NodeCondition;
  /** One short phrase: "Following live", "Not responding". */
  conditionText: string;
  /** Optional longer line for the card. */
  note?: string;
  lastContact: string;
  lag?: string;
  progress?: number;
  inSync: boolean;
  member: MemberView;
}

export interface Protection {
  kind: 'automatic' | 'manual' | 'stops';
  /** "If one computer stops: another takes over by itself." */
  text: string;
  /** Nudge toward the recommended setup, when not there yet. */
  recommend?: string;
}

export interface Action {
  label: string;
  to: string;
  primary?: boolean;
}

export interface HealthSummary {
  tone: Tone;
  label: string;
  sentence: string;
  details: string[];
  setup: SetupKind;
  protection: Protection;
  nodes: NodeSummary[];
  actions: Action[];
  dataCount: number;
  witnessCount: number;
}

export function setupKind(dataCount: number, witnessCount: number): SetupKind {
  if (dataCount <= 1) return witnessCount > 0 ? 'one_witness' : 'one';
  if (dataCount === 2) return witnessCount > 0 ? 'two_witness' : 'two';
  return 'three_plus';
}

function degradeWords(mode: string): string {
  return mode === 'strict'
    ? 'writes wait until it is back (you chose “wait” in Settings)'
    : 'the main computer carries on alone after the wait set in Settings';
}

export function protectionFor(kind: SetupKind, mode: string): Protection {
  switch (kind) {
    case 'one':
      return {
        kind: 'stops',
        text: 'If this computer stops, the database stops until it is back.',
        recommend: 'Add a second computer and a witness, and the database keeps running when one stops.',
      };
    case 'one_witness':
      return {
        kind: 'stops',
        text: 'If the computer with the data stops, the database stops until it is back. A witness alone holds no copy.',
        recommend: 'Add a second data computer, so there is a copy that can take over.',
      };
    case 'two':
      return {
        kind: 'manual',
        text: `If the main computer stops, you press Switch over on the other one. If the copy stops, ${degradeWords(mode)}.`,
        recommend: 'Add a witness — any small, always-on computer — and switch-over becomes automatic.',
      };
    case 'two_witness':
    case 'three_plus':
      return {
        kind: 'automatic',
        text: 'If one computer stops, another takes over by itself, usually in under 15 seconds. Apps reconnect on their own.',
      };
  }
}

const isData = (m: MemberView) => m.role === 'data';

/** The agent's reason, without the "Needs attention:" it starts with. */
function attentionNote(message: string | undefined): string {
  const t = (message ?? '').replace(/^\s*needs attention:\s*/i, '').trim();
  if (!t) return 'Something keeps going wrong on this computer. Its log has the details.';
  return t.charAt(0).toUpperCase() + t.slice(1);
}

function nodeSummary(m: MemberView, now: Date, mainDown: boolean): NodeSummary {
  const kind: NodeKind = m.role === 'witness' ? 'witness' : m.primary ? 'main' : 'copy';
  const r = m.report;
  const s: NodeSummary = {
    id: m.id,
    name: m.name,
    kind,
    isThis: m.this,
    condition: 'ok',
    conditionText: '',
    lastContact: m.this ? 'this computer' : m.reachable ? 'just now' : ago(m.last_seen ?? null, now),
    inSync: m.in_sync_set && (m.replication ?? '').endsWith('(sync)'),
    member: m,
  };
  if (kind === 'copy') s.lag = lagText(m.lag_bytes, m.lag_seconds);

  if (!m.reachable && !m.this) {
    s.condition = 'down';
    s.conditionText = 'Not responding';
    s.note = m.last_seen ? `Last heard from ${ago(m.last_seen, now)}.` : 'Not heard from since this computer started.';
    if (m.status === 'needs_reclone') s.note += ' When it is back it copies all the data again.';
    return s;
  }
  if (m.status === 'needs_reclone' && r?.state !== 'cloning') {
    s.condition = 'needs_reclone';
    s.conditionText = 'Needs a fresh copy';
    s.note = 'It was away too long to catch up, so it copies all the data again. This starts by itself.';
    return s;
  }
  switch (r?.state) {
    case 'needs-attention':
      s.condition = 'attention';
      s.conditionText = 'Needs attention';
      s.note = attentionNote(r.message);
      return s;
    case 'cloning':
      s.condition = 'copying';
      s.progress = r.copy_progress;
      s.conditionText = r.copy_progress != null ? `Copying the data · ${r.copy_progress}%` : 'Copying the data';
      s.note = 'It can take over once the copy is done and it is following live.';
      return s;
    case 'rejoining':
      s.condition = 'rejoining';
      s.conditionText = 'Catching up';
      s.note = 'It was the main node before; it is lining its data up with the new main node.';
      return s;
    case 'waiting':
      s.condition = 'waiting';
      s.conditionText = 'Waiting';
      s.note = r.message || undefined;
      return s;
    case 'starting':
    case '':
      s.condition = 'starting';
      s.conditionText = 'Starting';
      return s;
  }
  if (kind === 'witness') {
    s.conditionText = 'Watching · holds no data';
    return s;
  }
  if (kind === 'main') {
    s.conditionText = 'Taking reads and writes';
    return s;
  }
  const rep = m.replication ?? '';
  if (mainDown) {
    s.conditionText = 'Waiting for the main node';
    s.note = 'It has the data up to when the main node stopped answering.';
    return s;
  }
  if (rep.startsWith('streaming')) {
    s.conditionText = 'Following live';
    s.note = s.inSync ? 'Every write is saved here too before it is confirmed.' : 'Following, but writes do not wait for it.';
    return s;
  }
  s.condition = 'behind';
  s.conditionText = 'Not following';
  s.note = 'It is running but not receiving changes from the main node.';
  return s;
}

function listNames(names: string[]): string {
  if (names.length <= 1) return names[0] ?? '';
  return names.slice(0, -1).join(', ') + ' and ' + names[names.length - 1];
}

function orNames(names: string[]): string {
  if (names.length <= 1) return names[0] ?? '';
  return names.slice(0, -1).join(', ') + ' or ' + names[names.length - 1];
}

export function summarize(st: Status, now: Date = new Date()): HealthSummary {
  const mainMember = st.nodes.find((m) => m.primary);
  const mainDown = !!mainMember && !mainMember.reachable && !mainMember.this;
  const nodes = st.nodes.map((m) => nodeSummary(m, now, mainDown));
  const data = st.nodes.filter(isData);
  const witnesses = st.nodes.filter((m) => m.role === 'witness');
  const setup = setupKind(data.length, witnesses.length);
  const protection = protectionFor(setup, st.cluster.single_data_node_mode);
  const h = st.health;
  const main = st.nodes.find((m) => m.primary);
  const mainName = main?.name ?? (st.cluster.primary_name || '');
  const details: string[] = [];
  const actions: Action[] = [];
  const voters = st.nodes.filter((m) => m.suffrage === 'voter');
  const downNodes = nodes.filter((n) => n.condition === 'down');
  const copies = nodes.filter((n) => n.kind === 'copy');

  const result = (tone: Tone, sentence: string): HealthSummary => {
    if (st.cluster.stale) details.push('This computer cannot reach the others right now, so what you see is the last thing it heard and may be out of date.');
    if (protection.recommend && tone === 'ok') actions.push({ label: setup === 'two' ? 'Add a witness' : 'Add a computer', to: '/add' });
    return {
      tone,
      label: TONE_LABEL[tone],
      sentence,
      details,
      setup,
      protection,
      nodes,
      actions,
      dataCount: data.length,
      witnessCount: witnesses.length,
    };
  };

  // 1. A switch-over or failover is running.
  const sw = h.switching;
  if (sw) {
    if (sw.kind === 'planned') {
      return result('switching', `Moving the main node from ${sw.from} to ${sw.to}. Apps are disconnected briefly and reconnect by themselves.`);
    }
    if (sw.kind === 'forced') {
      return result('switching', `Making ${sw.to} the main node, as you asked.`);
    }
    if (sw.step === 'waiting_lease') {
      const wait = sw.takeover_in_ms != null ? ` in about ${duration(sw.takeover_in_ms)}` : ' shortly';
      details.push(`CoyoteDB waits a moment first, to be sure ${sw.from} has really stopped and is not just slow. This is what keeps two computers from both taking writes.`);
      return result('switching', `${sw.from} stopped responding. Another computer takes over${wait}.`);
    }
    details.push('Apps reconnect by themselves. Writes made before the switch are safe.');
    return result('switching', `${sw.to || 'A copy'} is taking over from ${sw.from}.`);
  }

  // 2. No quorum: nothing can be decided, and the main node pauses itself.
  if (!st.cluster.leader_id && voters.length > 1) {
    const away = downNodes.map((n) => n.name);
    if (away.length) details.push(`Bring back ${orNames(away)} (switch it on, or check its network) and the database starts again by itself.`);
    details.push('Your data is safe: nothing is lost while it is paused.');
    return result('down', 'Not enough computers can reach each other to agree on a main node, so the database is paused to keep your data safe.');
  }

  // 3. No main node at all.
  if (!main && !mainName) {
    details.push('This usually clears by itself. If it does not, check that the computers can reach each other.');
    return result('down', 'No computer is the main node right now, so the database cannot take reads or writes.');
  }

  // 4. The main node is gone.
  if (main && !main.reachable && !main.this) {
    const code = h.failover_code ?? '';
    const survivors = data.filter((m) => !m.primary && m.reachable).map((m) => m.name);
    if (code === 'waiting_lease') {
      return result('switching', `${mainName} stopped responding. Another computer takes over shortly.`);
    }
    if (setup === 'two' || code === 'two_nodes') {
      const other = survivors[0] ?? 'the other computer';
      details.push(`With two computers and no witness, ${other} cannot tell whether ${mainName} is really off or just unreachable, so it does not take over by itself.`);
      if (h.promote?.allowed) {
        details.push(`If ${mainName} is really off or broken, you can make ${other} the main node. Read the warning first: recent writes may be lost.`);
        actions.push({ label: 'Switch over', to: '/switch', primary: true });
      } else if (survivors.length) {
        details.push(`To make ${other} the main node, open this dashboard on ${other}.`);
      }
      return result('down', `${mainName}, the main computer, is not responding, so the database is down.`);
    }
    if (code === 'no_eligible_copy') {
      details.push(`No copy has all of the latest writes, so CoyoteDB will not promote one: that could lose data. Bring ${mainName} back to continue.`);
      return result('down', `${mainName}, the main computer, is not responding, and no copy is safe to take over.`);
    }
    if (code === 'no_quorum') {
      details.push(`Bring back ${mainName} or another computer so a majority can agree.`);
      return result('down', `${mainName}, the main computer, is not responding, and not enough computers are left to agree on a new one.`);
    }
    if (h.failover) details.push(h.failover);
    return result('down', `${mainName}, the main computer, is not responding.`);
  }

  // 5. Writes are on hold, waiting for a copy.
  if (h.writes_blocked) {
    const missing = copies.filter((n) => n.member.in_sync_set && n.condition !== 'ok').map((n) => n.name);
    const who = missing.length ? orNames(missing) : 'a copy';
    if (st.cluster.single_data_node_mode === 'strict') {
      details.push('You set this database to wait rather than run on one copy. Bring the copy back, or change this in Settings.');
      return result('attention', `Writes are on hold: ${mainName} is waiting for ${who} to come back.`);
    }
    const left = h.degrade_in_ms != null ? `about ${duration(h.degrade_in_ms)} for it` : 'for it for the time set in Settings';
    details.push('After that it keeps working with one copy, and tells you here.');
    return result('attention', `${who} stopped responding. Writes wait ${left}, then carry on.`);
  }

  // 6. Running on one copy.
  if (data.length > 1 && (h.one_copy || !st.cluster.standby_eligible)) {
    const since = h.one_copy_since ?? st.cluster.standby_eligible_since;
    const why: string[] = [];
    for (const c of copies) {
      if (c.condition === 'down') why.push(`${c.name} is not responding`);
      else if (c.condition === 'copying') why.push(`${c.name} is copying the data${c.progress != null ? ` (${c.progress}%)` : ''}`);
      else if (c.condition === 'rejoining') why.push(`${c.name} is catching up`);
      else if (c.condition === 'needs_reclone') why.push(`${c.name} needs a fresh copy`);
      else if (c.condition === 'behind') why.push(`${c.name} is not following`);
      else if (c.condition === 'ok') why.push(`${c.name} is catching up`);
    }
    if (why.length) details.push(listNames(why) + '. It is used again as soon as it has caught up.');
    details.push(`Writes still work. If ${mainName} stops before then, changes since ${since ? ago(since, now) : 'then'} could be lost.`);
    const s = since ? ` since ${ago(since, now)}` : '';
    return result('one-copy', `Your data is only on ${mainName} right now${s}.`);
  }

  // 7. Something else needs a look.
  const attention: string[] = [];
  const troubled = nodes.filter((n) => n.condition === 'attention');
  for (const n of troubled) {
    attention.push(n.kind === 'main' ? `${n.name}, the main computer, needs attention.` : `${n.name} needs attention.`);
    if (n.note) attention.push(n.note);
  }
  for (const n of nodes) {
    if (n.condition === 'down' && n.kind === 'witness') {
      attention.push(
        protection.kind === 'automatic' && data.length === 2
          ? `The witness ${n.name} is not responding. Your data is safe, but until it is back, switch-over is not automatic.`
          : `The witness ${n.name} is not responding.`,
      );
    } else if (n.condition === 'down') {
      const left = data.filter((m) => m.reachable).length;
      attention.push(`${n.name} is not responding. Your data is still on ${plural(left, 'computer', 'computers')}.`);
    } else if (n.condition === 'needs_reclone') {
      attention.push(`${n.name} was away too long to catch up. It copies all the data again; this starts by itself.`);
    } else if (n.condition === 'behind') {
      attention.push(`${n.name} is running but not following the main node.`);
    } else if (n.condition === 'waiting' && n.note) {
      attention.push(`${n.name}: ${n.note}`);
    }
  }
  if (attention.length) {
    details.push(...attention.slice(1));
    return result('attention', attention[0]!);
  }

  // 8. All good.
  for (const n of nodes) {
    if (n.condition === 'copying') details.push(`${n.name} is copying the data${n.progress != null ? ` (${n.progress}%)` : ''}. It can take over once that is done.`);
    if (n.condition === 'rejoining') details.push(`${n.name} is catching up.`);
  }
  const live = data.filter((m) => m.primary || (m.replication ?? '').startsWith('streaming'));
  const w = witnesses[0];
  switch (setup) {
    case 'one':
    case 'one_witness':
      return result('ok', `Your database is running on ${mainName}. Its data is on this one computer, with no copy yet.`);
    case 'two':
      return result('ok', h.writes_wait
        ? `Your data is saved on 2 computers. Every write reaches both before it is confirmed.`
        : `Your database is running on ${mainName}, with a copy on ${copies[0]?.name ?? 'the second computer'}.`);
    case 'two_witness':
      return result('ok', `Your data is saved on ${plural(live.length, 'computer', 'computers')}, and ${w?.name ?? 'the witness'} keeps watch. If one stops, the other takes over by itself.`);
    case 'three_plus':
      return result('ok', `Your data is saved on ${plural(live.length, 'computer', 'computers')}. If one stops, another takes over by itself.`);
  }
}

