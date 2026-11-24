import type { ComponentChildren } from 'preact';
import type { NodeSummary } from '../lib/health';
import { platformShort } from '../lib/format';
import { ROLE_WORD } from '../lib/words';
import * as I from './icons';

function KindIcon({ n }: { n: NodeSummary }) {
  if (n.condition === 'down') {
    return (
      <svg class="nc-icon" viewBox="0 0 28 28" aria-hidden="true">
        <circle cx="14" cy="14" r="12" fill="none" stroke="var(--down)" stroke-width="1.5" stroke-dasharray="4 3" />
        <path d="M10 10l8 8M18 10l-8 8" stroke="var(--down)" stroke-width="1.75" stroke-linecap="round" />
      </svg>
    );
  }
  if (n.kind === 'witness') {
    return (
      <svg class="nc-icon" viewBox="0 0 28 28" aria-hidden="true">
        <circle cx="14" cy="14" r="9" fill="none" stroke="var(--ink-2)" stroke-width="1.5" stroke-dasharray="3 2.5" />
      </svg>
    );
  }
  return (
    <svg class="nc-icon" viewBox="0 0 28 28" aria-hidden="true">
      <circle cx="14" cy="14" r="12" fill={n.kind === 'main' ? 'var(--accent)' : 'var(--ink)'} />
    </svg>
  );
}

function condClass(n: NodeSummary): string {
  if (n.condition === 'down') return 'c-down';
  if (n.condition === 'attention') return 'c-bad';
  if (n.condition === 'ok' || n.condition === 'copying' || n.condition === 'starting') return 'c-ok';
  return 'c-warn';
}

function CondIcon({ n }: { n: NodeSummary }) {
  switch (n.condition) {
    case 'down':
      return <I.CrossCircle />;
    case 'ok':
      return <I.CheckCircle />;
    case 'copying':
    case 'rejoining':
    case 'starting':
      return <I.Spinner />;
    default:
      return <I.Warn />;
  }
}

export function NodeCard({ n, children }: { n: NodeSummary; children?: ComponentChildren }) {
  const m = n.member;
  return (
    <article class={`node-card ${n.kind}${n.condition === 'down' ? ' down' : n.condition === 'attention' ? ' attention' : ''}`} aria-label={`${n.name}, ${ROLE_WORD[n.kind].toLowerCase()}`}>
      <div class="nc-head">
        <KindIcon n={n} />
        <div style={{ minWidth: 0 }}>
          <div class="nc-name">{n.name}</div>
          <span class="nc-role label">{ROLE_WORD[n.kind]}</span>
        </div>
        {n.isThis && <span class="this-tag">This computer</span>}
      </div>
      <div class={`nc-cond ${condClass(n)}`}>
        <CondIcon n={n} />
        <span>{n.conditionText}</span>
      </div>
      {n.condition === 'copying' && n.progress != null && (
        <div class="meter" role="progressbar" aria-valuenow={n.progress} aria-valuemin={0} aria-valuemax={100} aria-label={`${n.name} copy progress`}>
          <i style={{ width: `${n.progress}%` }} />
        </div>
      )}
      {n.note && <p class="nc-note">{n.note}</p>}
      <dl class="kv">
        <dt>Last contact</dt>
        <dd>{n.lastContact}</dd>
        {n.kind === 'copy' && (
          <>
            <dt>Behind</dt>
            <dd>{n.condition === 'down' ? 'unknown' : n.lag}</dd>
          </>
        )}
        <dt>System</dt>
        <dd>{platformShort(m.platform)}</dd>
        {m.addresses[0] && (
          <>
            <dt>Address</dt>
            <dd><code>{m.addresses[0].replace(/:\d+$/, '')}</code></dd>
          </>
        )}
      </dl>
      {children}
    </article>
  );
}
