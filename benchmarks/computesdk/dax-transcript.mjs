import { definitiveExecutionFailures } from "./output-validation.mjs";

export const DAX_PHASES = Object.freeze([
  "prepare", "cache_clear", "bun_download", "bun_unpack", "clone", "install", "typecheck", "total",
]);
export const DAX_EXPECTED_WORKLOAD_COMMIT = "08fb47373509ba64b13441061314eeacf4264f51";
export const DAX_EXPECTED_BUN_VERSION = "1.3.14";
export const DAX_EXPECTED_NODE_VERSION = "v24.14.1";
export const DAX_FULL_MARKER_SEQUENCE = Object.freeze([
  "phase:prepare",
  "cache:guest_page_cache", "cache:workspace", "cache:bun", "cache:turbo",
  "phase:cache_clear",
  "meta:commit", "meta:architecture", "meta:kernel", "meta:logical_cpus", "meta:cpu_model", "meta:memory_kib",
  "phase:bun_download", "phase:bun_unpack", "meta:bun_version", "meta:node_version",
  "phase:clone", "disk:after_clone", "phase:install", "disk:after_install",
  "phase:typecheck", "disk:after_typecheck", "done", "phase:total",
]);
export const DAX_PREPARE_MARKER_PREFIX = Object.freeze(
  DAX_FULL_MARKER_SEQUENCE.slice(0, DAX_FULL_MARKER_SEQUENCE.indexOf("phase:clone") + 1),
);

const REQUIRED_CACHE_STATES = Object.freeze({ workspace: "fresh", bun: "empty", turbo: "empty" });

function duplicateMessage(identity, line) {
  const separator = identity.indexOf(":");
  const kind = identity.slice(0, separator);
  const key = identity.slice(separator + 1);
  const compatibilityKind = kind === "meta" ? "metadata" : kind;
  return `duplicate ${identity} (duplicate ${compatibilityKind} key ${key}): ${line}`;
}

