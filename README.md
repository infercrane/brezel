<p align="center">
  <img src="assets/brezel-wordmark.svg" width="620" alt="Brezel: stateful agent sandboxes">
</p>

<p align="center">
  <strong>One small interface for isolated execution, durable work, and automatic standby.</strong>
  <br>
  Run agent code in Firecracker microVMs on a dedicated machine you control.
</p>

<p align="center">
  <a href="docs/STATUS.md"><img alt="Status: private preview" src="https://img.shields.io/badge/status-private_preview-D97706?style=flat-square"></a>
  <a href="docs/ARCHITECTURE.md"><img alt="Engine: Firecracker" src="https://img.shields.io/badge/engine-Firecracker-6D8CFF?style=flat-square"></a>
  <a href="go.mod"><img alt="Go 1.23" src="https://img.shields.io/badge/Go-1.23-00ADD8?style=flat-square&logo=go&logoColor=white"></a>
  <a href="LICENSE"><img alt="License: Apache 2.0" src="https://img.shields.io/badge/license-Apache--2.0-33A67C?style=flat-square"></a>
</p>

---

Brezel gives agents an isolated Linux environment without making callers
manage Firecracker, network namespaces, snapshots, or guest credentials. Its
public CLI and HTTP API cover commands, files, temporary HTTP previews,
persistent workspaces, checkpoints, and lifecycle.

> [!WARNING]
> **Private developer preview.** The supported evaluation profile is one
> organization on one dedicated Ubuntu 24.04 x86-64 host with KVM and
> `/dev/net/tun`. Brezel is not yet qualified for hostile shared multitenancy or
> production availability. See the exact [claim boundary](docs/STATUS.md).

## First sandbox

On a supported, dedicated host:

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

The qualification target is intentionally destructive. It builds and starts
the pinned engine distribution, creates real microVM resources, and runs the
single-host suite twice. Do not run it on a workstation that carries unrelated
workloads.

### CLI · available now

```console
$ brezel new --ttl 3600
sbx_01...    running

$ brezel run sbx_01... /bin/sh -lc 'python --version && npm test'
Python 3.13.7
...

$ brezel put sbx_01... /workspace/input.json ./input.json
$ brezel open sbx_01... 3000
http://127.0.0.1:...    expires 2026-09-14T13:00:00Z

$ brezel stop sbx_01...
standby

$ brezel start sbx_01...
running
```

Run `brezel help` for sandboxes, workspaces, checkpoints, files, previews, and
advanced resource commands.

### HTTP API · available now

The CLI uses the same substrate-neutral resource API. This is the implemented
request shape; the CLI reads credentials from a protected file so tokens do not
need to appear in shell history.

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

See the implemented [HTTP API](docs/API.md) and provisional schemas under
[`spec/v1alpha1`](spec/v1alpha1).

<details>
<summary><strong>SDK shape · planned, not published</strong></summary>

There is no installable Python or TypeScript SDK in the developer preview. The
intended interface remains deliberately small:

```python
# Design target only. This package does not exist yet.
sandbox = client.sandboxes.create(template="base")
result = sandbox.run(["python", "agent.py"])
url = sandbox.expose(3000)
sandbox.stop()
```

</details>

## What works today

| Capability | Current implementation |
| --- | --- |
| **Isolation** | Firecracker microVMs with no container fallback in release code |
| **Execution** | Streamed commands, deadlines, bounded output, and real exit status |
| **State** | Durable single-writer workspaces plus filesystem checkpoint and restore |
| **Lifecycle** | Expiration, automatic standby, same-host resume, cleanup, and restart recovery |
| **I/O** | Bounded file transfer and short-lived authenticated HTTP previews |
| **Network** | Deny-by-default policy and a narrow private model or tool connector preview |
| **Control** | Project-bound credentials, quotas, overload admission, idempotent operations, and a SQLite WAL lifecycle ledger |
| **Evidence** | Content-minimal Ed25519 lifecycle receipts and reproducible qualification |

## Architecture

