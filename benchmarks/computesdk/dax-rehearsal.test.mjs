import assert from "node:assert/strict";
import test from "node:test";

import { pairedABIdentity, structuredLines, summarizeAttempts, transcriptValid } from "./dax-rehearsal.mjs";

const completeOutput = `BENCH_PHASE\tprepare\t10
BENCH_CACHE\tguest_page_cache\tdropped
BENCH_CACHE\tworkspace\tfresh
BENCH_CACHE\tbun\tempty
BENCH_CACHE\tturbo\tempty
BENCH_PHASE\tcache_clear\t2
BENCH_META\tcommit\t08fb47373509ba64b13441061314eeacf4264f51
BENCH_META\tarchitecture\tx86_64
BENCH_META\tkernel\tLinux 6.8.0
BENCH_META\tlogical_cpus\t8
BENCH_META\tcpu_model\tIntel Xeon
BENCH_META\tmemory_kib\t16777216
BENCH_PHASE\tbun_download\t3
BENCH_PHASE\tbun_unpack\t4
BENCH_META\tbun_version\t1.3.14
BENCH_META\tnode_version\tv24.14.1
BENCH_PHASE\tclone\t5
BENCH_DISK\tafter_clone\t1e+6
BENCH_PHASE\tinstall\t6
BENCH_DISK\tafter_install\t2e+6
BENCH_PHASE\ttypecheck\t7
BENCH_DISK\tafter_typecheck\t2e+6
BENCH_DONE\t08fb47373509ba64b13441061314eeacf4264f51
BENCH_PHASE\ttotal\t40
`;

test("structuredLines retains exact phase, cache, metadata and disk evidence", () => {
  const parsed = structuredLines(completeOutput);
  assert.deepEqual(parsed.cache, {
    guest_page_cache: "dropped",
    workspace: "fresh",
    bun: "empty",
    turbo: "empty",
  });
  assert.equal(parsed.phases.typecheck, 7);
  assert.equal(parsed.disk.after_typecheck, 2_000_000);
  assert.equal(parsed.metadata.logical_cpus, "8");
  assert.deepEqual(parsed.parseIssues, []);
  assert.equal(transcriptValid(parsed, { cpus: 8, architecture: "x86_64" }), true);
});

test("structuredLines fails closed on duplicate or malformed evidence", () => {
  const duplicate = structuredLines(`${completeOutput}BENCH_PHASE\tprepare\t11\n`);
  assert.match(duplicate.parseIssues.join("\n"), /duplicate phase key prepare/);

  const malformed = structuredLines("BENCH_PHASE\tprepare\tnan\nBENCH_UNKNOWN\tvalue\n");
  assert.match(malformed.parseIssues.join("\n"), /malformed BENCH_PHASE/);
  assert.match(malformed.parseIssues.join("\n"), /unsupported structured line BENCH_UNKNOWN/);
});

test("structuredLines rejects an otherwise complete but reordered transcript", () => {
  const reordered = completeOutput.replace(
    "BENCH_META\tcommit\t08fb47373509ba64b13441061314eeacf4264f51\nBENCH_META\tarchitecture\tx86_64",
    "BENCH_META\tarchitecture\tx86_64\nBENCH_META\tcommit\t08fb47373509ba64b13441061314eeacf4264f51",
  );
  const parsed = structuredLines(reordered);
  assert.deepEqual(parsed.parseIssues, []);
  assert.equal(transcriptValid(parsed, { cpus: 8, architecture: "x86_64" }), false);
});

test("summarizeAttempts publishes leaderboard phase medians without dropping tails", () => {
  const phaseNames = ["prepare", "cache_clear", "bun_download", "bun_unpack", "clone", "install", "typecheck", "total"];
  const attempts = [1, 2, 30].map((value) => ({
    exitCode: 0,
    totalMs: value * 10,
    result: { phases: Object.fromEntries(phaseNames.map((name) => [name, value])) },
  }));
  const summary = summarizeAttempts(attempts);
  assert.equal(summary.p50Ms, 20);
  assert.equal(summary.p95Ms, 300);
  assert.deepEqual(summary.phaseMs.typecheck, {
    samples: 3,
    minMs: 1,
    p50Ms: 2,
    p95Ms: 30,
    p99Ms: 30,
    maxMs: 30,
  });
});

test("pairedABIdentity is absent or complete and rejects partial identity", () => {
  assert.equal(pairedABIdentity({}), undefined);
  assert.deepEqual(pairedABIdentity({
    BREZEL_DAX_PAIRED_PLAN_ID: "1".repeat(64),
    BREZEL_DAX_PAIRED_SLOT_ID: "pair-001-baseline",
    BREZEL_DAX_HOST_IDENTITY_SHA256: "2".repeat(64),
    BREZEL_DAX_CONFIGURATION_IDENTITY_SHA256: "3".repeat(64),
  }), {
    planId: "1".repeat(64),
    slotId: "pair-001-baseline",
    hostIdentitySha256: "2".repeat(64),
    configurationIdentitySha256: "3".repeat(64),
  });
  assert.throws(() => pairedABIdentity({ BREZEL_DAX_PAIRED_PLAN_ID: "1".repeat(64) }), /must be supplied together/);
});
