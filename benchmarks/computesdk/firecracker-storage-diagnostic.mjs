import { createHash } from "node:crypto";
import { closeSync, fstatSync, lstatSync, openSync, readFileSync } from "node:fs";
import { performance } from "node:perf_hooks";
import { pathToFileURL } from "node:url";

import { createBrezelComputeFromEnv } from "./adapter.mjs";
import { DAX_UPSTREAM, percentile } from "./dax-rehearsal.mjs";
import { DAX_PHASES, parseDaxTranscript, validateDaxTranscript } from "./dax-transcript.mjs";

const SUITE = "brezel-firecracker-storage-diagnostic";
const MAX_SETUP_BYTES = 64 * 1024;
const UPSTREAM_SCRIPT_URL = `https://raw.githubusercontent.com/computesdk/benchmarks/${DAX_UPSTREAM.commit}/benchmarks/scripts/dax-benchmark.sh`;
const MEMORY_SCALARS = new Set(["current", "peak", "max"]);
const MEMORY_EVENT = /^[a-z_]+$/;

function boundedInteger(name, fallback, minimum, maximum) {
  const raw = process.env[name] ?? String(fallback);
  if (!/^[0-9]+$/.test(raw)) throw new Error(`${name} must be an integer`);
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    throw new Error(`${name} must be between ${minimum} and ${maximum}`);
  }
  return value;
}

function digest(value) {
  return createHash("sha256").update(value).digest("hex");
}

export function loadDeclaredSetup(path, expectedDigest) {
  if (!path) throw new Error("BREZEL_STORAGE_DIAGNOSTIC_SETUP_FILE is required");
  if (!/^[0-9a-f]{64}$/.test(expectedDigest ?? "")) {
    throw new Error("BREZEL_STORAGE_DIAGNOSTIC_SETUP_SHA256 must be a lowercase SHA-256 digest");
  }
  const descriptor = openSync(path, "r");
  try {
    const before = fstatSync(descriptor);
    const after = lstatSync(path);
    if (!before.isFile() || !after.isFile() || before.dev !== after.dev || before.ino !== after.ino || before.nlink !== 1) {
      throw new Error("storage diagnostic setup must be one regular, non-linked file");
    }
    if (before.size <= 0 || before.size > MAX_SETUP_BYTES) {
      throw new Error(`storage diagnostic setup must contain at most ${MAX_SETUP_BYTES} bytes`);
    }
    const script = readFileSync(descriptor);
    const actualDigest = digest(script);
    if (actualDigest !== expectedDigest) {
      throw new Error(`storage diagnostic setup digest ${actualDigest} does not match ${expectedDigest}`);
    }
    let decoded;
    try {
      decoded = new TextDecoder("utf-8", { fatal: true }).decode(script);
    } catch {
      throw new Error("storage diagnostic setup must be valid UTF-8 text");
    }
    if (decoded.includes("\0")) throw new Error("storage diagnostic setup must not contain NUL bytes");
    return { script: decoded, sha256: actualDigest, bytes: script.length };
  } finally {
    closeSync(descriptor);
  }
}

async function fetchPinnedScript() {
  const response = await fetch(UPSTREAM_SCRIPT_URL, { redirect: "error", signal: AbortSignal.timeout(30_000) });
  if (!response.ok) throw new Error(`upstream DAX script download failed with ${response.status}`);
  const script = await response.text();
  const actualDigest = digest(script);
  if (actualDigest !== DAX_UPSTREAM.scriptSha256) {
    throw new Error(`upstream DAX script digest ${actualDigest} does not match ${DAX_UPSTREAM.scriptSha256}`);
  }
  return script;
}

function tail(value, lines = 40) {
  return String(value).trim().split("\n").slice(-lines).join("\n");
}

