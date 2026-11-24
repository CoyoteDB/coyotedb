import { describe, expect, it } from 'vitest';
import { ago, bytes, duration, lagText, parseGoDuration, platformLabel, platformShort, until } from '../../src/lib/format';

const now = new Date('2026-11-20T14:00:00Z');

describe('format', () => {
  it('bytes', () => {
    expect([bytes(0), bytes(1023), bytes(2048), bytes(94 * 1024 * 1024), bytes(3_067_833_856), bytes(-1)]).toEqual(['0 B', '1023 B', '2.0 KB', '94.0 MB', '2.9 GB', '—']);
  });
  it('duration', () => {
    expect([duration(400), duration(1000), duration(21_400), duration(60_000), duration(150_000), duration(3_900_000), duration(90_000_000)]).toEqual([
      'less than a second', '1 second', '21 seconds', '1 minute', '2 minutes', '1 hour 5 min', '1 day',
    ]);
  });
  it('ago and until', () => {
    expect(ago(new Date(now.getTime() - 2000), now)).toBe('just now');
    expect(ago(new Date(now.getTime() - 40_000), now)).toBe('40 seconds ago');
    expect(ago(new Date(now.getTime() - 6 * 60_000), now)).toBe('6 minutes ago');
    expect(ago(null, now)).toBe('never');
    expect(until(new Date(now.getTime() + 52 * 60_000), now)).toBe('in 52 minutes');
    expect(until(new Date(now.getTime() - 1), now)).toBe('expired');
  });
  it('lag', () => {
    expect(lagText(-1)).toBe('unknown');
    expect(lagText(0)).toBe('up to date');
    expect(lagText(4000, 0)).toBe('up to date');
    expect(lagText(5 * 1024 * 1024, 3)).toBe('5.0 MB behind (about 3 seconds)');
  });
  it('platforms', () => {
    expect(platformLabel({ os: 'linux', arch: 'amd64' })).toBe('Linux on Intel/AMD 64-bit');
    expect(platformLabel({ os: 'darwin', arch: 'arm64' })).toBe('macOS on Apple silicon');
    expect(platformLabel({ os: 'windows', arch: 'amd64' })).toBe('Windows on Intel/AMD 64-bit');
    expect(platformShort({ os: 'linux', arch: 'arm64' })).toBe('Linux · ARM64');
    expect(platformLabel(null)).toBe('unknown system');
  });
  it('Go durations', () => {
    expect([parseGoDuration('30s'), parseGoDuration('1m0s'), parseGoDuration('1h30m'), parseGoDuration('500ms')]).toEqual([30_000, 60_000, 5_400_000, 500]);
    expect(parseGoDuration('soon')).toBeNaN();
  });
});
