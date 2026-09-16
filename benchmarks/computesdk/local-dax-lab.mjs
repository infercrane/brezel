#!/usr/bin/env node

import { createHash, randomBytes } from "node:crypto";
import { execFile, execFileSync, spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { writeFileSync } from "node:fs";

import { definitiveExecutionFailures } from "./output-validation.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
const UPSTREAM_COMMIT = "273927519c0ac6558d3e545eed9d59eb56a47ec7";
const UPSTREAM_SCRIPT_SHA256 = "58f4640ac170b366f87e8466a9ac1e9f50383faa59553246e4664c77af34d550";
const UPSTREAM_SCRIPT_URL = `https://raw.githubusercontent.com/computesdk/benchmarks/${UPSTREAM_COMMIT}/benchmarks/scripts/dax-benchmark.sh`;
const BASELINE_IMAGE = "node:24.14.1-bookworm@sha256:80fc934952c8f1b2b4d39907af7211f8a9fff1a4c2cf673fb49099292c251cec";
const CANDIDATE_IMAGE = "brezel/dax-dev:local";
const EXPECTED_WORKLOAD_COMMIT = "08fb47373509ba64b13441061314eeacf4264f51";
const EXPECTED_BUN_VERSION = "1.3.14";
const EXPECTED_NODE_VERSION = "v24.14.1";
const FULL_MARKER_SEQUENCE = [
  "phase:prepare", "phase:cache_clear",
  "meta:commit", "meta:architecture", "meta:kernel", "meta:logical_cpus", "meta:cpu_model", "meta:memory_kib",
  "phase:bun_download", "phase:bun_unpack", "meta:bun_version", "meta:node_version",
  "phase:clone", "disk:after_clone", "phase:install", "disk:after_install",
  "phase:typecheck", "disk:after_typecheck", "done", "phase:total",
];
const PREPARE_MARKER_PREFIX = FULL_MARKER_SEQUENCE.slice(0, FULL_MARKER_SEQUENCE.indexOf("phase:clone") + 1);
const GIB = 1024 ** 3;
const OUTPUT_LIMIT_BYTES = 64 * 1024 * 1024;
const RUN_TIMEOUT_MS = 600_000;
const activeContainers = new Set();
const activeVolumes = new Set();

export function parseStructuredOutput(stdout, stderr = "") {
  const result = {
    phases: {}, metadata: {}, disk: {}, failures: [], errors: [], completedCommit: "",
    markerSequence: [], transcriptErrors: [],
  };
  const seen = new Set();
  const recordOnce = (identity, line) => {
    if (seen.has(identity)) {
      result.transcriptErrors.push(`duplicate ${identity}: ${line}`);
      return false;
    }
    seen.add(identity);
    return true;
  };
  for (const line of String(stdout).split("\n")) {
    if (!line.startsWith("BENCH_")) continue;
    const [kind, key, value] = line.split("\t");
    if (kind === "BENCH_CACHE") continue;
    if (kind === "BENCH_PHASE") {
      if (!/^[a-z_]+$/.test(key ?? "") || !/^(0|[1-9][0-9]*)$/.test(value ?? "")) {
        result.transcriptErrors.push(`malformed phase marker: ${line}`);
        continue;
      }
      const parsed = Number(value);
      if (!Number.isSafeInteger(parsed) || !recordOnce(`phase:${key}`, line)) continue;
      result.phases[key] = parsed;
      result.markerSequence.push(`phase:${key}`);
      continue;
    }
    if (kind === "BENCH_META") {
      const emptyValueAllowed = key === "cpu_model";
      if (!/^[a-z_]+$/.test(key ?? "") || value === undefined || (!emptyValueAllowed && value === "") || !recordOnce(`meta:${key}`, line)) {
        if (!result.transcriptErrors.some((entry) => entry.endsWith(line))) result.transcriptErrors.push(`malformed metadata marker: ${line}`);
        continue;
      }
      result.metadata[key] = value;
      result.markerSequence.push(`meta:${key}`);
      continue;
    }
    if (kind === "BENCH_DISK") {
      // POSIX awk may render large integer byte counts in decimal scientific
      // notation. Accept that one bounded decimal grammar, but never hex,
      // Infinity, NaN, signs, or arbitrary JavaScript Number syntax.
      if (!/^[a-z_]+$/.test(key ?? "") || !/^(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:e\+[0-9]+)?$/.test(value ?? "")) {
        result.transcriptErrors.push(`malformed disk marker: ${line}`);
        continue;
      }
      const parsed = Number(value);
      if (!Number.isSafeInteger(parsed)) {
        result.transcriptErrors.push(`unsafe disk marker: ${line}`);
        continue;
      }
      if (!recordOnce(`disk:${key}`, line)) continue;
      result.disk[key] = parsed;
      result.markerSequence.push(`disk:${key}`);
      continue;
    }
    if (kind === "BENCH_DONE") {
      if (!/^[0-9a-f]{40}$/.test(key ?? "") || value !== undefined || !recordOnce("done", line)) {
        result.transcriptErrors.push(`malformed completion marker: ${line}`);
        continue;
      }
      result.completedCommit = key;
      result.markerSequence.push("done");
      continue;
    }
    if (kind === "BENCH_FAIL") {
      if (!/^[a-z_]+$/.test(key ?? "") || value !== undefined || !recordOnce(`fail:${key}`, line)) {
        result.transcriptErrors.push(`malformed failure marker: ${line}`);
        continue;
      }
      result.failures.push(key);
      continue;
    }
    result.transcriptErrors.push(`unknown stdout marker: ${line}`);
  }
  for (const line of String(stderr).split("\n")) {
    if (!line.startsWith("BENCH_")) continue;
    const [kind, key, value] = line.split("\t");
    if (kind !== "BENCH_ERROR" || !/^[a-z_]+$/.test(key ?? "") || value === undefined || value === "") {
      result.transcriptErrors.push(`unexpected stderr marker: ${line}`);
      continue;
    }
    result.errors.push(`${key}:${value}`);
  }
  result.executionFailures = definitiveExecutionFailures(stderr);
  return result;
}

function arraysEqual(left, right) {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

export function validateFullTranscript(result, expected = {}) {
  const expectedArchitecture = expected.architecture ?? "";
  const expectedLogicalCPUs = expected.logicalCPUs;
  const requiredMetadata = ["commit", "architecture", "kernel", "logical_cpus", "memory_kib", "bun_version", "node_version"];
  const diskMonotonic = Number.isSafeInteger(result.disk.after_clone) && Number.isSafeInteger(result.disk.after_install) &&
    Number.isSafeInteger(result.disk.after_typecheck) && result.disk.after_clone > 0 &&
    result.disk.after_install >= result.disk.after_clone && result.disk.after_typecheck >= result.disk.after_install;
  return result.transcriptErrors.length === 0 && arraysEqual(result.markerSequence, FULL_MARKER_SEQUENCE) &&
    requiredMetadata.every((key) => typeof result.metadata[key] === "string" && result.metadata[key] !== "") &&
    (expected.allowEmptyCPUModel === true || (typeof result.metadata.cpu_model === "string" && result.metadata.cpu_model !== "")) &&
    result.metadata.commit === EXPECTED_WORKLOAD_COMMIT && result.completedCommit === EXPECTED_WORKLOAD_COMMIT &&
    (!expectedArchitecture || result.metadata.architecture === expectedArchitecture) &&
    (!Number.isFinite(expectedLogicalCPUs) || result.metadata.logical_cpus === String(expectedLogicalCPUs)) &&
    /^(0|[1-9][0-9]*)$/.test(result.metadata.memory_kib) && Number(result.metadata.memory_kib) >= 4 * 1024 * 1024 &&
    result.metadata.bun_version === EXPECTED_BUN_VERSION && result.metadata.node_version === EXPECTED_NODE_VERSION &&
    diskMonotonic;
}

export function percentile(values, fraction) {
  if (values.length === 0) return null;
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.max(0, Math.ceil(sorted.length * fraction) - 1)];
}

export function summarizeAttempts(attempts) {
  const successful = attempts.filter((attempt) => attempt.valid);
  const phaseNames = ["prepare", "cache_clear", "bun_download", "bun_unpack", "clone", "install", "typecheck", "total"];
  return {
    requested: attempts.length,
    succeeded: successful.length,
    phaseMedianMs: Object.fromEntries(phaseNames.map((phase) => [
      phase,
      percentile(successful.map((attempt) => attempt.result.phases[phase]).filter(Number.isFinite), 0.5),
    ])),
  };
}

export function parseRuntimeFacts(raw) {
  const facts = {};
  for (const line of raw.trim().split("\n")) {
    const separator = line.indexOf("\t");
    if (separator <= 0) continue;
    facts[line.slice(0, separator)] = line.slice(separator + 1);
  }
  return {
    nproc: Number(facts.nproc),
    getconfProcessorsOnline: Number(facts.getconf_processors_online),
    cpusetEffective: facts.cpuset_effective ?? "",
    cpuMax: facts.cpu_max ?? "",
    memoryMax: Number(facts.memory_max),
  };
}

export function parseByteQuantity(raw) {
  const match = String(raw).trim().match(/^([0-9]+(?:\.[0-9]+)?)(B|kB|KB|KiB|MB|MiB|GB|GiB|TB|TiB)$/);
  if (!match) return null;
  const factors = {
    B: 1,
    kB: 1_000,
    KB: 1_000,
    KiB: 1024,
    MB: 1_000 ** 2,
    MiB: 1024 ** 2,
    GB: 1_000 ** 3,
    GiB: 1024 ** 3,
    TB: 1_000 ** 4,
    TiB: 1024 ** 4,
  };
  return Math.round(Number(match[1]) * factors[match[2]]);
}

function parseBytePair(raw) {
  const [first, second] = String(raw).split("/").map((value) => parseByteQuantity(value));
  return [first ?? null, second ?? null];
}

export function parseDockerStats(raw, elapsedMs) {
  const row = JSON.parse(raw);
  const [memoryUsedBytes, memoryLimitBytes] = parseBytePair(row.MemUsage);
  const [blockReadBytes, blockWriteBytes] = parseBytePair(row.BlockIO);
  const [networkReadBytes, networkWriteBytes] = parseBytePair(row.NetIO);
  return {
    elapsedMs,
    cpuPercent: Number.parseFloat(String(row.CPUPerc).replace(/%$/, "")),
    memoryUsedBytes,
    memoryLimitBytes,
    blockReadBytes,
    blockWriteBytes,
    networkReadBytes,
    networkWriteBytes,
    pids: Number(row.PIDs),
  };
}

export function summarizePhaseTelemetry(samples, phaseEvents) {
  const summaries = {};
  for (const event of phaseEvents) {
    const startMs = Math.max(0, event.observedAtMs - event.durationMs);
    const selected = samples.filter((sample) => sample.elapsedMs >= startMs && sample.elapsedMs <= event.observedAtMs);
    if (selected.length === 0) {
      summaries[event.phase] = { samples: 0, startMs, endMs: event.observedAtMs };
      continue;
    }
    const numeric = (key) => selected.map((sample) => sample[key]).filter(Number.isFinite);
    const cpu = numeric("cpuPercent");
    const memory = numeric("memoryUsedBytes");
    const pids = numeric("pids");
    const first = selected[0];
    const last = selected[selected.length - 1];
    summaries[event.phase] = {
      samples: selected.length,
      startMs,
      endMs: event.observedAtMs,
      cpuMeanPercent: cpu.length > 0 ? cpu.reduce((sum, value) => sum + value, 0) / cpu.length : null,
      cpuMaxPercent: cpu.length > 0 ? Math.max(...cpu) : null,
      memoryMaxBytes: memory.length > 0 ? Math.max(...memory) : null,
      pidsMax: pids.length > 0 ? Math.max(...pids) : null,
      blockReadDeltaBytes: Number.isFinite(first.blockReadBytes) && Number.isFinite(last.blockReadBytes) ? Math.max(0, last.blockReadBytes - first.blockReadBytes) : null,
      blockWriteDeltaBytes: Number.isFinite(first.blockWriteBytes) && Number.isFinite(last.blockWriteBytes) ? Math.max(0, last.blockWriteBytes - first.blockWriteBytes) : null,
      networkReadDeltaBytes: Number.isFinite(first.networkReadBytes) && Number.isFinite(last.networkReadBytes) ? Math.max(0, last.networkReadBytes - first.networkReadBytes) : null,
      networkWriteDeltaBytes: Number.isFinite(first.networkWriteBytes) && Number.isFinite(last.networkWriteBytes) ? Math.max(0, last.networkWriteBytes - first.networkWriteBytes) : null,
    };
  }
  return summaries;
}

function usage() {
  return `Usage: node benchmarks/computesdk/local-dax-lab.mjs [options]

Options:
  --image baseline|candidate|IMAGE  Image under test (default: baseline)
  --build-candidate                Build the general development candidate
  --storage overlay|volume|tmpfs   Writable benchmark root (default: overlay)
  --probe full|prepare             Full workload or image-prepare A/B (default: full)
  --iterations N                   Fresh-container repetitions (default: 1)
  --cpus N                         Visible container CPUs (default: min(8, engine))
  --memory BYTES                   Container memory limit (default: 80% of engine, max 16 GiB)
  --telemetry-interval-ms N        Sample Docker CPU/memory/I/O; 250-10000 ms
  --output PATH                    Also write the JSON report to PATH
  --help                           Show this help

This is a local A/B laboratory. macOS, ARM, containers, undersized memory, and
non-KVM storage are recorded in the report and never count as leaderboard
evidence.
`;
}

function positiveInteger(raw, name, maximum) {
  if (!/^[1-9][0-9]*$/.test(raw)) throw new Error(`${name} must be a positive integer`);
  const value = Number(raw);
  if (!Number.isSafeInteger(value) || value > maximum) throw new Error(`${name} must be at most ${maximum}`);
  return value;
}

function parseArgs(argv) {
  const options = { image: "baseline", buildCandidate: false, storage: "overlay", probe: "full", iterations: 1, telemetryIntervalMs: 0, output: "" };
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === "--help") options.help = true;
    else if (arg === "--build-candidate") options.buildCandidate = true;
    else if (["--image", "--storage", "--probe", "--iterations", "--cpus", "--memory", "--telemetry-interval-ms", "--output"].includes(arg)) {
      const value = argv[index + 1];
      if (!value) throw new Error(`${arg} requires a value`);
      index += 1;
      if (arg === "--image") options.image = value;
      if (arg === "--storage") options.storage = value;
      if (arg === "--probe") options.probe = value;
      if (arg === "--iterations") options.iterations = positiveInteger(value, "iterations", 20);
      if (arg === "--cpus") options.cpus = positiveInteger(value, "cpus", 256);
      if (arg === "--memory") options.memory = positiveInteger(value, "memory", Number.MAX_SAFE_INTEGER);
      if (arg === "--telemetry-interval-ms") options.telemetryIntervalMs = positiveInteger(value, "telemetry interval", 10_000);
      if (arg === "--output") options.output = value;
    } else if (arg !== "--help" && arg !== "--build-candidate") {
      throw new Error(`unknown option ${arg}`);
    }
  }
  if (!["overlay", "volume", "tmpfs"].includes(options.storage)) {
    throw new Error("storage must be overlay, volume, or tmpfs");
  }
  if (!["full", "prepare"].includes(options.probe)) throw new Error("probe must be full or prepare");
  if (options.telemetryIntervalMs > 0 && options.telemetryIntervalMs < 250) {
    throw new Error("telemetry interval must be at least 250 ms");
  }
  return options;
}

