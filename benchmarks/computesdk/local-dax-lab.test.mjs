import assert from "node:assert/strict";
import test from "node:test";

import { parseRuntimeFacts, parseStructuredOutput, percentile, summarizeAttempts } from "./local-dax-lab.mjs";

test("parses a complete DAX result", () => {
  const stdout = [
    "BENCH_PHASE\tprepare\t1200",
    "BENCH_PHASE\tcache_clear\t4",
    "BENCH_PHASE\tbun_download\t200",
    "BENCH_PHASE\tbun_unpack\t300",
    "BENCH_PHASE\tclone\t1000",
    "BENCH_PHASE\tinstall\t9000",
    "BENCH_PHASE\ttypecheck\t18000",
    "BENCH_PHASE\ttotal\t30000",
    "BENCH_META\tlogical_cpus\t8",
    "BENCH_DISK\tafter_install\t3000000000",
    "BENCH_DONE\t08fb47373509ba64b13441061314eeacf4264f51",
  ].join("\n");
  const result = parseStructuredOutput(stdout);
  assert.equal(result.phases.typecheck, 18000);
  assert.equal(result.metadata.logical_cpus, "8");
  assert.equal(result.disk.after_install, 3000000000);
  assert.equal(result.completedCommit, "08fb47373509ba64b13441061314eeacf4264f51");
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
