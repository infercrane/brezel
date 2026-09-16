import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { createHash } from "node:crypto";

import {
  classifyOOM,
  loadDeclaredSetup,
  parseMemoryEvidence,
  strictAttemptValid,
  summarizeDiagnosticAttempts,
} from "./firecracker-storage-diagnostic.mjs";
import { parseDaxTranscript } from "./dax-transcript.mjs";

const strictDaxOutput = `BENCH_PHASE\tprepare\t10
BENCH_CACHE\tguest_page_cache\tdropped
BENCH_CACHE\tworkspace\tfresh
BENCH_CACHE\tbun\tempty
BENCH_CACHE\tturbo\tempty
BENCH_PHASE\tcache_clear\t2
BENCH_META\tcommit\t08fb47373509ba64b13441061314eeacf4264f51
BENCH_META\tarchitecture\tx86_64
BENCH_META\tkernel\tLinux 6.8.0
BENCH_META\tlogical_cpus\t8
BENCH_META\tcpu_model\tIntel test CPU
BENCH_META\tmemory_kib\t16777216
BENCH_PHASE\tbun_download\t3
BENCH_PHASE\tbun_unpack\t4
BENCH_META\tbun_version\t1.3.14
BENCH_META\tnode_version\tv24.14.1
BENCH_PHASE\tclone\t5
BENCH_DISK\tafter_clone\t1000000
BENCH_PHASE\tinstall\t6
BENCH_DISK\tafter_install\t2000000
BENCH_PHASE\ttypecheck\t7
BENCH_DISK\tafter_typecheck\t3000000
BENCH_DONE\t08fb47373509ba64b13441061314eeacf4264f51
BENCH_PHASE\ttotal\t40
`;

function memoryEvidence({ oom = 0, oomKill = 0, current = 1024, kernel = [] } = {}) {
  return parseMemoryEvidence(`BREZEL_DIAG_CGROUP_PATH\t/
BREZEL_DIAG_MEMORY_SCALAR\tcurrent\t${current}
BREZEL_DIAG_MEMORY_SCALAR\tpeak\t2048
BREZEL_DIAG_MEMORY_SCALAR\tmax\t17179869184
BREZEL_DIAG_AVAILABILITY\tevents\tavailable
BREZEL_DIAG_MEMORY_EVENT\tlow\t0
BREZEL_DIAG_MEMORY_EVENT\thigh\t0
BREZEL_DIAG_MEMORY_EVENT\tmax\t0
BREZEL_DIAG_MEMORY_EVENT\toom\t${oom}
BREZEL_DIAG_MEMORY_EVENT\toom_kill\t${oomKill}
BREZEL_DIAG_AVAILABILITY\tlocal_events\tunavailable
BREZEL_DIAG_AVAILABILITY\tdmesg\tavailable
${kernel.map((line) => `BREZEL_DIAG_KERNEL_OOM\t${line}`).join("\n")}`);
}

function successfulAttempt() {
  const before = memoryEvidence();
  const after = memoryEvidence({ current: 2048 });
  return {
    setup: { exitCode: 0 },
    dax: {
      exitCode: 0,
      wallMs: 50,
      strictTranscriptValid: true,
      result: parseDaxTranscript(strictDaxOutput),
    },
    memory: { before, after },
    oom: classifyOOM(before, after, 0),
    cleanup: "confirmed",
  };
}

test("loadDeclaredSetup accepts only the explicitly digested regular file", () => {
  const directory = mkdtempSync(join(tmpdir(), "brezel-storage-setup-"));
  try {
    const path = join(directory, "setup.sh");
    const script = "mount -t tmpfs -o size=8g tmpfs /tmp\n";
    writeFileSync(path, script, { mode: 0o600 });
    const sha256 = createHash("sha256").update(script).digest("hex");
    assert.deepEqual(loadDeclaredSetup(path, sha256), { script, sha256, bytes: Buffer.byteLength(script) });
    assert.throws(() => loadDeclaredSetup(path, "0".repeat(64)), /does not match/);
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("parseMemoryEvidence preserves counters, availability, and bounded kernel OOM evidence", () => {
  const evidence = memoryEvidence({ oom: 2, oomKill: 1, kernel: ["Out of memory: Killed process 42"] });
  assert.deepEqual(evidence.parseErrors, []);
  assert.equal(evidence.events.oom, 2);
  assert.equal(evidence.scalars.current, 1024);
  assert.equal(evidence.availability.localEvents, "unavailable");
  assert.deepEqual(evidence.kernelOomLines, ["Out of memory: Killed process 42"]);
});

test("parseMemoryEvidence fails closed on duplicate or malformed evidence", () => {
  const duplicate = parseMemoryEvidence(`BREZEL_DIAG_CGROUP_PATH\t/
BREZEL_DIAG_CGROUP_PATH\t/other
BREZEL_DIAG_MEMORY_SCALAR\tcurrent\tmax
BREZEL_DIAG_MEMORY_SCALAR\tpeak\t2
BREZEL_DIAG_MEMORY_SCALAR\tmax\tmax
BREZEL_DIAG_AVAILABILITY\tevents\tunavailable
BREZEL_DIAG_AVAILABILITY\tlocal_events\tunavailable
BREZEL_DIAG_AVAILABILITY\tdmesg\tunavailable
`);
  assert.match(duplicate.parseErrors.join("\n"), /duplicate cgroup_path/);
  assert.match(duplicate.parseErrors.join("\n"), /memory scalar current cannot be max/);
});

test("classifyOOM separates confirmed counter evidence from a possible signal exit", () => {
  const before = memoryEvidence();
  const after = memoryEvidence({ oom: 1, oomKill: 1 });
  const confirmed = classifyOOM(before, after, 137);
  assert.equal(confirmed.confirmed, true);
  assert.equal(confirmed.possibleSignalExit, true);
  assert.equal(confirmed.counters.oomKillDelta, 1);

  const signalOnly = classifyOOM(before, memoryEvidence(), 137);
  assert.equal(signalOnly.confirmed, false);
  assert.equal(signalOnly.possibleSignalExit, true);
});

test("strictAttemptValid and summary exclude setup, OOM, transcript, or cleanup failures", () => {
  const valid = successfulAttempt();
  assert.equal(strictAttemptValid(valid), true);
  const invalid = structuredClone(valid);
  invalid.oom.confirmed = true;
  assert.equal(strictAttemptValid(invalid), false);
  const summary = summarizeDiagnosticAttempts([valid, invalid]);
  assert.equal(summary.succeeded, 1);
  assert.equal(summary.phaseMs.typecheck.p50Ms, 7);
  assert.equal(summary.daxWallMs.p50Ms, 50);
});

test("missing cgroup OOM counters prevents a strict success claim", () => {
  const attempt = successfulAttempt();
  attempt.memory.before.availability.events = "unavailable";
  attempt.memory.after.availability.events = "unavailable";
  attempt.oom = classifyOOM(attempt.memory.before, attempt.memory.after, 0);
  assert.equal(attempt.oom.evidenceComplete, false);
  assert.equal(strictAttemptValid(attempt), false);
});