function dockerJSON(format) {
  return JSON.parse(execFileSync("docker", ["info", "--format", format], { encoding: "utf8", timeout: 30_000 }));
}

function inspectImage(image) {
  return JSON.parse(execFileSync("docker", ["image", "inspect", image, "--format", "{{json .}}"], {
    encoding: "utf8",
    timeout: 30_000,
  }));
}

function inspectRuntimeLimits(image, cpus, memory) {
  const output = execFileSync("docker", [
    "run", "--rm", "--pull=never",
    "--cpuset-cpus", `0-${cpus - 1}`,
    "--memory", String(memory), "--memory-swap", String(memory),
    image, "sh", "-c",
    [
      "printf 'nproc\\t%s\\n' \"$(nproc)\"",
      "printf 'getconf_processors_online\\t%s\\n' \"$(getconf _NPROCESSORS_ONLN)\"",
      "printf 'cpuset_effective\\t%s\\n' \"$(cat /sys/fs/cgroup/cpuset.cpus.effective)\"",
      "printf 'cpu_max\\t%s\\n' \"$(cat /sys/fs/cgroup/cpu.max)\"",
      "printf 'memory_max\\t%s\\n' \"$(cat /sys/fs/cgroup/memory.max)\"",
    ].join("; "),
  ], { encoding: "utf8", timeout: 30_000 });
  const facts = parseRuntimeFacts(output);
  const expectedCpuset = cpus === 1 ? "0" : `0-${cpus - 1}`;
  const conformant = facts.nproc === cpus && facts.cpusetEffective === expectedCpuset && facts.memoryMax === memory;
  if (!conformant) {
    throw new Error(`Docker did not enforce the requested local limits: ${JSON.stringify({ expectedCpuset, cpus, memory, facts })}`);
  }
  return { ...facts, conformant };
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

function buildCandidate() {
  execFileSync("docker", [
    "build",
    "--pull=false",
    "--tag", CANDIDATE_IMAGE,
    "--file", resolve(HERE, "images/dax-dev.Dockerfile"),
    resolve(HERE, "images"),
  ], { stdio: "inherit", timeout: 900_000 });
}

function dockerStats(container, started) {
  return new Promise((resolveSample) => {
    execFile("docker", ["stats", "--no-stream", "--format", "{{json .}}", container], {
      encoding: "utf8",
      maxBuffer: 1024 * 1024,
      timeout: 10_000,
    }, (error, stdout) => {
      if (error) {
        resolveSample({ error: error.message });
        return;
      }
      try {
        resolveSample({ sample: parseDockerStats(stdout.trim(), performance.now() - started) });
      } catch (parseError) {
        resolveSample({ error: parseError instanceof Error ? parseError.message : String(parseError) });
      }
    });
  });
}

function runDocker(script, args, container, telemetryIntervalMs, started) {
  return new Promise((resolveRun) => {
    const child = spawn("docker", args, { stdio: ["pipe", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    let stdoutBytes = 0;
    let stderrBytes = 0;
    let stdoutPending = "";
    let spawnError = "";
    let timedOut = false;
    let outputOverflow = false;
    const phaseEvents = [];
    const observedPhases = new Set();
    const telemetrySamples = [];
    const telemetryErrors = [];
    let telemetryInFlight = null;

    const observePhases = (chunk, final = false) => {
      stdoutPending += chunk;
      const lines = stdoutPending.split("\n");
      stdoutPending = final ? "" : lines.pop();
      for (const line of lines) {
        const [kind, phase, duration] = line.split("\t");
        if (kind !== "BENCH_PHASE" || observedPhases.has(phase) || !Number.isFinite(Number(duration))) continue;
        observedPhases.add(phase);
        phaseEvents.push({ phase, durationMs: Number(duration), observedAtMs: performance.now() - started });
      }
    };
    const capture = (target, chunk) => {
      const text = chunk.toString("utf8");
      const bytes = Buffer.byteLength(text);
      if (target === "stdout") {
        stdoutBytes += bytes;
        if (stdoutBytes <= OUTPUT_LIMIT_BYTES) stdout += text;
        observePhases(text);
      } else {
        stderrBytes += bytes;
        if (stderrBytes <= OUTPUT_LIMIT_BYTES) stderr += text;
      }
      if ((stdoutBytes > OUTPUT_LIMIT_BYTES || stderrBytes > OUTPUT_LIMIT_BYTES) && !outputOverflow) {
        outputOverflow = true;
        child.kill("SIGKILL");
      }
    };
    child.stdout.on("data", (chunk) => capture("stdout", chunk));
    child.stderr.on("data", (chunk) => capture("stderr", chunk));
    child.on("error", (error) => { spawnError = error.message; });
    child.stdin.on("error", (error) => {
      if (error.code !== "EPIPE" && spawnError === "") spawnError = error.message;
    });
    child.stdin.end(script);

    const sample = () => {
      if (telemetryInFlight) return;
      telemetryInFlight = dockerStats(container, started).then((result) => {
        if (result.sample) telemetrySamples.push(result.sample);
        else if (result.error && telemetryErrors.length < 10 && !result.error.includes("No such container")) telemetryErrors.push(result.error);
      }).finally(() => { telemetryInFlight = null; });
    };
    const telemetryTimer = telemetryIntervalMs > 0 ? setInterval(sample, telemetryIntervalMs) : null;
    if (telemetryTimer) sample();
    const timeout = setTimeout(() => {
      timedOut = true;
      child.kill("SIGKILL");
    }, RUN_TIMEOUT_MS);

    child.on("close", async (status, signal) => {
      const commandCloseWallMs = performance.now() - started;
      clearTimeout(timeout);
      if (telemetryTimer) clearInterval(telemetryTimer);
      if (telemetryInFlight) await telemetryInFlight;
      observePhases("", true);
      if (timedOut) spawnError = `Docker workload exceeded the ${RUN_TIMEOUT_MS / 1000} second deadline`;
      if (outputOverflow) spawnError = `Docker workload output exceeded ${OUTPUT_LIMIT_BYTES} bytes`;
      resolveRun({ status, signal, error: spawnError, stdout, stderr, phaseEvents, telemetrySamples, telemetryErrors, commandCloseWallMs });
    });
  });
}

function dockerResourceExists(kind, name) {
  try {
    execFileSync("docker", [kind, "inspect", name], { stdio: ["ignore", "ignore", "pipe"], timeout: 30_000 });
    return true;
  } catch (error) {
    const detail = `${error?.stderr ?? ""}\n${error?.message ?? ""}`;
    if (/No such (object|container|volume)/i.test(detail)) return false;
    throw new Error(`cannot determine whether Docker ${kind} ${name} exists: ${detail.trim()}`);
  }
}

function removeDockerResource(kind, name) {
  const args = kind === "container" ? ["container", "rm", "--force", name] : ["volume", "rm", "--force", name];
  execFileSync("docker", args, { stdio: ["ignore", "ignore", "pipe"], timeout: 30_000 });
  if (dockerResourceExists(kind, name)) throw new Error(`Docker ${kind} ${name} still exists after removal`);
}

function cleanupActiveResources() {
  for (const container of activeContainers) {
    try { removeDockerResource("container", container); } catch { /* the attempt report owns the actionable error */ }
  }
  for (const volume of activeVolumes) {
    try { removeDockerResource("volume", volume); } catch { /* the attempt report owns the actionable error */ }
  }
}

function installSignalCleanup() {
  for (const signal of ["SIGINT", "SIGTERM"]) {
    process.once(signal, () => {
      cleanupActiveResources();
      process.exit(signal === "SIGINT" ? 130 : 143);
    });
  }
}

function normalizedArchitecture(imageArchitecture) {
  if (imageArchitecture === "amd64" || imageArchitecture === "x86_64") return "x86_64";
  if (imageArchitecture === "arm64" || imageArchitecture === "aarch64") return "aarch64";
  return imageArchitecture;
}

async function runAttempt({ script, image, imageArchitecture, reportedLogicalCPUs, storage, probe, cpus, memory, iteration, telemetryIntervalMs }) {
  const volume = storage === "volume" ? `brezel-dax-${process.pid}-${iteration}-${randomBytes(4).toString("hex")}` : "";
  const container = `brezel-dax-lab-${process.pid}-${iteration}-${randomBytes(4).toString("hex")}`;
  const cleanupErrors = [];
  if (volume) {
    execFileSync("docker", ["volume", "create", "--label", "com.infercrane.brezel.dax-lab=true", volume], { stdio: "ignore", timeout: 30_000 });
    activeVolumes.add(volume);
  }
  const args = [
    "run", "--rm", "--interactive", "--pull=never", "--name", container,
    "--label", "com.infercrane.brezel.dax-lab=true",
    "--cpuset-cpus", `0-${cpus - 1}`,
    "--memory", String(memory), "--memory-swap", String(memory),
    "--env", "BENCH_PROVIDER=brezel-local-lab",
    "--env", "BENCH_REGION=local-noncomparable",
  ];
  // Keep the upstream script byte-for-byte unchanged. A deliberately missing
  // local repository stops the prepare probe at clone, after the script has
  // emitted the measured preparation phase. That expected clone failure is
  // accepted only for this explicitly labeled local probe.
  if (probe === "prepare") args.push("--env", "BENCH_REPO_URL=file:///brezel-local-prepare-probe-stop");
  if (storage === "volume") args.push("--mount", `type=volume,src=${volume},dst=/bench`, "--env", "BENCH_ROOT=/bench/workload");
  if (storage === "tmpfs") {
    const tmpfsBytes = Math.min(6 * GIB, Math.floor(memory * 0.65));
    args.push("--tmpfs", `/bench:rw,exec,size=${tmpfsBytes}`, "--env", "BENCH_ROOT=/bench/workload");
  }
  args.push(image, "bash", "-s");

  const started = performance.now();
  let execution;
  activeContainers.add(container);
  try {
    execution = await runDocker(script, args, container, telemetryIntervalMs, started);
  } finally {
    try {
      if (dockerResourceExists("container", container)) {
        removeDockerResource("container", container);
        cleanupErrors.push("workload container required forced cleanup");
      }
      activeContainers.delete(container);
    } catch (error) {
      cleanupErrors.push(error instanceof Error ? error.message : String(error));
    }
    if (volume) {
      try {
        removeDockerResource("volume", volume);
        activeVolumes.delete(volume);
      } catch (error) {
        cleanupErrors.push(error instanceof Error ? error.message : String(error));
      }
    }
  }
  const lifecycleWallMs = performance.now() - started;
  const stdout = execution?.stdout ?? "";
  const stderr = execution?.stderr ?? "";
  const result = parseStructuredOutput(stdout, stderr);
  const fullTranscriptValid = validateFullTranscript(result, {
    architecture: normalizedArchitecture(imageArchitecture),
    logicalCPUs: reportedLogicalCPUs,
    allowEmptyCPUModel: process.platform !== "linux" || normalizedArchitecture(imageArchitecture) !== "x86_64",
  });
  const prepareTranscriptValid = result.transcriptErrors.length === 0 &&
    arraysEqual(result.markerSequence, PREPARE_MARKER_PREFIX) && result.metadata.commit === EXPECTED_WORKLOAD_COMMIT &&
    result.metadata.architecture === normalizedArchitecture(imageArchitecture) && result.metadata.logical_cpus === String(reportedLogicalCPUs) &&
    result.metadata.bun_version === EXPECTED_BUN_VERSION && result.metadata.node_version === EXPECTED_NODE_VERSION;
  const fullValid = execution?.status === 0 && !execution.error && fullTranscriptValid && result.failures.length === 0 &&
    result.errors.length === 0 && result.executionFailures.length === 0 &&
    result.completedCommit === EXPECTED_WORKLOAD_COMMIT;
  const prepareValid = execution?.status === 1 && !execution.error && prepareTranscriptValid &&
    result.failures.length === 1 && result.failures[0] === "clone" && result.errors.length === 0 &&
    result.executionFailures.length === 0;
  const valid = cleanupErrors.length === 0 && (probe === "prepare" ? prepareValid : fullValid);
  return {
    iteration,
    valid,
    exitCode: execution?.status ?? null,
    signal: execution?.signal ?? null,
    error: execution?.error ?? "",
    cleanupErrors,
    commandCloseWallMs: execution?.commandCloseWallMs ?? null,
    lifecycleWallMs,
    result,
    telemetry: {
      intervalMs: telemetryIntervalMs,
      samples: execution?.telemetrySamples ?? [],
      errors: execution?.telemetryErrors ?? [],
      phaseEvents: execution?.phaseEvents ?? [],
      phases: summarizePhaseTelemetry(execution?.telemetrySamples ?? [], execution?.phaseEvents ?? []),
    },
    stdoutTail: stdout.trim().split("\n").slice(-80).join("\n"),
    stderrTail: stderr.trim().split("\n").slice(-40).join("\n"),
  };
}

async function main() {
  installSignalCleanup();
  const options = parseArgs(process.argv.slice(2));
  if (options.help) {
    process.stdout.write(usage());
    return;
  }
  const engine = dockerJSON('{"cpus":{{.NCPU}},"memory":{{.MemTotal}},"os":{{json .OSType}},"arch":{{json .Architecture}},"driver":{{json .Driver}},"cgroup":{{json .CgroupVersion}}}');
  const cpus = options.cpus ?? Math.min(8, engine.cpus);
  if (cpus > engine.cpus) throw new Error(`requested ${cpus} CPUs but Docker exposes ${engine.cpus}`);
  const memory = options.memory ?? Math.min(16 * GIB, Math.floor(engine.memory * 0.80));
  if (memory < 4 * GIB) throw new Error("the local DAX lab requires at least 4 GiB of container memory");
  if (options.buildCandidate) buildCandidate();
  const image = options.image === "baseline" ? BASELINE_IMAGE : options.image === "candidate" ? CANDIDATE_IMAGE : options.image;
  execFileSync("docker", ["image", "inspect", image], { stdio: "ignore", timeout: 30_000 });
  const imageInfo = inspectImage(image);
  const runtimeLimits = inspectRuntimeLimits(image, cpus, memory);
  const script = await fetchPinnedScript();
  const attempts = [];
  for (let iteration = 1; iteration <= options.iterations; iteration += 1) {
    process.stderr.write(`local DAX ${iteration}/${options.iterations}: image=${image} storage=${options.storage}\n`);
    attempts.push(await runAttempt({
      script,
      image,
      imageArchitecture: imageInfo.Architecture,
      reportedLogicalCPUs: runtimeLimits.getconfProcessorsOnline,
      storage: options.storage,
      probe: options.probe,
      cpus,
      memory,
      iteration,
      telemetryIntervalMs: options.telemetryIntervalMs,
    }));
  }
  const limitations = [];
  if (process.platform !== "linux") limitations.push(`runner is ${process.platform}, not Linux`);
  if (engine.arch !== "x86_64" && engine.arch !== "amd64") limitations.push(`Docker engine is ${engine.arch}, not x86-64`);
  if (memory < 16 * GIB) limitations.push(`container memory is ${(memory / GIB).toFixed(2)} GiB, below DAX's 16 GiB target`);
  if (runtimeLimits.getconfProcessorsOnline !== runtimeLimits.nproc) {
    limitations.push(`upstream getconf reports ${runtimeLimits.getconfProcessorsOnline} logical CPUs while the enforced cgroup cpuset exposes ${runtimeLimits.nproc}`);
  }
  limitations.push("container storage does not reproduce Firecracker block, NBD, KVM, NUMA, or snapshot behavior");
  if (options.telemetryIntervalMs > 0) limitations.push("Docker stats sampling is diagnostic instrumentation and can perturb local timing");
  if (options.probe === "prepare") limitations.push("prepare probe intentionally stops at clone and does not measure a complete workload");
  const report = {
    schemaVersion: 1,
    suite: "brezel-local-dax-lab",
    evidenceClass: "local-relative-only",
    leaderboardComparable: false,
    limitations,
    upstream: { commit: UPSTREAM_COMMIT, scriptSha256: UPSTREAM_SCRIPT_SHA256 },
    configuration: { image, imageId: imageInfo.Id, imageArchitecture: imageInfo.Architecture, storage: options.storage, probe: options.probe, cpus, memoryBytes: memory, telemetryIntervalMs: options.telemetryIntervalMs, runtimeLimits },
    engine,
    summary: summarizeAttempts(attempts),
    attempts,
  };
  const json = `${JSON.stringify(report, null, 2)}\n`;
  if (options.output) writeFileSync(options.output, json, { mode: 0o600 });
  process.stdout.write(json);
  if (report.summary.succeeded !== options.iterations) process.exitCode = 1;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main();
}
