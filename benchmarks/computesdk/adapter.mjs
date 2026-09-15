import { randomUUID } from "node:crypto";
import {
  closeSync,
  constants as fsConstants,
  fstatSync,
  lstatSync,
  openSync,
  readFileSync,
} from "node:fs";

const MAX_TOKEN_BYTES = 16 * 1024;
const MAX_ERROR_BYTES = 64 * 1024;
const MAX_JSON_BYTES = 2 * 1024 * 1024;
const MAX_STREAM_LINE_BYTES = 2 * 1024 * 1024;
const MAX_COMMAND_OUTPUT_BYTES = 64 * 1024 * 1024;
const DEFAULT_CREATE_TIMEOUT_MS = 120_000;
const DEFAULT_DESTROY_TIMEOUT_MS = 120_000;
const DEFAULT_COMMAND_TIMEOUT_MS = 300_000;
const DEFAULT_SANDBOX_TTL_SECONDS = 3_600;

class BrezelHttpError extends Error {
  constructor(status, message) {
    super(message);
    this.name = "BrezelHttpError";
    this.status = status;
  }
}

function positiveInteger(value, fallback, name) {
  const resolved = value ?? fallback;
  if (!Number.isSafeInteger(resolved) || resolved <= 0) {
    throw new Error(`${name} must be a positive integer`);
  }
  return resolved;
}

function resolveBaseUrl(value) {
  let parsed;
  try {
    parsed = new URL(value);
  } catch {
    throw new Error("BREZEL_API_URL must be an absolute URL");
  }
  if (parsed.username || parsed.password || parsed.hash) {
    throw new Error("BREZEL_API_URL must not contain credentials or a fragment");
  }
  const host = parsed.hostname.toLowerCase();
  const loopback = host === "localhost" || host === "::1" || /^127(?:\.\d{1,3}){3}$/.test(host);
  if (parsed.protocol !== "https:" && !(parsed.protocol === "http:" && loopback)) {
    throw new Error("BREZEL_API_URL must use HTTPS (loopback HTTP is allowed for development)");
  }
  parsed.pathname = parsed.pathname.replace(/\/+$/, "");
  parsed.search = "";
  return parsed;
}

function readProtectedToken(path) {
  if (!path) throw new Error("BREZEL_SERVICE_TOKEN_FILE is required");
  const noFollow = fsConstants.O_NOFOLLOW ?? 0;
  let fd;
  try {
    fd = openSync(path, fsConstants.O_RDONLY | noFollow);
    const before = fstatSync(fd);
    const after = lstatSync(path);
    if (!before.isFile() || !after.isFile() || before.dev !== after.dev || before.ino !== after.ino) {
      throw new Error("service token path must resolve to one regular file");
    }
    if (before.nlink !== 1) throw new Error("service token file must have exactly one hard link");
    if ((before.mode & 0o777) !== 0o600) throw new Error("service token file permissions must be 0600");
    if (typeof process.getuid === "function" && before.uid !== process.getuid()) {
      throw new Error("service token file must be owned by the current user");
    }
    if (before.size <= 0 || before.size > MAX_TOKEN_BYTES) {
      throw new Error(`service token file must contain at most ${MAX_TOKEN_BYTES} bytes`);
    }
    const token = readFileSync(fd, { encoding: "utf8" }).trim();
    if (token.length < 32) throw new Error("service token must contain at least 32 characters");
    if (/\s/.test(token)) throw new Error("service token must not contain whitespace");
    return token;
  } catch (error) {
    const message = error instanceof Error ? error.message : "could not read service token file";
    throw new Error(`Invalid Brezel service token file: ${message}`);
  } finally {
    if (fd !== undefined) closeSync(fd);
  }
}

