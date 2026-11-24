// Switch over: a planned move of the main role, or — with two computers and
// no witness, when the main one is gone — the forced promote, with its
// honest warning and a typed confirmation.
import { useMemo, useState } from 'preact/hooks';
import { api, ApiError, errorMessage } from '../api/client';
import type { Status } from '../api/types';
import * as I from '../components/icons';
import { A, ConfirmDialog, LinkButton, Notice, toast, useNow } from '../components/ui';
import { hasFeature } from '../lib/features';
import { ago, clock, lagText } from '../lib/format';
import { summarize } from '../lib/health';
import { useStore } from '../lib/store';
import { refreshStatus, store } from '../state';

export function SwitchOver() {
  const status = useStore(store, (s) => s.status);
  const version = useStore(store, (s) => s.version);
  const now = useNow(5000);
  const summary = useMemo(() => (status ? summarize(status, now) : null), [status, now]);
  if (!status || !summary) return <div class="skeleton" style={{ height: '240px' }} aria-label="Loading" />;

  const main = status.nodes.find((n) => n.primary);
  const promote = status.health.promote;
  const sw = status.health.switching;

  return (
    <>
      <div class="page-head">
        <h1>Switch over</h1>
        <p>
          Move the main role to another computer — before maintenance, or to use a faster machine. Apps are disconnected briefly and reconnect by
          themselves. No data is lost in a planned switch-over.
        </p>
      </div>
      {sw ? (
        <Notice kind="info" title="A switch-over is happening now">
          <p>{summary.sentence}</p>
        </Notice>
      ) : promote?.allowed && hasFeature(version, 'forced_promote') ? (
        <ForcedPromote status={status} />
      ) : main && !main.reachable ? (
        <MainGone status={status} />
      ) : (
        <Planned status={status} />
      )}
    </>
  );
}

function Planned({ status }: { status: Status }) {
  const main = status.nodes.find((n) => n.primary);
  const copies = status.nodes.filter((n) => n.role === 'data' && !n.primary);
  const firstEligible = copies.find((c) => c.eligible !== false && c.reachable && (c.replication ?? '').startsWith('streaming'));
  const [to, setTo] = useState(firstEligible?.name ?? '');
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  if (copies.length === 0) {
    return (
      <div class="empty">
        <h3>There is no other computer to switch to</h3>
        <p>A switch-over needs a second data computer holding a live copy.</p>
        <LinkButton href="/add">
          <I.Plus /> Add a computer
        </LinkButton>
      </div>
    );
  }
  const target = copies.find((c) => c.name === to);
  const canSwitch = (c: (typeof copies)[number]) => c.reachable && c.eligible !== false && (c.replication ?? '').startsWith('streaming');

  return (
    <section class="panel" aria-labelledby="pl-h">
      <h2 id="pl-h" class="section-title">Make another computer the main node</h2>
      <p class="ink2">Now: <strong>{main?.name ?? 'none'}</strong> is the main node.</p>
      <fieldset style={{ border: 0, padding: 0, margin: '16px 0 0' }}>
        <legend class="visually-hidden">New main node</legend>
        <div class="choices">
          {copies.map((c) => {
            const ok = canSwitch(c);
            const why = !c.reachable ? 'Not responding.' : c.report?.state === 'cloning' ? 'Still copying the data.' : !ok ? c.eligible_reason || 'Not following the main node closely enough yet.' : '';
            return (
              <label key={c.id} class="choice" style={ok ? undefined : { opacity: 0.7, cursor: 'not-allowed' }}>
                <input type="radio" name="target" disabled={!ok} checked={to === c.name} onChange={() => setTo(c.name)} />
                <span class="c-title">
                  {c.name}
                  {c.this && <span class="tag neutral">This computer</span>}
                </span>
                <div class="c-body">
                  {ok ? <>Following live · {lagText(c.lag_bytes, c.lag_seconds)}</> : <span>{why}</span>}
                </div>
              </label>
            );
          })}
        </div>
      </fieldset>
      {err && (
        <div style={{ marginTop: '16px' }}>
          <Notice kind="bad" role="alert"><p>{err}</p></Notice>
        </div>
      )}
      <div class="btn-row" style={{ marginTop: '20px' }}>
        <button type="button" class="btn" disabled={!target || !canSwitch(target)} onClick={() => { setErr(''); setConfirm(true); }}>
          <I.Swap /> Switch over to {to || '…'}
        </button>
      </div>
      <ConfirmDialog
        open={confirm}
        title={`Make ${to} the main node?`}
        confirmLabel="Switch over"
        busy={busy}
        error={err}
        onClose={() => setConfirm(false)}
        onConfirm={async () => {
          setBusy(true);
          setErr('');
          try {
            const r = await api.switchTo(to);
            setConfirm(false);
            toast(r.message || `${r.to} is now the main node.`);
            await refreshStatus().catch(() => undefined);
          } catch (e) {
            setErr(e instanceof ApiError && e.code === 'not_eligible' ? `${to} cannot take over right now: it is not caught up. Nothing changed.` : `${errorMessage(e)} Nothing changed.`);
          } finally {
            setBusy(false);
          }
        }}
      >
        <ul class="consequences">
          <li>{main?.name ?? 'The main node'} stops taking writes; {to} catches up to the last write and takes over.</li>
          <li>Apps are disconnected briefly and reconnect by themselves.</li>
          <li>{main?.name ?? 'The old main node'} becomes a copy of {to}.</li>
          <li>If anything goes wrong before the handover, nothing changes.</li>
        </ul>
      </ConfirmDialog>
    </section>
  );
}

