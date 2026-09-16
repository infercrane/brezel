import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const MAX_INPUT_BYTES = 512 * 1024 * 1024;

function parseArgs(argv) {
  const options = { dax: "", telemetry: "", output: "" };
  for (let index = 0; index < argv.length; index += 1) {
    const key = argv[index];
    if (!["--dax", "--telemetry", "--output"].includes(key) || !argv[index + 1]) {
      throw new Error(`invalid argument ${key ?? ""}`);
    }
    options[key.slice(2)] = argv[++index];
  }
  if (!options.dax || !options.telemetry) throw new Error("--dax and --telemetry are required");
  return options;
}

function readBounded(path) {
  const data = readFileSync(path);
  if (data.byteLength > MAX_INPUT_BYTES) throw new Error(`${path} exceeds the diagnostic input limit`);
  return data;
}

function delta(before, after) {
  return Number.isFinite(before) && Number.isFinite(after) && after >= before ? after - before : null;
}

function sum(values) {
  return values.reduce((total, value) => total + (Number.isFinite(value) ? value : 0), 0);
}

function ratio(numerator, denominator) {
  return denominator > 0 ? numerator * 100 / denominator : null;
}

function counterDelta(records, identity, field) {
  const ranges = new Map();
  for (const record of records) {
    for (const item of identity(record)) {
      const key = item.key;
      const value = item.value[field];
      if (!Number.isFinite(value)) continue;
      const range = ranges.get(key) ?? { first: value, last: value };
      range.last = value;
      ranges.set(key, range);
    }
  }
  return sum([...ranges.values()].map(({ first, last }) => Math.max(0, last - first)));
}

function blockItems(sample, prefix) {
  return (sample.block_devices ?? [])
    .filter((device) => prefix === "nbd" ? device.name.startsWith("nbd") : !device.name.startsWith("nbd"))
    .map((device) => ({ key: device.name, value: device }));
}

function firecrackerItems(sample) {
  return (sample.processes ?? [])
    .filter((process) => process.name === "firecracker")
    .map((process) => ({ key: `${process.pid}:${process.start_ticks}`, value: process }));
}

function taskItems(sample) {
  return firecrackerItems(sample).flatMap(({ key: processKey, value: process }) =>
    (process.task_samples ?? []).map((task) => ({ key: `${processKey}:${task.tid}:${task.start_ticks}`, value: task })));
}

function vcpuTaskItems(sample) {
  return taskItems(sample).filter(({ value: task }) => task.name.startsWith("fc_vcpu "));
}

function pressureDelta(first, last, resource, level) {
  return delta(first?.pressure?.[resource]?.[level]?.total_us, last?.pressure?.[resource]?.[level]?.total_us);
}

