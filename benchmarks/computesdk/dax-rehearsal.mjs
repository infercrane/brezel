import { createHash } from "node:crypto";
import { performance } from "node:perf_hooks";
import { pathToFileURL } from "node:url";

import { createBrezelComputeFromEnv } from "./adapter.mjs";
import { definitiveExecutionFailures } from "./output-validation.mjs";

const UPSTREAM_COMMIT = "273927519c0ac6558d3e545eed9d59eb56a47ec7";
const UPSTREAM_SCRIPT_SHA256 = "58f4640ac170b366f87e8466a9ac1e9f50383faa59553246e4664c77af34d550";
const UPSTREAM_SCRIPT_URL = `https://raw.githubusercontent.com/computesdk/benchmarks/${UPSTREAM_COMMIT}/benchmarks/scripts/dax-benchmark.sh`;

function boundedInteger(name, fallback, minimum, maximum) {
  const raw = process.env[name] ?? String(fallback);
  if (!/^[0-9]+$/.test(raw)) throw new Error(`${name} must be an integer`);
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    throw new Error(`${name} must be between ${minimum} and ${maximum}`);
  }
  return value;
}

export function percentile(values, fraction) {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.max(0, Math.ceil(sorted.length * fraction) - 1)];
}

const REQUIRED_PHASES = ["prepare", "cache_clear", "bun_download", "bun_unpack", "clone", "install", "typecheck", "total"];
const REQUIRED_CACHE_STATES = Object.freeze({
  workspace: "fresh",
  bun: "empty",
  turbo: "empty",
});
const EXPECTED_WORKLOAD_COMMIT = "08fb47373509ba64b13441061314eeacf4264f51";
const EXPECTED_BUN_VERSION = "1.3.14";
const EXPECTED_NODE_VERSION = "v24.14.1";
const FULL_MARKER_SEQUENCE = [
  "phase:prepare",
  "cache:guest_page_cache", "cache:workspace", "cache:bun", "cache:turbo",
  "phase:cache_clear",
  "meta:commit", "meta:architecture", "meta:kernel", "meta:logical_cpus", "meta:cpu_model", "meta:memory_kib",
  "phase:bun_download", "phase:bun_unpack", "meta:bun_version", "meta:node_version",
  "phase:clone", "disk:after_clone", "phase:install", "disk:after_install",
  "phase:typecheck", "disk:after_typecheck", "done", "phase:total",
];

function recordOnce(target, counts, kind, key, value, issues) {
  counts[key] = (counts[key] ?? 0) + 1;
  if (counts[key] > 1) {
    issues.push(`duplicate ${kind} key ${key}`);
    return;
  }
  target[key] = value;
}