function parseUnsigned(value, label) {
  if (!/^(?:0|[1-9][0-9]*)$/.test(value ?? "")) throw new Error(`${label} must be an unsigned integer`);
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed)) throw new Error(`${label} exceeds the safe integer range`);
  return parsed;
}

export function parseMemoryEvidence(stdout) {
  const evidence = {
    cgroupPath: "",
    scalars: {},
    events: {},
    localEvents: {},
    availability: { scalars: {}, events: "unknown", localEvents: "unknown", dmesg: "unknown" },
    kernelOomLines: [],
    parseErrors: [],
  };
  const seen = new Set();
  const once = (identity, line) => {
    if (seen.has(identity)) {
      evidence.parseErrors.push(`duplicate ${identity}: ${line}`);
      return false;
    }
    seen.add(identity);
    return true;
  };
  for (const line of String(stdout).split("\n")) {
    if (!line.startsWith("BREZEL_DIAG_")) continue;
    const parts = line.split("\t");
    const kind = parts[0];
    if (kind === "BREZEL_DIAG_CGROUP_PATH" && parts.length === 2 && once("cgroup_path", line)) {
      evidence.cgroupPath = parts[1];
    } else if (kind === "BREZEL_DIAG_MEMORY_SCALAR" && parts.length === 3 && MEMORY_SCALARS.has(parts[1]) && once(`scalar:${parts[1]}`, line)) {
      if (parts[2] === "unavailable" || parts[2] === "max") {
        if (parts[2] === "max" && parts[1] !== "max") {
          evidence.parseErrors.push(`memory scalar ${parts[1]} cannot be max`);
        } else {
          evidence.availability.scalars[parts[1]] = parts[2];
        }
      } else {
        try {
          evidence.scalars[parts[1]] = parseUnsigned(parts[2], `memory scalar ${parts[1]}`);
          evidence.availability.scalars[parts[1]] = "available";
        } catch (error) {
          evidence.parseErrors.push(error.message);
        }
      }
    } else if ((kind === "BREZEL_DIAG_MEMORY_EVENT" || kind === "BREZEL_DIAG_MEMORY_EVENT_LOCAL") &&
               parts.length === 3 && MEMORY_EVENT.test(parts[1])) {
      const target = kind.endsWith("_LOCAL") ? evidence.localEvents : evidence.events;
      const namespace = kind.endsWith("_LOCAL") ? "local_event" : "event";
      if (!once(`${namespace}:${parts[1]}`, line)) continue;
      try {
        target[parts[1]] = parseUnsigned(parts[2], `${namespace} ${parts[1]}`);
      } catch (error) {
        evidence.parseErrors.push(error.message);
      }
    } else if (kind === "BREZEL_DIAG_AVAILABILITY" && parts.length === 3 &&
               ["events", "local_events", "dmesg"].includes(parts[1]) &&
               ["available", "unavailable"].includes(parts[2]) && once(`availability:${parts[1]}`, line)) {
      evidence.availability[parts[1] === "local_events" ? "localEvents" : parts[1]] = parts[2];
    } else if (kind === "BREZEL_DIAG_KERNEL_OOM" && parts.length >= 2) {
      evidence.kernelOomLines.push(parts.slice(1).join("\t"));
    } else {
      evidence.parseErrors.push(`malformed diagnostic marker: ${line}`);
    }
  }
  if (!seen.has("cgroup_path")) evidence.parseErrors.push("missing cgroup path marker");
  for (const scalar of MEMORY_SCALARS) {
    if (!seen.has(`scalar:${scalar}`)) evidence.parseErrors.push(`missing memory scalar ${scalar}`);
  }
  for (const availability of ["events", "local_events", "dmesg"]) {
    if (!seen.has(`availability:${availability}`)) evidence.parseErrors.push(`missing availability marker ${availability}`);
  }
  if (evidence.availability.events === "available" && Object.keys(evidence.events).length === 0) {
    evidence.parseErrors.push("memory.events was available but empty");
  }
  return evidence;
}

