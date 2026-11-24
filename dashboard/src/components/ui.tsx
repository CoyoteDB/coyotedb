// Shared building blocks: logo, pills, notices, copy buttons, code blocks,
// confirm dialogs and toasts.
import type { ComponentChildren, TargetedEvent } from 'preact';
import { useEffect, useId, useRef, useState } from 'preact/hooks';
import { TONE_LABEL, type Tone } from '../lib/health';
import { createStore, useStore } from '../lib/store';
import { linkClick } from '../lib/router';
import * as I from './icons';

/** The CoyoteDB mark + wordmark (copied from site/src/components/Logo.astro). */
export function Logo({ size = 28, wordmark = true }: { size?: number; wordmark?: boolean }) {
  const w = Math.round(size * 1.2);
  const h = Math.round((w * 22) / 40);
  return (
    <span class="logo">
      <svg width={w} height={h} viewBox="0 10 40 22" aria-hidden="true" focusable="false">
        <path d="M6.07 20.25 A17 17 0 0 1 33.93 20.25" class="arc-2" />
        <path d="M10.58 23.4 A11.5 11.5 0 0 1 29.42 23.4" class="arc-1" />
        <path d="M13.6 30 A6.4 6.4 0 0 1 26.4 30 Z" class="disc" />
        <path d="M2 30 H38" class="horizon" />
      </svg>
      {wordmark && (
        <span class="logo-word">
          coyote<span class="db">db</span>
        </span>
      )}
    </span>
  );
}

/** The site's section marker (horizon, oxblood half-disc, one call arc). */
export function Mark() {
  return (
    <svg class="mark" viewBox="0 0 18 10" aria-hidden="true">
      <path d="M3.28 5.3A6.6 6.6 0 0 1 14.72 5.3" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
      <path d="M5.6 8.6A3.4 3.4 0 0 1 12.4 8.6Z" fill="var(--accent)" />
      <path d="M.75 8.6H17.25" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" />
    </svg>
  );
}

export function Eyebrow({ children }: { children: ComponentChildren }) {
  return (
    <div class="eyebrow label">
      <Mark />
      <span>{children}</span>
    </div>
  );
}

export function StatusPill({ tone, big, label }: { tone: Tone; big?: boolean; label?: string }) {
  return (
    <span class={`pill tone-${tone}${big ? ' big' : ''}`}>
      <I.ToneIcon tone={tone} />
      <span class="pill-text">{label ?? TONE_LABEL[tone]}</span>
    </span>
  );
}

export function Notice(props: { kind?: 'info' | 'warn' | 'bad' | 'good' | 'soon'; title?: string; children?: ComponentChildren; role?: 'alert' | 'status' }) {
  const kind = props.kind ?? 'info';
  const Icon = kind === 'bad' ? I.CrossCircle : kind === 'warn' ? I.Warn : kind === 'good' ? I.CheckCircle : kind === 'soon' ? I.Clock : I.Info;
  return (
    <div class={`notice ${kind}`} role={props.role}>
      <Icon />
      <div>
        {props.title && <p class="n-title">{props.title}</p>}
        {props.children}
      </div>
    </div>
  );
}

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Clipboard API needs a secure context; 127.0.0.1 is one, a VPN address over http is not.
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.className = 'visually-hidden';
    document.body.appendChild(ta);
    ta.select();
    let ok: boolean;
    try {
      ok = document.execCommand('copy');
    } catch {
      ok = false;
    }
    ta.remove();
    return ok;
  }
}

export function CopyButton({ text, label = 'Copy', what, class: cls = '' }: { text: string; label?: string; what?: string; class?: string }) {
  const [state, setState] = useState<'idle' | 'done' | 'failed'>('idle');
  const t = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(t.current), []);
  const onClick = async () => {
    const ok = await copyText(text);
    setState(ok ? 'done' : 'failed');
    clearTimeout(t.current);
    t.current = setTimeout(() => setState('idle'), 2000);
  };
  return (
    <button type="button" class={`copy-btn ${state === 'done' ? 'done' : ''} ${cls}`} onClick={onClick} aria-label={what ? `${label} ${what}` : label}>
      {state === 'done' ? <I.Check /> : <I.Copy />}
      <span aria-live="polite">{state === 'done' ? 'Copied' : state === 'failed' ? 'Select & copy' : label}</span>
    </button>
  );
}

export function CodeBlock({ title, code, copy = true, what, children }: { title?: string; code: string; copy?: boolean; what?: string; children?: ComponentChildren }) {
  return (
    <div class="code">
      {(title || copy) && (
        <div class="code-head">
          <span>{title}</span>
          {copy && <CopyButton text={code} what={what ?? title} />}
        </div>
      )}
      <pre tabIndex={0}>{children ?? code}</pre>
    </div>
  );
}

export function A({ href, children, class: cls }: { href: string; children: ComponentChildren; class?: string }) {
  return (
    <a href={href} class={cls} onClick={linkClick}>
      {children}
    </a>
  );
}

