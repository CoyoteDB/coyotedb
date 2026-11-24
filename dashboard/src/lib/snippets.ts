// Connection strings and driver snippets, built from GET /v1/connection.
// The password is a placeholder until the person reveals it. TLS is always
// on: apps verify the server against the cluster's CA certificate
// (sslmode=verify-full + sslrootcert), like the URL the CLI prints.
import type { Connection } from '../api/types';

export const PASSWORD_PLACEHOLDER = 'YOUR_PASSWORD';
/** Where the app's computer keeps the downloaded CA certificate. */
export const CA_PLACEHOLDER = '/path/to/coyote-ca.crt';
/** The file name the CA certificate downloads as. */
export const CA_FILENAME = 'coyote-ca.crt';

export interface ConnParts {
  user: string;
  password: string | null; // null: show placeholder
  database: string;
  sslmode: string;
  /** Path of the CA certificate on the app's computer; null: placeholder. */
  caPath: string | null;
  hosts: { host: string; port: number }[];
}

export function hostPort(h: { host: string; port: number }): string {
  const host = h.host.includes(':') && !h.host.startsWith('[') ? `[${h.host}]` : h.host;
  return `${host}:${h.port}`;
}

export function partsFor(c: Connection, hosts: { host: string; port: number }[], password: string | null, caPath: string | null): ConnParts {
  return { user: c.user, password, database: c.database, sslmode: c.sslmode, caPath, hosts: hosts.map((h) => ({ host: h.host, port: h.port })) };
}

const pw = (p: ConnParts) => (p.password == null ? PASSWORD_PLACEHOLDER : encodeURIComponent(p.password));
const ca = (p: ConnParts) => p.caPath ?? CA_PLACEHOLDER;

/**
 * A query value for libpq: percent-encoded (libpq decodes %XX but never
 * '+', so a space must be %20), with '/' and ':' left readable.
 */
export function queryValue(v: string): string {
  return encodeURIComponent(v).replace(/%2F/gi, '/').replace(/%3A/gi, ':');
}

/** postgresql://user:pw@a:5432,b:5432/db?sslmode=verify-full&sslrootcert=…[&target_session_attrs=read-write]&connect_timeout=10 */
export function url(p: ConnParts): string {
  const q = [`sslmode=${queryValue(p.sslmode)}`, `sslrootcert=${queryValue(ca(p))}`];
  if (p.hosts.length > 1) q.push('target_session_attrs=read-write');
  q.push('connect_timeout=10');
  return `postgresql://${encodeURIComponent(p.user)}:${pw(p)}@${p.hosts.map(hostPort).join(',')}/${encodeURIComponent(p.database)}?${q.join('&')}`;
}

export interface Snippet {
  id: string;
  label: string;
  /** What it is, in a few words. */
  note: string;
  lang: string;
  code: string;
  /** This driver cannot use several hosts; the snippet uses the first. */
  singleHostOnly?: boolean;
}

function single(p: ConnParts): ConnParts {
  return { ...p, hosts: p.hosts.slice(0, 1) };
}

/** Npgsql spells verify-full as VerifyFull. */
function npgsqlMode(m: string): string {
  return m
    .split('-')
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join('');
}