function arraysEqual(left, right) {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

function cacheStateValid(cache) {
  return ["dropped", "unavailable"].includes(cache.guest_page_cache) &&
    Object.entries(REQUIRED_CACHE_STATES).every(([key, value]) => cache[key] === value);
}

function metadataValid(result, expected) {
  const logicalCPUs = expected.logicalCPUs ?? expected.cpus;
  const minimumMemoryKiB = expected.minimumMemoryKiB ?? 4 * 1024 * 1024;
  const parsedLogicalCPUs = Number(result.metadata.logical_cpus);
  const parsedMemoryKiB = Number(result.metadata.memory_kib);
  const requiredMetadata = ["commit", "architecture", "kernel", "logical_cpus", "memory_kib", "bun_version", "node_version"];
  return requiredMetadata.every((key) => typeof result.metadata[key] === "string" && result.metadata[key] !== "") &&
    (expected.allowEmptyCPUModel === true || (typeof result.metadata.cpu_model === "string" && result.metadata.cpu_model !== "")) &&
    result.metadata.commit === DAX_EXPECTED_WORKLOAD_COMMIT &&
    result.metadata.bun_version === DAX_EXPECTED_BUN_VERSION &&
    result.metadata.node_version === DAX_EXPECTED_NODE_VERSION &&
    (!expected.architecture || result.metadata.architecture === expected.architecture) &&
    /^(0|[1-9][0-9]*)$/.test(result.metadata.logical_cpus) && Number.isSafeInteger(parsedLogicalCPUs) && parsedLogicalCPUs > 0 &&
    (!Number.isFinite(logicalCPUs) || parsedLogicalCPUs === logicalCPUs) &&
    /^(0|[1-9][0-9]*)$/.test(result.metadata.memory_kib) && Number.isSafeInteger(parsedMemoryKiB) &&
    Number.isSafeInteger(minimumMemoryKiB) && minimumMemoryKiB > 0 && parsedMemoryKiB >= minimumMemoryKiB;
}

export function parseDaxTranscript(stdout, stderr = "") {
  const transcriptErrors = [];
  const result = {
    phases: {}, metadata: {}, disk: {}, cache: {}, failures: [], errors: [], completedCommit: "",
    markerSequence: [], transcriptErrors, parseIssues: transcriptErrors,
  };
  const seen = new Set();
  const recordOnce = (identity, line) => {
    if (seen.has(identity)) {
      transcriptErrors.push(duplicateMessage(identity, line));
      return false;
    }
    seen.add(identity);
    return true;
  };

  for (const line of String(stdout).split("\n")) {
    if (!line.startsWith("BENCH_")) continue;
    const parts = line.split("\t");
    const [kind, key, value] = parts;
    if (kind === "BENCH_PHASE") {
      if (parts.length !== 3 || !/^[a-z_]+$/.test(key ?? "") || !/^(0|[1-9][0-9]*)$/.test(value ?? "")) {
        transcriptErrors.push(`malformed phase marker (malformed BENCH_PHASE): ${line}`);
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
      if (parts.length !== 3 || !/^[a-z_]+$/.test(key ?? "") || value === undefined || (!emptyValueAllowed && value === "")) {
        transcriptErrors.push(`malformed metadata marker (malformed BENCH_META): ${line}`);
        continue;
      }
      if (!recordOnce(`meta:${key}`, line)) continue;
      result.metadata[key] = value;
      result.markerSequence.push(`meta:${key}`);
      continue;
    }
    if (kind === "BENCH_DISK") {
      // POSIX awk may render large integer byte counts in decimal scientific
      // notation. Accept only that bounded grammar, never general JS numbers.
      if (parts.length !== 3 || !/^[a-z_]+$/.test(key ?? "") ||
          !/^(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:e\+[0-9]+)?$/.test(value ?? "")) {
        transcriptErrors.push(`malformed disk marker (malformed BENCH_DISK): ${line}`);
        continue;
      }
      const parsed = Number(value);
      if (!Number.isSafeInteger(parsed)) {
        transcriptErrors.push(`unsafe disk marker: ${line}`);
        continue;
      }
      if (!recordOnce(`disk:${key}`, line)) continue;
      result.disk[key] = parsed;
      result.markerSequence.push(`disk:${key}`);
      continue;
    }
    if (kind === "BENCH_CACHE") {
      if (parts.length !== 3 || !/^[a-z_]+$/.test(key ?? "") || !/^[a-z_]+$/.test(value ?? "")) {
        transcriptErrors.push(`malformed cache marker (malformed BENCH_CACHE): ${line}`);
        continue;
      }
      if (!recordOnce(`cache:${key}`, line)) continue;
      result.cache[key] = value;
      result.markerSequence.push(`cache:${key}`);
      continue;
    }
    if (kind === "BENCH_DONE") {
      if (parts.length !== 2 || !/^[0-9a-f]{40}$/.test(key ?? "") || !recordOnce("done:commit", line)) {
        if (!transcriptErrors.some((entry) => entry.endsWith(line))) transcriptErrors.push(`malformed completion marker: ${line}`);
        continue;
      }
      result.completedCommit = key;
      result.markerSequence.push("done");
      continue;
    }
    if (kind === "BENCH_FAIL") {
      if (parts.length !== 2 || !/^[a-z_]+$/.test(key ?? "") || !recordOnce(`fail:${key}`, line)) {
        if (!transcriptErrors.some((entry) => entry.endsWith(line))) transcriptErrors.push(`malformed failure marker: ${line}`);
        continue;
      }
      result.failures.push(key);
      continue;
    }
    transcriptErrors.push(`unknown stdout marker (unsupported structured line ${kind}): ${line}`);
  }

  for (const line of String(stderr).split("\n")) {
    if (!line.startsWith("BENCH_")) continue;
    const parts = line.split("\t");
    const [kind, key, value] = parts;
    if (kind !== "BENCH_ERROR" || parts.length !== 3 || !/^[a-z_]+$/.test(key ?? "") || value === "") {
      transcriptErrors.push(`unexpected stderr marker: ${line}`);
      continue;
    }
    result.errors.push(`${key}:${value}`);
  }
  result.executionFailures = definitiveExecutionFailures(stderr);
  return result;
}

export function validateDaxTranscript(result, expected = {}) {
  const diskMonotonic = Number.isSafeInteger(result.disk.after_clone) && result.disk.after_clone > 0 &&
    Number.isSafeInteger(result.disk.after_install) && result.disk.after_install >= result.disk.after_clone &&
    Number.isSafeInteger(result.disk.after_typecheck) && result.disk.after_typecheck >= result.disk.after_install;
  return result.transcriptErrors.length === 0 && arraysEqual(result.markerSequence, DAX_FULL_MARKER_SEQUENCE) &&
    metadataValid(result, expected) && cacheStateValid(result.cache) && diskMonotonic &&
    result.completedCommit === DAX_EXPECTED_WORKLOAD_COMMIT && result.failures.length === 0 &&
    result.errors.length === 0 && result.executionFailures.length === 0;
}

export function validateDaxPrepareProbeTranscript(result, expected = {}) {
  return result.transcriptErrors.length === 0 && arraysEqual(result.markerSequence, DAX_PREPARE_MARKER_PREFIX) &&
    metadataValid(result, expected) && cacheStateValid(result.cache) &&
    result.completedCommit === "" && Object.keys(result.disk).length === 0 &&
    result.failures.length === 1 && result.failures[0] === "clone" &&
    result.errors.length === 0 && result.executionFailures.length === 0;
}