function analyzeWindow(samples, startAt, endAt, intervalNS, guestCPUs) {
  const start = Date.parse(startAt);
  const end = Date.parse(endAt);
  if (!Number.isFinite(start) || !Number.isFinite(end) || end <= start) throw new Error("phase timestamps are invalid");
  const before = [...samples].reverse().find((sample) => Date.parse(sample.sampled_at) <= start) ?? samples[0];
  const after = samples.find((sample) => Date.parse(sample.sampled_at) >= end) ?? samples.at(-1);
  const selected = samples.filter((sample) => {
    const timestamp = Date.parse(sample.sampled_at);
    return timestamp > start && timestamp <= end;
  });
  const records = [before, ...selected, after].filter((value, index, values) => value && values.indexOf(value) === index);
  const totalTicks = sum(selected.map((sample) => sample.cpu?.delta_total_ticks));
  const busyTicks = sum(selected.map((sample) => sample.cpu?.delta_busy_ticks));
  const ioWaitTicks = sum(selected.map((sample) => sample.cpu?.delta_iowait_ticks));
  const stealTicks = sum(selected.map((sample) => sample.cpu?.delta_steal_ticks));
  const logicalCPUs = selected.find((sample) => Number.isFinite(sample.cpu?.logical_cpus))?.cpu.logical_cpus ?? null;
  const vcpuTicks = counterDelta(records, vcpuTaskItems, "user_ticks_total") + counterDelta(records, vcpuTaskItems, "system_ticks_total");
  const averageVCPUCores = totalTicks > 0 && logicalCPUs ? vcpuTicks * logicalCPUs / totalTicks : null;
  const taskNames = new Map();
  for (const record of records) {
    for (const { value: task } of taskItems(record)) {
      const entry = taskNames.get(task.name) ?? { processors: new Set(), cpuSets: new Set(), memoryNodes: new Set() };
      entry.processors.add(task.last_processor);
      if (task.cpus_allowed_list) entry.cpuSets.add(task.cpus_allowed_list);
      if (task.mems_allowed_list) entry.memoryNodes.add(task.mems_allowed_list);
      taskNames.set(task.name, entry);
    }
  }
  const placement = Object.fromEntries([...taskNames.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([name, entry]) => [name, {
    observedProcessors: [...entry.processors].sort((a, b) => a - b),
    allowedCPUs: [...entry.cpuSets].sort(),
    allowedMemoryNodes: [...entry.memoryNodes].sort(),
  }]));
  return {
    observedWindowMs: end - start,
    sampleCount: selected.length,
    underSampled: end - start < intervalNS / 1e6 * 2 || selected.length < 2,
    hostCPU: {
      busyPercent: ratio(busyTicks, totalTicks),
      ioWaitPercent: ratio(ioWaitTicks, totalTicks),
      stealPercent: ratio(stealTicks, totalTicks),
      peakRunnable: selected.length ? Math.max(...selected.map((sample) => sample.cpu?.processes_running ?? 0)) : null,
      peakBlocked: selected.length ? Math.max(...selected.map((sample) => sample.cpu?.processes_blocked ?? 0)) : null,
    },
    pressureUS: {
      cpuSome: pressureDelta(before, after, "cpu", "some"),
      cpuFull: pressureDelta(before, after, "cpu", "full"),
      ioSome: pressureDelta(before, after, "io", "some"),
      ioFull: pressureDelta(before, after, "io", "full"),
      memorySome: pressureDelta(before, after, "memory", "some"),
      memoryFull: pressureDelta(before, after, "memory", "full"),
    },
    memory: {
      minimumAvailableKiB: selected.length ? Math.min(...selected.map((sample) => sample.memory?.mem_available_kb ?? Number.MAX_SAFE_INTEGER)) : null,
      pageFaults: delta(before?.vm?.page_faults_total, after?.vm?.page_faults_total),
      majorPageFaults: delta(before?.vm?.major_page_faults_total, after?.vm?.major_page_faults_total),
      swapInPages: delta(before?.vm?.swap_in_pages_total, after?.vm?.swap_in_pages_total),
      swapOutPages: delta(before?.vm?.swap_out_pages_total, after?.vm?.swap_out_pages_total),
      allocStalls: delta(before?.vm?.alloc_stalls_total, after?.vm?.alloc_stalls_total),
    },
    firecracker: {
      cpuTicks: counterDelta(records, firecrackerItems, "user_ticks_total") + counterDelta(records, firecrackerItems, "system_ticks_total"),
      minorFaults: counterDelta(records, firecrackerItems, "minor_faults_total"),
      majorFaults: counterDelta(records, firecrackerItems, "major_faults_total"),
      readBytes: counterDelta(records, firecrackerItems, "read_bytes_total"),
      writeBytes: counterDelta(records, firecrackerItems, "write_bytes_total"),
      taskCPUTicks: counterDelta(records, taskItems, "user_ticks_total") + counterDelta(records, taskItems, "system_ticks_total"),
      vcpuTicks,
      averageVCPUCores,
      vCPUCapacityPercent: averageVCPUCores !== null && guestCPUs > 0 ? averageVCPUCores * 100 / guestCPUs : null,
      voluntaryContextSwitches: counterDelta(records, taskItems, "voluntary_context_switches_total"),
      involuntaryContextSwitches: counterDelta(records, taskItems, "involuntary_context_switches_total"),
      placement,
    },
    block: {
      nbdReadBytes: counterDelta(records, (sample) => blockItems(sample, "nbd"), "sectors_read_total") * 512,
      nbdWriteBytes: counterDelta(records, (sample) => blockItems(sample, "nbd"), "sectors_written_total") * 512,
      nbdIOTimeMs: counterDelta(records, (sample) => blockItems(sample, "nbd"), "io_time_ms_total"),
      nbdWeightedIOTimeMs: counterDelta(records, (sample) => blockItems(sample, "nbd"), "weighted_io_time_ms_total"),
      physicalReadBytes: counterDelta(records, (sample) => blockItems(sample, "physical"), "sectors_read_total") * 512,
      physicalWriteBytes: counterDelta(records, (sample) => blockItems(sample, "physical"), "sectors_written_total") * 512,
      physicalIOTimeMs: counterDelta(records, (sample) => blockItems(sample, "physical"), "io_time_ms_total"),
    },
    collector: {
      maximumCollectionMs: selected.length ? Math.max(...selected.map((sample) => (sample.collection_ns ?? 0) / 1e6)) : null,
      readErrors: Object.fromEntries(Object.keys(selected[0]?.read_errors ?? {}).map((key) => [key, sum(selected.map((sample) => sample.read_errors?.[key] ?? 0))])),
      capped: selected.some((sample) => sample.block_devices_capped || sample.processes_capped || sample.interfaces_capped || (sample.processes ?? []).some((process) => process.task_samples_capped)),
    },
  };
}

