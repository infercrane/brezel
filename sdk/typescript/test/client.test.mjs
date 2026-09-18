import assert from "node:assert/strict";
import { mkdtempSync, chmodSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, before, beforeEach, test } from "node:test";

import { BrezelClient, BrezelError, Sandbox } from "../src/index.js";

const requests = [];
let server;
let baseUrl;

before(async () => {
  server = createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    requests.push({ method: request.method, url: request.url, headers: request.headers, body: Buffer.concat(chunks) });
    const send = (status, value, contentType = "application/json") => {
      const body = contentType === "application/json" ? Buffer.from(JSON.stringify(value)) : Buffer.from(value);
      response.writeHead(status, { "content-type": contentType, "content-length": body.length });
      response.end(body);
    };
    if (request.method === "POST" && request.url === "/v1/environments") {
      send(202, { resource: { revision_id: "envr_test" }, operation: { state: "succeeded" } });
    } else if (request.method === "POST" && request.url === "/v1/sandboxes") {
      send(202, { resource: { id: "sbx_test", state: "running" }, operation: { state: "succeeded" } });
    } else if (request.method === "POST" && request.url === "/v1/sandboxes/sbx_test/commands") {
      const events = [
        { execution_id: "exec_test", type: "stdout", data: Buffer.from("42\n").toString("base64") },
        { execution_id: "exec_test", type: "stderr", data: Buffer.from("note\n").toString("base64") },
        { execution_id: "exec_test", type: "exited", exited: true, exit_code: 0 },
      ];
      send(200, events.map((event) => JSON.stringify(event)).join("\n") + "\n", "application/x-ndjson");
    } else if (request.method === "PUT" && request.url.startsWith("/v1/sandboxes/sbx_test/files?")) {
      send(200, { path: "/workspace/value.txt", size: chunks.reduce((sum, chunk) => sum + chunk.length, 0) });
    } else if (request.method === "GET" && request.url.startsWith("/v1/sandboxes/sbx_test/files?")) {
      send(200, "stored", "application/octet-stream");
    } else if (request.method === "POST" && request.url === "/v1/sandboxes/sbx_test/ports/3000/leases") {
      send(201, { path: "/p/opaque/", expires_at: "2026-09-17T00:00:00Z" });
    } else if (request.method === "POST" && request.url.endsWith(":pause")) {
      send(202, { resource: { id: "sbx_test", state: "standby" }, operation: { state: "succeeded" } });
    } else if (request.method === "POST" && request.url.endsWith(":resume")) {
      send(202, { resource: { id: "sbx_test", state: "running" }, operation: { state: "succeeded" } });
    } else if (request.method === "DELETE" && request.url === "/v1/sandboxes/sbx_test") {
      send(202, { resource: { id: "sbx_test", state: "deleted" }, operation: { state: "succeeded" } });
    } else {
      send(404, { error: { code: "not_found", message: "resource not found" } });
    }
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  baseUrl = `http://127.0.0.1:${server.address().port}`;
});

after(async () => {
  await new Promise((resolve, reject) => server.close((error) => (error ? reject(error) : resolve())));
});

beforeEach(() => requests.splice(0));

test("supports create, run, files, preview, standby, resume, and cleanup", async () => {
  const client = new BrezelClient({ token: "a-service-token-that-is-long-enough", baseUrl, project: "project-a" });
  const sandbox = await client.createSandbox({ ttlSeconds: 900 });
  const result = await sandbox.run(["python3", "-c", "print(6 * 7)"], { check: true });
  assert.equal(result.stdoutText, "42\n");
  assert.equal(result.stderrText, "note\n");
  assert.equal((await sandbox.writeFile("/workspace/value.txt", "stored")).size, 6);
  assert.equal(Buffer.from(await sandbox.readFile("/workspace/value.txt")).toString(), "stored");
  assert.equal(await sandbox.preview(3000), `${baseUrl}/p/opaque/`);
  assert.equal((await sandbox.pause()).state, "standby");
  assert.equal((await sandbox.resume()).state, "running");
  assert.equal((await sandbox.delete()).state, "deleted");
  assert.ok(requests.every((request) => request.headers.authorization === "Bearer a-service-token-that-is-long-enough"));
  assert.ok(requests.every((request) => request.headers["x-project-id"] === "project-a"));
});

test("creates from an immutable environment revision without mutating environments", async () => {
  const client = new BrezelClient({ token: "a-service-token-that-is-long-enough", baseUrl, project: "project-a" });
  const sandbox = await client.createSandbox({ environmentRevision: "envr_exact" });
  try {
    const environmentRequests = requests.filter((request) => request.url === "/v1/environments");
    assert.equal(environmentRequests.length, 0);
    const create = requests.find((request) => request.method === "POST" && request.url === "/v1/sandboxes");
    assert.equal(JSON.parse(create.body.toString("utf8")).environment_revision, "envr_exact");
    await assert.rejects(
      () => client.createSandbox({ template: "base", environmentRevision: "envr_exact" }),
      /mutually exclusive/,
    );
  } finally {
    await sandbox.delete();
  }
});

test("reads only private token files and rejects remote plaintext", () => {
  const directory = mkdtempSync(join(tmpdir(), "brezel-sdk-"));
  try {
    const tokenFile = join(directory, "token");
    writeFileSync(tokenFile, "a-service-token-that-is-long-enough\n", { mode: 0o600 });
    const client = BrezelClient.fromTokenFile(tokenFile, { baseUrl });
    assert.equal(client.project, "brezel-default");
    chmodSync(tokenFile, 0o644);
    assert.throws(() => BrezelClient.fromTokenFile(tokenFile, { baseUrl }), /permissions/);
    assert.throws(
      () => new BrezelClient({ token: "a-service-token-that-is-long-enough", baseUrl: "http://example.com" }),
      /HTTPS/,
    );
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("fails closed when a command stream has no terminal event", async () => {
  const fetch = async () => new Response(
    JSON.stringify({ type: "stdout", data: Buffer.from("partial").toString("base64") }) + "\n",
    { status: 200, headers: { "content-type": "application/x-ndjson" } },
  );
  const client = new BrezelClient({ token: "a-service-token-that-is-long-enough", baseUrl, fetch });
  const sandbox = new Sandbox(client, "sbx_test");
  await assert.rejects(() => sandbox.run(["true"]), (error) => error instanceof BrezelError && error.code === "command_indeterminate");
});
