import { describe, expect, it } from 'vitest';
import { protectionFor, setupKind, summarize, TONE_LABEL } from '../../src/lib/health';
import { clone, scenarioStatus } from './fixtures';

const now = new Date();

describe('status sentence for every scenario preset', () => {
  const cases: [string, string, RegExp][] = [
    ['single', 'ok', /^Your database is running on node-a\. Its data is on this one computer, with no copy yet\.$/],
    ['two-healthy', 'ok', /^Your data is saved on 2 computers\. Every write reaches both before it is confirmed\.$/],
    ['two-copy-down', 'one-copy', /^Your data is only on node-a right now since \d+ minutes ago\.$/],
    ['two-main-down', 'down', /^node-a, the main computer, is not responding, so the database is down\.$/],
    ['two-witness', 'ok', /^Your data is saved on 2 computers, and pi-witness keeps watch\. If one stops, the other takes over by itself\.$/],
    ['three-healthy', 'ok', /^Your data is saved on 3 computers\. If one stops, another takes over by itself\.$/],
    ['failover', 'switching', /^node-a stopped responding\. Another computer takes over in about \d+ seconds?\.$/],
    ['needs-attention', 'attention', /^node-b needs attention\.$/],
    ['rejoining', 'one-copy', /^Your data is only on node-a right now since 25 minutes ago\.$/],
    ['needs-reclone', 'attention', /^node-c was away too long to catch up\. It copies all the data again; this starts by itself\.$/],
    ['no-quorum', 'down', /^Not enough computers can reach each other to agree on a main node, so the database is paused to keep your data safe\.$/],
  ];
  for (const [id, tone, sentence] of cases) {
    it(id, () => {
      const s = summarize(scenarioStatus(id), now);
      expect(s.tone).toBe(tone);
      expect(s.label).toBe(TONE_LABEL[s.tone as keyof typeof TONE_LABEL]);
      expect(s.sentence).toMatch(sentence);
    });
  }

  it('two-copy-down explains why and what could be lost', () => {
    const s = summarize(scenarioStatus('two-copy-down'), now);
    expect(s.details[0]).toBe('node-b is not responding. It is used again as soon as it has caught up.');
    expect(s.details[1]).toMatch(/^Writes still work\. If node-a stops before then, changes since \d+ minutes ago could be lost\.$/);
  });

  it('rejoining shows the copy progress', () => {
    const s = summarize(scenarioStatus('rejoining'), now);
    expect(s.details[0]).toBe('node-b is copying the data (34%). It is used again as soon as it has caught up.');
    expect(s.nodes.find((n) => n.name === 'node-b')).toMatchObject({ condition: 'copying', progress: 34, conditionText: 'Copying the data · 34%' });
  });

  it('two-main-down offers the forced switch and words the survivor calmly', () => {
    const s = summarize(scenarioStatus('two-main-down'), now);
    expect(s.actions).toEqual([{ label: 'Switch over', to: '/switch', primary: true }]);
    expect(s.nodes.find((n) => n.name === 'node-b')?.conditionText).toBe('Waiting for the main node');
    expect(s.nodes.find((n) => n.name === 'node-a')).toMatchObject({ condition: 'down', conditionText: 'Not responding', note: 'Last heard from 4 minutes ago.' });
  });

  it('no-quorum tells the person which computers to bring back', () => {
    const s = summarize(scenarioStatus('no-quorum'), now);
    expect(s.details[0]).toBe('Bring back node-b or node-c (switch it on, or check its network) and the database starts again by itself.');
  });

  it('single node is not reported as "running on one copy"', () => {
    const st = scenarioStatus('single');
    expect(st.health.one_copy).toBe(true); // what the agent says for one data node
    expect(summarize(st, now).tone).toBe('ok');
  });

  it('recommends the next setup only when healthy', () => {
    expect(summarize(scenarioStatus('single'), now).actions).toEqual([{ label: 'Add a computer', to: '/add' }]);
    expect(summarize(scenarioStatus('two-healthy'), now).actions).toEqual([{ label: 'Add a witness', to: '/add' }]);
    expect(summarize(scenarioStatus('three-healthy'), now).actions).toEqual([]);
  });
});

