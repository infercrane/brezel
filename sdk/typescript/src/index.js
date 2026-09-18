import { createHash, randomBytes } from "node:crypto";
import { closeSync, constants, fstatSync, lstatSync, openSync, readSync } from "node:fs";

const MAX_JSON_BYTES = 4 << 20;
const MAX_EVENT_BYTES = 2 << 20;
const MAX_OUTPUT_BYTES = 64 << 20;

export const VERSION = "0.1.0";

export class BrezelError extends Error {
  constructor(message, { code = "", status = 0, cause } = {}) {
    super(message, cause === undefined ? undefined : { cause });
    this.name = "BrezelError";
    this.code = code;
    this.status = status;
  }
}

export class CommandExitError extends BrezelError {
  constructor(result) {
    super(`command exited with status ${result.exitCode}`, { code: "command_exit" });
    this.name = "CommandExitError";
    this.result = result;
  }
}

export class BrezelClient {
  constructor({
    token,
    baseUrl = "http://127.0.0.1:8080",
    project = "brezel-default",
    timeoutMs = 45_000,
    fetch: fetchImplementation = globalThis.fetch,
  }) {
    if (typeof fetchImplementation !== "function") {
      throw new TypeError("a fetch implementation is required");
    }
    this.baseUrl = validateBaseUrl(baseUrl);
    this.token = validateToken(token);
    this.project = validateProject(project);
    if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
      throw new TypeError("timeoutMs must be positive");
    }
    this.timeoutMs = timeoutMs;
    this.fetch = fetchImplementation;
  }

  static fromTokenFile(tokenFile, options = {}) {
    return new BrezelClient({ ...options, token: readPrivateToken(tokenFile) });
  }

  async createSandbox({
    template = "base",
    ttlSeconds = 3600,
    standbyAfterSeconds = 0,
    allowInternet = false,
    workspaceMounts = [],
  } = {}) {
    const revision = await this.#ensureEnvironment(template);
    const lifecycle = { expires_after_seconds: ttlSeconds };
    if (standbyAfterSeconds > 0) {
      Object.assign(lifecycle, {
        standby_after_seconds: standbyAfterSeconds,
        standby_checkpoint_kind: "full_state",
        auto_resume: true,
      });
    }
    const body = {
      environment_revision: revision,
      lifecycle,
      network: { allow_internet: allowInternet },
    };
    if (workspaceMounts.length > 0) {
      body.workspace_mounts = workspaceMounts;
    }
    const payload = await this.requestJSON("POST", "/v1/sandboxes", {
      body,
      idempotencyKey: randomKey("sandbox"),
    });
    const resource = requiredObject(payload, "resource");
    return new Sandbox(this, requiredString(resource, "id"), resource);
  }

  async sandbox(id) {
    const resource = await this.requestJSON("GET", `/v1/sandboxes/${segment(id)}`);
    if (!isObject(resource)) {
      throw new BrezelError("Brezel returned an invalid sandbox response", { code: "invalid_response" });
    }
    return new Sandbox(this, id, resource);
  }

  async listSandboxes({ includeTerminal = false } = {}) {
    const suffix = includeTerminal ? "?include_terminal=true" : "";
    const payload = await this.requestJSON("GET", `/v1/sandboxes${suffix}`);
    if (!isObject(payload) || !Array.isArray(payload.sandboxes) || !payload.sandboxes.every(isObject)) {
      throw new BrezelError("Brezel returned an invalid sandbox list", { code: "invalid_response" });
    }
    return payload.sandboxes;
  }

  async requestJSON(method, path, { body, idempotencyKey = "", timeoutMs = this.timeoutMs } = {}) {
    const encoded = body === undefined ? undefined : JSON.stringify(body);
    const response = await this.request(method, path, {
      body: encoded,
      contentType: encoded === undefined ? "" : "application/json",
      idempotencyKey,
      timeoutMs,
    });
    return decodeJSONResponse(response, MAX_JSON_BYTES, "Brezel JSON response");
  }

  async request(
    method,
    path,
    { body, contentType = "", idempotencyKey = "", timeoutMs = this.timeoutMs } = {},
  ) {
    const url = new URL(path.replace(/^\/+/, ""), `${this.baseUrl}/`);
    const headers = {
      Authorization: `Bearer ${this.token}`,
      "X-Project-ID": this.project,
      Accept: "application/json",
    };
    if (contentType) headers["Content-Type"] = contentType;
    if (idempotencyKey) headers["Idempotency-Key"] = idempotencyKey;
    let response;
    try {
      response = await this.fetch(url, {
        method,
        headers,
        body,
        redirect: "manual",
        signal: AbortSignal.timeout(timeoutMs),
      });
    } catch (error) {
      throw new BrezelError(`Brezel request failed: ${error.message}`, { code: "transport_error", cause: error });
    }
    if (!response.ok) {
      let code = "http_error";
      let message = `Brezel API returned HTTP ${response.status}`;
      try {
        const payload = await decodeJSONResponse(response, 1 << 20, "Brezel error response");
        if (isObject(payload?.error)) {
          code = typeof payload.error.code === "string" ? payload.error.code : code;
          message = typeof payload.error.message === "string" ? payload.error.message : message;
        }
      } catch {
        // Keep the bounded status-only error.
      }
      throw new BrezelError(message, { code, status: response.status });
    }
    return response;
  }

  async #ensureEnvironment(template) {
    template = template.trim();
    if (!template) throw new TypeError("template cannot be empty");
    const name = safeName(template)
      ? template
      : `brezel-${createHash("sha256").update(template).digest("hex").slice(0, 16)}`;
    const payload = await this.requestJSON("POST", "/v1/environments", {
      body: { name, template },
      idempotencyKey: stableKey("environment", template),
    });
    return requiredString(requiredObject(payload, "resource"), "revision_id");
  }
}

