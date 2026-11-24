// Connect your app: connection strings, driver snippets, the cluster's CA
// certificate, a new password, and a block for AI coding agents. The
// password appears only after an explicit reveal.
import { useEffect, useMemo, useState } from 'preact/hooks';
import { api, ApiError, errorMessage } from '../api/client';
import type { Connection } from '../api/types';
import * as I from '../components/icons';
import { A, Busy, CodeBlock, ConfirmDialog, CopyButton, Notice, toast } from '../components/ui';
import { hasFeature } from '../lib/features';
import { agentBlock, CA_FILENAME, CA_PLACEHOLDER, PASSWORD_PLACEHOLDER, partsFor, singleHostUrl, snippets, url } from '../lib/snippets';
import { useStore } from '../lib/store';
import { store } from '../state';

type From = 'all' | 'local' | string; // string: one node's name

const PLACEHOLDERS = new RegExp(`(${PASSWORD_PLACEHOLDER}|${CA_PLACEHOLDER.replace(/[/.]/g, '\\$&')})`);

/** Shows code with the placeholders (password, certificate path) highlighted. */
function Highlight({ code }: { code: string }) {
  const parts = code.split(PLACEHOLDERS);
  return (
    <>
      {parts.map((p, i) => (i % 2 === 1 ? <span key={i} class="ph">{p}</span> : <span key={i}>{p}</span>))}
    </>
  );
}

/** Saves the certificate as a file. A Blob URL made here is same-origin, so the CSP allows it. */
function downloadCert(pem: string): void {
  const blob = new Blob([pem.endsWith('\n') ? pem : pem + '\n'], { type: 'application/x-pem-file' });
  const href = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = href;
  a.download = CA_FILENAME;
  a.className = 'visually-hidden';
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(href), 1000);
}

