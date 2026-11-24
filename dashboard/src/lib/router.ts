// A tiny History API router. The agent serves index.html for every
// non-/v1 path, so deep links and reloads work.
import { useEffect, useState } from 'preact/hooks';

const subs = new Set<() => void>();

export function navigate(to: string, replace = false): void {
  if (to === location.pathname + location.search) return;
  if (replace) history.replaceState(null, '', to);
  else history.pushState(null, '', to);
  for (const fn of subs) fn();
  // Move focus to the new page's heading for screen readers and keyboard users.
  requestAnimationFrame(() => {
    const h = document.querySelector<HTMLElement>('main h1');
    if (h) {
      h.setAttribute('tabindex', '-1');
      h.focus({ preventScroll: true });
    }
    window.scrollTo({ top: 0 });
  });
}

window.addEventListener('popstate', () => {
  for (const fn of subs) fn();
});

export function usePath(): string {
  const [p, setP] = useState(location.pathname);
  useEffect(() => {
    const fn = () => setP(location.pathname);
    subs.add(fn);
    fn(); // catch a change made between the first render and now
    return () => {
      subs.delete(fn);
    };
  }, []);
  return p;
}

/** onClick for <a href> that should route in-app. */
export function linkClick(e: MouseEvent): void {
  const a = (e.currentTarget as HTMLAnchorElement | null) ?? null;
  if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const href = a.getAttribute('href');
  if (!href || !href.startsWith('/')) return;
  e.preventDefault();
  navigate(href);
}
