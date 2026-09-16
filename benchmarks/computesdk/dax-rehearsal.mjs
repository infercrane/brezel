import { createHash } from "node:crypto";
import { performance } from "node:perf_hooks";
import { pathToFileURL } from "node:url";

import { createBrezelComputeFromEnv } from "./adapter.mjs";
import { DAX_PHASES, parseDaxTranscript, validateDaxTranscript } from "./dax-transcript.mjs";

export const DAX_UPSTREAM = Object.freeze({
  commit: "273927519c0ac6558d3e545eed9d59eb56a47ec7",
  scriptSha256: "58f4640ac170b366f87e8466a9ac1e9f50383faa59553246e4664c77af34d550",
});
const UPSTREAM_SCRIPT_URL = `https://raw.githubusercontent.com/computesdk/benchmarks/${DAX_UPSTREAM.commit}/benchmarks/scripts/dax-benchmark.sh`;
const IMMUTABLE_BACKEND_TEMPLATE = /^[a-z0-9_-]+:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const DAX_EXPECTED_GUEST_CPUS = 8;

function optionalDigest(name, environment) {
  const value = environment[name];
  if (value === undefined) return undefined;
  if (!/^[0-9a-f]{64}$/.test(value)) throw new Error(`${name} must be a lowercase SHA-256 digest`);
  return value;
}

function optionalSlotID(name, environment) {
  const value = environment[name];
  if (value === undefined) return undefined;
  if (!/^[a-z0-9][a-z0-9-]{0,63}$/.test(value)) {
    throw new Error(`${name} must be a lowercase benchmark slot identifier`);
  }
  return value;
}

export function pairedABIdentity(environment = process.env) {
  const identity = {
    planId: optionalDigest("BREZEL_DAX_PAIRED_PLAN_ID", environment),
    slotId: optionalSlotID("BREZEL_DAX_PAIRED_SLOT_ID", environment),
    hostIdentitySha256: optionalDigest("BREZEL_DAX_HOST_IDENTITY_SHA256", environment),
    configurationIdentitySha256: optionalDigest("BREZEL_DAX_CONFIGURATION_IDENTITY_SHA256", environment),
  };
  const present = Object.values(identity).filter((value) => value !== undefined).length;
  if (present !== 0 && present !== Object.keys(identity).length) {
    throw new Error("paired DAX identity variables must be supplied together");
  }
  return present === 0 ? undefined : identity;
}

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

export function createPhaseObserver(clock = () => ({
  observedAt: new Date().toISOString(),
  elapsedMs: performance.now(),
})) {
  let buffer = "";
  const events = [];
  const seen = new Set();
  const consume = (line) => {
    const match = /^BENCH_PHASE\t([a-z_]+)\t([0-9]+)$/.exec(line);
    if (!match || !DAX_PHASES.includes(match[1]) || seen.has(match[1])) return;
    seen.add(match[1]);
    const observed = clock();
    events.push({ phase: match[1], guestDurationMs: Number(match[2]), ...observed });
  };
  return {
    push(chunk) {
      buffer += chunk;
      for (;;) {
        const newline = buffer.indexOf("\n");
        if (newline < 0) break;
        consume(buffer.slice(0, newline).replace(/\r$/, ""));
        buffer = buffer.slice(newline + 1);
      }
    },
    finish() {
      if (buffer) consume(buffer.replace(/\r$/, ""));
      buffer = "";
      return events.map((event) => ({ ...event }));
    },
  };
}

export { parseDaxTranscript as structuredLines };

export function transcriptValid(result, guest) {
  return validateDaxTranscript(result, {
    architecture: guest.architecture,
    logicalCPUs: guest.cpus,
    minimumMemoryKiB: 15728640,
    allowEmptyCPUModel: true,
  });
}

