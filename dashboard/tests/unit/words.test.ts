import { describe, expect, it } from 'vitest';
import type { ActivityEvent } from '../../src/api/types';
import { eventText, nameProblem, sanitizeName, stepDetail, stepLabel } from '../../src/lib/words';

const ev = (p: Partial<ActivityEvent>): ActivityEvent => ({ id: '1', at: '2026-11-20T14:00:00Z', kind: 'x', severity: 'info', message: 'server words', ...p });

describe('activity wording', () => {
  it('reads like a sentence', () => {
    expect(eventText(ev({ kind: 'node_unreachable', node: 'node-b' }))).toBe('node-b stopped responding');
    expect(eventText(ev({ kind: 'main_changed', node: 'node-c', from: 'node-a', reason: 'automatic' }))).toBe('node-c became the main node (node-a stopped responding)');
    expect(eventText(ev({ kind: 'main_changed', node: 'node-b', reason: 'planned' }))).toBe('node-b became the main node (planned switch-over)');
    expect(eventText(ev({ kind: 'member_joined', node: 'pi', role: 'witness' }))).toBe('pi joined as a witness');
    expect(eventText(ev({ kind: 'something_new' }))).toBe('server words');
  });
});

describe('names', () => {
  it('validates like pg.ValidName', () => {
    expect(nameProblem('node-b')).toBeNull();
    expect(nameProblem('')).toBe('Give this computer a name.');
    expect(nameProblem('Node')).toBe('Use lowercase letters.');
    expect(nameProblem('-a')).toBe('Start and end with a letter or digit.');
    expect(nameProblem('a_b')).toBe('Use only letters a–z, digits and dashes.');
    expect(nameProblem('a'.repeat(49))).toBe('Use at most 48 characters.');
  });
  it('sanitizes like pg.SanitizeName', () => {
    expect(sanitizeName("Alex's-MacBook.local")).toBe('alex-s-macbook');
    expect(sanitizeName('Office Mini!')).toBe('office-mini');
    expect(sanitizeName('!!!')).toBe('node');
  });
});

describe('setup steps', () => {
  it('labels and details', () => {
    expect(stepLabel({ id: 'start', state: 'active' }, 'witness')).toBe('Joining as a witness');
    expect(stepDetail({ id: 'copy', state: 'active', bytes_done: 1288490188, bytes_total: 3067833856, percent: 42 })).toBe('42% · 1.2 GB of 2.9 GB');
    expect(stepDetail({ id: 'download', state: 'skipped' })).toBe('Already on this computer');
  });
});
