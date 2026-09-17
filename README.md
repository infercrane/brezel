<p align="center">
  <img src="assets/brezel-wordmark.svg" width="620" alt="Brezel: stateful agent sandboxes">
</p>

<p align="center">
  <strong>A secure computer for agents that starts quickly, remembers its work, and stays out of the way.</strong>
  <br>
  Self-hosted Firecracker sandboxes with a small CLI and HTTP API.
</p>

<p align="center">
  <a href="docs/STATUS.md"><img alt="Status: developer preview" src="https://img.shields.io/badge/status-developer_preview-D97706?style=flat-square"></a>
  <a href="docs/ARCHITECTURE.md"><img alt="Engine: Firecracker" src="https://img.shields.io/badge/engine-Firecracker-6D8CFF?style=flat-square"></a>
  <a href="go.mod"><img alt="Go 1.26.6" src="https://img.shields.io/badge/Go-1.26.6-00ADD8?style=flat-square&logo=go&logoColor=white"></a>
  <a href="LICENSE"><img alt="License: Apache 2.0" src="https://img.shields.io/badge/license-Apache--2.0-33A67C?style=flat-square"></a>
</p>

---

Brezel gives every agent a stateful Linux computer without making the caller
manage Firecracker, network namespaces, guest credentials, or cleanup. Create a
sandbox, run commands, move files, expose a temporary HTTP port, and let Brezel
handle standby, resume, expiration, and recovery.

```console
$ brezel new --ttl 3600
sbx_01...    running

$ brezel run sbx_01... python -c 'print(6 * 7)'
42

$ brezel stop sbx_01...
standby
```

> [!IMPORTANT]
> Brezel is a **self-hosted developer preview**. The qualified deployment is one
> organization on one dedicated Ubuntu 24.04 x86-64 host with KVM. It is not yet
> a hostile shared-multitenant or highly available production system. Read the
> exact [status and evidence boundary](docs/STATUS.md) before deployment.

## Why Brezel

- **Small surface.** Create, run, upload, download, expose, stop.
- **State that lasts.** Durable workspaces, filesystem checkpoints, automatic
  standby, and same-host resume.
- **A real isolation boundary.** Firecracker microVMs in release code, with no
  container fallback for untrusted execution.
- **Private by default.** Self-hosted operation, deny-by-default guest egress,
  protected file credentials, and no managed runtime account.
- **Failure-aware lifecycle.** Idempotent mutations, bounded operations,
  explicit terminal states, restart reconciliation, and verified cleanup.
- **Reproducible evidence.** Pinned inputs, conformance tests, destructive host
  qualification, raw benchmark attempts, and signed lifecycle receipts.

## Start on a dedicated host

Requirements: Ubuntu 24.04 x86-64, KVM, `/dev/net/tun`, cgroup v2, Docker
Engine with Compose v2 and Buildx, Go 1.26.6, Python 3, Git, OpenSSL, GNU
`patch`, `sha256sum`, and `tar`.

```console
git clone https://github.com/infercrane/brezel.git
cd brezel

make build
export PATH="$PWD/bin:$PATH"
brezel doctor
make qualify-single-host

export BREZEL_SERVICE_TOKEN_FILE="$PWD/.brezel/secrets/service.token"
brezel new
```

`make qualify-single-host` is intentionally destructive. Run it only on a
dedicated evaluation machine. It installs the pinned engine distribution,
creates real microVMs, exercises the public product path twice, checks failure
boundaries, and verifies cleanup.

## One small interface

```console
# Sandboxes
brezel new --template base --ttl 3600
brezel list
brezel inspect sbx_01...
brezel delete sbx_01...

# Commands and files
brezel run --cwd /workspace sbx_01... npm test
brezel put sbx_01... /workspace/input.json ./input.json
brezel get sbx_01... /workspace/result.json ./result.json

# Temporary HTTP preview
brezel open --ttl 300 sbx_01... 3000

# Durable state and lifecycle
brezel workspace create agent-state
brezel checkpoint create --name before-refactor sbx_01...
brezel stop sbx_01...
brezel start sbx_01...
```

Run `brezel help`, `brezel doctor`, and `brezel version --json` without
credentials. Authenticated commands read the service token from a protected
file, keeping it out of argv, environment values, and shell history.

