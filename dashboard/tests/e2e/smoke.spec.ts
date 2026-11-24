import { expect, test, type Page } from '@playwright/test';
import { axe, codes, go, noOverflow, scenario, setTheme, shot, signIn, THEMES, watchConsole, WIDTHS } from './helpers';

test.beforeEach(async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce' });
});

// ---------------------------------------------------------------- wizard

/** Records every /v1 answer of 400 or more (the setup page must not cause any). */
function watchErrors(page: Page): string[] {
  const failed: string[] = [];
  page.on('response', (r) => {
    if (r.url().includes('/v1/') && r.status() >= 400) failed.push(`${r.status()} ${r.request().method()} ${new URL(r.url()).pathname}`);
  });
  return failed;
}

async function pasteAndContinue(page: Page, code: string, name?: string): Promise<void> {
  await page.getByLabel('Invite code').fill(code);
  await expect(page.getByText(/Valid invite, expires in/)).toBeVisible();
  await page.getByRole('button', { name: 'Continue' }).click();
  if (name) await page.getByLabel('Computer name').fill(name);
  await page.getByRole('button', { name: 'Continue' }).click();
}

test('wizard: start a new database, then make an invite', async ({ page, request }) => {
  const problems = watchConsole(page);
  const failed = watchErrors(page);
  let streams = 0;
  page.on('request', (r) => {
    if (new URL(r.url()).pathname === '/v1/stream') streams++;
  });
  await scenario(request, 'fresh');
  await page.setViewportSize({ width: 1440, height: 900 });
  await signIn(page);
  await expect(page.getByRole('heading', { name: 'Let’s set up this computer' })).toBeVisible();
  await axe(page, 'wizard choose');
  await shot(page, 'wizard-1-choose-1440-light');
  await page.getByRole('button', { name: /Start a new database/ }).click();

  await expect(page.getByRole('heading', { name: 'Name this computer' })).toBeVisible();
  const name = page.getByLabel('Computer name');
  await name.fill('Office Mini!');
  await name.blur();
  await expect(name).toHaveValue('office-mini');
  await page.getByLabel(/Name for the database/).fill('Bakery orders');
  await shot(page, 'wizard-2-name-1440-light');
  await page.getByRole('button', { name: 'Continue' }).click();

  // Backups: on by default, in a folder next to the data, with the real path.
  await expect(page.getByRole('heading', { name: 'Keep backups of your data' })).toBeVisible();
  await expect(page.getByRole('radio', { name: /On, in a folder on this computer/ })).toBeChecked();
  await expect(page.getByText('/var/lib/coyote/node-a-backups')).toBeVisible();
  await page.getByRole('button', { name: 'Continue' }).click();

  await expect(page.getByRole('heading', { name: /How will the other computers reach this one/ })).toBeVisible();
  await expect(page.getByRole('radio', { name: /Tailscale \(100\.64\.0\.44\)/ })).toBeChecked();
  await axe(page, 'wizard address');
  await shot(page, 'wizard-3-address-1440-light');
  await page.getByRole('button', { name: 'Start the database' }).click();

  await expect(page.getByRole('heading', { name: 'Setting up your database' })).toBeVisible();
  await expect(page.getByText('Downloading PostgreSQL').first()).toBeVisible();
  await shot(page, 'wizard-4-progress-1440-light');
  await expect(page.getByRole('heading', { name: 'Your database is running' })).toBeVisible({ timeout: 20_000 });
  await page.getByRole('button', { name: /Make an invite code/ }).click();
  await expect(page.getByRole('textbox', { name: 'Invite code' })).toContainText('coyote-');
  await expect(page.getByText(/Works once · expires in/)).toBeVisible();
  await axe(page, 'wizard success');
  await shot(page, 'wizard-5-success-1440-light');

  await page.getByRole('button', { name: /Go to the dashboard/ }).click();
  await expect(page.getByRole('heading', { level: 1 })).toContainText('All good');
  await expect(page.locator('.topbar')).toContainText('Bakery orders');
  // The setup page's stream carries setup events only: a member opens a new one.
  expect(streams).toBeGreaterThanOrEqual(2);
  expect(failed).toEqual([]);
  expect(problems).toEqual([]);
});

