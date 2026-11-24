// Backups (M7, D63/D65): is my data backed up, how far back can I go, and
// the few things a person does with backups: back up now, change where
// and when, check a backup, restore to a moment. A restore always goes
// into a NEW folder; the running database is never replaced.
import { Fragment, type ComponentChildren } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { api, ApiError, errorMessage } from '../api/client';
import type { Backup, BackupJob, Backups as BackupsT, BackupSchedule, RestorePlan } from '../api/types';
import { BackupWhere, destReady, useDestCheck } from '../components/BackupWhere';
import * as I from '../components/icons';
import { Busy, CodeBlock, ConfirmDialog, CopyButton, Notice, toast, useNow } from '../components/ui';
import {
  backupHeadline,
  days,
  draftRequest,
  durWords,
  EMPTY_DRAFT,
  EVERY_CHOICES,
  KEEP_CHOICES,
  KEY_WARNING,
  matchChoice,
  OFF_CONSEQUENCE,
  providerOf,
  SAME_DISK,
  scheduleSentence,
  stamp,
  whenText,
  type BackupTone,
  type DestDraft,
} from '../lib/backups';
import { hasFeature } from '../lib/features';
import { ago, bytes, duration, parseGoDuration } from '../lib/format';
import { useStore } from '../lib/store';
import { store } from '../state';

/** A calm "not in this version" panel (used by Coding agents too). */
export function Later({ what, children }: { what: string; children: ComponentChildren }) {
  return (
    <section class="panel quiet" aria-label={`${what}: later version`}>
      <span class="tag neutral">Arrives in a later version</span>
      <div style={{ marginTop: '14px' }}>{children}</div>
    </section>
  );
}

type Load = { state: 'loading' } | { state: 'ok'; data: BackupsT } | { state: 'witness' } | { state: 'later' } | { state: 'error'; error: string };

export function Backups() {
  const version = useStore(store, (s) => s.version);
  const [load, setLoad] = useState<Load>({ state: 'loading' });
  const busyPoll = load.state === 'ok' && (load.data.check?.state === 'running' || load.data.restore?.state === 'running' || !!load.data.status.running);
  const seq = useRef(0);

  const reload = async (refresh = false) => {
    if (version && !hasFeature(version, 'backups')) {
      setLoad({ state: 'later' });
      return;
    }
    const n = ++seq.current;
    try {
      const d = await api.backups(refresh);
      if (n === seq.current) setLoad({ state: 'ok', data: d });
    } catch (e) {
      if (n !== seq.current) return;
      if (e instanceof ApiError && e.code === 'not_data_node') setLoad({ state: 'witness' });
      else if (e instanceof ApiError && (e.code === 'not_yet_available' || e.status === 501)) setLoad({ state: 'later' });
      else setLoad((l) => (l.state === 'ok' ? l : { state: 'error', error: errorMessage(e) }));
    }
  };
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => void reload(), [version]);
  useEffect(() => {
    const t = setInterval(() => void reload(), busyPoll ? 2000 : 20_000);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [busyPoll, version]);

  if (load.state === 'loading')
    return (
      <div aria-busy="true">
        <div class="page-head">
          <h1>Backups</h1>
        </div>
        <div class="bk-hero">
          <div class="stack">
            <div class="skeleton" style={{ height: '28px', width: '80%' }} />
            <div class="skeleton" style={{ height: '18px', width: '60%' }} />
          </div>
          <div class="skeleton" style={{ height: '180px' }} />
        </div>
        <span class="visually-hidden" role="status">
          Loading backups…
        </span>
      </div>
    );
  if (load.state === 'witness')
    return (
      <>
        <div class="page-head">
          <h1>Backups</h1>
          <p>This computer is a witness: it holds no data, so it keeps no backups. Open the dashboard on one of your data computers to see and change them.</p>
        </div>
      </>
    );
  if (load.state === 'later')
    return (
      <>
        <div class="page-head">
          <h1>Backups</h1>
          <p>The version of CoyoteDB on this computer has no backup page. Update it to see and change backups here.</p>
        </div>
      </>
    );
  if (load.state === 'error')
    return (
      <>
        <div class="page-head">
          <h1>Backups</h1>
        </div>
        <Notice kind="bad" title="Could not load the backups" role="alert">
          <p>{load.error}</p>
          <button type="button" class="btn small ghost" onClick={() => void reload()}>
            Try again
          </button>
        </Notice>
      </>
    );
  return <BackupsLive data={load.data} reload={reload} />;
}

function ToneIcon({ tone }: { tone: BackupTone }) {
  if (tone === 'ok') return <I.CheckCircle />;
  if (tone === 'attention') return <I.Warn />;
  if (tone === 'pending') return <I.Clock />;
  return <I.Pending />;
}

