// Invite codes for the mock: the exact binary format of internal/invite
// (version 1), so codes made here decode in the dashboard and vice versa.
import { randomBytes } from 'node:crypto';

const ALPHABET = '0123456789abcdefghjkmnpqrstvwxyz';

function crc32(b) {
  let c = 0xffffffff;
  for (const x of b) {
    c ^= x;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  }
  return (c ^ 0xffffffff) >>> 0;
}

function encode32(bytes) {
  let out = '';
  let acc = 0;
  let bits = 0;
  for (const c of bytes) {
    acc = ((acc << 8) | c) & 0xffff;
    bits += 8;
    while (bits >= 5) {
      bits -= 5;
      out += ALPHABET[(acc >> bits) & 31];
    }
  }
  if (bits > 0) out += ALPHABET[(acc << (5 - bits)) & 31];
  return out;
}

function uvarint(n) {
  const out = [];
  while (n >= 0x80) {
    out.push((n % 128) | 0x80);
    n = Math.floor(n / 128);
  }
  out.push(n);
  return out;
}

/** @param {{clusterId: string, addrs: string[], expires: Date, secret?: Buffer, ca?: Buffer}} inv */
export function encodeInvite(inv) {
  const b = [1];
  b.push(...Buffer.from(inv.clusterId, 'hex'));
  b.push(...(inv.secret ?? randomBytes(32)));
  b.push(...(inv.ca ?? randomBytes(32)));
  b.push(...uvarint(Math.floor(inv.expires.getTime() / 1000)));
  b.push(inv.addrs.length);
  for (const a of inv.addrs) {
    const m = /^(?:\[(.+)\]|([^:]+)):(\d+)$/.exec(a);
    if (!m) throw new Error('bad address ' + a);
    const host = m[1] ?? m[2];
    const port = Number(m[3]);
    if (/^\d+\.\d+\.\d+\.\d+$/.test(host)) b.push(4, ...host.split('.').map(Number));
    else if (m[1]) b.push(6, ...ipv6Bytes(host));
    else b.push(1, host.length, ...Buffer.from(host, 'ascii'));
    b.push(port >> 8, port & 0xff);
  }
  const sum = crc32(b);
  b.push((sum >>> 24) & 0xff, (sum >>> 16) & 0xff, (sum >>> 8) & 0xff, sum & 0xff);
  return 'coyote-' + encode32(Uint8Array.from(b));
}

function ipv6Bytes(s) {
  const [head, tail] = s.split('::');
  const h = head ? head.split(':') : [];
  const t = tail !== undefined && tail ? tail.split(':') : [];
  const groups = [...h, ...Array(8 - h.length - t.length).fill('0'), ...t];
  return groups.flatMap((g) => {
    const v = parseInt(g, 16);
    return [v >> 8, v & 0xff];
  });
}

/** Garble a code the way a bad paste does: change one character in the middle. */
export function garble(code) {
  const i = Math.floor(code.length / 2);
  const c = code[i] === 'a' ? 'b' : 'a';
  return code.slice(0, i) + c + code.slice(i + 1);
}
