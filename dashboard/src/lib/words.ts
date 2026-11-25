// Product wording that is not health: activity lines, setup steps, names.
import type { ActivityEvent, Role, Step, StepId } from '../api/types';
import { bytes } from './format';

export function eventText(e: ActivityEvent): string {
  const n = e.node ?? 'A computer';
  switch (e.kind) {
    case 'cluster_created':
      return `${n} started a new database`;
    case 'member_joined':
      return e.role === 'witness' ? `${n} joined as a witness` : `${n} joined and is copying the data`;
    case 'member_left':
      return `${n} left the cluster`;
    case 'member_removed':
      return `${n} was removed from the cluster`;
    case 'member_renamed':
      return `${e.from ?? 'A computer'} is now called ${n}`;
    case 'node_unreachable':
      return `${n} stopped responding`;
    case 'node_back':
      return `${n} is responding again`;
    case 'switch_started':
      return `Switch-over started: ${e.from ?? '?'} → ${n}`;
    case 'switch_failed':
      return `Switch-over to ${n} did not happen; nothing changed`;
    case 'main_changed': {
      const why =
        e.reason === 'planned' ? ' (planned switch-over)' : e.reason === 'automatic' ? (e.from ? ` (${e.from} stopped responding)` : ' (automatic)') : e.reason === 'forced' ? ' (forced by you)' : '';
      return `${n} became the main node${why}`;
    }
    case 'forced_promote':
      return `${n} was made the main node by hand`;
    case 'one_copy_started':
      return `Running on one copy: data is only on ${n}`;
    case 'one_copy_ended':
      return `Back to two copies: ${n} caught up`;
    case 'writes_blocked':
      return `Writes are waiting for a copy`;
    case 'writes_resumed':
      return `Writes are flowing again`;
    case 'copy_started':
      return `${n} started copying the data`;
    case 'copy_finished':
      return `${n} finished copying and is following live`;
    case 'needs_reclone':
      return `${n} was away too long and will copy the data again`;
    case 'invite_created':
      return e.role === 'witness' ? 'An invite code for a witness was created' : 'An invite code for a computer was created';
    case 'invite_cancelled':
      return 'An invite code was cancelled';
    case 'settings_changed':
      return 'Settings were changed';
    case 'agent_started':
      return `CoyoteDB started on ${n}`;
  }
  return e.message;
}

const STEP_LABEL: Record<StepId, string> = {
  contact: 'Contacting the cluster',
  download: 'Downloading PostgreSQL',
  verify: 'Checking the download',
  prepare: 'Preparing the database',
  start: 'Starting',
  copy: 'Copying the data',
  follow: 'Following live',
};

/** inImage: PostgreSQL is part of the container image, nothing is downloaded (D68). */
export function stepLabel(s: Step, role: Role, inImage = false): string {
  if (inImage && s.id === 'download') return 'PostgreSQL 18';
  if (inImage && s.id === 'verify') return 'Checking the program files';
  if (s.id === 'start' && role === 'witness') return 'Joining as a witness';
  if (s.id === 'start') return 'Starting the database';
  return STEP_LABEL[s.id] ?? s.id;
}

export function stepDetail(s: Step, inImage = false): string {
  if (s.state === 'skipped') return s.id === 'download' ? (inImage ? 'Part of the image, nothing to download' : 'Already on this computer') : 'Not needed';
  if (s.detail) return s.detail;
  if (s.state === 'active' && s.bytes_total) {
    const pct = s.percent ?? Math.floor(((s.bytes_done ?? 0) / s.bytes_total) * 100);
    return `${pct}% · ${bytes(s.bytes_done ?? 0)} of ${bytes(s.bytes_total)}`;
  }
  if (s.state === 'active' && s.percent != null) return `${s.percent}%`;
  if (s.state === 'done' && s.bytes_total && s.id === 'download') return bytes(s.bytes_total);
  return '';
}

/** Same rule as pg.ValidName: 1–48 of a-z, 0-9 and inner '-'. */
export function nameProblem(s: string): string | null {
  if (s === '') return 'Give this computer a name.';
  if (s.length > 48) return 'Use at most 48 characters.';
  if (/[A-Z]/.test(s)) return 'Use lowercase letters.';
  if (!/^[a-z0-9-]+$/.test(s)) return 'Use only letters a–z, digits and dashes.';
  if (s.startsWith('-') || s.endsWith('-')) return 'Start and end with a letter or digit.';
  return null;
}

/** Turn anything typed into a valid name, as pg.SanitizeName does. */
export function sanitizeName(s: string): string {
  const out = s
    .toLowerCase()
    .split('.')[0]!
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 48)
    .replace(/-+$/, '');
  return out || 'node';
}

export const ROLE_WORD: Record<'main' | 'copy' | 'witness', string> = { main: 'Main node', copy: 'Copy', witness: 'Witness' };

/** Plain explanation of a word, shown on first use. */
export const GLOSSARY = {
  main: 'The main node is the computer that takes the writes right now. Any computer can become it.',
  copy: 'A copy is a computer that follows the main node live, so it can take over.',
  witness: 'A witness holds no data. It only helps the others agree on who is main, so switch-over can be automatic.',
  invite: 'An invite code lets one more computer join. It works once and expires.',
  switch: 'A switch-over moves the main role to another computer.',
} as const;
