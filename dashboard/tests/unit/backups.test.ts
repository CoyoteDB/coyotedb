import { describe, expect, it } from 'vitest';
import type { BackupStatus } from '../../src/api/types';
import {
  backupHeadline,
  days,
  draftKey,
  draftRequest,
  durWords,
  EMPTY_DRAFT,
  EVERY_CHOICES,
  KEEP_CHOICES,
  matchChoice,
  providerOf,
  s3FieldProblem,
  scheduleSentence,
  whenText,
  whereShort,
} from '../../src/lib/backups';

const dest = { kind: 'folder' as const, label: '/data-backups', per_node: true };
const st = (p: Partial<BackupStatus>): BackupStatus => ({ enabled: true, destination: dest, encrypted: false, archive: { waiting_bytes: 0, lag_ms: 0 }, ...p }) as BackupStatus;
const done = { id: 'x', kind: 'scheduled', state: 'done', node: 'node-a', started_at: '2026-11-24T02:56:00Z', finished_at: '2026-11-24T03:00:00Z', size_bytes: 1 };

describe('backup headline (same words as `coyote backup`)', () => {
  it('covers every state', () => {
    expect(backupHeadline(undefined)).toMatchObject({ tone: 'off', title: 'Backups are off.' });
    expect(backupHeadline(st({ enabled: false }))).toMatchObject({ tone: 'off', short: 'Off' });
    expect(backupHeadline(st({ problem: 'disk gone', last_backup: done as never }))).toMatchObject({ tone: 'attention', title: 'Backups need attention.' });
    expect(backupHeadline(st({ running: done as never }))).toMatchObject({ tone: 'pending', title: 'Backups are on. The first full backup is running now.' });
    expect(backupHeadline(st({}))).toMatchObject({ tone: 'pending', short: 'First backup soon', title: 'Backups are on. The first full backup is taken soon.' });
    expect(backupHeadline(st({ last_backup: done as never }))).toEqual({ tone: 'ok', title: 'Backups are on and working.', short: 'Healthy' });
  });
  it('says when the first backup is', () => {
    const now = new Date(2026, 10, 24, 12, 0);
    const h = backupHeadline(st({ next_backup_at: new Date(2026, 10, 25, 3, 0).toISOString() }), now);
    expect(h.title).toMatch(/^Backups are on\. The first full backup is taken tomorrow at /);
  });
});

describe('time words', () => {
  const now = new Date(2026, 10, 24, 12, 0);
  it('names today, tomorrow and yesterday', () => {
    expect(whenText(new Date(2026, 10, 24, 14, 5), now)).toMatch(/^at .+ today$/);
    expect(whenText(new Date(2026, 10, 25, 3, 0), now)).toMatch(/^tomorrow at /);
    expect(whenText(new Date(2026, 10, 23, 3, 0), now)).toMatch(/^yesterday at /);
    expect(whenText(new Date(2026, 10, 18, 3, 0), now)).toMatch(/^on .+ at /);
  });
  it('turns Go durations into words', () => {
    expect(durWords('24h0m0s')).toBe('1 day');
    expect(durWords('24h0m0s', true)).toBe('day');
    expect(durWords('168h0m0s')).toBe('7 days');
    expect(durWords('168h0m0s', true)).toBe('week');
    expect(durWords('12h0m0s')).toBe('12 hours');
    expect(durWords('1m0s')).toBe('1 minute');
    expect(durWords('30s')).toBe('30 seconds');
    expect(days('168h0m0s')).toBe(7);
    expect(days('12h0m0s')).toBe(0);
  });
  it('writes the schedule as one sentence', () => {
    expect(scheduleSentence({ enabled: true, every: '24h0m0s', at: '03:00', keep: '168h0m0s', archive_timeout: '1m0s' })).toBe(
      'Every change is saved within 1 minute. A full copy is taken every day at 03:00 and kept 7 days.',
    );
    expect(scheduleSentence({ enabled: true, every: '12h0m0s', at: '03:00', keep: '336h0m0s', archive_timeout: '1m0s' })).toBe(
      'Every change is saved within 1 minute. A full copy is taken every 12 hours and kept 14 days.',
    );
  });
  it('matches saved values to the choices', () => {
    expect(matchChoice('24h0m0s', EVERY_CHOICES)).toBe('24h');
    expect(matchChoice('336h0m0s', KEEP_CHOICES)).toBe('14d');
    expect(matchChoice('5h0m0s', EVERY_CHOICES)).toBe('');
  });
});

describe('where backups go', () => {
  it('builds the request for each choice', () => {
    expect(draftRequest({ ...EMPTY_DRAFT, choice: 'off' })).toBeNull();
    expect(draftRequest({ ...EMPTY_DRAFT })).toEqual({ kind: 'folder', folder: '', encrypt: false });
    expect(draftRequest({ ...EMPTY_DRAFT, choice: 'folder', folder: ' /mnt/usb/b ', encrypt: true })).toEqual({ kind: 'folder', folder: '/mnt/usb/b', encrypt: true });
    expect(
      draftRequest({ ...EMPTY_DRAFT, choice: 's3', endpoint: 'https://s3.example.com/', bucket: ' b ', prefix: '/coyote/', accessKey: 'a', secretKey: 's' }),
    ).toEqual({ kind: 's3', s3: { endpoint: 'https://s3.example.com', bucket: 'b', prefix: 'coyote', region: '', access_key: 'a', secret_key: 's' } });
  });
  it('a check result belongs to one draft', () => {
    expect(draftKey({ ...EMPTY_DRAFT, choice: 'folder', folder: '/a' })).not.toBe(draftKey({ ...EMPTY_DRAFT, choice: 'folder', folder: '/b' }));
    expect(draftKey({ ...EMPTY_DRAFT, choice: 'folder', folder: '' })).not.toBe(draftKey({ ...EMPTY_DRAFT }));
  });
  it('finds missing S3 fields before asking the agent', () => {
    const ok = { ...EMPTY_DRAFT, choice: 's3' as const, endpoint: 'https://s3.example.com', bucket: 'b', accessKey: 'a', secretKey: 's' };
    expect(s3FieldProblem(ok)).toBe('');
    expect(s3FieldProblem({ ...ok, endpoint: 'https://' })).toBe('Fill in the storage address.');
    expect(s3FieldProblem({ ...ok, endpoint: 's3.example.com' })).toBe('The storage address starts with https://.');
    expect(s3FieldProblem({ ...ok, endpoint: 'https://ACCOUNT-ID.r2.cloudflarestorage.com' })).toMatch(/account ID/);
    expect(s3FieldProblem({ ...ok, bucket: '' })).toBe('Fill in the bucket name.');
    expect(s3FieldProblem({ ...ok, secretKey: '' })).toBe('Fill in the secret key.');
  });
  it('guesses the provider and shortens labels', () => {
    expect(providerOf('https://s3.us-west-004.backblazeb2.com')).toBe('b2');
    expect(providerOf('https://s3.eu-central-1.amazonaws.com')).toBe('s3');
    expect(providerOf('https://abc.r2.cloudflarestorage.com')).toBe('r2');
    expect(providerOf('https://minio.home:9000')).toBe('other');
    expect(whereShort('s3', 's3://b/coyote (https://s3.example.com)')).toBe('s3://b/coyote');
    expect(whereShort('folder', '/x')).toBe('/x');
  });
});