function resolveConfig(config) {
  const baseUrlValue = config.baseUrl ?? process.env.BREZEL_API_URL;
  const tokenFile = config.tokenFile ?? process.env.BREZEL_SERVICE_TOKEN_FILE;
  const projectId = config.projectId ?? process.env.BREZEL_PROJECT_ID;
  const environmentRevision = config.environmentRevision ?? process.env.BREZEL_ENVIRONMENT_REVISION;
  if (!baseUrlValue) throw new Error("BREZEL_API_URL is required");
  if (!projectId) throw new Error("BREZEL_PROJECT_ID is required");
  if (!environmentRevision) throw new Error("BREZEL_ENVIRONMENT_REVISION is required");
  if (!/^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(projectId)) {
    throw new Error("BREZEL_PROJECT_ID is invalid");
  }
  return Object.freeze({
    baseUrl: resolveBaseUrl(baseUrlValue),
    projectId,
    token: readProtectedToken(tokenFile),
    environmentRevision,
    commandTimeoutMs: positiveInteger(config.commandTimeoutMs, DEFAULT_COMMAND_TIMEOUT_MS, "commandTimeoutMs"),
    createTimeoutMs: positiveInteger(config.createTimeoutMs, DEFAULT_CREATE_TIMEOUT_MS, "createTimeoutMs"),
    destroyTimeoutMs: positiveInteger(config.destroyTimeoutMs, DEFAULT_DESTROY_TIMEOUT_MS, "destroyTimeoutMs"),
    sandboxTtlSeconds: positiveInteger(config.sandboxTtlSeconds, DEFAULT_SANDBOX_TTL_SECONDS, "sandboxTtlSeconds"),
    allowInternet: config.allowInternet ?? false,
  });
}

function endpoint(config, path) {
  const prefix = config.baseUrl.pathname === "/" ? "" : config.baseUrl.pathname.replace(/\/+$/, "");
  return new URL(`${prefix}${path}`, `${config.baseUrl.origin}/`);
}

async function boundedText(response, limit = MAX_ERROR_BYTES) {
  const reader = response.body?.getReader();
  if (!reader) return "";
  const chunks = [];
  let size = 0;
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    if (!value) continue;
    size += value.byteLength;
    if (size > limit) {
      await reader.cancel("response exceeded adapter limit");
      throw new Error("Brezel API response exceeded the adapter limit");
    }
    chunks.push(value);
  }
  return new TextDecoder().decode(Buffer.concat(chunks));
}

function requireResponseType(response, expected) {
  const value = response.headers.get("content-type") ?? "";
  const mediaType = value.split(";", 1)[0].trim().toLowerCase();
  if (mediaType !== expected) {
    throw new Error(`Brezel API response must use ${expected}`);
  }
}

function sanitizedApiMessage(raw, status) {
  if (raw) {
    try {
      const parsed = JSON.parse(raw);
      const nested = parsed && typeof parsed.error === "object" ? parsed.error : undefined;
      const message = typeof parsed?.error === "string" ? parsed.error : nested?.message ?? parsed?.message;
      const code = nested?.code;
      if (typeof message === "string" && message) {
        return `Brezel API request failed (${code ?? status}): ${message.slice(0, 512)}`;
      }
    } catch {
      // Never echo arbitrary server text; it can contain reflected request data.
    }
  }
  return `Brezel API request failed with status ${status}`;
}

async function request(config, method, path, options = {}) {
  const headers = new Headers({
    Authorization: `Bearer ${config.token}`,
    "X-Project-ID": config.projectId,
  });
  if (options.contentType) headers.set("Content-Type", options.contentType);
  if (options.idempotencyKey) headers.set("Idempotency-Key", options.idempotencyKey);
  const response = await fetch(endpoint(config, path), {
    method,
    headers,
    body: options.body,
    signal: options.signal,
  });
  if (!response.ok) {
    throw new BrezelHttpError(response.status, sanitizedApiMessage(await boundedText(response), response.status));
  }
  return response;
}

async function requestJson(config, method, path, body, idempotencyKey, signal) {
  const response = await request(config, method, path, {
    body: body === undefined ? undefined : JSON.stringify(body),
    contentType: body === undefined ? undefined : "application/json",
    idempotencyKey,
    signal,
  });
  requireResponseType(response, "application/json");
  const raw = await boundedText(response, MAX_JSON_BYTES);
  try {
    return JSON.parse(raw);
  } catch {
    throw new Error("Brezel API returned malformed or oversized JSON");
  }
}