export function LinkButton({ href, children, kind = '' }: { href: string; children: ComponentChildren; kind?: string }) {
  return (
    <a href={href} class={`btn ${kind}`} onClick={linkClick}>
      {children}
    </a>
  );
}

export function Busy({ busy, children }: { busy: boolean; children: ComponentChildren }) {
  return (
    <>
      {busy && <I.Spinner class="spin" />}
      {children}
    </>
  );
}

/** A term with a short explanation the first time it matters. */
export function Term({ children, tip }: { children: ComponentChildren; tip: string }) {
  return (
    <span class="term-def" title={tip}>
      {children}
    </span>
  );
}

// ---- Confirm dialog ----

export interface ConfirmProps {
  open: boolean;
  title: string;
  children?: ComponentChildren;
  confirmLabel: string;
  /** The close button's words (default "Cancel"). */
  cancelLabel?: string;
  danger?: boolean;
  /** When set, the person must type this exact text. */
  typeToConfirm?: string;
  typeLabel?: ComponentChildren;
  /** Extra checkbox the person must tick. */
  acknowledge?: string;
  busy?: boolean;
  /** Keeps the confirm button off (for example while the input is invalid). */
  confirmDisabled?: boolean;
  error?: string;
  onConfirm: () => void;
  onClose: () => void;
}

export function ConfirmDialog(p: ConfirmProps) {
  const ref = useRef<HTMLDialogElement>(null);
  const [typed, setTyped] = useState('');
  const [ack, setAck] = useState(false);
  const id = useId();
  useEffect(() => {
    const d = ref.current;
    if (!d) return;
    if (p.open && !d.open) {
      setTyped('');
      setAck(false);
      d.showModal();
    } else if (!p.open && d.open) d.close();
  }, [p.open]);
  const ready = (!p.typeToConfirm || typed.trim() === p.typeToConfirm) && (!p.acknowledge || ack) && !p.busy && !p.confirmDisabled;
  const submit = (e: TargetedEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (ready) p.onConfirm();
  };
  return (
    <dialog ref={ref} class="modal" aria-labelledby={`${id}-t`} onClose={() => p.open && p.onClose()} onCancel={(e) => { if (p.busy) e.preventDefault(); }}>
      <form onSubmit={submit}>
        <div class="modal-body">
          <h2 id={`${id}-t`}>{p.title}</h2>
          {p.children}
          {p.acknowledge && (
            <label class="choice" style={{ marginTop: '16px' }}>
              <input type="checkbox" checked={ack} onChange={(e) => setAck(e.currentTarget.checked)} style={{ borderRadius: '3px' }} />
              <span class="c-title" style={{ fontWeight: 500 }}>{p.acknowledge}</span>
            </label>
          )}
          {p.typeToConfirm && (
            <div class="field" style={{ marginTop: '18px' }}>
              <label for={`${id}-i`}>{p.typeLabel ?? <>Type <code>{p.typeToConfirm}</code> to confirm</>}</label>
              <input id={`${id}-i`} class="input mono" value={typed} onInput={(e) => setTyped(e.currentTarget.value)} autoComplete="off" autoCapitalize="off" spellcheck={false} />
            </div>
          )}
          {p.error && (
            <div style={{ marginTop: '16px' }}>
              <Notice kind="bad" role="alert">
                <p>{p.error}</p>
              </Notice>
            </div>
          )}
        </div>
        <div class="modal-foot">
          <button type="button" class="btn ghost" onClick={p.onClose} disabled={p.busy}>
            {p.cancelLabel ?? 'Cancel'}
          </button>
          <button type="submit" class={`btn ${p.danger ? 'danger' : ''}`} disabled={!ready}>
            <Busy busy={!!p.busy}>{p.confirmLabel}</Busy>
          </button>
        </div>
      </form>
    </dialog>
  );
}

// ---- Toasts ----

interface Toast {
  id: number;
  kind: 'good' | 'bad' | 'info';
  text: string;
}
const toasts = createStore<{ list: Toast[] }>({ list: [] });
let toastId = 0;

export function toast(text: string, kind: Toast['kind'] = 'good'): void {
  const id = ++toastId;
  toasts.set((s) => ({ list: [...s.list, { id, kind, text }].slice(-3) }));
  setTimeout(() => toasts.set((s) => ({ list: s.list.filter((t) => t.id !== id) })), kind === 'bad' ? 8000 : 4500);
}

export function ToastHost() {
  const list = useStore(toasts, (s) => s.list);
  return (
    <div class="toast-host" role="status" aria-live="polite">
      {list.map((t) => (
        <div key={t.id} class={`toast ${t.kind}`}>
          {t.kind === 'bad' ? <I.CrossCircle /> : t.kind === 'good' ? <I.CheckCircle /> : <I.Info />}
          <span>{t.text}</span>
        </div>
      ))}
    </div>
  );
}

export function useNow(intervalMs = 1000): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), intervalMs);
    return () => clearInterval(t);
  }, [intervalMs]);
  return now;
}
