import assert from "node:assert/strict";
import test from "node:test";

import { fitInverseCPUPhase, modelDAXBottlenecks } from "./dax-bottleneck-model.mjs";

const phaseTimes = {
  prepare: 1_500,
  cache_clear: 500,
  bun_download: 6_000,
  bun_unpack: 1_000,
  clone: 14_000,
  install: 118_000,
  typecheck: 100_000,
};

function report(cpus, multiplier, overrides = {}) {
  const phases = Object.fromEntries(Object.entries(phaseTimes).map(([phase, value]) => [phase, value * multiplier]));
  Object.assign(phases, overrides);
  return {
    suite: "brezel-local-dax-lab",
    leaderboardComparable: false,
    configuration: {
      image: "brezel/dax-dev:local",
      imageId: "sha256:test",
      storage: "overlay",
      probe: "full",
      cpus,
      memoryBytes: 16 * 1024 ** 3,
      telemetryIntervalMs: 1000,
    },
    summary: {
      requested: 3,
      succeeded: 3,
      phaseMedianMs: { ...phases, total: Object.values(phases).reduce((sum, value) => sum + value, 5_000) },
    },
  };
}

test("fits a serial floor plus inverse CPU work", () => {
  const fit = fitInverseCPUPhase(300_000, 100_000, 4, 8);
  assert.equal(fit.fit, "upper-anchored-zero-floor");
  assert.equal(fit.serialFloorMs, 0);
  assert.equal(fit.parallelWorkMsCPU, 800_000);
});

test("does not invent scaling when a phase gets slower", () => {
  const fit = fitInverseCPUPhase(100_000, 120_000, 4, 8);
  assert.equal(fit.fit, "constant-no-speedup");
  assert.equal(fit.serialFloorMs, 120_000);
  assert.equal(fit.parallelWorkMsCPU, 0);
});

test("ranks measured bottlenecks and preserves the unmodeled envelope", () => {
  const lower = report(4, 1, { typecheck: 300_000, install: 121_000 });
  const upper = report(8, 1, { typecheck: 100_000, install: 118_000 });
  const result = modelDAXBottlenecks(lower, upper, { targetCPUs: [8, 32] });
  assert.equal(result.leaderboardComparable, false);
  assert.equal(result.rankedBottlenecks[0].phase, "install");
  assert.equal(result.rankedBottlenecks[1].phase, "typecheck");
  assert.equal(result.observed.unmodeledEnvelopeMs, 5_000);
  assert.equal(result.projections[0].totalMs, upper.summary.phaseMedianMs.total);
  assert.ok(result.projections[1].phasesMs.typecheck < result.projections[0].phasesMs.typecheck);
  assert.equal(result.projections[1].phasesMs.install, 115_750);
});

test("rejects a failed input instead of projecting it", () => {
  const lower = report(4, 1);
  const upper = report(8, 1);
  upper.summary.succeeded = 2;
  assert.throws(() => modelDAXBottlenecks(lower, upper), /only successful/);
});