const MEMORY_EVIDENCE_COMMAND = `set -u
cgroup_path=$(awk -F: '$1 == "0" { print $3; exit }' /proc/self/cgroup)
test -n "$cgroup_path" || cgroup_path=/
cgroup_root=/sys/fs/cgroup$cgroup_path
printf 'BREZEL_DIAG_CGROUP_PATH\\t%s\\n' "$cgroup_path"
for key in current peak max; do
  path="$cgroup_root/memory.$key"
  if test -r "$path"; then
    value=$(cat "$path" 2>/dev/null || printf unavailable)
  else
    value=unavailable
  fi
  printf 'BREZEL_DIAG_MEMORY_SCALAR\\t%s\\t%s\\n' "$key" "$value"
done
if test -r "$cgroup_root/memory.events"; then
  printf 'BREZEL_DIAG_AVAILABILITY\\tevents\\tavailable\\n'
  while read -r key value; do printf 'BREZEL_DIAG_MEMORY_EVENT\\t%s\\t%s\\n' "$key" "$value"; done < "$cgroup_root/memory.events"
else
  printf 'BREZEL_DIAG_AVAILABILITY\\tevents\\tunavailable\\n'
fi
if test -r "$cgroup_root/memory.events.local"; then
  printf 'BREZEL_DIAG_AVAILABILITY\\tlocal_events\\tavailable\\n'
  while read -r key value; do printf 'BREZEL_DIAG_MEMORY_EVENT_LOCAL\\t%s\\t%s\\n' "$key" "$value"; done < "$cgroup_root/memory.events.local"
else
  printf 'BREZEL_DIAG_AVAILABILITY\\tlocal_events\\tunavailable\\n'
fi
if dmesg >/dev/null 2>&1; then
  printf 'BREZEL_DIAG_AVAILABILITY\\tdmesg\\tavailable\\n'
  dmesg 2>/dev/null | grep -Ei 'out of memory|oom-kill|killed process' | tail -n 40 | sed 's/^/BREZEL_DIAG_KERNEL_OOM\\t/' || true
elif command -v sudo >/dev/null && sudo -n dmesg >/dev/null 2>&1; then
  printf 'BREZEL_DIAG_AVAILABILITY\\tdmesg\\tavailable\\n'
  sudo -n dmesg 2>/dev/null | grep -Ei 'out of memory|oom-kill|killed process' | tail -n 40 | sed 's/^/BREZEL_DIAG_KERNEL_OOM\\t/' || true
else
  printf 'BREZEL_DIAG_AVAILABILITY\\tdmesg\\tunavailable\\n'
fi`;

async function collectMemoryEvidence(sandbox) {
  const result = await sandbox.runCommand(MEMORY_EVIDENCE_COMMAND, { timeout: 30_000 });
  const parsed = parseMemoryEvidence(result.stdout);
  if (result.exitCode !== 0) parsed.parseErrors.push(`memory evidence command exited ${result.exitCode}`);
  if (result.stderr.trim()) parsed.commandStderrTail = tail(result.stderr, 20);
  return parsed;
}

async function inspectGuest(sandbox) {
  const result = await sandbox.runCommand(`set -eu
cpus=$(getconf _NPROCESSORS_ONLN)
memory_kib=$(awk '/^MemTotal:/ {print $2; exit}' /proc/meminfo)
free_root_kib=$(df -Pk / | awk 'NR == 2 {print $4}')
printf '{"cpus":%s,"memory_kib":%s,"free_root_kib":%s,"uid":%s,"architecture":"%s","kernel":"%s"}\\n' "$cpus" "$memory_kib" "$free_root_kib" "$(id -u)" "$(uname -m)" "$(uname -r)"
test "$cpus" -ge 8
test "$memory_kib" -ge 15728640
test "$free_root_kib" -ge 16777216
if test "$(id -u)" -ne 0; then command -v sudo >/dev/null; sudo -n true; fi
command -v bash >/dev/null
command -v apt-get >/dev/null || command -v dnf >/dev/null || command -v apk >/dev/null`);
  if (result.exitCode !== 0) throw new Error(`guest inspection exited ${result.exitCode}: ${tail(result.stderr)}`);
  return JSON.parse(result.stdout.trim().split("\n").at(-1));
}