function withDeadline(parent, milliseconds) {
  const controller = new AbortController();
  const abort = () => controller.abort(parent?.reason);
  if (parent?.aborted) abort();
  else parent?.addEventListener("abort", abort, { once: true });
  const timer = setTimeout(() => controller.abort(new Error("Brezel operation timed out")), milliseconds);
  return {
    signal: controller.signal,
    dispose() {
      clearTimeout(timer);
      parent?.removeEventListener("abort", abort);
    },
  };
}

async function delay(milliseconds, signal) {
  await new Promise((resolve, reject) => {
    const settled = () => signal.removeEventListener("abort", aborted);
    const timer = setTimeout(() => {
      settled();
      resolve();
    }, milliseconds);
    const aborted = () => {
      clearTimeout(timer);
      settled();
      reject(signal.reason ?? new Error("operation aborted"));
    };
    if (signal.aborted) aborted();
    else signal.addEventListener("abort", aborted, { once: true });
  });
}

async function getSandbox(config, sandboxId, signal) {
  try {
    const wire = await requestJson(config, "GET", `/v1/sandboxes/${encodeURIComponent(sandboxId)}`, undefined, undefined, signal);
    return validateSandbox(wire);
  } catch (error) {
    if (error instanceof BrezelHttpError && error.status === 404) return null;
    throw error;
  }
}

function validateSandbox(wire) {
  if (!wire || typeof wire !== "object" || Array.isArray(wire)) {
    throw new Error("Brezel API returned an invalid sandbox resource");
  }
  for (const name of ["id", "environment_revision", "state", "created_at", "expires_at"]) {
    if (typeof wire[name] !== "string" || wire[name].length === 0) {
      throw new Error(`Brezel API sandbox resource is missing ${name}`);
    }
  }
  if (!Number.isSafeInteger(wire.revision) || wire.revision < 1) {
    throw new Error("Brezel API sandbox resource has an invalid revision");
  }
  return wire;
}

async function waitForState(config, sandboxId, acceptable, timeoutMs, parentSignal) {
  const deadline = withDeadline(parentSignal, timeoutMs);
  try {
    for (;;) {
      const sandbox = await getSandbox(config, sandboxId, deadline.signal);
      if (!sandbox || acceptable.has(sandbox.state)) return sandbox;
      if (sandbox.state === "failed") throw new Error("Brezel sandbox entered failed state");
      await delay(100, deadline.signal);
    }
  } finally {
    deadline.dispose();
  }
}

async function waitForDeletion(config, sandboxId, timeoutMs) {
  // Expired means policy has forbidden further use while cleanup is pending.
  // Only a deleted resource or a 404 from getSandbox confirms absence.
  return waitForState(config, sandboxId, new Set(["deleted"]), timeoutMs);
}

function decodeBase64(value) {
  if (!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value)) {
    throw new Error("Brezel command stream contained invalid base64 output");
  }
  return Buffer.from(value, "base64");
}

