# World-class runtime gap audit

Date: 2026-09-14

This document is an engineering audit, not a product claim. It compares the
current repository with public implementation documentation and primary
systems research, then turns the differences into release gates. A feature in
another project is a design reference, not proof that it is secure, fast, or
appropriate here.

## Evidence labels

- **Implemented** means the behavior exists in this repository and has local
  automated tests or named-host qualification evidence.
- **Observed gap** means code inspection shows that the behavior is absent or
  bounded to the current single-host profile.
- **External reference** means the cited project's own documentation or source
  describes a technique. It is not an independent benchmark or endorsement.
- **Research direction** means a paper supports further investigation. It is
  not a release dependency or a performance promise.
- **Release gate** is work that must pass before the corresponding availability
  or performance claim can be made.

## Executive finding

The repository is already more than a policy wrapper: it owns a versioned API,
explicit lifecycle states, project-scoped identities, a real Firecracker
execution path, streamed commands and files, authenticated application ports,
durable single-writer workspaces, filesystem checkpoints, idempotent mutations,
reconciliation, and signed receipts. The named-host qualification is useful
evidence for a private single-host release candidate.

The current system is not yet a fleet runtime. Three architectural properties
prevent that label:

1. A single Go process and whole-state JSON rewrite remain the durable
   authority. Create, pause, resume, and delete no longer hold the service mutex
   across backend waits, but admission and finalization still converge through
   one process-local lock and store.
2. Guest commands, files, and previews now cross a product-owned authorization
   boundary, but its initial adapter remains in the API process and the bytes
   still cross that process. Production-scale systems keep authenticated data
   traffic off the durable control path.
3. There is no worker scheduler, warm-pool manager, distributed authority,
   off-host snapshot/workspace recovery, or resource ledger.

The fastest honest route is to preserve the current contract and replace the
single-host internals in layers: use the new internal phase telemetry to run the
reproducible end-to-end harness on Linux/KVM, then complete a node-local fast
path and warm capacity, and only then introduce replicated control state and a
worker fleet. Changing the benchmark boundary or weakening durability is not
an optimization.

## Current capability inventory

| Area | Current repository evidence | Boundary or missing behavior |
| --- | --- | --- |
| Isolation | Bundled, pinned E2B Runtime revision over Firecracker; no release container fallback | No hostile shared-multitenant qualification, external escape review, or continuous isolation regression program |
| Lifecycle | Requested/preparing/running/pausing/standby/resuming/deleting/deleted/expired/failed/unknown; durable activity and grace deadlines; automatic pause intent and restart recovery; independent create/resume/pause backend waits | One controller and process-local mutation fences; activity is API-visible rather than a complete node/network signal; no worker generation/fencing token, transactional authority, zone-loss, or rolling-upgrade proof |
| Guest data path | Real Connect command stream, bounded files, authenticated HTTP preview, and a product-owned revision-bound/revocable node handoff; create/resume seed a one-second revocable guest lease | The first node adapter is in process, guest setup after lease expiry reads sandbox detail from the engine, and all bytes traverse the product API; no direct regional relay, PTY, WebSocket, or resumable stream |
| Distribution artifacts | Engine source, patches, external OCI images, orchestrator, guest, Firecracker, kernel, and BusyBox are independently digest-locked and recorded in a protected post-install manifest; pull and preloaded modes fail closed | No signed release bundle, transparency log, complete SBOM/provenance policy, or physically offline qualification; air-gapped support is not claimed |
| State | Private, bounded, schema-validated JSON; keyed deep-copy sandbox reads; specialized ordered sandbox-event append; compact encoding; atomic replace, file and directory `fsync`; exclusive process lock | General views and mutations still clone the complete state, and every mutation/event append still serializes and replaces it; no transactions, multi-writer authority, compaction, or online migration |
| Snapshots | Full-state standby through the engine; independent immutable filesystem checkpoints and restore lineage | No independent full-memory checkpoint/fork contract, cross-node restore, encrypted object manifest, compatibility placement, warm pool, or fork fan-out |
| Workspaces | Host-backed, project-scoped, durable across sandbox replacement, single writer | Not encrypted, replicated, backed up, or recoverable after host loss; no concurrent drive or measured RPO/RTO |
| Network | Deny-by-default request contract, per-sandbox engine policy, guarded HTTP preview | No qualified raw TCP/UDP, WebSocket, private overlay, static egress, per-flow accounting, cached network-resource pool, or multi-host route convergence |
| Credentials | Opaque file secret handles and a narrow external connector broker; provider secret injected outside the guest | Connector renewal is bearer based rather than proof-of-possession; no managed KMS/secret provider, workload identity, streaming response path, dynamic rotation proof, or full-state snapshot canary gate |
| Identity | Protected SHA-256 token policy bound to explicit projects | No OIDC, organizations, roles, service accounts, approvals, short-lived user sessions, or external audit sink |
| Capacity | Per-project object and guest-operation counts plus process-wide request admission | No CPU, memory, PID, disk, IOPS, bandwidth, connection, snapshot, wake, or model-spend enforcement; no placement or noisy-neighbor measurements |
| Observability | Request IDs, route-pattern access logs, ordered resource events, low-cardinality process counters, fixed-enum lifecycle/guest phase histograms, signed content-minimal receipts | No OpenTelemetry trace graph, node/capacity metrics, durable log stream, webhook/watch API, SLO burn alerts, or fleet incident evidence |
| Environments | Immutable product revision referring to an existing backend template | No arbitrary OCI build pipeline, SBOM/provenance verification, vulnerability policy, signed image promotion, cache rollout, or rollback exercise |
| Jobs and rollouts | A versioned run schema describes the intended contract | No coordinator, queue, fan-out, retry isolation, cancellation, artifact collection, rollout budget, or straggler handling |
| Qualification | Destructive real-guest conformance and one named-host report; native TTI, warm-exec, resume, checkpoint, restore, preview, and workspace-I/O harness with sequential/staggered/burst modes and raw attempts | The new harness has not yet produced a post-change Linux/KVM report; no sustained-rate/soak/fault matrix, density curve, real-workload pipeline suite, or hosted comparison under matched boundaries |

