import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { createBrezelCompute } from "./adapter.mjs";

const token = "test_service_token_0123456789abcdef";

function sandbox(state) {
  return {
    id: "sb_test",
    environment_revision: "envr_test",
    state,
    created_at: "2026-09-14T00:00:00Z",
    updated_at: "2026-09-14T00:00:01Z",
    expires_at: "2026-09-14T01:00:00Z",
    revision: 2,
  };
}

async function fixture(t, options = {}) {
  const directory = mkdtempSync(join(tmpdir(), "brezel-computesdk-"));
  chmodSync(directory, 0o700);
  const tokenFile = join(directory, "token");
  writeFileSync(tokenFile, `${token}\n`, { mode: 0o600 });
  chmodSync(tokenFile, 0o600);
  const requests = [];
  let getCount = 0;
  let deleteRequested = false;
  let deleteGetCount = 0;
  const server = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = Buffer.concat(chunks).toString("utf8");
    requests.push({ method: request.method, url: request.url, headers: request.headers, body });
    response.setHeader("Content-Type", "application/json");

    if (request.method === "POST" && request.url === "/v1/sandboxes") {
      response.statusCode = 202;
      response.end(JSON.stringify({
        resource: sandbox(options.createState ?? "requested"),
        operation: { id: "op_create", state: "running" },
      }));
      return;
    }
    if (request.method === "GET" && request.url === "/v1/sandboxes/sb_test") {
      if (deleteRequested) {
        const states = options.deleteStates ?? ["deleting", "deleted"];
        const state = states[Math.min(deleteGetCount, states.length - 1)];
        deleteGetCount += 1;
        response.end(JSON.stringify(sandbox(state)));
        return;
      }
      getCount += 1;
      response.end(JSON.stringify(sandbox(getCount === 1 ? "preparing" : "running")));
      return;
    }
    if (request.method === "POST" && request.url === "/v1/sandboxes/sb_test/commands") {
      response.setHeader("Content-Type", options.commandContentType ?? "application/x-ndjson");
      if (options.oversizedCommandLine) {
        response.end(`${"x".repeat(2 * 1024 * 1024 + 1)}\n`);
        return;
      }
      if (options.commandError) {
        response.end(`${JSON.stringify({ type: "error", error: { code: "command_failed" } })}\n`);
        return;
      }
      response.write(`${JSON.stringify({ type: "started" })}\n`);
      response.write(`${JSON.stringify({ type: "stdout", data: Buffer.from("v22.0.0\n").toString("base64") })}\n`);
      response.write(`${JSON.stringify({ type: "stderr", data: Buffer.from("warn\n").toString("base64") })}\n`);
      response.end(`${JSON.stringify({ type: "exited", exited: true, exit_code: 0 })}\n`);
      return;
    }
    if (request.method === "DELETE" && request.url === "/v1/sandboxes/sb_test") {
      deleteRequested = true;
      response.statusCode = 202;
      response.end(JSON.stringify({ resource: sandbox("deleting"), operation: { id: "op_delete", state: "running" } }));
      return;
    }
    if (request.method === "GET" && request.url === "/v1/sandboxes") {
      response.end(JSON.stringify({ sandboxes: [sandbox("running")] }));
      return;
    }
    response.statusCode = 404;
    response.end(JSON.stringify({ error: { code: "not_found", message: "not found" } }));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(async () => {
    await new Promise((resolve) => server.close(resolve));
    rmSync(directory, { recursive: true, force: true });
  });
  const address = server.address();
  return { baseUrl: `http://127.0.0.1:${address.port}`, tokenFile, requests };
}

test("implements the benchmark create, runCommand, and destroy lifecycle", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t);
  const compute = createBrezelCompute({
    baseUrl,
    tokenFile,
    projectId: "project-test",
    environmentRevision: "envr_test",
    createTimeoutMs: 2_000,
    destroyTimeoutMs: 2_000,
  });

  const instance = await compute.sandbox.create();
  assert.equal(instance.sandboxId, "sb_test");
  assert.equal(instance.provider, "brezel");
  assert.equal(
    requests.filter((request) => request.method === "GET" && request.url === "/v1/sandboxes/sb_test").length,
    2,
    "transitional create response was polled through preparing to running",
  );
  const streamedOut = [];
  const streamedErr = [];
  const result = await instance.runCommand("node -v", {
    onStdout: (chunk) => streamedOut.push(chunk),
    onStderr: (chunk) => streamedErr.push(chunk),
  });
  assert.equal(result.exitCode, 0);
  assert.equal(result.stdout, "v22.0.0\n");
  assert.equal(result.stderr, "warn\n");
  assert.equal(streamedOut.join(""), result.stdout);
  assert.equal(streamedErr.join(""), result.stderr);
  await instance.destroy();

  const createRequest = requests.find((request) => request.method === "POST" && request.url === "/v1/sandboxes");
  assert.equal(createRequest.headers.authorization, `Bearer ${token}`);
  assert.equal(createRequest.headers["x-project-id"], "project-test");
  assert.ok(createRequest.headers["idempotency-key"]);
  assert.deepEqual(JSON.parse(createRequest.body), {
    environment_revision: "envr_test",
    lifecycle: { expires_after_seconds: 3600 },
    network: { allow_internet: false },
  });
  const commandRequest = requests.find((request) => request.url?.endsWith("/commands"));
  assert.deepEqual(JSON.parse(commandRequest.body).argv, ["/bin/sh", "-lc", "node -v"]);
  for (const request of requests) assert.equal(request.body.includes(token), false);
});