export function Connect() {
  const status = useStore(store, (s) => s.status);
  const version = useStore(store, (s) => s.version);
  const [conn, setConn] = useState<Connection | null>(null);
  const [err, setErr] = useState('');
  const [password, setPassword] = useState<string | null>(null);
  const [fresh, setFresh] = useState(false);
  const [revealing, setRevealing] = useState(false);
  const [from, setFrom] = useState<From>('all');
  const [tab, setTab] = useState('env');
  const [askNew, setAskNew] = useState(false);
  const [newBusy, setNewBusy] = useState(false);
  const [newErr, setNewErr] = useState('');

  useEffect(() => {
    api.connection().then(setConn, (e) => setErr(errorMessage(e)));
  }, []);
  // Forget the password when leaving the page.
  useEffect(() => () => setPassword(null), []);

  const local = conn?.local ?? null;
  const pick: From = from === 'local' && !local ? 'all' : from;
  const hosts = useMemo(() => {
    if (!conn) return [];
    if (pick === 'local') return local ? [local] : [];
    if (pick === 'all') return conn.hosts;
    return conn.hosts.filter((h) => h.node === pick);
  }, [conn, pick, local]);

  const head = (
    <div class="page-head">
      <h1>Connect your app</h1>
      <p>
        CoyoteDB is regular PostgreSQL 18: any Postgres driver, ORM or tool works. Connect to <strong>any</strong> of your computers — each one passes the connection to the
        main node, wherever it is right now.
      </p>
    </div>
  );

  if (err) {
    return (
      <>
        {head}
        <Notice kind="bad" title="Could not load the connection details"><p>{err}</p></Notice>
      </>
    );
  }
  if (!conn) {
    return (
      <>
        {head}
        <div class="skeleton" style={{ height: '160px' }} aria-label="Loading" />
      </>
    );
  }
  if (conn.pending) {
    return (
      <>
        {head}
        <Notice kind="info" title="Not ready yet" role="status">
          <p>{conn.note ? conn.note.charAt(0).toUpperCase() + conn.note.slice(1) + '.' : 'The database for your apps is not set up yet.'}</p>
          <p>The connection details appear here as soon as it is.</p>
        </Notice>
      </>
    );
  }
  if (hosts.length === 0) {
    return (
      <>
        {head}
        <Notice kind="info" title="No computer takes app connections yet">
          <p>Apps connect through a data computer. Once one is running, its address appears here.</p>
        </Notice>
      </>
    );
  }

  const onThis = pick === 'local';
  const caPath = onThis ? conn.ca_file || null : null;
  const parts = partsFor(conn, hosts, password, caPath);
  const main = url(parts);
  const list = snippets(parts);
  const cur = list.find((s) => s.id === tab) ?? list[0]!;
  const clusterLabel = status?.cluster.name || 'a CoyoteDB cluster';
  const canNewPassword = hasFeature(version, 'app_password');

  const reveal = async () => {
    setRevealing(true);
    try {
      const r = await api.revealPassword();
      setPassword(r.password);
    } catch (e) {
      toast(errorMessage(e), 'bad');
    } finally {
      setRevealing(false);
    }
  };

  const makeNew = async () => {
    setNewBusy(true);
    setNewErr('');
    try {
      const r = await api.newAppPassword();
      setPassword(r.password);
      setFresh(true);
      setAskNew(false);
      toast('New password made. Update your apps with it.');
    } catch (e) {
      setNewErr(
        e instanceof ApiError && (e.code === 'no_leader' || e.code === 'not_leader')
          ? 'A new password needs a majority of your computers to be reachable, and right now they are not. Try again when they are back.'
          : errorMessage(e),
      );
    } finally {
      setNewBusy(false);
    }
  };

  return (
    <>
      {head}

      <section class="panel" aria-labelledby="cs-h">
        <div class="panel-head">
          <h2 id="cs-h" class="section-title">Connection string</h2>
          {password == null ? (
            <button type="button" class="btn small ghost" onClick={reveal} disabled={revealing}>
              <Busy busy={revealing}>
                <I.Eye /> Show password
              </Busy>
            </button>
          ) : (
            <button type="button" class="btn small ghost" onClick={() => { setPassword(null); setFresh(false); }}>
              <I.Lock /> Hide password
            </button>
          )}
        </div>
        {fresh && password != null && (
          <div style={{ marginBottom: '16px' }}>
            <Notice kind="good" title="New password made" role="status">
              <p>Copy it now and update every app that connects to this database. The connection strings below already use it.</p>
              <CodeBlock title="New password" code={password} what="new password" />
            </Notice>
          </div>
        )}
        <div class="field" style={{ marginBottom: '16px' }}>
          <label for="from">Your app runs on</label>
          <select id="from" class="input" value={pick} onChange={(e) => setFrom(e.currentTarget.value)}>
            <option value="all">Another computer — use all {conn.hosts.length} addresses (recommended)</option>
            {local && <option value="local">This computer</option>}
            {conn.hosts.map((h) => (
              <option key={h.node || h.host} value={h.node}>
                Another computer — through {h.node || h.host} only
              </option>
            ))}
          </select>
          <p class="hint">
            {pick === 'all'
              ? 'With every address listed, your app keeps working when one computer stops: the driver simply tries the next.'
              : onThis
                ? 'For apps running on this same computer.'
                : 'Works, but if this one computer stops, your app cannot connect until it is back.'}
          </p>
        </div>
        <CodeBlock title="DATABASE_URL" code={main} what="connection string">
          <Highlight code={main} />
        </CodeBlock>
        <div class="muted small stack" style={{ '--gap': '6px', marginTop: '10px' }}>
          {password == null ? (
            <p style={{ margin: 0 }}>
              <span class="ph-key">{PASSWORD_PLACEHOLDER}</span> stands for the database password. Press <strong>Show password</strong> to put it in.
            </p>
          ) : (
            <p style={{ margin: 0 }}>The password is shown on this page only. Keep it out of code you share: put it in an environment variable.</p>
          )}
          {!onThis && (
            <p style={{ margin: 0 }}>
              <span class="ph-key">{CA_PLACEHOLDER}</span> stands for the certificate file on your app’s computer: download it below, save it there, and put its path in.
            </p>
          )}
        </div>
        {canNewPassword && (
          <>
            <hr class="divider" />
            <div class="switch-row">
              <div>
                <div style={{ fontWeight: 500 }}>Make a new password</div>
                <p class="hint" style={{ margin: '4px 0 0', color: 'var(--muted)' }}>
                  If the password may have leaked, replace it. Apps using the old one must be updated.
                </p>
              </div>
              <button type="button" class="btn small ghost" onClick={() => { setNewErr(''); setAskNew(true); }}>
                Make a new password…
              </button>
            </div>
          </>
        )}
      </section>

      <CaPanel conn={conn} onThis={onThis} />

      <section class="panel" aria-labelledby="sn-h">
        <h2 id="sn-h" class="section-title">For your language or framework</h2>
        <div class="tabs" role="tablist" aria-label="Language or framework">
          {list.map((s) => (
            <button key={s.id} role="tab" type="button" id={`tab-${s.id}`} aria-selected={s.id === cur.id} aria-controls="snippet" tabIndex={s.id === cur.id ? 0 : -1} onClick={() => setTab(s.id)}
              onKeyDown={(e) => {
                const i = list.findIndex((x) => x.id === cur.id);
                if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
                  const n = list[(i + (e.key === 'ArrowRight' ? 1 : list.length - 1)) % list.length]!;
                  setTab(n.id);
                  document.getElementById(`tab-${n.id}`)?.focus();
                }
              }}>
              {s.label}
            </button>
          ))}
        </div>
        <div id="snippet" role="tabpanel" aria-labelledby={`tab-${cur.id}`}>
          <p class="muted small">{cur.note}</p>
          <CodeBlock title={cur.label} code={cur.code} what={`${cur.label} snippet`}>
            <Highlight code={cur.code} />
          </CodeBlock>
          {cur.singleHostOnly && hosts.length > 1 && (
            <div style={{ marginTop: '12px' }}>
              <Notice kind="info" title="This driver takes one address">
                <p>It connects through the first computer only.{cur.id === 'prisma' ? ' Use the address shown in the comment above.' : ' Set DATABASE_URL to:'}</p>
                {cur.id !== 'prisma' && (
                  <CodeBlock code={singleHostUrl(parts)} what="single-host connection string">
                    <Highlight code={singleHostUrl(parts)} />
                  </CodeBlock>
                )}
              </Notice>
            </div>
          )}
        </div>
      </section>

      <section class="panel" aria-labelledby="ai-h">
        <h2 id="ai-h" class="section-title">For your AI coding agent</h2>
        <p class="ink2">Paste this into your agent’s instructions (for example <code>AGENTS.md</code> or <code>CLAUDE.md</code>) so it knows how to use this database.</p>
        <CodeBlock title="Agent instructions" code={agentBlock(parts, clusterLabel)} what="agent instructions">
          <Highlight code={agentBlock(parts, clusterLabel)} />
        </CodeBlock>
        <p class="muted small" style={{ marginTop: '12px', marginBottom: 0 }}>
          A direct connection for agents (MCP) arrives in a later version: see <A href="/agents">Coding agents</A>.
        </p>
      </section>

      <ConfirmDialog
        open={askNew}
        title="Make a new password?"
        confirmLabel="Make a new password"
        busy={newBusy}
        error={newErr}
        onClose={() => setAskNew(false)}
        onConfirm={() => void makeNew()}
      >
        <ul class="consequences">
          <li>The old password stops working shortly after. Every app that uses it must be updated with the new one, or it cannot open new connections.</li>
          <li>Connections that are already open stay up.</li>
          <li>The new password is shown once, right after this.</li>
        </ul>
      </ConfirmDialog>
    </>
  );
}

function CaPanel({ conn, onThis }: { conn: Connection; onThis: boolean }) {
  const pem = conn.ca_pem ?? '';
  if (!pem && !conn.ca_file) return null;
  return (
    <section class="panel" aria-labelledby="ca-h">
      <h2 id="ca-h" class="section-title">Security certificate</h2>
      <p class="ink2">
        Connections are encrypted. Your app checks it is really talking to your database using this database’s certificate (sslrootcert in the connection string).
        It is not a secret.
      </p>
      {conn.ca_file && (
        <p class="small" style={{ overflowWrap: 'anywhere' }}>
          {onThis ? 'For apps on this computer it is already here:' : 'On this computer it is at'} <code>{conn.ca_file}</code>
        </p>
      )}
      {pem && (
        <>
          <p class="small">On another computer, save it as a file (for example <code>{CA_FILENAME}</code>) and point sslrootcert at that file.</p>
          <div class="row" style={{ gap: '8px' }}>
            <button type="button" class="btn small" onClick={() => downloadCert(pem)}>
              Download {CA_FILENAME}
            </button>
            <CopyButton text={pem} label="Copy certificate" />
          </div>
        </>
      )}
    </section>
  );
}
