<p align="center">
  <img src="assets/brezel-wordmark.svg" width="620" alt="Brezel: stateful agent sandboxes">
</p>

<p align="center">
  <strong>Give every agent its own computer.</strong>
  <br>
  Stateful Firecracker sandboxes that keep untrusted code away from your laptop and keys.
</p>

<p align="center">
  <a href="https://github.com/infercrane/brezel/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/infercrane/brezel/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://pypi.org/project/brezel-sdk/"><img alt="PyPI: brezel-sdk" src="https://img.shields.io/pypi/v/brezel-sdk?style=flat-square&logo=pypi&logoColor=white"></a>
  <a href="https://www.npmjs.com/package/@infercrane/brezel"><img alt="npm: @infercrane/brezel" src="https://img.shields.io/npm/v/%40infercrane%2Fbrezel?style=flat-square&logo=npm"></a>
  <a href="docs/GO-LIVE.md"><img alt="Deployment: self-hosted" src="https://img.shields.io/badge/deployment-self--hosted-33A67C?style=flat-square"></a>
  <a href="docs/ARCHITECTURE.md"><img alt="Engine: Firecracker" src="https://img.shields.io/badge/engine-Firecracker-6D8CFF?style=flat-square"></a>
  <a href="go.mod"><img alt="Go 1.26.6" src="https://img.shields.io/badge/Go-1.26.6-00ADD8?style=flat-square&logo=go&logoColor=white"></a>
  <a href="LICENSE"><img alt="License: Apache 2.0" src="https://img.shields.io/badge/license-Apache--2.0-33A67C?style=flat-square"></a>
</p>

---

Brezel gives agents a stateful Linux computer without making you manage
Firecracker, network namespaces, guest credentials, or cleanup. Workspaces
survive disposable compute, previews put results in front of a human, and agent
commands never run on your laptop.

<p align="center">
  <img src="assets/demos/brezel-tour.gif" width="900" alt="A real Brezel terminal session creating an isolated sandbox, running an agent task, replacing the compute, and recovering the durable result">
</p>

<p align="center"><sub>Recorded against a qualified Firecracker host with the maintained <a href="examples/demos/readme-tour.sh">demo script</a>. Identifiers are shortened; commands and results are real.</sub></p>

```console
$ brezel new --ttl 900 --standby-after 120
sbx_01...    running

$ brezel run sbx_01... python -c 'print(6 * 7)'
42

$ brezel stop sbx_01...
standby
```

<table>
  <tr>
    <td><strong>Stateful</strong><br>Stop compute. Keep the workspace. Resume when the agent returns.</td>
    <td><strong>Isolated</strong><br>Run untrusted code inside a Firecracker microVM, never on the caller's machine.</td>
    <td><strong>Inspectable</strong><br>Stream commands, move files, and open short-lived previews for human review.</td>
  </tr>
</table>

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

Until the first signed binary release exists, build the runtime from a reviewed
source revision. The Python and TypeScript SDKs are published independently.

## A 60-second tour

```console
$ SANDBOX="$(brezel new --ttl 900 | cut -f1)"
$ brezel put "$SANDBOX" /workspace/input.json ./input.json
$ brezel run --cwd /workspace "$SANDBOX" python3 -c \
    'import json; print(json.load(open("input.json"))["answer"])'
42

$ brezel run "$SANDBOX" sh -lc \
    'nohup python3 -m http.server 3000 --directory /workspace >/tmp/http.log 2>&1 &'
$ brezel open --ttl 300 "$SANDBOX" 3000
https://sandbox.example.com/p/7B7.../    expires 2026-09-17T12:05:00Z

$ brezel stop "$SANDBOX"
$ brezel start "$SANDBOX"
$ brezel run "$SANDBOX" cat /workspace/input.json
{"answer":42}
$ brezel delete "$SANDBOX"
```

Run the maintained version of this walkthrough with
[`examples/terminal-tour.sh`](examples/terminal-tour.sh) on a qualified host.

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

## Competitive on useful work

The ComputeSDK DAX benchmark runs a cold clone, dependency install, and full
OpenCode typecheck inside a fresh sandbox. Lower is better.

| Provider | Total | Evidence |
| --- | ---: | --- |
| Isorun | 32.09 s | Public leaderboard |
| **Brezel** | **36.27 s** | Five-run self-run median, exact upstream workload |
| Blaxel | 46.44 s | Public leaderboard |
| Daytona | 75.70 s | Public leaderboard |
| Modal | 94.59 s | Public leaderboard |

