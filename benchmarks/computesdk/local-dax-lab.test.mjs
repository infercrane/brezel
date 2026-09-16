import assert from "node:assert/strict";
import test from "node:test";

import {
  parseByteQuantity,
  parseDockerStats,
  parseRuntimeFacts,
  parseStructuredOutput,
  percentile,
  summarizeAttempts,
  summarizePhaseTelemetry,
  validateFullTranscript,
} from "./local-dax-lab.mjs";

function completeTranscript(overrides = {}) {
  const values = {
    commit: "08fb47373509ba64b13441061314eeacf4264f51",
    architecture: "x86_64",
    logicalCPUs: "8",
    bunVersion: "1.3.14",
    nodeVersion: "v24.14.1",
    ...overrides,
  };
  return [
    "BENCH_PHASE\tprepare\t1200",
    "BENCH_PHASE\tcache_clear\t4",
    `BENCH_META\tcommit\t${values.commit}`,
    `BENCH_META\tarchitecture\t${values.architecture}`,
    "BENCH_META\tkernel\tLinux 6.8.0",
    `BENCH_META\tlogical_cpus\t${values.logicalCPUs}`,
    "BENCH_META\tcpu_model\tQualified CPU",
    "BENCH_META\tmemory_kib\t16777216",
    "BENCH_PHASE\tbun_download\t200",
    "BENCH_PHASE\tbun_unpack\t300",
    `BENCH_META\tbun_version\t${values.bunVersion}`,
    `BENCH_META\tnode_version\t${values.nodeVersion}`,
    "BENCH_PHASE\tclone\t1000",
    "BENCH_DISK\tafter_clone\t1000000",
    "BENCH_PHASE\tinstall\t9000",
    "BENCH_DISK\tafter_install\t3e+09",
    "BENCH_PHASE\ttypecheck\t18000",
    "BENCH_DISK\tafter_typecheck\t3000000000",
    `BENCH_DONE\t${values.commit}`,
    "BENCH_PHASE\ttotal\t30000",
  ].join("\n");
}

test("parses and validates a complete ordered DAX result", () => {
  const stdout = completeTranscript();
  const result = parseStructuredOutput(stdout);
  assert.equal(result.phases.typecheck, 18000);
  assert.equal(result.metadata.logical_cpus, "8");
  assert.equal(result.disk.after_install, 3000000000);
  assert.equal(result.completedCommit, "08fb47373509ba64b13441061314eeacf4264f51");
  assert.equal(validateFullTranscript(result, { architecture: "x86_64", logicalCPUs: 8 }), true);
});

test("rejects duplicate, malformed, and out-of-order benchmark evidence", () => {
  const duplicate = parseStructuredOutput(`${completeTranscript()}\nBENCH_PHASE\ttotal\t30000`);
  assert.match(duplicate.transcriptErrors[0], /duplicate phase:total/);
  assert.equal(validateFullTranscript(duplicate, { architecture: "x86_64", logicalCPUs: 8 }), false);

  const malformed = parseStructuredOutput(completeTranscript().replace("BENCH_PHASE\tinstall\t9000", "BENCH_PHASE\tinstall\t9e3"));
  assert.match(malformed.transcriptErrors[0], /malformed phase marker/);
  assert.equal(validateFullTranscript(malformed, { architecture: "x86_64", logicalCPUs: 8 }), false);

  const reorderedLines = completeTranscript().split("\n");
  [reorderedLines[12], reorderedLines[14]] = [reorderedLines[14], reorderedLines[12]];
  const reordered = parseStructuredOutput(reorderedLines.join("\n"));
  assert.equal(validateFullTranscript(reordered, { architecture: "x86_64", logicalCPUs: 8 }), false);
});

test("does not accept stdout evidence injected through stderr", () => {
  const result = parseStructuredOutput(completeTranscript(), "BENCH_PHASE\ttotal\t1");
  assert.match(result.transcriptErrors[0], /unexpected stderr marker/);
  assert.equal(validateFullTranscript(result, { architecture: "x86_64", logicalCPUs: 8 }), false);
});

test("requires the pinned versions, workload commit, and disk records", () => {
  const wrongVersion = parseStructuredOutput(completeTranscript({ bunVersion: "1.3.15" }));
  assert.equal(validateFullTranscript(wrongVersion, { architecture: "x86_64", logicalCPUs: 8 }), false);
  const missingDisk = parseStructuredOutput(completeTranscript().replace("BENCH_DISK\tafter_install\t3e+09\n", ""));
  assert.equal(validateFullTranscript(missingDisk, { architecture: "x86_64", logicalCPUs: 8 }), false);
});

