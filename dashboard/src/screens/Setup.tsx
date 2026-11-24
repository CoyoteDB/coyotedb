// First run: start a new database, or join one with an invite code.
// Steps: choose → (paste code) → name → (backups, new database) → address →
// live progress → success.
// Before this computer is in a cluster the agent serves only the setup
// routes (API.md §4), so nothing here asks for the status until it is.
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { api, ApiError, errorCode, errorMessage } from '../api/client';
import type { Address, Addresses, ApiErrorBody, InviteCheck, InviteResponse, Platform, Role, Setup as SetupT, Step } from '../api/types';
import * as I from '../components/icons';
import { Busy, CodeBlock, CopyButton, Eyebrow, Logo, Notice, useNow } from '../components/ui';
import { hasFeature } from '../lib/features';
import { clock, platformLabel, until } from '../lib/format';
import { decodeInvite, inviteProblemText, type InviteResult } from '../lib/invite';
import { explainError, failActions, inviteAddrs, retryStage, type FailAction, type Path } from '../lib/setupFail';
import { useStore } from '../lib/store';
import { ThemeToggle } from '../lib/theme';
import { nameProblem, sanitizeName, stepDetail, stepLabel } from '../lib/words';
import { navigate } from '../lib/router';
import { applySetup, refreshSetup, refreshStatus, store } from '../state';
import { EMPTY_DRAFT, type DestDraft } from '../lib/backups';
import { BackupApplied, BackupStep } from './SetupBackup';

type Stage = 'choose' | 'invite' | 'name' | 'backup' | 'address' | 'progress';

// The backup choice outlives a remount of the wizard (it is applied on the
// success screen), but not a reload: then the agent's default stays.
let chosenBackup: DestDraft | null = null;