export function buildHostPhaseReport(dax, manifest, samples) {
  if (dax?.suite !== "computesdk-dax-rehearsal") throw new Error("input is not a DAX rehearsal report");
  if (manifest?.kind !== "brezel_host_telemetry" || manifest?.truncated) throw new Error("host telemetry is invalid or truncated");
  if (!Array.isArray(samples) || samples.length < 2) throw new Error("host telemetry needs at least two samples");
  if (Date.parse(manifest.started_at) > Date.parse(dax.startedAt) || Date.parse(manifest.finished_at) < Date.parse(dax.finishedAt)) {
    throw new Error("host telemetry does not enclose the complete DAX run");
  }
  const attempts = dax.attempts.map((attempt) => {
    if (!Array.isArray(attempt.phaseEvents) || attempt.phaseEvents.length === 0) throw new Error("DAX report lacks opt-in phase events");
    let startAt = attempt.buildStartedAt;
    const phases = {};
    for (const event of attempt.phaseEvents) {
      phases[event.phase] = analyzeWindow(samples, startAt, event.observedAt, manifest.interval_ns, dax.guest?.cpus ?? 0);
      phases[event.phase].guestDurationMs = event.guestDurationMs;
      startAt = event.observedAt;
    }
    return { iteration: attempt.iteration, providerOverhead: attempt.providerOverhead, phases };
  });
  return {
    schemaVersion: 1,
    suite: "brezel-dax-host-phase-telemetry",
    diagnosticOnly: true,
    leaderboardComparable: false,
    dax: { sourceRevision: dax.provenance?.sourceRevision, startedAt: dax.startedAt, finishedAt: dax.finishedAt },
    telemetry: { startedAt: manifest.started_at, finishedAt: manifest.finished_at, intervalNS: manifest.interval_ns, samples: manifest.sample_count },
    interpretation: {
      cpuBound: "high Firecracker task CPU ticks with CPU PSI/runnable pressure, low I/O PSI and low NBD weighted time",
      blockBound: "high NBD weighted I/O time, blocked tasks or I/O PSI while Firecracker CPU progress falls",
      providerBound: "large command-minus-guest or stream-tail time without matching host CPU/block pressure",
      placementFault: "vCPU threads share processors, migrate heavily, or have wider CPU/memory-node masks than the declared guest placement",
    },
    attempts,
  };
}

function loadTelemetry(directory) {
  const manifestBytes = readBounded(join(directory, "manifest.json"));
  const samplesBytes = readBounded(join(directory, "samples.ndjson"));
  const checksumsBytes = readBounded(join(directory, "SHA256SUMS"));
  const manifest = JSON.parse(manifestBytes);
  const samplesDigest = createHash("sha256").update(samplesBytes).digest("hex");
  const manifestDigest = createHash("sha256").update(manifestBytes).digest("hex");
  const expectedChecksums = `${samplesDigest}  samples.ndjson\n${manifestDigest}  manifest.json\n`;
  if (checksumsBytes.toString("utf8") !== expectedChecksums || samplesDigest !== manifest.samples_sha256) {
    throw new Error("host telemetry artifact digest mismatch");
  }
  const samples = samplesBytes.toString("utf8").trim().split("\n").filter(Boolean).map((line) => JSON.parse(line));
  for (let index = 0; index < samples.length; index += 1) {
    if (samples[index].sequence !== index || (index > 0 && Date.parse(samples[index].sampled_at) <= Date.parse(samples[index - 1].sampled_at))) {
      throw new Error("host telemetry sample sequence is invalid");
    }
  }
  return { manifest, samples };
}

function main() {
  const options = parseArgs(process.argv.slice(2));
  const dax = JSON.parse(readBounded(resolve(options.dax)));
  const { manifest, samples } = loadTelemetry(resolve(options.telemetry));
  const output = `${JSON.stringify(buildHostPhaseReport(dax, manifest, samples), null, 2)}\n`;
  if (options.output) writeFileSync(resolve(options.output), output, { mode: 0o600, flag: "wx" });
  process.stdout.write(output);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main();
