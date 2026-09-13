# Architecture

## Design principles

1. **Use a proven microVM data plane.** Build on the Apache-2.0 E2B Runtime and
   upstream missing primitives rather than starting with a Firecracker fork.
2. **Session state and compute lifetime are different.** An idle sandbox may
   stop consuming CPU while its checkpoint and explicit storage remain.
3. **One API, honest states.** Active, standby, archived, expired, deleting, and
   failed are different conditions with durable transitions.
4. **Storage is explicit.** Root filesystem snapshots, single-writer volumes,
   shared drives, and immutable artifacts have different guarantees.
5. **Private by default.** No public ingress, unrestricted egress, ambient cloud
   identity, or long-lived sandbox credentials.
6. **Inference is local infrastructure.** A model route is a typed resource that
   can be placed near the sandbox without making this project a model server.
7. **Measure before claiming.** Startup, resume, density, durability, and
   isolation claims must name a tested release and environment.

## Logical components

```text
                                SDK / CLI
                                    |
                            +-------v--------+
                            | API + identity |
                            +-------+--------+
                                    |
               +--------------------+--------------------+
               |                    |                    |
       +-------v--------+   +-------v--------+   +-------v--------+
       | sandbox service|   | job coordinator|   | image service  |
       +-------+--------+   +-------+--------+   +-------+--------+
               |                    |                    |
               +--------------------+--------------------+
                                    |
                         +----------v----------+
                         | scheduler + policy  |
                         +----------+----------+
                                    |
              regional placement + durable operation record
                                    |
        +---------------------------+---------------------------+
        |                           |                           |
+-------v--------+          +-------v--------+          +-------v--------+
| worker node A  |          | worker node B  |          | worker node C  |
| E2B orchestrator|         | E2B orchestrator|         | E2B orchestrator|
| Firecracker VMs|          | Firecracker VMs|          | Firecracker VMs|
| snapshot cache |          | snapshot cache |          | snapshot cache |
+-------+--------+          +-------+--------+          +-------+--------+
        |                           |                           |
        +---------------------------+---------------------------+
                                    |
       +----------------------------+----------------------------+
       |                            |                            |
+------v-------+             +------v-------+             +------v-------+
| client proxy |             | egress/model|             | storage      |
| ports + wake |             | gateway     |             | gateway      |
+--------------+             +--------------+             +--------------+
```

### Upstream E2B Runtime

Use E2B's control API, per-node orchestrator, Firecracker virtual machines,
template builder, guest `envd`, client proxy, snapshot mechanics, and persistent
volume support. Pin one upstream release or commit per distribution release.

Changes that improve generic lifecycle or isolation should be contributed
upstream first. A maintained patch set is acceptable during incubation. A hard
fork requires an ADR explaining why compatibility and maintenance cost are
worth it.

### Project control plane

The project adds the opinionated product surface and missing system behavior:

- organization, project, tenant, role, and service-account identity;
- declarative sandbox and job APIs;
- lifecycle policy, TTL, quotas, placement, and cleanup operations;
- network and model-route admission;
- durable job fan-out, cancellation, retries, and result aggregation;
- execution receipts and content-minimal audit;
- self-host packaging, upgrades, backup, and air-gap support; and
- conformance and performance qualification.

The facade initially maps to stable E2B APIs. It does not hide E2B states or
invent success when the substrate reports an unknown condition.

### Guest API

The E2B guest daemon remains the authority for process, filesystem, terminal,
and port operations inside a sandbox. The initial SDK should use that surface
rather than install a second privileged agent.

Project metadata is attached outside the guest. If a guest extension becomes
necessary, it must be unprivileged where possible, versioned independently, and
included in conformance and receipts.

## Primary resources

### Image

An OCI reference plus build instructions resolves to an immutable template
manifest. The manifest binds container digest, kernel, init/guest version,
platform, build provenance, and snapshot artifacts. Mutable image tags are
allowed only as inputs to a build; a sandbox always launches a resolved digest.