function setupCommand(setup, daxScript) {
  const marker = `__BREZEL_STORAGE_${digest(Buffer.concat([Buffer.from(setup.script), Buffer.from(daxScript)]))}__`;
  if (setup.script.split("\n").includes(marker) || daxScript.split("\n").includes(marker)) {
    throw new Error("storage diagnostic heredoc marker collision");
  }
  return `set -u
cat > /var/tmp/brezel-storage-setup.sh <<'${marker}'
${setup.script}
${marker}
chmod 700 /var/tmp/brezel-storage-setup.sh
if test "$(id -u)" -eq 0; then
  bash /var/tmp/brezel-storage-setup.sh
else
  sudo -n bash /var/tmp/brezel-storage-setup.sh
fi`;
}

function daxCommand(script, region) {
  const marker = `__BREZEL_DAX_${digest(script)}__`;
  if (script.split("\n").includes(marker)) throw new Error("DAX heredoc marker collision");
  return `cat > /var/tmp/brezel-storage-dax.sh <<'${marker}'
${script}
${marker}
BENCH_PROVIDER=brezel-storage-diagnostic BENCH_REGION=${region} bash /var/tmp/brezel-storage-dax.sh`;
}

function eventIncrease(before, after, key) {
  if (!Number.isSafeInteger(before?.[key]) || !Number.isSafeInteger(after?.[key])) return undefined;
  return after[key] - before[key];
}

export function classifyOOM(before, after, daxExitCode) {
  const oomDelta = eventIncrease(before.events, after.events, "oom");
  const oomKillDelta = eventIncrease(before.events, after.events, "oom_kill");
  const localOomDelta = eventIncrease(before.localEvents, after.localEvents, "oom");
  const localOomKillDelta = eventIncrease(before.localEvents, after.localEvents, "oom_kill");
  const kernelLinesAdded = after.kernelOomLines.filter((line) => !before.kernelOomLines.includes(line));
  const counterEvidenceComplete =
    (before.availability.events === "available" && after.availability.events === "available") ||
    (before.availability.localEvents === "available" && after.availability.localEvents === "available");
  const finiteMemoryLimit = Number.isSafeInteger(before.scalars.max) && Number.isSafeInteger(after.scalars.max);
  const physicalOomEvidenceComplete =
    (before.availability.dmesg === "available" && after.availability.dmesg === "available") || finiteMemoryLimit;
  return {
    confirmed: [oomDelta, oomKillDelta, localOomDelta, localOomKillDelta].some((value) => value > 0) || kernelLinesAdded.length > 0,
    possibleSignalExit: [134, 137].includes(daxExitCode),
    counters: { oomDelta, oomKillDelta, localOomDelta, localOomKillDelta },
    kernelLinesAdded,
    evidenceComplete: before.parseErrors.length === 0 && after.parseErrors.length === 0 &&
      counterEvidenceComplete && physicalOomEvidenceComplete,
  };
}

export function strictAttemptValid(attempt) {
  return attempt?.setup?.exitCode === 0 && attempt?.dax?.exitCode === 0 && attempt.dax.strictTranscriptValid === true &&
    attempt?.memory?.before?.parseErrors?.length === 0 && attempt?.memory?.after?.parseErrors?.length === 0 &&
    attempt.cleanup === "confirmed" && attempt?.oom?.confirmed === false && attempt.oom.evidenceComplete === true;
}

