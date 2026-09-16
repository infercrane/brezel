import { createHash } from "node:crypto";
import { spawn } from "node:child_process";
import {
  lstatSync,
  openSync,
  closeSync,
  readFileSync,
  readdirSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import {
  DAX_UPSTREAM,
  percentile,
  summarizeAttempts,
  transcriptValid,
} from "./dax-rehearsal.mjs";
import { DAX_PHASES } from "./dax-transcript.mjs";

const SCHEMA_VERSION = 1;
const CONFIG_SUITE = "brezel-dax-paired-ab-config";
const PLAN_SUITE = "brezel-dax-paired-ab-plan";
const REPORT_SUITE = "brezel-dax-paired-ab-report";
const DAX_REPORT_SUITE = "computesdk-dax-rehearsal";
const MAX_JSON_BYTES = 64 * 1024 * 1024;
const RUN_TIMEOUT_MS = 45 * 60 * 1000;
const DIGEST = /^[0-9a-f]{64}$/;
const REVISION = /^[0-9a-f]{40}$/;
const SHORT_ID = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;
const SEED = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/;
const SLOT_ID = /^[a-z0-9][a-z0-9-]{0,63}$/;
const METRICS = Object.freeze([
  ...DAX_PHASES.map((phase) => ({ name: phase, source: "phase", phase })),
  { name: "provider_total_ms", source: "attempt", field: "totalMs" },
  { name: "create_ms", source: "attempt", field: "createMs" },
  { name: "destroy_ms", source: "attempt", field: "destroyMs" },
]);

function fail(message) {
  throw new Error(message);
}

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function exactKeys(value, keys, label) {
  if (!isObject(value)) fail(`${label} must be an object`);
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    fail(`${label} keys must be exactly ${expected.join(", ")}`);
  }
}

function integer(value, minimum, maximum, label) {
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    fail(`${label} must be an integer between ${minimum} and ${maximum}`);
  }
  return value;
}

function positiveNumber(value, label) {
  if (typeof value !== "number" || !Number.isFinite(value) || value <= 0) {
    fail(`${label} must be a positive finite number`);
  }
  return value;
}

function stringMatching(value, expression, label) {
  if (typeof value !== "string" || !expression.test(value)) fail(`${label} is invalid`);
  return value;
}

function canonicalValue(value) {
  if (Array.isArray(value)) return value.map(canonicalValue);
  if (isObject(value)) {
    return Object.fromEntries(Object.keys(value).sort().map((key) => [key, canonicalValue(value[key])]));
  }
  return value;
}

export function canonicalJSON(value) {
  return JSON.stringify(canonicalValue(value));
}

export function sha256JSON(value) {
  return createHash("sha256").update(canonicalJSON(value)).digest("hex");
}

function hashFile(path) {
  return createHash("sha256").update(readProtectedFile(path)).digest("hex");
}

function canonicalEndpoint(value) {
  let endpoint;
  try {
    endpoint = new URL(value);
  } catch {
    fail("commonIdentity.endpoint must be an absolute URL");
  }
  if (endpoint.username || endpoint.password || endpoint.hash) {
    fail("commonIdentity.endpoint must not contain credentials or a fragment");
  }
  const host = endpoint.hostname.toLowerCase();
  const loopback = host === "localhost" || host === "::1" || /^127(?:\.\d{1,3}){3}$/.test(host);
  if (endpoint.protocol !== "https:" && !(endpoint.protocol === "http:" && loopback)) {
    fail("commonIdentity.endpoint must use HTTPS (loopback HTTP is allowed for development)");
  }
  endpoint.pathname = endpoint.pathname.replace(/\/+$/, "");
  endpoint.search = "";
  return `${endpoint.protocol}//${endpoint.host}${endpoint.pathname}`;
}

function validateGuest(value, label) {
  exactKeys(value, ["cpus", "architecture", "memoryKiB", "minimumFreeRootKiB", "uid"], label);
  return {
    cpus: integer(value.cpus, 1, 1024, `${label}.cpus`),
    architecture: stringMatching(value.architecture, /^(?:x86_64|aarch64)$/, `${label}.architecture`),
    memoryKiB: integer(value.memoryKiB, 1, Number.MAX_SAFE_INTEGER, `${label}.memoryKiB`),
    minimumFreeRootKiB: integer(value.minimumFreeRootKiB, 1, Number.MAX_SAFE_INTEGER, `${label}.minimumFreeRootKiB`),
    uid: integer(value.uid, 0, 2 ** 31 - 1, `${label}.uid`),
  };
}