## Critical hot-path findings

### 1. Durable authority remains single-process

**Partially addressed.** Create, pause, resume, and delete now durably enter a
transitional state, register a scoped process-local mutation fence, release
`Service.mu` while waiting on the backend, and reacquire it for finalization.
Deterministic race tests prove that independent creates and resumes reach the
backend concurrently while reconciliation leaves the active transitions alone.

`Service.mu` still serializes admission, durable finalization, inspection, and
workspace coordination. Independent checkpoint backend calls now use the same
fenced unlock-during-I/O pattern as lifecycle operations; commands and
conflicting mutations remain blocked on the checkpointed sandbox. `FileStore`
uses typed deep copies instead of a whole-state JSON round trip, compact
encoding instead of indented JSON, an O(1) keyed deep-copy read for sandbox
authorization, and a specialized event append that avoids cloning unrelated
records. General views and updates still clone the complete state, while all
durable mutations and event appends still perform an atomic, directory-synced
full-file replacement. Those choices are defensible for one crash-consistent
controller, but they cannot be the fleet authority or a high-rate allocation
path.

**Release gate.** Before increasing concurrency:

1. instrument lock wait, admission wait, state transaction, scheduler, engine,
   guest readiness, first-exec, and cleanup separately;
2. add per-resource serialized state machines with generation-based fencing;
3. make quota reservation and idempotency atomic in a transactional store;
4. keep backend and network calls outside database transactions and global
   locks;
5. prove delete-wins and retry safety with deterministic race and crash tests;
6. publish saturation curves, not only latency at one concurrency.

The cluster authority should be PostgreSQL for durable desired state and an
outbox, with Redis or an equivalent lease/routing cache treated as
reconstructable. The existing JSON store should remain the explicit
single-controller distribution, not be stretched into a distributed database.

### 2. Guest traffic still crosses the control service

**Partially addressed.** Create and resume now populate a one-second,
process-local guest credential lease. The first command can therefore avoid a
redundant engine sandbox-detail lookup. Expiry falls back to fresh engine state;
identity mismatch purges the lease; and pause/delete revoke live, guest, and
port credentials before and after the mutation, including failure paths. A new
`node.DataPlane` boundary requires a content-free authorization handoff bound
to project, sandbox, engine identity, operation, durable revision, and expiry.
Stale or revoked handoffs fail before engine entry, and lifecycle observation
cannot advance the revision while an admitted guest operation is active.

The API still proxies command, file, and preview traffic. Guest setup after the
short lease expires performs an engine sandbox-detail lookup. This leaves extra
control-plane round trips and makes the API process a throughput and
availability bottleneck.

**External reference.** E2B's public architecture explicitly separates control
and data planes: the API chooses and records placement, while the node
orchestrator owns Firecracker, networking, storage, and guest access; sandbox
traffic bypasses the API. OpenShell uses a supervisor-initiated authenticated
session and multiplexes exec, file, configuration, and relay traffic over it.

**Release gate.** Introduce a regional or node-local relay that:

- receives an opaque, short-lived capability bound to project, sandbox,
  generation, operation, method, and audience;
- validates current routing and revocation without reading the durable sandbox
  row for every frame;
- connects directly to the assigned worker/guest data path;
- supports bounded backpressure, half-close, cancellation, PTY resize, file
  resume, and port/WebSocket streams;
- returns `unreachable` or a retryable resume state rather than inventing
  `running`; and
- emits only content-free operational telemetry by default.

The public API may mint and revoke capabilities, but it should not remain the
byte path for every terminal, file, and application connection.

### 3. Readiness needs a first-instruction boundary

**Implemented.** A sandbox is marked running only after the engine returns a
secured guest credential, and stale persisted running state is rechecked with a
short guest-liveness lease.

**Partially addressed.** The native benchmark now measures create through an
exact nonce command, warm command execution, and resume through that same
verified instruction. It preserves raw attempts, request IDs, setup, measured,
and cleanup phases, cache-state declarations, schedule delay, failures, and
cleanup risk across sequential, staggered, and burst modes.

**Gap.** It cannot yet distinguish the server-internal engine allocation,
isolation, policy, guest-health, and data-plane readiness stages. A fast API
response is not the user-visible outcome, and a client-side create duration is
not a substitute for those internal traces.

**External reference.** ComputeSDK defines Time to Interactive as wall time from
create initiation through the first successful deterministic command and
excludes cleanup. OpenSandbox's fast-runtime proposal distinguishes
`RuntimeReady` from `DataPlaneReady`. Modal separately describes scheduled,
started, ready, and in-use stages and argues that dependency setup often
dominates boot.

**Release gate.** Adopt these timestamps in every create trace and receipt:

```text
client_start
  -> admission_reserved
  -> desired_state_committed
  -> worker_selected
  -> runtime_allocated
  -> isolation_ready
  -> policy_and_routes_ready
  -> guest_ready
  -> first_command_started
  -> first_command_verified
```

Only `first_command_verified - client_start` is end-to-end TTI. Internal stage
latencies may be reported separately, with the exact cache state and topology.

## World-class target architecture

```text
SDK / CLI
    |
regional API: identity, policy, idempotency, desired state, operations
    |                         |
transactional authority      regional capability router
(Postgres + outbox)           (short leases, no customer content)
    |                         |
scheduler and reconcilers     +----------------------------+
    |                                                      |
worker control stream  ------------------------------> node agent
                                                           |
                  +-------------------+--------------------+----------------+
                  |                   |                    |                |
             warm snapshot       network pool       local NVMe cache  guest relay
                  |                   |                    |                |
                  +------------------- Firecracker microVM ----------------+
                                                           |
                                      policy egress / credential / model gateway

Durable artifacts: content-addressed, encrypted object storage
Durable workspaces: qualified block/POSIX service with backup and restore
Identity and keys: OIDC + service accounts + workload identity + KMS
Telemetry: OpenTelemetry stages, low-cardinality metrics, redacted audit
```

The node agent, not the regional API, owns the latency-sensitive allocation
transaction. The regional authority persists intent and a capacity reservation,
then sends a fenced assignment to one qualified node. A node prepares network,
storage, and the VM from local caches; it reports separate runtime and data-plane
readiness. The regional router switches traffic only to the matching sandbox
generation.

## Ordered engineering program

### Gate A: reproducible single-node performance evidence

This is the immediate gate and does not require a hosted fleet. The repository
now contains native TTI, warm-exec, resume, filesystem checkpoint, filesystem
restore, preview first-byte, warm preview, and workspace I/O scenarios under
sequential, 200 ms staggered, and burst load. TTI includes a deterministic
first command; every scenario validates an exact generated nonce or byte
payload; cleanup stays outside the latency timer but must be confirmed per
resource. Report schema v3 preserves every raw attempt, scheduled-arrival and
service latency, failed and censored latency, time to first success, the
measurement window, throughput, request identities, and cleanup outcome. The
single-host matrix also records the source and binary revisions, host shape,
kernel, CPU, filesystem, Docker runtime, and before/after load without
collecting customer content.

The remaining work is to run that matrix on the named Linux/KVM target and
extend it without changing its measurement boundaries.

- Verify image/template digest, resource size, network topology, and cache state
  from the runtime where possible; the current cache label is operator-declared.
- Add p90 where useful alongside the existing p50, p95, p99, max, success rate,
  admission failures, cleanup risk, and raw samples.
- Run clean boot, warmed cache, cache miss, steady rate, burst, and post-soak
  profiles independently.