export function summarizeDiagnosticAttempts(attempts) {
  const successful = attempts.filter(strictAttemptValid);
  if (successful.length === 0) return null;
  const phaseMs = {};
  for (const phase of DAX_PHASES) {
    const values = successful.map((attempt) => attempt.dax.result.phases[phase]);
    phaseMs[phase] = {
      samples: values.length,
      minMs: Math.min(...values),
      p50Ms: percentile(values, 0.50),
      p95Ms: percentile(values, 0.95),
      maxMs: Math.max(...values),
    };
  }
  const wall = successful.map((attempt) => attempt.dax.wallMs);
  return {
    succeeded: successful.length,
    daxWallMs: { minMs: Math.min(...wall), p50Ms: percentile(wall, 0.50), p95Ms: percentile(wall, 0.95), maxMs: Math.max(...wall) },
    phaseMs,
  };
}

function commandEvidence(result, wallMs) {
  return {
    exitCode: result.exitCode,
    wallMs,
    stdoutBytes: Buffer.byteLength(result.stdout),
    stderrBytes: Buffer.byteLength(result.stderr),
    stdoutSha256: digest(result.stdout),
    stderrSha256: digest(result.stderr),
    stdoutTail: tail(result.stdout),
    stderrTail: tail(result.stderr),
  };
}

async function runAttempt(compute, setup, daxScript, region, iteration) {
  let sandbox;
  const attempt = { iteration, startedAt: new Date().toISOString(), cleanup: "not-started" };
  try {
    const createStarted = performance.now();
    sandbox = await compute.sandbox.create();
    attempt.createMs = performance.now() - createStarted;
    attempt.guest = await inspectGuest(sandbox);
    attempt.memory = { before: await collectMemoryEvidence(sandbox) };

    const setupStarted = performance.now();
    const setupResult = await sandbox.runCommand(setupCommand(setup, daxScript), { timeout: 120_000 });
    attempt.setup = commandEvidence(setupResult, performance.now() - setupStarted);

    if (setupResult.exitCode === 0) {
      const daxStarted = performance.now();
      const daxResult = await sandbox.runCommand(daxCommand(daxScript, region), { timeout: 600_000 });
      const wallMs = performance.now() - daxStarted;
      const parsed = parseDaxTranscript(daxResult.stdout, daxResult.stderr);
      const strictTranscriptValid = daxResult.exitCode === 0 && validateDaxTranscript(parsed, {
        architecture: attempt.guest.architecture,
        logicalCPUs: attempt.guest.cpus,
        minimumMemoryKiB: 15_728_640,
        allowEmptyCPUModel: true,
      }) && Number(parsed.metadata.memory_kib) === attempt.guest.memory_kib;
      attempt.dax = {
        ...commandEvidence(daxResult, wallMs),
        strictTranscriptValid,
        result: parsed,
      };
    } else {
      attempt.dax = { exitCode: null, wallMs: 0, strictTranscriptValid: false, result: null, skipped: "setup-failed" };
    }
    attempt.memory.after = await collectMemoryEvidence(sandbox);
    attempt.oom = classifyOOM(attempt.memory.before, attempt.memory.after, attempt.dax.exitCode);
  } catch (error) {
    attempt.error = error instanceof Error ? error.message : String(error);
    if (sandbox && attempt.memory?.before && !attempt.memory.after) {
      try {
        attempt.memory.after = await collectMemoryEvidence(sandbox);
        attempt.oom = classifyOOM(attempt.memory.before, attempt.memory.after, attempt.dax?.exitCode);
      } catch (evidenceError) {
        attempt.memoryEvidenceError = evidenceError instanceof Error ? evidenceError.message : String(evidenceError);
      }
    }
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
      }
    }
    attempt.finishedAt = new Date().toISOString();
  }
  return attempt;
}

