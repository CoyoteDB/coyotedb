// One problem with a stable code (API.md §5.1): the plain short line for
// everyone, and, opt-in, the long form for people who code or use an AI
// agent: what happened, the evidence, the exact commands, and a Markdown
// block to paste into an agent. Every word comes from the server.
import { useId, useState } from 'preact/hooks';
import type { Problem } from '../api/types';
import { navigate } from '../lib/router';
import { addressPrefill, paragraphs, suggestLabel } from '../lib/problems';
import { ago } from '../lib/format';
import * as I from './icons';
import { CopyButton } from './ui';

/** Opens Settings → Computers with the Change addresses dialog prefilled. */
export function openSuggested(p: Problem): void {
  if (!p.node || !p.address?.suggested?.length) return;
  addressPrefill.set({ req: { node: p.node, addresses: [...p.address.suggested] } });
  if (location.pathname !== '/settings') navigate('/settings');
}

export function ProblemBanner({ p, compact = false, now }: { p: Problem; compact?: boolean; now?: Date }) {
  const [open, setOpen] = useState(false);
  const id = useId();
  const use = suggestLabel(p);
  const cmd = p.commands?.[0];
  const Icon = p.severity === 'problem' ? I.Warn : I.Info;
  return (
    <div class={`problem ${p.severity === 'problem' ? 'bad' : 'warn'}${compact ? ' compact' : ''}`} data-code={p.code}>
      <Icon />
      <div class="p-body">
        <p class="p-sum">{p.summary}</p>
        {p.fix && <p class="p-fix">{p.fix}</p>}
        <div class="p-actions">
          {use && (
            <button type="button" class="btn small accent" onClick={() => openSuggested(p)} aria-label={`${use} for ${p.node}`}>
              {use}
            </button>
          )}
          {cmd && <CopyButton text={cmd} label="Copy command" what={`for ${p.node || 'this problem'}`} />}
          <button type="button" class="linkish p-toggle" aria-expanded={open} aria-controls={open ? id : undefined} onClick={() => setOpen(!open)}>
            {open ? 'Hide details' : 'Show details'}
          </button>
        </div>
        {open && (
          <div class="p-details" id={id}>
            <p class="p-meta">
              <span class="label">Details for the technical</span>
              <code class="p-code">{p.code}</code>
              {p.since && <span class="muted"> · since {ago(p.since, now ?? new Date())}</span>}
            </p>
            {paragraphs(p.details).map((t) => (
              <p key={t}>{t}</p>
            ))}
            {p.evidence && p.evidence.length > 0 && (
              <>
                <p class="p-h">What CoyoteDB saw</p>
                <ul class="p-evidence">
                  {p.evidence.map((e) => (
                    <li key={e}>{e}</li>
                  ))}
                </ul>
              </>
            )}
            {p.commands && p.commands.length > 0 && (
              <>
                <p class="p-h">{p.commands.length > 1 ? 'Commands to fix it' : 'Command to fix it'}</p>
                {p.commands.map((c) => (
                  <div class="code p-cmd" key={c}>
                    <pre>
                      <span class="prompt">$ </span>
                      {c}
                    </pre>
                    <CopyButton text={c} label="Copy" what={`command ${c}`} />
                  </div>
                ))}
              </>
            )}
            {p.markdown && (
              <div class="p-agent">
                <CopyButton text={p.markdown} label="Copy for your AI agent" />
                <span class="muted small">Copied text never includes invite codes or passwords.</span>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}

export function ProblemList({ problems, now }: { problems: Problem[]; now?: Date }) {
  if (problems.length === 0) return null;
  return (
    <section class="section problems" aria-label="Problems">
      {problems.map((p) => (
        <ProblemBanner key={p.code + (p.node ?? '')} p={p} now={now} />
      ))}
    </section>
  );
}
