# Open Agent Runtime

Architecture and contract prototype for a self-hosted runtime for long-running AI
agents.

> Status: hardened single-host release candidate. It is not Blaxel-scale and
> is not yet qualified for hostile shared multitenancy or production
> availability guarantees. A named Linux/KVM host must pass the destructive
> qualification workflow before private evaluation.

The project name and API namespace are provisional. The repository is deliberately
separate from InferCrane so the runtime can become a useful open-source product on
its own and a future infrastructure primitive for inference platforms.

## One-sentence product

Run stateful agents in fast microVM sandboxes with durable workspaces, controlled
network access, private model connectivity, and resumable execution on your own
infrastructure.

The product wedge is **branchable, evidence-bearing agent trials**: checkpoint
one trusted state, fork isolated attempts across approved model/tool routes and
hard budgets, retain the useful branch, and verify the environment, policy,
lineage, outcome, and cleanup without collecting customer content by default.
That full loop is the north star, not a claim about the current release; see the
[implementation boundary](docs/IMPLEMENTATION-STATUS.md) and
[product claims ladder](docs/PRODUCT.md#claims-ladder).

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

The operator may configure isolation, egress, credentials, storage, and model
routes. Application developers use one runtime API and do not select an
underlying substrate.

## Why build this now

Agent infrastructure is no longer merely `docker run`. An agent needs isolated
compute, state that survives idle periods, fast resume, networking, shared
artifacts, and controlled access to tools and models. Agent execution,
inference, evaluation, and training workflows are converging around the same
durable compute primitive.

The product owns the sandbox API, CLI, lifecycle contract, data paths,
installation, policy, conformance, and enterprise operating profile. It uses
audited Apache-licensed Firecracker runtime components as an internal engine,
just as infrastructure products use KVM or containerd without making them the
customer API. The default install is self-contained on the operator's Linux
host and requires no managed sandbox account.

The product closes the remaining gap for private and enterprise agent
workloads:

- a coherent single-host installer and, later, a clustered installer;
- single-writer durable workspaces and, later, concurrent shared drives;
- jobs and fan-out execution;
- a qualified deny-by-default network and credential profile over upstream
  runtime hooks;
- first-class routes to private or open-weight inference;
- tenant policy, quotas, audit, and air-gapped operation; and
- signed execution receipts for debugging and governance.

Upstream runtime capability stays upstream whenever possible. This repository
owns the integrated distribution, stable contract, missing runtime features,
enterprise boundary, and conformance profile.

## Product primitives

| Primitive | User promise | Initial implementation |
| --- | --- | --- |
| **Environment** | Resolve software, resources, tools, connectors, and policy into one reproducible revision | Immutable template revision |
| **Sandbox** | Create, exec, transfer files, expose an authenticated HTTP port, pause, resume, and destroy | Product API over a local Firecracker engine |
| **Workspace** | Keep working state beyond compute; single-writer first | Independent host-backed volume with explicit create, attach, detach, and delete |
| **Checkpoint** | Capture explicit filesystem state | Filesystem checkpoint with project lineage |
| **Job** | Fan out bounded work, retry safely, collect results, cancel | Project-owned durable job coordinator |
| **Connector** | Reach an approved tool or private model without possessing its credential | Project broker over E2B egress/workload identity |
| **Rollout** | Run reproducible agent episodes across model routes and evaluators | Project-owned job specialization |
| **Policy** | Limit image, network, data, resources, region, and lifetime | Project-owned admission and organization policy |
| **Receipt** | Explain exactly what ran and what controls applied | Project-owned signed, content-minimal record |

## Architecture at a glance

```text
SDK / CLI / agent framework adapters
                         |
                  public API gateway
                         |
        +----------------+----------------+
        |                                 |
   runtime service                  durable job service
        |                                 |
        +------------ scheduler ----------+
                         |
                 regional data plane
          +--------------+--------------+
          |              |              |
   microVM worker microVM worker microVM worker
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

The repository implements a real single-host runtime path: immutable
environments; project-scoped sandbox create, inspect, pause, resume, checkpoint,
expiration, and delete; independent single-writer workspaces that survive
sandbox replacement; streamed process execution; bounded file upload and
download; short-lived authenticated HTTP previews; durable idempotency and
lifecycle events; Ed25519-signed DSSE lifecycle receipts; and a narrow private
model/tool connector preview that keeps long-lived credentials outside the VM.

The release binary has no fake or container fallback. It talks only to the
bundled local microVM engine by default, reads all service and engine credentials
from protected files, rejects remote plaintext engines, and disables redirects
on credentialed internal requests.

Production-style authentication uses a protected access-policy file containing
only SHA-256 token digests bound to explicit project IDs. A credential cannot
select an arbitrary tenant through `X-Project-ID`. The trusted-operator bearer
mode is disabled unless explicitly enabled for an isolated development host.
Per-project sandbox, workspace, environment, connector, and live guest-operation
limits fail before backend mutation. A process-wide admission ceiling rejects
overload while keeping health and readiness available. Every response carries a
generated request ID; enabled access logs contain only route patterns and
content-free timing/status metadata, and `/metrics` exports process counters plus
fixed-dimension lifecycle and guest phase histograms without tenant, sandbox,
path, command, or content labels.

```bash
make check
install -d -m 700 runtime-state
openssl rand -hex 32 > runtime-state/service.token
chmod 600 runtime-state/service.token
go run ./cmd/runtime-api keygen -out runtime-state/receipt.key

# For manual local development only. Production-style installs use the
# generated project-binding access policy instead.
export RUNTIME_TRUSTED_OPERATOR_MODE=true

# Copy config/runtime.env.example into your secret/configuration system.
# Never commit the substituted values.
go run ./cmd/runtime-api
```

On an Ubuntu 24.04 host with KVM, the integrated installer pins and starts the
local engine, generates local credentials, builds the runtime API, and waits for
health:

```bash
go build -o bin/runtimectl ./cmd/runtimectl
bin/runtimectl doctor
./deploy/single-host/install.sh
RUNTIME_SERVICE_TOKEN_FILE=.runtime/secrets/service.token bin/runtimectl \
  environment create --name base --template base
```

The installer fails before host mutation when its Linux/KVM, TUN, Docker,
Compose, Buildx, Git, archive, patch, hashing, or key-generation prerequisites are
absent. It builds the pinned engine patch locally and runs the destructive
deployment conformance suite, restarts the controller, and runs the suite again
before reporting success. Content-free JSON reports are retained under
`.runtime/qualification/`. The upstream engine
revision, patch digest, and license are pinned in
[`deploy/single-host/engine.lock`](deploy/single-host/engine.lock).
Engine images and privileged host artifacts are independently digest-locked;
successful installation retains the exact identities in a local distribution
manifest. Operator source, image, and artifact mirror options are documented
in [`docs/ARTIFACT-SUPPLY-CHAIN.md`](docs/ARTIFACT-SUPPLY-CHAIN.md). These
controls make the distribution mirrorable, but they do not yet constitute an
air-gapped support claim.

See [`docs/IMPLEMENTATION-STATUS.md`](docs/IMPLEMENTATION-STATUS.md) for the
precise availability boundary.

The conformance command creates and deletes a real sandbox, so it requires an
explicit execution flag. It checks the current control API lifecycle,
idempotency, cross-project denial, filesystem checkpoint, durable workspace
persistence and exclusivity, cleanup, and signed receipt. It does not qualify
the host isolation boundary or performance:

```bash
RUNTIME_SERVICE_TOKEN_FILE=./runtime-state/service.token go run ./cmd/runtime-conformance \
  -base-url https://runtime.example.com \
  -backend-template base \
  -project isolated-conformance-project \
  -target staging-linux-kvm-2026-09-13 \
  -execute
```

## Current release boundary

The release candidate runs on a dedicated Linux/KVM host for one organization.
macOS
can build and run the CLI and API tests but cannot qualify the Firecracker host
boundary. Project-bound credentials and count/concurrency quotas are enforced;
control state is schema-versioned and its project/resource indexes are checked
before open and commit;
OIDC, fine-grained roles, replicated storage, and multi-node scheduling are not.
Root filesystem state survives standby. Independent host-backed
workspaces survive sandbox replacement and are single-writer; concurrent shared
drives are not available. Template builds from arbitrary OCI images,
PTY/WebSocket transport, fork, jobs, rollouts, clustered scheduling,
multi-region recovery, GPU passthrough, OIDC/RBAC, and hardware attestation
remain explicit follow-on work.

Do not call this deployment “Blaxel-scale.” That claim requires replicated
control state, a worker fleet across failure domains, external identity and KMS,
workspace backup/restore, rolling-upgrade and disaster-recovery exercises,
load/soak results, and an independent isolation review. See
[`docs/PRODUCTION-READINESS.md`](docs/PRODUCTION-READINESS.md).

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

The native `sandbox-bench` command separately measures create through a verified
first instruction, warm command execution, resume through a verified
instruction, filesystem checkpoint, filesystem restore through exact state
verification, authenticated preview first byte, warm preview first byte, and
durable-workspace I/O. It supports sequential, finite open-loop staggered, and
burst arrivals and writes every raw attempt plus its summary as JSON. Cleanup is
excluded from user-visible latency but reported per attempt and per sandbox,
checkpoint, or workspace resource.

```bash
go build -trimpath -o bin/sandbox-bench ./cmd/sandbox-bench
RUNTIME_SERVICE_TOKEN_FILE=.runtime/secrets/service.token bin/sandbox-bench \
  -base-url https://runtime.example.com \
  -project isolated-benchmark-project \
  -backend-template base \
  -target single-host-linux-kvm-01 \
  -runtime-revision REPLACE_WITH_EXACT_GIT_REVISION \
  -evidence-class single-host-linux-kvm \
  -cache-state cached-template \
  -scenario tti \
  -mode sequential \
  -runs 100 \
  -max-in-flight 1 \
  -execute > tti-sequential.json
```

Run each scenario and load shape separately. A cache state is an operator
declaration, not something the harness infers or resets. Do not merge startup
TTI with repository-build or CPU-throughput scores; the
[world-class gap audit](docs/WORLD_CLASS_GAP_AUDIT.md#gate-a-reproducible-single-node-performance-evidence)
defines the complementary real-workload suite.

The first published clean-host run and post-reboot safety samples are in
[`docs/QUALIFICATION-2026-09-13.md`](docs/QUALIFICATION-2026-09-13.md). On its
older four-core Xeon, the initial cached-create and same-node-resume p50 values
were 371 ms and 228 ms. After liveness hardening and sustained destructive load,
the final sample measured 599 ms and 507 ms. The original 250 ms and 100 ms
design budgets were not met, host variance was material, and none of these
environment-specific observations are portable product claims.

## Product boundary

This project is:

- a self-hosted agent runtime;
- a lifecycle, storage, network, identity, and policy system around microVMs;
- compatible with agent frameworks through adapters; and
- designed to place agent execution close to private inference.

It is not initially:

- an inference server or model-training platform;
- a globally operated public cloud;
- a browser IDE;
- a promise of hostile multi-tenant GPU safety;
- a custom distributed filesystem;
- a new hypervisor or clean-room Firecracker orchestration rewrite; or
- a blanket compatibility promise for proprietary provider APIs.

## Repository map

- [`docs/PRODUCT.md`](docs/PRODUCT.md): users, jobs, UX, and positioning
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md): runtime services, execution plane, and state
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
