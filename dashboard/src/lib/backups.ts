// Backup words and small helpers, shared by the setup step, the Backups
// page and the overview card. The command line says the same things
// (cmd/coyote/backup_words.go): keep the two in step.
import type { BackupDestinationRequest, BackupS3, BackupSchedule, BackupStatus } from '../api/types';
import { parseGoDuration } from './format';

export type BackupTone = 'ok' | 'pending' | 'attention' | 'off';

export interface BackupHeadline {
  tone: BackupTone;
  /** "Backups are on and working." */
  title: string;
  /** Short form for the overview card: "Healthy", "Needs attention". */
  short: string;
}

/** The one-line summary (same as `coyote backup`). */
export function backupHeadline(s: BackupStatus | undefined | null, now: Date = new Date()): BackupHeadline {
  if (!s || !s.enabled) return { tone: 'off', title: 'Backups are off.', short: 'Off' };
  if (s.problem) return { tone: 'attention', title: 'Backups need attention.', short: 'Needs attention' };
  if (!s.last_backup && s.running) return { tone: 'pending', title: 'Backups are on. The first full backup is running now.', short: 'First backup running' };
  if (!s.last_backup) {
    const when = s.next_backup_at ? ` ${whenText(s.next_backup_at, now)}` : ' soon';
    return { tone: 'pending', title: `Backups are on. The first full backup is taken${when}.`, short: 'First backup soon' };
  }
  return { tone: 'ok', title: 'Backups are on and working.', short: 'Healthy' };
}

/** What "off" costs, in one line (setup step, page, overview). */
export const OFF_CONSEQUENCE = 'Nothing is saved, so a deleted table or a bad change cannot be undone.';

export const SAME_DISK =
  'This folder is on the same disk as the database. It protects you from mistakes, like a deleted table, but not from this disk failing. Use another disk, a USB drive or a network drive if you can.';

export const ENCRYPTION_TEXT =
  'Cloud backups are always encrypted on this computer before they are sent, with a key only your cluster has. The storage company sees only scrambled data.';

export const KEY_WARNING =
  'Save this key somewhere that is not one of these computers: a password manager, or on paper. If every computer is lost and nobody has the key, nobody can read the backups, not even us.';

/** "at 14:05 today", "tomorrow at 03:00", "on Mon 17 Nov at 03:00". */
export function whenText(t: string | Date, now: Date = new Date()): string {
  const d = typeof t === 'string' ? new Date(t) : t;
  const hm = d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const dd = Math.round((day(d) - day(now)) / 86_400_000);
  if (dd === 0) return `at ${hm} today`;
  if (dd === 1) return `tomorrow at ${hm}`;
  if (dd === -1) return `yesterday at ${hm}`;
  return `on ${d.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' })} at ${hm}`;
}

/** "today 14:05", "yesterday 03:00", "Mon 17 Nov 03:00": a cell or an end of a range. */
export function stamp(t: string | Date, now: Date = new Date()): string {
  const d = typeof t === 'string' ? new Date(t) : t;
  const hm = d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const dd = Math.round((day(d) - day(now)) / 86_400_000);
  if (dd === 0) return `today ${hm}`;
  if (dd === -1) return `yesterday ${hm}`;
  if (dd === 1) return `tomorrow ${hm}`;
  return `${d.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' })} ${hm}`;
}

/** "1 minute", "day", "7 days", "12 hours" from a Go duration ("" when unknown). */
export function durWords(goDur: string, unitOnly = false): string {
  const ms = parseGoDuration(goDur);
  if (!Number.isFinite(ms)) return goDur;
  const day = 86_400_000;
  if (ms >= day && ms % day === 0) {
    const n = ms / day;
    if (n === 1) return unitOnly ? 'day' : '1 day';
    if (n === 7 && unitOnly) return 'week';
    return `${n} days`;
  }
  if (ms >= 3_600_000 && ms % 3_600_000 === 0) {
    const n = ms / 3_600_000;
    return n === 1 ? (unitOnly ? 'hour' : '1 hour') : `${n} hours`;
  }
  if (ms >= 60_000 && ms % 60_000 === 0) {
    const n = ms / 60_000;
    return n === 1 ? '1 minute' : `${n} minutes`;
  }
  const s = Math.round(ms / 1000);
  return s === 1 ? '1 second' : `${s} seconds`;
}

/** Days in a Go duration (0 when under a day). */
export function days(goDur: string): number {
  const ms = parseGoDuration(goDur);
  return Number.isFinite(ms) ? Math.floor(ms / 86_400_000) : 0;
}

/** "Every change is saved within 1 minute. A full copy is taken every day at 03:00 and kept 7 days." */
export function scheduleSentence(s: BackupSchedule): string {
  const every = durWords(s.every, true);
  const daily = Number.isFinite(parseGoDuration(s.every)) && parseGoDuration(s.every) % 86_400_000 === 0;
  return `Every change is saved within ${durWords(s.archive_timeout)}. A full copy is taken every ${every}${daily && s.at ? ` at ${s.at}` : ''} and kept ${durWords(s.keep)}.`;
}

/** Choices offered for "a full copy every …". */
export const EVERY_CHOICES: { value: string; label: string }[] = [
  { value: '6h', label: 'Every 6 hours' },
  { value: '12h', label: 'Every 12 hours' },
  { value: '24h', label: 'Every day' },
  { value: '168h', label: 'Every week' },
];

