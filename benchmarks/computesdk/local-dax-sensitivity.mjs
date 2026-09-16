#!/usr/bin/env node

import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const PHASES = ["prepare", "bun_download", "bun_unpack", "clone", "install", "typecheck", "total"];

function finitePositive(value, description) {
  if (!Number.isFinite(value) || value <= 0) throw new Error(`${description} must be positive`);
  return value;
}

function assertComparableLocalReports(lower, upper) {
  for (const [name, report] of [["lower", lower], ["upper", upper]]) {
    if (report?.suite !== "brezel-local-dax-lab" || report?.leaderboardComparable !== false) {
      throw new Error(`${name} input is not a Brezel local DAX report`);
    }
    if (report?.summary?.succeeded !== report?.summary?.requested || report.summary.succeeded < 1) {
      throw new Error(`${name} input does not contain only successful attempts`);
    }
  }
  for (const key of ["image", "imageId", "storage", "probe", "memoryBytes", "telemetryIntervalMs"]) {
    if (lower.configuration[key] !== upper.configuration[key]) {
      throw new Error(`local sensitivity inputs differ in ${key}`);
    }
  }
  if (lower.configuration.probe !== "full") throw new Error("local sensitivity requires complete workloads");
  if (lower.configuration.cpus >= upper.configuration.cpus) {
    throw new Error("the lower input must expose fewer CPUs than the upper input");
  }
}

export function analyzeLocalSensitivity(lower, upper) {
  assertComparableLocalReports(lower, upper);
  const lowerCPUs = finitePositive(lower.configuration.cpus, "lower CPU count");
  const upperCPUs = finitePositive(upper.configuration.cpus, "upper CPU count");
  const cpuRatio = upperCPUs / lowerCPUs;
  const phases = {};
  for (const phase of PHASES) {
    const lowerMs = finitePositive(lower.summary.phaseMedianMs[phase], `lower ${phase}`);
    const upperMs = finitePositive(upper.summary.phaseMedianMs[phase], `upper ${phase}`);
    const speedup = lowerMs / upperMs;
    const elasticity = Math.log(speedup) / Math.log(cpuRatio);
    phases[phase] = {
      lowerMs,
      upperMs,
      speedup,
      cpuElasticity: elasticity,
      diagnosis: speedup >= 1.5 ? "cpu-sensitive" : speedup <= 1.15 ? "cpu-insensitive-at-this-range" : "mixed",
    };
  }
  return {
    schemaVersion: 1,
    evidenceClass: "local-relative-only",
    leaderboardComparable: false,
    caveat: "Sensitivity is an A/B diagnostic, not causal proof or leaderboard evidence.",
    configuration: {
      image: lower.configuration.image,
      imageId: lower.configuration.imageId,
      storage: lower.configuration.storage,
      memoryBytes: lower.configuration.memoryBytes,
      telemetryIntervalMs: lower.configuration.telemetryIntervalMs,
      lowerCPUs,
      upperCPUs,
    },
    phases,
  };
}

function main(argv) {
  if (argv.length !== 2) {
    throw new Error("usage: node local-dax-sensitivity.mjs LOWER_CPU_REPORT UPPER_CPU_REPORT");
  }
  const lower = JSON.parse(readFileSync(argv[0], "utf8"));
  const upper = JSON.parse(readFileSync(argv[1], "utf8"));
  process.stdout.write(`${JSON.stringify(analyzeLocalSensitivity(lower, upper), null, 2)}\n`);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main(process.argv.slice(2));
}