export function snippets(p: ConnParts): Snippet[] {
  const multi = url(p);
  const many = p.hosts.length > 1;
  const first = p.hosts[0] ?? { host: '127.0.0.1', port: 5432 };
  const password = p.password ?? PASSWORD_PLACEHOLDER;
  const caPath = ca(p);
  const jsPath = JSON.stringify(caPath);
  const npgHosts = p.hosts.map((h) => (h.port === 5432 ? h.host : `${h.host}:${h.port}`)).join(',');
  const jdbcHosts = p.hosts.map(hostPort).join(',');
  const out: Snippet[] = [
    { id: 'env', label: '.env file', note: 'Most frameworks read DATABASE_URL', lang: 'ini', code: `DATABASE_URL="${multi}"` },
    { id: 'psql', label: 'psql', note: 'The Postgres command line', lang: 'sh', code: `psql "${multi}"` },
    {
      id: 'node-pg',
      label: 'Node.js · pg',
      note: 'node-postgres reads sslrootcert from the connection string',
      lang: 'js',
      code: `import pg from 'pg';\n\nconst pool = new pg.Pool({ connectionString: process.env.DATABASE_URL });\nconst { rows } = await pool.query('select now()');`,
      singleHostOnly: true,
    },
    {
      id: 'postgres-js',
      label: 'Node.js · postgres',
      note: 'postgres.js, also used by Drizzle; give it the certificate directly',
      lang: 'js',
      code: `import { readFileSync } from 'node:fs';\nimport postgres from 'postgres';\n\nconst sql = postgres(process.env.DATABASE_URL, {\n  ssl: { ca: readFileSync(${jsPath}, 'utf8') },\n});\nconst rows = await sql\`select now()\`;`,
    },
    {
      id: 'prisma',
      label: 'Prisma',
      note: 'schema.prisma; Prisma names the certificate sslcert',
      lang: 'prisma',
      code: `// DATABASE_URL="postgresql://${encodeURIComponent(p.user)}:${pw(p)}@${hostPort(first)}/${encodeURIComponent(p.database)}?sslmode=require&sslaccept=strict&sslcert=${queryValue(caPath)}&connect_timeout=10"\ndatasource db {\n  provider = "postgresql"\n  url      = env("DATABASE_URL")\n}`,
      singleHostOnly: true,
    },
    {
      id: 'psycopg',
      label: 'Python · psycopg',
      note: 'psycopg 3',
      lang: 'py',
      code: `import os, psycopg\n\nwith psycopg.connect(os.environ["DATABASE_URL"]) as conn:\n    print(conn.execute("select now()").fetchone())`,
    },
    {
      id: 'sqlalchemy',
      label: 'Python · SQLAlchemy',
      note: 'with the psycopg driver',
      lang: 'py',
      code: `from sqlalchemy import create_engine\n\nengine = create_engine("${multi.replace('postgresql://', 'postgresql+psycopg://')}")`,
    },
    {
      id: 'django',
      label: 'Django',
      note: 'settings.py',
      lang: 'py',
      code: `DATABASES = {\n    "default": {\n        "ENGINE": "django.db.backends.postgresql",\n        "HOST": "${many ? p.hosts.map((h) => h.host).join(',') : first.host}",\n        "PORT": "${first.port}",\n        "NAME": "${p.database}",\n        "USER": "${p.user}",\n        "PASSWORD": "${password}",\n        "OPTIONS": {\n            "sslmode": "${p.sslmode}",\n            "sslrootcert": ${jsPath},\n            "connect_timeout": 10,${many ? '\n            "target_session_attrs": "read-write",' : ''}\n        },\n    }\n}`,
    },
    {
      id: 'pgx',
      label: 'Go · pgx',
      note: 'jackc/pgx v5',
      lang: 'go',
      code: `pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))\nif err != nil {\n\treturn err\n}\ndefer pool.Close()`,
    },
    {
      id: 'jdbc',
      label: 'Java · JDBC',
      note: 'PostgreSQL JDBC driver',
      lang: 'properties',
      code: `jdbc:postgresql://${jdbcHosts}/${p.database}?user=${p.user}&password=${password}&sslmode=${p.sslmode}&sslrootcert=${queryValue(caPath)}&connectTimeout=10${many ? '&targetServerType=primary' : ''}`,
    },
    {
      id: 'npgsql',
      label: '.NET · Npgsql',
      note: 'connection string',
      lang: 'ini',
      code: `Host=${npgHosts};Port=${first.port};Database=${p.database};Username=${p.user};Password=${password};SSL Mode=${npgsqlMode(p.sslmode)};Root Certificate=${caPath};Timeout=10${many ? ';Target Session Attributes=read-write' : ''}`,
    },
    {
      id: 'rails',
      label: 'Ruby on Rails',
      note: 'config/database.yml',
      lang: 'yaml',
      code: `production:\n  adapter: postgresql\n  url: <%= ENV["DATABASE_URL"] %>`,
    },
  ];
  return out;
}

/** For drivers that take one host only: the same string with the first host. */
export function singleHostUrl(p: ConnParts): string {
  return url(single(p));
}

/** Text to paste into an AI coding agent's instructions or chat. */
export function agentBlock(p: ConnParts, clusterLabel: string): string {
  const lines = [
    `This project uses a PostgreSQL 18 database run by CoyoteDB (${clusterLabel}).`,
    `Connection string (keep it in the DATABASE_URL environment variable, never in code):`,
    `  ${url(p)}`,
    ``,
    `Facts about this database:`,
    `- It is regular PostgreSQL 18: use normal SQL, migrations and Postgres drivers.`,
    p.hosts.length > 1
      ? `- It runs on ${p.hosts.length} computers. Any host in the connection string works: each forwards to the current main node. Keep all hosts in the string so the app survives one computer stopping.`
      : `- It runs on one computer.`,
    `- Connections use TLS and must verify the server (sslmode=${p.sslmode}) against the cluster's CA certificate at ${ca(p)} (sslrootcert). Do not turn verification off.`,
    `- During a switch-over, open connections are closed. Use a connection pool and retry a failed query once.`,
    `- Do not change server settings (ALTER SYSTEM), replication or roles named coyote_*: CoyoteDB manages them.`,
  ];
  return lines.join('\n');
}