export function structuredLines(stdout, stderr = "") {
  const phases = {};
  const metadata = {};
  const disk = {};
  const cache = {};
  const counts = { phases: {}, metadata: {}, disk: {}, cache: {}, done: 0 };
  const failures = [];
  const errors = [];
  const parseIssues = [];
  const markerSequence = [];
  let completedCommit = "";
  for (const line of stdout.split("\n")) {
    if (!line.startsWith("BENCH_")) continue;
    const parts = line.split("\t");
    const [kind, key, value] = parts;
    if (kind === "BENCH_PHASE") {
      const parsed = Number(value);
      if (parts.length !== 3 || !/^[a-z_]+$/.test(key ?? "") || !/^(0|[1-9][0-9]*)$/.test(value ?? "") || !Number.isSafeInteger(parsed)) {
        parseIssues.push(`malformed BENCH_PHASE line for ${key || "unknown"}`);
      } else {
        recordOnce(phases, counts.phases, "phase", key, parsed, parseIssues);
        markerSequence.push(`phase:${key}`);
      }
    } else if (kind === "BENCH_META") {
      const emptyValueAllowed = key === "cpu_model";
      if (parts.length !== 3 || !/^[a-z_]+$/.test(key ?? "") || value === undefined || (!emptyValueAllowed && value === "")) {
        parseIssues.push(`malformed BENCH_META line for ${key || "unknown"}`);
      } else {
        recordOnce(metadata, counts.metadata, "metadata", key, value, parseIssues);
        markerSequence.push(`meta:${key}`);
      }
    } else if (kind === "BENCH_DISK") {
      const parsed = Number(value);
      if (parts.length !== 3 || !/^[a-z_]+$/.test(key ?? "") ||
          !/^(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:e\+[0-9]+)?$/.test(value ?? "") || !Number.isSafeInteger(parsed)) {
        parseIssues.push(`malformed BENCH_DISK line for ${key || "unknown"}`);
      } else {
        recordOnce(disk, counts.disk, "disk", key, parsed, parseIssues);
        markerSequence.push(`disk:${key}`);
      }
    } else if (kind === "BENCH_CACHE") {
      if (parts.length !== 3 || !/^[a-z_]+$/.test(key ?? "") || !/^[a-z_]+$/.test(value ?? "")) {
        parseIssues.push(`malformed BENCH_CACHE line for ${key || "unknown"}`);
      } else {
        recordOnce(cache, counts.cache, "cache", key, value, parseIssues);
        markerSequence.push(`cache:${key}`);
      }
    } else if (kind === "BENCH_DONE") {
      counts.done += 1;
      if (parts.length !== 2 || !/^[0-9a-f]{40}$/.test(key ?? "") || counts.done > 1) parseIssues.push("malformed or duplicate BENCH_DONE line");
      else {
        completedCommit = key;
        markerSequence.push("done");
      }
    } else if (kind === "BENCH_FAIL") {
      if (parts.length !== 2 || !/^[a-z_]+$/.test(key ?? "")) parseIssues.push("malformed BENCH_FAIL line");
      else failures.push(key);
    } else if (kind === "BENCH_ERROR") {
      parseIssues.push(`BENCH_ERROR must be emitted on stderr, not stdout (${key || "unknown"})`);
    } else {
      parseIssues.push(`unsupported structured line ${kind}`);
    }
  }
  for (const line of String(stderr).split("\n")) {
    if (!line.startsWith("BENCH_")) continue;
    const parts = line.split("\t");
    const [kind, key, value] = parts;
    if (kind !== "BENCH_ERROR" || parts.length !== 3 || !/^[a-z_]+$/.test(key ?? "") || value === "") {
      parseIssues.push(`unexpected stderr marker ${kind || "unknown"}`);
    } else {
      errors.push(`${key}:${value}`);
    }
  }
  return { phases, metadata, disk, cache, failures, errors, parseIssues, markerSequence, completedCommit };
}

function cacheStateValid(cache) {
  if (!(["dropped", "unavailable"].includes(cache.guest_page_cache))) return false;
  return Object.entries(REQUIRED_CACHE_STATES).every(([key, value]) => cache[key] === value);
}