function MainGone({ status }: { status: Status }) {
  const main = status.nodes.find((n) => n.primary)!;
  const s = summarize(status);
  const reason = status.health.promote?.reason;
  return (
    <section class="panel" aria-labelledby="mg-h">
      <h2 id="mg-h" class="section-title">{main.name}, the main node, is not responding</h2>
      {s.protection.kind === 'automatic' ? (
        <p class="ink2">With this setup another computer takes over by itself. There is nothing to press; watch the <A href="/">overview</A>.</p>
      ) : (
        <>
          <p class="ink2">{reason || 'Switching by hand has to be done from the computer that will take over.'}</p>
          <p class="ink2">Open this dashboard on that computer (run <code>coyote dashboard</code> there) and come back to this page.</p>
        </>
      )}
    </section>
  );
}

function ForcedPromote({ status }: { status: Status }) {
  const self = status.nodes.find((n) => n.this)!;
  const main = status.nodes.find((n) => n.primary);
  const p = status.health.promote!;
  const now = useNow(10000);
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const loss = p.may_lose_since;

  return (
    <section class="panel" aria-labelledby="fp-h" style={{ borderColor: 'var(--down)' }}>
      <h2 id="fp-h">Make {self.name} the main node</h2>
      <p class="ink2">
        <strong>{main?.name ?? 'The main node'}</strong> has not responded since {p.since ? `${clock(p.since, now)} (${ago(p.since, now)})` : 'a while'}. With two computers and no witness,{' '}
        {self.name} cannot tell whether it is really off or only cut off from this one, so it will not take over by itself. You can make it take over.
      </p>
      <Notice kind="bad" title="Read this first">
        <p>
          Do this only if {main?.name ?? 'the main computer'} is <strong>really off or broken</strong>. If it is still running somewhere — for example cut off by a network
          problem — and apps can still reach it, writes it takes would be lost later.
        </p>
        {loss ? (
          <p>
            {self.name} has not been kept in step since <strong>{clock(loss, now)}</strong>. Anything written after that, and only to {main?.name ?? 'the main node'}, will be
            lost.
          </p>
        ) : (
          <p>{self.name} was following live until {main?.name ?? 'the main node'} stopped, so normally nothing is lost — but a write in its last moments might be.</p>
        )}
        <p>When {main?.name ?? 'it'} comes back, it copies the data again from {self.name} and becomes the copy.</p>
      </Notice>
      <div class="btn-row" style={{ marginTop: '20px' }}>
        <button type="button" class="btn danger" onClick={() => { setErr(''); setOpen(true); }}>
          Make {self.name} the main node…
        </button>
        <span class="muted small">Prefer to wait? Bring {main?.name ?? 'the main computer'} back and everything continues by itself.</span>
      </div>
      <ConfirmDialog
        open={open}
        danger
        title={`Force ${self.name} to take over?`}
        confirmLabel={`Make ${self.name} main`}
        typeToConfirm={self.name}
        typeLabel={<>Type <code>{self.name}</code> (this computer’s name) to confirm</>}
        acknowledge={loss ? `I understand that writes made after ${clock(loss, now)} may be lost.` : 'I understand that recent writes may be lost.'}
        busy={busy}
        error={err}
        onClose={() => setOpen(false)}
        onConfirm={async () => {
          setBusy(true);
          setErr('');
          try {
            const r = await api.promote(self.name);
            setOpen(false);
            toast(r.message || `${self.name} is now the main node.`);
            await refreshStatus().catch(() => undefined);
          } catch (e) {
            setErr(errorMessage(e));
          } finally {
            setBusy(false);
          }
        }}
      >
        <ul class="consequences">
          <li>{self.name} becomes the main node and takes writes straight away.</li>
          <li>{main?.name ?? 'The old main computer'} is no longer trusted: when it is back it copies everything again from {self.name}.</li>
          <li>Afterwards, add a witness so this is automatic next time.</li>
        </ul>
      </ConfirmDialog>
    </section>
  );
}
