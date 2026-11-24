import { describe, expect, it } from 'vitest';
import type { Problem } from '../../src/api/types';
import { addressProblemsFor, hostLabel, otherWarnings, paragraphs, problemsOf, suggestLabel } from '../../src/lib/problems';

const gone: Problem = {
  code: 'address_gone',
  severity: 'problem',
  node: 'node-b',
  summary: "node-b's address changed: it was 192.168.1.20, it is now 192.168.1.35.",
  details: 'One.\n\nTwo.',
  markdown: '## x',
  address: { recorded: ['192.168.1.20:7481'], now: ['192.168.1.35:7481'], suggested: ['192.168.1.35:7481'] },
};
const twoNodes: Problem = { code: 'two_nodes', severity: 'warning', summary: 'No witness.', details: '', markdown: '' };

describe('problems', () => {
  it('labels the Use button with the host, port only when not the default', () => {
    expect(hostLabel('192.168.1.35:7481')).toBe('192.168.1.35');
    expect(hostLabel('192.168.1.35:9000')).toBe('192.168.1.35:9000');
    expect(hostLabel('[fd7a::5]:7481')).toBe('fd7a::5');
    expect(hostLabel('[fd7a::5]:9000')).toBe('[fd7a::5]:9000');
    expect(hostLabel('nas.local:7481')).toBe('nas.local');
    expect(suggestLabel(gone)).toBe('Use 192.168.1.35');
    expect(suggestLabel({ ...gone, address: { ...gone.address!, suggested: ['192.168.1.35:7481', '100.64.0.9:7481'] } })).toBe('Use 192.168.1.35, 100.64.0.9');
  });

  it('offers no Use button without a node or a suggestion', () => {
    expect(suggestLabel({ ...gone, node: '' })).toBeNull();
    expect(suggestLabel({ ...gone, address: { recorded: ['192.168.1.20:7481'] } })).toBeNull();
    expect(suggestLabel(twoNodes)).toBeNull();
  });

  it('does not repeat a problem as a warning', () => {
    const s = { problems: [gone], warnings: [gone.summary, 'node-b is not reachable.'] };
    expect(otherWarnings(s)).toEqual(['node-b is not reachable.']);
    expect(otherWarnings({ warnings: ['a'] })).toEqual(['a']);
  });

  it('lists problems before warnings and treats a missing list as empty', () => {
    expect(problemsOf({ problems: [twoNodes, gone] }).map((p) => p.code)).toEqual(['address_gone', 'two_nodes']);
    expect(problemsOf({})).toEqual([]);
    expect(problemsOf(null)).toEqual([]);
  });

  it('finds the address problems of one computer', () => {
    expect(addressProblemsFor({ problems: [gone, twoNodes] }, 'node-b')).toEqual([gone]);
    expect(addressProblemsFor({ problems: [gone] }, 'node-a')).toEqual([]);
  });

  it('splits the long form into paragraphs', () => {
    expect(paragraphs('One.\n\nTwo.\n \nThree.')).toEqual(['One.', 'Two.', 'Three.']);
    expect(paragraphs('')).toEqual([]);
  });
});
