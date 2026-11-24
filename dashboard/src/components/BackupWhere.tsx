// "Where do backups go?": the choices shared by the setup step and the
// Backups page. Folders are checked by the agent as they are typed (exists,
// can write, free space, same disk as the data); cloud storage is tested
// only when the person presses Test (it contacts the storage).
import { useEffect, useRef, useState } from 'preact/hooks';
import { api, errorMessage } from '../api/client';
import type { BackupDestCheck } from '../api/types';
import { draftKey, draftRequest, ENCRYPTION_TEXT, OFF_CONSEQUENCE, PROVIDERS, s3FieldProblem, SAME_DISK, type DestDraft, type Provider } from '../lib/backups';
import { bytes } from '../lib/format';
import * as I from './icons';
import { Busy } from './ui';

export interface CheckState {
  key: string;
  result?: BackupDestCheck;
  error?: string;
}

/** Runs the agent's destination check for a draft; `current` is the answer for the draft as it is now. */
export function useDestCheck(draft: DestDraft) {
  const [res, setRes] = useState<CheckState | null>(null);
  const [busy, setBusy] = useState(false);
  const seq = useRef(0);
  const run = async (d: DestDraft = draft) => {
    const req = draftRequest(d);
    if (!req) return;
    const key = draftKey(d);
    const n = ++seq.current;
    setBusy(true);
    try {
      const r = await api.checkBackupDestination(req);
      if (n === seq.current) setRes({ key, result: r });
    } catch (e) {
      if (n === seq.current) setRes({ key, error: errorMessage(e) });
    } finally {
      if (n === seq.current) setBusy(false);
    }
  };
  const current = res && res.key === draftKey(draft) ? res : null;
  return { current, busy, run };
}

/** Ready to save or continue: "off", or a check that passed for exactly this draft. */
export function destReady(draft: DestDraft, current: CheckState | null): boolean {
  if (draft.choice === 'off') return true;
  if (draft.choice === 'folder' && !draft.folder.trim()) return false;
  return !!current?.result?.ok;
}

export function BackupWhere(p: {
  draft: DestDraft;
  setDraft: (d: DestDraft) => void;
  check: ReturnType<typeof useDestCheck>;
  /** The setup step: offers "Not now" and words the default as the recommendation. */
  wizard?: boolean;
  /** On the page: an S3 secret may be left empty to keep the saved one. */
  savedSecret?: boolean;
  /** The id prefix for fields, so two editors never share ids. */
  idp?: string;
}) {
  const { draft, setDraft, check } = p;
  const idp = p.idp ?? 'bk';
  const set = (o: Partial<DestDraft>) => setDraft({ ...draft, ...o });
  const key = draftKey(draft);

  // Folders (and the default) are checked as they are typed.
  useEffect(() => {
    if (draft.choice !== 'default' && draft.choice !== 'folder') return;
    if (draft.choice === 'folder' && !draft.folder.trim()) return;
    const t = setTimeout(() => void check.run(draft), draft.choice === 'default' ? 0 : 450);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key]);

  const cur = check.current;
  const r = cur?.result;

  return (
    <fieldset class="bk-where">
      <legend class="visually-hidden">Where backups go</legend>
      <div class="choices">
        <label class="choice">
          <input type="radio" name={`${idp}-where`} checked={draft.choice === 'default'} onChange={() => set({ choice: 'default' })} />
          <span class="c-title">
            {p.wizard ? 'On, in a folder on this computer' : 'A folder next to the database'}
            {p.wizard && <span class="tag">Recommended</span>}
          </span>
          <div class="c-body">
            {p.wizard ? 'Every change saved, a full copy every night, kept 7 days.' : 'The simplest place. Each data computer keeps its own full set next to its data.'}
            {draft.choice === 'default' && <FolderResult check={cur} busy={check.busy} calm={p.wizard} />}
          </div>
        </label>

        <div class={`bk-opt${draft.choice === 'folder' ? ' open' : ''}`}>
        <label class="choice">
          <input type="radio" name={`${idp}-where`} checked={draft.choice === 'folder'} onChange={() => set({ choice: 'folder' })} />
          <span class="c-title">Another folder or disk</span>
          <div class="c-body">A second disk, a USB drive or a network drive, so a broken disk cannot take the backups with it.</div>
        </label>
            {draft.choice === 'folder' && (
              <div class="bk-sub">
                <div class="field">
                  <label for={`${idp}-folder`}>Folder</label>
                  <input
                    id={`${idp}-folder`}
                    class="input mono"
                    value={draft.folder}
                    placeholder={placeholderFolder()}
                    autoComplete="off"
                    autoCapitalize="off"
                    spellcheck={false}
                    aria-invalid={r && !r.ok ? 'true' : undefined}
                    aria-describedby={`${idp}-folder-msg`}
                    onInput={(e) => set({ folder: e.currentTarget.value })}
                  />
                  <div id={`${idp}-folder-msg`} aria-live="polite">
                    {!draft.folder.trim() ? (
                      <p class="hint">The full path. The same path is used on every data computer; it is created if it does not exist.</p>
                    ) : (
                      <FolderResult check={cur} busy={check.busy} />
                    )}
                  </div>
                </div>
                <label class="check-line">
                  <input type="checkbox" checked={draft.encrypt} onChange={(e) => set({ encrypt: e.currentTarget.checked })} />
                  <span>
                    Encrypt these backups <span class="muted">— you get a key to keep; without it they cannot be read</span>
                  </span>
                </label>
              </div>
            )}
        </div>

        <div class={`bk-opt${draft.choice === 's3' ? ' open' : ''}`}>
        <label class="choice">
          <input type="radio" name={`${idp}-where`} checked={draft.choice === 's3'}
            onChange={() =>
              set({
                choice: 's3',
                // Start from the chosen provider's example address and region.
                endpoint: draft.endpoint || PROVIDERS[draft.provider].endpoint,
                region: draft.endpoint ? draft.region : PROVIDERS[draft.provider].region,
              })
            }
          />
          <span class="c-title">
            Cloud storage <span class="tag neutral">Always encrypted</span>
          </span>
          <div class="c-body">Backblaze B2, Amazon S3, Cloudflare R2 or any S3-compatible storage. Your backups survive even if every computer is lost.</div>
        </label>
            {draft.choice === 's3' && <S3Fields draft={draft} set={set} check={check} savedSecret={!!p.savedSecret} idp={idp} />}
        </div>

        {p.wizard && (
          <label class="choice">
            <input type="radio" name={`${idp}-where`} checked={draft.choice === 'off'} onChange={() => set({ choice: 'off' })} />
            <span class="c-title">Not now</span>
            <div class="c-body">
              {OFF_CONSEQUENCE} You can turn them on any time on the Backups page.
            </div>
          </label>
        )}
      </div>
    </fieldset>
  );
}