function arraysEqual(left, right) {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

export function transcriptValid(result, guest) {
  const diskMonotonic = Number.isSafeInteger(result.disk.after_clone) && result.disk.after_clone > 0 &&
    Number.isSafeInteger(result.disk.after_install) && result.disk.after_install >= result.disk.after_clone &&
    Number.isSafeInteger(result.disk.after_typecheck) && result.disk.after_typecheck >= result.disk.after_install;
  const metadataValid = result.metadata.commit === EXPECTED_WORKLOAD_COMMIT &&
    result.metadata.bun_version === EXPECTED_BUN_VERSION && result.metadata.node_version === EXPECTED_NODE_VERSION &&
    result.metadata.architecture === guest.architecture &&
    /^(0|[1-9][0-9]*)$/.test(result.metadata.logical_cpus ?? "") && Number(result.metadata.logical_cpus) === guest.cpus &&
    /^(0|[1-9][0-9]*)$/.test(result.metadata.memory_kib ?? "") && Number(result.metadata.memory_kib) >= 15728640 &&
    typeof result.metadata.kernel === "string" && result.metadata.kernel !== "" &&
    typeof result.metadata.cpu_model === "string";
  return result.parseIssues.length === 0 && arraysEqual(result.markerSequence, FULL_MARKER_SEQUENCE) &&
    result.completedCommit === EXPECTED_WORKLOAD_COMMIT && result.failures.length === 0 && result.errors.length === 0 &&
    REQUIRED_PHASES.every((name) => Number.isSafeInteger(result.phases[name]) && result.phases[name] >= 0) &&
    cacheStateValid(result.cache) && metadataValid && diskMonotonic;
}

export function summarizeAttempts(attempts) {
  const successful = attempts.filter((attempt) => attempt.exitCode === 0 && !attempt.error);
  const totals = successful.map((attempt) => attempt.totalMs);
  if (totals.length === 0) return null;
  const phaseMs = {};
  for (const phase of REQUIRED_PHASES) {
    const values = successful.map((attempt) => attempt.result.phases[phase]);
    phaseMs[phase] = {
      samples: values.length,
      minMs: Math.min(...values),
      p50Ms: percentile(values, 0.50),
      p95Ms: percentile(values, 0.95),
      p99Ms: percentile(values, 0.99),
      maxMs: Math.max(...values),
    };
  }
  return {
    p50Ms: percentile(totals, 0.50),
    p95Ms: percentile(totals, 0.95),
    p99Ms: percentile(totals, 0.99),
    phaseMs,
  };
}

async function fetchPinnedScript() {
  const response = await fetch(UPSTREAM_SCRIPT_URL, { redirect: "error", signal: AbortSignal.timeout(30_000) });
  if (!response.ok) throw new Error(`upstream DAX script download failed with ${response.status}`);
  const script = await response.text();
  const digest = createHash("sha256").update(script).digest("hex");
  if (digest !== UPSTREAM_SCRIPT_SHA256) {
    throw new Error(`upstream DAX script digest ${digest} does not match ${UPSTREAM_SCRIPT_SHA256}`);
  }
  return script;
}

async function qualifyGuest(compute) {
  let sandbox;
  try {
    sandbox = await compute.sandbox.create();
    const result = await sandbox.runCommand(`set -eu
cpus=$(getconf _NPROCESSORS_ONLN)
memory_kib=$(awk '/^MemTotal:/ {print $2; exit}' /proc/meminfo)
free_root_kib=$(df -Pk / | awk 'NR == 2 {print $4}')
printf '{"cpus":%s,"memory_kib":%s,"free_root_kib":%s,"uid":%s,"architecture":"%s"}\n' "$cpus" "$memory_kib" "$free_root_kib" "$(id -u)" "$(uname -m)"
test "$cpus" -ge 8
test "$memory_kib" -ge 15728640
test "$free_root_kib" -ge 16777216
if test "$(id -u)" -ne 0; then command -v sudo >/dev/null; sudo -n true; fi
command -v bash >/dev/null
command -v apt-get >/dev/null || command -v dnf >/dev/null || command -v apk >/dev/null`);
    if (result.exitCode !== 0) {
      throw new Error(`DAX guest shape preflight failed with exit ${result.exitCode}: stdout=${JSON.stringify(result.stdout.slice(-1000))} stderr=${JSON.stringify(result.stderr.slice(-1000))}`);
    }
    const line = result.stdout.trim().split("\n").at(-1);
    return JSON.parse(line);
  } finally {
    if (sandbox) await sandbox.destroy();
  }
}

export async function run() {
  const runStartedAt = new Date().toISOString();
  if (process.env.BREZEL_ALLOW_INTERNET !== "true") {
    throw new Error("DAX requires BREZEL_ALLOW_INTERNET=true");
  }
  const iterations = boundedInteger("BREZEL_COMPUTESDK_DAX_ITERATIONS", 3, 3, 20);
  const region = process.env.BREZEL_BENCHMARK_REGION ?? "unknown";
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(region)) {
    throw new Error("BREZEL_BENCHMARK_REGION must be a short label");
  }
  const compute = createBrezelComputeFromEnv();
  const sourceRevision = process.env.BREZEL_SOURCE_REVISION ?? "";
  if (!/^[0-9a-f]{40}$/.test(sourceRevision)) throw new Error("BREZEL_SOURCE_REVISION must be the exact 40-character Brezel git revision");
  const environmentRevision = process.env.BREZEL_ENVIRONMENT_REVISION;
  const endpoint = new URL(process.env.BREZEL_API_URL);
  const before = await compute.sandbox.list();
  if (before.length !== 0) throw new Error("the dedicated DAX benchmark project must be empty before execution");
  const script = await fetchPinnedScript();
  const guest = await qualifyGuest(compute);
  const marker = `__BREZEL_DAX_${createHash("sha256").update(script).digest("hex")}__`;
  if (script.split("\n").includes(marker)) throw new Error("DAX heredoc marker collision");

  const attempts = [];
  let failure;
  for (let iteration = 1; iteration <= iterations; iteration += 1) {
    let sandbox;
    const attempt = { iteration, startedAt: new Date().toISOString(), cleanup: "not-started" };
    try {
      const createStarted = performance.now();
      sandbox = await compute.sandbox.create();
      attempt.createMs = performance.now() - createStarted;
      const command = `cat > /tmp/dax-benchmark.sh <<'${marker}'\n${script}\n${marker}\nBENCH_PROVIDER=brezel BENCH_REGION=${region} bash /tmp/dax-benchmark.sh`;
      attempt.buildStartedAt = new Date().toISOString();
      const started = performance.now();
      const result = await sandbox.runCommand(command, { timeout: 600_000 });
      attempt.totalMs = performance.now() - started;
      attempt.buildFinishedAt = new Date().toISOString();
      attempt.exitCode = result.exitCode;
      attempt.result = structuredLines(result.stdout, result.stderr);
      attempt.result.executionFailures = definitiveExecutionFailures(result.stderr);
      attempt.stderrTail = result.stderr.trim().split("\n").slice(-40).join("\n");
      if (result.exitCode !== 0 || attempt.result.executionFailures.length !== 0 || !transcriptValid(attempt.result, guest)) {
        throw new Error(`DAX iteration ${iteration} did not complete the pinned workload`);
      }
    } catch (error) {
      attempt.error = error instanceof Error ? error.message : String(error);
      failure ??= error;
    } finally {
      if (sandbox) {
        try {
          const destroyStarted = performance.now();
          await sandbox.destroy();
          attempt.destroyMs = performance.now() - destroyStarted;
          attempt.cleanup = "confirmed";
        } catch (error) {
          attempt.cleanup = "failed";
          attempt.cleanupError = error instanceof Error ? error.message : String(error);
          failure ??= error;
        }
      }
      attempt.finishedAt = new Date().toISOString();
      attempts.push(attempt);
    }
  }

  const successful = attempts.filter((attempt) => attempt.exitCode === 0 && !attempt.error);
  const after = await compute.sandbox.list();
  const cleanupConformant = after.length === 0 && attempts.every((attempt) => attempt.cleanup === "confirmed");
  const report = {
    schemaVersion: 2,
    suite: "computesdk-dax-rehearsal",
    upstream: { commit: UPSTREAM_COMMIT, scriptSha256: UPSTREAM_SCRIPT_SHA256 },
    region,
    startedAt: runStartedAt,
    finishedAt: new Date().toISOString(),
    provenance: {
      sourceRevision,
      environmentRevision,
      endpoint: `${endpoint.protocol}//${endpoint.host}${endpoint.pathname.replace(/\/$/, "")}`,
      runner: { node: process.version, platform: process.platform, arch: process.arch },
      allowInternet: true,
      measuredState: "fresh sandbox after one disclosed shape preflight",
    },
    methodology: {
      iterations,
      concurrency: 1,
      sandboxState: "fresh sandbox per iteration",
      scoredBoundary: "runCommand request through complete command result",
      upstreamPhaseOrder: REQUIRED_PHASES,
      cleanupBoundary: "destroy confirmation followed by empty-project inventory",
    },
    guest,
    requested: iterations,
    succeeded: successful.length,
    cleanupConfirmed: attempts.filter((attempt) => attempt.cleanup === "confirmed").length,
    projectEmptyBefore: before.length === 0,
    projectEmptyAfter: after.length === 0,
    cleanupConformant,
    summary: summarizeAttempts(attempts),
    attempts,
  };
  process.stdout.write(`${JSON.stringify(report, null, 2)}\n`);
  if (failure || successful.length !== iterations || !cleanupConformant) process.exitCode = 1;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await run();
}