async function runCommand(config, sandboxId, command, options = {}) {
  if (typeof command !== "string" || command.length === 0) throw new Error("command must be a non-empty string");
  const timeoutMs = positiveInteger(options.timeout, config.commandTimeoutMs, "command timeout");
  const timeoutSeconds = Math.max(1, Math.ceil(timeoutMs / 1000));
  const commandLine = options.background ? `{ ${command}; } >/dev/null 2>&1 </dev/null &` : command;
  const deadline = withDeadline(options.signal, timeoutMs + 10_000);
  const startedAt = performance.now();
  try {
    const response = await request(config, "POST", `/v1/sandboxes/${encodeURIComponent(sandboxId)}/commands`, {
      body: JSON.stringify({
        argv: ["/bin/sh", "-lc", commandLine],
        cwd: options.cwd ?? "",
        env: options.env ?? {},
        timeout_seconds: timeoutSeconds,
      }),
      contentType: "application/json",
      signal: deadline.signal,
    });
    requireResponseType(response, "application/x-ndjson");
    if (!response.body) throw new Error("Brezel command response did not contain a stream");

    const reader = response.body.getReader();
    const lineDecoder = new TextDecoder();
    const stdoutDecoder = new TextDecoder();
    const stderrDecoder = new TextDecoder();
    let buffer = "";
    let stdout = "";
    let stderr = "";
    let outputBytes = 0;
    let exitCode;
    let streamFailed = false;
    let exitEvents = 0;

    const consume = (line) => {
      if (!line.trim()) return;
      let event;
      try {
        event = JSON.parse(line);
      } catch {
        throw new Error("Brezel command stream contained malformed JSON");
      }
      if (event.type === "stdout" || event.type === "stderr") {
        const bytes = decodeBase64(event.data ?? "");
        outputBytes += bytes.byteLength;
        if (outputBytes > MAX_COMMAND_OUTPUT_BYTES) throw new Error("Brezel command output exceeded the adapter limit");
        if (event.type === "stdout") {
          const text = stdoutDecoder.decode(bytes, { stream: true });
          stdout += text;
          if (text) options.onStdout?.(text);
        } else {
          const text = stderrDecoder.decode(bytes, { stream: true });
          stderr += text;
          if (text) options.onStderr?.(text);
        }
      } else if (event.type === "exited") {
        exitEvents += 1;
        if (exitEvents > 1 || event.exited !== true || !Number.isInteger(event.exit_code)) {
          throw new Error("Brezel command stream contained an invalid terminal event");
        }
        exitCode = event.exit_code;
      } else if (event.type === "error") {
        streamFailed = true;
      } else if (event.type !== "started") {
        throw new Error("Brezel command stream contained an unsupported event");
      }
    };

    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buffer += lineDecoder.decode(value, { stream: true });
      if (Buffer.byteLength(buffer) > MAX_STREAM_LINE_BYTES && !buffer.includes("\n")) {
        await reader.cancel("command stream line exceeded adapter limit");
        throw new Error("Brezel command stream line exceeded the adapter limit");
      }
      let newline;
      while ((newline = buffer.indexOf("\n")) >= 0) {
        const line = buffer.slice(0, newline);
        if (Buffer.byteLength(line) > MAX_STREAM_LINE_BYTES) {
          await reader.cancel("command stream line exceeded adapter limit");
          throw new Error("Brezel command stream line exceeded the adapter limit");
        }
        consume(line);
        buffer = buffer.slice(newline + 1);
      }
    }
    buffer += lineDecoder.decode();
    if (Buffer.byteLength(buffer) > MAX_STREAM_LINE_BYTES) {
      throw new Error("Brezel command stream line exceeded the adapter limit");
    }
    consume(buffer);
    const stdoutTail = stdoutDecoder.decode();
    const stderrTail = stderrDecoder.decode();
    stdout += stdoutTail;
    stderr += stderrTail;
    if (stdoutTail) options.onStdout?.(stdoutTail);
    if (stderrTail) options.onStderr?.(stderrTail);
    if (streamFailed) {
      throw new Error("Brezel command outcome is unknown because the stream failed");
    }
    if (exitCode === undefined) {
      throw new Error("Brezel command stream ended without an exit event");
    }
    return { stdout, stderr, exitCode, durationMs: Math.max(0, performance.now() - startedAt) };
  } finally {
    deadline.dispose();
  }
}

function mapStatus(state) {
  if (["requested", "preparing", "running", "resuming"].includes(state)) return "running";
  if (["pausing", "standby", "deleting", "deleted", "expired"].includes(state)) return "stopped";
  return "error";
}

function sandboxHandle(config, wire) {
  return {
    sandboxId: wire.id,
    provider: "brezel",
    async runCommand(command, options) {
      return runCommand(config, wire.id, command, options);
    },
    async destroy() {
      try {
        await requestJson(config, "DELETE", `/v1/sandboxes/${encodeURIComponent(wire.id)}`, undefined, randomUUID());
      } catch (error) {
        if (error instanceof BrezelHttpError && error.status === 404) return;
        throw error;
      }
      await waitForDeletion(config, wire.id, config.destroyTimeoutMs);
    },
    async getInfo() {
      const current = await getSandbox(config, wire.id);
      if (!current) {
        return { id: wire.id, provider: "brezel", status: "stopped", createdAt: new Date(wire.created_at), timeout: 0 };
      }
      return {
        id: current.id,
        provider: "brezel",
        status: mapStatus(current.state),
        createdAt: new Date(current.created_at),
        timeout: Math.max(0, new Date(current.expires_at).getTime() - Date.now()),
        metadata: {
          state: current.state,
          environmentRevision: current.environment_revision,
          revision: current.revision,
        },
      };
    },
    async getUrl(options) {
      if (!Number.isSafeInteger(options?.port) || options.port < 1 || options.port > 65_535) {
        throw new Error("port must be between 1 and 65535");
      }
      if (options.protocol && options.protocol !== "http" && options.protocol !== "https") {
        throw new Error("Brezel public previews currently support HTTP and HTTPS only");
      }
      const lease = await requestJson(
        config,
        "POST",
        `/v1/sandboxes/${encodeURIComponent(wire.id)}/ports/${options.port}/leases`,
        { ttl_seconds: 900 },
        randomUUID(),
      );
      return new URL(lease.path, config.baseUrl).toString();
    },
  };
}

