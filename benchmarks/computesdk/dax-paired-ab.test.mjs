import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  buildPlan,
  mergeReports,
  nextSlot,
  validatePlan,
  validateSlotReport,
} from "./dax-paired-ab.mjs";
import { DAX_UPSTREAM, summarizeAttempts } from "./dax-rehearsal.mjs";
import { DAX_PHASES, parseDaxTranscript } from "./dax-transcript.mjs";

const SOURCE_REVISION = "1".repeat(40);
const HOST_IDENTITY = "2".repeat(64);
const BASELINE_IDENTITY = "3".repeat(64);
const CANDIDATE_IDENTITY = "4".repeat(64);

function config(overrides = {}) {
  return {
    schemaVersion: 1,
    suite: "brezel-dax-paired-ab-config",
    seed: "n2-vcpu-causal-test",
    pairs: 5,
    iterationsPerSlot: 3,
    bootstrapSamples: 1_000,
    commonIdentity: {
      sourceRevision: SOURCE_REVISION,
      endpoint: "https://sandbox.example.test/api/",
      region: "us-east4",
      hostIdentitySha256: HOST_IDENTITY,
    },
    arms: {
      baseline: {
        environmentRevision: "envr_baseline",
        configurationIdentitySha256: BASELINE_IDENTITY,
        guest: { cpus: 8, architecture: "x86_64", memoryKiB: 16_792_000, minimumFreeRootKiB: 16_777_216, uid: 0 },
      },
      candidate: {
        environmentRevision: "envr_candidate",
        configurationIdentitySha256: CANDIDATE_IDENTITY,
        guest: { cpus: 16, architecture: "x86_64", memoryKiB: 33_584_000, minimumFreeRootKiB: 16_777_216, uid: 0 },
      },
    },
    ...overrides,
  };
}

function transcript(guest, factor, reorder = false) {
  const values = Object.fromEntries(DAX_PHASES.map((phase, index) => [phase, Math.max(1, Math.round((index + 1) * factor))]));
  let output = `BENCH_PHASE\tprepare\t${values.prepare}
BENCH_CACHE\tguest_page_cache\tdropped
BENCH_CACHE\tworkspace\tfresh
BENCH_CACHE\tbun\tempty
BENCH_CACHE\tturbo\tempty
BENCH_PHASE\tcache_clear\t${values.cache_clear}
BENCH_META\tcommit\t08fb47373509ba64b13441061314eeacf4264f51
BENCH_META\tarchitecture\t${guest.architecture}
BENCH_META\tkernel\tLinux 6.8.0-test
BENCH_META\tlogical_cpus\t${guest.cpus}
BENCH_META\tcpu_model\tIntel test CPU
BENCH_META\tmemory_kib\t${guest.memoryKiB}
BENCH_PHASE\tbun_download\t${values.bun_download}
BENCH_PHASE\tbun_unpack\t${values.bun_unpack}
BENCH_META\tbun_version\t1.3.14
BENCH_META\tnode_version\tv24.14.1
BENCH_PHASE\tclone\t${values.clone}
BENCH_DISK\tafter_clone\t1000000
BENCH_PHASE\tinstall\t${values.install}
BENCH_DISK\tafter_install\t2000000
BENCH_PHASE\ttypecheck\t${values.typecheck}
BENCH_DISK\tafter_typecheck\t3000000
BENCH_DONE\t08fb47373509ba64b13441061314eeacf4264f51
BENCH_PHASE\ttotal\t${values.total}
`;
  if (reorder) {
    output = output.replace(
      "BENCH_META\tcommit\t08fb47373509ba64b13441061314eeacf4264f51\nBENCH_META\tarchitecture\tx86_64",
      "BENCH_META\tarchitecture\tx86_64\nBENCH_META\tcommit\t08fb47373509ba64b13441061314eeacf4264f51",
    );
  }
  return parseDaxTranscript(output);
}

function timestamp(milliseconds) {
  return new Date(Date.UTC(2026, 8, 16, 12, 0, 0) + milliseconds).toISOString();
}

function report(plan, slot, options = {}) {
  const arm = plan.arms[slot.arm];
  const factor = options.factor ?? (slot.arm === "candidate" ? 8 : 10);
  const reportStart = slot.sequence * 100_000;
  const attempts = Array.from({ length: plan.iterationsPerSlot }, (_, index) => {
    const start = reportStart + 1_000 + index * 20_000;
    return {
      iteration: index + 1,
      startedAt: timestamp(start),
      cleanup: "confirmed",
      createMs: factor * 10 + index,
      buildStartedAt: timestamp(start + 1_000),
      totalMs: factor * 1_000 + index * 10,
      buildFinishedAt: timestamp(start + 8_000),
      exitCode: 0,
      result: transcript(arm.guest, factor + index, options.reorder === true && index === 0),
      stderrTail: "",
      destroyMs: factor * 5 + index,
      finishedAt: timestamp(start + 10_000),
    };
  });
  return {
    schemaVersion: 2,
    suite: "computesdk-dax-rehearsal",
    upstream: DAX_UPSTREAM,
    region: plan.commonIdentity.region,
    startedAt: timestamp(reportStart),
    finishedAt: timestamp(reportStart + 90_000),
    provenance: {
      sourceRevision: plan.commonIdentity.sourceRevision,
      environmentRevision: arm.environmentRevision,
      endpoint: plan.commonIdentity.endpoint,
      runner: { node: "v24.14.1", platform: "linux", arch: "x64" },
      allowInternet: true,
      measuredState: "fresh sandbox after one disclosed shape preflight",
      pairedAB: {
        planId: plan.planId,
        slotId: slot.slotId,
        hostIdentitySha256: plan.commonIdentity.hostIdentitySha256,
        configurationIdentitySha256: arm.configurationIdentitySha256,
      },
    },
    methodology: {
      iterations: plan.iterationsPerSlot,
      concurrency: 1,
      sandboxState: "fresh sandbox per iteration",
      scoredBoundary: "runCommand request through complete command result",
      upstreamPhaseOrder: DAX_PHASES,
      cleanupBoundary: "destroy confirmation followed by empty-project inventory",
    },
    guest: {
      cpus: arm.guest.cpus,
      memory_kib: arm.guest.memoryKiB,
      free_root_kib: arm.guest.minimumFreeRootKiB + 1,
      uid: arm.guest.uid,
      architecture: arm.guest.architecture,
    },
    requested: plan.iterationsPerSlot,
    succeeded: plan.iterationsPerSlot,
    cleanupConfirmed: plan.iterationsPerSlot,
    projectEmptyBefore: true,
    projectEmptyAfter: true,
    cleanupConformant: true,
    summary: summarizeAttempts(attempts),
    attempts,
  };
}

