import { describe, expect, it } from 'vitest';
import { addressProblem, splitAddresses } from '../../src/lib/addresses';
import { hasFeature } from '../../src/lib/features';
import type { VersionInfo } from '../../src/api/types';

describe('change addresses input', () => {
  it('splits on commas, spaces and new lines, without repeats', () => {
    expect(splitAddresses(' 100.64.0.42:7481,\n192.168.1.9:7481  100.64.0.42:7481\n')).toEqual(['100.64.0.42:7481', '192.168.1.9:7481']);
    expect(splitAddresses('  \n ,')).toEqual([]);
  });
  it('accepts HOST:PORT, names and bracketed IPv6', () => {
    for (const a of ['100.64.0.42:7481', 'office-mini.local:7481', '[fd7a:115c::2]:7481']) expect(addressProblem(a)).toBeNull();
  });
  it('explains what is wrong', () => {
    expect(addressProblem('100.64.0.42')).toMatch(/is not HOST:PORT/);
    expect(addressProblem('fd7a::2:7481')).toMatch(/is not HOST:PORT/);
    expect(addressProblem('host:70000')).toMatch(/between 1 and 65535/);
    expect(addressProblem('host:0')).toMatch(/between 1 and 65535/);
  });
});

describe('features', () => {
  const base: VersionInfo = { product: 'CoyoteDB', version: '0.6.0', commit: '', go_version: '', os: 'linux', arch: 'amd64', postgres_version: '18', api: 1 };
  it('a missing list or a missing key means not available', () => {
    expect(hasFeature(null, 'rename')).toBe(false);
    expect(hasFeature(base, 'app_password')).toBe(false);
    expect(hasFeature({ ...base, features: { app_password: true } }, 'rename')).toBe(false);
    expect(hasFeature({ ...base, features: { app_password: true, rename: false } }, 'app_password')).toBe(true);
  });
});