/** Choices offered for "keep …". */
export const KEEP_CHOICES: { value: string; label: string }[] = [
  { value: '3d', label: '3 days' },
  { value: '7d', label: '7 days' },
  { value: '14d', label: '14 days' },
  { value: '30d', label: '30 days' },
  { value: '90d', label: '90 days' },
];

/** The choice value that matches a Go duration ("24h0m0s" → "24h"), or "" for a custom one. */
export function matchChoice(goDur: string, choices: { value: string }[]): string {
  const ms = parseGoDuration(goDur);
  for (const c of choices) {
    const v = c.value.endsWith('d') ? Number(c.value.slice(0, -1)) * 86_400_000 : parseGoDuration(c.value);
    if (v === ms) return c.value;
  }
  return '';
}

// ---- where backups go: the editor's model ----

export type DestChoice = 'default' | 'folder' | 's3' | 'off';
export type Provider = 'b2' | 's3' | 'r2' | 'other';

export interface DestDraft {
  choice: DestChoice;
  folder: string;
  encrypt: boolean;
  provider: Provider;
  endpoint: string;
  region: string;
  bucket: string;
  prefix: string;
  accessKey: string;
  secretKey: string;
}

export const EMPTY_DRAFT: DestDraft = {
  choice: 'default',
  folder: '',
  encrypt: false,
  provider: 'b2',
  endpoint: '',
  region: '',
  bucket: '',
  prefix: 'coyote',
  accessKey: '',
  secretKey: '',
};

export const PROVIDERS: Record<Provider, { name: string; endpoint: string; endpointHint: string; region: string; regionHint: string; keyHint: string }> = {
  b2: {
    name: 'Backblaze B2',
    endpoint: 'https://s3.us-west-004.backblazeb2.com',
    endpointHint: 'Shown on the bucket’s page as “Endpoint”. Add https:// in front.',
    region: '',
    regionHint: 'Leave empty: it is read from the address.',
    keyHint: 'Make an application key for this bucket only (App Keys → Add a New Application Key, read and write).',
  },
  s3: {
    name: 'Amazon S3',
    endpoint: 'https://s3.us-east-1.amazonaws.com',
    endpointHint: 'https://s3.REGION.amazonaws.com, with your bucket’s region.',
    region: 'us-east-1',
    regionHint: 'The bucket’s region, like eu-central-1.',
    keyHint: 'Make an IAM user whose policy allows only this bucket: list, read, write and delete.',
  },
  r2: {
    name: 'Cloudflare R2',
    endpoint: 'https://ACCOUNT-ID.r2.cloudflarestorage.com',
    endpointHint: 'Put your account ID in place of ACCOUNT-ID (R2 → Overview).',
    region: 'auto',
    regionHint: 'R2 uses “auto”.',
    keyHint: 'Make an R2 API token with “Object Read & Write” for this bucket.',
  },
  other: {
    name: 'Other S3-compatible',
    endpoint: 'https://',
    endpointHint: 'The storage’s S3 address, for example a MinIO server or Wasabi.',
    region: '',
    regionHint: 'Only if your storage asks for one.',
    keyHint: 'An access key that may list, read, write and delete in this bucket.',
  },
};

/** Guess the provider from a saved endpoint. */
export function providerOf(endpoint: string): Provider {
  if (/backblazeb2\.com/i.test(endpoint)) return 'b2';
  if (/amazonaws\.com/i.test(endpoint)) return 's3';
  if (/r2\.cloudflarestorage\.com/i.test(endpoint)) return 'r2';
  return 'other';
}

/** The request for a draft (null for "off"). `default` is a folder next to the data folder. */
export function draftRequest(d: DestDraft): BackupDestinationRequest | null {
  switch (d.choice) {
    case 'off':
      return null;
    case 'default':
      return { kind: 'folder', folder: '', encrypt: false };
    case 'folder':
      return { kind: 'folder', folder: d.folder.trim(), encrypt: d.encrypt };
    case 's3': {
      const s3: BackupS3 = {
        endpoint: d.endpoint.trim().replace(/\/+$/, ''),
        bucket: d.bucket.trim(),
        prefix: d.prefix.trim().replace(/^\/+|\/+$/g, ''),
        region: d.region.trim(),
        access_key: d.accessKey.trim(),
        secret_key: d.secretKey.trim(),
      };
      return { kind: 's3', s3 };
    }
  }
}

/** A key that changes whenever the request would (a check result belongs to one key). */
export function draftKey(d: DestDraft): string {
  return JSON.stringify([d.choice, draftRequest(d)]);
}

/** What the person typed in the S3 fields that looks wrong, before asking the agent. */
export function s3FieldProblem(d: DestDraft): string {
  const e = d.endpoint.trim();
  if (!e || e === 'https://') return 'Fill in the storage address.';
  if (!/^https?:\/\/[^/\s]+/i.test(e)) return 'The storage address starts with https://.';
  if (/ACCOUNT-ID/.test(e)) return 'Put your Cloudflare account ID in the address.';
  if (!d.bucket.trim()) return 'Fill in the bucket name.';
  if (!d.accessKey.trim()) return 'Fill in the access key ID.';
  if (!d.secretKey.trim()) return 'Fill in the secret key.';
  return '';
}

/** "/Users/sam/coyote/node-a-backups" → a short label for the card. */
export function whereShort(kind: string, label: string): string {
  if (kind === 's3') return label.replace(/\s*\(https?:\/\/[^)]*\)$/, '');
  return label;
}