### Sandbox

A sandbox has stable identity and an explicit lifecycle:

```text
requested -> preparing -> running <-> standby
                        \-> failed

running/standby -> archiving -> archived -> restoring -> running
running/standby/archived -> deleting -> deleted
any live state -> expired -> deleting
```

- **Running:** the microVM is allocated and processes may execute.
- **Standby:** compute is released; checkpoint state is resumable.
- **Archived:** checkpoint artifacts are moved to durable, lower-cost storage and
  are not expected to resume at interactive latency.
- **Expired:** policy forbids further resume; cleanup is pending or running.
- **Deleted:** runtime, routes, leases, snapshots, and attachments are gone or
  a terminal cleanup failure is explicitly recorded.

An idle timer may move `running` to `standby`. It never changes expiration. A
connection may trigger resume only after identity, policy, and quota are
revalidated.

### Snapshot

An explicit checkpoint captures VM memory, device state, and root filesystem
state using the substrate's supported mechanism. A snapshot is immutable and can
be used for resume, rollback, or fork only on a compatible runtime and CPU
profile.

Snapshot locality is a scheduling input. Cross-node restore may be slower than
same-node resume and must be measured separately. Cross-region replication is a
later storage workflow, not live migration.

### Volume

A durable volume is a single-writer workspace attached to one sandbox at a time.
Its lifetime is independent from the sandbox and deletion is explicit.

### Drive

A drive is a concurrent shared workspace with semantics that differ from a
volume. It requires coherency, authorization, quota, backup, and failure tests.
The first implementation should evaluate JuiceFS over S3-compatible object
storage and PostgreSQL or Redis metadata. Do not expose it as production-ready
until rename, fsync, locking, partial-failure, and tenant-isolation tests pass.

### Job

A job is a durable desired state for one or many bounded runs. It stores input
references, concurrency, retry policy, timeout, result policy, and cancellation
intent. Each attempt receives a distinct sandbox and operation identity.

Cancellation is durable. A cancelled client connection is not cancellation.
Retries never reuse a potentially contaminated sandbox unless the job explicitly
starts from a qualified checkpoint.

### Model route

A model route maps a stable alias to an approved OpenAI-compatible or later
protocol endpoint. The route contains model allowlists, placement, credential
handle, request limits, and optional budget controls.

The sandbox sees a local or private route and a short-lived workload identity.
The egress gateway resolves the real endpoint and injects the credential only
after policy checks. Prompt and response content is not logged by default.

## Request flows

### Create and use a sandbox

1. Authenticate caller and resolve organization/project.
2. Validate the sandbox request and organization policy.
3. Resolve immutable image/template, storage attachments, routes, and limits.
4. Select a qualified worker with matching architecture, CPU profile, capacity,
   snapshot locality, and region policy.
5. Persist a creation operation and idempotency key.
6. Ask the E2B node orchestrator to create or resume the Firecracker VM.
7. Install routing and egress policy before returning a usable endpoint.
8. Mark `running` only after the guest health check and policy report succeed.
9. Proxy process, filesystem, terminal, and port requests to `envd`.
10. Refresh activity with bounded leases; activity does not extend expiration
    unless the caller is authorized to do so.

### Automatic standby and resume

1. The lifecycle controller observes no qualifying activity for the configured
   interval.
2. It acquires the sandbox operation lock and moves the object to `pausing`.
3. New traffic waits or receives a retryable state; it cannot race a second
   snapshot.
4. The node records the snapshot manifest and releases compute.
5. The controller marks `standby` only after artifacts are durable enough for the
   configured profile.
6. Authenticated incoming traffic requests resume.
7. Policy, quota, CPU compatibility, and snapshot identity are rechecked.
8. The client proxy buffers only within explicit limits and routes after health.

### Execute a job

