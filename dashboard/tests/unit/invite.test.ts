import { describe, expect, it } from 'vitest';
import { crc32, decodeInvite, normalizeInvite } from '../../src/lib/invite';
import { encodeInvite, garble } from '../../mock/invite.mjs';

// Made by internal/invite.(*Invite).Encode (Go) on 2026-11-20: cluster
// 3fa9c2e17b4d4e0a9c1d2b3e4f5a6b7c, secret 00..1f, CA ff..e0, expiry
// 1795000000, three addresses (IPv4, IPv6, host name).
const GO_VECTOR =
  'coyote-04ztkgq1fd6mw2mw3mnkwkttddy000820c20a1g7104gm2rc1m70y40h289h858p2wc1j6gv3gehw7zzzvyzsyztz7wffxqnyksz5wfgxzqevv7bxbmyhsz6wqje7rq1w308vxpq0r1g8s2000nhte86znx12q51w00000000000000004ekj08cdsqp8s9dc4q6rvv3c5p1teex1w43w';

describe('decodeInvite', () => {
  it('decodes a code made by the Go encoder', () => {
    const r = decodeInvite(GO_VECTOR, new Date('2026-11-01T00:00:00Z'));
    expect(r.ok).toBe(true);
    if (!r.ok) return;
    expect(r.invite.clusterId).toBe('3fa9c2e17b4d4e0a9c1d2b3e4f5a6b7c');
    expect(r.invite.addresses).toEqual(['100.64.0.43:7481', '[fd7a:115c:a1e0::1]:7481', 'node-a.local:7481']);
    expect(r.invite.expires.getTime()).toBe(1795000000 * 1000);
    expect(r.expired).toBe(false);
  });

  it('is expired after its time', () => {
    const r = decodeInvite(GO_VECTOR, new Date(1795000000 * 1000));
    expect(r.ok && r.expired).toBe(true);
  });

  it('tolerates what pasting adds, like the Go side', () => {
    const messy = `  "${GO_VECTOR.slice(0, 50).toUpperCase()}\n ${GO_VECTOR.slice(50, 120)}\r\n\t${GO_VECTOR.slice(120)}"  `;
    expect(normalizeInvite(messy)).toBe(GO_VECTOR);
    expect(decodeInvite(messy, new Date('2026-11-01T00:00:00Z')).ok).toBe(true);
  });

  it('maps look-alike letters o→0 and i/l→1', () => {
    // Build a code whose body contains 0 and 1, then type them as o and l.
    const code: string = encodeInvite({ clusterId: '00000000000000000000000000000001', addrs: ['10.0.0.1:7481'], expires: new Date(2_000_000_000_000) });
    const typo = 'coyote-' + code.slice(7).replace(/0/g, 'o').replace(/1/g, 'l');
    expect(decodeInvite(typo).ok).toBe(true);
  });

  it('round-trips the mock encoder', () => {
    const exp = new Date(Math.floor(Date.now() / 1000 + 3600) * 1000);
    const code: string = encodeInvite({ clusterId: 'aa'.repeat(16), addrs: ['100.1.2.3:7481', '[::1]:7481', 'box.lan:9'], expires: exp });
    const r = decodeInvite(code);
    expect(r).toMatchObject({ ok: true, expired: false, invite: { clusterId: 'aa'.repeat(16), addresses: ['100.1.2.3:7481', '[::1]:7481', 'box.lan:9'], expires: exp } });
  });

  it('catches every single-character change and truncation', () => {
    expect(decodeInvite(garble(GO_VECTOR))).toEqual({ ok: false, problem: 'corrupt' });
    expect(decodeInvite(GO_VECTOR.slice(0, -3))).toEqual({ ok: false, problem: 'corrupt' });
    expect(decodeInvite(GO_VECTOR + 'q')).toEqual({ ok: false, problem: 'corrupt' });
    expect(decodeInvite(GO_VECTOR.replace('w', 'u'))).toEqual({ ok: false, problem: 'corrupt' }); // u is not in the alphabet
  });

  it('says what is wrong', () => {
    expect(decodeInvite('   ')).toEqual({ ok: false, problem: 'empty' });
    expect(decodeInvite('hello')).toEqual({ ok: false, problem: 'not_invite' });
    expect(decodeInvite('coyote-')).toEqual({ ok: false, problem: 'corrupt' });
  });

  it('crc32 matches IEEE', () => {
    expect(crc32(new TextEncoder().encode('123456789'))).toBe(0xcbf43926);
  });
});
