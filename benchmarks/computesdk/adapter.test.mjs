import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { createBrezelCompute, createBrezelComputeFromEnv } from "./adapter.mjs";

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
  const files = new Map();
  let getCount = 0;
  let deleteRequested = false;
  let deleteGetCount = 0;
  let commandCount = 0;
  let createPostCount = 0;
  const server = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = Buffer.concat(chunks).toString("utf8");
    requests.push({ method: request.method, url: request.url, headers: request.headers, body });
    response.setHeader("Content-Type", "application/json");
    const parsedUrl = new URL(request.url, "http://127.0.0.1");

    if (request.method === "GET" && request.url === "/v1/environments/envr_test") {
      response.end(JSON.stringify(options.environmentResponse ?? {
        revision_id: "envr_test",
        name: "dax",
        template: "dax-c4:018f47a2-4f5c-7d8e-9a0b-123456789abc",
        created_at: "2026-09-14T00:00:00Z",
      }));
      return;
    }
    if (request.method === "POST" && request.url === "/v1/sandboxes") {
      createPostCount += 1;
      if (options.createErrorStatus) {
        response.statusCode = options.createErrorStatus;
        response.end(JSON.stringify({ error: { code: "invalid_environment", message: "environment is unavailable" } }));
        return;
      }
      if (options.dropFirstCreateResponse && createPostCount === 1) {
        response.destroy();
        return;
      }
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
      const commandResult = options.commandResults?.[commandCount] ?? {
        stdout: "v22.0.0\n",
        stderr: "warn\n",
        exitCode: 0,
      };
      commandCount += 1;
      response.write(`${JSON.stringify({ type: "started" })}\n`);
      response.write(`${JSON.stringify({ type: "stdout", data: Buffer.from(commandResult.stdout ?? "").toString("base64") })}\n`);
      response.write(`${JSON.stringify({ type: "stderr", data: Buffer.from(commandResult.stderr ?? "").toString("base64") })}\n`);
      response.end(`${JSON.stringify({ type: "exited", exited: true, exit_code: commandResult.exitCode ?? 0 })}\n`);
      return;
    }
    if (request.method === "PUT" && parsedUrl.pathname === "/v1/sandboxes/sb_test/files") {
      const path = parsedUrl.searchParams.get("path");
      files.set(path, body);
      response.end(JSON.stringify({ path, size: Buffer.byteLength(body), content_type: "application/octet-stream" }));
      return;
    }
    if (request.method === "GET" && parsedUrl.pathname === "/v1/sandboxes/sb_test/files") {
      const path = parsedUrl.searchParams.get("path");
      if (!files.has(path)) {
        response.statusCode = 404;
        response.end(JSON.stringify({ error: { code: "not_found", message: "not found" } }));
        return;
      }
      response.setHeader("Content-Type", "application/octet-stream");
      response.end(files.get(path));
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
  assert.deepEqual(JSON.parse(commandRequest.body).argv, ["/bin/sh", "-c", "node -v"]);
  for (const request of requests) assert.equal(request.body.includes(token), false);
});

test("reads the stored environment without allocating a sandbox", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t);
  const compute = createBrezelCompute({
    baseUrl,
    tokenFile,
    projectId: "project-test",
    environmentRevision: "envr_test",
  });

  const environment = await compute.environment.getByRevision();
  assert.equal(environment.template, "dax-c4:018f47a2-4f5c-7d8e-9a0b-123456789abc");
  assert.equal(requests.length, 1);
  assert.equal(requests[0].method, "GET");
  assert.equal(requests[0].url, "/v1/environments/envr_test");
  assert.equal(requests[0].headers.authorization, `Bearer ${token}`);
  assert.equal(requests[0].headers["x-project-id"], "project-test");
});

