// Full-page states: loading, signed out, agent unreachable, left the cluster.
import { useState } from 'preact/hooks';
import { api, errorMessage } from '../api/client';
import { CodeBlock, Logo, Notice } from '../components/ui';
import { useStore } from '../lib/store';
import { refreshSetup, store } from '../state';

function Frame({ children }: { children: preact.ComponentChildren }) {
  return (
    <main class="state-page" id="main">
      <div class="state-card">
        <Logo />
        {children}
      </div>
    </main>
  );
}

export function LoadingPage() {
  return (
    <Frame>
      <h1 class="visually-hidden">Loading</h1>
      <div aria-busy="true" aria-live="polite" class="stack">
        <div class="skeleton" style={{ height: '44px', width: '70%' }} />
        <div class="skeleton" style={{ height: '18px', width: '90%' }} />
        <div class="skeleton" style={{ height: '18px', width: '60%' }} />
        <span class="visually-hidden">Loading the dashboard…</span>
      </div>
    </Frame>
  );
}

export function SignInPage() {
  const conn = useStore(store, (s) => s.conn);
  const err = useStore(store, (s) => s.signinError);
  const hadSession = useStore(store, (s) => s.status !== null || s.setup !== null);
  return (
    <Frame>
      <h1>{conn === 'signin_failed' ? 'That link did not work' : hadSession ? 'Your session ended' : 'Open the dashboard from CoyoteDB'}</h1>
      {conn === 'signin_failed' ? (
        <Notice kind="warn">
          <p>{err}</p>
        </Notice>
      ) : (
        <p class="lede">
          {hadSession
            ? 'For safety, a dashboard session lasts at most a day, and ends when CoyoteDB restarts.'
            : 'For safety, the dashboard only opens from a one-time link that CoyoteDB makes for you.'}
        </p>
      )}
      <p style={{ marginTop: '24px' }}>On this computer, open a terminal and run:</p>
      <CodeBlock title="Terminal" code="coyote dashboard" what="command">
        <span class="prompt">$ </span>coyote dashboard
      </CodeBlock>
      <p class="muted small" style={{ marginTop: '14px' }}>
        It opens your browser signed in. Each link works once, for two minutes, so nobody else can reuse it.
      </p>
    </Frame>
  );
}

export function UnreachablePage() {
  return (
    <Frame>
      <h1>CoyoteDB isn’t answering</h1>
      <p class="lede">This page comes from the CoyoteDB program on this computer, and it has stopped responding.</p>
      <div class="stack" style={{ '--gap': '12px', marginTop: '20px' }}>
        <p>Things to try:</p>
        <ul class="consequences">
          <li>Wait a moment: if it was restarting, this page reconnects by itself.</li>
          <li>
            Check that it is running: <code>coyote status</code>
          </li>
          <li>
            If you opened this on another device, check that the device can reach this computer (VPN on, same network).
          </li>
        </ul>
      </div>
      <p class="muted small" role="status">Trying again every few seconds…</p>
    </Frame>
  );
}

export function LeftPage() {
  const setup = useStore(store, (s) => s.setup);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const removed = setup?.state === 'removed';
  return (
    <Frame>
      <h1>{removed ? 'This computer was removed from its cluster' : 'This computer left its cluster'}</h1>
      <p class="lede">
        Its database is stopped. The data it had is still on this computer{setup?.removed_reason ? ` (${setup.removed_reason})` : ''}.
      </p>
      <p>You can set this computer up again: start a new database, or join a cluster with an invite code. The old data is moved aside, not deleted.</p>
      {err && (
        <Notice kind="bad" role="alert">
          <p>{err}</p>
        </Notice>
      )}
      <div class="btn-row" style={{ marginTop: '20px' }}>
        <button
          type="button"
          class="btn"
          disabled={busy}
          onClick={async () => {
            setBusy(true);
            setErr('');
            try {
              await api.resetSetup();
              await refreshSetup();
            } catch (e) {
              setErr(errorMessage(e));
            } finally {
              setBusy(false);
            }
          }}
        >
          Set up this computer again
        </button>
      </div>
    </Frame>
  );
}