1. Persist job and immutable input references.
2. Expand attempts lazily up to the concurrency and budget limits.
3. Create attempts from a clean template or approved checkpoint.
4. Stream content-free state and resource events; logs are separate tenant data.
5. Collect declared artifacts by digest.
6. Retry only errors permitted by policy.
7. On cancel or terminal completion, reconcile every attempt to a cleanup state.

## Networking

### Ingress

All inbound traffic passes through the client proxy. Routes use unguessable
identifiers plus caller authorization; the hostname is not a capability. Public
preview links are opt-in, scoped, revocable, rate-limited, and never expose the
guest management API.

The proxy may wake a standby sandbox. It must cap queued bytes, connections, and
resume attempts to prevent a cheap denial of service.

### Egress

The default is deny. An allowed HTTP route is enforced outside the VM by a
gateway that independently resolves DNS, blocks metadata and rebinding, checks
scheme/host/port/method/path, strips caller-supplied auth headers, and optionally
injects a short-lived credential.

Direct TCP and UDP are separate capabilities. A domain allowlist is not a valid
claim for traffic that bypasses the HTTP gateway.

### Private networking

The single-host preview uses per-sandbox virtual interfaces and host-enforced
rules. Cluster mode assigns stable workload identity and regional addresses,
then routes through an overlay or VPC network. Standard Linux/Cilium networking
comes before custom VPP work. VPP is justified only by measured throughput,
latency, or density limits.

## State and data stores

| State | Recommended store | Notes |
| --- | --- | --- |
| organizations, policies, sandboxes, jobs, operations | PostgreSQL | durable source of truth |
| live routing, locks, short leases | Redis | reconstructable; never sole lifecycle truth |
| templates, snapshots, artifacts, receipts | S3-compatible object storage | immutable manifests and tenant prefixes |
| metrics and content-free events | OpenTelemetry backend | ClickHouse optional at scale |
| logs and terminal output | tenant-scoped object/log store | opt-in retention; never control-plane truth |
| secret values | Vault/KMS/cloud secret manager | only opaque handles in project databases |

Single-host development may combine services through E2B Embed and local object
storage, but the API semantics must remain identical.

## Scheduling

Placement filters before scoring:

1. tenant and region policy;
2. runtime and image compatibility;
3. CPU architecture and feature template;
4. required volume/drive/network capability;
5. hard resource and quota availability;
6. snapshot accessibility; and
7. node qualification and patch policy.

Then score snapshot locality, image cache, model-route proximity, available
capacity, failure-domain spread, and estimated transfer cost. A fast but
unqualified node is not a candidate.

## Compatibility

Initial compatibility priorities:

1. E2B SDK semantics for common sandbox operations where the substrate already
   supports them;
2. OpenAI Agents SDK sandbox adapter;
3. Anthropic self-hosted sandbox adapter;
4. MCP tools for process and filesystem operations; and
5. provider migration helpers only after conformance tests.

We should not promise a Blaxel-compatible API. Its public concepts inform the
resource model, but undocumented behavior and trademarks stay out of scope.

## Availability model

Control-plane operations are durable and idempotent. Every mutation returns an
operation ID. Reconcilers can continue after API, controller, or client restart.

The first production profile is single region with multiple worker nodes. It
supports node failure by restoring from durable snapshots or volumes where
available; it does not promise transparent process continuity. Multi-region
failover requires replicated image, snapshot, policy, and routing state and is a
later profile.

## Repository shape after implementation begins

```text
cmd/
  runtime-api/
  runtimectl/
  egressd/
internal/
  api/
  auth/
  admission/
  audit/
  e2b/
  image/
  job/
  lifecycle/
  modelroute/
  operation/
  policy/
  scheduler/
  storage/
sdk/
  python/
  typescript/
spec/v1alpha1/
deploy/
  compose/
  terraform/
  helm/
conformance/
benchmarks/
examples/
docs/
```
