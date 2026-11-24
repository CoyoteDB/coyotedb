import type { ComponentChildren } from 'preact';
import { useEffect, useMemo, useState } from 'preact/hooks';
import * as I from './components/icons';
import { A, Logo, StatusPill, ToastHost } from './components/ui';
import { summarize } from './lib/health';
import { hasFeature } from './lib/features';
import { linkClick, navigate, usePath } from './lib/router';
import { useStore } from './lib/store';
import { ThemeToggle } from './lib/theme';
import { Activity } from './screens/Activity';
import { AddComputer } from './screens/AddComputer';
import { Agents } from './screens/Agents';
import { Backups } from './screens/Backups';
import { Connect } from './screens/Connect';
import { Home } from './screens/Home';
import { Settings } from './screens/Settings';
import { Setup } from './screens/Setup';
import { LeftPage, LoadingPage, SignInPage, UnreachablePage } from './screens/States';
import { SwitchOver } from './screens/SwitchOver';
import { signOut, store } from './state';
import { DevBar } from './dev';

const NAV: { href: string; label: string; soon?: boolean }[] = [
  { href: '/', label: 'Overview' },
  { href: '/add', label: 'Add a computer' },
  { href: '/connect', label: 'Connect your app' },
  { href: '/switch', label: 'Switch over' },
  { href: '/activity', label: 'Activity' },
  { href: '/settings', label: 'Computers & settings' },
];
const NAV_BACKUPS = { href: '/backups', label: 'Backups' };
const NAV_LATER: { href: string; label: string }[] = [
  { href: '/backups', label: 'Backups' },
  { href: '/agents', label: 'Coding agents' },
];

export function App() {
  const conn = useStore(store, (s) => s.conn);
  const setup = useStore(store, (s) => s.setup);
  const wizard = useStore(store, (s) => s.wizard);
  const hasState = setup !== null;

  let page: ComponentChildren;
  if (conn === 'booting') page = <LoadingPage />;
  else if (conn === 'unauthenticated' || conn === 'signin_failed') page = <SignInPage />;
  else if (conn === 'unreachable' && !hasState) page = <UnreachablePage />;
  else if (!setup) page = <LoadingPage />;
  else if (wizard || setup.state === 'fresh' || setup.state === 'creating' || setup.state === 'joining' || setup.state === 'failed') page = <Setup />;
  else if (setup.state === 'left' || setup.state === 'removed') page = <LeftPage />;
  else page = <Shell />;

  return (
    <>
      {page}
      <ToastHost />
      {import.meta.env.DEV && <DevBar />}
    </>
  );
}

function Route({ path }: { path: string }) {
  switch (path) {
    case '/':
      return <Home />;
    case '/add':
      return <AddComputer />;
    case '/connect':
      return <Connect />;
    case '/switch':
      return <SwitchOver />;
    case '/activity':
      return <Activity />;
    case '/settings':
      return <Settings />;
    case '/backups':
      return <Backups />;
    case '/agents':
      return <Agents />;
    default:
      return (
        <div class="page-head">
          <h1>Nothing here</h1>
          <p>
            This page does not exist. <A href="/">Go to the overview</A>.
          </p>
        </div>
      );
  }
}

function Shell() {
  const path = usePath();
  const status = useStore(store, (s) => s.status);
  const conn = useStore(store, (s) => s.conn);
  const version = useStore(store, (s) => s.version);
  const [menu, setMenu] = useState(false);
  const summary = useMemo(() => (status ? summarize(status) : null), [status]);
  const backupsOn = hasFeature(version, 'backups');
  // Backups sits with the main pages once this agent serves them.
  const nav = backupsOn ? [...NAV.slice(0, 3), NAV_BACKUPS, ...NAV.slice(3)] : NAV;
  const later = backupsOn ? NAV_LATER.filter((n) => n.href !== '/backups') : NAV_LATER;

  useEffect(() => setMenu(false), [path]);
  useEffect(() => {
    if (!menu) return;
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && setMenu(false);
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [menu]);
  useEffect(() => {
    const t = summary ? `${summary.label} · ${status?.cluster.name || 'CoyoteDB'}` : 'CoyoteDB';
    document.title = t;
  }, [summary, status]);

  const clusterName = status?.cluster.name || 'Your database';
  return (
    <>
      <a class="skip-link" href="#main">Skip to content</a>
      <header class="topbar">
        <a class="brand" href="/" onClick={linkClick} aria-label="CoyoteDB overview">
          <Logo />
        </a>
        <span class="cluster-name">{clusterName}</span>
        <span class="spacer" />
        {summary && (
          <a href="/" onClick={linkClick} style={{ textDecoration: 'none' }} aria-label={`Status: ${summary.label}`}>
            <StatusPill tone={summary.tone} />
          </a>
        )}
        <ThemeToggle />
        <button type="button" class="icon-btn menu-btn" aria-expanded={menu} aria-controls="sidenav" aria-label={menu ? 'Close menu' : 'Open menu'} onClick={() => setMenu(!menu)}>
          {menu ? <I.Cross /> : <I.Menu />}
        </button>
      </header>
      {conn === 'unreachable' && (
        <div class="banner bad" role="alert">
          <I.Plug />
          <span>
            Lost contact with CoyoteDB on this computer. Showing the last thing it said; trying again every few seconds.
          </span>
        </div>
      )}
      <div class="frame">
        {menu && <div class="nav-backdrop" onClick={() => setMenu(false)} />}
        <nav id="sidenav" class={`sidenav${menu ? ' open' : ''}`} aria-label="Main">
          <ul>
            {nav.map((n) => (
              <li key={n.href}>
                <a class="navlink" href={n.href} onClick={linkClick} aria-current={path === n.href ? 'page' : undefined}>
                  {n.label}
                </a>
              </li>
            ))}
          </ul>
          <div class="group-label label">Coming later</div>
          <ul>
            {later.map((n) => (
              <li key={n.href}>
                <a class="navlink" href={n.href} onClick={linkClick} aria-current={path === n.href ? 'page' : undefined}>
                  {n.label}
                  <span class="soon">Later</span>
                </a>
              </li>
            ))}
          </ul>
          <div class="navfoot">
            <button type="button" class="linkish" onClick={() => void signOut().then(() => navigate('/', true))}>
              Sign out
            </button>
            {version && <span class="label">CoyoteDB {version.version}</span>}
          </div>
        </nav>
        <main id="main" class="page">
          <Route path={path} />
        </main>
      </div>
    </>
  );
}
