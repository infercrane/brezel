import assert from "node:assert/strict";
import test from "node:test";

import { buildHostPhaseReport } from "./dax-host-telemetry-report.mjs";

function sample(second, values = {}) {
  return {
    sampled_at: `2026-09-16T00:00:0${second}.000Z`, collection_ns: 1_000_000,
    cpu: { logical_cpus: 32, delta_total_ticks: 3200, delta_busy_ticks: 800, delta_iowait_ticks: values.iowait ?? 0, delta_steal_ticks: 0, processes_running: 9, processes_blocked: values.blocked ?? 0 },
    pressure: { cpu: { some: { total_us: second * 10 }, full: { total_us: 0 } }, io: { some: { total_us: values.ioPSI ?? 0 }, full: { total_us: 0 } }, memory: { some: { total_us: 0 }, full: { total_us: 0 } } },
    memory: { mem_available_kb: 20_000_000 }, vm: { page_faults_total: second * 100, major_page_faults_total: second, swap_in_pages_total: 0, swap_out_pages_total: 0, alloc_stalls_total: 0 },
    block_devices: [{ name: "nbd0", sectors_read_total: second * 2, sectors_written_total: second * 4, io_time_ms_total: second * 5, weighted_io_time_ms_total: second * 7 }],
    processes: [{ pid: 10, name: "firecracker", start_ticks: 1, user_ticks_total: second * 20, system_ticks_total: second * 5, minor_faults_total: second * 2, major_faults_total: 0, read_bytes_total: second * 1024, write_bytes_total: second * 2048, task_samples: [{ tid: 11, start_ticks: 2, name: "fc_vcpu 0", user_ticks_total: second * 19, system_ticks_total: second, voluntary_context_switches_total: second * 3, involuntary_context_switches_total: second, last_processor: 4, cpus_allowed_list: "4-11", mems_allowed_list: "0" }] }],
    read_errors: {},
  };
}

test("buildHostPhaseReport aligns phase markers and distinguishes CPU/block evidence", () => {
  const dax = { suite: "computesdk-dax-rehearsal", guest: { cpus: 8 }, provenance: { sourceRevision: "a".repeat(40) }, startedAt: "2026-09-16T00:00:00Z", finishedAt: "2026-09-16T00:00:04Z", attempts: [{ iteration: 1, buildStartedAt: "2026-09-16T00:00:00Z", providerOverhead: { commandMinusGuestMs: 4, markerObservationMinusGuestMs: 1, streamTailMs: 3 }, phaseEvents: [{ phase: "prepare", guestDurationMs: 1900, observedAt: "2026-09-16T00:00:02Z" }, { phase: "total", guestDurationMs: 3900, observedAt: "2026-09-16T00:00:04Z" }] }] };
  const manifest = { kind: "brezel_host_telemetry", truncated: false, interval_ns: 1_000_000_000, sample_count: 5, started_at: "2026-09-16T00:00:00Z", finished_at: "2026-09-16T00:00:04Z" };
  const report = buildHostPhaseReport(dax, manifest, [0, 1, 2, 3, 4].map((value) => sample(value)));
  const prepare = report.attempts[0].phases.prepare;
  assert.equal(prepare.block.nbdWriteBytes, 4 * 2 * 512);
  assert.equal(prepare.firecracker.cpuTicks, 50);
  assert.equal(prepare.firecracker.averageVCPUCores, 0.2);
  assert.deepEqual(prepare.firecracker.placement["fc_vcpu 0"].allowedCPUs, ["4-11"]);
  assert.equal(report.leaderboardComparable, false);
  assert.deepEqual(report.attempts[0].commandBoundaryTiming, {
    commandMinusGuestMs: 4,
    markerObservationMinusGuestMs: 1,
    postTotalGuestAndProviderMs: 3,
  });
  assert.equal(report.attempts[0].providerOverhead.streamTailMs, 3, "legacy raw timing remains available");
  assert.equal("providerBound" in report.interpretation, false);
  assert.match(report.interpretation.postTotalBoundary, /EXIT-trap workspace removal.+not provider-only/);
});

test("buildHostPhaseReport rejects missing phase observations", () => {
  assert.throws(() => buildHostPhaseReport({ suite: "computesdk-dax-rehearsal", attempts: [{}] }, { kind: "brezel_host_telemetry", truncated: false }, [sample(0), sample(1)]), /lacks opt-in phase events/);
});
