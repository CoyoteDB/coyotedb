// Coding agents / MCP (M7). Wired to the contract; until the agent serves
// it (501 not_yet_available), a calm "later version" state.
import { useEffect, useState } from 'preact/hooks';
import { api, ApiError, errorMessage } from '../api/client';
import { hasFeature } from '../lib/features';
import type { AgentKey, Agents as AgentsT } from '../api/types';
import { A, Busy, CodeBlock, ConfirmDialog, Notice, toast, useNow } from '../components/ui';
import { ago } from '../lib/format';
import { useStore } from '../lib/store';
import { store } from '../state';
import { Later } from './Backups';

export function Agents() {
  const [data, setData] = useState<AgentsT | null>(null);
  const [state, setState] = useState<'loading' | 'ok' | 'later' | 'error'>('loading');
  const [err, setErr] = useState('');
  const version = useStore(store, (st) => st.version);
  const load = () => {
    // The agent says it does not serve this yet: no request, no 501.
    if (version && !hasFeature(version, 'agents')) {
      setState('later');
      return Promise.resolve();
    }
    return api.agents().then(
      (d) => {
        setData(d);
        setState('ok');
      },
      (e) => {
        if (e instanceof ApiError && (e.code === 'not_yet_available' || e.status === 501 || e.status === 404)) setState('later');
        else {
          setErr(errorMessage(e));
          setState('error');
        }
      },
    );
  };
  // Load again once the version (with its feature list) has arrived.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => void load(), [version]);
  return (
    <>
      <div class="page-head">
        <h1>Coding agents</h1>
        <p>Let AI coding agents such as Claude Code, Cursor or Codex look at your database directly, through MCP, with a key you can take back at any time.</p>
      </div>
      {state === 'loading' && <div class="skeleton" style={{ height: '200px' }} aria-label="Loading" />}
      {state === 'error' && <Notice kind="bad" title="Could not load agent access"><p>{err}</p></Notice>}
      {state === 'later' && (
        <>
          <Later what="Coding agents">
            <h2 class="section-title">A direct line for your coding agent</h2>
            <ul class="consequences" style={{ marginTop: '12px' }}>
              <li>An MCP server built into CoyoteDB: your agent can list tables, read the schema and run queries.</li>
              <li>A key per agent, read-only by default, that you can revoke here.</li>
              <li>Copy-paste setup for Claude Code, Cursor and other MCP clients.</li>
            </ul>
            <p class="muted small" style={{ margin: 0 }}>This page will let you create keys once your version of CoyoteDB includes it.</p>
          </Later>
          <section class="panel" style={{ marginTop: '20px' }}>
            <h2 class="section-title">Until then</h2>
            <p class="ink2" style={{ margin: 0 }}>
              Give your agent the connection details from <A href="/connect">Connect your app</A>: there is a ready-made block for agent instructions.
            </p>
          </section>
        </>
      )}
      {state === 'ok' && data && <AgentsLive data={data} reload={load} />}
    </>
  );
}

function AgentsLive({ data, reload }: { data: AgentsT; reload: () => Promise<void> }) {
  const now = useNow(60000);
  const [name, setName] = useState('');
  const [access, setAccess] = useState<AgentKey['access']>('read_only');
  const [busy, setBusy] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);
  const [revoke, setRevoke] = useState<AgentKey | null>(null);
  const cfg = secret
    ? `claude mcp add --transport http coyotedb ${data.mcp.url} --header "Authorization: Bearer ${secret}"`
    : '';
  return (
    <>
      <section class="panel" aria-labelledby="new-h">
        <h2 id="new-h" class="section-title">Give an agent access</h2>
        <form
          class="row"
          style={{ alignItems: 'flex-end', gap: '16px' }}
          onSubmit={async (e) => {
            e.preventDefault();
            if (!name.trim()) return;
            setBusy(true);
            try {
              const r = await api.createAgentKey(name.trim(), access);
              setSecret(r.secret);
              setName('');
              await reload();
            } catch (e2) {
              toast(errorMessage(e2), 'bad');
            } finally {
              setBusy(false);
            }
          }}
        >
          <label class="field" style={{ flex: '1 1 240px' }}>
            <span class="small">Name it</span>
            <input class="input" value={name} placeholder="Claude Code on my laptop" onInput={(e) => setName(e.currentTarget.value)} />
          </label>
          <label class="field">
            <span class="small">Access</span>
            <select class="input" value={access} onChange={(e) => setAccess(e.currentTarget.value as AgentKey['access'])}>
              <option value="read_only">Read only</option>
              <option value="read_write">Read and write</option>
            </select>
          </label>
          <button type="submit" class="btn" disabled={busy || !name.trim()}>
            <Busy busy={busy}>Create key</Busy>
          </button>
        </form>
        {secret && (
          <div style={{ marginTop: '16px' }} class="stack">
            <Notice kind="warn" title="Copy it now">
              <p>The key is shown this once. Anyone with it can use the database with the access you chose.</p>
            </Notice>
            <CodeBlock title="Claude Code" code={cfg} what="agent setup command">
              <span class="prompt">$ </span>
              {cfg}
            </CodeBlock>
          </div>
        )}
      </section>
      <section class="section panel" aria-labelledby="keys-h">
        <h2 id="keys-h" class="section-title">Keys</h2>
        {data.keys.length === 0 ? (
          <p class="ink2" style={{ margin: 0 }}>No agent has access yet.</p>
        ) : (
          <ul class="timeline">
            {data.keys.map((k) => (
              <li key={k.id}>
                <span class="t-icon info" />
                <div>
                  <strong>{k.name}</strong>
                  <div class="muted small">
                    {k.access === 'read_only' ? 'Read only' : 'Read and write'} · last used {ago(k.last_used_at ?? null, now)}
                  </div>
                </div>
                <button type="button" class="btn small danger-ghost" onClick={() => setRevoke(k)}>Revoke</button>
              </li>
            ))}
          </ul>
        )}
      </section>
      <ConfirmDialog
        open={!!revoke}
        danger
        title={`Revoke “${revoke?.name ?? ''}”?`}
        confirmLabel="Revoke"
        busy={busy}
        onClose={() => setRevoke(null)}
        onConfirm={async () => {
          if (!revoke) return;
          setBusy(true);
          try {
            await api.revokeAgentKey(revoke.id);
            toast('Key revoked. That agent can no longer connect.');
            setRevoke(null);
            await reload();
          } catch (e) {
            toast(errorMessage(e), 'bad');
          } finally {
            setBusy(false);
          }
        }}
      >
        <p>The agent using this key is disconnected straight away.</p>
      </ConfirmDialog>
    </>
  );
}