function BackupsLive({ data, reload }: { data: BackupsT; reload: (refresh?: boolean) => Promise<void> }) {
  const now = useNow(30_000);
  const s = data.status;
  const head = backupHeadline(s, now);
  const [busy, setBusy] = useState<'' | 'now' | 'on'>('');
  const [restoreOpen, setRestoreOpen] = useState(false);
  const [checkOpen, setCheckOpen] = useState(false);
  const jobRunning = data.check?.state === 'running' || data.restore?.state === 'running';

  const backupNow = async () => {
    setBusy('now');
    try {
      const r = await api.runBackup();
      toast(r.message);
      await reload();
    } catch (e) {
      toast(errorMessage(e), 'bad');
    } finally {
      setBusy('');
    }
  };
  const turnOn = async () => {
    setBusy('on');
    try {
      await api.setBackupSchedule({ enabled: true });
      toast('Backups are on.');
      await reload(true);
    } catch (e) {
      toast(errorMessage(e), 'bad');
    } finally {
      setBusy('');
    }
  };

  const keep = durWords(data.schedule.keep);
  let sentence: ComponentChildren;
  if (!s.enabled) sentence = OFF_CONSEQUENCE;
  else if (s.problem) sentence = s.problem;
  else if (s.restorable) sentence = <>You can go back to any moment from {stamp(s.restorable.from, now)} to {ago(s.restorable.to, now)}.</>;
  else sentence = 'Every change is already being saved. Once the first full copy is done, you can go back to any moment after it.';

  return (
    <>
      <section class="bk-hero" aria-labelledby="bk-h">
        <div>
          <span class="label">Backups</span>
          <h1 id="bk-h" class={`t-${head.tone}`} style={{ marginTop: '10px' }}>
            <ToneIcon tone={head.tone} />
            <span>{head.title}</span>
          </h1>
          <p class="sentence" aria-live="polite">
            {sentence}
          </p>
          {s.problem && s.last_error && s.last_error !== s.problem.replace(/^Backups: /, '') && (
            <details class="said" style={{ margin: '-8px 0 16px' }}>
              <summary>What CoyoteDB reported</summary>
              <p>{s.last_error}</p>
            </details>
          )}
          <div class="btn-row">
            {s.enabled ? (
              <>
                <button type="button" class="btn accent" onClick={() => void backupNow()} disabled={busy !== '' || !!s.running}>
                  <Busy busy={busy === 'now'}>{s.running ? 'A backup is running' : 'Back up now'}</Busy>
                </button>
                <button type="button" class="btn ghost" onClick={() => setRestoreOpen(true)} disabled={!s.restorable || jobRunning}>
                  Restore to a moment…
                </button>
                <button type="button" class="btn ghost" onClick={() => setCheckOpen(true)} disabled={!s.last_backup || jobRunning}>
                  Check a backup…
                </button>
              </>
            ) : (
              <button type="button" class="btn accent" onClick={() => void turnOn()} disabled={busy !== ''}>
                <Busy busy={busy === 'on'}>Turn backups on</Busy>
              </button>
            )}
          </div>
          {s.enabled && !s.restorable && !s.last_backup && (
            <p class="muted small" style={{ margin: '12px 0 0' }}>
              Restore and check open once the first full backup is done.
            </p>
          )}
        </div>
        {s.enabled && <Facts data={data} now={now} keep={keep} />}
      </section>

      {data.restore && <JobPanel job={data.restore} now={now} />}
      {data.check && <JobPanel job={data.check} now={now} />}

      <div class="bk-cards">
        <WhereCard data={data} reload={reload} />
        <ScheduleCard data={data} reload={reload} now={now} />
      </div>

      <BackupList data={data} now={now} reload={reload} />

      <details class="section bk-details">
        <summary class="linkish">Details for the technical</summary>
        <div class="panel quiet">
          <dl class="kv small">
            <dt>Every change</dt>
            <dd>WAL archiving from every data computer, at least every {durWords(data.schedule.archive_timeout)}.</dd>
            <dt>Last change saved here</dt>
            <dd>
              {s.archive.last_archived ? (
                <>
                  <code>{s.archive.last_archived}</code> {s.archive.last_archived_at ? `· ${ago(s.archive.last_archived_at, now)}` : ''}
                </>
              ) : (
                'none yet'
              )}
            </dd>
            <dt>Waiting to be saved</dt>
            <dd>{s.archive.waiting_bytes ? `${bytes(s.archive.waiting_bytes)} (oldest ${duration(s.archive.lag_ms)})` : 'nothing'}</dd>
            {s.archive.failing_since && (
              <>
                <dt>Saving fails since</dt>
                <dd>{stamp(s.archive.failing_since, now)}</dd>
              </>
            )}
            {!!s.archive.conflicts && (
              <>
                <dt>Kept aside after a failover</dt>
                <dd>{s.archive.conflicts} segments (normal after a switch-over)</dd>
              </>
            )}
            {s.gap && (
              <>
                <dt>Gap</dt>
                <dd>
                  {s.gap.from}…{s.gap.to} at {stamp(s.gap.at, now)}: {s.gap.reason} {s.gap.closed ? '(closed by a later full backup)' : ''}
                </dd>
              </>
            )}
            <dt>Restore</dt>
            <dd>Point-in-time recovery into a new folder; the running database is never replaced.</dd>
            <dt>Command line</dt>
            <dd>
              <code>coyote backup</code> shows the same; <code>coyote backup help</code> lists every command.
            </dd>
          </dl>
        </div>
      </details>

      <RestoreDialog open={restoreOpen} data={data} now={now} onClose={() => setRestoreOpen(false)} reload={reload} />
      <ConfirmDialog
        open={checkOpen}
        title="Check the newest backup?"
        confirmLabel="Check it"
        onClose={() => setCheckOpen(false)}
        onConfirm={async () => {
          setCheckOpen(false);
          try {
            await api.checkBackup();
            await reload();
          } catch (e) {
            toast(errorMessage(e), 'bad');
          }
        }}
      >
        <ul class="consequences">
          <li>The newest full backup and every change after it are restored into a temporary folder on this computer.</li>
          <li>Every page of every table and index is read. Then the folder is deleted.</li>
          <li>It takes a few minutes for most databases. Your database keeps running.</li>
        </ul>
      </ConfirmDialog>
    </>
  );
}