function withReports(plan, callback) {
  const directory = mkdtempSync(join(tmpdir(), "brezel-dax-paired-"));
  try {
    for (const slot of plan.schedule) {
      writeFileSync(join(directory, `${slot.slotId}.report.json`), `${JSON.stringify(report(plan, slot))}\n`, { mode: 0o600 });
    }
    return callback(directory);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
}

test("buildPlan creates a deterministic balanced randomized paired schedule", () => {
  const first = buildPlan(config());
  const second = buildPlan(config());
  assert.deepEqual(first, second);
  assert.equal(first.schedule.length, 10);
  assert.equal(first.schedule.filter((slot) => slot.position === 1 && slot.arm === "baseline").length, 3);
  assert.equal(first.schedule.filter((slot) => slot.position === 1 && slot.arm === "candidate").length, 2);
  assert.notDeepEqual(first.schedule, buildPlan(config({ seed: "different-seed" })).schedule);
  assert.match(first.planId, /^[0-9a-f]{64}$/);
});

test("validatePlan rejects schedule and identity tampering", () => {
  const plan = buildPlan(config());
  assert.deepEqual(validatePlan(plan), plan);
  const reordered = structuredClone(plan);
  [reordered.schedule[0], reordered.schedule[1]] = [reordered.schedule[1], reordered.schedule[0]];
  assert.throws(() => validatePlan(reordered), /plan content, schedule, or planId is invalid/);
  const identity = structuredClone(plan);
  identity.commonIdentity.hostIdentitySha256 = "9".repeat(64);
  assert.throws(() => validatePlan(identity), /plan content, schedule, or planId is invalid/);
});

test("mergeReports validates every transcript and returns deterministic paired bootstrap inference", () => {
  const plan = buildPlan(config());
  withReports(plan, (directory) => {
    const first = mergeReports(plan, directory);
    const second = mergeReports(plan, directory);
    assert.deepEqual(first, second);
    assert.equal(first.metrics.total.pairCount, 5);
    assert.ok(first.metrics.total.medianDeltaMs < 0);
    assert.ok(first.metrics.provider_total_ms.medianSpeedup > 1);
    assert.ok(first.metrics.total.ci95.deltaMs.upper < 0);
    assert.equal(first.evidence.length, 10);
    assert.match(first.evidence[0].reportSha256, /^[0-9a-f]{64}$/);
  });
});

test("validateSlotReport fails closed on reordered transcript and guest drift", () => {
  const plan = buildPlan(config());
  const slot = plan.schedule[0];
  assert.doesNotThrow(() => validateSlotReport(report(plan, slot), plan, slot));
  assert.throws(() => validateSlotReport(report(plan, slot, { reorder: true }), plan, slot), /exact valid pinned DAX transcript/);
  const drifted = report(plan, slot);
  drifted.guest.cpus += 1;
  assert.throws(() => validateSlotReport(drifted, plan, slot), /guest identity or qualified capacity/);
});

test("nextSlot accepts only a contiguous immutable schedule prefix", () => {
  const plan = buildPlan(config());
  const directory = mkdtempSync(join(tmpdir(), "brezel-dax-next-"));
  try {
    assert.deepEqual(nextSlot(plan, directory), plan.schedule[0]);
    const first = plan.schedule[0];
    writeFileSync(join(directory, `${first.slotId}.report.json`), JSON.stringify(report(plan, first)), { mode: 0o600 });
    assert.deepEqual(nextSlot(plan, directory), plan.schedule[1]);
    const third = plan.schedule[2];
    writeFileSync(join(directory, `${third.slotId}.report.json`), JSON.stringify(report(plan, third)), { mode: 0o600 });
    assert.throws(() => nextSlot(plan, directory), /exists after an unfilled schedule slot/);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("mergeReports rejects unexpected report files", () => {
  const plan = buildPlan(config());
  withReports(plan, (directory) => {
    writeFileSync(join(directory, "foreign.report.json"), "{}", { mode: 0o600 });
    assert.throws(() => mergeReports(plan, directory), /unexpected DAX reports/);
  });
});

test("mergeReports rejects machine drift between otherwise valid slots", () => {
  const plan = buildPlan(config());
  withReports(plan, (directory) => {
    const slot = plan.schedule[1];
    const path = join(directory, `${slot.slotId}.report.json`);
    const drifted = report(plan, slot);
    for (const attempt of drifted.attempts) attempt.result.metadata.cpu_model = "Different test CPU";
    writeFileSync(path, JSON.stringify(drifted), { mode: 0o600 });
    assert.throws(() => mergeReports(plan, directory), /machine identity differs/);
  });
});
