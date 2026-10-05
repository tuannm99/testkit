import { defineConfig } from '@playwright/test';

// TestKit runs this inside the pinned Playwright image (step ui.run):
//   BASE_URL      service under test, as seen on the test network
//   TK_OUT        evidence directory of the execution (mounted)
// Screenshots and traces are evidence; pass/fail comes from the assertions.
const out = process.env.TK_OUT ?? 'test-results';

export default defineConfig({
  testDir: './tests',
  timeout: 30_000,
  expect: { timeout: 10_000 },  // web-first assertions poll; tests never sleep
  retries: 0,                   // flakiness is detected by TestKit (--retries), never hidden here
  workers: 1,
  outputDir: `${out}/artifacts`,
  reporter: [['list'], ['json', { outputFile: `${out}/results.json` }]],
  use: {
    baseURL: process.env.BASE_URL,
    screenshot: 'on',
    trace: 'on',
    video: 'off',
    locale: 'vi-VN',
    timezoneId: 'Asia/Ho_Chi_Minh',
  },
});