test("rejects an environment response for another revision", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t, {
    environmentResponse: {
      revision_id: "envr_other",
      name: "dax",
      template: "dax-c4:018f47a2-4f5c-7d8e-9a0b-123456789abc",
      created_at: "2026-09-14T00:00:00Z",
    },
  });
  const compute = createBrezelCompute({
    baseUrl,
    tokenFile,
    projectId: "project-test",
    environmentRevision: "envr_test",
  });

  await assert.rejects(compute.environment.getByRevision(), /different environment revision/);
  assert.equal(requests.some((request) => request.method === "POST" && request.url === "/v1/sandboxes"), false);
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

test("reconciles and deletes an idempotent create when the first response is lost", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t, {
    createState: "running",
    dropFirstCreateResponse: true,
  });
  const compute = createBrezelCompute({
    baseUrl,
    tokenFile,
    projectId: "project-test",
    environmentRevision: "envr_test",
    createTimeoutMs: 2_000,
    destroyTimeoutMs: 2_000,
  });

  await assert.rejects(compute.sandbox.create(), /fetch failed|socket|other side closed/i);

  const creates = requests.filter((request) => request.method === "POST" && request.url === "/v1/sandboxes");
  assert.equal(creates.length, 2, "the unknown create was reconciled exactly once");
  assert.ok(creates[0].headers["idempotency-key"]);
  assert.equal(
    creates[1].headers["idempotency-key"],
    creates[0].headers["idempotency-key"],
    "reconciliation reused the original idempotency key",
  );
  assert.equal(
    requests.filter((request) => request.method === "DELETE" && request.url === "/v1/sandboxes/sb_test").length,
    1,
    "the reconciled sandbox was deleted",
  );
});

test("returns a definitive create client error without reconciliation", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t, { createErrorStatus: 404 });
  const compute = createBrezelCompute({
    baseUrl,
    tokenFile,
    projectId: "project-test",
    environmentRevision: "envr_missing",
    createTimeoutMs: 2_000,
  });

  await assert.rejects(
    compute.sandbox.create(),
    /Brezel API request failed \(invalid_environment\): environment is unavailable/,
  );
  assert.equal(
    requests.filter((request) => request.method === "POST" && request.url === "/v1/sandboxes").length,
    1,
    "a definitive client error must not be retried as a lost response",
  );
});

test("enables sandbox internet only from an explicit true environment value", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t);
  const compute = createBrezelComputeFromEnv({
    BREZEL_API_URL: baseUrl,
    BREZEL_SERVICE_TOKEN_FILE: tokenFile,
    BREZEL_PROJECT_ID: "project-test",
    BREZEL_ENVIRONMENT_REVISION: "envr_test",
    BREZEL_ALLOW_INTERNET: "true",
  });

  const instance = await compute.sandbox.create();
  await instance.destroy();

  const createRequest = requests.find((request) => request.method === "POST" && request.url === "/v1/sandboxes");
  assert.deepEqual(JSON.parse(createRequest.body).network, { allow_internet: true });
});

test("rejects an invalid internet environment value before making a request", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t);
  for (const value of ["", "TRUE", " true ", "1"]) {
    assert.throws(
      () => createBrezelComputeFromEnv({
        BREZEL_API_URL: baseUrl,
        BREZEL_SERVICE_TOKEN_FILE: tokenFile,
        BREZEL_PROJECT_ID: "project-test",
        BREZEL_ENVIRONMENT_REVISION: "envr_test",
        BREZEL_ALLOW_INTERNET: value,
      }),
      /BREZEL_ALLOW_INTERNET must be exactly "true" or "false" when set/,
    );
  }
  assert.equal(requests.length, 0);
});

test("rejects a non-boolean programmatic internet setting", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t);
  for (const allowInternet of ["true", null, 1]) {
    assert.throws(
      () => createBrezelCompute({
        baseUrl,
        tokenFile,
        projectId: "project-test",
        environmentRevision: "envr_test",
        allowInternet,
      }),
      /allowInternet must be a boolean/,
    );
  }
  assert.equal(requests.length, 0);
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

