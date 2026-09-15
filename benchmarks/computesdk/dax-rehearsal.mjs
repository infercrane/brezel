import { createHash } from "node:crypto";
import { performance } from "node:perf_hooks";

import { createBrezelComputeFromEnv } from "./adapter.mjs";

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

function percentile(values, fraction) {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.max(0, Math.ceil(sorted.length * fraction) - 1)];
}

function structuredLines(stdout) {
  const phases = {};
  const metadata = {};
  const disk = {};
  const failures = [];
  const errors = [];
  let completedCommit = "";
  for (const line of stdout.split("\n")) {
    const [kind, key, value] = line.split("\t");
    if (kind === "BENCH_PHASE") phases[key] = Number(value);
    if (kind === "BENCH_META") metadata[key] = value;
    if (kind === "BENCH_DISK") disk[key] = Number(value);
    if (kind === "BENCH_DONE") completedCommit = key;
    if (kind === "BENCH_FAIL") failures.push(key);
    if (kind === "BENCH_ERROR") errors.push([key, value].filter(Boolean).join(":"));
  }
  return { phases, metadata, disk, failures, errors, completedCommit };
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
printf '{"cpus":%s,"memory_kib":%s,"free_root_kib":%s,"uid":%s}\n' "$cpus" "$memory_kib" "$free_root_kib" "$(id -u)"
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

async function run() {
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
    const attempt = { iteration, cleanup: "not-started" };
    try {
      sandbox = await compute.sandbox.create();
      const command = `cat > /tmp/dax-benchmark.sh <<'${marker}'\n${script}\n${marker}\nBENCH_PROVIDER=brezel BENCH_REGION=${region} bash /tmp/dax-benchmark.sh`;
      const started = performance.now();
      const result = await sandbox.runCommand(command, { timeout: 600_000 });
      attempt.totalMs = performance.now() - started;
      attempt.exitCode = result.exitCode;
      attempt.result = structuredLines(result.stdout);
      for (const line of result.stderr.split("\n")) {
        const [kind, key, value] = line.split("\t");
        if (kind === "BENCH_ERROR") attempt.result.errors.push([key, value].filter(Boolean).join(":"));
      }
      attempt.stderrTail = result.stderr.trim().split("\n").slice(-40).join("\n");
      const requiredPhases = ["prepare", "cache_clear", "bun_download", "bun_unpack", "clone", "install", "typecheck", "total"];
      const phasesValid = requiredPhases.every((name) => Number.isFinite(attempt.result.phases[name]) && attempt.result.phases[name] >= 0);
      const metadataValid = attempt.result.metadata.commit === "08fb47373509ba64b13441061314eeacf4264f51" &&
        Number(attempt.result.metadata.logical_cpus) >= 8 && Number(attempt.result.metadata.memory_kib) >= 15728640;
      if (result.exitCode !== 0 || attempt.result.completedCommit !== "08fb47373509ba64b13441061314eeacf4264f51" ||
          attempt.result.failures.length !== 0 || attempt.result.errors.length !== 0 || !phasesValid || !metadataValid) {
        throw new Error(`DAX iteration ${iteration} did not complete the pinned workload`);
      }
    } catch (error) {
      attempt.error = error instanceof Error ? error.message : String(error);
      failure ??= error;
    } finally {
      if (sandbox) {
        try {
          await sandbox.destroy();
          attempt.cleanup = "confirmed";
        } catch (error) {
          attempt.cleanup = "failed";
          attempt.cleanupError = error instanceof Error ? error.message : String(error);
          failure ??= error;
        }
      }
      attempts.push(attempt);
    }
  }

  const successful = attempts.filter((attempt) => attempt.exitCode === 0 && !attempt.error);
  const totals = successful.map((attempt) => attempt.totalMs);
  const after = await compute.sandbox.list();
  const cleanupConformant = after.length === 0 && attempts.every((attempt) => attempt.cleanup === "confirmed");
  const report = {
    schemaVersion: 1,
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
    guest,
    requested: iterations,
    succeeded: successful.length,
    cleanupConfirmed: attempts.filter((attempt) => attempt.cleanup === "confirmed").length,
    projectEmptyBefore: before.length === 0,
    projectEmptyAfter: after.length === 0,
    cleanupConformant,
    summary: totals.length === 0 ? null : {
      p50Ms: percentile(totals, 0.50),
      p95Ms: percentile(totals, 0.95),
      p99Ms: percentile(totals, 0.99),
    },
    attempts,
  };
  process.stdout.write(`${JSON.stringify(report, null, 2)}\n`);
  if (failure || successful.length !== iterations || !cleanupConformant) process.exitCode = 1;
}

await run();
