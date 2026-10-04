import { readFile } from 'node:fs/promises';
import process from 'node:process';

const apiURL = process.env.E2B_API_URL?.replace(/\/$/, '');
const apiKey = process.env.E2B_API_KEY;
const sourceCommit = process.env.SOURCE_COMMIT;
const sourceImage =
  process.env.BROWSER_SOURCE_IMAGE ??
  'mcr.microsoft.com/playwright:v1.55.0-noble@sha256:ffc33305f7b4b04057ae4a0caa70aad4fde87454fb403a1a22e7f931707dfcf9';

if (!apiURL || !apiKey) {
  throw new Error('E2B_API_URL and E2B_API_KEY are required');
}
if (!/^[0-9a-f]{40}$/.test(sourceCommit ?? '')) {
  throw new Error('SOURCE_COMMIT must be a full Git commit');
}
if (!sourceImage.includes('@sha256:')) {
  throw new Error('BROWSER_SOURCE_IMAGE must be digest-pinned');
}

const templateName =
  process.env.BROWSER_TEMPLATE_NAME ?? `brezel_browser_${sourceCommit.slice(0, 8)}`;
if (!/^[a-z0-9][a-z0-9_-]{0,63}$/.test(templateName)) {
  throw new Error('BROWSER_TEMPLATE_NAME is invalid');
}

const launcher = await readFile(new URL('./launch-browser.sh', import.meta.url));
const relay = await readFile(new URL('./cdp-relay.py', import.meta.url));
const encodedLauncher = launcher.toString('base64');
const encodedRelay = relay.toString('base64');

async function request(path, init = {}) {
  const response = await fetch(`${apiURL}${path}`, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      'X-API-Key': apiKey,
      ...(init.headers ?? {}),
    },
  });
  const body = await response.text();
  if (!response.ok) {
    throw new Error(
      `${init.method ?? 'GET'} ${path} failed with ${response.status}: ${body.slice(0, 500)}`,
    );
  }
  return body ? JSON.parse(body) : undefined;
}

const build = await request('/v3/templates', {
  method: 'POST',
  body: JSON.stringify({
    name: templateName,
    cpuCount: 4,
    memoryMB: 8192,
    minFreeDiskMb: 40960,
  }),
});

const installCommand = [
  'set -eu',
  'install -d -m 0755 /usr/local/bin',
  `printf %s '${encodedLauncher}' | base64 -d > /usr/local/bin/brezel-browser`,
  `printf %s '${encodedRelay}' | base64 -d > /usr/local/bin/brezel-cdp-relay`,
  'chmod 0755 /usr/local/bin/brezel-browser /usr/local/bin/brezel-cdp-relay',
  'test -x /ms-playwright/chromium-1187/chrome-linux/chrome',
  'ln -sfn /ms-playwright/chromium-1187/chrome-linux/chrome /usr/local/bin/brezel-chromium',
  '/usr/local/bin/brezel-browser status >/dev/null 2>&1 && exit 1 || test $? -eq 1',
].join('; ');

await request(
  `/v2/templates/${encodeURIComponent(build.templateID)}/builds/${encodeURIComponent(build.buildID)}`,
  {
    method: 'POST',
    body: JSON.stringify({
      fromImage: sourceImage,
      force: true,
      steps: [{ type: 'RUN', force: true, args: [installCommand, 'root'] }],
    }),
  },
);

let logsOffset = 0;
for (;;) {
  const status = await request(
    `/templates/${encodeURIComponent(build.templateID)}/builds/${encodeURIComponent(build.buildID)}/status?logsOffset=${logsOffset}`,
  );
  for (const entry of status.logEntries ?? []) {
    process.stderr.write(`${entry.level ?? 'info'}: ${entry.message ?? ''}\n`);
  }
  logsOffset += (status.logEntries ?? []).length;
  if (status.status === 'ready') break;
  if (status.status === 'error') {
    throw new Error(status.reason?.message ?? 'browser template build failed');
  }
  if (status.status !== 'building' && status.status !== 'waiting') {
    throw new Error(`unexpected browser template status: ${status.status}`);
  }
  await new Promise((resolve) => setTimeout(resolve, 1000));
}

process.stdout.write(
  `${JSON.stringify({
    build_id: build.buildID,
    reference: `${build.templateID}:${build.buildID}`,
    source_commit: sourceCommit,
    source_image: sourceImage,
    template_id: build.templateID,
  })}\n`,
);
