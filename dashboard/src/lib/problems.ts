// Problems with a stable code (API.md §5.1): the server writes every word
// (summary, details, evidence, commands, the Markdown for an AI agent);
// these helpers only pick and label. Pure, unit-tested in
// tests/unit/problems.test.ts.
import type { Problem, Status } from '../api/types';
import { createStore } from './store';

/** The agent port; dropped from button labels when it is this one. */
export const DEFAULT_AGENT_PORT = '7481';

export function problemsOf(s: Pick<Status, 'problems'> | null | undefined): Problem[] {
  const list = s?.problems ?? [];
  // Problems before warnings, then in the server's order.
  return [...list].sort((a, b) => sevRank(a) - sevRank(b));
}

const sevRank = (p: Problem) => (p.severity === 'problem' ? 0 : 1);

/** Warnings that are not already shown as a problem banner. */
export function otherWarnings(s: Pick<Status, 'problems' | 'warnings'>): string[] {
  const shown = new Set((s.problems ?? []).map((p) => p.summary));
  return (s.warnings ?? []).filter((w) => !shown.has(w));
}

export const isAddressProblem = (p: Problem): boolean => p.code.startsWith('address_');

/** Address problems about one computer (for its card in Settings). */
export function addressProblemsFor(s: Pick<Status, 'problems'> | null | undefined, node: string): Problem[] {
  return problemsOf(s).filter((p) => p.node === node && isAddressProblem(p));
}

/** "192.168.1.35" for 192.168.1.35:7481; the full HOST:PORT for any other port. */
export function hostLabel(hostPort: string): string {
  const m = /^(\[([^\]]+)\]|[^:]+):(\d+)$/.exec(hostPort.trim());
  if (!m) return hostPort.trim();
  const host = m[2] ?? m[1] ?? hostPort;
  return m[3] === DEFAULT_AGENT_PORT ? host : hostPort.trim();
}

/** The "Use …" button's label, or null when there is nothing to use. */
export function suggestLabel(p: Problem): string | null {
  const s = p.address?.suggested ?? [];
  if (!p.node || s.length === 0) return null;
  return `Use ${s.map(hostLabel).join(', ')}`;
}

/** Paragraphs of a long explanation (blank-line separated). */
export function paragraphs(text: string): string[] {
  return text
    .split(/\n\s*\n/)
    .map((t) => t.trim())
    .filter(Boolean);
}

/**
 * A request to open the Change addresses dialog for a computer, prefilled:
 * set by a banner's "Use …" button (on any page), consumed by Settings.
 */
export interface AddressPrefill {
  node: string;
  addresses: string[];
}
export const addressPrefill = createStore<{ req: AddressPrefill | null }>({ req: null });