function validateArm(value, label) {
  exactKeys(value, ["environmentRevision", "configurationIdentitySha256", "guest"], label);
  return {
    environmentRevision: stringMatching(value.environmentRevision, SHORT_ID, `${label}.environmentRevision`),
    configurationIdentitySha256: stringMatching(value.configurationIdentitySha256, DIGEST, `${label}.configurationIdentitySha256`),
    guest: validateGuest(value.guest, `${label}.guest`),
  };
}

export function validateConfig(value) {
  exactKeys(value, ["schemaVersion", "suite", "seed", "pairs", "iterationsPerSlot", "bootstrapSamples", "commonIdentity", "arms"], "config");
  if (value.schemaVersion !== SCHEMA_VERSION || value.suite !== CONFIG_SUITE) fail("config schema or suite is unsupported");
  exactKeys(value.commonIdentity, ["sourceRevision", "endpoint", "region", "hostIdentitySha256"], "config.commonIdentity");
  exactKeys(value.arms, ["baseline", "candidate"], "config.arms");
  const config = {
    schemaVersion: SCHEMA_VERSION,
    suite: CONFIG_SUITE,
    seed: stringMatching(value.seed, SEED, "config.seed"),
    pairs: integer(value.pairs, 5, 50, "config.pairs"),
    iterationsPerSlot: integer(value.iterationsPerSlot, 3, 20, "config.iterationsPerSlot"),
    bootstrapSamples: integer(value.bootstrapSamples, 1_000, 100_000, "config.bootstrapSamples"),
    commonIdentity: {
      sourceRevision: stringMatching(value.commonIdentity.sourceRevision, REVISION, "config.commonIdentity.sourceRevision"),
      endpoint: canonicalEndpoint(value.commonIdentity.endpoint),
      region: stringMatching(value.commonIdentity.region, /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/, "config.commonIdentity.region"),
      hostIdentitySha256: stringMatching(value.commonIdentity.hostIdentitySha256, DIGEST, "config.commonIdentity.hostIdentitySha256"),
    },
    arms: {
      baseline: validateArm(value.arms.baseline, "config.arms.baseline"),
      candidate: validateArm(value.arms.candidate, "config.arms.candidate"),
    },
  };
  if (config.arms.baseline.configurationIdentitySha256 === config.arms.candidate.configurationIdentitySha256) {
    fail("baseline and candidate configuration identities must differ");
  }
  return config;
}

function seededRandom(seed) {
  const digest = createHash("sha256").update(seed).digest();
  let state = digest.readUInt32LE(0) || 0x6d2b79f5;
  return () => {
    state += 0x6d2b79f5;
    let value = state;
    value = Math.imul(value ^ (value >>> 15), value | 1);
    value ^= value + Math.imul(value ^ (value >>> 7), value | 61);
    return ((value ^ (value >>> 14)) >>> 0) / 4294967296;
  };
}

function shuffled(values, random) {
  const result = [...values];
  for (let index = result.length - 1; index > 0; index -= 1) {
    const selected = Math.floor(random() * (index + 1));
    [result[index], result[selected]] = [result[selected], result[index]];
  }
  return result;
}

function buildSchedule(seed, pairs) {
  const random = seededRandom(`brezel-dax-paired-ab:${seed}`);
  const orientations = [];
  for (let index = 0; index < pairs; index += 1) orientations.push(index % 2 === 0 ? "baseline-first" : "candidate-first");
  const randomized = shuffled(orientations, random);
  return randomized.flatMap((orientation, index) => {
    const pair = index + 1;
    const arms = orientation === "baseline-first" ? ["baseline", "candidate"] : ["candidate", "baseline"];
    return arms.map((arm, position) => ({
      sequence: (pair - 1) * 2 + position + 1,
      pair,
      position: position + 1,
      arm,
      slotId: `pair-${String(pair).padStart(3, "0")}-${arm}`,
    }));
  });
}

