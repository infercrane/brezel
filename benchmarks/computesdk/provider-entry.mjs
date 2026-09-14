import { createBrezelComputeFromEnv } from "./adapter.mjs";

/** ProviderConfig-compatible entry for a local computesdk-benchmarks checkout. */
export const brezelBenchmarkProvider = {
  name: "brezel",
  requiredEnvVars: [
    "BREZEL_API_URL",
    "BREZEL_SERVICE_TOKEN_FILE",
    "BREZEL_PROJECT_ID",
    "BREZEL_ENVIRONMENT_REVISION",
  ],
  createCompute: createBrezelComputeFromEnv,
  destroyTimeoutMs: 120_000,
};