test('wizard: join with a pasted code (spaces, line breaks, quotes)', async ({ page, request }) => {
  const problems = watchConsole(page);
  const failed = watchErrors(page);
  await scenario(request, 'fresh');
  const c = await codes(request);
  await page.setViewportSize({ width: 360, height: 780 });
  await signIn(page);
  await page.getByRole('button', { name: /Join with an invite code/ }).click();
  await page.getByLabel('Invite code').fill(c.messy);
  await expect(page.getByText(/Valid invite, expires in/)).toBeVisible();
  await expect(page.getByText('The cluster answered at 100.64.0.41:7481. It is the cluster this code was made for.')).toBeVisible();
  // Today's agent cannot learn the cluster's system, so nothing is claimed about it.
  await expect(page.getByText(/Must run Linux/)).toHaveCount(0);
  await expect(page.getByRole('radio', { name: /A data computer/ })).toBeChecked();
  await noOverflow(page, 'join invite 360');
  await axe(page, 'join invite');
  await shot(page, 'wizard-join-1-code-360-light');
  await page.getByRole('button', { name: 'Continue' }).click();
  await page.getByLabel('Computer name').fill('node-b');
  await page.getByRole('button', { name: 'Continue' }).click();
  await page.getByRole('button', { name: 'Join and copy the data' }).click();
  await expect(page.getByRole('heading', { name: 'Joining the cluster' })).toBeVisible();
  // The agent's order: the program files first, then the cluster.
  const labels = await page.locator('.steps .s-label').allInnerTexts();
  expect(labels.map((t) => t.trim())).toEqual(['Downloading PostgreSQL', 'Checking the download', 'Contacting the cluster', 'Copying the data', 'Following live']);
  await expect(page.getByRole('progressbar', { name: 'Copying the data' })).toBeVisible({ timeout: 15_000 });
  await shot(page, 'wizard-join-2-copying-360-light');
  await expect(page.getByRole('heading', { name: 'This computer is now a copy' })).toBeVisible({ timeout: 30_000 });
  await shot(page, 'wizard-join-3-done-360-light');
  await page.getByRole('button', { name: /Go to the dashboard/ }).click();
  await expect(page.getByRole('heading', { level: 1 })).toContainText('All good');
  expect(failed).toEqual([]);
  expect(problems).toEqual([]);
});

