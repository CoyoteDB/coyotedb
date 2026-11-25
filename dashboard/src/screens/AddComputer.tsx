// Add a computer: make an invite code for a data computer or a witness, with
// the same-system rule explained before anything else.
import { useEffect, useState } from 'preact/hooks';
import { api, ApiError, errorMessage } from '../api/client';
import type { InviteResponse, OpenInvite, Role } from '../api/types';
import * as I from '../components/icons';
import { DockerJoinSteps } from '../components/DockerJoin';
import { Busy, CodeBlock, ConfirmDialog, CopyButton, Notice, toast, useNow } from '../components/ui';
import { hasFeature } from '../lib/features';
import { clock, platformLabel, until } from '../lib/format';
import { summarize } from '../lib/health';
import { useStore } from '../lib/store';
import { store } from '../state';

const TTLS = [
  { v: '30m', label: '30 min' },
  { v: '1h', label: '1 hour' },
  { v: '24h', label: '1 day' },
  { v: '168h', label: '7 days' },
];

export function AddComputer() {
  const status = useStore(store, (s) => s.status);
  const version = useStore(store, (s) => s.version);
  const canCancel = hasFeature(version, 'invite_cancel');
  // Without invite_role the agent does not tie a code to a role: the role only changes the command.
  const roleBound = hasFeature(version, 'invite_role');
  const summary = status ? summarize(status) : null;
  const recommendWitness = summary?.setup === 'two';
  const [picked, setRole] = useState<Role | null>(null);
  // Until the person picks, follow the recommendation (status may arrive after this page opens).
  const role: Role = picked ?? (recommendWitness ? 'witness' : 'data');
  const [ttl, setTtl] = useState('1h');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const [invite, setInvite] = useState<InviteResponse | null>(null);
  const [open, setOpen] = useState<OpenInvite[] | null>(null);
  const [cancel, setCancel] = useState<OpenInvite | null>(null);
  const [cancelBusy, setCancelBusy] = useState(false);
  const [cancelErr, setCancelErr] = useState('');
  const now = useNow(10000);
  const platform = status?.cluster.platform ?? status?.nodes.find((n) => n.role === 'data')?.platform ?? null;

  const loadOpen = () =>
    api.invites().then(
      (r) => setOpen(r.invites),
      () => setOpen(null),
    );
  useEffect(() => void loadOpen(), []);

  const create = async () => {
    setBusy(true);
    setErr('');
    try {
      const inv = await api.createInvite({ ttl, role });
      setInvite(inv);
      void loadOpen();
    } catch (e) {
      setErr(e instanceof ApiError && (e.code === 'no_leader' || e.code === 'not_leader') ? 'An invite needs a majority of your computers to be reachable, and right now they are not. Bring them back first.' : errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const cmd = invite?.command ?? (invite ? `coyote ${(invite.role ?? role) === 'witness' ? 'witness' : 'join'}` : '');
  const invRole = invite?.role ?? role;

  return (
    <>
      <div class="page-head">
        <h1>Add a computer</h1>
        <p>Make an invite code here, then use it on the new computer. Each code works once and expires.</p>
      </div>

      <section class="panel" aria-labelledby="kind-h">
        <h2 id="kind-h" class="section-title">What kind of computer?</h2>
        <fieldset style={{ border: 0, padding: 0, margin: '12px 0 0' }}>
          <legend class="visually-hidden">Kind of computer</legend>
          <div class="choices two">
            <label class="choice">
              <input type="radio" name="kind" checked={role === 'data'} onChange={() => { setRole('data'); setInvite(null); }} />
              <span class="c-title">
                Data computer {!recommendWitness && <span class="tag">Recommended</span>}
              </span>
              <div class="c-body">
                Keeps a full, live copy and can become the main node.
                <p class="muted small">
                  {platform
                    ? `Must run ${platformLabel(platform)}, like the others.`
                    : 'Recommended: Linux. macOS and Windows work too, as long as every data computer runs the same system.'}
                </p>
              </div>
            </label>
            <label class="choice">
              <input type="radio" name="kind" checked={role === 'witness'} onChange={() => { setRole('witness'); setInvite(null); }} />
              <span class="c-title">
                Witness {recommendWitness && <span class="tag">Recommended</span>}
              </span>
              <div class="c-body">
                Holds no data; it only helps the others agree, so switch-over happens by itself. A small always-on computer, like a Raspberry Pi or a home server, is ideal.
                <p class="muted small">Can run any system: Windows, macOS or Linux.</p>
              </div>
            </label>
          </div>
        </fieldset>

        {platform && role === 'data' && (
          <div style={{ marginTop: '16px' }}>
            <Notice kind="info" title="Same system type for data computers">
              <p>
                This database’s data computers run <strong>{platformLabel(platform)}</strong>, so the new one must too. PostgreSQL’s files only work between identical systems. A
                witness can be anything.
              </p>
            </Notice>
          </div>
        )}

        <div class="field" style={{ marginTop: '24px' }}>
          <span class="flabel" id="ttl-l">Code expires after</span>
          <div class="segmented" role="group" aria-labelledby="ttl-l">
            {TTLS.map((t) => (
              <button key={t.v} type="button" aria-pressed={ttl === t.v} onClick={() => { setTtl(t.v); setInvite(null); }}>
                {t.label}
              </button>
            ))}
          </div>
        </div>
        {err && (
          <div style={{ marginTop: '16px' }}>
            <Notice kind="bad" role="alert"><p>{err}</p></Notice>
          </div>
        )}
        {!invite && (
          <div class="btn-row" style={{ marginTop: '24px' }}>
            <button type="button" class="btn" onClick={create} disabled={busy}>
              <Busy busy={busy}>
                <I.Plus />
                Make an invite code
              </Busy>
            </button>
          </div>
        )}
      </section>

      {invite && (
        <section class="panel" aria-labelledby="code-h" style={{ marginTop: '20px' }}>
          <div class="panel-head">
            <h2 id="code-h" class="section-title">{roleBound ? `Invite code for a ${invRole === 'witness' ? 'witness' : 'data computer'}` : 'Invite code'}</h2>
            <span class="muted small">
              Expires {until(invite.expires, now)} · {clock(invite.expires, now)}
            </span>
          </div>
          <div class="invite-code" role="textbox" aria-readonly="true" aria-label="Invite code" tabIndex={0}>{invite.code}</div>
          <div class="row" style={{ marginTop: '12px' }}>
            <CopyButton text={invite.code} label="Copy code" what="invite code" />
            <button type="button" class="linkish" onClick={() => setInvite(null)}>
              Make another
            </button>
          </div>

          <hr class="divider" />
          <h3 class="section-title">On the new computer</h3>
          {invite.docker ? (
            <div style={{ marginTop: '12px' }}>
              <DockerJoinSteps d={invite.docker} role={invRole} />
            </div>
          ) : (
          <div class="grid-2" style={{ marginTop: '12px' }}>
            <div>
              <span class="label">Easiest</span>
              <p class="ink2" style={{ marginTop: '6px' }}>
                Install CoyoteDB, open its dashboard, choose <strong>Join with an invite code</strong> and paste the code. Pick “{invRole === 'witness' ? 'A witness' : 'A data computer'}”.
              </p>
            </div>
            <div>
              <span class="label">Or in a terminal</span>
              <p class="ink2" style={{ margin: '6px 0 10px' }}>Run this, then paste the code when it asks:</p>
              <CodeBlock title="Terminal" code={cmd} what="command">
                <span class="prompt">$ </span>
                {cmd}
              </CodeBlock>
            </div>
          </div>
          )}
          {!roleBound && (
            <p class="muted small" style={{ marginTop: '16px', marginBottom: 0 }}>
              The code itself works for a data computer or a witness. What the new computer becomes is chosen when it joins: the command above joins it as a{' '}
              {invRole === 'witness' ? 'witness' : 'data computer'}.
            </p>
          )}
          <p class="muted small" style={{ marginTop: '16px', marginBottom: 0 }}>
            Treat the code like a password until it is used: anyone with it can join this database before it expires.{!canCancel && ' An unused code stops working when it expires.'}
          </p>
        </section>
      )}

      {open && open.length > 0 && (
        <section class="section" aria-labelledby="open-h" style={{ marginTop: '32px' }}>
          <h2 id="open-h" class="section-title">Invite codes</h2>
          <ul class="timeline">
            {open.map((o) => {
              const expired = !o.used && Date.parse(o.expires) <= now.getTime();
              return (
                <li key={o.id}>
                  <span class={`t-icon ${o.cancelled || expired ? 'info' : o.used ? 'good' : 'info'}`}>
                    {o.cancelled || expired ? <I.Cross /> : o.used ? <I.CheckCircle /> : <I.Clock />}
                  </span>
                  <div>
                    {roleBound ? `For a ${o.role === 'witness' ? 'witness' : 'data computer'}` : 'Invite code'}
                    {o.cancelled ? (
                      <> · <strong>Cancelled</strong></>
                    ) : o.used ? (
                      <> · used by <strong>{o.used_by ?? 'a computer'}</strong></>
                    ) : expired ? (
                      <> · expired</>
                    ) : (
                      <> · expires {until(o.expires, now)}</>
                    )}
                    <div class="muted small">Made {clock(o.created_at, now)}</div>
                  </div>
                  {!o.used && !expired && canCancel && (
                    <button type="button" class="btn small ghost" onClick={() => { setCancelErr(''); setCancel(o); }} aria-label={`Cancel invite (made ${clock(o.created_at, now)})`}>
                      Cancel invite
                    </button>
                  )}
                </li>
              );
            })}
          </ul>
        </section>
      )}

      <ConfirmDialog
        open={!!cancel}
        title="Cancel this invite code?"
        confirmLabel="Cancel the code"
        cancelLabel="Keep it"
        danger
        busy={cancelBusy}
        error={cancelErr}
        onClose={() => setCancel(null)}
        onConfirm={async () => {
          if (!cancel) return;
          setCancelBusy(true);
          try {
            await api.cancelInvite(cancel.id);
            toast('Invite code cancelled. It no longer works.');
            if (invite?.id === cancel.id) setInvite(null);
            setCancel(null);
          } catch (e) {
            setCancelErr(cancelError(e));
          } finally {
            void loadOpen();
            setCancelBusy(false);
          }
        }}
      >
        <p>Nobody can join with it afterwards. This cannot be undone: to add a computer later, make a new code.</p>
      </ConfirmDialog>
    </>
  );
}

/** Plain words for a cancel that did not go through, each with what to do next. */
function cancelError(e: unknown): string {
  if (e instanceof ApiError) {
    switch (e.code) {
      case 'secret_used':
        return 'That code was already used, so there is nothing to cancel. To take that computer out, remove it in Computers & settings.';
      case 'secret_expired':
        return 'That code has already expired, so it no longer works. Nothing else to do.';
      case 'unknown_invite':
        return 'That code is gone: it expired and was cleared. Nothing else to do.';
      case 'no_leader':
      case 'not_leader':
        return 'Cancelling needs a majority of your computers to be reachable, and right now they are not. Bring them back, then try again.';
    }
  }
  return `${errorMessage(e)} Try again in a moment.`;
}