/** The big facts: last good backup, how far back, with the window drawn. */
function Facts({ data, now, keep }: { data: BackupsT; now: Date; keep: string }) {
  const s = data.status;
  const lb = s.last_backup;
  const fin = lb?.finished_at;
  return (
    <div class="bk-facts">
      <div class="bk-fact">
        <span class="label">Last good backup</span>
        <span class="big">{fin ? ago(fin, now) : 'None yet'}</span>
        <span class="sub">
          {fin && lb
            ? `${stamp(fin, now)} · ${bytes(lb.size_bytes)} · by ${lb.node}`
            : s.running
              ? `Running on ${s.running.node} since ${stamp(s.running.started_at, now)}`
              : s.next_backup_at
                ? `The first is taken ${whenText(s.next_backup_at, now)}.`
                : 'The first is taken soon.'}
        </span>
      </div>
      <div class="bk-fact">
        <span class="label">How far back you can go</span>
        <span class="big">{s.restorable ? spanWords(s.restorable.from, s.restorable.to) : '—'}</span>
        <span class="sub">{s.restorable ? `Any moment from ${stamp(s.restorable.from, now)} to ${ago(s.restorable.to, now)}. Kept ${keep}.` : `After the first full backup. Kept ${keep}.`}</span>
        {s.restorable && <Window data={data} now={now} />}
      </div>
    </div>
  );
}

function spanWords(from: string, to: string): string {
  const ms = new Date(to).getTime() - new Date(from).getTime();
  if (ms >= 86_400_000) {
    const d = Math.round(ms / 86_400_000);
    return d === 1 ? '1 day' : `${d} days`;
  }
  return duration(ms);
}

/** The restorable window as a bar, with a tick per usable full backup. */
function Window({ data, now }: { data: BackupsT; now: Date }) {
  const r = data.status.restorable!;
  const keepMs = parseGoDuration(data.schedule.keep);
  const end = now.getTime();
  const start = Math.min(new Date(r.from).getTime(), end - (Number.isFinite(keepMs) ? keepMs : 7 * 86_400_000));
  const pos = (t: string) => Math.max(0, Math.min(100, ((new Date(t).getTime() - start) / (end - start)) * 100));
  const ticks = data.backups.filter((b) => b.state === 'done' && b.finished_at).map((b) => pos(b.finished_at!));
  const left = pos(r.from);
  const right = 100 - pos(r.to);
  return (
    <div class="bk-window">
      <div class="bk-bar" role="img" aria-label={`Restorable from ${stamp(r.from, now)} to ${ago(r.to, now)}, ${ticks.length} full backups`}>
        <span class="span" style={{ left: `${left}%`, right: `${right}%` }} />
        {ticks.map((t, i) => (
          <span key={i} class="tick" style={{ left: `${t}%` }} />
        ))}
        <span class="now" />
      </div>
      <div class="bk-axis" aria-hidden="true">
        <span>{stamp(new Date(start), now)}</span>
        <span>now</span>
      </div>
      <ul class="bk-legend" aria-hidden="true">
        <li>
          <i class="l-span" />
          can restore
        </li>
        <li>
          <i class="l-tick" />
          full backup
        </li>
        <li>
          <i class="l-now" />
          now
        </li>
      </ul>
    </div>
  );
}

// ---------------------------------------------------------------- where

function draftFrom(data: BackupsT): DestDraft {
  const d = data.destination;
  if (d.kind === 's3') {
    const m = /^s3:\/\/([^/\s]+)\/?(\S*)\s*\((https?:\/\/[^)]+)\)/.exec(d.label);
    return { ...EMPTY_DRAFT, choice: 's3', bucket: m?.[1] ?? '', prefix: m?.[2] ?? '', endpoint: m?.[3] ?? '', provider: providerOf(m?.[3] ?? '') };
  }
  return { ...EMPTY_DRAFT, choice: d.same_disk && /-backups$/.test(d.label) ? 'default' : 'folder', folder: d.label, encrypt: data.status.encrypted };
}

function WhereCard({ data, reload }: { data: BackupsT; reload: (refresh?: boolean) => Promise<void> }) {
  const d = data.destination;
  const s = data.status;
  const [edit, setEdit] = useState(false);
  const [askKey, setAskKey] = useState(false);
  const [key, setKey] = useState<{ key: string; fresh: boolean } | null>(null);
  const [keyErr, setKeyErr] = useState('');
  const [keyBusy, setKeyBusy] = useState(false);
  return (
    <section class="panel" aria-labelledby="where-h">
      <span class="label">Where</span>
      <h2 id="where-h" class="section-title" style={{ marginTop: '6px' }}>
        {d.kind === 's3' ? 'Cloud storage' : d.per_node ? 'A folder on each data computer' : 'A folder'}
      </h2>
      <p class="bk-where-label">{d.label}</p>
      <div class="row" style={{ gap: '6px', marginBottom: '12px' }}>
        {s.encrypted ? <span class="tag">Encrypted</span> : <span class="tag neutral">Not encrypted</span>}
        <span class="tag neutral">{d.per_node ? 'Each computer keeps its own set' : 'One shared set'}</span>
      </div>
      {d.same_disk && (
        <p class="warn-line" style={{ margin: '0 0 12px' }}>
          <I.Warn />
          <span>{SAME_DISK}</span>
        </p>
      )}
      <div class="btn-row">
        <button type="button" class="btn small" onClick={() => setEdit(true)}>
          Change where…
        </button>
        {s.encrypted && (
          <button type="button" class="btn small ghost" onClick={() => { setKeyErr(''); setAskKey(true); }}>
            <I.Lock /> Show the backup key…
          </button>
        )}
      </div>
      <WhereDialog
        open={edit}
        data={data}
        onClose={() => setEdit(false)}
        onSaved={async (newKey) => {
          setEdit(false);
          if (newKey) setKey({ key: newKey, fresh: true });
          toast('Saved. New backups go there from now on.');
          await reload(true);
        }}
      />
      <ConfirmDialog
        open={askKey}
        title="Show the backup key?"
        confirmLabel="Show it"
        busy={keyBusy}
        error={keyErr}
        onClose={() => setAskKey(false)}
        onConfirm={async () => {
          setKeyBusy(true);
          try {
            const r = await api.revealBackupKey();
            setAskKey(false);
            setKey({ key: r.key, fresh: false });
          } catch (e) {
            setKeyErr(errorMessage(e));
          } finally {
            setKeyBusy(false);
          }
        }}
      >
        <p class="ink2">Anyone with this key and access to the backups can read your data. Show it only where nobody is looking over your shoulder.</p>
      </ConfirmDialog>
      <KeyDialog shown={key} onClose={() => setKey(null)} />
    </section>
  );
}

