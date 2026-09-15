import { performance } from "node:perf_hooks";

import { createBrezelComputeFromEnv } from "./adapter.mjs";

function boundedInteger(name, fallback, minimum, maximum) {
  const raw = process.env[name] ?? String(fallback);
  if (!/^[0-9]+$/.test(raw)) throw new Error(`${name} must be an integer`);
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    throw new Error(`${name} must be between ${minimum} and ${maximum}`);
  }
  return value;
}

function percentile(values, fraction) {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.max(0, Math.ceil(sorted.length * fraction) - 1)];
}

async function oneBurst(compute, concurrency, repetition) {
  const attempts = Array.from({ length: concurrency }, (_, index) => ({ repetition, index, cleanup: "not-started" }));
  const handles = new Map();
  const wallStarted = performance.now();
  let firstReadyMs;
  await Promise.all(attempts.map(async (attempt) => {
    const started = performance.now();
    try {
      const sandbox = await compute.sandbox.create();
      handles.set(attempt.index, sandbox);
      const result = await sandbox.runCommand("node -v", { timeout: 30_000 });
      attempt.ttiMs = performance.now() - started;
      attempt.exitCode = result.exitCode;
      attempt.version = result.stdout.trim();
      if (result.exitCode !== 0) throw new Error(`node -v exited with ${result.exitCode}`);
      firstReadyMs ??= performance.now() - wallStarted;
    } catch (error) {
      attempt.error = error instanceof Error ? error.message : String(error);
    }
  }));
  const wallClockMs = performance.now() - wallStarted;
  const commandReady = attempts.filter((attempt) => attempt.exitCode === 0 && !attempt.error).length;
  const held = await compute.sandbox.list();
  const simultaneouslyHeld = held.length;
  await Promise.all(attempts.map(async (attempt) => {
    const sandbox = handles.get(attempt.index);
    if (!sandbox) return;
    try {
      await sandbox.destroy();
      attempt.cleanup = "confirmed";
    } catch (error) {
      attempt.cleanup = "failed";
      attempt.cleanupError = error instanceof Error ? error.message : String(error);
    }
  }));
  const values = attempts.filter((attempt) => attempt.exitCode === 0 && !attempt.error).map((attempt) => attempt.ttiMs);
  return {
    repetition,
    wallClockMs,
    timeToFirstReadyMs: firstReadyMs,
    commandReady,
    simultaneouslyHeld,
    summary: values.length === 0 ? null : {
      p50Ms: percentile(values, 0.50),
      p95Ms: percentile(values, 0.95),
      p99Ms: percentile(values, 0.99),
      successRate: values.length / concurrency,
    },
    attempts,
  };
}

async function qualifyGuest(compute) {
  const sandbox = await compute.sandbox.create();
  try {
    const result = await sandbox.runCommand(`set -eu
cpus=$(getconf _NPROCESSORS_ONLN)
memory_kib=$(awk '/^MemTotal:/ {print $2; exit}' /proc/meminfo)
free_root_kib=$(df -Pk / | awk 'NR == 2 {print $4}')
printf '{"cpus":%s,"memory_kib":%s,"free_root_kib":%s}\n' "$cpus" "$memory_kib" "$free_root_kib"
test "$cpus" -ge 2
test "$memory_kib" -ge 491520
test "$free_root_kib" -ge 524288
command -v node >/dev/null`);
    if (result.exitCode !== 0) {
      throw new Error(`burst guest shape preflight failed with exit ${result.exitCode}: stdout=${JSON.stringify(result.stdout.slice(-1000))} stderr=${JSON.stringify(result.stderr.slice(-1000))}`);
    }
    return JSON.parse(result.stdout.trim().split("\n").at(-1));
  } finally {
    await sandbox.destroy();
  }
}

async function run() {
  const runStartedAt = new Date().toISOString();
  const concurrency = boundedInteger("BREZEL_COMPUTESDK_BURST_CONCURRENCY", 100, 100, 1000);
  const repetitions = boundedInteger("BREZEL_COMPUTESDK_BURST_REPETITIONS", 3, 3, 20);
  const compute = createBrezelComputeFromEnv();
  const sourceRevision = process.env.BREZEL_SOURCE_REVISION ?? "";
  if (!/^[0-9a-f]{40}$/.test(sourceRevision)) throw new Error("BREZEL_SOURCE_REVISION must be the exact 40-character Brezel git revision");
  const environmentRevision = process.env.BREZEL_ENVIRONMENT_REVISION;
  const endpoint = new URL(process.env.BREZEL_API_URL);
  const before = await compute.sandbox.list();
  if (before.length !== 0) throw new Error("the dedicated burst benchmark project must be empty before execution");
  const guest = await qualifyGuest(compute);
  if ((await compute.sandbox.list()).length !== 0) throw new Error("burst shape preflight cleanup was not confirmed");
  const runs = [];
  for (let repetition = 1; repetition <= repetitions; repetition += 1) {
    runs.push(await oneBurst(compute, concurrency, repetition));
    if ((await compute.sandbox.list()).length !== 0) break;
  }
  const attempts = runs.flatMap((entry) => entry.attempts);
  const successful = attempts.filter((attempt) => attempt.exitCode === 0 && !attempt.error);
  const after = await compute.sandbox.list();
  const cleanupConformant = after.length === 0 && attempts.every((attempt) => attempt.cleanup === "confirmed");
  const report = {
    schemaVersion: 1,
    suite: "computesdk-burst-tti-rehearsal",
    command: "node -v",
    startedAt: runStartedAt,
    finishedAt: new Date().toISOString(),
    provenance: {
      sourceRevision,
      environmentRevision,
      endpoint: `${endpoint.protocol}//${endpoint.host}${endpoint.pathname.replace(/\/$/, "")}`,
      runner: { node: process.version, platform: process.platform, arch: process.arch },
      measuredState: "fresh sandboxes after one disclosed shape preflight",
    },
    guest,
    concurrency,
    repetitions,
    requested: concurrency * repetitions,
    succeeded: successful.length,
    cleanupConfirmed: attempts.filter((attempt) => attempt.cleanup === "confirmed").length,
    projectEmptyBefore: before.length === 0,
    projectEmptyAfter: after.length === 0,
    cleanupConformant,
    runs,
  };
  process.stdout.write(`${JSON.stringify(report, null, 2)}\n`);
  if (successful.length !== concurrency * repetitions || !cleanupConformant ||
      runs.some((entry) => entry.commandReady !== concurrency || entry.simultaneouslyHeld !== concurrency)) process.exitCode = 1;
}

await run();
