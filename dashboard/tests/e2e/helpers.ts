import AxeBuilder from '@axe-core/playwright';
import { expect, type APIRequestContext, type Page } from '@playwright/test';
import { mkdirSync } from 'node:fs';
import { join } from 'node:path';

export const WIDTHS = [360, 768, 1440] as const;
export const THEMES = ['light', 'dark'] as const;
export type Theme = (typeof THEMES)[number];

const SHOTS = join(import.meta.dirname, '..', '..', 'screenshots');
mkdirSync(SHOTS, { recursive: true });

export async function scenario(request: APIRequestContext, id: string): Promise<void> {
  const r = await request.post('/__mock/scenario', { data: { id } });
  expect(r.ok()).toBeTruthy();
}

export async function codes(request: APIRequestContext): Promise<{ valid: string; expired: string; garbled: string; messy: string }> {
  return (await request.get('/__mock/codes')).json();
}

/** Fails the test on any CSP violation or uncaught error. */
export function watchConsole(page: Page): string[] {
  const problems: string[] = [];
  page.on('console', (m) => {
    const t = m.text();
    if (/Content Security Policy|Refused to/i.test(t)) problems.push(t);
  });
  page.on('pageerror', (e) => problems.push(String(e)));
  return problems;
}

export async function setTheme(page: Page, theme: Theme): Promise<void> {
  await page.addInitScript((t) => {
    try {
      localStorage.setItem('coyote-theme', t);
    } catch {
      /* ignore */
    }
  }, theme);
}

export async function signIn(page: Page, path = '/'): Promise<void> {
  const link = (await (await page.request.get('/__mock/login-link')).json()) as { url: string };
  const u = new URL(link.url);
  await page.goto(u.pathname + u.hash);
  await page.waitForFunction(() => location.pathname === '/' && location.hash === '');
  if (path !== '/') await go(page, path);
}

/** In-app navigation (keeps the session and the live stream). */
export async function go(page: Page, path: string): Promise<void> {
  await page.evaluate((p) => {
    history.pushState(null, '', p);
    dispatchEvent(new PopStateEvent('popstate'));
  }, path);
}

export async function shot(page: Page, name: string): Promise<void> {
  // Let animations settle; motion is reduced in screenshots anyway.
  await page.waitForTimeout(150);
  await page.screenshot({ path: join(SHOTS, `${name}.png`), fullPage: true, animations: 'disabled' });
}

/** WCAG 2.1 AA automated checks (axe-core). */
export async function axe(page: Page, label: string): Promise<void> {
  const r = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze();
  const bad = r.violations.filter((v) => v.impact === 'serious' || v.impact === 'critical');
  expect(bad.map((v) => `${label}: ${v.id} — ${v.help} (${v.nodes.slice(0, 3).map((n) => n.target.join(' ')).join(' | ')})`)).toEqual([]);
}

/** No element wider than the viewport (no sideways scrolling on phones). */
export async function noOverflow(page: Page, label: string): Promise<void> {
  const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(over, `${label} scrolls sideways by ${over}px`).toBeLessThanOrEqual(0);
}