function KeyDialog({ shown, onClose }: { shown: { key: string; fresh: boolean } | null; onClose: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const [saved, setSaved] = useState(false);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (shown && !d.open) {
      setSaved(false);
      d.showModal();
    } else if (!shown && d.open) d.close();
  }, [shown]);
  const mustAck = !!shown?.fresh;
  return (
    <dialog ref={ref} class="modal" aria-labelledby="key-h" onCancel={(e) => mustAck && !saved && e.preventDefault()} onClose={() => shown && onClose()}>
      <div class="modal-body">
        <h2 id="key-h">{mustAck ? 'Your new backup key' : 'Backup key'}</h2>
        {shown && (
          <div class="key-box">
            {mustAck && (
              <p>
                <strong>It is shown once now.</strong> You need it to restore these backups.
              </p>
            )}
            <div class="key" role="textbox" aria-readonly="true" aria-label="Backup key" tabIndex={0}>
              {shown.key}
            </div>
            <div class="row">
              <CopyButton text={shown.key} label="Copy key" what="backup key" />
            </div>
            <p class="small">{KEY_WARNING}</p>
            {mustAck && (
              <label class="check-line">
                <input type="checkbox" checked={saved} onChange={(e) => setSaved(e.currentTarget.checked)} />
                <span>I saved the backup key somewhere safe</span>
              </label>
            )}
          </div>
        )}
      </div>
      <div class="modal-foot">
        <button type="button" class="btn" disabled={mustAck && !saved} onClick={onClose}>
          Done
        </button>
      </div>
    </dialog>
  );
}

function WhereDialog({ open, data, onClose, onSaved }: { open: boolean; data: BackupsT; onClose: () => void; onSaved: (newKey?: string) => Promise<void> }) {
  const ref = useRef<HTMLDialogElement>(null);
  const [draft, setDraft] = useState<DestDraft>(() => draftFrom(data));
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const check = useDestCheck(draft);
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) {
      setDraft(draftFrom(data));
      setErr('');
      d.showModal();
    } else if (!open && d.open) d.close();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);
  const savedS3 = data.destination.kind === 's3';
  const keepSecret = savedS3 && draft.choice === 's3' && !draft.secretKey.trim();
  // With the saved secret kept, the agent's test uses it too.
  const ready = destReady(draft, check.current);
  const save = async () => {
    const req = draftRequest(draft);
    if (!req) return;
    setBusy(true);
    setErr('');
    try {
      const r = await api.setBackupDestination(req);
      await onSaved(r.new_key);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <dialog ref={ref} class="modal wide" aria-labelledby="wd-h" onClose={() => open && onClose()} onCancel={(e) => busy && e.preventDefault()}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (ready && !busy) void save();
        }}
      >
        <div class="modal-body">
          <h2 id="wd-h">Where should backups go?</h2>
          <p class="ink2">Backups already taken stay where they are. New ones, and every change from now on, go to the new place.</p>
          {open && <BackupWhere draft={draft} setDraft={setDraft} check={check} savedSecret={keepSecret || savedS3} idp="wd" />}
          {err && (
            <div style={{ marginTop: '16px' }}>
              <Notice kind="bad" role="alert">
                <p>{err}</p>
              </Notice>
            </div>
          )}
        </div>
        <div class="modal-foot">
          <button type="button" class="btn ghost" onClick={onClose} disabled={busy}>
            Cancel
          </button>
          <button type="submit" class="btn" disabled={!ready || busy}>
            <Busy busy={busy}>Save</Busy>
          </button>
        </div>
      </form>
    </dialog>
  );
}

// ---------------------------------------------------------------- schedule

