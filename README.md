# Open Agent Runtime Lab

Architecture and contract prototype for a self-hosted runtime for long-running AI
agents.

> Status: early control-plane implementation. It is not production-safe or a
> qualified hostile shared-multitenant service yet.

The project name and API namespace are provisional. The repository is deliberately
separate from InferCrane so the runtime can become a useful open-source product on
its own and a future infrastructure primitive for inference platforms.

## One-sentence product

Run stateful agents in fast microVM sandboxes with durable workspaces, controlled
network access, private model connectivity, and resumable execution on your own
infrastructure.

The future SDK should feel like one small surface:

```python
sandbox = client.sandboxes.create(
    image="python:3.13",
    memory_mb=4096,
    standby_after="30s",
    expires_after="24h",
)

sandbox.files.write("/workspace/task.md", task)
result = sandbox.processes.run("python /workspace/agent.py")
preview = sandbox.ports.url(3000)
```

The operator may configure isolation, placement, egress, credentials, storage,
and inference routes. Application developers should not have to understand the
control plane to run an agent.

## Why build this now

Agent infrastructure is no longer merely `docker run`. An agent needs isolated
compute, state that survives idle periods, fast resume, networking, shared
artifacts, and controlled access to tools and models. Agent execution,
inference, evaluation, and training workflows are converging around the same
durable compute primitive.

We should not build a Firecracker fleet from an empty repository. The
open-source E2B Runtime already provides a strong candidate substrate:
Firecracker sandboxes, templates, pause/resume, fork, volumes, a guest daemon,
node orchestration, a client proxy, and self-hosted deployment.

The proposed project is an opinionated open-source distribution around that
substrate. It closes the remaining product gap for private and enterprise agent
workloads:

- a coherent single-host and clustered installer;
- durable volumes and, later, shared workspaces;
- jobs and fan-out execution;
- a qualified deny-by-default network and credential profile over upstream
  runtime hooks;
- first-class routes to private or open-weight inference;
- tenant policy, quotas, audit, and air-gapped operation; and
- signed execution receipts for debugging and governance.

This is not an E2B rebrand. Upstream runtime capability stays upstream whenever
possible. This repository owns the distribution, missing control-plane features,
enterprise boundary, and conformance profile.

## Product primitives

| Primitive | User promise | Initial implementation |
| --- | --- | --- |
| **Environment** | Resolve software, resources, tools, connectors, and policy into one reproducible revision | Project manifest plus E2B template builder |
| **Sandbox** | Create, exec, inspect, expose a port, pause, resume, fork, destroy | E2B Firecracker runtime and guest `envd` |
| **Workspace** | Keep working state beyond compute; single-writer first | E2B volume capability; shared mode later |
| **Checkpoint** | Restore or fork explicit filesystem or full machine state | E2B snapshots plus project lineage and policy |
| **Job** | Fan out bounded work, retry safely, collect results, cancel | Project-owned durable job coordinator |
| **Connector** | Reach an approved tool or private model without possessing its credential | Project broker over E2B egress/workload identity |
| **Rollout** | Run reproducible agent episodes across model routes and evaluators | Project-owned job specialization |
| **Policy** | Limit image, network, data, resources, region, and lifetime | Project-owned admission and organization policy |
| **Receipt** | Explain exactly what ran and what controls applied | Project-owned signed, content-minimal record |

## Architecture at a glance

```text
SDK / CLI / OpenAI Agents adapter / Anthropic adapter
                         |
                  public API gateway
                         |
        +----------------+----------------+
        |                                 |
   sandbox service                  durable job service
        |                                 |
        +------------ scheduler ----------+
                         |
                 regional data plane
          +--------------+--------------+
          |              |              |
     E2B node       E2B node       E2B node
   Firecracker     Firecracker     Firecracker
       + envd          + envd          + envd
          |              |              |
          +------ private networking ---+
                         |
          egress + credentials + model routes

Control state: PostgreSQL     Live routing/leases: Redis
Images/snapshots/artifacts: S3-compatible object storage
Telemetry: OpenTelemetry; ClickHouse is optional at scale
```

Read the full design in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

## What runs now

The repository includes a small Go control plane with no third-party Go
dependencies. It implements immutable Environment registration; tenant-scoped
Sandbox create, inspect, pause, resume, filesystem checkpoint, expiration, and
delete; durable idempotency and lifecycle events; an audited E2B Runtime HTTP
adapter; Ed25519-signed DSSE lifecycle receipts; and a narrow private-model/tool
connector preview that keeps long-lived credentials outside the sandbox.

Production execution fails closed unless `RUNTIME_BACKEND=e2b` and the required
backend, service-token, state, and receipt-key configuration are present. The
only fake backend exists in `_test.go` files and cannot be enabled in a release
binary. Connector attachment is available only when its gateway and 0600 file
resolver are explicitly configured. The current bearer-renewal design remains a
trusted-operator preview, not a production shared-tenant claim.