describe('edge cases', () => {
  it('witness down in 2 + witness: data safe, switch-over not automatic', () => {
    const st = clone(scenarioStatus('two-witness'));
    const w = st.nodes.find((n) => n.role === 'witness')!;
    w.reachable = false;
    w.last_seen = new Date(now.getTime() - 120_000).toISOString();
    const s = summarize(st, now);
    expect(s.tone).toBe('attention');
    expect(s.sentence).toBe('The witness pi-witness is not responding. Your data is safe, but until it is back, switch-over is not automatic.');
  });

  it('a copy down in a 3-node cluster', () => {
    const st = clone(scenarioStatus('three-healthy'));
    st.nodes[2]!.reachable = false;
    expect(summarize(st, now).sentence).toBe('node-c is not responding. Your data is still on 2 computers.');
  });

  it('writes blocked in strict mode', () => {
    const st = clone(scenarioStatus('two-healthy'));
    st.cluster.single_data_node_mode = 'strict';
    st.health.writes_blocked = true;
    st.nodes[1]!.reachable = false;
    const s = summarize(st, now);
    expect(s.tone).toBe('attention');
    expect(s.sentence).toBe('Writes are on hold: node-a is waiting for node-b to come back.');
  });

  it('writes blocked in degrade mode counts down', () => {
    const st = clone(scenarioStatus('two-healthy'));
    st.health.writes_blocked = true;
    st.health.degrade_in_ms = 21_000;
    st.nodes[1]!.reachable = false;
    expect(summarize(st, now).sentence).toBe('node-b stopped responding. Writes wait about 21 seconds for it, then carry on.');
  });

  it('planned switch-over in progress', () => {
    const st = clone(scenarioStatus('three-healthy'));
    st.health.switching = { kind: 'planned', from: 'node-a', to: 'node-b', started_at: now.toISOString(), step: 'catching_up' };
    const s = summarize(st, now);
    expect(s.tone).toBe('switching');
    expect(s.sentence).toBe('Moving the main node from node-a to node-b. Apps are disconnected briefly and reconnect by themselves.');
  });

  it('automatic takeover after the lease', () => {
    const st = clone(scenarioStatus('failover'));
    st.health.switching = { kind: 'automatic', from: 'node-a', to: 'node-c', started_at: now.toISOString(), step: 'promoting' };
    expect(summarize(st, now).sentence).toBe('node-c is taking over from node-a.');
  });

  it('a takeover with no countdown known does not promise a time', () => {
    const st = clone(scenarioStatus('failover'));
    delete st.health.switching!.takeover_in_ms;
    expect(summarize(st, now).sentence).toBe('node-a stopped responding. Another computer takes over shortly.');
  });

  it('writes blocked in degrade mode without a countdown points at Settings', () => {
    const st = clone(scenarioStatus('two-healthy'));
    st.health.writes_blocked = true;
    st.nodes[1]!.reachable = false;
    expect(summarize(st, now).sentence).toBe('node-b stopped responding. Writes wait for it for the time set in Settings, then carry on.');
  });

  it('needs attention: the computer, the reason without the prefix, a problem (not down) card', () => {
    const s = summarize(scenarioStatus('needs-attention'), now);
    expect(s.tone).toBe('attention');
    expect(s.details[0]).toMatch(/^PostgreSQL keeps stopping \(5 times in a row/);
    expect(s.nodes.find((n) => n.name === 'node-b')).toMatchObject({ condition: 'attention', conditionText: 'Needs attention' });
    expect(s.nodes.find((n) => n.name === 'node-b')!.note).toMatch(/^PostgreSQL keeps stopping/);
  });

  it('needs attention on the main computer says so', () => {
    const st = clone(scenarioStatus('three-healthy'));
    st.nodes[0]!.report = { ...st.nodes[0]!.report!, state: 'needs-attention', message: '' };
    const s = summarize(st, now);
    expect(s.sentence).toBe('node-a, the main computer, needs attention.');
    expect(s.details[0]).toBe('Something keeps going wrong on this computer. Its log has the details.');
  });

  it('no main node at all', () => {
    const st = clone(scenarioStatus('three-healthy'));
    for (const n of st.nodes) n.primary = false;
    st.cluster.primary_id = '';
    st.cluster.primary_name = '';
    expect(summarize(st, now)).toMatchObject({ tone: 'down', sentence: 'No computer is the main node right now, so the database cannot take reads or writes.' });
  });

  it('stale view adds a note', () => {
    const st = clone(scenarioStatus('two-healthy'));
    st.cluster.stale = true;
    expect(summarize(st, now).details.at(-1)).toMatch(/cannot reach the others/);
  });

  it('works with only the fields the agent serves today', () => {
    const st = clone(scenarioStatus('two-healthy'));
    for (const n of st.nodes) {
      delete n.platform;
      delete n.last_seen;
      delete n.eligible;
      delete n.lag_seconds;
    }
    delete st.health.switching;
    delete st.health.promote;
    delete st.cluster.name;
    st.cluster.sync_set = null;
    expect(summarize(st, now).tone).toBe('ok');
  });
});

describe('protection', () => {
  it('maps node counts to setups', () => {
    expect([setupKind(1, 0), setupKind(1, 1), setupKind(2, 0), setupKind(2, 1), setupKind(3, 0), setupKind(5, 2)]).toEqual([
      'one', 'one_witness', 'two', 'two_witness', 'three_plus', 'three_plus',
    ]);
  });
  it('words what happens if a computer stops', () => {
    expect(protectionFor('one', 'degrade_after').kind).toBe('stops');
    expect(protectionFor('two', 'degrade_after').text).toBe(
      'If the main computer stops, you press Switch over on the other one. If the copy stops, the main computer carries on alone after the wait set in Settings.',
    );
    expect(protectionFor('two', 'strict').text).toMatch(/writes wait until it is back/);
    expect(protectionFor('three_plus', 'strict')).toEqual({ kind: 'automatic', text: 'If one computer stops, another takes over by itself, usually in under 15 seconds. Apps reconnect on their own.' });
  });
});