test('wizard: garbled, expired and already-used invite codes; try again keeps the code; start over', async ({ page, request }) => {
  await scenario(request, 'invite-bad');
  const c = await codes(request);
  await page.setViewportSize({ width: 768, height: 1000 });
  await signIn(page);
  await page.getByRole('button', { name: /Join with an invite code/ }).click();
  const box = page.getByLabel('Invite code');

  await box.fill('hello there');
  await expect(page.getByText(/doesn’t look like an invite code/)).toBeVisible();
  await box.fill(c.garbled);
  await expect(page.getByText(/Part of the code is missing or mistyped/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Continue' })).toBeDisabled();
  await shot(page, 'wizard-invite-garbled-768-light');
  await box.fill(c.expired);
  await expect(page.getByText(/This invite code has expired/)).toBeVisible();
  await shot(page, 'wizard-invite-expired-768-light');

  await pasteAndContinue(page, c.valid);
  await page.getByRole('button', { name: 'Join and copy the data' }).click();
  await expect(page.getByRole('heading', { name: 'Joining did not finish' })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText(/already used by another computer/)).toBeVisible();
  await expect(page.getByText(/Make a new invite on a computer in the cluster/)).toBeVisible();
  await axe(page, 'join failed');
  await shot(page, 'wizard-invite-used-768-light');

  await page.getByRole('button', { name: 'Try again' }).click();
  await expect(page.getByRole('heading', { name: 'Paste the invite code' })).toBeVisible();
  await expect(box).toHaveValue(c.valid);
  await page.getByRole('button', { name: 'Continue' }).click();
  await page.getByRole('button', { name: 'Continue' }).click();
  await page.getByRole('button', { name: 'Join and copy the data' }).click();
  await expect(page.getByRole('heading', { name: 'Joining did not finish' })).toBeVisible({ timeout: 15_000 });
  // The program files stayed from the first try.
  await expect(page.locator('.steps li', { hasText: 'Downloading PostgreSQL' })).toContainText('Already on this computer');
  await page.getByRole('button', { name: 'Start over' }).click();
  await expect(page.getByRole('heading', { name: 'Let’s set up this computer' })).toBeVisible();
});

test('wizard: a different kind of computer: join anyway, then join as a witness', async ({ page, request }) => {
  const problems = watchConsole(page);
  await scenario(request, 'platform-mismatch');
  const c = await codes(request);
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page);
  await page.getByRole('button', { name: /Join with an invite code/ }).click();
  await pasteAndContinue(page, c.valid);
  await page.getByRole('button', { name: 'Join and copy the data' }).click();
  await expect(page.getByRole('heading', { name: 'Joining did not finish' })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText(/different kind \(system or processor\)/)).toBeVisible();
  await page.getByText('What CoyoteDB reported').click();
  await expect(page.getByText(/darwin\/arm64/)).toBeVisible();
  await axe(page, 'mixed os off');
  await shot(page, 'wizard-mixed-os-off-1440-light');

  await page.getByRole('button', { name: 'Join anyway (the cluster checks this computer first)' }).click();
  await expect(page.getByRole('heading', { name: 'Joining did not finish' })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText(/This computer cannot hold a copy of the data/)).toBeVisible({ timeout: 15_000 });
  await expect(page.getByRole('button', { name: /Join anyway/ })).toHaveCount(0);
  await shot(page, 'wizard-platform-mismatch-1440-light');
  await page.getByRole('button', { name: 'Join as a witness instead' }).click();
  await expect(page.getByRole('heading', { name: 'This computer is now a witness' })).toBeVisible({ timeout: 15_000 });
  expect(problems).toEqual([]);
});

test('wizard: cluster not reachable (probe and join), with next steps', async ({ page, request }) => {
  await scenario(request, 'join-unreachable');
  const c = await codes(request);
  await page.setViewportSize({ width: 360, height: 780 });
  await signIn(page);
  await page.getByRole('button', { name: /Join with an invite code/ }).click();
  await page.getByLabel('Invite code').fill(c.valid);
  await expect(page.getByText('Cannot reach the cluster from here yet')).toBeVisible();
  await expect(page.getByText(/No answer from 100\.64\.0\.41:7481\. .*firewall lets port 7481 in/)).toBeVisible();
  await page.getByRole('button', { name: 'Check again' }).click();
  await expect(page.getByText('Cannot reach the cluster from here yet')).toBeVisible();
  await axe(page, 'probe unreachable');
  await noOverflow(page, 'probe unreachable 360');
  await shot(page, 'wizard-probe-unreachable-360-light');
  await expect(page.getByRole('button', { name: 'Continue' })).toBeEnabled();
  await page.getByRole('button', { name: 'Continue' }).click();
  await page.getByRole('button', { name: 'Continue' }).click();
  await page.getByRole('button', { name: 'Join and copy the data' }).click();
  await expect(page.getByText(/None of the cluster’s computers answered \(tried 100\.64\.0\.41:7481\)/)).toBeVisible({ timeout: 15_000 });
  await noOverflow(page, 'join unreachable 360');
});

test('wizard: another cluster answers: shown clearly, cannot continue', async ({ page, request }) => {
  await scenario(request, 'join-wrong-cluster');
  const c = await codes(request);
  await page.setViewportSize({ width: 768, height: 1000 });
  await signIn(page);
  await page.getByRole('button', { name: /Join with an invite code/ }).click();
  await page.getByLabel('Invite code').fill(c.valid);
  await expect(page.getByText('That is not the cluster this code was made for')).toBeVisible();
  await expect(page.getByText(/Check that the code is the newest one, or create a new invite/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Continue' })).toBeDisabled();
  await axe(page, 'probe wrong cluster');
  await shot(page, 'wizard-probe-wrong-cluster-768-light');
});

test('wizard: a name already in the cluster: try again goes back to the name', async ({ page, request }) => {
  await scenario(request, 'fresh');
  const c = await codes(request);
  await page.setViewportSize({ width: 768, height: 1000 });
  await signIn(page);
  await page.getByRole('button', { name: /Join with an invite code/ }).click();
  await pasteAndContinue(page, c.valid, 'node-a');
  await page.getByRole('button', { name: 'Join and copy the data' }).click();
  await expect(page.getByText(/already has this name\. Pick another name for this computer/)).toBeVisible({ timeout: 15_000 });
  await page.getByRole('button', { name: 'Try again' }).click();
  await expect(page.getByLabel('Computer name')).toHaveValue('node-a');
  await page.getByLabel('Computer name').fill('node-b');
  await page.getByRole('button', { name: 'Continue' }).click();
  await expect(page.getByRole('radio', { name: 'Tailscale (100.64.0.44)' })).toBeChecked();
  await page.getByRole('button', { name: 'Join and copy the data' }).click();
  await expect(page.getByRole('heading', { name: 'This computer is now a copy' })).toBeVisible({ timeout: 30_000 });
});

test('wizard: an earlier join that did not finish', async ({ page, request }) => {
  await scenario(request, 'unfinished-join');
  await page.setViewportSize({ width: 768, height: 1000 });
  await signIn(page);
  await expect(page.getByRole('heading', { name: 'Joining did not finish' })).toBeVisible();
  await expect(page.getByText(/started joining a cluster but did not finish/)).toBeVisible();
  await axe(page, 'unfinished join');
  await page.getByRole('button', { name: 'Try again' }).click();
  await expect(page.getByRole('heading', { name: 'Paste the invite code' })).toBeVisible();
});

// ---------------------------------------------------------------- scenarios

const MEMBER_SCENARIOS: { id: string; tone: RegExp; sentence: RegExp }[] = [
  { id: 'single', tone: /All good/, sentence: /no copy yet/ },
  { id: 'two-healthy', tone: /All good/, sentence: /saved on 2 computers/ },
  { id: 'two-copy-down', tone: /Running on one copy/, sentence: /only on node-a/ },
  { id: 'two-main-down', tone: /Down/, sentence: /node-a, the main computer, is not responding/ },
  { id: 'two-witness', tone: /All good/, sentence: /pi-witness keeps watch/ },
  { id: 'three-healthy', tone: /All good/, sentence: /saved on 3 computers/ },
  { id: 'failover', tone: /Switching over/, sentence: /node-a stopped responding/ },
  { id: 'rejoining', tone: /Running on one copy/, sentence: /only on node-a/ },
  { id: 'needs-reclone', tone: /Needs attention/, sentence: /node-c/ },
  { id: 'needs-attention', tone: /Needs attention/, sentence: /node-b needs attention/ },
  { id: 'no-quorum', tone: /Down/, sentence: /Not enough computers can reach each other/ },
];

for (const s of MEMBER_SCENARIOS) {
  for (const theme of THEMES) {
    test(`overview · ${s.id} · ${theme}`, async ({ page, request }) => {
      const problems = watchConsole(page);
      await scenario(request, s.id);
      await setTheme(page, theme);
      await page.setViewportSize({ width: 1440, height: 1000 });
      await signIn(page);
      await expect(page.getByRole('heading', { level: 1 })).toContainText(s.tone);
      await expect(page.locator('.status-hero .sentence')).toContainText(s.sentence);
      await axe(page, `${s.id} ${theme}`);
      for (const w of WIDTHS) {
        await page.setViewportSize({ width: w, height: w < 500 ? 780 : 1000 });
        await noOverflow(page, `${s.id} ${w}`);
        await shot(page, `overview-${s.id}-${w}-${theme}`);
      }
      expect(problems).toEqual([]);
    });
  }
}

// ---------------------------------------------------------------- screens

const SCREENS: { path: string; heading: RegExp; name: string }[] = [
  { path: '/add', heading: /Add a computer/, name: 'add' },
  { path: '/connect', heading: /Connect your app/, name: 'connect' },
  { path: '/switch', heading: /Switch over/, name: 'switch' },
  { path: '/activity', heading: /Activity/, name: 'activity' },
  { path: '/settings', heading: /Computers & settings/, name: 'settings' },
  { path: '/backups', heading: /Backups/, name: 'backups' },
  { path: '/agents', heading: /Coding agents/, name: 'agents' },
];

for (const theme of THEMES) {
  test(`screens · two-witness · ${theme}`, async ({ page, request }) => {
    const problems = watchConsole(page);
    await scenario(request, 'two-witness');
    await setTheme(page, theme);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await signIn(page);
    for (const sc of SCREENS) {
      await page.setViewportSize({ width: 1440, height: 1000 });
      await go(page, sc.path);
      await expect(page.getByRole('heading', { level: 1 })).toHaveText(sc.heading);
      await page.waitForLoadState('networkidle');
      await axe(page, `${sc.name} ${theme}`);
      for (const w of WIDTHS) {
        await page.setViewportSize({ width: w, height: w < 500 ? 780 : 1000 });
        await noOverflow(page, `${sc.name} ${w}`);
        await shot(page, `screen-${sc.name}-${w}-${theme}`);
      }
    }
    expect(problems).toEqual([]);
  });
}

test('mobile menu opens, navigates and closes', async ({ page, request }) => {
  await scenario(request, 'two-healthy');
  await page.setViewportSize({ width: 360, height: 780 });
  await signIn(page);
  await page.getByRole('button', { name: 'Open menu' }).click();
  await shot(page, 'mobile-menu-360-light');
  await page.getByRole('link', { name: 'Connect your app' }).click();
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Connect your app');
  await expect(page.getByRole('button', { name: 'Open menu' })).toHaveAttribute('aria-expanded', 'false');
});

// ---------------------------------------------------------------- flows

test('add a computer: witness invite shows the command and the platform rule', async ({ page, request }) => {
  await scenario(request, 'two-healthy');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/add');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Add a computer');
  await expect(page.getByRole('radio', { name: /Witness/ })).toBeChecked(); // recommended for 2 nodes
  await page.getByRole('radio', { name: /Data computer/ }).check();
  await expect(page.getByText(/data computers run Linux on Intel\/AMD 64-bit/)).toBeVisible();
  await page.getByRole('radio', { name: /Witness/ }).check();
  await page.getByRole('button', { name: '1 day' }).click();
  await page.getByRole('button', { name: /Make an invite code/ }).click();
  await expect(page.locator('.code pre')).toContainText('coyote witness');
  await expect(page.getByText(/Expires in 1 day|Expires in 23 hours/)).toBeVisible();
  await shot(page, 'flow-add-witness-code-1440-light');
});

test('add a computer: cancel an unused invite (confirm), then it shows as cancelled', async ({ page, request }) => {
  const problems = watchConsole(page);
  await scenario(request, 'two-healthy');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/add');
  await page.getByRole('button', { name: /Make an invite code/ }).click();
  await expect(page.getByRole('textbox', { name: 'Invite code' })).toContainText('coyote-');
  const row = page.locator('.timeline li').first();
  await expect(row).toContainText(/expires in/);
  await row.getByRole('button', { name: /Cancel invite/ }).click();
  const dlg = page.getByRole('dialog');
  await expect(dlg).toContainText('Nobody can join with it afterwards');
  await axe(page, 'cancel invite dialog');
  await shot(page, 'flow-cancel-invite-confirm-1440-light');
  await dlg.getByRole('button', { name: 'Keep it' }).click();
  await expect(dlg).toBeHidden();
  await row.getByRole('button', { name: /Cancel invite/ }).click();
  await dlg.getByRole('button', { name: 'Cancel the code' }).click();
  await expect(page.getByText('Invite code cancelled. It no longer works.')).toBeVisible();
  await expect(row).toContainText('Cancelled');
  await expect(row.getByRole('button', { name: /Cancel invite/ })).toHaveCount(0);
  await expect(page.getByRole('textbox', { name: 'Invite code' })).toHaveCount(0);
  expect(problems).toEqual([]);
});

test('connect: password only after reveal; snippets', async ({ page, request }) => {
  await scenario(request, 'three-healthy');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/connect');
  const cs = page.locator('.code pre').first();
  await expect(cs).toContainText(
    'postgresql://app:YOUR_PASSWORD@100.64.0.41:5432,100.64.0.42:5432,100.64.0.43:5432/app?sslmode=verify-full&sslrootcert=/path/to/coyote-ca.crt&target_session_attrs=read-write&connect_timeout=10',
  );
  await page.getByLabel('Your app runs on').selectOption('local');
  await expect(cs).toContainText('@127.0.0.1:5432/app?sslmode=verify-full&sslrootcert=/var/lib/coyote/ca.crt&connect_timeout=10');
  await page.getByLabel('Your app runs on').selectOption('all');
  await page.getByRole('button', { name: /Show password/ }).click();
  await expect(cs).toContainText('kq3V-mock-password_9zLw');
  await page.getByRole('tab', { name: 'Node.js · pg' }).click();
  await expect(page.getByText('This driver takes one address')).toBeVisible();
  await shot(page, 'flow-connect-revealed-1440-light');
  await page.getByRole('button', { name: /Hide password/ }).click();
  await expect(cs).not.toContainText('kq3V-mock-password_9zLw');
});

test('connect: make a new password (confirm, shown once, snippets follow)', async ({ page, request }) => {
  const problems = watchConsole(page);
  await scenario(request, 'two-witness');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/connect');
  await page.getByRole('button', { name: 'Make a new password…' }).click();
  const dlg = page.getByRole('dialog');
  await expect(dlg).toContainText('Every app that uses it must be updated');
  await axe(page, 'new password dialog');
  await shot(page, 'flow-new-password-confirm-1440-light');
  await dlg.getByRole('button', { name: 'Make a new password' }).click();
  await expect(page.getByText('New password made', { exact: true })).toBeVisible();
  const pw = (await page.locator('.notice.good .code pre').innerText()).trim();
  expect(pw).toMatch(/^[A-Za-z0-9_-]{20,}$/);
  expect(pw).not.toBe('kq3V-mock-password_9zLw');
  await expect(page.locator('.code pre').nth(1)).toContainText(`postgresql://app:${pw}@`);
  await axe(page, 'new password shown');
  await shot(page, 'flow-new-password-shown-1440-light');
  // The agent now hands out the new one.
  await page.getByRole('button', { name: /Hide password/ }).click();
  await expect(page.getByText('New password made', { exact: true })).toHaveCount(0);
  await page.getByRole('button', { name: /Show password/ }).click();
  await expect(page.locator('.code pre').first()).toContainText(`app:${pw}@`);
  expect(problems).toEqual([]);
});

test('connect: the CA certificate downloads as coyote-ca.crt under the CSP', async ({ page, request }) => {
  const problems = watchConsole(page);
  await scenario(request, 'two-witness');
  await page.setViewportSize({ width: 768, height: 1000 });
  await signIn(page, '/connect');
  await expect(page.getByText('/var/lib/coyote/ca.crt')).toBeVisible();
  const [dl] = await Promise.all([page.waitForEvent('download'), page.getByRole('button', { name: 'Download coyote-ca.crt' }).click()]);
  expect(dl.suggestedFilename()).toBe('coyote-ca.crt');
  const path = await dl.path();
  const { readFileSync } = await import('node:fs');
  const text = readFileSync(path, 'utf8');
  expect(text).toMatch(/^-----BEGIN CERTIFICATE-----\n[\s\S]+\n-----END CERTIFICATE-----\n$/);
  await noOverflow(page, 'connect ca 768');
  expect(problems).toEqual([]);
});

test('settings: change addresses (checked and saved; failed check, then save anyway)', async ({ page, request }) => {
  const problems = watchConsole(page);
  await scenario(request, 'two-witness');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/settings');
  const row = page.locator('li', { hasText: 'node-b' }).filter({ has: page.getByRole('button', { name: 'Change addresses of node-b' }) });
  await row.getByRole('button', { name: 'Change addresses of node-b' }).click();
  const dlg = page.getByRole('dialog');
  const box = dlg.getByLabel('Addresses (HOST:PORT)');
  await expect(box).toHaveValue('100.64.0.42:7481');

  // Malformed input is caught before anything is sent.
  await box.fill('100.64.0.42');
  await expect(dlg.getByText(/is not HOST:PORT/)).toBeVisible();
  await expect(dlg.getByRole('button', { name: 'Check and save' })).toBeDisabled();

  // Success: one address answers.
  await box.fill('100.64.0.52:7481, 192.168.1.42:7481');
  await dlg.getByRole('button', { name: 'Check and save' }).click();
  await expect(page.getByText('Other nodes now reach node-b at 100.64.0.52:7481, 192.168.1.42:7481.')).toBeVisible();
  await expect(row).toContainText('100.64.0.52:7481');

  // Failed check: nothing changes, then "Save anyway".
  await row.getByRole('button', { name: 'Change addresses of node-b' }).click();
  await box.fill('198.51.100.9:7481');
  await dlg.getByRole('button', { name: 'Check and save' }).click();
  await expect(dlg.getByText('Nothing was changed')).toBeVisible();
  await expect(dlg.getByRole('list', { name: 'Check results' })).toContainText('198.51.100.9:7481 did not answer as node-b');
  await expect(dlg).not.toContainText('--force');
  await axe(page, 'change addresses failed');
  await shot(page, 'flow-addresses-failed-1440-light');
  await dlg.getByRole('button', { name: 'Save anyway' }).click();
  await expect(page.getByText('Other nodes now reach node-b at 198.51.100.9:7481.')).toBeVisible();
  await expect(row).toContainText('198.51.100.9:7481');
  expect(problems).toEqual([]);
});

test('real agent features: hidden controls stay hidden and nothing errors', async ({ page, request }) => {
  const problems = watchConsole(page);
  const failed: string[] = [];
  page.on('response', (r) => {
    if (r.url().includes('/v1/') && r.status() >= 400) failed.push(`${r.status()} ${r.request().method()} ${new URL(r.url()).pathname}`);
  });
  expect((await request.post('/__mock/features', { data: { real: true } })).ok()).toBeTruthy();
  try {
    await scenario(request, 'two-healthy');
    await page.setViewportSize({ width: 1440, height: 1000 });
    await signIn(page);
    await expect(page.getByRole('heading', { level: 1 })).toContainText('All good');
    await expect(page.locator('.topbar .cluster-name')).toHaveText('Your database');

    await go(page, '/settings');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Computers & settings');
    await page.waitForLoadState('networkidle');
    await expect(page.getByRole('button', { name: 'Rename' })).toHaveCount(0);
    await expect(page.getByLabel('Name', { exact: true })).toHaveCount(0);
    await expect(page.getByRole('switch')).toHaveCount(0);
    await expect(page.getByText('Opens on this computer only')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Change addresses of node-b' })).toBeVisible();
    await axe(page, 'settings real features');
    await noOverflow(page, 'settings real features');
    await shot(page, 'real-features-settings-1440-light');

    await go(page, '/add');
    await page.getByRole('radio', { name: /Witness/ }).check();
    await page.getByRole('button', { name: /Make an invite code/ }).click();
    await expect(page.locator('.code pre')).toContainText('coyote witness');
    await expect(page.getByRole('heading', { name: 'Invite code', exact: true })).toBeVisible();
    await expect(page.getByText(/The code itself works for a data computer or a witness/)).toBeVisible();
    // The real agent cancels invites now.
    await expect(page.locator('.timeline li').first().getByRole('button', { name: /Cancel invite/ })).toBeVisible();
    await axe(page, 'add real features');
    await noOverflow(page, 'add real features');
    await shot(page, 'real-features-add-1440-light');

    await go(page, '/connect');
    await expect(page.getByRole('button', { name: 'Make a new password…' })).toBeVisible();
    await axe(page, 'connect real features');
    await noOverflow(page, 'connect real features');

    await go(page, '/switch');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Switch over');
    await go(page, '/activity');
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('Activity');
    await page.waitForLoadState('networkidle');
    expect(failed).toEqual([]);
    expect(problems).toEqual([]);
  } finally {
    await request.post('/__mock/features', { data: { real: false } });
  }
});

test('real agent features: the setup page sends only what the agent takes', async ({ page, request }) => {
  const problems = watchConsole(page);
  const failed = watchErrors(page);
  const bodies: string[] = [];
  page.on('request', (r) => {
    if (r.url().endsWith('/v1/setup/create')) bodies.push(r.postData() ?? '');
  });
  expect((await request.post('/__mock/features', { data: { real: true } })).ok()).toBeTruthy();
  try {
    await scenario(request, 'fresh');
    await page.setViewportSize({ width: 1440, height: 1000 });
    await signIn(page);
    await page.getByRole('button', { name: /Start a new database/ }).click();
    await expect(page.getByRole('heading', { name: 'Name this computer' })).toBeVisible();
    await expect(page.getByLabel(/Name for the database/)).toHaveCount(0);
    await page.getByRole('button', { name: 'Continue' }).click();
    await expect(page.getByRole('heading', { name: 'Keep backups of your data' })).toBeVisible();
    await expect(page.getByText('/var/lib/coyote/node-a-backups')).toBeVisible();
    await page.getByRole('button', { name: 'Continue' }).click();
    await page.getByRole('button', { name: 'Start the database' }).click();
    await expect(page.getByRole('heading', { name: 'Your database is running' })).toBeVisible({ timeout: 20_000 });
    expect(bodies.length).toBe(1);
    expect(JSON.parse(bodies[0]!)).toEqual({ name: 'office-mini', advertise: '100.64.0.44' });
    await page.getByRole('button', { name: /Go to the dashboard/ }).click();
    await expect(page.getByRole('heading', { level: 1 })).toContainText('All good');
    expect(failed).toEqual([]);
    expect(problems).toEqual([]);
  } finally {
    await request.post('/__mock/features', { data: { real: false } });
  }
});

test('planned switch-over with confirmation', async ({ page, request }) => {
  await scenario(request, 'three-healthy');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/switch');
  await page.getByRole('radio', { name: /node-c/ }).check();
  await page.getByRole('button', { name: /Switch over to node-c/ }).click();
  await expect(page.getByRole('dialog')).toContainText('Make node-c the main node?');
  await shot(page, 'flow-switch-confirm-1440-light');
  await page.getByRole('dialog').getByRole('button', { name: 'Switch over' }).click();
  await expect(page.getByText('node-c is now the main node.')).toBeVisible({ timeout: 15_000 });
  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Overview' }).click();
  await expect(page.locator('.node-card.main')).toContainText('node-c');
});

test('forced promote in the 2-node case needs typing the name and ticking the warning', async ({ page, request }) => {
  await scenario(request, 'two-main-down');
  await page.setViewportSize({ width: 768, height: 1000 });
  await signIn(page);
  await page.getByRole('link', { name: 'Switch over' }).first().click();
  await expect(page.getByRole('heading', { name: 'Make node-b the main node' })).toBeVisible();
  await page.getByRole('button', { name: /Make node-b the main node…/ }).click();
  const dlg = page.getByRole('dialog');
  const go = dlg.getByRole('button', { name: 'Make node-b main' });
  await expect(go).toBeDisabled();
  await dlg.getByRole('textbox').fill('node-b');
  await expect(go).toBeDisabled();
  await dlg.getByRole('checkbox').check();
  await expect(go).toBeEnabled();
  await axe(page, 'forced promote dialog');
  await shot(page, 'flow-forced-promote-768-light');
  await go.click();
  await expect(page.getByText(/node-b is now the main node/)).toBeVisible({ timeout: 15_000 });
});

test('failover completes by itself and the overview follows live', async ({ page, request }) => {
  await scenario(request, 'failover');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page);
  await expect(page.getByRole('heading', { level: 1 })).toContainText('Switching over');
  await expect(page.getByRole('heading', { level: 1 })).toContainText('Needs attention', { timeout: 20_000 });
  await expect(page.locator('.node-card.main')).toContainText('node-c');
  await shot(page, 'flow-failover-after-1440-light');
  await page.getByRole('link', { name: 'Activity' }).click();
  await expect(page.getByText('node-c became the main node (node-a stopped responding)')).toBeVisible();
});

test('remove a computer spells out consequences and needs the name', async ({ page, request }) => {
  await scenario(request, 'two-witness');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/settings');
  await page.locator('li', { hasText: 'pi-witness' }).getByRole('button', { name: 'Remove' }).click();
  const dlg = page.getByRole('dialog');
  await expect(dlg).toContainText('Switch-over will no longer happen by itself');
  await dlg.getByRole('textbox').fill('pi-witness');
  await shot(page, 'flow-remove-witness-1440-light');
  await dlg.getByRole('button', { name: 'Remove pi-witness' }).click();
  await expect(page.getByText('Removed pi-witness.')).toBeVisible();
});

test('settings: single-node behaviour saves', async ({ page, request }) => {
  await scenario(request, 'two-healthy');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/settings');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Computers & settings');
  const strict = page.getByRole('radio', { name: /Wait for the copy/ });
  await strict.check();
  await expect(strict).toBeChecked();
  await page.getByRole('button', { name: 'Save' }).click();
  await expect(page.getByText('Settings saved on every computer.')).toBeVisible();
});

// ---------------------------------------------------------------- states

test('agent unreachable, expired session, and a used sign-in link', async ({ page, request, browser }) => {
  await scenario(request, 'two-healthy');
  await page.setViewportSize({ width: 768, height: 900 });
  await signIn(page);
  await expect(page.getByRole('heading', { level: 1 })).toContainText('All good');

  await scenario(request, 'agent-down');
  await expect(page.getByRole('alert').filter({ hasText: 'Lost contact with CoyoteDB' })).toBeVisible({ timeout: 15_000 });
  await shot(page, 'state-lost-contact-768-light');

  const fresh = await browser.newPage();
  await fresh.goto('/');
  await expect(fresh.getByRole('heading', { name: 'CoyoteDB isn’t answering' })).toBeVisible({ timeout: 15_000 });
  await fresh.screenshot({ path: 'screenshots/state-unreachable-1440-light.png' });

  await scenario(request, 'two-healthy');
  await request.post('/__mock/expire-sessions');
  await expect(page.getByRole('heading', { name: 'Your session ended' })).toBeVisible({ timeout: 20_000 });
  await shot(page, 'state-session-ended-768-light');

  const other = await browser.newPage();
  const link = (await (await other.request.get('/__mock/login-link')).json()) as { url: string };
  const u = new URL(link.url);
  await other.goto(u.pathname + u.hash);
  await expect(other.getByRole('heading', { level: 1 })).toContainText('All good');
  const third = await browser.newPage();
  await third.goto(u.pathname + u.hash);
  await expect(third.getByRole('heading', { name: 'That link did not work' })).toBeVisible();
  await third.screenshot({ path: 'screenshots/state-link-used-1440-light.png' });
  await expect(third).toHaveURL(/\/$/); // token removed from the address bar
});

test('API refuses state changes without the CSRF header', async ({ page, request }) => {
  await scenario(request, 'two-healthy');
  await signIn(page);
  const r = await page.evaluate(async () => {
    const res = await fetch('/v1/settings', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ set: { cluster_name: 'x' } }) });
    return { status: res.status, body: await res.json() };
  });
  expect(r.status).toBe(403);
  expect(r.body.code).toBe('csrf_failed');
});
