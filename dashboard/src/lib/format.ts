// Formatting helpers: plain words for bytes, time and platforms.
import type { Platform } from '../api/types';

export function bytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v >= 100 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

/** "about 4 seconds", "2 minutes", "1 hour 5 minutes". */
export function duration(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 1) return 'less than a second';
  if (s < 60) return s === 1 ? '1 second' : `${s} seconds`;
  const m = Math.floor(s / 60);
  if (m < 60) return m === 1 ? '1 minute' : `${m} minutes`;
  const h = Math.floor(m / 60);
  const rm = m % 60;
  const hs = h === 1 ? '1 hour' : `${h} hours`;
  if (h >= 24) {
    const d = Math.floor(h / 24);
    return d === 1 ? '1 day' : `${d} days`;
  }
  return rm ? `${hs} ${rm} min` : hs;
}

/** "just now", "12 seconds ago", "3 minutes ago", "yesterday at 14:03". */
export function ago(t: string | Date | null | undefined, now: Date = new Date()): string {
  if (!t) return 'never';
  const d = typeof t === 'string' ? new Date(t) : t;
  const diff = now.getTime() - d.getTime();
  if (!Number.isFinite(diff)) return 'unknown';
  if (diff < 5_000) return 'just now';
  if (diff < 60_000) return `${Math.round(diff / 1000)} seconds ago`;
  if (diff < 3_600_000) {
    const m = Math.round(diff / 60_000);
    return m === 1 ? '1 minute ago' : `${m} minutes ago`;
  }
  if (diff < 12 * 3_600_000) {
    const h = Math.round(diff / 3_600_000);
    return h === 1 ? '1 hour ago' : `${h} hours ago`;
  }
  return clock(d, now);
}

/** "14:03", "yesterday at 14:03", "Mon 18 Nov at 14:03". */
export function clock(t: string | Date, now: Date = new Date()): string {
  const d = typeof t === 'string' ? new Date(t) : t;
  const hm = d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const dd = Math.round((day(now) - day(d)) / 86_400_000);
  if (dd === 0) return `today at ${hm}`;
  if (dd === 1) return `yesterday at ${hm}`;
  const date = d.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' });
  return `${date} at ${hm}`;
}

/** "in 52 minutes" / "in 3 days" for an expiry. */
export function until(t: string | Date, now: Date = new Date()): string {
  const d = typeof t === 'string' ? new Date(t) : t;
  const ms = d.getTime() - now.getTime();
  if (ms <= 0) return 'expired';
  return 'in ' + duration(ms);
}

const OS: Record<string, string> = { linux: 'Linux', darwin: 'macOS', windows: 'Windows', freebsd: 'FreeBSD' };
const ARCH: Record<string, string> = { amd64: 'Intel/AMD 64-bit', arm64: 'ARM 64-bit', '386': 'Intel 32-bit' };

export function osName(os: string): string {
  return OS[os] ?? os;
}

/** "Linux on Intel/AMD 64-bit", "macOS on Apple silicon". */
export function platformLabel(p: Platform | null | undefined): string {
  if (!p) return 'unknown system';
  if (p.os === 'darwin' && p.arch === 'arm64') return 'macOS on Apple silicon';
  if (p.os === 'darwin' && p.arch === 'amd64') return 'macOS on Intel';
  return `${osName(p.os)} on ${ARCH[p.arch] ?? p.arch}`;
}

/** Short form for node cards: "Linux · x86-64". */
export function platformShort(p: Platform | null | undefined): string {
  if (!p) return '—';
  const arch = p.arch === 'amd64' ? 'x86-64' : p.arch === 'arm64' ? (p.os === 'darwin' ? 'Apple silicon' : 'ARM64') : p.arch;
  return `${osName(p.os)} · ${arch}`;
}

/** How far a copy is behind, in words. */
export function lagText(lagBytes: number, lagSeconds?: number | null): string {
  if (lagBytes < 0) return 'unknown';
  if (lagBytes === 0) return 'up to date';
  if (lagBytes < 64 * 1024 && (lagSeconds ?? 0) < 1) return 'up to date';
  const t = lagSeconds != null && lagSeconds >= 1 ? ` (about ${duration(lagSeconds * 1000)})` : '';
  return `${bytes(lagBytes)} behind${t}`;
}

export function plural(n: number, one: string, many: string): string {
  return `${n} ${n === 1 ? one : many}`;
}

/** Go duration string ("30s", "1m0s", "5m") to milliseconds; NaN when not parseable. */
export function parseGoDuration(s: string): number {
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
  const mult: Record<string, number> = { ns: 1e-6, us: 1e-3, µs: 1e-3, ms: 1, s: 1000, m: 60_000, h: 3_600_000 };
  let total = 0;
  let matched = '';
  for (const m of s.matchAll(re)) {
    total += parseFloat(m[1]!) * mult[m[2]!]!;
    matched += m[0];
  }
  return matched === s && s !== '' ? total : NaN;
}