function placeholderFolder(): string {
  return /Win/i.test(navigator.platform || navigator.userAgent) ? 'D:\\coyote-backups' : '/mnt/backup-disk/coyote-backups';
}

/** The agent's answer about a folder: where, free space, same disk. */
function FolderResult({ check, busy, calm }: { check: CheckState | null; busy: boolean; calm?: boolean }) {
  if (busy && !check)
    return (
      <p class="hint bk-line">
        <I.Spinner class="spin" /> Checking the folder…
      </p>
    );
  if (!check) return null;
  if (check.error)
    return (
      <p class="error bk-line">
        <I.Warn />
        <span>Could not check the folder: {check.error}</span>
      </p>
    );
  const r = check.result!;
  if (!r.ok)
    return (
      <p class="error bk-line" role="alert">
        <I.Warn />
        <span>{r.problem}</span>
      </p>
    );
  return (
    <div class="bk-result">
      <p class="ok bk-line">
        <I.CheckCircle />
        <span>
          <code class="bk-path">{r.folder}</code>
          {r.free_bytes ? <span class="muted"> · {bytes(r.free_bytes)} free</span> : null}
        </span>
      </p>
      {r.same_disk &&
        (calm ? (
          <p class="hint bk-line">Same disk as the database: it protects you from mistakes, not from that disk failing. You can move the backups later.</p>
        ) : (
          <p class="warn-line">
            <I.Warn />
            <span>{SAME_DISK}</span>
          </p>
        ))}
      {r.warnings?.map((w) => (
        <p key={w} class="hint bk-line">
          {w}
        </p>
      ))}
    </div>
  );
}

