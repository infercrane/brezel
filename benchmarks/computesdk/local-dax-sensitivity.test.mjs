import assert from "node:assert/strict";
import test from "node:test";

import { analyzeLocalSensitivity } from "./local-dax-sensitivity.mjs";

function report(cpus, install, typecheck) {
  return {
    suite: "brezel-local-dax-lab",
    leaderboardComparable: false,
    configuration: {
      image: "brezel/dax-dev:local",
      imageId: "sha256:test",
      storage: "overlay",
      probe: "full",
      cpus,
      memoryBytes: 17179869184,
      telemetryIntervalMs: 1000,
    },
    summary: {
      requested: 1,
      succeeded: 1,
      phaseMedianMs: {
        prepare: 1000,
        bun_download: 5000,
        bun_unpack: 1000,
        clone: 10000,
        install,
        typecheck,
        total: 1000 + 5000 + 1000 + 10000 + install + typecheck,
      },
    },
  };
}

test("separates CPU-sensitive work from CPU-insensitive work", () => {
  const result = analyzeLocalSensitivity(report(4, 120000, 300000), report(8, 118000, 100000));
  assert.equal(result.leaderboardComparable, false);
  assert.equal(result.phases.install.diagnosis, "cpu-insensitive-at-this-range");
  assert.equal(result.phases.typecheck.diagnosis, "cpu-sensitive");
  assert.ok(result.phases.typecheck.cpuElasticity > 1);
});

test("rejects inputs that differ by more than CPU count", () => {
  const lower = report(4, 120000, 300000);
  const upper = report(8, 118000, 100000);
  upper.configuration.storage = "volume";
  assert.throws(() => analyzeLocalSensitivity(lower, upper), /differ in storage/);
});