- Add a separate real-workload suite patterned after StarSling's cold repository
  pipelines: clone, dependency installation, lint, typecheck, build, and the
  workload's own validation. Do not combine this score with startup TTI.
- Add failure injection for engine timeout, guest-health failure, controller
  restart, disk full, snapshot corruption, cleanup failure, and lost worker.

Exit: a third party can reproduce one named-host result and trace every
percentile to raw attempts. The result may say the target was missed.

### Gate B: node-local fast path

- Cache a guest connection only for the lifetime of a short, revocable sandbox
  generation lease; revoke on pause, unknown, delete, or reconciliation change.
- Replace repeated engine metadata lookups on warm guest operations with the
  generation-bound connection cache.
- Introduce a bounded pool for reusable network namespaces/TAP resources only
  after identity and stale-route tests pass.
- Pre-open or cache template and snapshot artifacts on local NVMe; report cache
  hit and miss separately.
- Remove global lifecycle serialization in favor of per-resource locks,
  transactional capacity reservations, and fencing.
- Add a node resource ledger and hard cgroup/device/network enforcement for CPU,
  memory, PIDs, disk, IOPS, connections, and bandwidth.

Exit: sequential and burst TTI improve without lost cleanup, weaker readiness,
or cross-sandbox route/credential reuse. Tail latency and throughput improve on
the same host, not just the median.

### Gate C: snapshot, fork, and warm-capacity product

- Resolve arbitrary OCI inputs into signed, pre-booted immutable environment
  revisions with SBOM, provenance, guest, kernel, CPU template, device, policy,
  and artifact digests.
- Store snapshot manifests and encrypted artifacts off host; keep hot chunks on
  local NVMe and fetch lazily.
- Implement independent full-state checkpoint and fork only after memory, disk,
  network, vsock, entropy/uniqueness, credential-rebind, and external-effect
  tests pass.
- Maintain demand-shaped warm pools by environment/resource/network/policy
  class; claim assigns fresh sandbox and workload identities before exposure.
- Replenish asynchronously with explicit pool-depleted and cache-miss behavior.
- Support clean-template job retries by default; never reuse a contaminated
  attempt implicitly.

Firecracker documents that snapshots contain memory and VMM state while block
devices remain the integrator's responsibility; network/vsock continuity is not
guaranteed; snapshot files need authentication and encryption; and CPU/kernel
compatibility constrains placement. Those are release requirements, not edge
cases.

Exit: fork/restore is a documented state contract, not another name for boot.
Cross-node resume survives node loss from durable artifacts, and warm-pool
identity isolation is covered by canary tests.

### Gate D: private multi-host runtime

- Move desired state and idempotency to PostgreSQL transactions with an outbox;
  run multiple API and reconciler instances.
- Add node registration, qualification labels, heartbeats, generation-fenced
  assignments, draining, rebalancing, and placement by architecture, CPU
  template, snapshot locality, policy, storage, capacity, and failure domain.
- Use reconstructable distributed leases for live routing; never make the lease
  store lifecycle truth.
- Add encrypted, tenant-scoped object storage and a qualified workspace backup
  and restore service with measured RPO/RTO.
- Support rolling controller and node upgrades, mixed-version compatibility,
  rollback, zone loss, and orphan adoption/cleanup drills.
- Implement signed webhooks or an ordered watch stream with resume cursors,
  deduplication, and delivery retry policy.

Exit: a private single-tenant fleet survives one worker and one controller loss,
and restores declared durable state within published RPO/RTO. This is still not
hostile shared multitenancy.

### Gate E: enterprise and shared-multitenant assurance

- OIDC, organizations, scoped service accounts, least-privilege roles,
  approvals, session revocation, policy administration, and external audit
  integration.
- SPIFFE or equivalent proof-of-possession workload identity, endpoint-bound
  short-lived credentials, managed secret/KMS integration, rotation, revocation,
  and snapshot/fork secret canaries.
- Method, path, scheme, host, port, DNS, redirect, response-size, rate, and
  concurrency egress enforcement with request-smuggling, rebinding, metadata,
  tunnel, and raw-socket bypass tests.
- Patch-lag SLOs for host kernel, KVM, Firecracker, guest kernel, engine, agent,
  and base images; signed release artifacts, SBOMs, provenance, rollback, and a
  vulnerability response runbook.
- External isolation review, fuzzing of API/guest/relay parsers, hostile image
  and guest corpus, side-channel statement, and noisy-neighbor qualification.
- Billing-grade metering and abuse controls separated from lifecycle truth.

Exit: the declared shared profile has independent evidence for identity,
isolation, data deletion, capacity controls, and incident recovery. Until then,
keep shared multitenancy unavailable.

