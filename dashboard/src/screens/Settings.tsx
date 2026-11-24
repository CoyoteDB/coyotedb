// Computers & settings: names, what happens on one copy, this computer,
// dashboard access, and the danger zone (remove, leave).
import { useEffect, useState } from 'preact/hooks';
import { api, ApiError, errorMessage } from '../api/client';
import type { AddressProbe, MemberView, NodeInfo, Problem, Settings as SettingsT } from '../api/types';
import * as I from '../components/icons';
import { ProblemBanner } from '../components/ProblemBanner';
import { A, Busy, ConfirmDialog, Notice, toast } from '../components/ui';
import { hasFeature } from '../lib/features';
import { parseGoDuration, platformLabel } from '../lib/format';
import { useStore } from '../lib/store';
import { addressProblem, MAX_ADDRESSES, splitAddresses } from '../lib/addresses';
import { addressPrefill, addressProblemsFor } from '../lib/problems';
import { nameProblem } from '../lib/words';
import { refreshSetup, refreshStatus, store } from '../state';

const WAITS = [
  { v: '10s', label: '10 seconds' },
  { v: '30s', label: '30 seconds' },
  { v: '1m0s', label: '1 minute' },
  { v: '5m0s', label: '5 minutes' },
];

export function Settings() {
  const status = useStore(store, (s) => s.status);
  const version = useStore(store, (s) => s.version);
  const [settings, setSettings] = useState<SettingsT | null>(null);
  const [node, setNode] = useState<NodeInfo | null>(null);
  const [err, setErr] = useState('');
  useEffect(() => {
    api.settings().then(setSettings, (e) => setErr(errorMessage(e)));
    api.node().then(setNode, () => setNode(null));
  }, []);

  return (
    <>
      <div class="page-head">
        <h1>Computers &amp; settings</h1>
        <p>Names, what happens when only one copy is left, and removing computers.</p>
      </div>
      {err && <Notice kind="bad" title="Could not load the settings"><p>{err}</p></Notice>}
      {settings && <General settings={settings} onSaved={setSettings} dataCount={status?.nodes.filter((n) => n.role === 'data').length ?? 1} canName={hasFeature(version, 'cluster_name')} />}
      {status && <Computers nodes={status.nodes} problems={status.problems ?? []} canRename={hasFeature(version, 'rename')} canSetAddress={hasFeature(version, 'set_address')} />}
      {node && <ThisComputer node={node} onChange={setNode} canRemote={hasFeature(version, 'remote_dashboard')} />}
      {status && <DangerZone self={status.nodes.find((n) => n.this)} />}
      {version && (
        <section class="section panel quiet" aria-labelledby="about-h">
          <h2 id="about-h" class="section-title">About</h2>
          <dl class="kv small">
            <dt>CoyoteDB</dt>
            <dd>{version.version} {version.commit ? <span class="muted">({version.commit})</span> : null}</dd>
            <dt>PostgreSQL</dt>
            <dd>{version.postgres_version}</dd>
            <dt>System</dt>
            <dd>{platformLabel({ os: version.os, arch: version.arch })}</dd>
          </dl>
        </section>
      )}
    </>
  );
}

