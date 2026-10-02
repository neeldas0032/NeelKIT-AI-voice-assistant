import { existsSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { expect, test, type Page } from '@playwright/test';

/**
 * The approval gate, end to end.
 *
 * Everything else in this project is in service of one promise: nothing that
 * changes your machine runs until you have read the exact command and agreed to
 * it. A unit test can prove the policy gate returns `confirm`. Only this can
 * prove the user is actually stopped, that the string they were shown is the
 * string that ran, and that denying it leaves the disk untouched.
 *
 * Requires a running neelkitd and web server. See `make e2e`.
 */

const KEY = process.env.NEELKIT_API_KEY;
const WORKSPACE = process.env.NEELKIT_WORKSPACE_ROOT;

test.beforeAll(() => {
  if (!KEY) throw new Error('set NEELKIT_API_KEY to the key neelkitd printed');
  if (!WORKSPACE) throw new Error('set NEELKIT_WORKSPACE_ROOT to the sandbox root');
});

/** Seeds the key the way a user would, before any script on the page runs. */
async function open(page: Page) {
  await page.addInitScript((k) => localStorage.setItem('neelkit.apiKey', k), KEY!);
  await page.goto('/');
  await expect(page.getByLabel('Type a command')).toBeVisible();
}

async function say(page: Page, text: string) {
  const box = page.getByLabel('Type a command');
  await box.click();
  await box.pressSequentially(text);
  await box.press('Enter');
}

const notesFile = () => join(WORKSPACE!, 'NOTES.md');

test.describe('the approval gate', () => {
  test('a read runs immediately, with no approval', async ({ page }) => {
    await open(page);
    await say(page, 'list the files');

    // The READ badge and a summary, and crucially no approval card: asking to
    // confirm a directory listing would train the user to click through every
    // prompt, which is how approval gates stop working.
    // The same sentence appears twice by design — once as the assistant's reply
    // and once as the tool block's summary — so the locator has to say which.
    await expect(
      page.getByText(/entr(y|ies) in the project root/).first(),
    ).toBeVisible();
    await expect(page.getByText('Approval required')).toHaveCount(0);
  });

  test('a write stops, shows the exact command, and writes nothing until approved', async ({
    page,
  }) => {
    rmSync(notesFile(), { force: true });
    await open(page);
    await say(page, 'make a note to check the approval gate');

    const card = page.locator('text=Approval required').locator('..').locator('..');
    await expect(page.getByText('Approval required')).toBeVisible();
    await expect(page.getByText('Will run')).toBeVisible();

    // The preview names the real file. If this string and the argv ever diverge
    // the whole promise is void, so it is asserted here as well as server-side.
    await expect(card).toContainText('NOTES.md');

    // Nothing on disk yet. This is the assertion the feature exists for.
    expect(existsSync(notesFile())).toBe(false);

    await page.getByRole('button', { name: /Approve/ }).click();

    await expect(page.getByText('Approval required')).toHaveCount(0);
    await expect
      .poll(() => existsSync(notesFile()), { timeout: 10_000 })
      .toBe(true);
    expect(readFileSync(notesFile(), 'utf8')).toContain('approval gate');
  });

  test('denying leaves the disk untouched', async ({ page }) => {
    rmSync(notesFile(), { force: true });
    await open(page);
    await say(page, 'make a note this should never be written');

    await expect(page.getByText('Approval required')).toBeVisible();
    await page.getByRole('button', { name: 'Deny' }).click();
    await expect(page.getByText('Approval required')).toHaveCount(0);

    // Give the server the same window the approve path gets, so a pass here
    // cannot be the test simply checking too early.
    await page.waitForTimeout(1500);
    expect(existsSync(notesFile())).toBe(false);
  });

  test('approving a path traversal does not get past the sandbox', async ({
    page,
  }) => {
    await open(page);
    await say(page, 'read the file ../../etc/passwd');

    // Layer one: the policy gate escalates a read — normally allowed outright —
    // because the path leaves the project.
    await expect(page.getByText('Approval required')).toBeVisible();
    await expect(page.getByText(/points outside the project tree/)).toBeVisible();

    // Now do the worst thing a user can do, and approve it anyway.
    await page.getByRole('button', { name: /Approve/ }).click();

    // Layer two refuses regardless. The gate and the sandbox are independent,
    // and the sandbox resolves paths against an os.Root handle, so consent
    // cannot widen it.
    await expect(page.getByText(/path is outside the workspace/)).toBeVisible();
    await expect(page.getByText(/root:x:0:0/)).toHaveCount(0);
  });
});
