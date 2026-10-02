import { expect, test, type Page } from '@playwright/test';

/**
 * The pages that describe the system to its user.
 *
 * These are cheap tests with a specific job: catch the class of bug where the
 * interface renders but is wired to nothing. A tools page that loads and shows
 * an empty list is indistinguishable from a working one at a glance, and it is
 * the exact failure an API rename produces.
 */

const KEY = process.env.NEELKIT_API_KEY;

test.beforeAll(() => {
  if (!KEY) throw new Error('set NEELKIT_API_KEY to the key neelkitd printed');
});

async function open(page: Page, path: string) {
  await page.addInitScript((k) => localStorage.setItem('neelkit.apiKey', k), KEY!);
  await page.goto(path);
}

test('the patch bay lists the real registry, grouped by namespace', async ({
  page,
}) => {
  await open(page, '/tools');

  await expect(page.getByText(/\d+ tools/)).toBeVisible();
  // The namespaces are the same prefixes that appear in every audit row.
  for (const ns of ['fs', 'git', 'notes']) {
    await expect(page.getByText(ns, { exact: true }).first()).toBeVisible();
  }
  // Risk is on screen for every tool, because it is the only thing on this page
  // that tells the user what a capability can do to them.
  await expect(page.getByText('read').first()).toBeVisible();
});

test('setup reports live subsystem health', async ({ page }) => {
  await open(page, '/settings');

  const status = page.getByRole('list').filter({ hasText: 'database' }).first();
  await expect(page.getByText('Service status')).toBeVisible();
  await expect(status.getByText('asr', { exact: true })).toBeVisible();
  await expect(status.getByText('database', { exact: true })).toBeVisible();
  // At least one check must be reporting ok, or the server under test is not
  // actually healthy and every other result in this suite is suspect.
  await expect(status.getByText('ok', { exact: true }).first()).toBeVisible();
});

test('history records what was said and can search it', async ({ page }) => {
  await open(page, '/');
  const box = page.getByLabel('Type a command');
  await box.click();
  await box.pressSequentially('list the files');
  await box.press('Enter');
  await expect(
    page.getByText(/entr(y|ies) in the project root/).first(),
  ).toBeVisible();

  await open(page, '/history');
  await expect(page.getByText('Threads')).toBeVisible();

  const search = page.getByLabel('Search transcripts');
  await search.click();
  // A prefix, not a whole word: transcript search uses prefix indexes rather
  // than the Porter stemmer precisely so this works.
  await search.pressSequentially('lis');
  await expect(page.getByText(/^\d+ match(es)?$/)).toBeVisible({ timeout: 10_000 });
});

test('the interface does not overflow horizontally on a phone', async ({
  page,
}) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await open(page, '/');
  await expect(page.getByLabel('Type a command')).toBeVisible();

  const overflows = await page.evaluate(
    () => document.documentElement.scrollWidth > window.innerWidth + 1,
  );
  expect(overflows).toBe(false);
});