function ScheduleCard({ data, reload, now }: { data: BackupsT; reload: (refresh?: boolean) => Promise<void>; now: Date }) {
  const sc = data.schedule;
  const s = data.status;
  const [edit, setEdit] = useState(false);
  const [off, setOff] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  return (
    <section class="panel" aria-labelledby="when-h">
      <span class="label">When</span>
      <h2 id="when-h" class="section-title" style={{ marginTop: '6px' }}>
        {sc.enabled ? `A full copy every ${durWords(sc.every, true)}, kept ${durWords(sc.keep)}` : 'Off'}
      </h2>
      <p class="ink2" style={{ margin: '4px 0 12px' }}>
        {sc.enabled ? scheduleSentence(sc) : OFF_CONSEQUENCE}
      </p>
      {sc.enabled && s.next_backup_at && !s.running && (
        <p class="muted small" style={{ margin: '0 0 12px' }}>
          Next full backup {whenText(s.next_backup_at, now)}.
        </p>
      )}
      {!edit ? (
        <div class="btn-row">
          {sc.enabled && (
            <button type="button" class="btn small" onClick={() => setEdit(true)}>
              Change schedule…
            </button>
          )}
          {sc.enabled && (
            <button type="button" class="btn small danger-ghost" onClick={() => { setErr(''); setOff(true); }}>
              Turn backups off…
            </button>
          )}
        </div>
      ) : (
        <ScheduleForm
          sc={sc}
          onCancel={() => setEdit(false)}
          onSaved={async () => {
            setEdit(false);
            toast('Schedule saved.');
            await reload();
          }}
        />
      )}
      <ConfirmDialog
        open={off}
        danger
        title="Turn backups off?"
        confirmLabel="Turn off"
        busy={busy}
        error={err}
        onClose={() => setOff(false)}
        onConfirm={async () => {
          setBusy(true);
          try {
            await api.setBackupSchedule({ enabled: false });
            setOff(false);
            toast('Backups are off.');
            await reload();
          } catch (e) {
            setErr(errorMessage(e));
          } finally {
            setBusy(false);
          }
        }}
      >
        <ul class="consequences">
          <li>No new full copies, and changes are no longer saved, on every computer.</li>
          <li>{OFF_CONSEQUENCE}</li>
          <li>Backups already taken stay where they are. You can turn backups on again any time.</li>
        </ul>
      </ConfirmDialog>
    </section>
  );
}

const SAVE_EVERY = [
  { value: '30s', label: '30 seconds' },
  { value: '60s', label: '1 minute' },
  { value: '5m', label: '5 minutes' },
];

function ScheduleForm({ sc, onCancel, onSaved }: { sc: BackupSchedule; onCancel: () => void; onSaved: () => Promise<void> }) {
  const [every, setEvery] = useState(matchChoice(sc.every, EVERY_CHOICES) || '24h');
  const [at, setAt] = useState(sc.at || '03:00');
  const [keep, setKeep] = useState(matchChoice(sc.keep, KEEP_CHOICES) || `${Math.max(1, days(sc.keep))}d`);
  const [save, setSave] = useState(matchChoice(sc.archive_timeout, SAVE_EVERY) || '60s');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const keepOpts = useMemo(() => (KEEP_CHOICES.some((k) => k.value === keep) ? KEEP_CHOICES : [...KEEP_CHOICES, { value: keep, label: `${keep.slice(0, -1)} days` }]), [keep]);
  const daily = every === '24h' || every === '168h';
  const keepMs = Number(keep.slice(0, -1)) * 86_400_000;
  const tooShort = keepMs < parseGoDuration(every);
  return (
    <form
      class="stack"
      style={{ '--gap': '14px' }}
      onSubmit={async (e) => {
        e.preventDefault();
        if (tooShort) return;
        setBusy(true);
        setErr('');
        try {
          await api.setBackupSchedule({ every, at, keep, archive_timeout: save });
          await onSaved();
        } catch (e2) {
          setErr(errorMessage(e2));
        } finally {
          setBusy(false);
        }
      }}
    >
      <div class="bk-grid">
        <div class="field">
          <label for="sc-every">A full copy</label>
          <select id="sc-every" class="select input" value={every} onChange={(e) => setEvery(e.currentTarget.value)}>
            {EVERY_CHOICES.map((c) => (
              <option key={c.value} value={c.value}>
                {c.label}
              </option>
            ))}
          </select>
        </div>
        <div class="field">
          <label for="sc-at">{daily ? 'At' : 'Starting at'}</label>
          <input id="sc-at" class="input" type="time" value={at} onInput={(e) => setAt(e.currentTarget.value)} />
        </div>
        <div class="field">
          <label for="sc-keep">Keep backups for</label>
          <select id="sc-keep" class="select input" value={keep} onChange={(e) => setKeep(e.currentTarget.value)}>
            {keepOpts.map((c) => (
              <option key={c.value} value={c.value}>
                {c.label}
              </option>
            ))}
          </select>
        </div>
        <div class="field">
          <label for="sc-save">Save changes at least every</label>
          <select id="sc-save" class="select input" value={save} onChange={(e) => setSave(e.currentTarget.value)}>
            {SAVE_EVERY.map((c) => (
              <option key={c.value} value={c.value}>
                {c.label}
              </option>
            ))}
          </select>
        </div>
      </div>
      <p class="hint small muted" style={{ margin: 0 }}>
        The time is this computer’s clock. Keeping longer needs more space; you can go back as far as you keep.
      </p>
      {tooShort && (
        <p class="error-text">
          <I.Warn /> Keep backups at least as long as the time between full copies.
        </p>
      )}
      {err && (
        <Notice kind="bad" role="alert">
          <p>{err}</p>
        </Notice>
      )}
      <div class="btn-row">
        <button type="submit" class="btn small" disabled={busy || tooShort}>
          <Busy busy={busy}>Save schedule</Busy>
        </button>
        <button type="button" class="btn small ghost" onClick={onCancel} disabled={busy}>
          Cancel
        </button>
      </div>
    </form>
  );
}

