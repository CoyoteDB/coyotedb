// Overview: one glanceable answer, the live diagram, every computer, and
// what protection the current setup gives.
import { useMemo } from 'preact/hooks';
import * as I from '../components/icons';
import { NodeCard } from '../components/NodeCard';
import { NodeDiagram } from '../components/NodeDiagram';
import { ProblemList } from '../components/ProblemBanner';
import { A, LinkButton, useNow } from '../components/ui';
import type { BackupStatus } from '../api/types';
import { backupHeadline, OFF_CONSEQUENCE, stamp, whenText } from '../lib/backups';
import { hasFeature } from '../lib/features';
import { ago } from '../lib/format';
import { summarize, type HealthSummary, type SetupKind } from '../lib/health';
import { otherWarnings, problemsOf } from '../lib/problems';
import { useStore } from '../lib/store';
import { store } from '../state';

export function Home() {
  const status = useStore(store, (s) => s.status);
  const statusAt = useStore(store, (s) => s.statusAt);
  const stream = useStore(store, (s) => s.stream);
  const conn = useStore(store, (s) => s.conn);
  const version = useStore(store, (s) => s.version);
  const now = useNow(5000);
  const summary = useMemo(() => (status ? summarize(status, now) : null), [status, now]);

  if (!status || !summary) return <HomeSkeleton />;
  const warnings = otherWarnings(status);

  return (
    <>
      <ProblemList problems={problemsOf(status)} now={now} />
      <section class="status-hero" aria-labelledby="status-h">
        <div>
          <h1 id="status-h" class={`status-title t-${summary.tone}`}>
            <I.ToneIcon tone={summary.tone} />
            <span key={summary.tone}>{summary.label}</span>
          </h1>
          <p class="sentence" aria-live="polite">
            {summary.sentence}
          </p>
          {summary.details.length > 0 && (
            <ul class="details">
              {summary.details.map((d) => (
                <li key={d}>{d}</li>
              ))}
            </ul>
          )}
          {summary.actions.length > 0 && (
            <div class="btn-row" style={{ marginBottom: '18px' }}>
              {summary.actions.map((a) => (
                <LinkButton key={a.to + a.label} href={a.to} kind={a.primary ? 'accent' : 'ghost'}>
                  {a.label}
                  <I.Arrow />
                </LinkButton>
              ))}
            </div>
          )}
          <span class={`updated${stream === 'polling' ? ' polling' : ''}`}>
            <span class="dot" aria-hidden="true" />
            {conn === 'unreachable' ? `Last update ${ago(new Date(statusAt), now)}` : stream === 'live' ? 'Live' : `Updated ${ago(new Date(statusAt), now)}`}
          </span>
        </div>
        <NodeDiagram summary={summary} />
      </section>

      <section class="section" aria-labelledby="computers-h">
        <div class="panel-head">
          <h2 id="computers-h">Your computers</h2>
          <A href="/settings" class="linkish">
            Manage
          </A>
        </div>
        <div class="nodes">
          {summary.nodes.map((n) => (
            <NodeCard key={n.id} n={n} />
          ))}
        </div>
      </section>

      {status.backup && hasFeature(version, 'backups') && <BackupCard b={status.backup} now={now} />}

      <Protection summary={summary} />

      <details class="section" style={{ marginTop: '32px' }}>
        <summary class="linkish">Technical details</summary>
        <div class="panel quiet" style={{ marginTop: '12px' }}>
          <dl class="kv small">
            <dt>Cluster id</dt>
            <dd><code>{status.cluster.id}</code></dd>
            <dt>Term (epoch)</dt>
            <dd>{status.cluster.epoch}</dd>
            <dt>Main node</dt>
            <dd>{status.health.main_node}</dd>
            <dt>Coordinator</dt>
            <dd>{status.cluster.leader_name || 'none (no majority)'}</dd>
            <dt>Main node’s lease</dt>
            <dd>{status.health.lease}</dd>
            <dt>Writes</dt>
            <dd>{status.health.copies}</dd>
            <dt>Mode on one copy</dt>
            <dd>{status.cluster.single_data_node_mode === 'strict' ? 'Wait for the copy' : 'Carry on after a short wait'}</dd>
          </dl>
          {warnings.length > 0 && (
            <>
              <h3 class="section-title" style={{ marginTop: '16px' }}>Messages from CoyoteDB</h3>
              <ul class="consequences">
                {warnings.map((w) => (
                  <li key={w}>{w}</li>
                ))}
              </ul>
            </>
          )}
        </div>
      </details>
    </>
  );
}