export function Setup() {
  const setup = useStore(store, (s) => s.setup)!;
  const version = useStore(store, (s) => s.version);
  const canNameCluster = hasFeature(version, 'cluster_name');
  const canBackup = hasFeature(version, 'backups');
  const [backupDraft, setBackupDraft] = useState<DestDraft>(chosenBackup ?? EMPTY_DRAFT);
  const busy = setup.state === 'creating' || setup.state === 'joining';
  const failedJoin = setup.state === 'failed' && (setup.progress?.operation === 'join' || setup.error?.code === 'unfinished_join');
  const [path, setPath] = useState<Path | null>(setup.progress?.operation ?? (failedJoin ? 'join' : null));
  const [stage, setStage] = useState<Stage>(busy || setup.state === 'member' || setup.state === 'failed' ? 'progress' : 'choose');
  const [code, setCode] = useState('');
  const [role, setRole] = useState<Role>(setup.progress?.role ?? 'data');
  const [check, setCheck] = useState<InviteCheck | null>(null);
  const [name, setName] = useState(setup.suggested_name || 'node');
  const [clusterName, setClusterName] = useState('');
  const [addr, setAddr] = useState('');
  const [allowMixed, setAllowMixed] = useState(false);
  const [startErr, setStartErr] = useState<ApiErrorBody | null>(null);
  const [starting, setStarting] = useState(false);

  useEffect(() => {
    store.set({ wizard: true });
  }, []);
  useEffect(() => {
    if (busy || setup.state === 'failed') setStage('progress');
  }, [busy, setup.state]);

  const order: Stage[] =
    path === 'join' ? ['choose', 'invite', 'name', 'address', 'progress'] : canBackup ? ['choose', 'name', 'backup', 'address', 'progress'] : ['choose', 'name', 'address', 'progress'];
  const idx = order.indexOf(stage);
  const back = () => setStage(order[Math.max(0, idx - 1)]!);
  const next = () => setStage(order[Math.min(order.length - 1, idx + 1)]!);

  const start = async (advertise: string, o: { role?: Role; allowMixed?: boolean; path?: Path } = {}) => {
    const r = o.role ?? role;
    const pth = o.path ?? path;
    const mixed = r === 'data' && (o.allowMixed ?? allowMixed);
    setAddr(advertise);
    setStarting(true);
    setStartErr(null);
    try {
      const s =
        pth === 'join'
          ? await api.join({ code, name, advertise, role: r, ...(mixed ? { allow_mixed_os: true } : {}) })
          : await api.create({ name, advertise, ...(canNameCluster && clusterName.trim() ? { cluster_name: clusterName.trim() } : {}) });
      chosenBackup = pth === 'create' && canBackup ? backupDraft : null;
      applySetup(s);
      setStage('progress');
    } catch (e) {
      setStartErr(e instanceof ApiError ? { error: e.message, code: e.code, details: e.details } : { error: errorMessage(e), code: errorCode(e) });
      // A request sent from the failure page (no form on screen) shows its error on the form.
      setStage('address');
    } finally {
      setStarting(false);
    }
  };

  const startOver = async () => {
    try {
      applySetup(await api.resetSetup());
    } catch {
      // Not in "failed" any more (another tab, or it already started over): show what it is now.
      await refreshSetup().catch(() => undefined);
    }
    setPath(null);
    setCode('');
    setCheck(null);
    setRole('data');
    setAllowMixed(false);
    setName(setup.suggested_name || 'node');
    setClusterName('');
    setAddr('');
    setStartErr(null);
    chosenBackup = null;
    setBackupDraft(EMPTY_DRAFT);
    setStage('choose');
  };

  const onFail = async (a: FailAction) => {
    const p: Path = path ?? (failedJoin ? 'join' : 'create');
    if (!path) setPath(p);
    setStartErr(null);
    if (a === 'reset') return startOver();
    if (a === 'mixed' || a === 'witness') {
      const o = a === 'mixed' ? { allowMixed: true, role: 'data' as Role } : { role: 'witness' as Role, allowMixed: false };
      setRole(o.role);
      setAllowMixed(o.allowMixed);
      // The code is only kept in this page; after a reload it has to be pasted again.
      if (code) return start(addr, { ...o, path: p });
      return setStage('invite');
    }
    setStage(retryStage(setup.error?.code ?? '', p, !!code));
  };

  return (
    <div class="setup-shell">
      <header class="setup-top">
        <Logo />
        <span class="spacer" />
        <ThemeToggle />
      </header>
      <main class="setup-main" id="main">
        <div class="wizard">
          {stage !== 'progress' && stage !== 'choose' && (
            <ol class="wizard-steps" aria-label={`Step ${idx} of ${order.length - 1}`}>
              {order.slice(1).map((s, i) => (
                <li key={s} class={i + 1 < idx ? 'on' : i + 1 === idx ? 'now' : ''} />
              ))}
            </ol>
          )}
          {stage === 'choose' && (
            <Choose
              onPick={(p) => {
                setPath(p);
                setStage(p === 'join' ? 'invite' : 'name');
              }}
            />
          )}
          {stage === 'invite' && (
            <InviteStep
              code={code}
              setCode={setCode}
              role={role}
              setRole={(r) => {
                setRole(r);
                if (r === 'witness') setAllowMixed(false);
              }}
              allowMixed={allowMixed}
              platform={setup.platform}
              onChecked={setCheck}
              onBack={back}
              onNext={next}
            />
          )}
          {stage === 'name' && (
            <NameStep
              path={path!}
              role={role}
              name={name}
              setName={setName}
              canNameCluster={canNameCluster}
              clusterName={clusterName}
              setClusterName={setClusterName}
              clusterFromInvite={check?.probe?.cluster_name}
              onBack={back}
              onNext={next}
            />
          )}
          {stage === 'backup' && <BackupStep draft={backupDraft} setDraft={setBackupDraft} onBack={back} onNext={next} />}
          {stage === 'address' && (
            <AddressStep
              addr={addr}
              setAddr={setAddr}
              onBack={back}
              path={path!}
              name={name}
              role={role}
              allowMixed={allowMixed}
              starting={starting}
              error={startErr}
              code={code}
              backupsNote={canBackup}
              onStart={(a) => void start(a)}
            />
          )}
          {stage === 'progress' && <ProgressStep setup={setup} code={code} busy={starting} onAction={onFail} backups={canBackup} />}
        </div>
      </main>
      <footer class="setup-foot">
        CoyoteDB runs real PostgreSQL on your own computers. Nothing here talks to the internet except the PostgreSQL download, and the cloud storage you pick for backups.
      </footer>
    </div>
  );
}