// ---------------------------------------------------------------- list

const STATE_TAG: Record<string, { cls: string; text: string }> = {
  done: { cls: '', text: 'Ready' },
  running: { cls: 'running', text: 'Running' },
  incomplete: { cls: 'warn', text: 'Incomplete' },
};

function BackupList({ data, now, reload }: { data: BackupsT; now: Date; reload: (refresh?: boolean) => Promise<void> }) {
  const list = data.backups;
  const [refreshing, setRefreshing] = useState(false);
  const incomplete = list.some((b) => b.state === 'incomplete');
  return (
    <section class="section panel" aria-labelledby="list-h">
      <div class="panel-head">
        <h2 id="list-h" class="section-title">
          Full backups
        </h2>
        <button
          type="button"
          class="linkish"
          disabled={refreshing}
          onClick={async () => {
            setRefreshing(true);
            await reload(true);
            setRefreshing(false);
          }}
        >
          <Busy busy={refreshing}>Refresh</Busy>
        </button>
      </div>
      {list.length === 0 ? (
        <div class="empty">
          <h3 class="section-title">No full backups yet</h3>
          <p style={{ margin: 0 }}>
            {!data.status.enabled
              ? 'Backups are off.'
              : data.status.next_backup_at
                ? `The first one is taken ${whenText(data.status.next_backup_at, now)}, by itself. Or press Back up now.`
                : 'The first one is taken by itself soon. Or press Back up now.'}
          </p>
        </div>
      ) : (
        <table class="bk-table">
          <caption class="visually-hidden">Full backups, newest first</caption>
          <thead>
            <tr>
              <th scope="col">Finished</th>
              <th scope="col">State</th>
              <th scope="col">Taken</th>
              <th scope="col">Size</th>
              <th scope="col">Name</th>
            </tr>
          </thead>
          <tbody>
            {list.map((b) => (
              <BackupRow key={b.id || b.started_at} b={b} now={now} />
            ))}
          </tbody>
        </table>
      )}
      {incomplete && (
        <p class="muted small" style={{ margin: '12px 0 0' }}>
          Incomplete: it finished, but some changes after it were not saved, or the computer taking it stopped. It is never used for a restore and is removed with the old ones.
        </p>
      )}
      {data.destination.per_node && list.length > 0 && (
        <p class="muted small" style={{ margin: '12px 0 0' }}>
          These are the backups of this computer’s folder. Each data computer keeps its own set.
        </p>
      )}
    </section>
  );
}

function BackupRow({ b, now }: { b: Backup; now: Date }) {
  const tag = STATE_TAG[b.state] ?? { cls: 'neutral', text: b.state };
  return (
    <tr>
      <td class="when num">{b.finished_at ? stamp(b.finished_at, now) : `started ${stamp(b.started_at, now)}`}</td>
      <td class="state">
        <span class={`tag ${tag.cls}`}>{tag.text}</span>
      </td>
      <td class="meta">
        {b.kind === 'manual' ? 'By hand' : b.kind === 'scheduled' ? 'On schedule' : '—'} · {b.node}
      </td>
      <td class="meta num">{b.state === 'running' ? '…' : bytes(b.size_bytes)}</td>
      <td class="id">{b.id || '—'}</td>
    </tr>
  );
}

// ---------------------------------------------------------------- jobs

function JobPanel({ job, now }: { job: BackupJob; now: Date }) {
  const running = job.state === 'running';
  const failed = job.state === 'failed';
  const what = job.kind === 'check' ? 'Backup check' : 'Restore';
  const title = running
    ? job.kind === 'check'
      ? 'Checking the newest backup…'
      : `Restoring into ${job.into}…`
    : failed
      ? `${what} did not finish`
      : job.kind === 'check'
        ? 'Backup check passed'
        : 'Restored into a new folder';
  return (
    <section class="section panel" aria-labelledby={`job-${job.kind}-h`} aria-live="polite">
      <div class="panel-head">
        <h2 id={`job-${job.kind}-h`} class="section-title" style={{ display: 'flex', gap: '8px', alignItems: 'center' }}>
          {running ? <I.Spinner class="spin" style={{ width: '18px', height: '18px' }} /> : failed ? <I.CrossCircle style={{ width: '18px', height: '18px', color: 'var(--down)' }} /> : <I.CheckCircle style={{ width: '18px', height: '18px', color: 'var(--moss)' }} />}
          {title}
        </h2>
        <span class="muted small">{running ? `started ${ago(job.started_at, now)}` : job.finished_at ? ago(job.finished_at, now) : ''}</span>
      </div>
      {!running && !failed && job.kind === 'check' && (
        <p class="ink2">
          The newest backup restores, with every saved change, and every page reads without damage: {job.databases ?? 0} databases, {job.relations ?? 0} tables and indexes
          {job.took_ms ? `, in ${duration(job.took_ms)}` : ''}. The temporary copy was deleted.
        </p>
      )}
      {!running && !failed && job.kind === 'restore' && (
        <>
          <p class="ink2">
            The copy in <code>{job.into}</code> is your database as it was {job.target ? `at ${stamp(job.target, now)}` : 'at the newest saved change'}
            {job.last_change ? <> (last change in it: {job.last_change})</> : null}. Nothing in your running database changed.
          </p>
          {job.next_command && (
            <>
              <p class="ink2" style={{ marginBottom: '8px' }}>
                <strong>Next:</strong> start it as a new, separate database. Run this in a terminal on this computer (the ports are free here now):
              </p>
              <CodeBlock title="Terminal" code={job.next_command} what="start command">
                <span class="prompt">$ </span>
                {job.next_command}
              </CodeBlock>
              <p class="muted small" style={{ margin: '10px 0 0' }}>
                Then look at it, copy out what you need, or move your app to it. To get rid of it, stop it and delete the folder.
              </p>
            </>
          )}
        </>
      )}
      {failed && (
        <Notice kind="bad" role="alert">
          <p>{job.error}</p>
          <p>
            {job.kind === 'check'
              ? 'Your running database is fine; this backup is not. Take a new one with Back up now and check again.'
              : 'Nothing in your running database changed. Fix the cause above and restore again into a new, empty folder.'}
          </p>
        </Notice>
      )}
      {job.lines.length > 0 && (
        <details open={running} style={{ marginTop: '12px' }}>
          <summary class="linkish">What happened</summary>
          <ul class="job-lines" style={{ marginTop: '8px' }}>
            {job.lines.map((l, i) => (
              <li key={i}>{l}</li>
            ))}
          </ul>
        </details>
      )}
    </section>
  );
}