test("does not poll an immediate-running create response", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t, { createState: "running" });
  const compute = createBrezelCompute({
    baseUrl,
    tokenFile,
    projectId: "project-test",
    environmentRevision: "envr_test",
  });

  const instance = await compute.sandbox.create();
  assert.equal(instance.sandboxId, "sb_test");
  assert.equal(
    requests.filter((request) => request.method === "GET" && request.url === "/v1/sandboxes/sb_test").length,
    0,
  );
  await instance.destroy();
});

test("rejects permissive token files before making a network request", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t);
  chmodSync(tokenFile, 0o644);
  assert.throws(
    () => createBrezelCompute({ baseUrl, tokenFile, projectId: "p", environmentRevision: "envr_test" }),
    /permissions must be 0600/,
  );
  assert.equal(requests.length, 0);
});

test("rejects non-loopback plaintext API URLs", async (t) => {
  const { tokenFile } = await fixture(t);
  assert.throws(
    () => createBrezelCompute({
      baseUrl: "http://192.0.2.1:8080",
      tokenFile,
      projectId: "p",
      environmentRevision: "envr_test",
    }),
    /must use HTTPS/,
  );
});

test("does not silently put provider credentials in sandbox environment", async (t) => {
  const { baseUrl, tokenFile } = await fixture(t);
  const compute = createBrezelCompute({ baseUrl, tokenFile, projectId: "p", environmentRevision: "envr_test" });
  await assert.rejects(
    compute.sandbox.create({ envs: { BREZEL_SERVICE_TOKEN: token } }),
    /create-time environment variables are not exposed/,
  );
});

test("rejects an indeterminate command stream instead of inventing an exit code", async (t) => {
  const { baseUrl, tokenFile } = await fixture(t, { commandError: true });
  const compute = createBrezelCompute({ baseUrl, tokenFile, projectId: "p", environmentRevision: "envr_test" });
  const instance = await compute.sandbox.create();
  try {
    await assert.rejects(instance.runCommand("true"), /outcome is unknown/);
  } finally {
    await instance.destroy();
  }
});

test("rejects a command stream with the wrong media type", async (t) => {
  const { baseUrl, tokenFile } = await fixture(t, { commandContentType: "application/json" });
  const compute = createBrezelCompute({ baseUrl, tokenFile, projectId: "p", environmentRevision: "envr_test" });
  const instance = await compute.sandbox.create();
  try {
    await assert.rejects(instance.runCommand("true"), /must use application\/x-ndjson/);
  } finally {
    await instance.destroy();
  }
});

test("rejects an oversized command event before parsing it", async (t) => {
  const { baseUrl, tokenFile } = await fixture(t, { oversizedCommandLine: true });
  const compute = createBrezelCompute({ baseUrl, tokenFile, projectId: "p", environmentRevision: "envr_test" });
  const instance = await compute.sandbox.create();
  try {
    await assert.rejects(instance.runCommand("true"), /line exceeded the adapter limit/);
  } finally {
    await instance.destroy();
  }
});

test("waits through expired until deletion is confirmed", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t, { deleteStates: ["expired", "deleting", "deleted"] });
  const compute = createBrezelCompute({ baseUrl, tokenFile, projectId: "p", environmentRevision: "envr_test" });
  const instance = await compute.sandbox.create();
  await instance.destroy();
  const deleteIndex = requests.findIndex((request) => request.method === "DELETE" && request.url === "/v1/sandboxes/sb_test");
  const cleanupReads = requests.slice(deleteIndex + 1).filter(
    (request) => request.method === "GET" && request.url === "/v1/sandboxes/sb_test",
  );
  assert.equal(cleanupReads.length, 3, "destroy waited through expired and deleting before accepting deleted");
});

test("rejects cleanup when an expired sandbox is never confirmed deleted", async (t) => {
  const { baseUrl, tokenFile } = await fixture(t, { deleteStates: ["expired"] });
  const compute = createBrezelCompute({
    baseUrl,
    tokenFile,
    projectId: "p",
    environmentRevision: "envr_test",
    destroyTimeoutMs: 150,
  });
  const instance = await compute.sandbox.create();
  await assert.rejects(instance.destroy(), /timed out|aborted/);
});