export function buildPlan(input) {
  const config = validateConfig(input);
  const body = {
    schemaVersion: SCHEMA_VERSION,
    suite: PLAN_SUITE,
    seed: config.seed,
    pairs: config.pairs,
    iterationsPerSlot: config.iterationsPerSlot,
    bootstrapSamples: config.bootstrapSamples,
    commonIdentity: config.commonIdentity,
    arms: config.arms,
    schedule: buildSchedule(config.seed, config.pairs),
  };
  return { ...body, planId: sha256JSON(body) };
}

export function validatePlan(value) {
  exactKeys(value, ["schemaVersion", "suite", "seed", "pairs", "iterationsPerSlot", "bootstrapSamples", "commonIdentity", "arms", "schedule", "planId"], "plan");
  const config = validateConfig({
    schemaVersion: value.schemaVersion,
    suite: CONFIG_SUITE,
    seed: value.seed,
    pairs: value.pairs,
    iterationsPerSlot: value.iterationsPerSlot,
    bootstrapSamples: value.bootstrapSamples,
    commonIdentity: value.commonIdentity,
    arms: value.arms,
  });
  const expected = buildPlan(config);
  if (canonicalJSON(value) !== canonicalJSON(expected)) fail("plan content, schedule, or planId is invalid");
  return expected;
}

function readProtectedFile(path) {
  const info = lstatSync(path);
  if (!info.isFile() || info.isSymbolicLink() || info.nlink !== 1) fail(`${path} must be one regular, non-linked file`);
  if (info.size <= 0 || info.size > MAX_JSON_BYTES) fail(`${path} is empty or exceeds the evidence size limit`);
  return readFileSync(path);
}

function readJSON(path, label) {
  try {
    return JSON.parse(readProtectedFile(path).toString("utf8"));
  } catch (error) {
    fail(`${label} is not valid bounded JSON: ${error instanceof Error ? error.message : String(error)}`);
  }
}

function requireResultDirectory(path) {
  const resolved = resolve(path);
  const info = lstatSync(resolved);
  if (!info.isDirectory() || info.isSymbolicLink()) fail("report directory must be an existing non-symlink directory");
  return resolved;
}

function reportPath(directory, slot) {
  return resolve(directory, `${slot.slotId}.report.json`);
}

function writeExclusive(path, value) {
  const descriptor = openSync(path, "wx", 0o600);
  try {
    writeFileSync(descriptor, value);
  } finally {
    closeSync(descriptor);
  }
}

function sameJSON(left, right) {
  return canonicalJSON(left) === canonicalJSON(right);
}

function validTimestamp(value, label) {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/.test(value) || Number.isNaN(Date.parse(value))) {
    fail(`${label} must be a UTC ISO timestamp with milliseconds`);
  }
  return Date.parse(value);
}

function validateReportSummary(report) {
  const expected = summarizeAttempts(report.attempts);
  if (!sameJSON(report.summary, expected)) fail("DAX report summary does not match its attempts");
}

function validateAttempt(attempt, index, guest, reportStarted, reportFinished) {
  const label = `attempt ${index + 1}`;
  exactKeys(attempt, ["iteration", "startedAt", "cleanup", "createMs", "buildStartedAt", "totalMs", "buildFinishedAt", "exitCode", "result", "stderrTail", "destroyMs", "finishedAt"], label);
  exactKeys(attempt.result, ["phases", "metadata", "disk", "cache", "failures", "errors", "completedCommit", "markerSequence", "transcriptErrors", "parseIssues", "executionFailures"], `${label}.result`);
  if (attempt.iteration !== index + 1) fail(`${label} has the wrong iteration`);
  if (attempt.exitCode !== 0 || attempt.error !== undefined) fail(`${label} did not succeed`);
  if (attempt.cleanup !== "confirmed") fail(`${label} cleanup was not confirmed`);
  positiveNumber(attempt.createMs, `${label}.createMs`);
  positiveNumber(attempt.totalMs, `${label}.totalMs`);
  positiveNumber(attempt.destroyMs, `${label}.destroyMs`);
  const started = validTimestamp(attempt.startedAt, `${label}.startedAt`);
  const buildStarted = validTimestamp(attempt.buildStartedAt, `${label}.buildStartedAt`);
  const buildFinished = validTimestamp(attempt.buildFinishedAt, `${label}.buildFinishedAt`);
  const finished = validTimestamp(attempt.finishedAt, `${label}.finishedAt`);
  if (!(reportStarted <= started && started <= buildStarted && buildStarted <= buildFinished && buildFinished <= finished && finished <= reportFinished)) {
    fail(`${label} timestamps are not monotonic within the report boundary`);
  }
  if (!transcriptValid(attempt.result, guest) || Number(attempt.result.metadata.memory_kib) !== guest.memory_kib) {
    fail(`${label} does not contain an exact valid pinned DAX transcript`);
  }
  return { started, finished };
}

