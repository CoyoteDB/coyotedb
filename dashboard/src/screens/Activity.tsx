// Activity: what happened, in plain words, newest first.
import { useEffect, useMemo, useState } from 'preact/hooks';
import { api, errorMessage } from '../api/client';
import type { ActivityEvent } from '../api/types';
import * as I from '../components/icons';
import { Busy, Notice, useNow } from '../components/ui';
import { useStore } from '../lib/store';
import { eventText } from '../lib/words';
import { store } from '../state';

function SevIcon({ s }: { s: ActivityEvent['severity'] }) {
  if (s === 'good') return <I.CheckCircle />;
  if (s === 'warning') return <I.Warn />;
  if (s === 'problem') return <I.CrossCircle />;
  return <I.Dot />;
}

const SEV_WORD = { good: 'Good news', warning: 'Warning', problem: 'Problem', info: 'Note' } as const;

function dayLabel(d: Date, now: Date): string {
  const day = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime();
  const diff = Math.round((day(now) - day(d)) / 86_400_000);
  if (diff === 0) return 'Today';
  if (diff === 1) return 'Yesterday';
  return d.toLocaleDateString(undefined, { weekday: 'long', day: 'numeric', month: 'long' });
}

export function Activity() {
  const live = useStore(store, (s) => s.live);
  const [events, setEvents] = useState<ActivityEvent[] | null>(null);
  const [more, setMore] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const [seen] = useState(() => new Set(live.map((e) => e.id)));
  const now = useNow(60000);

  useEffect(() => {
    api.events().then(
      (p) => {
        setEvents(p.events);
        setMore(p.more);
      },
      (e) => setErr(errorMessage(e)),
    );
  }, []);

  const all = useMemo(() => {
    const m = new Map<string, ActivityEvent>();
    for (const e of [...live, ...(events ?? [])]) m.set(e.id, e);
    return [...m.values()].sort((a, b) => (a.id < b.id ? 1 : a.id > b.id ? -1 : 0));
  }, [live, events]);

  const loadMore = async () => {
    const last = events?.[events.length - 1];
    if (!last) return;
    setBusy(true);
    try {
      const p = await api.events(last.id);
      setEvents([...(events ?? []), ...p.events]);
      setMore(p.more);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  let lastDay = '';
  return (
    <>
      <div class="page-head">
        <h1>Activity</h1>
        <p>What happened to your database and computers, newest first. New events appear here as they happen.</p>
      </div>
      {err && <Notice kind="bad" title="Could not load the activity"><p>{err}</p></Notice>}
      {!events && !err && <div class="skeleton" style={{ height: '240px' }} aria-label="Loading" />}
      {events && all.length === 0 && (
        <div class="empty">
          <h3>Nothing has happened yet</h3>
          <p style={{ margin: 0 }}>Events like a computer joining, stopping or taking over show up here.</p>
        </div>
      )}
      {all.length > 0 && (
        <section class="panel" aria-label="Events">
          <ul class="timeline">
            {all.map((e) => {
              const d = new Date(e.at);
              const dl = dayLabel(d, now);
              const head = dl !== lastDay;
              lastDay = dl;
              return [
                head ? <li key={`d-${dl}`} class="t-day label" aria-hidden="false">{dl}</li> : null,
                <li key={e.id} class={live.some((x) => x.id === e.id) && !seen.has(e.id) ? 'new' : ''}>
                  <span class={`t-icon ${e.severity}`} role="img" aria-label={SEV_WORD[e.severity]}><SevIcon s={e.severity} /></span>
                  <span>{eventText(e)}</span>
                  <time class="t-when" dateTime={e.at} title={d.toLocaleString()}>
                    {d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })}
                  </time>
                </li>,
              ];
            })}
          </ul>
          {more && (
            <div class="btn-row" style={{ marginTop: '16px' }}>
              <button type="button" class="btn ghost small" onClick={loadMore} disabled={busy}>
                <Busy busy={busy}>Show older</Busy>
              </button>
            </div>
          )}
        </section>
      )}
    </>
  );
}