function S3Fields({ draft, set, check, savedSecret, idp }: { draft: DestDraft; set: (o: Partial<DestDraft>) => void; check: ReturnType<typeof useDestCheck>; savedSecret: boolean; idp: string }) {
  const prov = PROVIDERS[draft.provider];
  const fieldProblem = savedSecret && !draft.secretKey.trim() ? s3FieldProblem({ ...draft, secretKey: 'kept' }) : s3FieldProblem(draft);
  const cur = check.current;
  const r = cur?.result;
  const pick = (pv: Provider) => {
    const was = PROVIDERS[draft.provider];
    // Replace the address and region only while they are still the previous provider's examples.
    const endpoint = !draft.endpoint.trim() || draft.endpoint === was.endpoint ? PROVIDERS[pv].endpoint : draft.endpoint;
    const region = !draft.region.trim() || draft.region === was.region ? PROVIDERS[pv].region : draft.region;
    set({ provider: pv, endpoint, region });
  };
  return (
    <div class="bk-sub">
      <div class="segmented bk-prov" role="group" aria-label="Storage provider">
        {(Object.keys(PROVIDERS) as Provider[]).map((k) => (
          <button key={k} type="button" aria-pressed={draft.provider === k} onClick={() => pick(k)}>
            {PROVIDERS[k].name}
          </button>
        ))}
      </div>
      <div class="bk-grid">
        <div class="field span-2">
          <label for={`${idp}-endpoint`}>Storage address (endpoint)</label>
          <input id={`${idp}-endpoint`} class="input mono" value={draft.endpoint} onInput={(e) => set({ endpoint: e.currentTarget.value })} autoComplete="off" autoCapitalize="off" spellcheck={false} />
          <p class="hint">{prov.endpointHint}</p>
        </div>
        <div class="field">
          <label for={`${idp}-bucket`}>Bucket</label>
          <input id={`${idp}-bucket`} class="input mono" value={draft.bucket} onInput={(e) => set({ bucket: e.currentTarget.value })} autoComplete="off" autoCapitalize="off" spellcheck={false} />
        </div>
        <div class="field">
          <label for={`${idp}-prefix`}>
            Folder in the bucket <span class="muted">(optional)</span>
          </label>
          <input id={`${idp}-prefix`} class="input mono" value={draft.prefix} onInput={(e) => set({ prefix: e.currentTarget.value })} autoComplete="off" autoCapitalize="off" spellcheck={false} />
        </div>
        <div class="field">
          <label for={`${idp}-access`}>Access key ID</label>
          <input id={`${idp}-access`} class="input mono" value={draft.accessKey} onInput={(e) => set({ accessKey: e.currentTarget.value })} autoComplete="off" autoCapitalize="off" spellcheck={false} />
        </div>
        <div class="field">
          <label for={`${idp}-secret`}>Secret key</label>
          <input
            id={`${idp}-secret`}
            class="input mono"
            type="password"
            value={draft.secretKey}
            placeholder={savedSecret ? 'Leave empty to keep the saved one' : ''}
            onInput={(e) => set({ secretKey: e.currentTarget.value })}
            autoComplete="new-password"
            spellcheck={false}
          />
        </div>
        {(draft.provider === 's3' || draft.provider === 'other' || draft.region) && (
          <div class="field">
            <label for={`${idp}-region`}>
              Region <span class="muted">{draft.provider === 'other' ? '(optional)' : ''}</span>
            </label>
            <input id={`${idp}-region`} class="input mono" value={draft.region} onInput={(e) => set({ region: e.currentTarget.value })} autoComplete="off" autoCapitalize="off" spellcheck={false} />
            <p class="hint">{prov.regionHint}</p>
          </div>
        )}
      </div>
      <p class="hint" style={{ margin: '4px 0 0' }}>
        {prov.keyHint} The secret key is kept by your cluster and never shown again.
      </p>
      <div class="bk-test">
        <button type="button" class="btn small" disabled={!!fieldProblem || check.busy} onClick={() => void check.run()}>
          <Busy busy={check.busy}>Test access</Busy>
        </button>
        <div aria-live="polite" class="bk-test-msg">
          {fieldProblem ? (
            <span class="muted small">{fieldProblem}</span>
          ) : check.busy ? (
            <span class="muted small">Writing a small test file and deleting it again…</span>
          ) : cur?.error ? (
            <span class="error-text">
              <I.Warn />
              <span>{cur.error}</span>
            </span>
          ) : r && !r.ok ? (
            <span class="error-text" role="alert">
              <I.Warn />
              <span>{r.problem}</span>
            </span>
          ) : r?.ok ? (
            <span class="ok-text">
              <I.CheckCircle />
              <span>
                Works: this computer can list, write and delete in <code>{r.label}</code>.
              </span>
            </span>
          ) : (
            <span class="muted small">Test before you continue: it writes one small file and deletes it.</span>
          )}
        </div>
      </div>
      {r?.ok && r.warnings?.map((w) => (
        <p key={w} class="warn-line">
          <I.Warn />
          <span>{w}</span>
        </p>
      ))}
      <div class="bk-lock">
        <I.Lock />
        <p>
          {ENCRYPTION_TEXT} You see the key once, after you save. Keep it safe: you need it to restore if every computer is lost.
        </p>
      </div>
    </div>
  );
}