export function validateSlotReport(report, plan, slot) {
  exactKeys(report, ["schemaVersion", "suite", "upstream", "region", "startedAt", "finishedAt", "provenance", "methodology", "guest", "requested", "succeeded", "cleanupConfirmed", "projectEmptyBefore", "projectEmptyAfter", "cleanupConformant", "summary", "attempts"], slot.slotId);
  if (report.schemaVersion !== 2 || report.suite !== DAX_REPORT_SUITE) fail(`${slot.slotId} is not a supported DAX report`);
  if (!sameJSON(report.upstream, DAX_UPSTREAM)) fail(`${slot.slotId} did not run the pinned upstream DAX workload`);
  if (report.region !== plan.commonIdentity.region) fail(`${slot.slotId} region differs from the plan`);
  const arm = plan.arms[slot.arm];
  const expectedPaired = {
    planId: plan.planId,
    slotId: slot.slotId,
    hostIdentitySha256: plan.commonIdentity.hostIdentitySha256,
    configurationIdentitySha256: arm.configurationIdentitySha256,
  };
  exactKeys(report.provenance, ["sourceRevision", "environmentRevision", "endpoint", "runner", "allowInternet", "measuredState", "pairedAB"], `${slot.slotId}.provenance`);
  exactKeys(report.provenance.runner, ["node", "platform", "arch"], `${slot.slotId}.provenance.runner`);
  exactKeys(report.provenance.pairedAB, ["planId", "slotId", "hostIdentitySha256", "configurationIdentitySha256"], `${slot.slotId}.provenance.pairedAB`);
  if (report.provenance.sourceRevision !== plan.commonIdentity.sourceRevision ||
      report.provenance.environmentRevision !== arm.environmentRevision ||
      canonicalEndpoint(report.provenance.endpoint) !== plan.commonIdentity.endpoint ||
      report.provenance.allowInternet !== true || !sameJSON(report.provenance.pairedAB, expectedPaired)) {
    fail(`${slot.slotId} provenance does not match the immutable plan identity`);
  }
  const expectedMethodology = {
    iterations: plan.iterationsPerSlot,
    concurrency: 1,
    sandboxState: "fresh sandbox per iteration",
    scoredBoundary: "runCommand request through complete command result",
    upstreamPhaseOrder: DAX_PHASES,
    cleanupBoundary: "destroy confirmation followed by empty-project inventory",
  };
  if (!sameJSON(report.methodology, expectedMethodology)) fail(`${slot.slotId} methodology differs from the plan`);
  if (report.requested !== plan.iterationsPerSlot || report.succeeded !== plan.iterationsPerSlot ||
      report.cleanupConfirmed !== plan.iterationsPerSlot || report.projectEmptyBefore !== true ||
      report.projectEmptyAfter !== true || report.cleanupConformant !== true ||
      !Array.isArray(report.attempts) || report.attempts.length !== plan.iterationsPerSlot) {
    fail(`${slot.slotId} is incomplete or cleanup-nonconformant`);
  }
  const expectedGuest = arm.guest;
  exactKeys(report.guest, ["cpus", "memory_kib", "free_root_kib", "uid", "architecture"], `${slot.slotId}.guest`);
  if (report.guest.cpus !== expectedGuest.cpus || report.guest.architecture !== expectedGuest.architecture ||
      report.guest.memory_kib !== expectedGuest.memoryKiB || report.guest.free_root_kib < expectedGuest.minimumFreeRootKiB ||
      report.guest.uid !== expectedGuest.uid) {
    fail(`${slot.slotId} guest identity or qualified capacity differs from the plan`);
  }
  const reportStarted = validTimestamp(report.startedAt, `${slot.slotId}.startedAt`);
  const reportFinished = validTimestamp(report.finishedAt, `${slot.slotId}.finishedAt`);
  if (reportStarted >= reportFinished) fail(`${slot.slotId} report timestamps are not increasing`);
  let previousFinished = reportStarted;
  for (let index = 0; index < report.attempts.length; index += 1) {
    const boundary = validateAttempt(report.attempts[index], index, report.guest, reportStarted, reportFinished);
    if (boundary.started < previousFinished) fail(`${slot.slotId} attempts overlap or are reordered`);
    previousFinished = boundary.finished;
  }
  validateReportSummary(report);
  return { reportStarted, reportFinished };
}