## Performance and scale scorecard

Targets must be set before tuning and are not current results.

| Dimension | Minimum credible evidence | World-class release gate |
| --- | --- | --- |
| User-visible startup | Sequential, staggered, and burst create-to-first-command with raw attempts | Same workload and host show low p50 and bounded p99; cache misses and pool depletion remain explicit |
| Resume/fork | Full state and filesystem measured separately; compatibility and state integrity verified | Cross-node durable restore and high-fan-out fork maintain identity, entropy, network, secret, and output correctness |
| Warm operations | Command, file, PTY, and preview round trips measured independently | Data path avoids per-operation durable-control reads and stays stable under many concurrent streams |
| Throughput | Open-loop accepted/completed rate, queue time, and admission rejection | Linear useful throughput until a documented resource saturates; overload fails quickly and recovers |
| Tail | p90/p95/p99/max and per-stage trace | Tail regression budget survives burst and soak; no coordinated-omission benchmark |
| Density | Host baseline plus per-idle and active sandbox RSS, page cache, disk, FDs, netns/TAPs | Capacity model prevents swap storms and noisy neighbors while meeting TTI/SLO |
| Reliability | Success and confirmed cleanup in every report | No leaked VM, network, lease, workspace attachment, route, or snapshot over soak/fault matrix |
| Recovery | Controller restart, engine restart, host reboot, host loss, zone loss | Published RPO/RTO met during automated destructive drills |
| Security | API/tenant conformance and declared threat model | External review plus continuous escape, network, credential, snapshot, parser, and resource-abuse gates |

Avoid a single composite score as the primary truth. A runtime can be fast by
returning before the guest or route is usable, keeping excessive warm capacity,
weakening durability, or omitting isolation. Publish latency, reliability,
density, security profile, cache/pool policy, and cost as separate dimensions.

## Defensible product wedge

### Positioning statement

The useful wedge is not "another fast sandbox." It is a **verifiable private
agent execution runtime**:

> Run, branch, and compare agent episodes against approved private or
> open-weight model routes without giving the guest a provider credential, then
> export a signed, content-minimal evidence bundle that identifies the
> environment, policy, route, inputs, outputs, state lineage, and cleanup.

This is narrower than a general agent cloud and stronger than a VM API. It maps
to a concrete enterprise workflow: reproduce an agent failure, fan out a
bounded evaluation or rollout, compare model or policy revisions, and retain an
auditable record without moving the workload to a managed sandbox account. The
runtime remains inference-adjacent; it does not need to become a model server.

No single element is unique. The possible advantage is the integrated contract:
private deployment + mediated model identity + reproducible episodes + portable
evidence. It becomes differentiated only when all four parts are implemented
and qualified together.

### Current market capability map

This table records capabilities described by each project's official public
documentation on 2026-09-14. A blank or qualified cell means the reviewed source
does not establish that behavior, not that the provider can never support it.

| System | Official center of gravity | Lifecycle and state | Parallel/GPU adjacency | Network and credentials | Consequence for this project |
| --- | --- | --- | --- | --- | --- |
| E2B | General-purpose secure Linux VMs for agents | Memory + filesystem pause/resume, filesystem-only mode, snapshots/templates | Agent SDK surface; not an execution-evidence product in the reviewed docs | Sandbox security and access controls exist; model-route evidence is not the main abstraction | Fast Firecracker lifecycle is a substrate/table stake, and upstream engine improvements should be reused |
| Daytona | Agent/development sandboxes across container, VM, and GPU classes | OCI-derived snapshots, filesystem or hot VM snapshots, many children from a snapshot, warm pools | GPU snapshot class plus broad SDK/agent tooling | CIDR/domain/block-all controls and proxy-injected, host-scoped secret placeholders | Warm pools, OCI environments, GPU classes, egress, and secret mediation cannot be claimed as unique |
| Modal Sandboxes | Hosted serverless AI compute spanning inference, batch, training, notebooks, and sandboxes | Filesystem, directory, and alpha memory snapshots; filesystem snapshots fork into multiple sandboxes | Large asynchronous fan-out and a broad GPU fleet sit beside the sandbox product | OIDC/secrets plus an alpha sidecar pattern that can filter egress and inject secrets | Hosted GPU and batch adjacency is already strong; compete on private reproducibility and evidence, not breadth |
| Blaxel | A hosted agent cloud combining perpetual sandboxes, hosting, jobs, storage, networking, and a model gateway | Automatic standby/resume; snapshots and forking are documented as private preview | Batch jobs, agent/MCP hosting, and co-location with sandbox workloads | Per-workload allowlists, static egress identity, credential isolation, and a co-located model gateway | This is the closest breadth benchmark. Feature parity alone is not a wedge |
| AWS Lambda MicroVMs | Managed Firecracker environments for sessions, jobs, AI sandboxes, and RL environments | Dockerfile-derived pre-initialized images, suspend/resume with memory + disk, lifecycle policies | Serverless allocation; AWS positions RL and job sandboxes as direct uses | Dedicated HTTPS endpoint, JWE authentication, public/VPC egress control | Production microVM lifecycle and isolation are now hyperscaler primitives; self-hosting must add a higher-level contract |
| OpenSandbox | Open-source sandbox API/SDK/MCP over Kubernetes or Docker-style deployments | Filesystem pause/resume through an OCI capture; pluggable runtime proposals | Extensible plugins and Kubernetes scheduling | Transparent egress sidecar and credential vault keep real credentials outside the workload | Open source, policy, and secret brokering are table stakes; its fail-closed binding model is a useful baseline |
| NVIDIA OpenShell | Open-source policy sandbox for agents with container/GPU-oriented execution | Policy-controlled sandbox lifecycle rather than a Firecracker state product | GPU-aware agent runtime | Landlock/seccomp/network namespaces plus endpoint-bound provider profiles and gateway-only credentials | Strong policy and credential mediation already exist in open source; evidence and state lineage must add value |