test("accepts awk decimal scientific disk output but rejects general number syntax", () => {
  const scientific = parseStructuredOutput(completeTranscript());
  assert.equal(scientific.disk.after_install, 3000000000);
  assert.equal(validateFullTranscript(scientific, { architecture: "x86_64", logicalCPUs: 8 }), true);

  const hexadecimal = parseStructuredOutput(completeTranscript().replace("BENCH_DISK\tafter_install\t3e+09", "BENCH_DISK\tafter_install\t0xB2D05E00"));
  assert.match(hexadecimal.transcriptErrors[0], /malformed disk marker/);
  assert.equal(validateFullTranscript(hexadecimal, { architecture: "x86_64", logicalCPUs: 8 }), false);
});

test("combines structured and hidden execution failures", () => {
  const result = parseStructuredOutput("BENCH_FAIL\tinstall", "gyp ERR! build error");
  assert.deepEqual(result.failures, ["install"]);
  assert.notEqual(result.executionFailures.length, 0);
});

test("uses nearest-rank percentiles and phase medians", () => {
  assert.equal(percentile([30, 10, 20], 0.5), 20);
  const attempts = [10, 20, 30].map((total, index) => ({
    valid: true,
    result: { phases: { prepare: index + 1, cache_clear: 1, bun_download: 1, bun_unpack: 1, clone: 1, install: 2, typecheck: 3, total } },
  }));
  const summary = summarizeAttempts(attempts);
  assert.equal(summary.succeeded, 3);
  assert.equal(summary.phaseMedianMs.prepare, 2);
  assert.equal(summary.phaseMedianMs.total, 20);
});

test("omits unavailable phases from a partial local probe", () => {
  const summary = summarizeAttempts([{ valid: true, result: { phases: { prepare: 1444 } } }]);
  assert.equal(summary.phaseMedianMs.prepare, 1444);
  assert.equal(summary.phaseMedianMs.typecheck, null);
});

test("separates cgroup CPU enforcement from misleading getconf metadata", () => {
  const facts = parseRuntimeFacts([
    "nproc\t8",
    "getconf_processors_online\t10",
    "cpuset_effective\t0-7",
    "cpu_max\tmax 100000",
    "memory_max\t17179869184",
  ].join("\n"));
  assert.deepEqual(facts, {
    nproc: 8,
    getconfProcessorsOnline: 10,
    cpusetEffective: "0-7",
    cpuMax: "max 100000",
    memoryMax: 17179869184,
  });
});

test("parses Docker byte quantities and one stats sample", () => {
  assert.equal(parseByteQuantity("1.5GiB"), 1610612736);
  assert.equal(parseByteQuantity("1.29MB"), 1290000);
  assert.equal(parseByteQuantity("invalid"), null);
  const sample = parseDockerStats(JSON.stringify({
    BlockIO: "1.29MB / 4kB",
    CPUPerc: "752.50%",
    MemUsage: "13.2GiB / 16GiB",
    NetIO: "800MB / 12.5MB",
    PIDs: "91",
  }), 1234);
  assert.deepEqual(sample, {
    elapsedMs: 1234,
    cpuPercent: 752.5,
    memoryUsedBytes: 14173392077,
    memoryLimitBytes: 17179869184,
    blockReadBytes: 1290000,
    blockWriteBytes: 4000,
    networkReadBytes: 800000000,
    networkWriteBytes: 12500000,
    pids: 91,
  });
});

test("aligns sampled resource use with reported phase completion", () => {
  const samples = [
    { elapsedMs: 1000, cpuPercent: 100, memoryUsedBytes: 10, pids: 2, blockReadBytes: 100, blockWriteBytes: 200, networkReadBytes: 300, networkWriteBytes: 400 },
    { elapsedMs: 2000, cpuPercent: 700, memoryUsedBytes: 30, pids: 8, blockReadBytes: 150, blockWriteBytes: 500, networkReadBytes: 900, networkWriteBytes: 600 },
    { elapsedMs: 3000, cpuPercent: 800, memoryUsedBytes: 20, pids: 7, blockReadBytes: 190, blockWriteBytes: 900, networkReadBytes: 1000, networkWriteBytes: 800 },
  ];
  const summary = summarizePhaseTelemetry(samples, [{ phase: "typecheck", durationMs: 2100, observedAtMs: 3100 }]);
  assert.deepEqual(summary.typecheck, {
    samples: 3,
    startMs: 1000,
    endMs: 3100,
    cpuMeanPercent: 1600 / 3,
    cpuMaxPercent: 800,
    memoryMaxBytes: 30,
    pidsMax: 8,
    blockReadDeltaBytes: 90,
    blockWriteDeltaBytes: 700,
    networkReadDeltaBytes: 700,
    networkWriteDeltaBytes: 400,
  });
});