function presentReports(plan, directory, strictComplete) {
  const expected = new Set(plan.schedule.map((slot) => `${slot.slotId}.report.json`));
  const unexpected = readdirSync(directory).filter((name) => name.endsWith(".report.json") && !expected.has(name));
  if (unexpected.length !== 0) fail(`report directory contains unexpected DAX reports: ${unexpected.join(", ")}`);
  const reports = new Map();
  let gap = false;
  for (const slot of plan.schedule) {
    const path = reportPath(directory, slot);
    let exists = false;
    try {
      exists = statSync(path).isFile();
    } catch (error) {
      if (error?.code !== "ENOENT") throw error;
    }
    if (!exists) {
      gap = true;
      if (strictComplete) fail(`missing report for ${slot.slotId}`);
      continue;
    }
    if (gap) fail(`report ${slot.slotId} exists after an unfilled schedule slot`);
    const report = readJSON(path, slot.slotId);
    validateSlotReport(report, plan, slot);
    reports.set(slot.slotId, { report, path, sha256: hashFile(path) });
  }
  return reports;
}

export function nextSlot(planInput, directoryInput) {
  const plan = validatePlan(planInput);
  const directory = requireResultDirectory(directoryInput);
  const reports = presentReports(plan, directory, false);
  return plan.schedule.find((slot) => !reports.has(slot.slotId)) ?? null;
}

function median(values) {
  if (values.length === 0) fail("cannot compute a median without values");
  const sorted = [...values].sort((a, b) => a - b);
  const middle = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 1 ? sorted[middle] : (sorted[middle - 1] + sorted[middle]) / 2;
}

function attemptMetric(attempt, metric) {
  return metric.source === "phase" ? attempt.result.phases[metric.phase] : attempt[metric.field];
}

function bootstrap(values, samples, seed, statistic) {
  const random = seededRandom(seed);
  const estimates = [];
  for (let sample = 0; sample < samples; sample += 1) {
    const resampled = [];
    for (let index = 0; index < values.length; index += 1) resampled.push(values[Math.floor(random() * values.length)]);
    estimates.push(statistic(resampled));
  }
  return { lower: percentile(estimates, 0.025), upper: percentile(estimates, 0.975) };
}

function analyzeMetric(metric, pairs, samples, seed) {
  const rows = pairs.map((pair) => {
    const baselineMs = median(pair.baseline.report.attempts.map((attempt) => attemptMetric(attempt, metric)));
    const candidateMs = median(pair.candidate.report.attempts.map((attempt) => attemptMetric(attempt, metric)));
    return {
      pair: pair.pair,
      baselineMs,
      candidateMs,
      deltaMs: candidateMs - baselineMs,
      relativeDelta: candidateMs / baselineMs - 1,
      speedup: baselineMs / candidateMs,
    };
  });
  const delta = rows.map((row) => row.deltaMs);
  const relative = rows.map((row) => row.relativeDelta);
  const speedup = rows.map((row) => row.speedup);
  return {
    unit: "randomized_same-host_pair",
    pairCount: rows.length,
    interpretation: "negative delta and relativeDelta, or speedup above 1, favor candidate",
    medianDeltaMs: median(delta),
    medianRelativeDelta: median(relative),
    medianSpeedup: median(speedup),
    ci95: {
      deltaMs: bootstrap(delta, samples, `${seed}:${metric.name}:delta`, median),
      relativeDelta: bootstrap(relative, samples, `${seed}:${metric.name}:relative`, median),
      speedup: bootstrap(speedup, samples, `${seed}:${metric.name}:speedup`, median),
    },
    pairs: rows,
  };
}

