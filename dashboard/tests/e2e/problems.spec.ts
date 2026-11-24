import { expect, test } from '@playwright/test';
import { axe, go, noOverflow, scenario, setTheme, shot, signIn, THEMES, watchConsole } from './helpers';

// Problems with a stable code (API.md §5.1): the plain line for everyone,
// the long form on request, and for an address that changed, one click to
// the Change addresses dialog with the new address filled in.

test.beforeEach(async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce' });
});

const SUMMARY = "node-b's address changed: it was 192.168.1.20, it is now 192.168.1.35.";

test('address_gone: banner, details, copy for an agent, Use the new address', async ({ page, request, context }) => {
  const problems = watchConsole(page);
  await context.grantPermissions(['clipboard-read', 'clipboard-write']);
  await scenario(request, 'address-changed');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page);

  const banner = page.locator('.problems .problem[data-code="address_gone"]');
  await expect(banner).toContainText(SUMMARY);
  await expect(banner).toContainText('The cluster still records 192.168.1.20');
  // The long form is opt-in.
  await expect(banner.getByText('What CoyoteDB saw')).toHaveCount(0);
  // Not said twice: the same sentence is not repeated under the technical details.
  await page.getByText('Technical details').click();
  await expect(page.locator('.consequences').getByText(SUMMARY, { exact: false })).toHaveCount(0);

  const toggle = banner.getByRole('button', { name: 'Show details' });
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  await toggle.click();
  await expect(banner.getByRole('button', { name: 'Hide details' })).toHaveAttribute('aria-expanded', 'true');
  await expect(banner.locator('.p-code')).toHaveText('address_gone');
  await expect(banner.locator('.p-evidence')).toContainText('Recorded for node-b: 192.168.1.20:7481');
  await expect(banner.locator('.p-evidence')).toContainText('node-b has now: 192.168.1.35:7481');
  await expect(banner.locator('.p-cmd pre')).toHaveText('$ coyote node set-address node-b 192.168.1.35:7481');
  await expect(banner.getByText('Copied text never includes invite codes or passwords.')).toBeVisible();
  await axe(page, 'problem banner open');
  await noOverflow(page, 'problem banner open');

  await banner.getByRole('button', { name: 'Copy for your AI agent' }).click();
  await expect(banner.getByRole('button', { name: 'Copy for your AI agent' })).toContainText('Copied');
  const md = await page.evaluate(() => navigator.clipboard.readText());
  expect(md).toContain('## CoyoteDB problem: `address_gone` on node-b');
  expect(md).toContain('coyote node set-address node-b 192.168.1.35:7481');
  expect(md).toContain('### Evidence');
  expect(md).not.toMatch(/coyote-[0-9a-z]{20,}|password=/i);

  await banner.getByRole('button', { name: 'Copy command for node-b' }).click();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe('coyote node set-address node-b 192.168.1.35:7481');

  // One click: Settings → Computers, the dialog filled in with the new address.
  await banner.getByRole('button', { name: 'Use 192.168.1.35 for node-b' }).click();
  await expect(page).toHaveURL(/\/settings$/);
  const dlg = page.getByRole('dialog');
  await expect(dlg).toBeVisible();
  await expect(dlg.getByLabel('Addresses (HOST:PORT)')).toHaveValue('192.168.1.35:7481');
  await dlg.getByRole('button', { name: 'Check and save' }).click();
  await expect(page.getByText('Other nodes now reach node-b at 192.168.1.35:7481.')).toBeVisible();
  const row = page.locator('li', { has: page.getByRole('button', { name: 'Change addresses of node-b' }) });
  await expect(row).toContainText('192.168.1.35:7481');
  await expect(page.locator('.problem[data-code="address_gone"]')).toHaveCount(0);
  await go(page, '/');
  await expect(page.locator('.problems')).toHaveCount(0);
  expect(problems).toEqual([]);
});

test('address_gone on the computer card, Use from Settings', async ({ page, request }) => {
  await scenario(request, 'address-changed');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/settings');
  const row = page.locator('li', { has: page.getByRole('button', { name: 'Change addresses of node-b' }) });
  const card = row.locator('.problem.compact');
  await expect(card).toContainText(SUMMARY);
  await expect(page.locator('li', { has: page.getByRole('button', { name: 'Change addresses of node-c' }) }).locator('.problem')).toHaveCount(0);
  await card.getByRole('button', { name: 'Use 192.168.1.35 for node-b' }).click();
  await expect(page.getByRole('dialog').getByLabel('Addresses (HOST:PORT)')).toHaveValue('192.168.1.35:7481');
  await page.getByRole('dialog').getByRole('button', { name: 'Cancel' }).click();
  // Opening the dialog by hand shows the recorded address, not the suggestion.
  await row.getByRole('button', { name: 'Change addresses of node-b' }).click();
  await expect(page.getByRole('dialog').getByLabel('Addresses (HOST:PORT)')).toHaveValue('192.168.1.20:7481');
  await axe(page, 'settings with address problem');
});

test('address_unreachable (one way): no Use button, says who cannot connect', async ({ page, request }) => {
  await scenario(request, 'address-one-way');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page);
  const banner = page.locator('.problems .problem[data-code="address_unreachable"]');
  await expect(banner).toContainText('node-a and node-b cannot connect to node-c at 192.168.1.23');
  await expect(banner).toContainText('only one direction works');
  await expect(banner.getByRole('button', { name: /^Use / })).toHaveCount(0);
  await banner.getByRole('button', { name: 'Show details' }).click();
  await expect(banner.locator('.p-code')).toHaveText('address_unreachable');
  await axe(page, 'address unreachable');
});

test('problem banners: screenshots, light and dark, wide and phone', async ({ page, request }) => {
  for (const theme of THEMES) {
    await setTheme(page, theme);
    for (const width of [1440, 390]) {
      await scenario(request, 'address-changed');
      await page.setViewportSize({ width, height: width > 800 ? 1000 : 844 });
      await signIn(page);
      const banner = page.locator('.problems .problem').first();
      await banner.getByRole('button', { name: 'Show details' }).click();
      await noOverflow(page, `overview ${width} ${theme}`);
      await shot(page, `problem-overview-${width}-${theme}`);
      await go(page, '/settings');
      await expect(page.locator('.problem.compact')).toBeVisible();
      await noOverflow(page, `settings ${width} ${theme}`);
      await page.locator('.problem.compact').scrollIntoViewIfNeeded();
      await page.waitForTimeout(150);
      await page.screenshot({ path: `screenshots/problem-settings-${width}-${theme}.png`, animations: 'disabled' });
    }
  }
});
