import { defineConfig, devices } from '@playwright/test';

/**
 * End-to-end configuration.
 *
 * These tests drive a real browser against a real neelkitd with a real sandbox.
 * They are the only tests in the project that can catch a gap between what the
 * interface promises and what the server does — a unit test can assert the
 * policy gate returns `confirm`, but only this can assert the user is actually
 * stopped.
 *
 * Both processes must already be running. Starting them here would hide the
 * setup from anyone reading the file, and the setup is the interesting part:
 * see `make e2e` and the `e2e` job in .github/workflows/ci.yaml.
 */
export default defineConfig({
  testDir: './e2e',
  // One worker. The tests share a single server, a single workspace directory
  // and a single audit log; running them in parallel would make failures depend
  // on scheduling, which is the worst kind of flake to debug.
  workers: 1,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: process.env.CI ? [['github'], ['list']] : [['list']],
  timeout: 30_000,
  expect: { timeout: 10_000 },

  use: {
    baseURL: process.env.NEELKIT_WEB_URL ?? 'http://127.0.0.1:3000',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },

  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        // The sandbox image ships Chromium at a known path; fall back to the
        // bundled download when it is absent so this works on a laptop too.
        launchOptions: process.env.PLAYWRIGHT_CHROMIUM_PATH
          ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_PATH }
          : {},
      },
    },
  ],
});
