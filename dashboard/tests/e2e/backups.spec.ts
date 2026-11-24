// Backups (D63, D65): the setup step (every choice), the Backups page in
// every state, back up now, schedule, where (folder and cloud, key shown
// once), restore to a moment into a new folder, and check a backup.
import { expect, test, type Page } from '@playwright/test';
import { axe, codes, go, noOverflow, scenario, setTheme, shot, signIn, THEMES, watchConsole, WIDTHS } from './helpers';

test.beforeEach(async ({ page }) => {
  await page.emulateMedia({ reducedMotion: 'reduce' });
});

function watchErrors(page: Page): string[] {
  const failed: string[] = [];
  page.on('response', (r) => {
    if (r.url().includes('/v1/') && r.status() >= 400) failed.push(`${r.status()} ${r.request().method()} ${new URL(r.url()).pathname}`);
  });
  return failed;
}

/** Fresh install → "Start a new database" → name → the backup step. */
async function toBackupStep(page: Page): Promise<void> {
  await signIn(page);
  await page.getByRole('button', { name: /Start a new database/ }).click();
  await page.getByRole('button', { name: 'Continue' }).click();
  await expect(page.getByRole('heading', { name: 'Keep backups of your data' })).toBeVisible();
}

async function startAndWait(page: Page): Promise<void> {
  await expect(page.getByRole('heading', { name: /How will the other computers reach this one/ })).toBeVisible();
  await page.getByRole('button', { name: 'Start the database' }).click();
  await expect(page.getByRole('heading', { name: 'Your database is running' })).toBeVisible({ timeout: 20_000 });
}

// ---------------------------------------------------------------- setup step

test('setup: backups on by default, in a folder next to the data, with details', async ({ page, request }) => {
  const problems = watchConsole(page);
  const failed = watchErrors(page);
  await scenario(request, 'fresh');
  for (const theme of THEMES) {
    await setTheme(page, theme);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await toBackupStep(page);
    await expect(page.getByRole('radio', { name: /On, in a folder on this computer/ })).toBeChecked();
    await expect(page.getByText('/var/lib/coyote/node-a-backups')).toBeVisible();
    await expect(page.getByText(/412 GB free/)).toBeVisible();
    await expect(page.getByText(/Same disk as the database: it protects you from mistakes/)).toBeVisible();
    await page.getByText('Details for the technical').click();
    await expect(page.getByText(/Continuous WAL archiving/)).toBeVisible();
    await expect(page.getByText(/Point-in-time recovery \(PITR\)/)).toBeVisible();
    await axe(page, `setup backups ${theme}`);
    for (const w of WIDTHS) {
      await page.setViewportSize({ width: w, height: w < 500 ? 780 : 1000 });
      await noOverflow(page, `setup backups ${w}`);
      await shot(page, `backups-setup-default-${w}-${theme}`);
    }
    await scenario(request, 'fresh');
  }
  await page.setViewportSize({ width: 1440, height: 1000 });
  await toBackupStep(page);
  await page.getByRole('button', { name: 'Continue' }).click();
  await startAndWait(page);
  const panel = page.locator('section', { has: page.getByRole('heading', { name: 'Backups', exact: true }) });
  await expect(panel).toContainText('On: every change is saved');
  await expect(panel).toContainText('/var/lib/coyote/node-a-backups');
  await shot(page, 'backups-setup-success-default-1440-light');
  await page.getByRole('button', { name: /Go to the dashboard/ }).click();
  await expect(page.getByRole('heading', { level: 1 })).toContainText('All good');
  await expect(page.getByText(/Backups · First backup soon/)).toBeVisible();
  expect(failed).toEqual([]);
  expect(problems).toEqual([]);
});