function General({ settings, onSaved, dataCount, canName }: { settings: SettingsT; onSaved: (s: SettingsT) => void; dataCount: number; canName: boolean }) {
  const [name, setName] = useState(settings.cluster_name ?? '');
  const [mode, setMode] = useState(settings.single_data_node_mode);
  const curWait = WAITS.find((w) => parseGoDuration(w.v) === parseGoDuration(settings.degrade_after))?.v ?? settings.degrade_after;
  const [wait, setWait] = useState(curWait);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const dirty = name !== (settings.cluster_name ?? '') || mode !== settings.single_data_node_mode || (mode === 'degrade_after' && wait !== curWait);

  const save = async (e: Event) => {
    e.preventDefault();
    setBusy(true);
    setErr('');
    const set: Record<string, string> = {};
    if (name !== (settings.cluster_name ?? '')) set.cluster_name = name.trim();
    if (mode !== settings.single_data_node_mode) set.single_data_node_mode = mode;
    if (mode === 'degrade_after' && wait !== curWait) set.degrade_after = wait;
    try {
      const s = await api.setSettings(set);
      onSaved(s);
      toast('Settings saved on every computer.');
      void refreshStatus().catch(() => undefined);
    } catch (e2) {
      setErr(e2 instanceof ApiError && e2.code === 'no_leader' ? 'Settings need a majority of your computers to be reachable. Try again when they are back.' : errorMessage(e2));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form class="panel" onSubmit={save} aria-labelledby="gen-h">
      <h2 id="gen-h" class="section-title">Database</h2>
      {canName && (
        <div class="field" style={{ marginTop: '12px' }}>
          <label for="cname">Name</label>
          <input id="cname" class="input" maxLength={64} value={name} placeholder="Your database" onInput={(e) => setName(e.currentTarget.value)} />
          <p class="hint">Shown at the top of the dashboard on every computer.</p>
        </div>
      )}
      <fieldset class="field" style={{ border: 0, padding: 0, margin: canName ? '24px 0 0' : '12px 0 0' }}>
        <legend class="flabel" style={{ fontWeight: 500, marginBottom: '4px' }}>When only one data computer is left</legend>
        <p class="hint" style={{ marginBottom: '10px' }}>
          Normally every write is saved on two computers before it is confirmed. If the copy stops, the main computer has to choose.
          {dataCount < 2 && ' This matters once you have a second data computer.'}
        </p>
        <div class="choices">
          <label class="choice">
            <input type="radio" name="mode" checked={mode === 'degrade_after'} onChange={() => setMode('degrade_after')} />
            <span class="c-title">Keep going <span class="tag">Recommended</span></span>
            <div class="c-body">
              Wait a little for the copy to come back, then carry on alone. Your apps keep working; the dashboard shows “Running on one copy” until the copy is back.
              {mode === 'degrade_after' && (
                <div class="field" style={{ marginTop: '10px' }}>
                  <label class="small" for="wait">Wait for</label>
                  <select id="wait" class="input" value={wait} onChange={(e) => setWait(e.currentTarget.value)} style={{ maxWidth: '220px' }}>
                    {WAITS.map((w) => <option key={w.v} value={w.v}>{w.label}</option>)}
                    {!WAITS.some((w) => w.v === curWait) && <option value={curWait}>{curWait}</option>}
                  </select>
                </div>
              )}
            </div>
          </label>
          <label class="choice">
            <input type="radio" name="mode" checked={mode === 'strict'} onChange={() => setMode('strict')} />
            <span class="c-title">Wait for the copy</span>
            <div class="c-body">Never confirm a write that is on one computer only. Writes stop until the copy is back. Choose this only if losing a recent write is worse than stopping.</div>
          </label>
        </div>
      </fieldset>
      {err && <div style={{ marginTop: '16px' }}><Notice kind="bad" role="alert"><p>{err}</p></Notice></div>}
      <div class="btn-row" style={{ marginTop: '20px' }}>
        <button type="submit" class="btn" disabled={!dirty || busy}>
          <Busy busy={busy}>Save</Busy>
        </button>
      </div>
    </form>
  );
}

function Computers({ nodes, problems, canRename, canSetAddress }: { nodes: MemberView[]; problems: Problem[]; canRename: boolean; canSetAddress: boolean }) {
  const [rename, setRename] = useState<MemberView | null>(null);
  const [addrFor, setAddrFor] = useState<MemberView | null>(null);
  const [addrInitial, setAddrInitial] = useState<string[] | null>(null);
  // A banner's "Use …" button (here or on another page) asks for the
  // Change addresses dialog, prefilled with the suggested addresses.
  const prefill = useStore(addressPrefill, (s) => s.req);
  useEffect(() => {
    if (!prefill) return;
    const m = nodes.find((n) => n.name === prefill.node);
    addressPrefill.set({ req: null });
    if (!m || !canSetAddress) return;
    setAddrInitial(prefill.addresses);
    setAddrFor(m);
    requestAnimationFrame(() => document.getElementById(`comp-${m.id}`)?.scrollIntoView({ block: 'center' }));
  }, [prefill, nodes, canSetAddress]);
  const openAddr = (m: MemberView | null, initial: string[] | null = null) => {
    setAddrInitial(initial);
    setAddrFor(m);
  };
  const [newName, setNewName] = useState('');
  const [remove, setRemove] = useState<MemberView | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const dataCount = nodes.filter((n) => n.role === 'data').length;
  const witnesses = nodes.filter((n) => n.role === 'witness').length;

  const removeConsequences = (m: MemberView): string[] => {
    const out = [`${m.name}’s database stops and it can no longer connect to the others. Its files stay on that computer.`];
    if (m.role === 'data' && dataCount === 2) out.push(`Your data will be on one computer only: if it stops, the database stops.`);
    if (m.role === 'data' && dataCount === 3 && witnesses === 0) out.push('With two data computers left and no witness, switch-over is no longer automatic.');
    if (m.role === 'witness' && dataCount === 2) out.push('Switch-over will no longer happen by itself: you will have to press Switch over if the main computer stops.');
    out.push('To bring it back later, make a new invite code; it copies the data again.');
    return out;
  };

  return (
    <section class="section panel" aria-labelledby="comp-h">
      <div class="panel-head">
        <h2 id="comp-h" class="section-title">Computers</h2>
        <A href="/add" class="linkish">Add a computer</A>
      </div>
      <ul class="timeline">
        {nodes.map((m) => (
          <li key={m.id} id={`comp-${m.id}`}>
            <span class="t-icon info" aria-hidden="true">
              <svg viewBox="0 0 18 18">
                {m.role === 'witness' ? (
                  <circle cx="9" cy="9" r="6" fill="none" stroke="var(--ink-2)" stroke-width="1.4" stroke-dasharray="2.4 2" />
                ) : (
                  <circle cx="9" cy="9" r="7" fill={m.primary ? 'var(--accent)' : 'var(--ink)'} />
                )}
              </svg>
            </span>
            <div>
              <strong>{m.name}</strong>
              {m.this && <span class="muted"> · this computer</span>}
              <div class="muted small">
                {m.primary ? 'Main node' : m.role === 'witness' ? 'Witness' : 'Copy'} · {platformLabel(m.platform)}
              </div>
              {m.addresses.length > 0 && (
                <div class="muted small" style={{ overflowWrap: 'anywhere' }}>
                  Reached at {m.addresses.map((a, i) => <span key={a}>{i ? ', ' : ''}<code>{a}</code></span>)}
                </div>
              )}
              {addressProblemsFor({ problems }, m.name).map((p) => (
                <ProblemBanner key={p.code} p={p} compact />
              ))}
            </div>
            <div class="row" style={{ gap: '8px' }}>
              {canSetAddress && (
                <button type="button" class="btn small ghost" onClick={() => openAddr(m)} aria-label={`Change addresses of ${m.name}`}>Change addresses</button>
              )}
              {canRename && <button type="button" class="btn small ghost" onClick={() => { setErr(''); setNewName(m.name); setRename(m); }}>Rename</button>}
              {!m.this && (
                <button type="button" class="btn small danger-ghost" disabled={m.primary} title={m.primary ? 'Switch over to another computer first' : undefined} onClick={() => { setErr(''); setRemove(m); }}>
                  Remove
                </button>
              )}
            </div>
          </li>
        ))}
      </ul>
      {nodes.some((n) => n.primary && !n.this) && (
        <p class="muted small" style={{ marginBottom: 0 }}>To remove the main node, switch over to another computer first.</p>
      )}

      <AddressDialog member={addrFor} initial={addrInitial} onClose={() => openAddr(null)} />

      <ConfirmDialog
        open={!!rename}
        title={`Rename ${rename?.name ?? ''}`}
        confirmLabel="Rename"
        busy={busy}
        error={err || (newName && newName !== rename?.name ? nameProblem(newName) ?? '' : '')}
        onClose={() => setRename(null)}
        onConfirm={async () => {
          if (!rename || nameProblem(newName) || newName === rename.name) return;
          setBusy(true);
          setErr('');
          try {
            await api.renameMember(rename.name, newName);
            toast(`${rename.name} is now called ${newName}.`);
            setRename(null);
            await refreshStatus().catch(() => undefined);
          } catch (e) {
            setErr(e instanceof ApiError && e.code === 'not_yet_available' ? 'Renaming arrives in a later version.' : e instanceof ApiError && e.code === 'duplicate_name' ? 'Another computer already has that name.' : errorMessage(e));
          } finally {
            setBusy(false);
          }
        }}
      >
        <div class="field">
          <label for="rn">New name</label>
          <input id="rn" class="input mono" value={newName} maxLength={48} autoCapitalize="off" spellcheck={false} onInput={(e) => setNewName(e.currentTarget.value)} />
          <p class="hint">Lowercase letters, digits and dashes.</p>
        </div>
      </ConfirmDialog>

      <ConfirmDialog
        open={!!remove}
        danger
        title={`Remove ${remove?.name ?? ''} from the cluster?`}
        confirmLabel={`Remove ${remove?.name ?? ''}`}
        typeToConfirm={remove?.name}
        busy={busy}
        error={err}
        onClose={() => setRemove(null)}
        onConfirm={async () => {
          if (!remove) return;
          setBusy(true);
          setErr('');
          try {
            const r = await api.removeMember(remove.name);
            toast(r.message);
            setRemove(null);
            await refreshStatus().catch(() => undefined);
          } catch (e) {
            setErr(errorMessage(e));
          } finally {
            setBusy(false);
          }
        }}
      >
        <ul class="consequences">{remove && removeConsequences(remove).map((c) => <li key={c}>{c}</li>)}</ul>
      </ConfirmDialog>
    </section>
  );
}

function ThisComputer({ node, onChange, canRemote }: { node: NodeInfo; onChange: (n: NodeInfo) => void; canRemote: boolean }) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  return (
    <section class="section panel" aria-labelledby="this-h">
      <h2 id="this-h" class="section-title">This computer</h2>
      <dl class="kv small" style={{ marginTop: '12px' }}>
        <dt>Name</dt>
        <dd>{node.name}</dd>
        <dt>Role</dt>
        <dd>{node.role === 'witness' ? 'Witness' : 'Data computer'}</dd>
        <dt>System</dt>
        <dd>{platformLabel(node.platform)}</dd>
        <dt>Data folder</dt>
        <dd><code>{node.data_dir}</code></dd>
        <dt>Other computers reach it at</dt>
        <dd>{node.advertise.map((a) => <code key={a} style={{ marginRight: '6px' }}>{a}</code>)}</dd>
        <dt>Apps connect on port</dt>
        <dd>{node.ports.postgres}</dd>
        <dt>Runs in the background</dt>
        <dd>{node.service ? 'Yes, starts with the computer' : 'No, only while started by hand'}</dd>
        {!canRemote && (
          <>
            <dt>Dashboard</dt>
            <dd>Opens on this computer only</dd>
          </>
        )}
      </dl>
      {canRemote && (<>
      <hr class="divider" />
      <div class="switch-row">
        <div>
          <div style={{ fontWeight: 500 }} id="remote-l">Open this dashboard from other devices</div>
          <p class="hint" style={{ margin: '4px 0 0', color: 'var(--muted)' }} id="remote-d">
            For checking on your phone over your VPN or home network. Signing in still needs a one-time link: run <code>coyote dashboard --host {node.advertise[0]?.replace(/:\d+$/, '') ?? 'ADDRESS'}</code>. Never offered on a public internet address.
          </p>
          {node.dashboard.remote && node.dashboard.urls.length > 1 && (
            <p class="small" style={{ margin: '8px 0 0' }}>Now open at {node.dashboard.urls.filter((u) => !u.includes('127.0.0.1')).map((u) => <code key={u} style={{ marginRight: '6px' }}>{u}</code>)}</p>
          )}
          {err && <p class="small" style={{ color: 'var(--down)', margin: '8px 0 0' }} role="alert">{err}</p>}
        </div>
        <span class="toggle">
          <input type="checkbox" role="switch" aria-labelledby="remote-l" aria-describedby="remote-d" checked={node.dashboard.remote} disabled={busy}
            onChange={async (e) => {
              const on = e.currentTarget.checked;
              setBusy(true);
              setErr('');
              try {
                onChange(await api.setNode({ dashboard_remote: on }));
                toast(on ? 'The dashboard can now be opened from your other devices.' : 'The dashboard opens on this computer only.');
              } catch (e2) {
                setErr(errorMessage(e2));
              } finally {
                setBusy(false);
              }
            }} />
          <span />
        </span>
      </div>
      </>)}
    </section>
  );
}