/** One line about backups: healthy, last backup, or what needs attention. */
function BackupCard({ b, now }: { b: BackupStatus; now: Date }) {
  const h = backupHeadline(b, now);
  const fin = b.last_backup?.finished_at;
  let text: string;
  if (!b.enabled) text = OFF_CONSEQUENCE;
  else if (b.problem) text = b.problem;
  else if (fin && b.restorable) text = `Last full backup ${ago(fin, now)}. You can go back to any moment since ${stamp(b.restorable.from, now)}.`;
  else if (fin) text = `Last full backup ${ago(fin, now)}.`;
  else if (b.running) text = 'The first full backup is running now. Every change is already being saved.';
  else text = b.next_backup_at ? `The first full backup is taken ${whenText(b.next_backup_at, now)}. Every change is already being saved.` : 'Every change is already being saved.';
  return (
    <section class="section panel" aria-labelledby="bk-card-h">
      <div class="bk-card">
        <svg class={`p-icon t-${h.tone}`} viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round">
          <path d="M4 7.5c0-1.9 3.6-3.5 8-3.5s8 1.6 8 3.5-3.6 3.5-8 3.5-8-1.6-8-3.5z" />
          <path d="M4 7.5v9c0 1.9 3.6 3.5 8 3.5 1 0 2-.1 2.9-.2M20 7.5V12" />
          <path d="M4 12c0 1.9 3.6 3.5 8 3.5 1.1 0 2.1-.1 3-.3" />
          {h.tone === 'ok' && <path d="M15.5 18l2 2 4-4.5" />}
          {h.tone === 'attention' && <path d="M19 15v3M19 20.6v.1" />}
          {h.tone === 'pending' && <path d="M19 15.5v2.2l1.4.9" />}
          {h.tone === 'off' && <path d="M16.5 15.5l4.5 4.5M21 15.5L16.5 20" />}
        </svg>
        <div>
          <span class="label">Backups · {h.short}</span>
          <h2 id="bk-card-h">{h.title.replace(/\.$/, '')}</h2>
          <p>{text}</p>
        </div>
        <LinkButton href="/backups" kind={h.tone === 'attention' || h.tone === 'off' ? 'accent' : 'ghost'}>
          {h.tone === 'off' ? 'Turn on' : 'Backups'}
          <I.Arrow />
        </LinkButton>
      </div>
    </section>
  );
}

const SETUPS: { id: SetupKind[]; title: string; text: string; recommended?: boolean }[] = [
  { id: ['one', 'one_witness'], title: '1 computer', text: 'No copy. If it stops, the database stops.' },
  { id: ['two'], title: '2 computers', text: 'A live copy. If the main one stops, you press Switch over.' },
  { id: ['two_witness'], title: '2 + a witness', text: 'Switch-over happens by itself. The witness can be any small computer.', recommended: true },
  { id: ['three_plus'], title: '3 or more', text: 'Switch-over happens by itself, and there are two copies.', recommended: true },
];

function Protection({ summary }: { summary: HealthSummary }) {
  const p = summary.protection;
  return (
    <section class="section panel" aria-labelledby="protect-h">
      <div class="protection">
        <svg class="p-icon" viewBox="0 0 24 24" aria-hidden="true" fill="none" stroke="currentColor" stroke-width="1.5">
          <path d="M12 3l7.5 3v5.5c0 4.6-3.2 8.2-7.5 9.5-4.3-1.3-7.5-4.9-7.5-9.5V6z" />
          {p.kind === 'automatic' && <path d="M8.5 12l2.5 2.5 4.5-5" />}
          {p.kind === 'manual' && <path d="M12 8v5M12 15.8v.1" />}
          {p.kind === 'stops' && <path d="M9.5 9.5l5 5M14.5 9.5l-5 5" />}
        </svg>
        <div>
          <span class="label">If a computer stops</span>
          <h2 id="protect-h">
            {p.kind === 'automatic' ? 'Another takes over by itself' : p.kind === 'manual' ? 'You press Switch over' : 'The database stops'}
          </h2>
          <p class="ink2" style={{ marginBottom: 0 }}>{p.text}</p>
          {p.recommend && (
            <p style={{ marginTop: '10px', marginBottom: 0 }}>
              <strong>Recommended:</strong> {p.recommend} <A href="/add">Add a computer →</A>
            </p>
          )}
        </div>
      </div>
      <div class="setups" role="list" aria-label="Setups and what they give">
        {SETUPS.map((s) => {
          const current = s.id.includes(summary.setup);
          return (
            <div key={s.title} role="listitem" class={`${current ? 'current' : ''} ${s.recommended ? 'recommended' : ''}`} aria-current={current ? 'true' : undefined}>
              <span class="label">{current ? 'You are here' : s.recommended ? 'Recommended' : ' '}</span>
              <div class="s-title">{s.title}</div>
              <p>{s.text}</p>
            </div>
          );
        })}
      </div>
    </section>
  );
}

function HomeSkeleton() {
  return (
    <div aria-busy="true">
      <h1 class="visually-hidden">Loading status</h1>
      <div class="status-hero">
        <div class="stack">
          <div class="skeleton" style={{ height: '38px', width: '180px' }} />
          <div class="skeleton" style={{ height: '28px', width: '90%' }} />
          <div class="skeleton" style={{ height: '18px', width: '70%' }} />
        </div>
        <div class="skeleton" style={{ height: '200px' }} />
      </div>
      <div class="nodes">
        <div class="skeleton" style={{ height: '220px' }} />
        <div class="skeleton" style={{ height: '220px' }} />
      </div>
      <span class="visually-hidden" role="status">Loading the status…</span>
    </div>
  );
}