Snapshot: 2026-09-19, using the public leaderboard's 2026-09-18 run. Brezel's
number is retained, checksummed named-host evidence, but it is not an official
rank. Independent inclusion is pending in
[ComputeSDK PR #791](https://github.com/computesdk/computesdk/pull/791). See the
[source snapshot](examples/demos/dax-comparison-2026-09-19.json), the
[reproducible terminal comparison](examples/demos/dax-comparison.sh), and the
[benchmark claim boundary](docs/BENCHMARKING.md) before quoting these numbers.

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

## Python and TypeScript SDKs

The first-party SDKs cover the supported fast path: create, run, upload,
download, preview, standby, resume, list, and confirmed cleanup. They reject
remote plaintext endpoints, can read credentials from protected files, do not
follow redirects, and never retry a command or file write after an ambiguous
transport failure.

Install either SDK directly from its public registry:

```console
pip install brezel-sdk
npm install @infercrane/brezel
```

<details open>
<summary><strong>Python</strong></summary>

```python
from brezel import BrezelClient

client = BrezelClient.from_token_file(
    ".brezel/secrets/service.token",
    base_url="http://127.0.0.1:8080",
)

with client.create_sandbox(ttl_seconds=900) as sandbox:
    result = sandbox.run(
        ["python3", "-c", "print('hello from a microVM')"],
        check=True,
    )
    print(result.stdout_text, end="")
    sandbox.write_file("/workspace/task.txt", b"ship it\n")
    sandbox.run([
        "sh", "-lc",
        "nohup python3 -m http.server 3000 --directory /workspace >/tmp/http.log 2>&1 &",
    ], check=True)
    print(sandbox.preview(3000))
```

</details>

<details>
<summary><strong>TypeScript</strong></summary>

```typescript
import { BrezelClient } from "@infercrane/brezel";

const client = BrezelClient.fromTokenFile(
  ".brezel/secrets/service.token",
  { baseUrl: "http://127.0.0.1:8080" },
);

const sandbox = await client.createSandbox({ ttlSeconds: 900 });
try {
  const result = await sandbox.run(
    ["python3", "-c", "print('hello from a microVM')"],
    { check: true },
  );
  console.log(result.stdoutText);
  await sandbox.writeFile("/workspace/task.txt", "ship it\n");
  await sandbox.run([
    "sh", "-lc",
    "nohup python3 -m http.server 3000 --directory /workspace >/tmp/http.log 2>&1 &",
  ], { check: true });
  console.log(await sandbox.preview(3000));
} finally {
  await sandbox.delete();
}
```

</details>

Package source, metadata, compatibility tests, and release automation live
under [`sdk/`](sdk/). See the package READMEs for supported runtimes and the
current publication state. Maintainers follow the tokenless
[SDK release procedure](docs/SDK-RELEASE.md).

## Practical examples

- [`terminal-tour.sh`](examples/terminal-tour.sh): create, files, command,
  preview, standby, resume, cleanup.
- [`coding-agent-sandbox.yaml`](examples/coding-agent-sandbox.yaml): intended
  coding-agent capabilities and lifecycle.
- [`sandbox-with-workspace.json`](examples/sandbox-with-workspace.json): attach
  durable state to disposable compute.
- [`private-model-connector.yaml`](examples/private-model-connector.yaml): keep
  an upstream bearer credential outside the guest.

The [GCP Terraform example](deploy/terraform/gcp-evaluation) provisions a
disposable Ubuntu host with nested KVM, SSD storage, a dedicated VPC, and SSH
restricted to operator CIDRs:

```console
cd deploy/terraform/gcp-evaluation
terraform init
terraform apply \
  -var="project_id=$GOOGLE_CLOUD_PROJECT" \
  -var='ssh_source_ranges=["203.0.113.8/32"]'
```

It deliberately does not expose the Brezel API, attach a cloud service account,
install mutable source, or claim the host is qualified. Clone the exact release
revision on that machine and run `make qualify-single-host` before use.

## What ships today

| Area | Capability |
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
WebSocket transport, public full-state forks, resumable SDK stream attachment,
multi-node scheduling, replicated state, OIDC/RBAC, GPU passthrough, and
hostile shared-multitenant assurance. The [roadmap](docs/ROADMAP.md) defines the
gates for those capabilities.

## How it works

```mermaid
flowchart LR
    client["CLI / HTTP"] --> api["Brezel API<br/>policy · lifecycle · state"]
    api -->|"mTLS + one-operation capability"| node["node relay"]
    api -->|"lifecycle"| engine["pinned Firecracker engine"]
    node --> guest["isolated guest"]
    engine --> guest
    guest --- workspace[("durable workspace")]
```

Brezel owns the public contract, lifecycle, policy, data paths, packaging,
qualification, and evidence. The initial engine distribution uses pinned,
locally patched Apache-2.0 E2B Runtime components. Brezel does not use E2B
Cloud and does not require an E2B API key. The dependency and replacement
boundary are documented in [ADR 0002](docs/decisions/0002-runtime-substrate.md).
The full [current architecture](docs/ARCHITECTURE.md#current-single-host-architecture)
separates the lifecycle and guest-data paths and marks every trust boundary.

## Deployment

Brezel is designed for self-hosted private deployment on dedicated KVM hosts.
Before exposing an installation, follow the [go-live checklist](docs/GO-LIVE.md)
and qualify the exact revision on its deployment host. Fleet, high-availability,
and shared-multitenant profiles have separate gates in
[Status](docs/STATUS.md); they are not silently implied by the single-host
package.

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
- [Go-live checklist](docs/GO-LIVE.md)
- [SDK release](docs/SDK-RELEASE.md)
- [Roadmap](docs/ROADMAP.md)

## Contributing and security

Read [CONTRIBUTING.md](CONTRIBUTING.md) before changing a trust, lifecycle, or
API boundary. Report vulnerabilities privately according to
[SECURITY.md](SECURITY.md), never through a public issue.

Brezel is licensed under [Apache 2.0](LICENSE). Third-party attributions cover
the pinned [runtime patches](third_party/e2b-runtime/NOTICE.md) and the narrow
[guest protocol subset](third_party/e2b/NOTICE.md).
