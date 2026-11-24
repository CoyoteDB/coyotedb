// App-wide state: session, setup state, live status, and the connection to
// the agent (SSE stream with a polling fallback).
import { api, ApiError, onConnectionEvent, setCsrf, UnreachableError } from './api/client';
import type { ActivityEvent, Setup, Status, VersionInfo } from './api/types';
import { createStore } from './lib/store';

export type Conn = 'booting' | 'ok' | 'unreachable' | 'unauthenticated' | 'signin_failed';

export interface AppState {
  conn: Conn;
  signinError: string;
  setup: Setup | null;
  status: Status | null;
  statusAt: number;
  version: VersionInfo | null;
  live: ActivityEvent[];
  stream: 'live' | 'polling' | 'off';
  /** The setup wizard is showing (kept on through its success screen). */
  wizard: boolean;
}

export const store = createStore<AppState>({
  conn: 'booting',
  signinError: '',
  setup: null,
  status: null,
  statusAt: 0,
  version: null,
  live: [],
  stream: 'off',
  wizard: false,
});

let es: EventSource | null = null;
let pollTimer: ReturnType<typeof setTimeout> | undefined;
let retryTimer: ReturnType<typeof setTimeout> | undefined;
let streamFailures = 0;

onConnectionEvent((kind) => {
  const s = store.get();
  if (kind === 'unauthenticated' && s.conn !== 'unauthenticated') {
    stopLive();
    store.set({ conn: 'unauthenticated' });
  } else if (kind === 'unreachable' && s.conn === 'ok') {
    store.set({ conn: 'unreachable' });
    scheduleRetry();
  }
});

function readLoginToken(): string | null {
  if (location.pathname !== '/login') return null;
  const m = /(?:^#|&)t=([^&]+)/.exec(location.hash);
  // Remove the token from the address bar and history straight away.
  history.replaceState(null, '', '/');
  return m ? decodeURIComponent(m[1]!) : null;
}

export async function boot(): Promise<void> {
  const token = readLoginToken();
  try {
    if (token) {
      try {
        const s = await api.signIn(token);
        setCsrf(s.csrf_token);
      } catch (err) {
        if (err instanceof ApiError) {
          store.set({
            conn: 'signin_failed',
            signinError:
              err.code === 'login_link_invalid'
                ? 'This sign-in link has expired or was already used. Each link works once, for two minutes.'
                : err.message,
          });
          return;
        }
        throw err;
      }
    } else {
      const s = await api.session();
      setCsrf(s.csrf_token);
    }
    // Load the version (and its feature list) with the first state, so
    // controls an agent does not serve never flash up.
    const version = api.version().catch(() => null);
    await refreshSetup();
    store.set({ conn: 'ok', version: await version });
    startLive();
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      store.set({ conn: 'unauthenticated' });
    } else if (err instanceof UnreachableError) {
      store.set({ conn: 'unreachable' });
      scheduleRetry();
    } else {
      store.set({ conn: 'unreachable' });
      scheduleRetry();
    }
  }
}

export async function refreshSetup(): Promise<Setup> {
  const setup = await api.setup();
  applySetup(setup);
  if (setup.state === 'member') await refreshStatus();
  return setup;
}

/**
 * Before this computer is in a cluster the agent serves only the setup
 * routes; every other /v1 route answers 409 not_set_up. That is not an
 * error to show: it means the setup state is out of date, so fetch it again
 * (once, without asking for the status again).
 */
export async function refreshStatus(): Promise<void> {
  try {
    const status = await api.status();
    store.set({ status, statusAt: Date.now() });
  } catch (err) {
    if (err instanceof ApiError && err.code === 'not_set_up') {
      applySetup(await api.setup());
      return;
    }
    throw err;
  }
}

const busy = (s: Setup | null | undefined) => s?.state === 'creating' || s?.state === 'joining';
let setupTimer: ReturnType<typeof setTimeout> | undefined;

/**
 * Takes a new setup state. When it turns "member", the agent starts serving
 * the whole API: load the status and open the stream again (a stream opened
 * before carries setup events only). While a create or join runs, the setup
 * is also asked for every few seconds, so the page never waits on a stream
 * that has stopped sending setup changes.
 */
export function applySetup(setup: Setup): void {
  const prev = store.get().setup;
  store.set({ setup });
  clearTimeout(setupTimer);
  if (busy(setup)) {
    setupTimer = setTimeout(() => {
      if (store.get().conn !== 'ok' || !busy(store.get().setup)) return;
      api.setup().then(applySetup, () => undefined);
    }, 3000);
  }
  if (setup.state === 'member' && prev && prev.state !== 'member') {
    void refreshStatus().catch(() => undefined);
    if (store.get().stream !== 'off') startLive();
  }
}

function scheduleRetry(delay = 3000): void {
  clearTimeout(retryTimer);
  retryTimer = setTimeout(() => {
    void (async () => {
      try {
        const s = await api.session();
        setCsrf(s.csrf_token);
        await refreshSetup();
        store.set({ conn: 'ok' });
        if (!store.get().version) void api.version().then((version) => store.set({ version }), () => undefined);
        startLive();
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) store.set({ conn: 'unauthenticated' });
        else scheduleRetry(Math.min(delay * 1.5, 15000));
      }
    })();
  }, delay);
}


export function startLive(): void {
  stopLive();
  if (typeof EventSource === 'undefined') {
    startPolling();
    return;
  }
  es = new EventSource('/v1/stream');
  es.addEventListener('open', () => {
    streamFailures = 0;
    clearTimeout(pollTimer);
    store.set({ stream: 'live' });
  });
  es.addEventListener('status', (e) => {
    if (store.get().setup?.state !== 'member') {
      // The agent serves status only to a member: the setup state here is out of date.
      api.setup().then(applySetup, () => undefined);
      return;
    }
    store.set({ status: JSON.parse((e as MessageEvent<string>).data) as Status, statusAt: Date.now() });
  });
  es.addEventListener('setup', (e) => applySetup(JSON.parse((e as MessageEvent<string>).data) as Setup));
  es.addEventListener('activity', (e) => {
    const ev = JSON.parse((e as MessageEvent<string>).data) as ActivityEvent;
    store.set((s) => ({ live: [ev, ...s.live.filter((x) => x.id !== ev.id)].slice(0, 200) }));
  });
  es.addEventListener('error', () => {
    streamFailures++;
    // The browser reconnects by itself; after a couple of failures, find out why.
    if (streamFailures >= 2 || es?.readyState === EventSource.CLOSED) {
      stopLive();
      void api.session().then(
        () => startPolling(),
        () => undefined, // the client emits unauthenticated / unreachable
      );
    }
  });
}

function startPolling(): void {
  clearTimeout(pollTimer);
  store.set({ stream: 'polling' });
  const tick = async () => {
    try {
      const setup = store.get().setup;
      if (setup?.state === 'member') await refreshStatus();
      else await refreshSetup();
    } catch {
      /* reported through onConnectionEvent */
    }
    if (store.get().conn === 'ok') {
      // Try the stream again every so often.
      if (++pollCount % 10 === 0) startLive();
      else pollTimer = setTimeout(() => void tick(), 3000);
    }
  };
  pollTimer = setTimeout(() => void tick(), 1500);
}
let pollCount = 0;

export function stopLive(): void {
  es?.close();
  es = null;
  clearTimeout(pollTimer);
  store.set({ stream: 'off' });
}

export async function signOut(): Promise<void> {
  try {
    await api.signOut();
  } finally {
    stopLive();
    store.set({ conn: 'unauthenticated', status: null, setup: null });
  }
}