```text
CLI / HTTP client
       │
       ▼
Brezel API ───── policy · lifecycle · durable state · receipts
       │
       ▼
node data path ───── pinned Firecracker engine ───── isolated guest
       │                                                │
       └────────────── durable workspace ───────────────┘
```

Brezel owns the public contract, lifecycle, policy, data paths, packaging,
qualification, and evidence. The first engine distribution uses pinned,
locally patched Apache-2.0 E2B Runtime components. It does not use E2B Cloud or
require an E2B API key.

The single-host distribution sends command, file, and preview traffic through
a separate node relay. It uses mTLS identities, one-operation Ed25519
capabilities, replay defense, durable route generations, and bounded protocols.
The API remains the lifecycle authority. This integrated path passes repository
tests but still requires fresh Linux/KVM failure qualification before it can
carry a stronger release label.

## Readiness boundary

**Current label:** private single-host release candidate.

- The repository test suite exercises contracts, races, authorization denial,
  restart behavior, cleanup, and security-negative cases.
- A previous revision passed destructive qualification on a named four-core
  Xeon host. That historical run does not qualify the current commit.
- The current revision needs a fresh Linux/KVM qualification and benchmark
  matrix. Brezel makes no portable startup-latency claim.

<details>
<summary><strong>Not implemented yet</strong></summary>

- hostile shared-multitenant production assurance
- arbitrary OCI environment builds
- interactive PTY, SSH, desktop, WebSocket, or raw TCP transport
- full-state checkpoint and fork
- Python and TypeScript SDKs
- multi-node scheduling or replicated state and storage
- OIDC, organizations, RBAC, approvals, or dynamic quota administration
- GPU passthrough, hardware attestation, or qualified air-gapped operation

</details>

## Verify and benchmark

Repository checks run on macOS or Linux. Only a supported Linux/KVM host can
establish the microVM runtime boundary.

```console
make check
make qualify-single-host
make benchmark-single-host
```

The benchmark validates useful work rather than an accepted create request. It
measures create through first instruction, warm command, pause and resume,
checkpoint and restore, preview first byte, and workspace I/O under sequential,
staggered, and burst arrivals. Reports retain raw attempts, failures, cleanup,
host and revision identity, and p50, p95, and p99. Read the [methodology](docs/BENCHMARKING.md)
before publishing results.

A dependency-free adapter under [`benchmarks/computesdk`](benchmarks/computesdk)
also exercises Brezel through the public ComputeSDK sandbox shape. It is an
integration and independent-benchmark bridge, not yet a published provider
package or a performance claim.

## Host requirements

- Ubuntu 24.04 on x86-64
- KVM, `/dev/net/tun`, and cgroup v2
- Docker Engine, Compose v2, and Buildx
- Go 1.23, Python 3, Git, OpenSSL, `patch`, `sha256sum`, and `tar`

Inputs are commit- or digest-pinned, or explicitly operator-preloaded. The
installer records the resulting distribution identities. Read the [artifact
supply-chain contract](docs/ARTIFACT-SUPPLY-CHAIN.md) before mirroring inputs.

## Documentation

<p align="center">
  <a href="docs/STATUS.md">Status</a> ·
  <a href="docs/API.md">API</a> ·
  <a href="docs/ARCHITECTURE.md">Architecture</a> ·
  <a href="docs/THREAT_MODEL.md">Threat model</a> ·
  <a href="docs/ENGINEERING-STRATEGY.md">Engineering strategy</a> ·
  <a href="docs/BENCHMARKING.md">Benchmarks</a> ·
  <a href="docs/ROADMAP.md">Roadmap</a>
</p>

## Contributing, security, and license

This repository is currently a private developer preview. Follow
[CONTRIBUTING.md](CONTRIBUTING.md) for changes and report vulnerabilities using
the private process in [SECURITY.md](SECURITY.md), not a public issue.

Brezel is licensed under [Apache 2.0](LICENSE). Third-party attributions cover
the pinned [runtime patches](third_party/e2b-runtime/NOTICE.md) and the narrow
[guest protocol subset](third_party/e2b/NOTICE.md).