export class Sandbox {
  constructor(client, id, resource = {}) {
    this.client = client;
    this.id = id;
    this.resource = { ...resource };
    this.deleted = false;
  }

  async run(
    argv,
    { cwd = "", env = {}, timeoutSeconds = 300, check = false, onEvent } = {},
  ) {
    if (!Array.isArray(argv) || argv.length === 0 || !argv.every((item) => typeof item === "string")) {
      throw new TypeError("argv must be a non-empty string array");
    }
    const response = await this.client.request(
      "POST",
      `/v1/sandboxes/${segment(this.id)}/commands`,
      {
        body: JSON.stringify({ argv, cwd, env, timeout_seconds: timeoutSeconds }),
        contentType: "application/json",
        timeoutMs: Math.max(this.client.timeoutMs, (timeoutSeconds + 5) * 1000),
      },
    );
    if (!response.body) {
      throw new BrezelError("command response did not contain a stream", { code: "invalid_response" });
    }
    if ((response.headers.get("content-type") || "").split(";", 1)[0].trim().toLowerCase() !== "application/x-ndjson") {
      await response.body.cancel();
      throw new BrezelError("command response used an unexpected media type", { code: "invalid_response" });
    }
    const stdout = [];
    const stderr = [];
    let outputBytes = 0;
    let executionId = "";
    let exitCode;
    for await (const event of ndjsonEvents(response.body)) {
      if (onEvent) onEvent(event);
      if (typeof event.execution_id === "string") executionId = event.execution_id;
      if (event.type === "stdout" || event.type === "stderr") {
        if (typeof event.data !== "string" || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(event.data)) {
          throw new BrezelError("command stream contained invalid output encoding", { code: "invalid_response" });
        }
        const chunk = Buffer.from(event.data, "base64");
        outputBytes += chunk.byteLength;
        if (outputBytes > MAX_OUTPUT_BYTES) {
          throw new BrezelError("command output exceeded the SDK limit", { code: "response_too_large" });
        }
        (event.type === "stdout" ? stdout : stderr).push(chunk);
      } else if (event.type === "exited") {
        if (exitCode !== undefined || !Number.isSafeInteger(event.exit_code)) {
          throw new BrezelError("command stream contained an invalid terminal event", { code: "invalid_response" });
        }
        exitCode = event.exit_code;
      } else if (event.type === "error") {
        throw new BrezelError("command outcome is indeterminate", { code: "command_indeterminate" });
      }
    }
    if (exitCode === undefined) {
      throw new BrezelError("command stream ended without a confirmed exit", { code: "command_indeterminate" });
    }
    const stdoutBuffer = Buffer.concat(stdout);
    const stderrBuffer = Buffer.concat(stderr);
    const result = {
      executionId,
      exitCode,
      stdout: stdoutBuffer,
      stderr: stderrBuffer,
      stdoutText: stdoutBuffer.toString("utf8"),
      stderrText: stderrBuffer.toString("utf8"),
    };
    if (check && exitCode !== 0) throw new CommandExitError(result);
    return result;
  }