Blaxel's docs already combine sandboxes, batch jobs, model routing, networking,
and shared storage. Modal already combines sandbox execution with high-scale batch
and GPU inference. Daytona, OpenSandbox, and OpenShell all document forms of
egress or credential mediation. E2B, Daytona, Modal, Blaxel, and AWS document
snapshot, resume, fork, or warm-start behavior. These are required product
features, not defensible headlines.

The reviewed official pages do not present a portable, signed execution receipt
as the primary user contract for a model-mediated agent rollout. That is a
promising opening, not proof of exclusivity. A broader patent, code, and product
survey would be needed before any "only" or "first" claim.

### Feature taxonomy

| Class | Capabilities | Product treatment |
| --- | --- | --- |
| Table stakes | MicroVM/container isolation; create/exec/files/ports; OCI environments; TTL; pause/resume; filesystem snapshot/fork; warm capacity; logs/metrics; SDK/CLI; explicit egress; managed secrets; durable workspace; batch fan-out | Implement or integrate them, benchmark them, and describe their limits. Do not lead with them as unique |
| Contextual strengths | Simple self-host install; operation without a managed account; Firecracker isolation; private network placement; route to customer vLLM/SGLang or provider APIs; single-host-to-fleet contract; raw conformance evidence | These can win private/regulated evaluations, but competitors offer subsets and deployment models differ |
| Defensible wedge after qualification | Immutable episode specification; endpoint-bound model-route identity; no provider secret in the guest; bounded rollout matrix; checkpoint lineage; digested inputs/outputs/evaluator; signed lifecycle, policy, route, and cleanup evidence; offline verification | Make this the product object and public demo. It answers "what ran, against which model route, under which controls, and can I reproduce it?" |

The existing `Run` schema is a good start, but it is only a schema. The current
receipt is a signed control-plane statement about sandbox lifecycle with
`InputIdentity` and `OutputIdentity` explicitly set to
`none:lifecycle-receipt`; it is not yet an execution receipt. The current
connector is a developer preview with bearer renewal. It keeps a long-lived
upstream secret outside the VM, but it is not yet proof-of-possession workload
identity. Jobs and rollouts are documented designs, not runnable services.

### Three-milestone implementation wedge

#### Milestone 1: one evidence-backed private run

Product surface: `runtime run RUN.yaml`, returning a result plus an offline-
verifiable receipt.

- Implement one bounded `Job` attempt from the existing `Run` schema before
  implementing generic workflow orchestration.
- Resolve its environment to immutable image/template, guest, policy, and
  connector revisions; reject mutable or unresolved identities.
- Bind a short-lived proof-of-possession workload identity to sandbox
  generation, connector revision, endpoint, method/path, expiry, and request
  budget. Keep the provider credential outside the guest.
- Digest declared input artifacts, command, non-secret environment, evaluator,
  declared outputs, event log, connector/model-route revision, and confirmed
  cleanup into a DSSE/in-toto statement. Preserve the current
  `signed control-plane claim; not hardware attestation` assurance label.
- Add negative conformance for redirect, DNS rebinding, direct IP, metadata,
  header/cookie smuggling, connector replay, snapshot secret persistence,
  response overflow, cancellation, timeout, failed output collection, and
  unconfirmed cleanup.