export function mergeReports(planInput, directoryInput) {
  const plan = validatePlan(planInput);
  const directory = requireResultDirectory(directoryInput);
  const reports = presentReports(plan, directory, true);
  let previousFinished = -Infinity;
  let runnerIdentity;
  let machineIdentity;
  for (const slot of plan.schedule) {
    const report = reports.get(slot.slotId).report;
    const started = Date.parse(report.startedAt);
    if (started < previousFinished) fail(`${slot.slotId} overlaps or precedes the previous scheduled slot`);
    previousFinished = Date.parse(report.finishedAt);
    if (runnerIdentity === undefined) runnerIdentity = report.provenance.runner;
    else if (!sameJSON(runnerIdentity, report.provenance.runner)) fail(`${slot.slotId} runner identity differs from earlier slots`);
    for (const attempt of report.attempts) {
      const observedMachine = {
        architecture: attempt.result.metadata.architecture,
        kernel: attempt.result.metadata.kernel,
        cpuModel: attempt.result.metadata.cpu_model,
      };
      if (machineIdentity === undefined) machineIdentity = observedMachine;
      else if (!sameJSON(machineIdentity, observedMachine)) fail(`${slot.slotId} machine identity differs from earlier attempts`);
    }
  }
  const pairs = [];
  for (let pair = 1; pair <= plan.pairs; pair += 1) {
    const slots = plan.schedule.filter((slot) => slot.pair === pair);
    const baselineSlot = slots.find((slot) => slot.arm === "baseline");
    const candidateSlot = slots.find((slot) => slot.arm === "candidate");
    pairs.push({
      pair,
      baseline: reports.get(baselineSlot.slotId),
      candidate: reports.get(candidateSlot.slotId),
    });
  }
  return {
    schemaVersion: SCHEMA_VERSION,
    suite: REPORT_SUITE,
    planId: plan.planId,
    identities: {
      sourceRevision: plan.commonIdentity.sourceRevision,
      endpoint: plan.commonIdentity.endpoint,
      region: plan.commonIdentity.region,
      hostIdentitySha256: plan.commonIdentity.hostIdentitySha256,
      baselineConfigurationIdentitySha256: plan.arms.baseline.configurationIdentitySha256,
      candidateConfigurationIdentitySha256: plan.arms.candidate.configurationIdentitySha256,
      runner: runnerIdentity,
      observedMachine: machineIdentity,
    },
    methodology: {
      schedule: "seeded balanced within-pair baseline/candidate order",
      slotStatistic: `median of ${plan.iterationsPerSlot} fresh-sandbox attempts`,
      inferenceUnit: `median paired difference across ${plan.pairs} same-host pairs`,
      confidenceInterval: `deterministic percentile bootstrap over pairs (${plan.bootstrapSamples} resamples)`,
      claimBoundary: "same-host causal engineering evidence; not hardware attestation or independent leaderboard evidence",
    },
    evidence: plan.schedule.map((slot) => ({
      sequence: slot.sequence,
      pair: slot.pair,
      arm: slot.arm,
      slotId: slot.slotId,
      reportSha256: reports.get(slot.slotId).sha256,
    })),
    metrics: Object.fromEntries(METRICS.map((metric) => [
      metric.name,
      analyzeMetric(metric, pairs, plan.bootstrapSamples, plan.seed),
    ])),
  };
}

function childEnvironment(plan, slot) {
  const expected = {
    BREZEL_API_URL: plan.commonIdentity.endpoint,
    BREZEL_SOURCE_REVISION: plan.commonIdentity.sourceRevision,
    BREZEL_BENCHMARK_REGION: plan.commonIdentity.region,
    BREZEL_ALLOW_INTERNET: "true",
  };
  for (const [name, value] of Object.entries(expected)) {
    if (process.env[name] !== value) fail(`${name} must exactly match the immutable paired plan`);
  }
  const arm = plan.arms[slot.arm];
  return {
    ...process.env,
    BREZEL_ENVIRONMENT_REVISION: arm.environmentRevision,
    BREZEL_COMPUTESDK_DAX_ITERATIONS: String(plan.iterationsPerSlot),
    BREZEL_DAX_PAIRED_PLAN_ID: plan.planId,
    BREZEL_DAX_PAIRED_SLOT_ID: slot.slotId,
    BREZEL_DAX_HOST_IDENTITY_SHA256: plan.commonIdentity.hostIdentitySha256,
    BREZEL_DAX_CONFIGURATION_IDENTITY_SHA256: arm.configurationIdentitySha256,
  };
}