export function summarizeAttempts(attempts) {
  const successful = attempts.filter((attempt) => attempt.exitCode === 0 && !attempt.error);
  const totals = successful.map((attempt) => attempt.totalMs);
  if (totals.length === 0) return null;
  const phaseMs = {};
  for (const phase of DAX_PHASES) {
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

export function validateDaxEnvironment(environment, expectedRevision) {
  if (!environment || typeof environment !== "object" || Array.isArray(environment)) {
    throw new Error("DAX environment preflight received an invalid environment resource");
  }
  if (typeof expectedRevision !== "string" || environment.revision_id !== expectedRevision) {
    throw new Error("DAX environment preflight received a different environment revision");
  }
  if (typeof environment.template !== "string" || !IMMUTABLE_BACKEND_TEMPLATE.test(environment.template)) {
    throw new Error(
      "DAX environment must store an immutable templateID:buildID backend template; mutable aliases such as base are forbidden",
    );
  }
  return environment;
}

export async function preflightDaxEnvironment(compute, expectedRevision) {
  return validateDaxEnvironment(
    await compute.environment.getByRevision(expectedRevision),
    expectedRevision,
  );
}

async function fetchPinnedScript() {
  const response = await fetch(UPSTREAM_SCRIPT_URL, { redirect: "error", signal: AbortSignal.timeout(30_000) });
  if (!response.ok) throw new Error(`upstream DAX script download failed with ${response.status}`);
  const script = await response.text();
  const digest = createHash("sha256").update(script).digest("hex");
  if (digest !== DAX_UPSTREAM.scriptSha256) {
    throw new Error(`upstream DAX script digest ${digest} does not match ${DAX_UPSTREAM.scriptSha256}`);
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
smt_active=unknown
if test -r /sys/devices/system/cpu/smt/active; then smt_active=$(cat /sys/devices/system/cpu/smt/active); fi
printf '{"cpus":%s,"memory_kib":%s,"free_root_kib":%s,"uid":%s,"architecture":"%s"}\n' "$cpus" "$memory_kib" "$free_root_kib" "$(id -u)" "$(uname -m)"
test "$cpus" -eq ${DAX_EXPECTED_GUEST_CPUS}
test "$smt_active" != 1
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
  const pairedAB = pairedABIdentity();
  const capturePhaseEvents = process.env.BREZEL_DAX_CAPTURE_PHASE_EVENTS === "true";
  if (pairedAB && capturePhaseEvents) {
    throw new Error("phase telemetry is diagnostic-only and cannot be enabled for paired benchmark slots");
  }
  const endpoint = new URL(process.env.BREZEL_API_URL);
  await preflightDaxEnvironment(compute, environmentRevision);
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
      const phaseObserver = capturePhaseEvents ? createPhaseObserver(() => ({
        observedAt: new Date().toISOString(),
        elapsedMs: performance.now() - started,
      })) : undefined;
      const result = await sandbox.runCommand(command, {
        timeout: 600_000,
        ...(phaseObserver ? { onStdout: (chunk) => phaseObserver.push(chunk) } : {}),
      });
      attempt.totalMs = performance.now() - started;
      attempt.buildFinishedAt = new Date().toISOString();
      attempt.exitCode = result.exitCode;
      attempt.result = parseDaxTranscript(result.stdout, result.stderr);
      if (phaseObserver) {
        attempt.phaseEvents = phaseObserver.finish();
        const totalEvent = attempt.phaseEvents.find((event) => event.phase === "total");
        attempt.providerOverhead = {
          commandMinusGuestMs: attempt.totalMs - attempt.result.phases.total,
          markerObservationMinusGuestMs: totalEvent ? totalEvent.elapsedMs - attempt.result.phases.total : null,
          streamTailMs: totalEvent ? attempt.totalMs - totalEvent.elapsedMs : null,
        };
      }
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
    upstream: DAX_UPSTREAM,
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
      ...(pairedAB ? { pairedAB } : {}),
    },
    methodology: {
      iterations,
      concurrency: 1,
      sandboxState: "fresh sandbox per iteration",
      scoredBoundary: "runCommand request through complete command result",
      upstreamPhaseOrder: DAX_PHASES,
      cleanupBoundary: "destroy confirmation followed by empty-project inventory",
      ...(capturePhaseEvents ? { phaseTelemetry: "opt-in provider receipt timestamps for strict BENCH_PHASE markers" } : {}),
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
