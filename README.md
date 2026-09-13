# Open Agent Runtime Lab

Architecture and contract prototype for a self-hosted runtime for long-running AI
agents.

> Status: pre-implementation design. Nothing here is production-safe yet.

The project name and API namespace are provisional. The repository is deliberately
separate from InferCrane so the runtime can become a useful open-source product on
its own and a future infrastructure primitive for inference platforms.

## One-sentence product

Run stateful agents in fast microVM sandboxes with durable workspaces, controlled
network access, private model connectivity, and resumable execution on your own
infrastructure.

The product should feel like one small SDK:

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

Blaxel demonstrated that agent infrastructure is not merely `docker run`: an
agent needs isolated compute, state that survives idle periods, fast resume,
networking, and shared artifacts. Its acquisition by Baseten also makes the
strategic direction clear: agent execution and inference are converging.

We should not recreate Blaxel's proprietary implementation byte for byte, nor
build a Firecracker fleet from an empty repository. The open-source E2B Runtime
already provides the strongest available substrate for this shape of product:
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
| **Image** | Turn an OCI image into a reproducible boot template | E2B template builder and immutable artifacts |
| **Sandbox** | Create, exec, inspect, expose a port, pause, resume, fork, destroy | E2B Firecracker runtime and guest `envd` |
| **Volume** | Keep one sandbox's durable working state beyond a VM lifetime | E2B persistent volume capability |
| **Drive** | Share a durable POSIX workspace across agents | Later milestone; evaluate JuiceFS rather than inventing a filesystem |
| **Job** | Fan out bounded work, retry safely, collect results, cancel | Project-owned durable job coordinator |
| **Model route** | Reach an approved private or managed model by alias | E2B egress/workload identity plus a project route gateway |
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

Blaxel reports approximately 25 ms resume on its custom bare-metal runtime. That
number depends on snapshot format, local caches, CPU compatibility, networking,
and fleet placement. It is not an acceptable MVP promise for this project.

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
- API-compatible with Blaxel unless a public conformance suite proves it.

## Repository map

- [`docs/PRODUCT.md`](docs/PRODUCT.md): users, jobs, UX, and positioning
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md): control plane, data plane, and state
- [`docs/BLAXEL-GAP-MAP.md`](docs/BLAXEL-GAP-MAP.md): what to reproduce, reuse, or defer
- [`docs/THREAT_MODEL.md`](docs/THREAT_MODEL.md): isolation and enterprise boundaries
- [`docs/MILESTONES.md`](docs/MILESTONES.md): build sequence and exit criteria
- [`docs/MVP-API.md`](docs/MVP-API.md): initial resources and lifecycle semantics
- [`docs/RESEARCH-2026.md`](docs/RESEARCH-2026.md): current projects and papers
- [`docs/UPSTREAM-AUDIT.md`](docs/UPSTREAM-AUDIT.md): pinned substrate inspection
- [`docs/PRIVACY-COMMERCIALIZATION.md`](docs/PRIVACY-COMMERCIALIZATION.md): license,
  privacy, product boundary, and commercial model
- [`spec/v1alpha1/sandbox.schema.json`](spec/v1alpha1/sandbox.schema.json): lifecycle contract
- [`spec/v1alpha1/run.schema.json`](spec/v1alpha1/run.schema.json): bounded job-run contract
- [`docs/decisions/`](docs/decisions): architectural decisions

## License

Apache-2.0 is the proposed license. Every bundled component and distribution
artifact still needs a dependency and trademark review before public release.
