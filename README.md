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
> **Self-hosted private-tenant developer preview.** The supported profile is one
> organization on one dedicated Ubuntu 24.04 x86-64 host with KVM and
> `/dev/net/tun`. Revision `67410ab5b928a335a79701d67eaf859df890da9c` completed
> destructive qualification on two separately administered hosts, each as an
> independent single-host deployment. Revision
> `f9fbc0ede72636349b27f01db49343d8daa87c5c` additionally passed the dedicated
> 100-way Burst profile on a named GCP KVM host. Its historical DAX report is
> retained for provenance but is no longer qualification evidence: one of three
> attempts contained a native dependency build failure hidden behind a zero
> shell exit. Brezel is
> not qualified for a cluster, high availability, hostile shared multitenancy,
> or public production. See the exact [claim boundary](docs/STATUS.md).

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

$ brezel run sbx_01... python -c 'print(6 * 7)'
42

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
| **Execution** | Streamed commands, deadlines, bounded output, confirmed exit status, and generation-bound cursor replay after an interrupted output stream without rerunning the command |
| **State** | Durable single-writer workspaces plus filesystem checkpoint and restore |
| **Lifecycle** | Expiration, automatic standby, same-host resume, cleanup, and restart recovery |
| **I/O** | Bounded file transfer and short-lived authenticated HTTP previews |
| **Network** | Deny by default; explicit unrestricted-internet opt-in; narrow private model or tool connector preview |
| **Control** | Project-bound credentials, quotas, overload admission, durable lifecycle idempotency, and a SQLite WAL lifecycle ledger |
| **Evidence** | Content-minimal Ed25519 lifecycle receipts and revision-bound qualification evidence |

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
The API remains the lifecycle authority. At revision
`67410ab5b928a335a79701d67eaf859df890da9c`, this integrated path completed
destructive qualification on each of two separately administered Linux/KVM
hosts, including simultaneous conformance and controller/node-relay crash
containment. Revision `f9fbc0ede72636349b27f01db49343d8daa87c5c`
added bounded, fail-closed guest-output replay and passed the same destructive
single-host workflow under both benchmark profiles on GCP. This evidence does
not establish a multi-node product topology.

## Readiness boundary

**Current label:** self-hosted private-tenant developer preview.

- The repository test suite exercises contracts, races, authorization denial,
  restart behavior, cleanup, and security-negative cases.
- Revision `67410ab5b928a335a79701d67eaf859df890da9c` passed the destructive
  workflow on each of two independent Ubuntu 24.04 x86-64 KVM hosts. See the
  [dated qualification report](docs/QUALIFICATION-2026-09-15.md).
- Both 24-cell matrices passed: 48 of 48 cells, 2,112 of 2,112 attempts, and
  3,168 of 3,168 expected resource cleanups. Sequential cached create through
  first verified instruction measured 70.457–70.957 ms p50 and 79.553–80.804
  ms p95 across the two hosts. Their 16-way burst p50 was 359.690–384.931 ms.
  A separate 320-attempt immediate-command stress run also completed with 100%
  success and cleanup. See the checksummed [dated
  evidence](docs/QUALIFICATION-2026-09-15.md). These host-local observations do
  not prove exactly-once execution or elimination of every stream failure, and
  do not authorize a portable startup-latency or competitive performance claim.
- Both hosts also passed an exact-revision provider-reset drill. Each reboot
  changed the host boot identity, preserved the six-file workspace corpus,
  restored access through a replacement sandbox, and confirmed cleanup. This is
  crash-durable same-host workspace recovery, not transparent process resume or
  host-loss recovery.
- On a separate 32-vCPU GCP N2 KVM host, revision
  `f9fbc0ede72636349b27f01db49343d8daa87c5c` completed 1,000 of 1,000
  command-ready executions across ten consecutive 100-sandbox bursts, held all
  100 sandboxes in every wave, and confirmed all 1,000 deletions. Its 100-way
  TTI was 2.431 s p50, 2.937 s p95, and 3.182 s p99 from a neutral HTTPS client.
  The DAX report from that revision is retained but reclassified nonconformant:
  one of its three attempts printed a native dependency build failure even
  though the enclosing upstream shell returned zero. These are self-run
  rehearsals, not official ComputeSDK leaderboard results. Raw reports and the
  corrected boundary are retained in the [dated evidence](docs/QUALIFICATION-2026-09-15.md).

<details>
<summary><strong>Not implemented yet</strong></summary>

- hostile shared-multitenant production assurance
- arbitrary OCI environment builds
- interactive PTY, SSH, desktop, WebSocket, or raw TCP transport
- public full-state checkpoint and fork operations
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
package or an official leaderboard result. The 8-vCPU/16-GiB DAX workload and
separately sized 100-way Burst TTI profile both passed on a named GCP KVM host
at revision `f9fbc0ede72636349b27f01db49343d8daa87c5c`, with complete cleanup
and retained evidence. Read the [benchmark methodology, results, and remaining
external-submission boundary](docs/BENCHMARKING.md).

Load benchmark profiles through the checked runner so every value reaches the
installer and Compose process tree:

```console
./deploy/profiles/run.sh deploy/profiles/computesdk-dax.env make qualify-single-host
./deploy/profiles/run.sh deploy/profiles/burst-100-capacity.env make qualify-single-host
```

The profiles are mutually exclusive host-wide environment shapes. Requalify
after switching one; do not mix their results.

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