// ---------------------------------------------------------------- restore

/** "2026-11-24T14:05" in local time for an <input type=datetime-local>. */
function localInput(t: Date): string {
  const p = (n: number) => String(n).padStart(2, '0');
  return `${t.getFullYear()}-${p(t.getMonth() + 1)}-${p(t.getDate())}T${p(t.getHours())}:${p(t.getMinutes())}`;
}

type RStep = 'moment' | 'folder' | 'confirm';

function RestoreDialog({ open, data, now, onClose, reload }: { open: boolean; data: BackupsT; now: Date; onClose: () => void; reload: () => Promise<void> }) {
  const ref = useRef<HTMLDialogElement>(null);
  const win = data.status.restorable;
  const [step, setStep] = useState<RStep>('moment');
  const [latest, setLatest] = useState(true);
  const [moment, setMoment] = useState('');
  const [into, setInto] = useState('');
  const [plan, setPlan] = useState<RestorePlan | null>(null);
  const [planKey, setPlanKey] = useState('');
  const [planning, setPlanning] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const seq = useRef(0);

  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (open && !d.open) {
      setStep('moment');
      setLatest(true);
      setMoment(win ? localInput(new Date(Math.max(new Date(win.from).getTime(), new Date(win.to).getTime() - 15 * 60_000))) : '');
      setInto('');
      setPlan(null);
      setErr('');
      d.showModal();
    } else if (!open && d.open) d.close();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  const target = latest ? '' : moment ? new Date(moment).toISOString() : '';
  const momentProblem = (() => {
    if (latest || !win) return '';
    if (!moment) return 'Pick a date and time.';
    const t = new Date(moment).getTime();
    if (t < new Date(win.from).getTime()) return `The backups reach back to ${stamp(win.from, now)}. Pick a later moment.`;
    if (t > new Date(win.to).getTime()) return `The newest saved change is from ${stamp(win.to, now)}. Pick an earlier moment, or the newest.`;
    return '';
  })();

  // Ask the agent about the plan whenever the moment or the folder changes.
  useEffect(() => {
    if (!open || step === 'moment' || momentProblem) return;
    const n = ++seq.current;
    const k = `${target}|${into.trim()}`;
    setPlanning(true);
    const t = setTimeout(() => {
      api.planRestore({ to: target || undefined, into: into.trim() || undefined }).then(
        (p) => {
          if (n !== seq.current) return;
          setPlan(p);
          setPlanKey(k);
          if (!into.trim() && p.suggested_into) setInto(p.suggested_into);
          setPlanning(false);
        },
        (e) => {
          if (n !== seq.current) return;
          setPlan({ ok: false, problem: errorMessage(e), suggested_into: '' });
          setPlanning(false);
        },
      );
    }, 350);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, step, target, into]);

  const planOk = !!plan?.ok && !planning && !!into.trim() && planKey === `${target}|${into.trim()}`;
  const picks: { label: string; at: number }[] = win
    ? [
        { label: '15 minutes ago', at: now.getTime() - 15 * 60_000 },
        { label: '1 hour ago', at: now.getTime() - 3_600_000 },
        { label: 'This time yesterday', at: now.getTime() - 86_400_000 },
      ].filter((x) => x.at >= new Date(win.from).getTime() && x.at <= new Date(win.to).getTime())
    : [];
  const order: RStep[] = ['moment', 'folder', 'confirm'];
  const idx = order.indexOf(step);

  const start = async () => {
    setBusy(true);
    setErr('');
    try {
      await api.restore({ to: target || undefined, into: into.trim() });
      onClose();
      await reload();
      window.scrollTo({ top: 0, behavior: 'smooth' });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <dialog ref={ref} class="modal wide" aria-labelledby="rs-h" onClose={() => open && onClose()} onCancel={(e) => busy && e.preventDefault()}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (step === 'moment' && !momentProblem) setStep('folder');
          else if (step === 'folder' && planOk) setStep('confirm');
          else if (step === 'confirm' && planOk && !busy) void start();
        }}
      >
        <div class="modal-body">
          <ol class="flow-steps" aria-label={`Step ${idx + 1} of 3`}>
            {order.map((s, i) => (
              <li key={s} class={i < idx ? 'on' : i === idx ? 'now' : ''} />
            ))}
          </ol>
          {step === 'moment' && (
            <Fragment key="moment">
              <h2 id="rs-h">Which moment do you want back?</h2>
              <p class="ink2">
                The restore makes a <strong>new, separate copy</strong> of your database as it was then. Your running database is not touched.
              </p>
              <fieldset style={{ border: 0, padding: 0, margin: 0 }}>
                <legend class="visually-hidden">Moment</legend>
                <div class="choices">
                  <label class="choice">
                    <input type="radio" name="rs-when" checked={latest} onChange={() => setLatest(true)} />
                    <span class="c-title">The newest saved change</span>
                    <div class="c-body">{win ? `About ${ago(win.to, now)}. Use this when a computer or disk was lost.` : ''}</div>
                  </label>
                  <label class="choice">
                    <input type="radio" name="rs-when" checked={!latest} onChange={() => setLatest(false)} />
                    <span class="c-title">A moment I pick</span>
                    <div class="c-body">Just before a mistake, like a deleted table or a bad update.</div>
                  </label>
                </div>
              </fieldset>
              {!latest && win && (
                <div class="field" style={{ marginTop: '16px' }}>
                  <label for="rs-moment">Date and time (this computer’s clock)</label>
                  <input
                    id="rs-moment"
                    class="input"
                    type="datetime-local"
                    step={1}
                    value={moment}
                    min={localInput(new Date(win.from))}
                    max={localInput(new Date(win.to))}
                    aria-invalid={momentProblem ? 'true' : undefined}
                    aria-describedby="rs-moment-msg"
                    onInput={(e) => setMoment(e.currentTarget.value)}
                  />
                  <div id="rs-moment-msg" aria-live="polite">
                    {momentProblem ? (
                      <p class="error">
                        <I.Warn />
                        {momentProblem}
                      </p>
                    ) : (
                      <p class="hint">
                        Anywhere from {stamp(win.from, now)} to {stamp(win.to, now)}.
                      </p>
                    )}
                  </div>
                  {picks.length > 0 && (
                    <div class="moment-picks">
                      {picks.map((x) => (
                        <button key={x.label} type="button" class="btn small ghost" onClick={() => setMoment(localInput(new Date(x.at)))}>
                          {x.label}
                        </button>
                      ))}
                    </div>
                  )}
                </div>
              )}
            </Fragment>
          )}
          {step === 'folder' && (
            <Fragment key="folder">
              <h2 id="rs-h">Where should the copy go?</h2>
              <p class="ink2">A new, empty folder on this computer. Nothing is ever written over: a folder that has files in it is refused.</p>
              <div class="field">
                <label for="rs-into">New folder</label>
                <input
                  id="rs-into"
                  class="input mono"
                  value={into}
                  autoComplete="off"
                  autoCapitalize="off"
                  spellcheck={false}
                  aria-invalid={plan && !plan.ok && !planning ? 'true' : undefined}
                  aria-describedby="rs-into-msg"
                  onInput={(e) => setInto(e.currentTarget.value)}
                />
                <div id="rs-into-msg" aria-live="polite">
                  {planning || !plan ? (
                    <p class="hint">
                      <I.Spinner class="spin" style={{ width: '14px', height: '14px', verticalAlign: '-2px' }} /> Checking…
                    </p>
                  ) : !plan.ok ? (
                    <p class="error">
                      <I.Warn />
                      {plan.problem}
                    </p>
                  ) : (
                    <p class="ok">
                      <I.CheckCircle />
                      <span>
                        {plan.exists ? 'An empty folder' : 'A new folder'}
                        {plan.free_bytes ? ` · ${bytes(plan.free_bytes)} free` : ''}
                        {plan.need_bytes ? ` · needs about ${bytes(plan.need_bytes)}` : ''}
                      </span>
                    </p>
                  )}
                </div>
              </div>
            </Fragment>
          )}
          {step === 'confirm' && plan && (
            <Fragment key="confirm">
              <h2 id="rs-h">Restore into a new folder?</h2>
              <dl class="kv" style={{ margin: '8px 0 16px' }}>
                <dt>Moment</dt>
                <dd>{plan.target ? stamp(plan.target, now) : 'The newest saved change'}</dd>
                <dt>From full backup</dt>
                <dd>
                  <code>{plan.backup_id}</code>
                </dd>
                <dt>Into</dt>
                <dd>
                  <code>{plan.into}</code>
                </dd>
              </dl>
              <ul class="consequences">
                <li>Nothing in your running database changes, on any computer.</li>
                <li>It takes a few minutes, longer for a big database. You can close this window; it carries on.</li>
                <li>When it is done, this page shows the command that starts the copy as a new, separate database.</li>
              </ul>
            </Fragment>
          )}
          {err && (
            <Notice kind="bad" role="alert">
              <p>{err}</p>
            </Notice>
          )}
        </div>
        <div class="modal-foot">
          {step === 'moment' ? (
            <button type="button" class="btn ghost" onClick={onClose}>
              Cancel
            </button>
          ) : (
            <button type="button" class="btn ghost" onClick={() => setStep(order[idx - 1]!)} disabled={busy}>
              Back
            </button>
          )}
          {step === 'moment' && (
            <button type="submit" class="btn" disabled={!!momentProblem}>
              Continue
            </button>
          )}
          {step === 'folder' && (
            <button type="submit" class="btn" disabled={!planOk}>
              Continue
            </button>
          )}
          {step === 'confirm' && (
            <button type="submit" class="btn accent" disabled={!planOk || busy}>
              <Busy busy={busy}>Restore into the new folder</Busy>
            </button>
          )}
        </div>
      </form>
    </dialog>
  );
}