- Export the evidence bundle and public key so a disconnected verifier can
  validate signature, digests, and contract without accessing customer
  content.

Exit gate: a named Linux/KVM host runs the same immutable example twice, the
declared deterministic outputs match, credential canaries never appear inside
the guest or checkpoint, every failed path has a terminal or explicit unknown
record, and an independent verifier accepts the successful receipt and rejects
tampering.

#### Milestone 2: bounded evidence-backed rollouts

Product surface: `runtime rollout ROLLOUT.yaml`, returning an evidence matrix
instead of a pile of sandbox IDs.

- Add durable job queueing, attempt identities, maximum concurrency,
  token/time/cost budgets, stop conditions, cancellation, retries from a clean
  checkpoint, declared artifacts, and straggler handling.
- Fan out one immutable task set across explicit model-route, prompt/policy,
  or agent revisions. Do not silently select a winner or promote a model.
- Use the implemented filesystem checkpoint as a clean branch point; add
  independent full-state fork only after uniqueness, network, credential,
  external-effect, and cross-node compatibility gates pass.
- Support approved OpenAI-compatible private endpoints first, including
  customer-operated vLLM/SGLang, without bundling a GPU scheduler into the
  sandbox milestone.
- Produce a signed rollout manifest that references every episode receipt,
  evaluator revision, failures, budget consumption, and cleanup outcome.
- Add warm capacity only with fresh sandbox/workload identities and measured
  cold, cache-hit, and pool-depleted paths.

Exit gate: fault-injected fan-out is reproducible from immutable artifacts;
retry never inherits contaminated state; cancellation and budget exhaustion
stop new work and account for every attempt; evidence includes failures rather
than filtering them from the result.

#### Milestone 3: private fleet and inference adjacency

Product surface: the same Run and Rollout contracts on a private multi-host
installation, with explicit CPU-sandbox and model/GPU placement profiles.

- Complete Gates B through E: regional/node-local data path, PostgreSQL/outbox
  authority, fenced workers, encrypted object artifacts, workspace recovery,
  OIDC/RBAC, workload identity/KMS, OpenTelemetry, upgrades, DR, and external
  isolation review.
- Place CPU agent sandboxes near approved GPU model endpoints and prove route
  latency, failure, and data-boundary behavior. Treat direct sandbox GPU access
  as a separate device/isolation profile with its own qualification.
- Add topology and locality constraints without claiming that the runtime
  optimizes model serving. vLLM/SGLang or a managed endpoint remains the model
  data plane.
- Publish RPO/RTO, saturation, density, isolation, connector, and evidence-
  verification results for named releases and environments.

Exit gate: a private fleet survives a worker and controller loss, meets stated
RPO/RTO, preserves route and receipt identity across recovery, and passes an
independent security review. Shared hostile multitenancy remains a later claim
unless its separate gate passes.

### Claims ledger

| Availability point | Defensible wording | Wording that remains prohibited |
| --- | --- | --- |
| Current repository | "A self-hosted, hardened single-host Firecracker runtime release candidate for controlled private evaluation, with project-bound API access, durable host workspaces, filesystem checkpoints, a narrow credential-broker preview, and signed lifecycle receipts." | Production-ready, Blaxel-scale, fastest, safer than a named provider, zero-secret, full-state fork, rollout engine, GPU runtime, air-gapped enterprise platform |
| After milestone 1 gates | "Run a reproducible private agent task and verify a signed control-plane execution record; approved model credentials remain outside the guest in the qualified connector profile." | Hardware-attested, breach-proof, deterministic for arbitrary agents, no data leakage, fleet-ready |
| After milestone 2 gates | "Run bounded, evidence-backed agent evaluations across approved private/open-weight model routes on a qualified single-host profile." | Autonomous model promotion, universal model evaluation, multi-host durability, production SLA |
| After milestone 3 gates | "Operate the same evidence-backed Run and Rollout contract on a qualified private fleet near approved model endpoints." | Better/faster/safer than Blaxel or any competitor without matched independent measurements; hostile shared-multitenant production without its gate |

This project can earn an advantage through evidence-backed private execution,
but it does not have that advantage today. No current evidence supports a claim
that this runtime is faster, safer, more available, or more scalable than Blaxel
or another hosted platform.

## Research backlog, not immediate dependencies

- E2B's upstream runtime combines pre-booted snapshots, lazy userfaultfd memory
  loading, copy-on-write root filesystems, and local/shared template caches. The
  pinned integration should consume or contribute generic improvements there
  before maintaining a hard fork.