function Choose({ onPick }: { onPick: (p: Path) => void }) {
  return (
    <>
      <Eyebrow>Welcome</Eyebrow>
      <h1>Let’s set up this computer</h1>
      <p class="lede" style={{ marginBottom: '32px' }}>
        Start a new database here, or add this computer to one you already run on another computer.
      </p>
      <div class="big-choice">
        <button type="button" onClick={() => onPick('create')}>
          <svg class="bc-art" viewBox="0 0 120 48" aria-hidden="true">
            <path d="M8 44H112" stroke="var(--line-strong)" />
            <path d="M44 28 A20 20 0 0 1 76 28" fill="none" stroke="var(--accent)" stroke-width="1.6" stroke-linecap="round" opacity="0.5" />
            <path d="M49.5 32 A12 12 0 0 1 70.5 32" fill="none" stroke="var(--accent)" stroke-width="1.6" stroke-linecap="round" />
            <circle cx="60" cy="38" r="6" fill="var(--accent)" />
          </svg>
          <h2>Start a new database</h2>
          <p>This computer becomes the main node. You can add more computers afterwards.</p>
          <span class="go">Start new →</span>
        </button>
        <button type="button" onClick={() => onPick('join')}>
          <svg class="bc-art" viewBox="0 0 120 48" aria-hidden="true">
            <path d="M8 44H112" stroke="var(--line-strong)" />
            <path d="M40 32 Q60 14 80 32" fill="none" stroke="var(--ink-2)" stroke-width="1.25" stroke-dasharray="2 4" />
            <circle cx="36" cy="38" r="6" fill="var(--accent)" />
            <circle cx="84" cy="38" r="6" fill="none" stroke="var(--ink)" stroke-width="1.5" />
          </svg>
          <h2>Join with an invite code</h2>
          <p>Add this computer to a database you already run. You need an invite code from it.</p>
          <span class="go">Join →</span>
        </button>
      </div>
      <p class="muted small" style={{ marginTop: '24px' }}>
        For the computers that hold the data — recommended: Linux. macOS and Windows work too. All data computers in one database run the same system; a witness can run any.
      </p>
    </>
  );
}

function Nav({ onBack, children }: { onBack?: () => void; children: preact.ComponentChildren }) {
  return (
    <div class="wizard-nav">
      {onBack && (
        <button type="button" class="btn ghost back" onClick={onBack}>
          Back
        </button>
      )}
      {children}
    </div>
  );
}

