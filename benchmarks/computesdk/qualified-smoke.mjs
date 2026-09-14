import { createBrezelComputeFromEnv } from "./adapter.mjs";

const required = [
  "BREZEL_API_URL",
  "BREZEL_SERVICE_TOKEN_FILE",
  "BREZEL_PROJECT_ID",
  "BREZEL_ENVIRONMENT_REVISION",
];
const missing = required.filter((name) => !process.env[name]);
if (missing.length > 0) {
  throw new Error(`Missing required environment variables: ${missing.join(", ")}`);
}

const compute = createBrezelComputeFromEnv();
let sandbox;
const startedAt = performance.now();
try {
  sandbox = await compute.sandbox.create();
  const result = await sandbox.runCommand("node -v");
  if (result.exitCode !== 0) {
    throw new Error(`node -v exited with status ${result.exitCode}`);
  }
  process.stdout.write(`${JSON.stringify({
    provider: "brezel",
    sandboxId: sandbox.sandboxId,
    command: "node -v",
    stdout: result.stdout.trim(),
    createAndFirstCommandMs: performance.now() - startedAt,
  })}\n`);
} finally {
  if (sandbox) await sandbox.destroy();
}
