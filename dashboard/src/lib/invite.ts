// Invite codes, decoded in the browser for instant, friendly validation.
// Mirrors internal/invite/invite.go (format version 1, D21) byte for byte;
// the server decodes again and stays the authority.

export const PREFIX = 'coyote-';
const ALPHABET = '0123456789abcdefghjkmnpqrstvwxyz';

export interface DecodedInvite {
  clusterId: string;
  addresses: string[];
  expires: Date;
}

export type InviteProblem = 'empty' | 'not_invite' | 'corrupt' | 'version';

export type InviteResult =
  | { ok: true; invite: DecodedInvite; normalized: string; expired: boolean }
  | { ok: false; problem: InviteProblem };

/** Same as invite.Normalize: drop whitespace, line breaks, quotes, angle brackets; lower-case. */
export function normalizeInvite(s: string): string {
  return s.replace(/[\s"'`<>]/g, '').toLowerCase();
}

const decodeMap: Int8Array = (() => {
  const m = new Int8Array(128).fill(-1);
  for (let i = 0; i < ALPHABET.length; i++) {
    const c = ALPHABET.charCodeAt(i);
    m[c] = i;
    if (c >= 97 && c <= 122) m[c - 32] = i;
  }
  for (const [ch, v] of [['o', 0], ['O', 0], ['i', 1], ['I', 1], ['l', 1], ['L', 1]] as const) m[ch.charCodeAt(0)] = v;
  return m;
})();

function decode32(s: string): Uint8Array | null {
  const out: number[] = [];
  let acc = 0;
  let bits = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    const v = c < 128 ? decodeMap[c]! : -1;
    if (v < 0) return null;
    acc = ((acc << 5) | v) & 0xffff;
    bits += 5;
    if (bits >= 8) {
      bits -= 8;
      out.push((acc >> bits) & 0xff);
    }
  }
  if (bits >= 5 || (acc & ((1 << bits) - 1)) !== 0) return null;
  return Uint8Array.from(out);
}

let crcTable: Uint32Array | null = null;
export function crc32(b: Uint8Array): number {
  if (!crcTable) {
    crcTable = new Uint32Array(256);
    for (let n = 0; n < 256; n++) {
      let c = n;
      for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
      crcTable[n] = c >>> 0;
    }
  }
  let c = 0xffffffff;
  for (const x of b) c = crcTable[(c ^ x) & 0xff]! ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

class Reader {
  bad = false;
  private i = 0;
  private readonly b: Uint8Array;
  constructor(b: Uint8Array) {
    this.b = b;
  }
  get left(): number {
    return this.b.length - this.i;
  }
  take(n: number): Uint8Array {
    if (this.bad || this.left < n) {
      this.bad = true;
      return new Uint8Array(n);
    }
    const v = this.b.subarray(this.i, this.i + n);
    this.i += n;
    return v;
  }
  u8(): number {
    return this.take(1)[0]!;
  }
  u16(): number {
    const v = this.take(2);
    return (v[0]! << 8) | v[1]!;
  }
  uvarint(): number {
    let x = 0;
    let mul = 1;
    for (let k = 0; k < 10; k++) {
      if (this.bad || this.left < 1) {
        this.bad = true;
        return 0;
      }
      const byte = this.u8();
      x += (byte & 0x7f) * mul;
      if (byte < 0x80) return x;
      mul *= 128;
    }
    this.bad = true;
    return 0;
  }
}

const hex = (b: Uint8Array) => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');

function ipv6(b: Uint8Array): string {
  const groups: string[] = [];
  for (let i = 0; i < 16; i += 2) groups.push(((b[i]! << 8) | b[i + 1]!).toString(16));
  // Compress the longest run of zero groups (RFC 5952), as Go prints it.
  let best = -1;
  let bestLen = 0;
  for (let i = 0; i < 8; ) {
    if (groups[i] !== '0') {
      i++;
      continue;
    }
    let j = i;
    while (j < 8 && groups[j] === '0') j++;
    if (j - i > bestLen && j - i > 1) {
      best = i;
      bestLen = j - i;
    }
    i = j;
  }
  if (best < 0) return groups.join(':');
  return groups.slice(0, best).join(':') + '::' + groups.slice(best + bestLen).join(':');
}

const HOST_RE = /^[A-Za-z0-9._-]{1,253}$/;

export function decodeInvite(text: string, now: Date = new Date()): InviteResult {
  const s = normalizeInvite(text);
  if (s === '') return { ok: false, problem: 'empty' };
  if (!s.startsWith(PREFIX)) return { ok: false, problem: 'not_invite' };
  const bytes = decode32(s.slice(PREFIX.length).replace(/-/g, ''));
  if (!bytes || bytes.length < 5) return { ok: false, problem: 'corrupt' };
  const payload = bytes.subarray(0, bytes.length - 4);
  const t = bytes.subarray(bytes.length - 4);
  const sum = ((t[0]! << 24) | (t[1]! << 16) | (t[2]! << 8) | t[3]!) >>> 0;
  if (crc32(payload) !== sum) return { ok: false, problem: 'corrupt' };
  if (payload[0] !== 1) return { ok: false, problem: 'version' };
  const r = new Reader(payload.subarray(1));
  const clusterId = hex(r.take(16));
  r.take(32); // secret
  r.take(32); // CA fingerprint
  const exp = r.uvarint();
  if (exp > 2 ** 40) r.bad = true;
  const n = r.u8();
  if (n === 0 || n > 16) r.bad = true;
  const addresses: string[] = [];
  for (let i = 0; i < n && !r.bad; i++) {
    let host = '';
    let v6 = false;
    switch (r.u8()) {
      case 4:
        host = Array.from(r.take(4)).join('.');
        break;
      case 6:
        host = ipv6(r.take(16));
        v6 = true;
        break;
      case 1: {
        const l = r.u8();
        host = new TextDecoder().decode(r.take(l));
        if (!HOST_RE.test(host)) r.bad = true;
        break;
      }
      default:
        r.bad = true;
    }
    const port = r.u16();
    addresses.push(v6 ? `[${host}]:${port}` : `${host}:${port}`);
  }
  if (r.bad || r.left !== 0) return { ok: false, problem: 'corrupt' };
  const expires = new Date(exp * 1000);
  return { ok: true, invite: { clusterId, addresses, expires }, normalized: s, expired: now.getTime() >= expires.getTime() };
}

/** Plain words for a problem, for the paste box. */
export function inviteProblemText(p: InviteProblem | 'expired'): string {
  switch (p) {
    case 'empty':
      return '';
    case 'not_invite':
      return 'That doesn’t look like an invite code. Invite codes start with “coyote-”.';
    case 'corrupt':
      return 'Part of the code is missing or mistyped. Copy the whole code again — it’s fine if it includes spaces or line breaks.';
    case 'version':
      return 'This code was made by a newer version of CoyoteDB. Update CoyoteDB on this computer first.';
    case 'expired':
      return 'This invite code has expired. On a computer that is already in the cluster, make a new one.';
  }
}