function InviteStep(p: {
  code: string;
  setCode: (s: string) => void;
  role: Role;
  setRole: (r: Role) => void;
  allowMixed: boolean;
  platform: Platform;
  onChecked: (c: InviteCheck | null) => void;
  onBack: () => void;
  onNext: () => void;
}) {
  const now = useNow(15000);
  const local: InviteResult = useMemo(() => decodeInvite(p.code, now), [p.code, now]);
  const [check, setCheck] = useState<InviteCheck | null>(null);
  const [checking, setChecking] = useState(false);
  const [serverErr, setServerErr] = useState('');
  const [again, setAgain] = useState(0);
  const seq = useRef(0);

  useEffect(() => {
    setCheck(null);
    setServerErr('');
    p.onChecked(null);
    if (!local.ok || local.expired) return;
    const n = ++seq.current;
    setChecking(true);
    const t = setTimeout(() => {
      api.checkInvite(local.normalized).then(
        (c) => {
          if (n !== seq.current) return;
          setCheck(c);
          p.onChecked(c);
          setChecking(false);
        },
        (e) => {
          if (n !== seq.current) return;
          setServerErr(e instanceof ApiError ? explainError({ error: e.message, code: e.code }) : errorMessage(e));
          setChecking(false);
        },
      );
    }, 250);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [local.ok ? local.normalized : '', local.ok && local.expired, again]);

  const problem = !local.ok ? inviteProblemText(local.problem) : local.expired ? inviteProblemText('expired') : serverErr;
  const probe = check?.probe;
  const wrongCluster = !!probe?.wrong_cluster;
  const unreachable = !!probe && !probe.reachable && !wrongCluster;
  const answered = !!probe?.reachable && probe.cluster_verified !== false;
  // Only an agent that can learn the cluster's system sends these; today's does not.
  const mismatch = p.role === 'data' && !!probe?.platform && probe.compatible === false;
  const canGo = local.ok && !local.expired && !serverErr && !!check && !checking && !mismatch && !wrongCluster;
  const recheck = (
    <button type="button" class="btn small ghost" onClick={() => setAgain((x) => x + 1)} disabled={checking}>
      <Busy busy={checking}>Check again</Busy>
    </button>
  );

  return (
    <>
      <Eyebrow>Join a cluster</Eyebrow>
      <h1>Paste the invite code</h1>
      <p class="lede">
        On a computer that is already part of your database, open its dashboard and choose <strong>Add a computer</strong>, or run{' '}
        <code>coyote invite</code>. Copy the code it shows and paste it here.
      </p>
      <div class="field" style={{ marginTop: '24px' }}>
        <label for="invite">Invite code</label>
        <textarea
          id="invite"
          class="textarea"
          rows={4}
          value={p.code}
          onInput={(e) => p.setCode(e.currentTarget.value)}
          placeholder="coyote-…"
          spellcheck={false}
          autoCapitalize="off"
          autoComplete="off"
          aria-invalid={problem || wrongCluster ? 'true' : undefined}
          aria-describedby="invite-msg"
        />
        <div id="invite-msg" aria-live="polite">
          {!p.code.trim() ? (
            <p class="hint">Spaces, line breaks and quotes around the code are fine — they are ignored.</p>
          ) : problem ? (
            <p class="error">
              <I.Warn />
              {problem}
            </p>
          ) : checking || !check ? (
            <p class="hint">
              <I.Spinner class="spin" style={{ width: '14px', height: '14px', verticalAlign: '-2px' }} /> Checking the code and contacting the cluster…
            </p>
          ) : (
            <>
              <p class="ok">
                <I.CheckCircle />
                <span>
                  Valid invite{probe?.cluster_name ? <> for <strong>{probe.cluster_name}</strong></> : ''}, expires {until(check.expires, now)} ({clock(check.expires, now)}).
                </span>
              </p>
              {answered && (
                <p class="ok">
                  <I.CheckCircle />
                  <span>{probe?.via ? <>The cluster answered at <code>{probe.via}</code>.</> : 'The cluster answered.'} It is the cluster this code was made for.</span>
                </p>
              )}
            </>
          )}
        </div>
      </div>

      {check && wrongCluster && (
        <div style={{ marginTop: '16px' }}>
          <Notice kind="bad" title="That is not the cluster this code was made for" role="alert">
            <p>
              {probe?.message ||
                'A computer answered, but it is not the cluster this code was made for, so nothing was sent to it. Check that the code is the newest one, or make a new invite.'}
            </p>
            <p>On a computer in your cluster, make a new invite (Add a computer in its dashboard, or run <code>coyote invite</code>) and paste that code here.</p>
            {recheck}
          </Notice>
        </div>
      )}

      {check && unreachable && (
        <div style={{ marginTop: '16px' }}>
          <Notice kind="warn" title="Cannot reach the cluster from here yet">
            <p>
              {probe?.message ||
                <>
                  This computer tried {probe.tried?.length ? probe.tried.map((a, i) => <span key={a}>{i ? ', ' : ''}<code>{a}</code></span>) : 'its addresses'} and got no answer. Check that
                  the computer that made the code is running and that both are on the same network or VPN (for example Tailscale).
                </>}
            </p>
            <p>Fix that, then check again. You can also continue, for example if the other computer is starting up right now.</p>
            {recheck}
          </Notice>
        </div>
      )}

      {check && !wrongCluster && (
        <fieldset class="field" style={{ border: 0, padding: 0, margin: '28px 0 0' }}>
          <legend class="flabel" style={{ fontWeight: 500, marginBottom: '8px' }}>What will this computer be?</legend>
          <div class="choices two">
            <label class="choice">
              <input type="radio" name="role" checked={p.role === 'data'} onChange={() => p.setRole('data')} />
              <span class="c-title">A data computer</span>
              <div class="c-body">
                Holds a full, live copy of the database and can take over as the main node.
                <p class="muted small">
                  {probe?.platform
                    ? `Must run ${platformLabel(probe.platform)}, like the cluster’s other data computers.`
                    : 'Best on the same system as the cluster’s other data computers. If it is not, the cluster says so when this computer joins.'}
                </p>
              </div>
            </label>
            <label class="choice">
              <input type="radio" name="role" checked={p.role === 'witness'} onChange={() => p.setRole('witness')} />
              <span class="c-title">
                A witness <span class="tag neutral">Any system</span>
              </span>
              <div class="c-body">Holds no data. It only helps the others agree, so switch-over can happen by itself. A small always-on computer is ideal.</div>
            </label>
          </div>
        </fieldset>
      )}

      {check && p.allowMixed && p.role === 'data' && (
        <div style={{ marginTop: '16px' }}>
          <Notice kind="info" title="The cluster will check this computer first">
            <p>This computer is a different kind than the cluster’s data computers. When it joins, the cluster checks that it stores data exactly like them, and refuses it if not.</p>
          </Notice>
        </div>
      )}

      {mismatch && probe?.platform && (
        <div style={{ marginTop: '16px' }}>
          <Notice kind="bad" title="This computer can’t hold a copy of this database" role="alert">
            <p>
              The cluster’s data computers run <strong>{platformLabel(probe.platform)}</strong>. This computer runs <strong>{platformLabel(p.platform)}</strong>. PostgreSQL’s files only
              work between identical systems, so mixing them could corrupt data.
            </p>
            <p>
              It can still join as a <strong>witness</strong>, which holds no data.{' '}
              <button type="button" class="linkish" onClick={() => p.setRole('witness')}>
                Join as a witness instead
              </button>
            </p>
          </Notice>
        </div>
      )}

      <Nav onBack={p.onBack}>
        <button type="button" class="btn" disabled={!canGo} onClick={p.onNext}>
          Continue
        </button>
      </Nav>
    </>
  );
}

function NameStep(p: {
  path: Path;
  role: Role;
  name: string;
  setName: (s: string) => void;
  canNameCluster: boolean;
  clusterName: string;
  setClusterName: (s: string) => void;
  clusterFromInvite?: string;
  onBack: () => void;
  onNext: () => void;
}) {
  const [touched, setTouched] = useState(false);
  const problem = nameProblem(p.name);
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        setTouched(true);
        if (!problem) p.onNext();
      }}
    >
      <Eyebrow>{p.path === 'create' ? 'New database' : p.clusterFromInvite ? `Joining ${p.clusterFromInvite}` : 'Join a cluster'}</Eyebrow>
      <h1>Name this computer</h1>
      <p class="lede">The name shows in the dashboard and in messages like “kitchen-pc stopped responding”. Pick something you will recognise.</p>
      <div class="field" style={{ marginTop: '24px' }}>
        <label for="nodename">Computer name</label>
        <input
          id="nodename"
          class="input mono"
          value={p.name}
          maxLength={48}
          autoComplete="off"
          autoCapitalize="off"
          spellcheck={false}
          aria-invalid={touched && problem ? 'true' : undefined}
          aria-describedby="nodename-msg"
          onInput={(e) => p.setName(e.currentTarget.value)}
          onBlur={() => {
            setTouched(true);
            if (nameProblem(p.name)) p.setName(sanitizeName(p.name));
          }}
        />
        <div id="nodename-msg" aria-live="polite">
          {touched && problem ? (
            <p class="error">
              <I.Warn />
              {problem}
            </p>
          ) : (
            <p class="hint">Lowercase letters, digits and dashes, like “office-mini” or “node-b”.</p>
          )}
        </div>
      </div>
      {p.path === 'create' && p.canNameCluster && (
        <div class="field">
          <label for="clustername">
            Name for the database <span class="muted">(optional)</span>
          </label>
          <input id="clustername" class="input" value={p.clusterName} maxLength={64} placeholder="For example: Bakery orders" onInput={(e) => p.setClusterName(e.currentTarget.value)} />
          <p class="hint">Shown at the top of every dashboard. You can change it later.</p>
        </div>
      )}
      <Nav onBack={p.onBack}>
        <button type="submit" class="btn">
          Continue
        </button>
      </Nav>
    </form>
  );
}