```bash
make check
mkdir -p runtime-state
go run ./cmd/runtime-api keygen -out runtime-state/receipt.key

# Copy config/runtime.env.example into your secret/configuration system.
# Never commit the substituted values.
go run ./cmd/runtime-api
```

The configurable `E2B_API_URL` may target the managed API or a separately
deployed self-hosted E2B Runtime control API. Remote plaintext endpoints and HTTP
redirects are rejected so the backend API key cannot silently leave its origin.

See [`docs/IMPLEMENTATION-STATUS.md`](docs/IMPLEMENTATION-STATUS.md) for the
precise availability boundary.

The conformance command creates and deletes a real sandbox, so it requires an
explicit execution flag. It checks the current control API lifecycle,
idempotency, cross-project denial, filesystem checkpoint, cleanup, and signed
receipt. It does not qualify the host isolation boundary or performance:

```bash
RUNTIME_SERVICE_TOKEN=... go run ./cmd/runtime-conformance \
  -base-url https://runtime.example.com \
  -backend-template existing-template-id \
  -project isolated-conformance-project \
  -target staging-linux-kvm-2026-09-13 \
  -execute
```

## What ships first

The first useful release is a single-host Linux distribution for a trusted
operator. It runs Firecracker through E2B Runtime and provides:

1. image/template creation;
2. sandbox create, exec, files, logs, ports, pause, resume, and destroy;
3. idle-to-standby and independent expiration;
4. one durable volume;
5. a qualified deny-by-default network profile using upstream enforcement;
6. approved private model routes without placing provider credentials in the VM;
7. lifecycle audit and an optional signed receipt; and
8. a conformance and benchmark command that publishes observed results.

The preview runs on Linux with KVM. macOS can run the CLI and control plane, but
it is not the initial Firecracker host. Cluster scheduling, shared drives,
multi-region migration, GPU passthrough, and hardware attestation come later.

## Honest performance targets

Resume and creation latency depend on snapshot format, local caches, CPU
compatibility, networking, and fleet placement. A number measured by another
system is not an acceptable MVP promise for this project.

For the first release we will publish a reproducible benchmark suite and report
p50, p95, and p99 for:

- cached template create;
- warm exec;
- idle snapshot;
- resume on the same node;
- first byte through a preview URL; and
- first model request through a private route.

Initial engineering budgets are under 250 ms p50 for a cached create and under
100 ms p50 for same-node resume on qualified bare metal. These are design budgets,
not claims, until reproduced in CI or a published test environment.

## Product boundary

This project is:

- a self-hosted agent compute plane;
- a lifecycle, storage, network, identity, and policy system around microVMs;
- compatible with agent frameworks through adapters; and
- designed to place agent execution close to private inference.

It is not initially:

- an inference server or model-training platform;
- a globally operated public cloud;
- a browser IDE;
- a promise of hostile multi-tenant GPU safety;
- a custom distributed filesystem;
- a clean-room rewrite of Firecracker or E2B Runtime; or
- a blanket compatibility promise for proprietary provider APIs.

## Repository map

- [`docs/PRODUCT.md`](docs/PRODUCT.md): users, jobs, UX, and positioning
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md): control plane, data plane, and state
- [`docs/STRATEGY-2026.md`](docs/STRATEGY-2026.md): source-backed product and engineering plan
- [`docs/CAPABILITY-MAP.md`](docs/CAPABILITY-MAP.md): what to build, integrate, qualify, or defer
- [`docs/THREAT_MODEL.md`](docs/THREAT_MODEL.md): isolation and enterprise boundaries
- [`docs/MILESTONES.md`](docs/MILESTONES.md): build sequence and exit criteria
- [`docs/MVP-API.md`](docs/MVP-API.md): initial resources and lifecycle semantics
- [`docs/RESEARCH-2026.md`](docs/RESEARCH-2026.md): current projects and papers
- [`docs/UPSTREAM-AUDIT.md`](docs/UPSTREAM-AUDIT.md): pinned substrate inspection
- [`docs/PRIVACY-COMMERCIALIZATION.md`](docs/PRIVACY-COMMERCIALIZATION.md): license,
  privacy, product boundary, and commercial model
- [`docs/IMPLEMENTATION-STATUS.md`](docs/IMPLEMENTATION-STATUS.md): implemented,
  unavailable, and unqualified capabilities
- [`spec/v1alpha1/sandbox.schema.json`](spec/v1alpha1/sandbox.schema.json): lifecycle contract
- [`spec/v1alpha1/run.schema.json`](spec/v1alpha1/run.schema.json): bounded job-run contract
- [`docs/decisions/`](docs/decisions): architectural decisions

## Contributing and license

The project is licensed under [Apache-2.0](LICENSE). Read
[CONTRIBUTING.md](CONTRIBUTING.md) before changing runtime behavior. Every
bundled component and distribution artifact still needs a dependency and
trademark review before a public binary distribution.
