#!/usr/bin/env node

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const PHASES = ["prepare", "cache_clear", "bun_download", "bun_unpack", "clone", "install", "typecheck"];

function finitePositive(value, name) {
  if (!Number.isFinite(value) || value <= 0) throw new Error(`${name} must be positive`);
  return value;
}

function assertReport(report, name) {
  if (report?.suite !== "brezel-local-dax-lab" || report?.leaderboardComparable !== false) {
    throw new Error(`${name} is not a local Brezel DAX report`);
  }
  if (report?.configuration?.probe !== "full") throw new Error(`${name} is not a full-workload report`);
  if (report?.summary?.succeeded !== report?.summary?.requested || report.summary.succeeded < 1) {
    throw new Error(`${name} does not contain only successful attempts`);
  }
}

function assertComparable(lower, upper) {
  assertReport(lower, "lower");
  assertReport(upper, "upper");
  for (const key of ["image", "imageId", "storage", "memoryBytes", "telemetryIntervalMs"]) {
    if (lower.configuration[key] !== upper.configuration[key]) {
      throw new Error(`reports differ in ${key}`);
    }
  }
  if (lower.configuration.cpus >= upper.configuration.cpus) {
    throw new Error("lower report must expose fewer CPUs than upper report");
  }
}

// Fit t(n) = serialFloor + parallelWork / n from two observations. The model
// is intentionally conservative: noisy negative scaling becomes a constant
// phase instead of manufacturing a future speedup.
export function fitInverseCPUPhase(lowerMs, upperMs, lowerCPUs, upperCPUs) {
  finitePositive(lowerMs, "lower phase time");
  finitePositive(upperMs, "upper phase time");
  finitePositive(lowerCPUs, "lower CPU count");
  finitePositive(upperCPUs, "upper CPU count");
  if (lowerCPUs >= upperCPUs) throw new Error("CPU counts must increase");

  if (upperMs >= lowerMs) {
    return {
      serialFloorMs: upperMs,
      parallelWorkMsCPU: 0,
      observedSpeedup: lowerMs / upperMs,
      fit: "constant-no-speedup",
    };
  }

  let parallelWorkMsCPU = (lowerMs - upperMs) / ((1 / lowerCPUs) - (1 / upperCPUs));
  let serialFloorMs = upperMs - (parallelWorkMsCPU / upperCPUs);
  let fit = "two-point-inverse-cpu";
  if (serialFloorMs < 0) {
    // Super-linear observations cannot be extrapolated by Amdahl's model
    // without a physically meaningless negative floor. Anchor the faster
    // observation and assume an optimistic zero floor instead.
    serialFloorMs = 0;
    parallelWorkMsCPU = upperMs * upperCPUs;
    fit = "upper-anchored-zero-floor";
  }
  return {
    serialFloorMs,
    parallelWorkMsCPU,
    observedSpeedup: lowerMs / upperMs,
    fit,
  };
}

function projectPhase(model, cpus) {
  return model.serialFloorMs + (model.parallelWorkMsCPU / cpus);
}

export function modelDAXBottlenecks(lower, upper, options = {}) {
  assertComparable(lower, upper);
  const lowerCPUs = finitePositive(lower.configuration.cpus, "lower CPU count");
  const upperCPUs = finitePositive(upper.configuration.cpus, "upper CPU count");
  const targets = options.targetCPUs ?? [upperCPUs, 16, 32, 44];
  const thresholdsMs = options.thresholdsMs ?? { top3: 43_653, first: 33_973 };
  const phaseModels = {};

  for (const phase of PHASES) {
    const lowerMs = finitePositive(lower.summary.phaseMedianMs[phase], `lower ${phase}`);
    const upperMs = finitePositive(upper.summary.phaseMedianMs[phase], `upper ${phase}`);
    phaseModels[phase] = fitInverseCPUPhase(lowerMs, upperMs, lowerCPUs, upperCPUs);
  }

  const observedUpperTotalMs = finitePositive(upper.summary.phaseMedianMs.total, "upper total");
  const observedUpperPhaseSumMs = PHASES.reduce((sum, phase) => sum + upper.summary.phaseMedianMs[phase], 0);
  const unmodeledEnvelopeMs = Math.max(0, observedUpperTotalMs - observedUpperPhaseSumMs);
  const projections = targets.map((cpus) => {
    finitePositive(cpus, "target CPU count");
    const phasesMs = Object.fromEntries(PHASES.map((phase) => [phase, projectPhase(phaseModels[phase], cpus)]));
    const totalMs = Object.values(phasesMs).reduce((sum, value) => sum + value, unmodeledEnvelopeMs);
    return {
      cpus,
      phasesMs,
      unmodeledEnvelopeMs,
      totalMs,
      gapsMs: Object.fromEntries(Object.entries(thresholdsMs).map(([name, threshold]) => [name, totalMs - threshold])),
    };
  });

  const upperContribution = Object.fromEntries(PHASES.map((phase) => [
    phase,
    upper.summary.phaseMedianMs[phase] / observedUpperTotalMs,
  ]));
  const rankedBottlenecks = [...PHASES]
    .sort((left, right) => upperContribution[right] - upperContribution[left])
    .map((phase) => ({ phase, share: upperContribution[phase], model: phaseModels[phase] }));

  return {
    schemaVersion: 1,
    evidenceClass: "local-two-point-simulation",
    leaderboardComparable: false,
    caveat: "Two-point projections identify experiments; they are not benchmark evidence or a performance claim.",
    observed: { lowerCPUs, upperCPUs, observedUpperTotalMs, observedUpperPhaseSumMs, unmodeledEnvelopeMs },
    thresholdsMs,
    rankedBottlenecks,
    projections,
  };
}

function parseTargets(raw) {
  return raw.split(",").map((value) => Number(value.trim()));
}

function main(argv) {
  if (argv.length < 2) {
    throw new Error("usage: node dax-bottleneck-model.mjs LOWER_REPORT UPPER_REPORT [--target-cpus 8,16,32,44]");
  }
  let targetCPUs;
  for (let index = 2; index < argv.length; index += 1) {
    if (argv[index] !== "--target-cpus" || !argv[index + 1]) throw new Error(`unknown or incomplete option ${argv[index]}`);
    targetCPUs = parseTargets(argv[index + 1]);
    index += 1;
  }
  const lower = JSON.parse(readFileSync(argv[0], "utf8"));
  const upper = JSON.parse(readFileSync(argv[1], "utf8"));
  process.stdout.write(`${JSON.stringify(modelDAXBottlenecks(lower, upper, { targetCPUs }), null, 2)}\n`);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2));
}
