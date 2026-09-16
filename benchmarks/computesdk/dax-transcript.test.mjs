import assert from "node:assert/strict";
import test from "node:test";

import {
  parseDaxTranscript,
  validateDaxPrepareProbeTranscript,
  validateDaxTranscript,
} from "./dax-transcript.mjs";

const commit = "08fb47373509ba64b13441061314eeacf4264f51";

function prefix() {
  return [
    "BENCH_PHASE\tprepare\t10",
    "BENCH_CACHE\tguest_page_cache\tdropped",
    "BENCH_CACHE\tworkspace\tfresh",
    "BENCH_CACHE\tbun\tempty",
    "BENCH_CACHE\tturbo\tempty",
    "BENCH_PHASE\tcache_clear\t2",
    `BENCH_META\tcommit\t${commit}`,
    "BENCH_META\tarchitecture\tx86_64",
    "BENCH_META\tkernel\tLinux 6.8.0",
    "BENCH_META\tlogical_cpus\t8",
    "BENCH_META\tcpu_model\tQualified CPU",
    "BENCH_META\tmemory_kib\t16777216",
    "BENCH_PHASE\tbun_download\t3",
    "BENCH_PHASE\tbun_unpack\t4",
    "BENCH_META\tbun_version\t1.3.14",
    "BENCH_META\tnode_version\tv24.14.1",
    "BENCH_PHASE\tclone\t5",
  ];
}

function completeTranscript() {
  return [
    ...prefix(),
    "BENCH_DISK\tafter_clone\t1000000",
    "BENCH_PHASE\tinstall\t6",
    "BENCH_DISK\tafter_install\t2000000",
    "BENCH_PHASE\ttypecheck\t7",
    "BENCH_DISK\tafter_typecheck\t2000000",
    `BENCH_DONE\t${commit}`,
    "BENCH_PHASE\ttotal\t40",
  ].join("\n");
}

test("one strict parser exposes compatible error aliases and resource policies", () => {
  const parsed = parseDaxTranscript(completeTranscript());
  assert.strictEqual(parsed.parseIssues, parsed.transcriptErrors);
  assert.equal(validateDaxTranscript(parsed, {
    architecture: "x86_64", logicalCPUs: 8, minimumMemoryKiB: 4 * 1024 * 1024,
  }), true);
  assert.equal(validateDaxTranscript(parsed, {
    architecture: "x86_64", logicalCPUs: 8, minimumMemoryKiB: 20 * 1024 * 1024,
  }), false);
});

test("full validation fails closed when cold-cache evidence is missing", () => {
  const parsed = parseDaxTranscript(completeTranscript().replace("BENCH_CACHE\tbun\tempty\n", ""));
  assert.equal(validateDaxTranscript(parsed, { architecture: "x86_64", logicalCPUs: 8 }), false);

  const hiddenFailure = parseDaxTranscript(completeTranscript(), "gyp ERR! build error");
  assert.notEqual(hiddenFailure.executionFailures.length, 0);
  assert.equal(validateDaxTranscript(hiddenFailure, { architecture: "x86_64", logicalCPUs: 8 }), false);
});

test("the shared parser validates the expected local prepare-probe failure only", () => {
  const expected = parseDaxTranscript(`${prefix().join("\n")}\nBENCH_FAIL\tclone\n`);
  assert.equal(validateDaxPrepareProbeTranscript(expected, { architecture: "x86_64", logicalCPUs: 8 }), true);

  const wrongFailure = parseDaxTranscript(`${prefix().join("\n")}\nBENCH_FAIL\tinstall\n`);
  assert.equal(validateDaxPrepareProbeTranscript(wrongFailure, { architecture: "x86_64", logicalCPUs: 8 }), false);
});