// ---- change a computer's addresses (POST /v1/members/{node}/addresses) ----

function AddressDialog({ member, initial, onClose }: { member: MemberView | null; initial?: string[] | null; onClose: () => void }) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const [probes, setProbes] = useState<AddressProbe[] | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    if (!member) return;
    setText((initial?.length ? initial : member.addresses).join('\n'));
    setErr('');
    setProbes(null);
    setFailed(false);
  }, [member, initial]);

  const list = splitAddresses(text);
  const problem =
    list.length === 0 ? 'Give at least one address.' : list.length > MAX_ADDRESSES ? `Give at most ${MAX_ADDRESSES} addresses.` : list.map(addressProblem).find((p) => p) ?? '';
  const name = member?.name ?? '';

  const save = async (force: boolean) => {
    if (!member || problem) return;
    setBusy(true);
    setErr('');
    try {
      const r = await api.setAddresses(member.name, list, force);
      setProbes(r.probes);
      if (r.recorded) {
        toast(r.message || `Saved the addresses of ${member.name}.`);
        onClose();
        await refreshStatus().catch(() => undefined);
      } else {
        setFailed(true);
      }
    } catch (e) {
      setErr(
        e instanceof ApiError && (e.code === 'no_leader' || e.code === 'not_leader')
          ? 'Saving addresses needs a majority of your computers to be reachable. Try again when they are back.'
          : errorMessage(e),
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <ConfirmDialog
      open={!!member}
      title={`Addresses of ${name}`}
      confirmLabel={failed ? 'Save anyway' : 'Check and save'}
      busy={busy}
      confirmDisabled={!!problem}
      error={err}
      onClose={onClose}
      onConfirm={() => void save(failed)}
    >
      <p class="ink2">
        The addresses the other computers use to reach {name}. Each one is checked first: this computer connects to it and makes sure {name} answers there. Nothing changes
        unless one does.
      </p>
      <div class="field">
        <label for="addrs">Addresses (HOST:PORT)</label>
        <textarea id="addrs" class="textarea mono" rows={3} value={text} autoCapitalize="off" spellcheck={false} aria-invalid={problem && text ? 'true' : undefined} aria-describedby="addrs-h"
          onInput={(e) => { setText(e.currentTarget.value); setFailed(false); setProbes(null); }} />
        <p class="hint" id="addrs-h">One to {MAX_ADDRESSES}, separated by commas or new lines. The port is the one the computers talk to each other on (often 7481).</p>
        {problem && text && <p class="error" role="alert">{problem}</p>}
      </div>
      {probes && probes.length > 0 && (
        <ul class="probe-list" aria-label="Check results">
          {probes.map((p) => (
            <li key={p.addr} class={p.ok ? 'ok' : 'bad'}>
              {p.ok ? <I.CheckCircle /> : <I.CrossCircle />}
              <span>
                <code>{p.addr}</code> {p.ok ? <>answered as {name}{p.rtt_ms != null ? ` · ${p.rtt_ms < 1 ? '<1' : Math.round(p.rtt_ms)} ms` : ''}</> : <>did not answer as {name}{p.error ? <span class="muted"> ({p.error})</span> : null}</>}
              </span>
            </li>
          ))}
        </ul>
      )}
      {failed && (
        <div style={{ marginTop: '16px' }}>
          <Notice kind="warn" title="Nothing was changed">
            <p>None of these addresses answered as {name} from this computer. Check them for typos first.</p>
            <p>
              Press <strong>Save anyway</strong> only if {name} is switched off or offline right now, or if this computer cannot reach it but the others can. The others then
              use these addresses when {name} is back.
            </p>
          </Notice>
        </div>
      )}
    </ConfirmDialog>
  );
}

function DangerZone({ self }: { self?: MemberView }) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  if (!self) return null;
  const isMain = self.primary;
  return (
    <section class="section" aria-labelledby="dz-h" style={{ marginTop: '32px' }}>
      <h2 id="dz-h" class="section-title" style={{ marginBottom: '12px' }}>Danger zone</h2>
      <div class="danger-zone">
        <div class="dz-row">
          <div class="dz-text">
            <strong>Leave the cluster</strong>
            <p>
              {isMain
                ? 'This computer is the main node. Switch over to another computer first, then it can leave.'
                : 'This computer stops its database and leaves. Its files stay here; the others carry on without it.'}
            </p>
          </div>
          <button type="button" class="btn danger-ghost" disabled={isMain} onClick={() => { setErr(''); setOpen(true); }}>
            Leave…
          </button>
        </div>
      </div>
      <ConfirmDialog
        open={open}
        danger
        title={`Take ${self.name} out of the cluster?`}
        confirmLabel="Leave the cluster"
        typeToConfirm={self.name}
        busy={busy}
        error={err}
        onClose={() => setOpen(false)}
        onConfirm={async () => {
          setBusy(true);
          setErr('');
          try {
            await api.leave();
            setOpen(false);
            await refreshSetup().catch(() => undefined);
          } catch (e) {
            setErr(errorMessage(e));
          } finally {
            setBusy(false);
          }
        }}
      >
        <ul class="consequences">
          <li>The database on this computer stops. Apps connected through it are disconnected.</li>
          <li>The other computers carry on, with one computer fewer.</li>
          <li>To come back, it needs a new invite code and copies the data again.</li>
        </ul>
        <p class="muted small" style={{ margin: 0 }}><I.Info style={{ width: '14px', height: '14px', verticalAlign: '-2px' }} /> Its files are not deleted.</p>
      </ConfirmDialog>
    </section>
  );
}
