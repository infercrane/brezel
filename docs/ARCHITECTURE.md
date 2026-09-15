# Architecture

## Design principles

1. **Own the runtime, reuse a proven engine.** The product owns the API, data
   paths, lifecycle, policy, packaging, and conformance while a pinned
   Apache-2.0 Firecracker engine remains an internal dependency.
2. **Session state and compute lifetime are different.** An idle sandbox may
   stop consuming CPU while its checkpoint and explicit storage remain.
3. **One API, honest states.** Active, standby, archived, expired, deleting, and
   failed are different conditions with durable transitions.
4. **Storage is explicit.** Root filesystem snapshots, single-writer volumes,
   shared drives, and immutable artifacts have different guarantees.
5. **Private by default.** No public ingress, ambient cloud identity, long-lived
   sandbox credentials, or egress unless the caller explicitly opts into the
   developer-preview unrestricted-internet mode or attaches an approved route.
6. **Inference is local infrastructure.** A model route is a typed resource that
   can be placed near the sandbox without making this project a model server.
7. **Measure before claiming.** Startup, resume, density, durability, and
   isolation claims must name a tested release and environment.

## Target fleet architecture

The diagram and fleet services in this section are a design target, not the
current release. The implemented private single-host topology is described
under [State and data stores](#state-and-data-stores) and bounded by
[Status](STATUS.md).

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
| mTLS node relay|          | mTLS node relay|          | mTLS node relay|
| microVM worker |          | microVM worker |          | microVM worker |
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

### Internal microVM engine

The first engine implementation uses the open-source E2B Runtime's node
orchestrator, Firecracker virtual machines, template builder, guest agent,
client proxy, snapshot mechanics, and persistent volume support. It is fetched
by an immutable commit during installation and is not a customer-selected API.

Changes that improve generic lifecycle or isolation should be contributed
upstream first. A maintained patch set is acceptable during incubation. A hard
fork requires an ADR explaining why compatibility and maintenance cost are
worth it.

### Product runtime service

The target product runtime adds the opinionated product surface and system
behavior:

- organization, project, tenant, role, and service-account identity;
- declarative sandbox and job APIs;
- lifecycle policy, TTL, quotas, placement, and cleanup operations;
- network and model-route admission;
- durable job fan-out, cancellation, retries, and result aggregation;
- execution receipts and content-minimal audit;
- self-host packaging, upgrades, backup, and air-gap support; and
- conformance and performance qualification.

The service maps its stable contract to the pinned engine protocol. Engine
states are translated without inventing success when the engine reports an
unknown condition.

The implemented single-host profile binds hashed bearer credentials to an
explicit project allowlist, applies positive per-project primitive and live
operation limits, and runs exactly one controller over an exclusively locked
SQLite WAL lifecycle ledger. Stored resource identities are validated on open
and generic commits; sandbox lookup, activity, and event hot paths use keyed
transactions. A process-wide admission limit keeps
ordinary traffic bounded while health, readiness, and content-free Prometheus
counters and fixed-dimension phase histograms remain available. Phase labels
are closed operation, phase, and outcome enums; tenant, resource, path,
command, and content dimensions are structurally unavailable. Access logs use
matched route patterns rather than raw paths. This is a hardened local authority, not the
PostgreSQL/Redis multi-writer design shown above. `/readyz` verifies that state
boundary, the authenticated engine and node data paths, and the separate node
control listener when routed execution is configured.

The packaged installer reconciles the embedded engine's effective tenant
admission limit transactionally before API startup. Because the engine caches
the complete authenticated team in Redis, the same stopped-API upgrade boundary
removes only `auth:team:*` derived entries before restarting admission; sandbox
and routing state in Redis is never flushed. Public API and node services remain
stopped when an upgrade or post-start qualification fails.

### Guest API

The bundled guest agent remains the authority for operations inside a sandbox.
The product currently exposes streamed commands, bounded file transfer, and
authenticated HTTP previews through its own API rather than installing a second
privileged guest agent.

Project metadata is attached outside the guest. If a guest extension becomes
necessary, it must be unprivileged where possible, versioned independently, and
included in conformance and receipts.

### Node-local relay trust boundary

The node relay is the product-owned boundary between an admitted product
operation and the private engine guest identity. Its internal protocol carries
commands, file transfer, and application-port traffic without exposing engine
IDs or guest-management credentials to a public caller.

The API and each node authenticate one another with TLS 1.3 and exact URI SAN
identities. A node certificate has the identity
`spiffe://brezel/node/<node-id>` and the API certificate has
`spiffe://brezel/api/<api-id>`. Certificate role is also constrained by the
exclusive server or client extended key usage. The implementation performs
normal CA and DNS or IP SAN verification in addition to the exact URI check;
it does not use insecure verification or follow redirects. Certificate, key,
and CA inputs must be private regular files and cannot be symlinks.

After normal project, lifecycle, and quota admission, the API signs an Ed25519
capability for exactly one operation: command execution, file read, file write,
or application-port proxy. The capability binds issuer and key ID, audience,
node and boot identity, opaque route ID, monotonically increasing route
generation, project, sandbox, canonical request digest, operation-specific
byte or time bounds, a random identifier, and a lifetime of at most 30 seconds.
The relay rejects an invalid or stale binding before resolving the private
engine ID.

Each node holds a crash-safe, exclusively locked generation ledger. Rebinding a
route allocates a new generation, so a capability for a prior VM assignment
cannot reach its replacement. Released routes remain fenced by the durable
generation clock. An operation lease on the exact route generation prevents
standby, release, or rebinding until admitted work exits. A bounded in-memory
replay cache consumes a capability ID once and fails closed rather than
evicting an unexpired entry. The relay server generates a fresh random 128-bit
boot identity inside every process; tokens from a prior process then fail before
replay admission.

The packaged node process exposes two listeners behind the same exact mTLS
identity: a bounded control listener for route bind and state transitions, and
a data listener whose per-operation contexts can carry long command streams.
The control protocol applies compare-and-swap transitions for attach-ready,
drain, standby, rebind, release, and removal. It never returns a private engine
identity. This separation keeps streaming data deadlines from weakening route
control limits.

The relay protocol has explicit command, file, and port endpoints, bounded
headers, bodies, output, and operation duration, and no content logger. It
streams command events but currently buffers file bodies and proxied HTTP
bodies. Terminal sessions, WebSockets, raw TCP, capability delegation, and
multi-hop forwarding are not part of this milestone.

The packaged single-host profile selects this relay for admitted command, file,
and preview operations. Public HTTP still terminates at `brezeld`; clients do
not receive a node capability or connect directly to a node. Lifecycle,
placement, desired route state, and reconciliation remain controller-owned.
Dynamic node enrollment, online certificate and signing-key rotation, durable
node-operation receipts, service-unit packaging, and a direct data-edge
handoff remain future work. See [ADR
0009](decisions/0009-node-relay-trust-boundary.md).

## Primary resources

### Environment and image

In the target architecture, an environment is the reproducible launch contract. It resolves an OCI image,
resources, startup actions, tool capability manifests, connectors, network
policy, storage, and retention defaults to immutable revisions.

Its image manifest binds container digest, kernel, init/guest version, platform,
build provenance, and checkpoint artifacts. Mutable image tags are allowed only
as build inputs; a sandbox always launches a resolved digest.

### Sandbox

A sandbox has stable identity and an explicit lifecycle. `archiving`,
`archived`, and `restoring` below are target fleet states; the current release
implements requested, preparing, running, standby, failed, expired, deleting,
and deleted:

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

### Checkpoint

The target checkpoint contract is an immutable resource with an explicit kind:

- `filesystem` preserves the declared writable filesystem state; and
- `full_state` also preserves process memory and supported device state.

Every checkpoint binds source sandbox, parent checkpoint, image, kernel,
init/guest, runtime, CPU, device, policy, storage, and encryption identities. It
records compatibility constraints, retention, integrity, and known in-flight
external effects at the checkpoint boundary.

Checkpoint locality is a scheduling input. Cross-node restore may be slower than
same-node resume and must be measured separately. Cross-region replication is a
later storage workflow, not live migration. A dependent checkpoint cannot be
deleted until children are deleted or materialized independently.

The current release exposes filesystem checkpoint and restore. Public
`full_state` checkpoint and fork operations are not implemented. Any future
full-state release checkpoints only at an explicit quiescent boundary. A
checkpoint cannot undo an external effect such as a message, payment, tool call,
or model request. Restore and fork must not silently duplicate unresolved
effects.

### Workspace

A durable workspace is a single-writer filesystem whose lifetime is independent
from any sandbox. The public API uses project-scoped workspace IDs; substrate
volume IDs, names, and content tokens never leave the runtime service.

The sandbox writable root and a durable workspace are different storage
classes. The writable root is local, ephemeral execution state optimized for
package installs, compilers, caches, and many-small-file workloads. It may be
captured as part of a full-state checkpoint, but it is not acknowledged as
host-loss-durable storage. A mounted workspace is the explicit persistence
boundary: acknowledged writes must survive the profile's declared crash model,
even when that requires a slower write path. Users choose durability by writing
important state below `/workspace`; Brezel must not make every temporary build
file pay that cost or silently weaken the workspace contract to win a benchmark.

The current single-host profile persists a creation or deletion intent before
calling the pinned engine. A workspace must be `ready` before attachment. The
service rejects a second attachment while any non-terminal sandbox owns it and
rejects deletion while attached. Unknown create or cleanup outcomes remain
`unknown` for reconciliation instead of being reported as success.

The installer enables the engine volume path explicitly, binds its backing
directory from protected host state, reads its signing key from a protected
file, and applies a pinned patch that confirms physical data removal before
deleting engine metadata. A second pinned engine patch flushes workspace file,
truncate, and namespace mutations before the NFS server returns its advertised
stable acknowledgement. The hard-reset qualification corpus verifies create,
overwrite, truncate, atomic rename, nested creation, and a one-megabyte payload
across an actual boot-identity change. This is local-host crash durability, not
replicated storage, backup, secure erase, or host-loss recovery.

### Drive (planned)

A drive is a concurrent shared workspace with semantics that differ from a
volume. It requires coherency, authorization, quota, backup, and failure tests.
The first implementation should evaluate JuiceFS over S3-compatible object
storage and PostgreSQL or Redis metadata. Do not expose it as production-ready
until rename, fsync, locking, partial-failure, and tenant-isolation tests pass.

### Job (planned)

A job is a durable desired state for one or many bounded runs. It stores input
references, concurrency, retry policy, timeout, result policy, and cancellation
intent. Each attempt receives a distinct sandbox and operation identity.

Cancellation is durable. A cancelled client connection is not cancellation.
Retries never reuse a potentially contaminated sandbox unless the job explicitly
starts from a qualified checkpoint.

### Model route (target)

A model route maps a stable alias to an approved OpenAI-compatible or later
protocol endpoint. The route contains model allowlists, placement, credential
handle, request limits, and optional budget controls.

The sandbox sees a local or private route and a short-lived workload identity.
The egress gateway resolves the real endpoint and injects the credential only
after policy checks. Prompt and response content is not logged by default.

### Connector

A connector generalizes the model route to an approved external service. Its
revision binds destination, protocol, method/path rules, request limits, an
opaque credential handle, response-scrubbing policy, region, and data boundary.
The sandbox receives only a placeholder or short lease identity.

The gateway authenticates workload identity, admits each request, injects the
credential outside the guest, and removes configured sensitive response values.
A model route is a connector specialization with model, token, concurrency, and
optional spend controls.

The current developer preview implements a narrow bearer connector. It passes a
five-minute signed lease to the sandbox, adds only the gateway host to the
effective egress allowlist, and resolves `secret://file/...` handles from 0400 or 0600
operator files. The gateway binds the lease to project, sandbox, and immutable
connector revisions; checks that the sandbox is still running; strips guest
authorization and cookie headers; injects the credential outside the VM; and
scrubs exact credential bytes from bounded responses.

This is not yet the production identity design. Renewal is bearer-based rather
than proof-of-possession, response buffering does not support streaming model
output, and the file resolver is a single-host development adapter. ADR 0003
records the boundary. The private profile requires SPIFFE or equivalent
workload identity, a managed secret resolver, rate and concurrency budgets, and
streaming conformance.

### Sandbox group (planned)

A sandbox group requests co-located computers with an explicit private link
network. It supports an agent with a browser, database, trusted helper, or peer
workers without packing mutually untrusted workloads into one sandbox. Group
identity, addressability, and teardown are atomic at the control-plane level.

### Rollout (planned)

A rollout is a reproducible collection of agent episodes over one environment,
one or more model connector revisions, a task set, evaluators, budgets, branch
strategy, stop conditions, and declared artifacts. It is implemented over Jobs;
it does not own agent reasoning or inference serving.

### Operation event and receipt

The current release emits ordered lifecycle events and signed receipts. Durable
webhook delivery and SDK event streams are planned. Events are lifecycle
metadata; tenant logs and customer content remain separate data products.

A receipt binds immutable identities and the controls observed or enforced. It
uses a standard attestation envelope and never implies hardware attestation
unless a separately qualified confidential profile supplies verified platform
measurements.

## Request flows

### Create and use a sandbox

The current single-host flow implements authentication, project policy,
immutable template resolution, durable creation intent, engine creation,
guest-readiness verification, relay route installation, and lifecycle activity.
Fleet placement and organization policy in the numbered flow are target steps.

1. Authenticate caller and resolve organization/project.
2. Validate the sandbox request and organization policy.
3. Resolve immutable image/template, storage attachments, routes, and limits.
4. Select a qualified worker with matching architecture, CPU profile, capacity,
   snapshot locality, and region policy.
5. Persist a creation operation and idempotency key.
6. Ask the selected microVM worker to create or resume the Firecracker VM.
7. Install routing and egress policy before returning a usable endpoint.
8. Mark `running` only after the guest health check and policy report succeed.
9. Authorize process, filesystem, and port requests without exposing the guest
   credential. The current single-host path issues one-operation capabilities so
   public command, file, and application-port requests travel through `brezeld`
   to the node relay. The relay owns guest-facing transport and private engine
   resolution; lifecycle operations remain in the durable API authority.
   Terminal transport remains unimplemented.
10. Refresh activity through a bounded, coalesced lifecycle signal; the relay
    hot path must not fsync durable activity state for every byte operation, and
    activity does not extend absolute expiration.

The single-host adapter accepts a persisted engine `running` observation only
with an authenticated guest-health proof no older than one second. Create and
resume establish the same short proof. Pause, unknown state, and delete revoke
it, and the proof cache is process-local, so controller or host restart always
forces a fresh guest probe. This bounded lease removes duplicate probes from a
request burst without allowing an engine metadata row to survive a lost VM as
a false `running` state.

### Automatic standby and resume

1. The lifecycle controller observes no qualifying activity for the configured
   interval.
2. It acquires the sandbox operation lock and moves the object to `pausing`.
3. New traffic waits or receives a retryable state; it cannot race a second
   snapshot.
4. The engine records the full-state standby artifact and releases compute.
5. The controller marks `standby` only after the engine confirms the artifact.
6. Authenticated incoming traffic requests resume.
7. Project policy, quota, and sandbox identity are rechecked; engine
   compatibility remains part of the pinned single-host profile.
8. Traffic routes only after a fresh guest health check.

### Execute a job (planned)

1. Persist job and immutable input references.
2. Expand attempts lazily up to the concurrency and budget limits.
3. Create attempts from a clean template or approved checkpoint.
4. Stream content-free state and resource events; logs are separate tenant data.
5. Collect declared artifacts by digest.
6. Retry only errors permitted by policy.
7. On cancel or terminal completion, reconcile every attempt to a cleanup state.

## Networking

### Ingress

The current HTTP preview path uses an opaque, short-lived lease, rechecks
sandbox state, and never exposes the guest-management API. It does not yet
provide a public hosted ingress service, custom domains, WebSockets, or
independent per-route rate limits.

A future hosted proxy may wake a standby sandbox. It must cap queued bytes,
connections, and resume attempts to prevent a cheap denial of service.

### Egress

The default is deny. An allowed HTTP route is enforced outside the VM by a
gateway that independently resolves DNS, blocks metadata and rebinding, checks
scheme/host/port/method/path, strips caller-supplied auth headers, optionally
injects a short-lived credential, and scrubs configured credential or session
material from the response.

Direct TCP and UDP are separate capabilities. A domain allowlist is not a valid
claim for traffic that bypasses the HTTP gateway.

### Private networking

The single-host preview uses per-sandbox virtual interfaces and host-enforced
rules. A future cluster mode assigns stable workload identity and regional addresses,
then routes through an overlay or VPC network. Standard Linux/Cilium networking
comes before custom VPP work. VPP is justified only by measured throughput,
latency, or density limits.

## State and data stores

The following table is the target fleet storage split. It is not implemented by
the private single-host release.

| State | Recommended store | Notes |
| --- | --- | --- |
| organizations, policies, sandboxes, jobs, operations | PostgreSQL | durable source of truth |
| live routing, locks, short leases | Redis | reconstructable; never sole lifecycle truth |
| templates, snapshots, artifacts, receipts | S3-compatible object storage | immutable manifests and tenant prefixes |
| metrics and content-free events | OpenTelemetry backend | ClickHouse optional at scale |
| logs and terminal output | tenant-scoped object/log store | opt-in retention; never control-plane truth |
| secret values | Vault/KMS/cloud secret manager | only opaque handles in project databases |

The single-host distribution combines the pinned engine, a separate execution
node relay, an embedded SQLite lifecycle ledger, a host-backed workspace
directory, and the product runtime API. The API and node authenticate with TLS
1.3 mutual identities. The node alone maps opaque route generations to private
engine IDs; one-operation capabilities authorize command, file, and preview
traffic. Clustered profiles must preserve the same identity, exclusivity, and
confirmed-cleanup semantics.

SQLite runs in WAL mode with full synchronous durability and one controller
process lock. A one-time transaction imports a valid protected legacy JSON
state file without modifying it. Keyed sandbox reads, activity updates, and
content-free event appends avoid copying unrelated records. Some low-frequency
lifecycle transactions still use the generic state interface and are a known
single-host scaling limit, not a fleet database design.

## Scheduling (target)

Placement filters before scoring:

1. tenant and region policy;
2. runtime and image compatibility;
3. CPU architecture and feature template;
4. required volume/drive/network capability;
5. hard resource and quota availability;
6. snapshot accessibility; and
7. node qualification and patch policy.

Then score checkpoint locality, image cache, model-route proximity, available
capacity, failure-domain spread, and estimated transfer cost. A fast but
unqualified node is not a candidate.

## Compatibility

Initial compatibility priorities:

1. the product's compact sandbox API and CLI;
2. OpenAI Agents SDK sandbox adapter;
3. Anthropic self-hosted sandbox adapter;
4. MCP tools for process and filesystem operations; and
5. provider migration helpers only after conformance tests.

We should not promise compatibility with a proprietary provider API without a
public conformance profile. Undocumented behavior and trademarks stay out of
scope.

## Availability model (target)

Resource lifecycle mutations in the current single-host release have durable
idempotency and reconciliation. Command execution, file writes, and ephemeral
preview leases are not replay-safe mutations. Some node rebind and removal
transport failures intentionally remain ambiguous and fail closed until durable
node-operation receipts exist.

The target first production profile is single region with multiple worker nodes. It
supports node failure by restoring from durable snapshots or volumes where
available; it does not promise transparent process continuity. Multi-region
failover requires replicated image, snapshot, policy, and routing state and is a
later profile.

## Target repository shape

```text
cmd/
  brezeld/
  brezel/
  egressd/
internal/
  api/
  auth/
  admission/
  audit/
  backend/
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