export async function run() {
  if (process.env.BREZEL_ALLOW_INTERNET !== "true") throw new Error("pinned DAX diagnostic requires BREZEL_ALLOW_INTERNET=true");
  const sourceRevision = process.env.BREZEL_SOURCE_REVISION ?? "";
  if (!/^[0-9a-f]{40}$/.test(sourceRevision)) throw new Error("BREZEL_SOURCE_REVISION must be the exact 40-character Brezel git revision");
  const environmentRevision = process.env.BREZEL_ENVIRONMENT_REVISION;
  if (!environmentRevision) throw new Error("BREZEL_ENVIRONMENT_REVISION is required");
  const label = process.env.BREZEL_STORAGE_DIAGNOSTIC_LABEL ?? "";
  if (!/^[a-z0-9][a-z0-9-]{0,63}$/.test(label)) throw new Error("BREZEL_STORAGE_DIAGNOSTIC_LABEL must be a lowercase label");
  const region = process.env.BREZEL_BENCHMARK_REGION ?? "unknown";
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(region)) throw new Error("BREZEL_BENCHMARK_REGION must be a short label");
  const iterations = boundedInteger("BREZEL_STORAGE_DIAGNOSTIC_ITERATIONS", 3, 1, 10);
  const setup = loadDeclaredSetup(process.env.BREZEL_STORAGE_DIAGNOSTIC_SETUP_FILE, process.env.BREZEL_STORAGE_DIAGNOSTIC_SETUP_SHA256);
  const daxScript = await fetchPinnedScript();
  const compute = createBrezelComputeFromEnv();
  const endpoint = new URL(process.env.BREZEL_API_URL);
  const before = await compute.sandbox.list();
  if (before.length !== 0) throw new Error("the dedicated storage diagnostic project must be empty before execution");

  const startedAt = new Date().toISOString();
  const attempts = [];
  for (let iteration = 1; iteration <= iterations; iteration += 1) {
    attempts.push(await runAttempt(compute, setup, daxScript, region, iteration));
  }
  const after = await compute.sandbox.list();
  const cleanupConformant = after.length === 0 && attempts.every((attempt) => attempt.cleanup === "confirmed");
  const strictSucceeded = attempts.filter(strictAttemptValid).length;
  const report = {
    schemaVersion: 1,
    suite: SUITE,
    classification: "diagnostic-non-leaderboard",
    claimBoundary: "same-provider Firecracker storage diagnosis only; not a ComputeSDK leaderboard or cross-provider result",
    upstream: DAX_UPSTREAM,
    label,
    region,
    startedAt,
    finishedAt: new Date().toISOString(),
    provenance: {
      sourceRevision,
      environmentRevision,
      endpoint: `${endpoint.protocol}//${endpoint.host}${endpoint.pathname.replace(/\/$/, "")}`,
      runner: { node: process.version, platform: process.platform, arch: process.arch },
      allowInternet: true,
      setup: { sha256: setup.sha256, bytes: setup.bytes },
    },
    methodology: {
      iterations,
      concurrency: 1,
      sandboxState: "fresh sandbox per iteration",
      setupBoundary: "declared setup after guest inspection and before pinned DAX",
      scoredBoundary: "diagnostic runCommand wall time; never leaderboard-comparable",
      upstreamPhaseOrder: DAX_PHASES,
      memoryEvidence: "cgroup-v2 counters and bounded kernel OOM matches before and after DAX",
    },
    requested: iterations,
    strictSucceeded,
    cleanupConfirmed: attempts.filter((attempt) => attempt.cleanup === "confirmed").length,
    projectEmptyBefore: before.length === 0,
    projectEmptyAfter: after.length === 0,
    cleanupConformant,
    oomConfirmed: attempts.filter((attempt) => attempt.oom?.confirmed === true).length,
    summary: summarizeDiagnosticAttempts(attempts),
    attempts,
  };
  process.stdout.write(`${JSON.stringify(report, null, 2)}\n`);
  if (strictSucceeded !== iterations || !cleanupConformant) process.exitCode = 1;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    await run();
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
    process.exitCode = 1;
  }
}