function AddressChoice({ a, checked, onPick, recommended }: { a: Address; checked: boolean; onPick: () => void; recommended: boolean }) {
  return (
    <label class="choice">
      <input type="radio" name="addr" checked={checked} onChange={onPick} />
      <span class="c-title">
        {a.label}
        {recommended && <span class="tag">Recommended</span>}
        {a.wireless && <span class="tag neutral">Wi-Fi</span>}
      </span>
      <div class="c-body">
        <span class="muted small">
          Network: {a.interface} · <code>{a.addr}</code>
        </span>
        {a.warnings?.map((w) => (
          <div class="warn-line" key={w}>
            <I.Warn />
            <span>{w}</span>
          </div>
        ))}
      </div>
    </label>
  );
}

function AddressStep(p: {
  addr: string;
  setAddr: (s: string) => void;
  onBack: () => void;
  path: Path;
  name: string;
  role: Role;
  allowMixed: boolean;
  starting: boolean;
  error: ApiErrorBody | null;
  code: string;
  backupsNote?: boolean;
  onStart: (advertise: string) => void;
}) {
  const [data, setData] = useState<Addresses | null>(null);
  const [err, setErr] = useState('');
  const [custom, setCustom] = useState(false);
  const [customVal, setCustomVal] = useState('');
  const load = () => {
    setErr('');
    api.addresses().then(
      (d) => {
        setData(d);
        if (!p.addr) {
          const def = d.default ?? d.addresses.find((a) => a.suggested)?.addr ?? '';
          if (def) p.setAddr(def);
          else setCustom(true);
        } else if (!d.addresses.some((a) => a.addr === p.addr)) {
          // Back from a failure with an address that was typed in.
          setCustom(true);
          setCustomVal(p.addr);
        }
      },
      (e) => setErr(errorMessage(e)),
    );
  };
  // Load once when the step opens; a later name or address change must not reload.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(load, []);
  const suggested = data?.addresses.filter((a) => a.suggested) ?? [];
  const other = data?.addresses.filter((a) => !a.suggested) ?? [];
  const pickedOther = other.some((a) => a.addr === p.addr);
  const value = custom ? customVal.trim() : p.addr;
  const customProblem = custom && customVal.trim() && !/^[A-Za-z0-9.:_-]+$/.test(customVal.trim()) ? 'Type an IP address or a host name, without a port.' : '';
  const ready = !!value && !customProblem && !p.starting;

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        if (!ready) return;
        p.onStart(value);
      }}
    >
      <Eyebrow>{p.path === 'create' ? 'New database' : 'Join a cluster'}</Eyebrow>
      <h1>How will the other computers reach this one?</h1>
      <p class="lede">
        Pick the address your other computers will use. If they are all on Tailscale, pick Tailscale: it keeps working when you move between networks.
      </p>
      {err && (
        <Notice kind="bad" title="Could not list this computer’s addresses">
          <p>{err}</p>
          <button type="button" class="btn small ghost" onClick={load}>
            Try again
          </button>
        </Notice>
      )}
      {!data && !err && <div class="skeleton" style={{ height: '140px', marginTop: '24px' }} aria-label="Finding addresses" />}
      {data && (
        <div style={{ marginTop: '24px' }}>
          {data.warnings?.map((w) => (
            <div key={w} style={{ marginBottom: '12px' }}>
              <Notice kind="warn">
                <p>{w}</p>
              </Notice>
            </div>
          ))}
          <fieldset style={{ border: 0, padding: 0, margin: 0 }}>
            <legend class="visually-hidden">Address</legend>
            <div class="choices">
              {suggested.map((a) => (
                <AddressChoice key={a.addr} a={a} recommended={a.addr === (data.default ?? suggested[0]?.addr)} checked={!custom && p.addr === a.addr} onPick={() => { setCustom(false); p.setAddr(a.addr); }} />
              ))}
              {(other.length > 0 || pickedOther) && (
                <details open={pickedOther}>
                  <summary class="linkish" style={{ listStyle: 'none' }}>
                    Other addresses ({other.length}) — not recommended
                  </summary>
                  <div class="choices" style={{ marginTop: '8px' }}>
                    {other.map((a) => (
                      <AddressChoice key={a.addr} a={a} recommended={false} checked={!custom && p.addr === a.addr} onPick={() => { setCustom(false); p.setAddr(a.addr); }} />
                    ))}
                  </div>
                </details>
              )}
              <label class="choice">
                <input type="radio" name="addr" checked={custom} onChange={() => setCustom(true)} />
                <span class="c-title">Type an address or host name</span>
                <div class="c-body">
                  For a name your network knows, like <code>office-mini.local</code> or a Tailscale name.
                  {custom && (
                    <div class="field" style={{ marginTop: '10px' }}>
                      <label class="visually-hidden" for="custom-addr">Address or host name</label>
                      <input id="custom-addr" class="input mono" value={customVal} onInput={(e) => setCustomVal(e.currentTarget.value)} placeholder="office-mini.local" autoCapitalize="off" spellcheck={false} aria-invalid={customProblem ? 'true' : undefined} />
                      {customProblem && <p class="error"><I.Warn />{customProblem}</p>}
                    </div>
                  )}
                </div>
              </label>
            </div>
          </fieldset>
        </div>
      )}
      {p.backupsNote && p.path === 'join' && (
        <p class="muted small" style={{ display: 'flex', gap: '8px', margin: '20px 0 0' }}>
          <I.Shield style={{ width: '16px', height: '16px', flex: 'none', marginTop: '3px' }} />
          <span>
            {p.role === 'witness'
              ? 'Backups: a witness holds no data, so it keeps no backups.'
              : 'Backups follow the cluster’s settings: nothing to choose here. This computer starts saving its changes as soon as it has its copy.'}
          </span>
        </p>
      )}
      {p.error && (
        <div style={{ marginTop: '20px' }}>
          <Notice kind="bad" title="That did not start" role="alert">
            <p>{explainError(p.error, inviteAddrs(p.code))}</p>
          </Notice>
        </div>
      )}
      <Nav onBack={p.starting ? undefined : p.onBack}>
        <button type="submit" class="btn accent" disabled={!ready}>
          <Busy busy={p.starting}>{p.path === 'create' ? 'Start the database' : p.role === 'witness' ? 'Join as a witness' : p.allowMixed ? 'Join (the cluster checks this computer first)' : 'Join and copy the data'}</Busy>
        </button>
      </Nav>
    </form>
  );
}

