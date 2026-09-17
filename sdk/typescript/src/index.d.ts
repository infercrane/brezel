export interface ClientOptions {
  token: string;
  baseUrl?: string;
  project?: string;
  timeoutMs?: number;
  fetch?: typeof globalThis.fetch;
}

export interface TokenFileOptions extends Omit<ClientOptions, "token"> {}

export interface WorkspaceMount {
  workspace_id: string;
  path: string;
}

export interface CreateSandboxOptions {
  template?: string;
  ttlSeconds?: number;
  standbyAfterSeconds?: number;
  allowInternet?: boolean;
  workspaceMounts?: WorkspaceMount[];
}

export interface CommandEvent {
  execution_id?: string;
  type: "started" | "stdout" | "stderr" | "exited" | "error";
  data?: string;
  exit_code?: number;
  [key: string]: unknown;
}

export interface CommandResult {
  executionId: string;
  exitCode: number;
  stdout: Uint8Array;
  stderr: Uint8Array;
  stdoutText: string;
  stderrText: string;
}

export interface RunOptions {
  cwd?: string;
  env?: Record<string, string>;
  timeoutSeconds?: number;
  check?: boolean;
  onEvent?: (event: CommandEvent) => void;
}

export class BrezelError extends Error {
  readonly code: string;
  readonly status: number;
}

export class CommandExitError extends BrezelError {
  readonly result: CommandResult;
}

export class BrezelClient {
  constructor(options: ClientOptions);
  static fromTokenFile(tokenFile: string, options?: TokenFileOptions): BrezelClient;
  readonly baseUrl: string;
  readonly project: string;
  readonly timeoutMs: number;
  createSandbox(options?: CreateSandboxOptions): Promise<Sandbox>;
  sandbox(id: string): Promise<Sandbox>;
  listSandboxes(options?: { includeTerminal?: boolean }): Promise<Record<string, unknown>[]>;
  requestJSON(method: string, path: string, options?: Record<string, unknown>): Promise<unknown>;
}

export class Sandbox implements AsyncDisposable {
  readonly id: string;
  resource: Record<string, unknown>;
  run(argv: string[], options?: RunOptions): Promise<CommandResult>;
  writeFile(path: string, data: string | Uint8Array): Promise<Record<string, unknown>>;
  readFile(path: string): Promise<Uint8Array>;
  preview(port: number, options?: { ttlSeconds?: number }): Promise<string>;
  pause(): Promise<Record<string, unknown>>;
  resume(): Promise<Record<string, unknown>>;
  delete(): Promise<Record<string, unknown>>;
  [Symbol.asyncDispose](): Promise<void>;
}