  async writeFile(path, data) {
    const body = typeof data === "string" ? new TextEncoder().encode(data) : data;
    if (!(body instanceof Uint8Array)) throw new TypeError("data must be a string or Uint8Array");
    const response = await this.client.request(
      "PUT",
      `/v1/sandboxes/${segment(this.id)}/files?path=${encodeURIComponent(path)}`,
      { body, contentType: "application/octet-stream" },
    );
    return decodeJSONResponse(response, MAX_JSON_BYTES, "file metadata");
  }

  async readFile(path) {
    const response = await this.client.request(
      "GET",
      `/v1/sandboxes/${segment(this.id)}/files?path=${encodeURIComponent(path)}`,
    );
    return readLimited(response.body, 64 << 20, "file");
  }

  async preview(port, { ttlSeconds = 300 } = {}) {
    if (!Number.isSafeInteger(port) || port < 1 || port > 65535) {
      throw new TypeError("port must be between 1 and 65535");
    }
    const payload = await this.client.requestJSON(
      "POST",
      `/v1/sandboxes/${segment(this.id)}/ports/${port}/leases`,
      { body: { ttl_seconds: ttlSeconds } },
    );
    return new URL(requiredString(payload, "path").replace(/^\/+/, ""), `${this.client.baseUrl}/`).toString();
  }

  pause() {
    return this.#lifecycle("pause");
  }

  resume() {
    return this.#lifecycle("resume");
  }

  async delete() {
    if (this.deleted) return this.resource;
    const payload = await this.client.requestJSON("DELETE", `/v1/sandboxes/${segment(this.id)}`, {
      idempotencyKey: randomKey("delete"),
    });
    this.resource = { ...requiredObject(payload, "resource") };
    this.deleted = true;
    return this.resource;
  }

  async #lifecycle(action) {
    const payload = await this.client.requestJSON(
      "POST",
      `/v1/sandboxes/${segment(this.id)}:${action}`,
      { idempotencyKey: randomKey(action) },
    );
    this.resource = { ...requiredObject(payload, "resource") };
    return this.resource;
  }

  async [Symbol.asyncDispose]() {
    await this.delete();
  }
}

async function* ndjsonEvents(stream) {
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let buffer = "";
  for await (const chunk of stream) {
    try {
      buffer += decoder.decode(chunk, { stream: true });
    } catch (error) {
      throw new BrezelError("command stream contained invalid UTF-8", { code: "invalid_response", cause: error });
    }
    if (Buffer.byteLength(buffer) > MAX_EVENT_BYTES && !buffer.includes("\n")) {
      throw new BrezelError("command event exceeded the SDK limit", { code: "response_too_large" });
    }
    while (true) {
      const newline = buffer.indexOf("\n");
      if (newline < 0) break;
      const line = buffer.slice(0, newline);
      buffer = buffer.slice(newline + 1);
      if (!line) continue;
      if (Buffer.byteLength(line) > MAX_EVENT_BYTES) {
        throw new BrezelError("command event exceeded the SDK limit", { code: "response_too_large" });
      }
      try {
        const event = JSON.parse(line);
        if (!isObject(event)) throw new Error("event must be an object");
        yield event;
      } catch (error) {
        throw new BrezelError("command stream contained invalid JSON", { code: "invalid_response", cause: error });
      }
    }
  }
  try {
    buffer += decoder.decode();
  } catch (error) {
    throw new BrezelError("command stream contained invalid UTF-8", { code: "invalid_response", cause: error });
  }
  if (buffer.trim()) {
    if (Buffer.byteLength(buffer) > MAX_EVENT_BYTES) {
      throw new BrezelError("command event exceeded the SDK limit", { code: "response_too_large" });
    }
    try {
      const event = JSON.parse(buffer);
      if (!isObject(event)) throw new Error("event must be an object");
      yield event;
    } catch (error) {
      throw new BrezelError("command stream contained invalid JSON", { code: "invalid_response", cause: error });
    }
  }
}

