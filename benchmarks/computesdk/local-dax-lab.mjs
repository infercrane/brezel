#!/usr/bin/env node

import { createHash, randomBytes } from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
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
const GIB = 1024 ** 3;

export function parseStructuredOutput(stdout, stderr = "") {
  const result = { phases: {}, metadata: {}, disk: {}, failures: [], errors: [], completedCommit: "" };
  for (const line of `${stdout}\n${stderr}`.split("\n")) {
    const [kind, key, value] = line.split("\t");
    if (kind === "BENCH_PHASE") result.phases[key] = Number(value);
    if (kind === "BENCH_META") result.metadata[key] = value;
    if (kind === "BENCH_DISK") result.disk[key] = Number(value);
    if (kind === "BENCH_DONE") result.completedCommit = key;
    if (kind === "BENCH_FAIL") result.failures.push(key);
    if (kind === "BENCH_ERROR") result.errors.push([key, value].filter(Boolean).join(":"));
  }
  result.executionFailures = definitiveExecutionFailures(stderr);
  return result;
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
  const options = { image: "baseline", buildCandidate: false, storage: "overlay", probe: "full", iterations: 1, output: "" };
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === "--help") options.help = true;
    else if (arg === "--build-candidate") options.buildCandidate = true;
    else if (["--image", "--storage", "--probe", "--iterations", "--cpus", "--memory", "--output"].includes(arg)) {
      const value = argv[index + 1];
      if (!value) throw new Error(`${arg} requires a value`);
      index += 1;
      if (arg === "--image") options.image = value;
      if (arg === "--storage") options.storage = value;
      if (arg === "--probe") options.probe = value;
      if (arg === "--iterations") options.iterations = positiveInteger(value, "iterations", 20);
      if (arg === "--cpus") options.cpus = positiveInteger(value, "cpus", 256);
      if (arg === "--memory") options.memory = positiveInteger(value, "memory", Number.MAX_SAFE_INTEGER);
      if (arg === "--output") options.output = value;
    } else if (arg !== "--help" && arg !== "--build-candidate") {
      throw new Error(`unknown option ${arg}`);
    }
  }
  if (!["overlay", "volume", "tmpfs"].includes(options.storage)) {
    throw new Error("storage must be overlay, volume, or tmpfs");
  }
  if (!["full", "prepare"].includes(options.probe)) throw new Error("probe must be full or prepare");
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

function runAttempt({ script, image, storage, probe, cpus, memory, iteration }) {
  const volume = storage === "volume" ? `brezel-dax-${process.pid}-${iteration}-${randomBytes(4).toString("hex")}` : "";
  if (volume) execFileSync("docker", ["volume", "create", volume], { stdio: "ignore", timeout: 30_000 });
  const args = [
    "run", "--rm", "--interactive", "--pull=never",
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
  let child;
  let cleanupError = "";
  try {
    child = spawnSync("docker", args, {
      input: script,
      encoding: "utf8",
      maxBuffer: 64 * 1024 * 1024,
      timeout: 900_000,
    });
  } finally {
    if (volume) {
      try {
        execFileSync("docker", ["volume", "rm", "--force", volume], { stdio: "ignore", timeout: 30_000 });
      } catch (error) {
        // A named lab volume is never reused, so a cleanup failure cannot
        // contaminate another run. It still invalidates this attempt and is
        // preserved for explicit operator reconciliation.
        cleanupError = error instanceof Error ? error.message : String(error);
      }
    }
  }
  const wallMs = performance.now() - started;
  const stdout = child.stdout ?? "";
  const stderr = child.stderr ?? "";
  const result = parseStructuredOutput(stdout, stderr);
  const required = ["prepare", "cache_clear", "bun_download", "bun_unpack", "clone", "install", "typecheck", "total"];
  const phasesValid = required.every((phase) => Number.isFinite(result.phases[phase]) && result.phases[phase] >= 0);
  const fullValid = child.status === 0 && !child.error && phasesValid && result.failures.length === 0 &&
    result.errors.length === 0 && result.executionFailures.length === 0 &&
    result.completedCommit === "08fb47373509ba64b13441061314eeacf4264f51";
  const prepareValid = child.status === 1 && !child.error && Number.isFinite(result.phases.prepare) &&
    result.failures.length === 1 && result.failures[0] === "clone" && result.errors.length === 0 &&
    result.executionFailures.length === 0;
  const valid = cleanupError === "" && (probe === "prepare" ? prepareValid : fullValid);
  return {
    iteration,
    valid,
    exitCode: child.status,
    signal: child.signal,
    error: child.error?.message ?? "",
    cleanupError,
    wallMs,
    result,
    stdoutTail: stdout.trim().split("\n").slice(-80).join("\n"),
    stderrTail: stderr.trim().split("\n").slice(-40).join("\n"),
  };
}

async function main() {
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
  const script = await fetchPinnedScript();
  const attempts = [];
  for (let iteration = 1; iteration <= options.iterations; iteration += 1) {
    process.stderr.write(`local DAX ${iteration}/${options.iterations}: image=${image} storage=${options.storage}\n`);
    attempts.push(runAttempt({ script, image, storage: options.storage, probe: options.probe, cpus, memory, iteration }));
  }
  const limitations = [];
  if (process.platform !== "linux") limitations.push(`runner is ${process.platform}, not Linux`);
  if (engine.arch !== "x86_64" && engine.arch !== "amd64") limitations.push(`Docker engine is ${engine.arch}, not x86-64`);
  if (memory < 16 * GIB) limitations.push(`container memory is ${(memory / GIB).toFixed(2)} GiB, below DAX's 16 GiB target`);
  limitations.push("container storage does not reproduce Firecracker block, NBD, KVM, NUMA, or snapshot behavior");
  if (options.probe === "prepare") limitations.push("prepare probe intentionally stops at clone and does not measure a complete workload");
  const report = {
    schemaVersion: 1,
    suite: "brezel-local-dax-lab",
    evidenceClass: "local-relative-only",
    leaderboardComparable: false,
    limitations,
    upstream: { commit: UPSTREAM_COMMIT, scriptSha256: UPSTREAM_SCRIPT_SHA256 },
    configuration: { image, imageId: imageInfo.Id, imageArchitecture: imageInfo.Architecture, storage: options.storage, probe: options.probe, cpus, memoryBytes: memory },
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