test('setup: another folder or disk is checked by the agent as it is typed', async ({ page, request }) => {
  const problems = watchConsole(page);
  const bodies: string[] = [];
  page.on('request', (r) => {
    if (r.url().endsWith('/v1/backups/destination') && r.method() === 'PUT') bodies.push(r.postData() ?? '');
  });
  await scenario(request, 'fresh');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await toBackupStep(page);
  await page.getByRole('radio', { name: /Another folder or disk/ }).check();
  const folder = page.getByLabel('Folder', { exact: true });
  const cont = page.getByRole('button', { name: 'Continue' });
  await expect(cont).toBeDisabled();

  await folder.fill('backups');
  await expect(page.getByText(/Type the full path of the folder/)).toBeVisible();
  await expect(cont).toBeDisabled();
  await folder.fill('/var/lib/coyote/node-a/backups');
  await expect(page.getByText(/Pick a folder outside the database's own folder/)).toBeVisible();
  await folder.fill('/readonly/backups');
  await expect(page.getByText(/cannot write in \/readonly\/backups/)).toBeVisible();
  await folder.fill('/full/backups');
  await expect(page.getByText(/Backups need at least 2 GB/)).toBeVisible();
  await folder.fill('/home/sam/coyote-backups');
  await expect(page.getByText(/This folder is on the same disk as the database/)).toBeVisible();
  await expect(cont).toBeEnabled();
  await folder.fill('/mnt/usb/coyote-backups');
  await expect(page.getByText(/1\.7 TB free/)).toBeVisible();
  await expect(page.getByText(/This folder is on the same disk as the database/)).toHaveCount(0);
  await page.getByLabel(/Encrypt these backups/).check();
  await axe(page, 'setup folder');
  await shot(page, 'backups-setup-folder-1440-light');
  await page.setViewportSize({ width: 360, height: 780 });
  await noOverflow(page, 'setup folder 360');
  await shot(page, 'backups-setup-folder-360-light');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await cont.click();
  await startAndWait(page);
  // Encrypted folder: a new key, shown once, saved before going on.
  await expect(page.getByRole('textbox', { name: 'Backup key' })).toContainText('cybk-');
  await expect(page.getByRole('button', { name: /Go to the dashboard/ })).toBeDisabled();
  await page.getByLabel('I saved the backup key somewhere safe').check();
  await expect(page.getByRole('button', { name: /Go to the dashboard/ })).toBeEnabled();
  expect(bodies.map((b) => JSON.parse(b))).toEqual([{ kind: 'folder', folder: '/mnt/usb/coyote-backups', encrypt: true }]);
  await expect(page.getByText('/mnt/usb/coyote-backups')).toBeVisible();
  expect(problems).toEqual([]);
});

test('setup: cloud storage needs a passing test, then shows the key once', async ({ page, request }) => {
  const problems = watchConsole(page);
  await scenario(request, 'fresh');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await toBackupStep(page);
  await page.getByRole('radio', { name: /Cloud storage/ }).check();
  await expect(page.getByRole('button', { name: 'Backblaze B2' })).toHaveAttribute('aria-pressed', 'true');
  await expect(page.getByLabel('Storage address (endpoint)')).toHaveValue('https://s3.us-west-004.backblazeb2.com');
  await expect(page.getByText(/always encrypted on this computer before they are sent/)).toBeVisible();
  const test1 = page.getByRole('button', { name: 'Test access' });
  await expect(test1).toBeDisabled();
  await expect(page.getByText('Fill in the bucket name.')).toBeVisible();
  // Switching provider changes the example address and region.
  await page.getByRole('button', { name: 'Cloudflare R2' }).click();
  await expect(page.getByLabel('Storage address (endpoint)')).toHaveValue('https://ACCOUNT-ID.r2.cloudflarestorage.com');
  await expect(page.getByLabel(/Region/)).toHaveValue('auto');
  await page.getByRole('button', { name: 'Amazon S3' }).click();
  await expect(page.getByLabel('Storage address (endpoint)')).toHaveValue('https://s3.us-east-1.amazonaws.com');
  await page.getByRole('button', { name: 'Backblaze B2' }).click();

  await page.getByLabel('Bucket', { exact: true }).fill('bakery-backups');
  await page.getByLabel('Access key ID').fill('004a1b2c3d4e5f');
  await page.getByLabel('Secret key').fill('wrong');
  await test1.click();
  await expect(page.getByText(/The storage refused these keys/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Continue' })).toBeDisabled();
  await page.getByLabel('Bucket', { exact: true }).fill('missing');
  await page.getByLabel('Secret key').fill('K005s3cret');
  await test1.click();
  await expect(page.getByText(/That bucket was not found/)).toBeVisible();
  await page.getByLabel('Bucket', { exact: true }).fill('bakery-backups');
  // A changed field needs a new test.
  await expect(page.getByRole('button', { name: 'Continue' })).toBeDisabled();
  await test1.click();
  await expect(page.getByText(/Works: this computer can list, write and delete/)).toBeVisible();
  await axe(page, 'setup s3');
  await shot(page, 'backups-setup-s3-1440-light');
  await page.setViewportSize({ width: 360, height: 780 });
  await noOverflow(page, 'setup s3 360');
  await shot(page, 'backups-setup-s3-360-light');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.getByRole('button', { name: 'Continue' }).click();
  await startAndWait(page);
  await expect(page.getByRole('textbox', { name: 'Backup key' })).toContainText('cybk-');
  await expect(page.getByText(/It is shown once/)).toBeVisible();
  await expect(page.getByText('s3://bakery-backups/coyote (https://s3.us-west-004.backblazeb2.com)')).toBeVisible();
  const go1 = page.getByRole('button', { name: /Go to the dashboard/ });
  await expect(go1).toBeDisabled();
  await axe(page, 'setup s3 key');
  await shot(page, 'backups-setup-key-1440-light');
  await page.getByLabel('I saved the backup key somewhere safe').check();
  await go1.click();
  await expect(page.getByRole('heading', { level: 1 })).toContainText('All good');
  await go(page, '/backups');
  await expect(page.getByRole('heading', { name: 'Cloud storage' })).toBeVisible();
  await expect(page.getByText('Encrypted', { exact: true })).toBeVisible();
  expect(problems).toEqual([]);
});

test('setup: not now says what it costs, and backups end up off', async ({ page, request }) => {
  const problems = watchConsole(page);
  await scenario(request, 'fresh');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await toBackupStep(page);
  await page.getByRole('radio', { name: /Not now/ }).check();
  await expect(page.getByText(/Nothing is saved, so a deleted table or a bad change cannot be undone/)).toBeVisible();
  await page.getByRole('button', { name: 'Continue without backups' }).click();
  await startAndWait(page);
  await expect(page.getByText(/Backups are off\. Nothing is saved/)).toBeVisible();
  await page.getByRole('button', { name: /Go to the dashboard/ }).click();
  await expect(page.getByText(/Backups · Off/)).toBeVisible();
  await page.getByRole('link', { name: 'Turn on' }).click();
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Backups are off.');
  expect(problems).toEqual([]);
});

test('setup: a joining computer follows the cluster’s backups, no questions', async ({ page, request }) => {
  await scenario(request, 'fresh');
  const c = await codes(request);
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page);
  await page.getByRole('button', { name: /Join with an invite code/ }).click();
  await page.getByLabel('Invite code').fill(c.valid);
  await expect(page.getByText(/Valid invite, expires in/)).toBeVisible();
  await page.getByRole('button', { name: 'Continue' }).click();
  await page.getByRole('button', { name: 'Continue' }).click();
  await expect(page.getByRole('heading', { name: /How will the other computers reach this one/ })).toBeVisible();
  await expect(page.getByText(/Backups follow the cluster’s settings: nothing to choose here/)).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Keep backups of your data' })).toHaveCount(0);
});

// ---------------------------------------------------------------- the page

const PAGE_STATES: { id: string; title: string | RegExp; sentence: RegExp; card: RegExp }[] = [
  { id: 'two-healthy', title: 'Backups are on and working.', sentence: /You can go back to any moment from/, card: /Backups · Healthy/ },
  { id: 'backups-first', title: /The first full backup is taken at/, sentence: /Every change is already being saved/, card: /Backups · First backup soon/ },
  { id: 'backups-attention', title: 'Backups need attention.', sentence: /Saving changes to the backup has failed since/, card: /Backups · Needs attention/ },
  { id: 'backups-off', title: 'Backups are off.', sentence: /Nothing is saved/, card: /Backups · Off/ },
  { id: 'backups-s3', title: 'Backups are on and working.', sentence: /You can go back to any moment from/, card: /Backups · Healthy/ },
];

for (const st of PAGE_STATES) {
  for (const theme of THEMES) {
    test(`page · ${st.id} · ${theme}`, async ({ page, request }) => {
      const problems = watchConsole(page);
      const failed = watchErrors(page);
      await scenario(request, st.id);
      await setTheme(page, theme);
      await page.setViewportSize({ width: 1440, height: 1000 });
      await signIn(page);
      await expect(page.getByText(st.card)).toBeVisible();
      if (theme === 'light') await shot(page, `backups-overview-${st.id}-1440-light`);
      await go(page, '/backups');
      await expect(page.getByRole('heading', { level: 1 })).toHaveText(st.title);
      await expect(page.locator('.bk-hero .sentence')).toContainText(st.sentence);
      await page.waitForLoadState('networkidle');
      await axe(page, `backups ${st.id} ${theme}`);
      for (const w of WIDTHS) {
        await page.setViewportSize({ width: w, height: w < 500 ? 780 : 1000 });
        await noOverflow(page, `backups ${st.id} ${w}`);
        await shot(page, `backups-page-${st.id}-${w}-${theme}`);
      }
      expect(failed).toEqual([]);
      expect(problems).toEqual([]);
    });
  }
}

test('page: healthy shows the last backup, how far back, where and the list', async ({ page, request }) => {
  await scenario(request, 'two-healthy');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/backups');
  await expect(page.getByText('Last good backup')).toBeVisible();
  await expect(page.locator('.bk-fact .big').nth(1)).toHaveText(/\d days/);
  await expect(page.getByRole('img', { name: /Restorable from .* 7 full backups/ })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'A folder on each data computer' })).toBeVisible();
  await expect(page.getByText(/This folder is on the same disk as the database/)).toBeVisible();
  await expect(page.locator('.bk-table tbody tr')).toHaveCount(7);
  await expect(page.locator('.bk-table tbody tr').first()).toContainText('Ready');
  // The nav lists Backups with the main pages now.
  await expect(page.getByRole('link', { name: 'Backups', exact: true })).toHaveAttribute('aria-current', 'page');
});

test('page: a witness explains it keeps no backups; incomplete backups are explained', async ({ page, request }) => {
  const problems = watchConsole(page);
  await scenario(request, 'backups-witness');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/backups');
  await expect(page.getByText(/This computer is a witness: it holds no data, so it keeps no backups/)).toBeVisible();
  await axe(page, 'backups witness');
  await shot(page, 'backups-page-witness-1440-light');
  await scenario(request, 'backups-incomplete');
  await page.reload();
  await expect(page.locator('.bk-table')).toContainText('Incomplete');
  await expect(page.getByText(/It is never used for a restore/)).toBeVisible();
  expect(problems).toEqual([]);
});

test('page: back up now runs and lands in the list', async ({ page, request }) => {
  await scenario(request, 'backups-first');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/backups');
  await expect(page.getByRole('heading', { name: 'No full backups yet' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Restore to a moment…' })).toBeDisabled();
  await expect(page.getByRole('button', { name: 'Check a backup…' })).toBeDisabled();
  await page.getByRole('button', { name: 'Back up now' }).click();
  await expect(page.getByText('Full backup started on node-a.')).toBeVisible();
  await expect(page.getByRole('button', { name: 'A backup is running' })).toBeDisabled();
  await expect(page.locator('.bk-table')).toContainText('Running');
  await shot(page, 'backups-page-running-1440-light');
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Backups are on and working.', { timeout: 15_000 });
  await expect(page.locator('.bk-table tbody tr').first()).toContainText('Ready');
  await expect(page.locator('.bk-table tbody tr').first()).toContainText('By hand');
});

test('page: change the schedule, then turn backups off and on', async ({ page, request }) => {
  await scenario(request, 'two-healthy');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/backups');
  await expect(page.getByRole('heading', { name: 'A full copy every day, kept 7 days' })).toBeVisible();
  await page.getByRole('button', { name: 'Change schedule…' }).click();
  await page.getByLabel('A full copy', { exact: true }).selectOption('12h');
  await page.getByLabel('Keep backups for').selectOption('14d');
  await axe(page, 'schedule form');
  await shot(page, 'backups-page-schedule-1440-light');
  await page.getByRole('button', { name: 'Save schedule' }).click();
  await expect(page.getByRole('heading', { name: 'A full copy every 12 hours, kept 14 days' })).toBeVisible();

  await page.getByRole('button', { name: 'Turn backups off…' }).click();
  const dlg = page.getByRole('dialog', { name: 'Turn backups off?' });
  await expect(dlg).toContainText('Nothing is saved, so a deleted table or a bad change cannot be undone.');
  await dlg.getByRole('button', { name: 'Turn off' }).click();
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Backups are off.');
  await page.getByRole('button', { name: 'Turn backups on' }).click();
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('Backups are on and working.');
});

test('page: move backups to cloud storage; the new key needs “I saved it”; show the key again', async ({ page, request }) => {
  const problems = watchConsole(page);
  await scenario(request, 'two-healthy');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/backups');
  await page.getByRole('button', { name: 'Change where…' }).click();
  const dlg = page.getByRole('dialog', { name: 'Where should backups go?' });
  await dlg.getByRole('radio', { name: /Cloud storage/ }).check();
  await dlg.getByLabel('Bucket', { exact: true }).fill('bakery-backups');
  await dlg.getByLabel('Access key ID').fill('004a1b2c3d4e5f');
  await dlg.getByLabel('Secret key').fill('K005s3cret');
  await expect(dlg.getByRole('button', { name: 'Save' })).toBeDisabled();
  await dlg.getByRole('button', { name: 'Test access' }).click();
  await expect(dlg.getByText(/Works: this computer can list, write and delete/)).toBeVisible();
  await axe(page, 'where dialog');
  await shot(page, 'backups-page-where-dialog-1440-light');
  await dlg.getByRole('button', { name: 'Save' }).click();
  const key = page.getByRole('dialog', { name: 'Your new backup key' });
  await expect(key.getByRole('textbox', { name: 'Backup key' })).toContainText('cybk-');
  await expect(key.getByRole('button', { name: 'Done' })).toBeDisabled();
  await shot(page, 'backups-page-new-key-1440-light');
  await key.getByLabel('I saved the backup key somewhere safe').check();
  await key.getByRole('button', { name: 'Done' }).click();
  await expect(page.getByRole('heading', { name: 'Cloud storage' })).toBeVisible();

  await page.getByRole('button', { name: /Show the backup key/ }).click();
  await page.getByRole('dialog', { name: 'Show the backup key?' }).getByRole('button', { name: 'Show it' }).click();
  const again = page.getByRole('dialog', { name: 'Backup key' });
  await expect(again.getByRole('textbox', { name: 'Backup key' })).toContainText('cybk-');
  await again.getByRole('button', { name: 'Done' }).click();
  expect(problems).toEqual([]);
});

test('page: restore to a moment, into a new folder only, then the start command', async ({ page, request }) => {
  const problems = watchConsole(page);
  const bodies: unknown[] = [];
  page.on('request', (r) => {
    if (r.url().endsWith('/v1/backups/restore')) bodies.push(JSON.parse(r.postData() ?? '{}'));
  });
  await scenario(request, 'two-healthy');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/backups');
  await page.getByRole('button', { name: 'Restore to a moment…' }).click();
  const dlg = page.getByRole('dialog');
  await expect(dlg.getByRole('heading', { name: 'Which moment do you want back?' })).toBeVisible();
  await expect(dlg.getByRole('radio', { name: /The newest saved change/ })).toBeChecked();
  await dlg.getByRole('radio', { name: /A moment I pick/ }).check();
  const moment = dlg.getByLabel(/Date and time/);
  await moment.fill('2001-01-01T10:00');
  await expect(dlg.getByText(/The backups reach back to/)).toBeVisible();
  await expect(dlg.getByRole('button', { name: 'Continue' })).toBeDisabled();
  await dlg.getByRole('button', { name: '1 hour ago' }).click();
  await expect(dlg.getByText(/Anywhere from/)).toBeVisible();
  await axe(page, 'restore moment');
  await shot(page, 'backups-restore-1-moment-1440-light');
  await dlg.getByRole('button', { name: 'Continue' }).click();

  await expect(dlg.getByRole('heading', { name: 'Where should the copy go?' })).toBeVisible();
  const into = dlg.getByLabel('New folder');
  await expect(into).toHaveValue(/\/var\/lib\/coyote\/coyote-restore-\d{8}-\d{4}/);
  await expect(dlg.getByText(/A new folder · 412 GB free · needs about/)).toBeVisible();
  const suggested = await into.inputValue();
  await into.fill('/var/lib/coyote/node-a');
  await expect(dlg.getByText(/That is this database's own folder/)).toBeVisible();
  await into.fill('/home/sam/notes');
  await expect(dlg.getByText(/is not empty\. A restore only goes into a new or empty folder/)).toBeVisible();
  await expect(dlg.getByRole('button', { name: 'Continue' })).toBeDisabled();
  await into.fill(suggested);
  await expect(dlg.getByText(/A new folder · 412 GB free/)).toBeVisible();
  await axe(page, 'restore folder');
  await shot(page, 'backups-restore-2-folder-1440-light');
  await dlg.getByRole('button', { name: 'Continue' }).click();

  await expect(dlg.getByRole('heading', { name: 'Restore into a new folder?' })).toBeVisible();
  await expect(dlg).toContainText('Nothing in your running database changes, on any computer.');
  await shot(page, 'backups-restore-3-confirm-1440-light');
  await dlg.getByRole('button', { name: 'Restore into the new folder' }).click();
  await expect(page.getByRole('heading', { name: /Restoring into/ })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'Restored into a new folder' })).toBeVisible({ timeout: 15_000 });
  const cmd = page.locator('.code pre');
  await expect(cmd).toContainText(`coyote up --data-dir ${suggested} --pg-port 7532`);
  await expect(page.getByText(/Nothing in your running database changed/)).toBeVisible();
  await axe(page, 'restore done');
  await shot(page, 'backups-restore-4-done-1440-light');
  await page.setViewportSize({ width: 360, height: 780 });
  await noOverflow(page, 'restore done 360');
  await shot(page, 'backups-restore-4-done-360-light');
  // Only dry runs, then one real request with the moment and the folder.
  const real = bodies.filter((b) => !(b as { dry_run?: boolean }).dry_run);
  expect(real).toHaveLength(1);
  expect(real[0]).toMatchObject({ into: suggested });
  expect((real[0] as { to?: string }).to).toMatch(/^\d{4}-\d{2}-\d{2}T/);
  expect(problems).toEqual([]);
});

test('page: check a backup (passes), and a check that finds damage', async ({ page, request }) => {
  const problems = watchConsole(page);
  await scenario(request, 'two-healthy');
  await page.setViewportSize({ width: 1440, height: 1000 });
  await signIn(page, '/backups');
  await page.getByRole('button', { name: 'Check a backup…' }).click();
  const dlg = page.getByRole('dialog', { name: 'Check the newest backup?' });
  await expect(dlg).toContainText('Your database keeps running.');
  await dlg.getByRole('button', { name: 'Check it' }).click();
  await expect(page.getByRole('heading', { name: 'Checking the newest backup…' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Restore to a moment…' })).toBeDisabled();
  await shot(page, 'backups-check-running-1440-light');
  await expect(page.getByRole('heading', { name: 'Backup check passed' })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText(/3 databases, 811 tables and indexes/)).toBeVisible();
  await axe(page, 'check passed');
  await shot(page, 'backups-check-passed-1440-light');

  await scenario(request, 'backups-check-fails');
  await page.reload();
  await page.getByRole('button', { name: 'Check a backup…' }).click();
  await page.getByRole('dialog', { name: 'Check the newest backup?' }).getByRole('button', { name: 'Check it' }).click();
  await expect(page.getByRole('heading', { name: 'Backup check did not finish' })).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText(/page 812 has a bad checksum/).first()).toBeVisible();
  await expect(page.getByText(/Your running database is fine; this backup is not/)).toBeVisible();
  await shot(page, 'backups-check-failed-1440-light');
  expect(problems).toEqual([]);
});