test("maps ComputeSDK file reads and writes to Brezel file APIs", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t, { createState: "running" });
  const compute = createBrezelCompute({ baseUrl, tokenFile, projectId: "p", environmentRevision: "envr_test" });
  const instance = await compute.sandbox.create();
  const path = "/workspace/file with spaces and 'quotes'.txt";
  const content = "hello from Brezel π";

  await instance.filesystem.writeFile(path, content);
  assert.equal(await instance.filesystem.readFile(path), content);

  const fileRequests = requests.filter((request) => new URL(request.url, baseUrl).pathname.endsWith("/files"));
  assert.equal(fileRequests.length, 2);
  assert.equal(new URL(fileRequests[0].url, baseUrl).searchParams.get("path"), path);
  assert.equal(fileRequests[0].headers["content-type"], "application/octet-stream");
  assert.equal(fileRequests[0].body, content);
  assert.equal(new URL(fileRequests[1].url, baseUrl).searchParams.get("path"), path);
  await instance.destroy();
});

test("implements directory, existence, and removal operations without interpolating paths into commands", async (t) => {
  const modifiedMs = Date.parse("2026-09-14T12:34:56.000Z");
  const { baseUrl, tokenFile, requests } = await fixture(t, {
    createState: "running",
    commandResults: [
      { exitCode: 0 },
      {
        stdout: JSON.stringify([
          { name: "folder", type: "directory", size: 4096, modifiedMs },
          { name: "file.txt", type: "file", size: 7, modifiedMs },
        ]),
        exitCode: 0,
      },
      { exitCode: 1 },
      { exitCode: 0 },
      { exitCode: 0 },
    ],
  });
  const compute = createBrezelCompute({ baseUrl, tokenFile, projectId: "p", environmentRevision: "envr_test" });
  const instance = await compute.sandbox.create();
  const path = "/workspace/a directory with '$HOME'";

  await instance.filesystem.mkdir(path);
  assert.deepEqual(await instance.filesystem.readdir(path), [
    { name: "folder", type: "directory", size: 4096, modified: new Date(modifiedMs) },
    { name: "file.txt", type: "file", size: 7, modified: new Date(modifiedMs) },
  ]);
  assert.equal(await instance.filesystem.exists(path), false);
  assert.equal(await instance.filesystem.exists(path), true);
  await instance.filesystem.remove(path);

  const commandRequests = requests.filter((request) => request.url?.endsWith("/commands"));
  assert.equal(commandRequests.length, 5);
  for (const request of commandRequests) {
    const body = JSON.parse(request.body);
    assert.equal(body.env.BREZEL_COMPUTESDK_FS_PATH, path);
    assert.equal(body.argv.join(" ").includes(path), false, "guest path was not interpolated into a shell command");
  }
  await instance.destroy();
});

test("rejects invalid filesystem inputs before making file or command requests", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t, { createState: "running" });
  const compute = createBrezelCompute({ baseUrl, tokenFile, projectId: "p", environmentRevision: "envr_test" });
  const instance = await compute.sandbox.create();
  const requestCount = requests.length;

  for (const path of ["relative", "/workspace/../secret", "/workspace/trailing/", "/workspace/new\nline"]) {
    await assert.rejects(instance.filesystem.readFile(path), /filesystem path/);
  }
  await assert.rejects(instance.filesystem.writeFile("/workspace/file", Buffer.from("binary")), /must be a string/);
  assert.equal(requests.length, requestCount);
  await instance.destroy();
});

test("fails closed on unsupported per-sandbox resource overrides", async (t) => {
  const { baseUrl, tokenFile, requests } = await fixture(t);
  const compute = createBrezelCompute({ baseUrl, tokenFile, projectId: "p", environmentRevision: "envr_test" });

  for (const [name, value] of [["vcpus", 8], ["memMiB", 16_384], ["diskMiB", 32_768], ["vcpus", null]]) {
    await assert.rejects(
      compute.sandbox.create({ [name]: value }),
      new RegExp(`does not support per-sandbox resource overrides \\(${name}\\).+templateId`),
    );
  }
  await assert.rejects(
    compute.sandbox.create({ vcpus: 8, memMiB: 16_384, diskMiB: 32_768 }),
    /resource overrides \(vcpus, memMiB, diskMiB\)/,
  );
  assert.equal(requests.length, 0);
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