function StepIcon({ s }: { s: Step }) {
  switch (s.state) {
    case 'done':
      return <I.CheckCircle />;
    case 'skipped':
      return <I.Check />;
    case 'active':
      return <I.Spinner />;
    case 'failed':
      return <I.CrossCircle />;
    default:
      return <I.Pending />;
  }
}

function ProgressStep({ setup, code, busy, onAction, backups }: { setup: SetupT; code: string; busy: boolean; onAction: (a: FailAction) => Promise<void>; backups: boolean }) {
  const pr = setup.progress;
  const role = pr?.role ?? setup.role ?? 'data';
  const done = setup.state === 'member';
  const failed = setup.state === 'failed';
  const op = pr?.operation ?? (setup.error?.code === 'unfinished_join' ? 'join' : 'create');
  const active = pr?.steps.find((s) => s.state === 'active');
  const [pending, setPending] = useState<FailAction | null>(null);
  const err = setup.error;
  const extra = failed && err ? failActions(err.code, op, role) : [];
  const explained = err ? explainError(err, inviteAddrs(code)) : '';
  // Show the agent's own words too when the page says something else (they name the exact cause).
  const said = err && err.error.trim() && !explained.startsWith(err.error.trim()) ? err.error.trim() : '';

  const act = async (a: FailAction) => {
    setPending(a);
    try {
      await onAction(a);
    } finally {
      setPending(null);
    }
  };
  const working = busy || pending !== null;

  const head = failed
    ? op === 'join' ? 'Joining did not finish' : 'Setting up did not finish'
    : done
      ? op === 'join' ? (role === 'witness' ? 'This computer is now a witness' : 'This computer is now a copy') : 'Your database is running'
      : op === 'join' ? `Joining ${pr?.cluster_name || 'the cluster'}` : 'Setting up your database';

  return (
    <>
      <Eyebrow>{done ? 'Done' : failed ? 'Stopped' : 'In progress'}</Eyebrow>
      <h1>{head}</h1>
      {!done && !failed && (
        <p class="lede" aria-live="polite">
          {active ? `${stepLabel(active, role)}…` : 'Getting ready…'} You can close this page: it carries on while CoyoteDB runs on this computer.
        </p>
      )}
      {failed && err && (
        <div style={{ margin: '8px 0 24px' }}>
          <Notice kind="bad" role="alert">
            <p>{explained}</p>
            {said && (
              <details class="said">
                <summary>What CoyoteDB reported</summary>
                <p>{said}</p>
              </details>
            )}
          </Notice>
        </div>
      )}
      {pr && !done && (
        <div class="panel" style={{ marginTop: '20px' }}>
          <ol class="steps">
            {pr.steps.map((s) => {
              const d = stepDetail(s);
              const pct = s.percent ?? (s.bytes_total ? Math.floor(((s.bytes_done ?? 0) / s.bytes_total) * 100) : null);
              return (
                <li key={s.id} class={s.state} aria-current={s.state === 'active' ? 'step' : undefined}>
                  <span class="s-icon"><StepIcon s={s} /></span>
                  <div>
                    <div class="s-label">
                      {stepLabel(s, role)}
                      {s.id === 'copy' && pr.from ? <span class="muted"> from {pr.from}</span> : null}
                    </div>
                    {d && <div class="s-detail">{d}</div>}
                    {s.state === 'active' && pct != null && (
                      <div class="meter" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100} aria-label={stepLabel(s, role)}>
                        <i style={{ width: `${pct}%` }} />
                      </div>
                    )}
                  </div>
                </li>
              );
            })}
          </ol>
        </div>
      )}
      {failed && (
        <div class="wizard-nav">
          <div class="btn-row">
            {extra.includes('mixed') && (
              <button type="button" class="btn accent" disabled={working} onClick={() => void act('mixed')}>
                <Busy busy={pending === 'mixed'}>Join anyway (the cluster checks this computer first)</Busy>
              </button>
            )}
            {extra.includes('witness') && (
              <button type="button" class={extra.includes('mixed') ? 'btn' : 'btn accent'} disabled={working} onClick={() => void act('witness')}>
                <Busy busy={pending === 'witness'}>Join as a witness instead</Busy>
              </button>
            )}
            <button type="button" class={extra.length ? 'btn ghost' : 'btn'} disabled={working} onClick={() => void act('retry')}>
              Try again
            </button>
          </div>
          <button type="button" class="btn ghost" disabled={working} onClick={() => void act('reset')}>
            <Busy busy={pending === 'reset'}>Start over</Busy>
          </button>
        </div>
      )}
      {done && <Success op={op} role={role} backups={backups} />}
    </>
  );
}

