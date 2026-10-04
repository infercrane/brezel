import process from 'node:process';

import { chromium } from 'playwright-core';

const mode = process.argv[2] ?? 'probe';
if (!['probe', 'set-state', 'check-state'].includes(mode)) {
  throw new Error('mode must be probe, set-state, or check-state');
}

let connectURL = '';
for await (const chunk of process.stdin) {
  connectURL += chunk;
}
connectURL = connectURL.trim();
if (!connectURL.startsWith('wss://') && !connectURL.startsWith('ws://')) {
  throw new Error('a WebSocket connection URL is required on stdin');
}

const browser = await chromium.connectOverCDP(connectURL, { timeout: 15_000 });
try {
  const context = browser.contexts()[0];
  if (!context) {
    throw new Error('Chromium did not expose its default context');
  }
  const page = context.pages()[0] ?? (await context.newPage());
  await page.goto('https://example.com/', {
    waitUntil: 'domcontentloaded',
    timeout: 15_000,
  });
  const title = await page.title();
  if (title !== 'Example Domain') {
    throw new Error(`unexpected page title: ${title}`);
  }

  if (mode === 'set-state') {
    await page.evaluate(() =>
      localStorage.setItem('brezel-browser-qualification', 'persisted'),
    );
  }
  if (mode === 'check-state') {
    const value = await page.evaluate(() =>
      localStorage.getItem('brezel-browser-qualification'),
    );
    if (value !== 'persisted') {
      throw new Error(`durable browser state mismatch: ${value}`);
    }
  }

  process.stdout.write(
    `${JSON.stringify({
      browser: browser.version(),
      mode,
      title,
      url: page.url(),
    })}\n`,
  );
} finally {
  await browser.close();
}
