import { describe, expect, it } from 'vitest';
import type { Connection } from '../../src/api/types';
import { agentBlock, CA_PLACEHOLDER, partsFor, queryValue, singleHostUrl, snippets, url } from '../../src/lib/snippets';
import { connection, SCENARIOS } from '../../mock/world.mjs';

const conn: Connection = {
  database: 'app',
  user: 'app',
  sslmode: 'verify-full',
  hosts: [
    { node: 'a', host: '100.64.0.41', port: 5432, this: true, reachable: true },
    { node: 'b', host: 'fd7a::2', port: 5432, this: false, reachable: true },
  ],
  local: { host: '127.0.0.1', port: 5432 },
  ca_file: '/Library/Application Support/CoyoteDB/ca.crt',
};

describe('connection strings', () => {
  it('multi-host with placeholders: verify-full, the CA certificate, read-write, timeout', () => {
    expect(url(partsFor(conn, conn.hosts, null, null))).toBe(
      'postgresql://app:YOUR_PASSWORD@100.64.0.41:5432,[fd7a::2]:5432/app?sslmode=verify-full&sslrootcert=/path/to/coyote-ca.crt&target_session_attrs=read-write&connect_timeout=10',
    );
  });
  it('this computer: the CA path is percent-encoded with %20, never +', () => {
    expect(url(partsFor(conn, [conn.local!], 'p@ss/w:rd', conn.ca_file!))).toBe(
      'postgresql://app:p%40ss%2Fw%3Ard@127.0.0.1:5432/app?sslmode=verify-full&sslrootcert=/Library/Application%20Support/CoyoteDB/ca.crt&connect_timeout=10',
    );
    expect(queryValue('C:\\ProgramData\\CoyoteDB\\ca.crt')).toBe('C:%5CProgramData%5CCoyoteDB%5Cca.crt');
    expect(queryValue('a+b&c=d#e')).toBe('a%2Bb%26c%3Dd%23e');
  });
  it('single-host variant for one-address drivers', () => {
    expect(singleHostUrl(partsFor(conn, conn.hosts, null, null))).toBe(
      'postgresql://app:YOUR_PASSWORD@100.64.0.41:5432/app?sslmode=verify-full&sslrootcert=/path/to/coyote-ca.crt&connect_timeout=10',
    );
  });
  it('every snippet verifies the server, and none leaks a password before reveal', () => {
    const list = snippets(partsFor(conn, conn.hosts, null, null));
    expect(list.length).toBeGreaterThanOrEqual(10);
    expect(new Set(list.map((s) => s.id)).size).toBe(list.length);
    const jdbc = list.find((s) => s.id === 'jdbc')!;
    expect(jdbc.code).toBe(
      'jdbc:postgresql://100.64.0.41:5432,[fd7a::2]:5432/app?user=app&password=YOUR_PASSWORD&sslmode=verify-full&sslrootcert=/path/to/coyote-ca.crt&connectTimeout=10&targetServerType=primary',
    );
    expect(list.find((s) => s.id === 'npgsql')!.code).toContain('SSL Mode=VerifyFull;Root Certificate=/path/to/coyote-ca.crt;Timeout=10');
    expect(list.find((s) => s.id === 'django')!.code).toContain('"sslrootcert": "/path/to/coyote-ca.crt"');
    expect(list.find((s) => s.id === 'postgres-js')!.code).toContain(`ssl: { ca: readFileSync("${CA_PLACEHOLDER}", 'utf8') }`);
    expect(list.find((s) => s.id === 'prisma')!.code).toContain('sslaccept=strict&sslcert=/path/to/coyote-ca.crt');
    // Every snippet either reads DATABASE_URL (which verifies) or names the certificate itself.
    for (const s of list) expect(s.code.includes('DATABASE_URL') || s.code.includes('coyote-ca.crt') || s.code.includes('sslrootcert')).toBe(true);
    expect(list.find((s) => s.id === 'node-pg')!.singleHostOnly).toBe(true);
    expect(list.some((s) => /sslmode=disable|SSL Mode=Disable/.test(s.code))).toBe(false);
  });
  it('agent block names the facts an agent needs', () => {
    const t = agentBlock(partsFor(conn, conn.hosts, null, null), 'Bakery');
    expect(t).toContain('PostgreSQL 18 database run by CoyoteDB (Bakery)');
    expect(t).toContain('It runs on 2 computers. Any host in the connection string works');
    expect(t).toContain('DATABASE_URL');
    expect(t).toContain('sslrootcert');
  });
  it('the mock serves the real agent’s connection shape', () => {
    const w = (SCENARIOS as { id: string; make: () => unknown }[]).find((s) => s.id === 'two-witness')!.make();
    const c = connection(w) as Connection;
    expect(c).toMatchObject({ database: 'app', user: 'app', sslmode: 'verify-full', ca_file: '/var/lib/coyote/ca.crt' });
    expect(c.ca_pem).toMatch(/^-----BEGIN CERTIFICATE-----\n[\s\S]+\n-----END CERTIFICATE-----\n$/);
    expect(c.hosts.map((h) => h.node)).toEqual(['node-a', 'node-b']); // data computers only, no witness
  });
});