The CLI uses the same resource-oriented HTTP API:

```http
POST /v1/sandboxes HTTP/1.1
Authorization: Bearer <opaque-service-token>
X-Project-ID: brezel-default
Idempotency-Key: <unique-operation-key>
Content-Type: application/json

{
  "environment_revision": "envrev_...",
  "lifecycle": { "expires_after_seconds": 3600 },
  "network": { "allow_internet": false }
}
```

See the implemented [API reference](docs/API.md) and provisional schemas in
[`spec/v1alpha1`](spec/v1alpha1).

## What is implemented

| Area | Developer-preview capability |
| --- | --- |
| Isolation | Firecracker microVMs; no release container fallback |
| Commands | Streaming output, deadlines, bounded replay, and confirmed exit status |
| Files and ports | Bounded upload/download and short-lived authenticated HTTP previews |
| State | Durable single-writer workspaces and filesystem checkpoint/restore |
| Lifecycle | Expiration, automatic standby, same-host resume, cleanup, and restart recovery |
| Network | Deny by default; explicit internet opt-in; narrow private connector preview |
| Control | Project-bound credentials, quotas, overload admission, and SQLite WAL state |
| Node path | mTLS relay, single-operation capabilities, replay defense, and generation fencing |
| Supply chain | Pinned source, patches, images, VM artifacts, and runtime attestation |

Not yet implemented: arbitrary OCI builds, interactive PTY/SSH, desktop or
WebSocket transport, public full-state forks, Python and TypeScript SDKs,
multi-node scheduling, replicated state, OIDC/RBAC, GPU passthrough, and
hostile shared-multitenant assurance. The [roadmap](docs/ROADMAP.md) defines the
gates for those capabilities.

## How it works

```text
CLI / HTTP client
       │
       ▼
Brezel API ───── policy · lifecycle · durable state · receipts
       │
       ▼ mTLS + signed, single-operation capability
node relay ───── pinned Firecracker engine ───── isolated guest
       │                                           │
       └──────────── durable workspace ────────────┘
```

Brezel owns the public contract, lifecycle, policy, data paths, packaging,
qualification, and evidence. The initial engine distribution uses pinned,
locally patched Apache-2.0 E2B Runtime components. Brezel does not use E2B
Cloud and does not require an E2B API key. The dependency and replacement
boundary are documented in [ADR 0002](docs/decisions/0002-runtime-substrate.md).

## Verify and benchmark

Repository checks run on macOS and Linux. Only the dedicated Linux/KVM workflow
establishes the microVM boundary.

```console
make check
make qualify-single-host     # destructive, dedicated Linux/KVM host only
make benchmark-single-host   # destructive, preserves raw evidence
```

The benchmark measures completed useful work, not accepted API requests. It
covers create through first verified instruction, warm commands, pause/resume,
checkpoint/restore, preview first byte, and workspace I/O under sequential and
burst arrivals. Reports retain every attempt, failure, cleanup result, host
identity, revision, and percentile. Read the [methodology](docs/BENCHMARKING.md)
before comparing or publishing results.

The repository also includes a dependency-free
[ComputeSDK adapter](benchmarks/computesdk) for identical-workload testing. It
is an integration bridge, not a published provider or official leaderboard
result. Named-host qualification results and their limitations are retained in
[Status](docs/STATUS.md).

## Documentation

- [Status and claim boundary](docs/STATUS.md)
- [API](docs/API.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Threat model](docs/THREAT_MODEL.md)
- [Artifact supply chain](docs/ARTIFACT-SUPPLY-CHAIN.md)
- [Benchmark methodology](docs/BENCHMARKING.md)
- [Roadmap](docs/ROADMAP.md)

## Contributing and security

Read [CONTRIBUTING.md](CONTRIBUTING.md) before changing a trust, lifecycle, or
API boundary. Report vulnerabilities privately according to
[SECURITY.md](SECURITY.md), never through a public issue.

Brezel is licensed under [Apache 2.0](LICENSE). Third-party attributions cover
the pinned [runtime patches](third_party/e2b-runtime/NOTICE.md) and the narrow
[guest protocol subset](third_party/e2b/NOTICE.md).