async function executeRunner(plan, slot) {
  const runner = fileURLToPath(new URL("./dax-rehearsal.mjs", import.meta.url));
  const child = spawn(process.execPath, [runner], {
    env: childEnvironment(plan, slot),
    stdio: ["ignore", "pipe", "pipe"],
  });
  const stdout = [];
  const stderr = [];
  let stdoutBytes = 0;
  let stderrBytes = 0;
  let overflow;
  child.stdout.on("data", (chunk) => {
    stdoutBytes += chunk.length;
    if (stdoutBytes > MAX_JSON_BYTES) {
      overflow = "DAX runner stdout exceeded the evidence size limit";
      child.kill("SIGKILL");
    } else stdout.push(chunk);
  });
  child.stderr.on("data", (chunk) => {
    stderrBytes += chunk.length;
    if (stderrBytes > MAX_JSON_BYTES) {
      overflow = "DAX runner stderr exceeded the evidence size limit";
      child.kill("SIGKILL");
    } else stderr.push(chunk);
  });
  const timer = setTimeout(() => {
    overflow = `DAX runner exceeded ${RUN_TIMEOUT_MS} ms`;
    child.kill("SIGKILL");
  }, RUN_TIMEOUT_MS);
  const status = await new Promise((resolveStatus, reject) => {
    child.once("error", reject);
    child.once("close", (code, signal) => resolveStatus({ code, signal }));
  }).finally(() => clearTimeout(timer));
  return { ...status, stdout: Buffer.concat(stdout), stderr: Buffer.concat(stderr), overflow };
}

function requireAbsent(path) {
  try {
    lstatSync(path);
    fail(`${path} already exists; archive the prior immutable evidence before retrying this slot`);
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }
}

export async function runNext(planInput, directoryInput) {
  const plan = validatePlan(planInput);
  const directory = requireResultDirectory(directoryInput);
  const slot = nextSlot(plan, directory);
  if (!slot) fail("all paired DAX schedule slots already have reports");
  requireAbsent(reportPath(directory, slot));
  requireAbsent(resolve(directory, `${slot.slotId}.stderr.log`));
  requireAbsent(resolve(directory, `${slot.slotId}.stdout.log`));
  const result = await executeRunner(plan, slot);
  const stderrPath = resolve(directory, `${slot.slotId}.stderr.log`);
  writeExclusive(stderrPath, result.stderr);
  let report;
  try {
    report = JSON.parse(result.stdout.toString("utf8"));
  } catch {
    writeExclusive(resolve(directory, `${slot.slotId}.stdout.log`), result.stdout);
    fail(result.overflow ?? `DAX runner for ${slot.slotId} did not emit JSON (exit=${result.code}, signal=${result.signal})`);
  }
  const path = reportPath(directory, slot);
  writeExclusive(path, `${JSON.stringify(report, null, 2)}\n`);
  validateSlotReport(report, plan, slot);
  if (result.overflow || result.code !== 0 || result.signal !== null) {
    fail(result.overflow ?? `DAX runner for ${slot.slotId} failed (exit=${result.code}, signal=${result.signal})`);
  }
  return { slot, reportPath: path, reportSha256: hashFile(path), stderrPath };
}

function usage() {
  return "usage: dax-paired-ab.mjs plan CONFIG | next PLAN REPORT_DIR | run-next PLAN REPORT_DIR | merge PLAN REPORT_DIR";
}

export async function main(args = process.argv.slice(2)) {
  const [command, planOrConfigPath, directory, ...extra] = args;
  if (!command || !planOrConfigPath || extra.length !== 0 || (command !== "plan" && !directory)) fail(usage());
  if (command === "plan") {
    if (directory !== undefined) fail(usage());
    process.stdout.write(`${JSON.stringify(buildPlan(readJSON(resolve(planOrConfigPath), "config")), null, 2)}\n`);
    return;
  }
  const plan = validatePlan(readJSON(resolve(planOrConfigPath), "plan"));
  if (command === "next") {
    process.stdout.write(`${JSON.stringify(nextSlot(plan, directory), null, 2)}\n`);
    return;
  }
  if (command === "run-next") {
    process.stdout.write(`${JSON.stringify(await runNext(plan, directory), null, 2)}\n`);
    return;
  }
  if (command === "merge") {
    process.stdout.write(`${JSON.stringify(mergeReports(plan, directory), null, 2)}\n`);
    return;
  }
  fail(usage());
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    await main();
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
    process.exitCode = 1;
  }
}