function Success({ op, role, backups }: { op: 'create' | 'join'; role: Role; backups: boolean }) {
  const [invite, setInvite] = useState<InviteResponse | null>(null);
  const [blocked, setBlocked] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const now = useNow(30000);
  const finish = async () => {
    chosenBackup = null;
    await refreshStatus().catch(() => undefined);
    store.set({ wizard: false });
    navigate('/', true);
  };
  const makeInvite = async () => {
    setBusy(true);
    setErr('');
    try {
      setInvite(await api.createInvite({ ttl: '1h', role: 'data' }));
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      <p class="lede">
        {op === 'create'
          ? 'It is real PostgreSQL 18, running on this computer. Right now the data is on this computer only.'
          : role === 'witness'
            ? 'It holds no data. It helps the other computers agree, so switch-over can happen by itself.'
            : 'It has a full copy of the data and follows the main node live. If the main node stops, it can take over.'}
      </p>
      {op === 'create' && backups && <BackupApplied draft={chosenBackup} onBlock={setBlocked} />}
      {op === 'create' && (
        <div class="panel" style={{ marginTop: '20px' }}>
          <h2 class="section-title">Next: add a second computer</h2>
          <p class="ink2">A second computer keeps a live copy. With a third (or a small witness), switch-over happens by itself.</p>
          <p class="muted small">A second data computer must run the same system as this one. A witness can run any.</p>
          {!invite ? (
            <>
              {err && <Notice kind="bad"><p>{err}</p></Notice>}
              <button type="button" class="btn" onClick={makeInvite} disabled={busy} style={{ marginTop: '8px' }}>
                <Busy busy={busy}>
                  <I.Plus /> Make an invite code
                </Busy>
              </button>
            </>
          ) : (
            <div class="stack" style={{ '--gap': '14px', marginTop: '12px' }}>
              <div class="invite-code" role="textbox" aria-readonly="true" aria-label="Invite code" tabIndex={0}>{invite.code}</div>
              <div class="row">
                <CopyButton text={invite.code} label="Copy code" what="invite code" />
                <span class="muted small">
                  Works once · expires {until(invite.expires, now)} ({clock(invite.expires, now)})
                </span>
              </div>
              <p class="ink2" style={{ margin: 0 }}>On the other computer, install CoyoteDB and open its dashboard, choose <strong>Join</strong> and paste the code. Or run this and paste the code when it asks:</p>
              <CodeBlock title="On the other computer" code={invite.command ?? 'coyote join'} what="join command">
                <span class="prompt">$ </span>
                {invite.command ?? 'coyote join'}
              </CodeBlock>
            </div>
          )}
        </div>
      )}
      <div class="wizard-nav">
        <button type="button" class="btn accent" disabled={blocked} onClick={() => void finish()}>
          Go to the dashboard <I.Arrow />
        </button>
      </div>
    </>
  );
}