function validateBaseUrl(value) {
  let url;
  try {
    url = new URL(value.replace(/\/+$/, ""));
  } catch {
    throw new TypeError("baseUrl must be an absolute HTTP(S) URL");
  }
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) {
    throw new TypeError("baseUrl must be an absolute HTTP(S) URL without credentials, query, or fragment");
  }
  if (url.protocol === "http:" && !["127.0.0.1", "localhost", "[::1]"].includes(url.hostname)) {
    throw new TypeError("remote Brezel URLs must use HTTPS");
  }
  return url.toString().replace(/\/$/, "");
}

function validateToken(value) {
  if (typeof value !== "string") throw new TypeError("token is required");
  const token = value.trim();
  if (token.length < 32 || /[\r\n]/.test(token)) {
    throw new TypeError("token must contain one line of at least 32 characters");
  }
  return token;
}

function validateProject(value) {
  if (typeof value !== "string" || !value || /[\r\n]/.test(value)) {
    throw new TypeError("project is required");
  }
  return value;
}

function readPrivateToken(path) {
  const before = lstatSync(path);
  validatePrivateFile(before);
  const noFollow = process.platform === "win32" ? 0 : constants.O_NOFOLLOW;
  const fd = openSync(path, constants.O_RDONLY | noFollow);
  try {
    const opened = fstatSync(fd);
    validatePrivateFile(opened);
    if (before.dev !== opened.dev || before.ino !== opened.ino) {
      throw new TypeError("tokenFile changed while opening");
    }
    const data = Buffer.alloc(4097);
    const bytesRead = readSync(fd, data, 0, data.length, 0);
    if (bytesRead > 4096) throw new TypeError("tokenFile exceeds 4096 bytes");
    const value = new TextDecoder("utf-8", { fatal: true }).decode(data.subarray(0, bytesRead));
    const after = lstatSync(path);
    validatePrivateFile(after);
    if (opened.dev !== after.dev || opened.ino !== after.ino) {
      throw new TypeError("tokenFile changed while reading");
    }
    return validateToken(value);
  } finally {
    closeSync(fd);
  }
}

function validatePrivateFile(info) {
  if (!info.isFile() || info.isSymbolicLink()) {
    throw new TypeError("tokenFile must be a regular file, not a symlink");
  }
  if (![0o400, 0o600].includes(info.mode & 0o777) || info.nlink !== 1) {
    throw new TypeError("tokenFile permissions must be 0400 or 0600 with one hard link");
  }
  if (typeof process.geteuid === "function" && info.uid !== process.geteuid()) {
    throw new TypeError("tokenFile must be owned by the effective user");
  }
}

async function decodeJSONResponse(response, maximum, description) {
  const bytes = await readLimited(response.body, maximum, description);
  try {
    return JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
  } catch (error) {
    throw new BrezelError(`${description} contained invalid JSON`, { code: "invalid_response", cause: error });
  }
}

async function readLimited(stream, maximum, description) {
  if (!stream) {
    throw new BrezelError(`${description} response did not contain a body`, { code: "invalid_response" });
  }
  const chunks = [];
  let total = 0;
  for await (const chunk of stream) {
    const bytes = chunk instanceof Uint8Array ? chunk : new Uint8Array(chunk);
    total += bytes.byteLength;
    if (total > maximum) {
      await stream.cancel().catch(() => {});
      throw new BrezelError(`${description} exceeded the SDK limit`, { code: "response_too_large" });
    }
    chunks.push(bytes);
  }
  const result = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    result.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return result;
}

function requiredObject(value, key) {
  const result = isObject(value) ? value[key] : undefined;
  if (!isObject(result)) {
    throw new BrezelError(`Brezel response is missing ${key}`, { code: "invalid_response" });
  }
  return result;
}

function requiredString(value, key) {
  const result = isObject(value) ? value[key] : undefined;
  if (typeof result !== "string" || !result) {
    throw new BrezelError(`Brezel response is missing ${key}`, { code: "invalid_response" });
  }
  return result;
}

function isObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function segment(value) {
  if (typeof value !== "string" || !value || /[\/\r\n]/.test(value)) {
    throw new TypeError("resource ID must be one non-empty path segment");
  }
  return encodeURIComponent(value);
}

function safeName(value) {
  return /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(value);
}

function randomKey(prefix) {
  return `${prefix}-${randomBytes(12).toString("hex")}`;
}

function stableKey(prefix, value) {
  return `${prefix}-${createHash("sha256").update(value).digest("hex").slice(0, 24)}`;
}