async function cleanupFailedCreate(config, sandboxId, originalError) {
  let cleanupError;
  try {
    await requestJson(config, "DELETE", `/v1/sandboxes/${encodeURIComponent(sandboxId)}`, undefined, randomUUID());
    await waitForDeletion(config, sandboxId, config.destroyTimeoutMs);
  } catch (error) {
    if (!(error instanceof BrezelHttpError && error.status === 404)) cleanupError = error;
  }
  if (cleanupError) {
    throw new AggregateError(
      [originalError, cleanupError],
      "Brezel sandbox creation failed and cleanup could not be confirmed",
    );
  }
  throw originalError;
}

/**
 * Return the small ComputeSDK shape consumed by computesdk-benchmarks.
 * Authentication is deliberately file-backed; this function accepts no raw
 * service-token option and never places that token in a command or environment.
 */
export function createBrezelCompute(configInput = {}) {
  const config = resolveConfig(configInput);
  return {
    sandbox: {
      async create(options = {}) {
        if (options.envs && Object.keys(options.envs).length > 0) {
          throw new Error("Brezel create-time environment variables are not exposed by the current public API");
        }
        if (options.snapshotId && options.templateId) {
          throw new Error("Brezel create accepts either snapshotId or templateId, not both");
        }
        const ttlSeconds = options.timeout === undefined
          ? config.sandboxTtlSeconds
          : Math.ceil(positiveInteger(options.timeout, config.sandboxTtlSeconds * 1000, "sandbox timeout") / 1000);
        const body = options.snapshotId
          ? {
              checkpoint_id: options.snapshotId,
              lifecycle: { expires_after_seconds: ttlSeconds },
              network: { allow_internet: config.allowInternet },
            }
          : {
              environment_revision: options.templateId ?? config.environmentRevision,
              lifecycle: { expires_after_seconds: ttlSeconds },
              network: { allow_internet: config.allowInternet },
            };
        const response = await requestJson(config, "POST", "/v1/sandboxes", body, randomUUID(), options.signal);
        const requested = validateSandbox(response?.resource);
        if (requested.state === "running") return sandboxHandle(config, requested);
        try {
          const ready = await waitForState(config, requested.id, new Set(["running"]), config.createTimeoutMs, options.signal);
          if (!ready) throw new Error("Brezel sandbox disappeared while it was being created");
          return sandboxHandle(config, ready);
        } catch (error) {
          return cleanupFailedCreate(config, requested.id, error);
        }
      },
      async getById(sandboxId) {
        const wire = await getSandbox(config, sandboxId);
        return wire ? sandboxHandle(config, wire) : null;
      },
      async list() {
        const response = await requestJson(config, "GET", "/v1/sandboxes");
        if (!Array.isArray(response?.sandboxes)) throw new Error("Brezel API returned an invalid sandbox list");
        return response.sandboxes.map((wire) => sandboxHandle(config, validateSandbox(wire)));
      },
      async destroy(sandboxId) {
        const sandbox = await this.getById(sandboxId);
        if (sandbox) await sandbox.destroy();
      },
    },
  };
}

/** Create an adapter from the four non-secret environment values used by CI. */
export function createBrezelComputeFromEnv() {
  return createBrezelCompute({
    baseUrl: process.env.BREZEL_API_URL,
    tokenFile: process.env.BREZEL_SERVICE_TOKEN_FILE,
    projectId: process.env.BREZEL_PROJECT_ID,
    environmentRevision: process.env.BREZEL_ENVIRONMENT_REVISION,
  });
}