- Sabre reports hardware-assisted snapshot compression up to 4.5x with near-
  negligible decompression cost and up to 55% faster restoration for evaluated
  workloads. Evaluate only after the ordinary local cache and prefetch path is
  measured.
- PASS explores persistent-memory, zero-copy, on-demand restore. Aquifer explores
  CXL and RDMA memory tiers for snapshots. Both require specialized hardware and
  remain research profiles, not MVP architecture.
- Semantic checkpointing and speculative prewarming may reduce work, but only
  after ordinary lifecycle correctness, external-effect fencing, and real trace
  evidence exist.

## Primary sources

- E2B Runtime architecture: <https://github.com/e2b-dev/runtime/blob/main/docs/ARCHITECTURE.md>
- E2B sandbox overview and persistence: <https://docs.e2b.dev/> and <https://docs.e2b.dev/sandbox/persistence>
- Firecracker specification: <https://github.com/firecracker-microvm/firecracker/blob/main/SPECIFICATION.md>
- Firecracker snapshot support and security: <https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md>
- ComputeSDK benchmark methodology: <https://github.com/runloopai/computesdk-benchmarks/blob/master/METHODOLOGY.md>
- StarSling real-workload and synthetic sandbox methodology: <https://starsling.dev/hpc-sandbox-benchmarks> and <https://github.com/starslingdev/hpc-sandbox-benchmarks>
- Blaxel platform and sandbox overview: <https://docs.blaxel.ai/Overview> and <https://docs.blaxel.ai/Sandboxes/Overview>
- Blaxel jobs and networking: <https://docs.blaxel.ai/Jobs/Overview> and <https://blaxel.ai/platform/networking>
- Blaxel public runtime architecture: <https://blaxel.ai/blog/anatomy-of-a-runtime>
- Daytona snapshots and network controls: <https://www.daytona.io/docs/snapshots/> and <https://www.daytona.io/docs/en/network-limits/>
- Daytona warm pools: <https://www.daytona.io/docs/en/warm-pools/>
- Daytona endpoint-bound secrets: <https://www.daytona.io/docs/en/secrets/>
- Modal platform, snapshots, batch, GPU, and sandbox sidecars: <https://modal.com/docs/guide>, <https://modal.com/docs/guide/sandbox-snapshots>, <https://modal.com/docs/guide/batch-processing>, <https://modal.com/docs/guide/gpu>, and <https://modal.com/docs/guide/sandbox-sidecars>
- Modal startup readiness and pools: <https://modal.com/blog/unpacking-sandbox-startup-latency>
- Modal million-sandbox architecture and measurements: <https://modal.com/blog/scaling-to-1-million-concurrent-sandboxes-in-seconds>
- AWS Lambda MicroVMs: <https://docs.aws.amazon.com/lambda/latest/dg/lambda-microvms-guide.html>
- AWS Lambda MicroVMs launch architecture: <https://aws.amazon.com/blogs/aws/run-isolated-sandboxes-with-full-lifecycle-control-aws-lambda-introduces-microvms/>
- AWS Lambda SnapStart state, encryption, and uniqueness boundary: <https://docs.aws.amazon.com/lambda/latest/dg/snapstart.html>
- Kubernetes Agent Sandbox roadmap: <https://github.com/kubernetes-sigs/agent-sandbox/blob/main/roadmap.md>
- Kubernetes Agent Sandbox threat model: <https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/security/threat_model.md>
- OpenSandbox overview, pause/resume, and credential vault: <https://github.com/opensandbox-group/OpenSandbox>, <https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/guides/pause-resume.md>, and <https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/guides/credential-vault.md>
- OpenSandbox fast-runtime proposal: <https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0007-fast-sandbox-runtime-support.md>
- NVIDIA OpenShell sandbox architecture: <https://github.com/NVIDIA/OpenShell/blob/main/architecture/sandbox.md>
- NVIDIA OpenShell gateway architecture: <https://github.com/NVIDIA/OpenShell/blob/main/architecture/gateway.md>
- NVIDIA OpenShell provider credential model: <https://github.com/NVIDIA/OpenShell/blob/main/docs/sandboxes/manage-providers.mdx>
- in-toto Attestation Framework specification: <https://github.com/in-toto/attestation/tree/main/spec/v1>
- Sabre, OSDI 2024: <https://www.usenix.org/conference/osdi24/presentation/lazarev>
- PASS, USENIX ATC 2024: <https://www.usenix.org/system/files/atc24-pang.pdf>
- Aquifer, 2026: <https://arxiv.org/abs/2606.24079>
- Comparative AI sandbox security study, 2026: <https://arxiv.org/abs/2606.08433>
