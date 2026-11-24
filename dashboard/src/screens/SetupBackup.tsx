// The setup wizard's backup step ("Start a new database"), and what the
// success screen does with the choice once the database runs: the agent
// starts with backups on, next to the data folder (D63), so only a
// different choice is sent (D65).
import { useEffect, useRef, useState } from 'preact/hooks';
import { api, ApiError, errorMessage } from '../api/client';
import type { Backups } from '../api/types';
import { BackupWhere, destReady, useDestCheck } from '../components/BackupWhere';
import * as I from '../components/icons';
import { Busy, CopyButton, Eyebrow, Notice } from '../components/ui';
import { draftRequest, KEY_WARNING, OFF_CONSEQUENCE, type DestDraft } from '../lib/backups';

export function BackupStep(p: { draft: DestDraft; setDraft: (d: DestDraft) => void; onBack: () => void; onNext: () => void }) {
  const check = useDestCheck(p.draft);
  const ready = destReady(p.draft, check.current);
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        if (ready) p.onNext();
      }}
    >
      <Eyebrow>New database</Eyebrow>
      <h1>Keep backups of your data</h1>
      <p class="lede">
        A copy on another computer keeps you running when a computer stops. A backup protects you from mistakes, like a deleted table or a bad change: it lets you go back
        to any moment.
      </p>
      <div style={{ marginTop: '24px' }}>
        <BackupWhere draft={p.draft} setDraft={p.setDraft} check={check} wizard idp="wz" />
      </div>
      <details class="bk-details">
        <summary class="linkish">Details for the technical</summary>
        <div class="panel quiet">
          <dl class="kv">
            <dt>Every change</dt>
            <dd>
              Continuous WAL archiving from every data computer (<code>archive_mode = always</code>). A finished segment is saved at once, and at least every 60 seconds
              (<code>archive_timeout</code>).
            </dd>
            <dt>Full copy</dt>
            <dd>
              <code>pg_basebackup</code> every 24 hours at 03:00 on this computer’s clock (the first one about 10 minutes after the start), compressed. On cloud storage it is
              taken by a copy, not the main computer.
            </dd>
            <dt>Restore</dt>
            <dd>Point-in-time recovery (PITR) to any moment in the kept window, always into a new folder. The running database is never replaced.</dd>
            <dt>Kept</dt>
            <dd>7 days: every full copy newer than that, plus the newest older one and the WAL they need.</dd>
            <dt>Encryption</dt>
            <dd>Cloud: always, AES-256-GCM on this computer before upload, one key per cluster. Folder: optional.</dd>
          </dl>
          <p class="muted small" style={{ margin: '12px 0 0' }}>
            Change any of it later on the Backups page, or with <code>coyote backup</code>.
          </p>
        </div>
      </details>
      <div class="wizard-nav">
        <button type="button" class="btn ghost back" onClick={p.onBack}>
          Back
        </button>
        <button type="submit" class="btn" disabled={!ready}>
          {p.draft.choice === 'off' ? 'Continue without backups' : 'Continue'}
        </button>
      </div>
    </form>
  );
}

type Applied =
  | { state: 'working' }
  | { state: 'done'; label: string; key?: string; off?: boolean; sameDisk?: boolean }
  | { state: 'failed'; error: string };

/** After a new database starts: applies the backup choice, shows the key once. `onBlock` keeps "Go to the dashboard" off until a new key is saved. */
export function BackupApplied({ draft, onBlock }: { draft: DestDraft | null; onBlock: (b: boolean) => void }) {
  const [st, setSt] = useState<Applied>({ state: 'working' });
  const [saved, setSaved] = useState(false);
  const ran = useRef(false);

  const apply = async () => {
    setSt({ state: 'working' });
    // Right after the start the cluster may still be settling: try a few times.
    for (let attempt = 0; ; attempt++) {
      try {
        if (!draft || draft.choice === 'default') {
          const b: Backups = await api.backups();
          setSt(b.status.enabled ? { state: 'done', label: b.destination.label, sameDisk: b.destination.same_disk } : { state: 'done', label: '', off: true });
        } else if (draft.choice === 'off') {
          await api.setBackupSchedule({ enabled: false });
          setSt({ state: 'done', label: '', off: true });
        } else {
          const r = await api.setBackupDestination(draftRequest(draft)!);
          setSt({ state: 'done', label: r.destination.label, key: r.new_key, sameDisk: r.destination.same_disk });
          if (r.new_key) onBlock(true);
        }
        return;
      } catch (e) {
        const retry = e instanceof ApiError && (e.status === 409 || e.status === 503) && e.code !== 'destination_failed' && attempt < 4;
        if (!retry) {
          setSt({ state: 'failed', error: errorMessage(e) });
          return;
        }
        await new Promise((r) => setTimeout(r, 1500));
      }
    }
  };
  useEffect(() => {
    if (ran.current) return;
    ran.current = true;
    void apply();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <section class="panel" style={{ marginTop: '20px' }} aria-labelledby="bk-applied-h">
      <h2 id="bk-applied-h" class="section-title">
        Backups
      </h2>
      {st.state === 'working' && (
        <p class="ink2" style={{ margin: 0 }}>
          <Busy busy>Setting up backups…</Busy>
        </p>
      )}
      {st.state === 'failed' && (
        <Notice kind="warn" title="Your choice for backups is not saved yet">
          <p>{st.error}</p>
          <p>Until it is, backups stay on in the folder next to the database. You can also change this on the Backups page.</p>
          <button type="button" class="btn small ghost" onClick={() => void apply()}>
            Try again
          </button>
        </Notice>
      )}
      {st.state === 'done' && st.off && (
        <p class="ink2" style={{ margin: 0 }}>
          Backups are off. {OFF_CONSEQUENCE} Turn them on any time on the Backups page.
        </p>
      )}
      {st.state === 'done' && !st.off && (
        <>
          <p class="ink2" style={{ margin: '0 0 4px' }}>
            On: every change is saved, and a full copy is taken every night and kept 7 days. The first full copy is taken in about 10 minutes.
          </p>
          <p class="bk-where-label">{st.label}</p>
          {st.sameDisk && <p class="muted small" style={{ margin: 0 }}>Same disk as the database. A second disk or cloud storage also protects you from that disk failing.</p>}
        </>
      )}
      {st.state === 'done' && st.key && (
        <div class="key-box" style={{ marginTop: '16px' }} role="group" aria-labelledby="bk-key-h">
          <p id="bk-key-h">
            <strong>Your backup key. It is shown once.</strong> You need it to restore these backups.
          </p>
          <div class="key" aria-label="Backup key" role="textbox" aria-readonly="true" tabIndex={0}>
            {st.key}
          </div>
          <div class="row">
            <CopyButton text={st.key} label="Copy key" what="backup key" />
          </div>
          <p class="small">{KEY_WARNING}</p>
          <label class="check-line">
            <input
              type="checkbox"
              checked={saved}
              onChange={(e) => {
                setSaved(e.currentTarget.checked);
                onBlock(!e.currentTarget.checked);
              }}
            />
            <span>I saved the backup key somewhere safe</span>
          </label>
          {!saved && (
            <p class="muted small" style={{ display: 'flex', gap: '6px' }}>
              <I.Lock style={{ width: '14px', height: '14px', marginTop: '3px', flex: 'none' }} /> Tick this to go on. While a cluster computer runs, the Backups page can
              show it again.
            </p>
          )}
        </div>
      )}
    </section>
  );
}
