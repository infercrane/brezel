# Brezel Product and Engineering Strategy

## Executive decision

Brezel should become the simplest self-hosted persistent computer for production
agents. The public product is intentionally small: create a sandbox, execute a
command, move files, expose an application, pause or resume it, fork it, and
delete it. The implementation underneath must make those operations durable,
secure, observable, and inexpensive without asking application developers to
operate Firecracker or understand snapshot mechanics.

The defensible claim is not “the fastest sandbox” in every environment. The
claim Brezel can earn is:

> **Persistent agent computers on your infrastructure. Files and processes
> survive inactivity. Credentials do not enter the guest. Every lifecycle
> outcome is known or explicitly unknown.**

The shorter product line is:

> **Create, run, resume, fork, stop. Brezel handles everything underneath.**

Performance is part of this claim, but only when paired with success rate, tail
latency, cleanup confirmation, and raw reproducible evidence. A fast accepted
request that never becomes usable, hangs during snapshot, or leaks a VM is a
failed result.

The first public release should remain a single-host, private-tenant release.
Hostile shared multitenancy must wait for independent security review and a
qualified fleet design.

## Product boundary and deliberate cuts

Brezel is a sandbox runtime, not a general agent platform. A world-class first
release is smaller than the full opportunity map. The product owns the secure
computer and the lifecycle around it; it integrates with systems above and
beside that boundary.

| Decision | Capability | Reason |
| --- | --- | --- |
| **Build now** | Create, command, streaming files, PTY/SSH, authenticated HTTP preview, OCI environment, stop/resume/delete | The irreducible sandbox experience |
| **Build now** | Durable lifecycle ledger, receipts, reconciliation, cancellation, backup/restore, upgrade/rollback | Reliability is the product |
| **Build now** | Firecracker hardening, node identity, operation capabilities, egress policy, credential broker | The security boundary cannot be delegated |
| **Build now** | Filesystem persistence, full-state standby, compatible resume, quiescent fork | The persistent-agent differentiation |
| **Build now** | Python and TypeScript SDKs, CLI, one-command private installation | Adoption surface |
| **Integrate later** | Temporal and other workflow engines | Useful above Brezel, but not runtime truth or a release blocker |
| **Integrate later** | ComputeSDK | External benchmark and discovery channel; the adapter is intentionally thin |
| **Integrate later** | OpenAI Agents, Anthropic, MCP, OpenHands | Add after the core SDK contract stabilizes and only with conformance tests |
| **Defer** | Desktop streaming and first-party browser automation | Separate product category and large support surface |
| **Defer** | GPU passthrough | Different isolation, cleanup, scheduling, and economics profile |
| **Defer** | Shared mutable distributed drive | Coherency and cross-tenant risk; workspaces cover the first customer jobs |
| **Defer** | Multi-region continuity and live migration | Premature before one region is operationally correct |
| **Do not build** | Agent reasoning, prompting, memory, evaluation, or workflow DSL | Frameworks already own this layer |
| **Do not build** | Model serving or a general AI gateway | Brezel may offer private routes but should not become an inference provider |
| **Do not build** | Custom scheduler, database, object store, PKI, or secrets manager when maintained components fit | These are dependencies, not differentiation |
| **Do not claim** | Universal fastest, zero risk, or hostile multitenancy before reproduced evidence | Trust is more valuable than an unsupported headline |

Batch jobs and schedules should initially be small lifecycle conveniences over
the same sandbox API, not a new orchestration language. Raw TCP, public unauthenticated
ingress, shared tenancy, confidential computing, and accelerators each require
their own explicit profile and customer evidence before inclusion.

## Current evidence and boundary

Revision `67410ab5b928a335a79701d67eaf859df890da9c` is a self-hosted
private-tenant developer preview. It has a meaningful foundation:

- a Firecracker microVM boundary with no release-mode container fallback;
- create, command, file, preview, pause, resume, filesystem checkpoint,
  workspace, expiration, deletion, and reconciliation paths;
- project-bound credentials, idempotency, quotas, bounded inputs and outputs,
  and content-minimal receipts;
- a packaged node relay with TLS 1.3 mutual authentication, exact workload
  identities, signed single-operation capabilities, replay rejection, route
  generation fencing, and operation leases; and
- destructive conformance and benchmark tooling that records the host,
  revision, attempts, failures, percentiles, throughput, and cleanup results.

It does not have a public-production, hostile-multitenant, cluster, or
high-availability claim. The node relay is the default command, file, and
preview byte path, and SQLite is the single-host lifecycle ledger. Both paths
completed named Linux/KVM qualification at the revision above, including a
paired run on two separately administered single-host installations. SQLite
remains an embedded single-controller authority, not a fleet-scale ledger.

The final revision's two complete matrices passed 48 of 48 cells, 2,112 of
2,112 attempts, and 3,168 of 3,168 expected resource cleanups. Sequential
cached create through first verified instruction measured 70.457–70.957 ms p50
and 79.553–80.804 ms p95 across the two hosts. Their 16-way burst p50 was
359.690–384.931 ms. A separate stress set completed 320 of 320 immediate-command
burst attempts with confirmed cleanup and a pooled 812.188 ms p99.

That evidence establishes correctness and host-local latency for the tested
profile, not a broad speed claim. Checkpoint burst p95 remained approximately
12.8–13.0 seconds and restore behavior varied materially between hosts. A later
separately sized profile at revision
`f9fbc0ede72636349b27f01db49343d8daa87c5c` cleared Brezel's internal 100-way
functional gate with 1,000 of 1,000 command-ready executions and confirmed
cleanup, although its 2.431 s p50 is not leaderboard-leading. Exact host
observations, hard-reset limits, raw evidence, and external-comparison gates are
in [Qualification 2026-09-15](QUALIFICATION-2026-09-15.md) and
[Benchmarking](BENCHMARKING.md).

## User jobs

### Required product surface

The first public SDK should make the ordinary path fit in four concepts:

```python
sandbox = client.sandboxes.create(image="python:3.13")
result = sandbox.run("python agent.py")
url = sandbox.expose(3000)
sandbox.stop()
```

Advanced controls remain available without becoming mandatory. The full user
job set is larger than create and exec:

| Job | Required behavior |
| --- | --- |
| Start | Immutable OCI-derived environment, deterministic readiness, idempotent create |
| Execute | Streaming stdout/stderr, real exit status, deadline, signal, cancel, detach, reattach |
| Interact | PTY resize, SSH, stdin, WebSocket/SSE preview, authenticated public or private ingress |
| Move data | Streaming file and directory transfer, range/resume, digest verification, bounded memory |
| Persist | Durable workspace, full-state standby, filesystem archive, transparent compatible resume |
| Branch | Fast fork from a quiescent checkpoint, lineage, independent cleanup, explicit external-effect boundary |
| Connect | Deny-by-default egress, private network, fixed outbound identity, domain-scoped credential broker |
| Operate | TTL, idle standby, schedules, webhooks, quotas, usage, audit, backup, restore, drain, upgrade |
| Diagnose | Stable error taxonomy, operation status, state history, metrics, redacted support bundle |
| Integrate | Python, TypeScript, CLI, OpenAI Agents, Temporal, MCP, ComputeSDK |

### Important needs missing from the original list

1. **Stable process semantics.** A production sandbox needs attached and
   detached processes, stdin closure, signals, PTY resize, reconnect cursors,
   bounded output retention, and an unambiguous final exit state. “The HTTP
   request ended” cannot mean “the process stopped.”

2. **Useful readiness.** `create()` should complete only after the guest agent,
   filesystem, policy, and requested routes are usable. Every result should
   distinguish admitted, scheduled, booted, guest-ready, and application-ready.

3. **Streaming data paths.** Files, preview traffic, WebSockets, SSE, and PTYs
   must use bounded streaming with backpressure. A 64 MiB limit is not a license
   to buffer 64 MiB per concurrent request.

4. **Reproducible environments.** Resolve mutable OCI tags to manifests,
   preserve build logs, scan inputs, generate an SBOM and provenance, sign the
   resulting environment, and reject an unknown or incompatible artifact.

5. **Fork correctness.** Fork is valuable for parallel agent attempts, but it
   must declare whether memory, filesystem, running processes, clocks, random
   state, network identity, and unresolved external effects are copied.
   Firecracker itself requires the integrator to handle snapshot files,
   compatibility, network continuity, and restored identity safely.[^1]

6. **Ingress that wakes safely.** Preview routes need optional wake-on-request,
   bounded queues, rate limits, revocation, custom domains, and clear behavior
   for connections that cannot survive standby.

7. **Operational visibility without content collection.** Users need resource
   usage, state transitions, durations, failures, and receipt identities. Source,
   prompts, terminal output, and artifacts should remain tenant data rather than
   default platform telemetry.

8. **Resource and spend control.** Enforce CPU, memory, PIDs, disk bytes, IOPS,
   bandwidth, connections, snapshot storage, wake rate, fork fan-out, and model
   spend independently. Admission should reject before overload becomes a host
   failure.

9. **Compatibility and migration.** Publish a feature matrix for image support,
   architectures, snapshot compatibility, networks, and SDK versions. An
   environment or checkpoint upgrade needs a clear migrate, rebuild, or reject
   outcome.

10. **Enterprise evidence.** Organizations, service accounts, RBAC, OIDC,
    audit export, retention, deletion evidence, customer-managed encryption,
    data residency, and an air-gapped artifact mirror are later milestones, but
    their identities must be present in the resource model now.

11. **Operator ergonomics.** `brezel doctor`, capacity diagnostics, preflight,
    support bundles, safe drain, rollback, restore verification, and cleanup
    inspection are product features, not internal conveniences.

12. **Agent-framework fit.** Agents need a persistent execution session and
    an idempotent external-effects contract. Brezel should expose receipt and
    artifact references rather than forcing frameworks to put large logs or
    binary data in their own workflow histories.

## Target architecture

```text
Agent framework / customer application
                 |
      Python · TypeScript · CLI
                 |
        identity + policy API
                 |
  durable desired state + operation ledger
                 |
     scheduler / reconciler / outbox
                 |
        authenticated data edge
                 |
    mTLS Brezel node relay on each host
                 |
 Firecracker jailer + guest agent + local caches
                 |
          isolated agent computer
```

Command, file, terminal, and preview bytes should travel through the node relay
after API admission. The durable API persists desired state and receipts but
does not proxy high-volume customer content. The relay accepts only a current
route generation and a short-lived capability for one exact operation.

For the single-host release, use an embedded transactional lifecycle ledger in
WAL mode and a transactional outbox. Keep immutable artifacts and checkpoint
payloads out of that database. The current closure-over-an-entire-state-file API
must be replaced by resource-scoped transactions; placing SQLite behind the
same whole-state closure would preserve the expensive clone and validation
behavior.

Cluster mode can later map the same repository interfaces to PostgreSQL,
Redis for reconstructable leases, and object storage. It must not introduce a
second lifecycle truth.

## Optional Temporal adapter

Temporal is an excellent optional orchestration adapter and the wrong place for
Brezel's node lifecycle truth. Temporal's current OpenAI Agents integration
dispatches sandbox create, command, file, and PTY operations as Activities and
serializes sandbox session state with the Workflow. It supports pluggable
`SandboxClientProvider` implementations.[^2]

The Brezel integration should look like this:

```text
Temporal Workflow      plan · wait · schedule · approve · fan out · judge
        |
Temporal Activity      brezel.create/run/read/write/fork/stop
        |
Brezel API              idempotency · desired state · operation receipt
        |
Brezel node             command/file/PTY/preview bytes · Firecracker lifecycle
```

The implementation rules are:

- map Temporal Workflow, Run, and Activity IDs to a canonical Brezel
  idempotency key and bind that key to the request digest;
- map Activity cancellation and heartbeats to a durable Brezel cancellation
  intent, not merely HTTP disconnect;
- return small operation, artifact, and receipt references; keep streamed
  output and large content outside Workflow history;
- resolve credentials worker-side or through Brezel's credential broker;
  Temporal explicitly warns that Activity and provider factory arguments can
  enter Workflow history;[^3]
- handle Activity retries as at-least-once delivery and never repeat an
  unresolved external effect silently; and
- keep Temporal optional so a disconnected enterprise install remains fully
  functional.

This is not on the critical path. Build a thin adapter only after the Python SDK,
idempotency, cancellation, and artifact-reference contracts are stable, and
only when a design partner or upstream integration justifies its maintenance.
The provider can then serve as both an adoption path and a demanding external
test of Brezel's retry semantics without turning Brezel into a workflow engine.

## Performance program before paid hosts

### Control path

1. Replace the whole-state JSON transaction with resource-scoped transactional
   operations. Use an embedded SQLite WAL implementation for the single-host
   profile, with foreign keys, monotonic revisions, operation receipts, an
   outbox, schema migrations, online backup, and fail-closed integrity checks.
2. Keep keyed reads and add keyed updates for sandbox activity, operation state,
   route placement, events, and idempotency records. Do not validate or copy
   unrelated tenants and resources.
3. Coalesce activity deadlines in memory and persist a bounded heartbeat or
   lifecycle boundary. File, terminal, and preview chunks must not cause an
   `fsync`.
4. Keep a persistent TLS/HTTP2 or Connect transport pool between API edge and
   node. Parse certificates and verification key sets on rotation, not on each
   operation.
5. Batch content-minimal receipts and outbox publication without acknowledging
   a lifecycle transition before its durable commit.

### Node and guest path

1. Make the relay the default byte path and stream file uploads, file downloads,
   preview request/response bodies, PTYs, WebSockets, and SSE with bounded
   buffers and cancellation propagation.
2. Preallocate network namespaces, TAP devices, address leases, and common
   cgroup skeletons. Reset and verify them before reuse; never reuse uncertain
   state.
3. Use a minimal guest init and guest agent. Remove unneeded services, probe
   readiness over vsock, and keep guest boot logs bounded.
4. Maintain a local content-addressed cache for verified OCI layers, kernels,
   root filesystems, snapshot manifests, and hot snapshot chunks. Cache keys
   must bind every compatibility and tenant-isolation input.
5. Split warm capacity into three explicit modes: pre-created host plumbing,
   booted clean templates, and resumable customer snapshots. Measure cost and
   security separately instead of calling all three “warm.”
6. Trace first-use memory pages during representative workloads and prefetch
   only the stable working set. Firecracker supports userspace page-fault
   handling with `userfaultfd`, enabling measured lazy restore strategies.[^4]
7. Evaluate compression only after the prefetch baseline. Sabre reports up to
   4.5× snapshot compression and up to 55% restore improvement using specific
   hardware acceleration; those results identify a research path, not a Brezel
   claim.[^5]
8. Pin vCPUs and Firecracker processes deliberately on NUMA hosts, isolate noisy
   host services, reserve memory, and measure disk queueing. Do not apply host
   tuning without A/B/A/B evidence.

### Local gates

Before renting a KVM host, a candidate should pass:

- unit, race, contract, fuzz, and security-negative tests;
- default relay integration tests with process restart and stale-capability
  rejection;
- a 64 MiB transfer test showing bounded resident-memory growth;
- a lifecycle-store benchmark at 100, 1,000, and 10,000 resources showing
  constant-scale keyed updates rather than whole-state scaling;
- database crash tests at each transaction boundary;
- cancellation tests that end with no process, route, lease, or counter leak;
- deterministic schema upgrade and rollback tests; and
- `go test` leak and long-running contention profiles with saved `pprof` and
  trace artifacts.

These gates cannot prove Firecracker behavior on macOS. They eliminate known
software waste so paid Linux/KVM time measures the candidate that might ship.

## Reliability contract

Brezel should publish a small set of invariants before publishing latency:

1. A sandbox is `running` only after a fresh authenticated guest readiness
   proof.
2. Every durable resource-lifecycle mutation has an idempotency identity bound
   to its canonical input. Command execution, file writes, and ephemeral preview
   leases are excluded until they have their own explicit replay semantics.
3. A timeout has one of three explicit outcomes: not started, completed with a
   durable receipt, or unknown and being reconciled.
4. Delete intent wins over create, resume, fork, and retry.
5. Cancelled work reaches a durable terminal or cleanup-pending state.
6. A node generation never moves backward, including after restore.
7. A node restart invalidates capabilities from the prior boot.
8. No API process restart can create a phantom `running` sandbox.
9. No acknowledged checkpoint exists without an authenticated, checksummed
   manifest and durable payload for its declared profile.
10. No successful delete leaves an allocated VM, route, lease, workspace
    attachment, snapshot reference, TAP device, mount, or cgroup.

### Failure test matrix

Automate failure at every boundary:

| Failure | Required proof |
| --- | --- |
| API killed before/after commit | idempotent recovery, no duplicate VM |
| Node killed during command/file/preview | stream closes, operation reconciles, lease releases |
| Firecracker unresponsive | watchdog terminates it and cleanup remains visible |
| Snapshot request returns late or response is lost | no false standby/checkpoint state |
| Disk full or quota reached | fail before corruption, preserve deletion capacity |
| Short write, `fsync`, rename, WAL checkpoint, backup failure | prior committed state remains readable |
| Host reboot | desired state and node ledger reconcile without generation rollback |
| Certificate or signing-key rotation interrupted | old/new overlap is bounded and recoverable |
| Upgrade interrupted | service rolls forward or back with schema compatibility checked |
| Object store unavailable | no checkpoint or archive success until durability is proven |
| Cancellation races create/resume/pause/delete | delete precedence and zero leaked resources |
| Clock moves | monotonic durations and bounded credential lifetime remain safe |
| Dependency or DNS failure | deadlines, circuit breaking, and bounded queues prevent cascading failure |

Use property-based state-machine tests for lifecycle sequences and fault
injection around every durable boundary. Run 24-hour and 72-hour soak tests that
track processes, file descriptors, cgroups, namespaces, TAP devices, mounts,
ports, disk use, and memory. The test should fail on drift, not merely print it.

## Security program

### Host and microVM

Follow Firecracker's production guidance as a release gate: the jailer or a
strictly stronger boundary, default seccomp filters, a unique unprivileged
UID/GID per microVM, trusted immutable parent paths, cgroup and resource limits,
bounded logs and serial output, current host/guest kernels and microcode, and
disabled or encrypted swap.[^6] Add a host watchdog that can terminate an
unresponsive Firecracker process and continue cleanup.

Define two honest scheduling profiles:

- **private tenant:** SMT may remain available under the customer's accepted
  risk and capacity policy;
- **reviewed shared tenant:** dedicated cores or SMT disabled, strict device and
  cache policy, stronger host isolation, and no release until independent
  review.

### Identity, capabilities, and credentials

- Enroll nodes from one-time bootstrap material and issue short-lived workload
  certificates. Rotate certificates automatically with bounded overlap and
  explicit revocation.
- Publish signing-key sets with `not-before` and retirement times. A capability
  binds key ID, route generation, exact operation, canonical request digest,
  byte/time limits, audience, and node boot identity.
- Use proof-of-possession workload identity for credential renewal. Never place
  long-lived cloud, model, source-control, or signing credentials in images,
  environment variables, command arguments, snapshots, logs, or receipts.
- Enforce semantic HTTP policy outside the guest. Resolve DNS independently,
  pin the admitted address for the connection, and revalidate every redirect.
  Block metadata, loopback, link-local, multicast, unspecified, private, and
  rebinding targets unless the exact private route is explicitly authorized.
- Treat raw TCP/UDP as a separate, weaker capability. Response scrubbing cannot
  prevent arbitrary exfiltration over an encrypted tunnel.

### Files, snapshots, and artifacts

- Resolve guest paths by file descriptor using `openat2`-style constraints on
  Linux. Test symlink, hardlink, rename, mount, and time-of-check/time-of-use
  races.
- Reject archive traversal, decompression bombs, sparse-file quota bypass,
  malformed OCI layers, special devices, setuid content, and unsafe ownership
  changes.
- Authenticate and encrypt checkpoint memory and disks per tenant. Bind source,
  image, kernel, guest, CPU compatibility, policy, network identity, entropy
  generation, parent lineage, and unresolved external effects.
- Generate fresh network identity and randomness on restore or fork. Do not copy
  credential leases into a reusable snapshot.
- Produce SBOMs and signed provenance for release artifacts and environment
  builds. SLSA 1.2 distinguishes basic provenance, signed hosted provenance, and
  hardened isolated builds; Brezel should target a hosted signed release first
  and a hardened build process before a shared-tenant claim.[^7]

### Agent-specific abuse

MicroVM isolation protects the host; it does not decide whether an agent should
send an email, publish data, or invoke a production tool. Add a deterministic
capability boundary above execution:

- minimize visible tools and accessible data per task;
- distinguish read, write, external side effect, credential use, and approval;
- bind approvals to exact canonical operations and expiration;
- record content-minimal evidence of enforcement and externally visible effect;
- use canary credentials and protected objects in adversarial tests; and
- keep learned policy advisory until deterministic policy or a human approves
  it.

Recent agent-security work reinforces this distinction. SafeClawBench separates
semantic acceptance, audit-visible evidence, and actual sandbox-observed harm;
its results show that a semantic pass does not imply the absence of executable
harm.[^8] Brezel's security benchmark should likewise test real filesystem,
network, credential, persistence, and cross-user outcomes rather than model text.

### Assurance work

- fuzz the public API, guest protocol, node relay, OCI parser, archive ingest,
  and snapshot manifest parser;
- use `syzkaller` or targeted kernel fuzzing for the qualified host/guest/kernel
  surface where practical;
- maintain a patch-lag policy and automated component/VEX inventory;
- sign releases and publish SBOM, provenance, checksums, and verification
  instructions;
- run a threat-model-led external assessment before shared multitenancy;
- publish a coordinated vulnerability policy and later a bounty; and
- never call a runtime-signed receipt hardware attestation.

## Benchmark and qualification program

### Evidence levels

| Level | Purpose | Environment | Publishable claim |
| --- | --- | --- | --- |
| L0 | microbench and regression | macOS/Linux developer host | implementation evidence only |
| L1 | functional microVM qualification | dedicated Ubuntu/KVM host | named single-host compatibility |
| L2 | A/B/A/B performance | same dedicated NVMe host | revision-specific improvement |
| L3 | external comparability | ComputeSDK and StarSling-compatible harnesses | same-workload provider comparison |
| L4 | reliability | fault injection, reboot, 24/72-hour soak | recovery and leak invariants |
| L5 | security | adversarial conformance and external review | only the reviewed deployment profile |

### Required metric families

- cold, cached-template, pre-plumbed, warm-template, and standby-resume time to
  first verified instruction;
- warm command and PTY round trip;
- file upload/download at 1 KiB, 1 MiB, 64 MiB, directory/tar, and many-small-file
  shapes;
- preview first byte, sustained HTTP, WebSocket/SSE reconnect, and wake-on-request;
- filesystem checkpoint, full-state checkpoint, live fork, snapshot fork, and
  first useful read/command after each;
- create/cancel, command/cancel, snapshot interruption, delete, and cleanup time;
- CPU, memory bandwidth, disk latency/IOPS, hardlink behavior, loopback and WAN
  networking, Git, package install, SQLite/PostgreSQL, and web-tooling workloads;
- host density, noisy-neighbor effect, offered-load saturation, success rate,
  queue delay, tail latency, and recovery latency; and
- real coding-agent workloads with pinned repositories and toolchains.

ComputeSDK's methodology correctly uses identical workloads, repeated runs,
median and tail latency, success-rate-adjusted scoring, and public raw JSON. Its
Burst TTI test creates 100 sandboxes concurrently and times create through the
first successful command.[^9] StarSling complements that with cold runs across
pinned real repositories and convergent Phoronix suites on equal advertised
resource shapes.[^10]

Brezel should integrate with both after the current path qualifies. The native
harness remains necessary because it validates Brezel-specific lifecycle,
standby, receipt, cancellation, and cleanup semantics that a generic leaderboard
cannot see.

### Publication rules

Every public result must include:

- exact source revision, dirty state, engine lock, build provenance, binary and
  image digests;
- provider, region, host, CPU topology, memory, storage, kernel, KVM, network,
  resource shape, cache state, and time;
- offered-load schedule and all raw attempts;
- p50, p95, p99, maximum, success, failure, censored latency, throughput, and
  cleanup confirmation;
- separate client queue and service time to avoid coordinated omission;
- two independent repetitions and an A/B/A/B sequence for optimization claims;
- the exact timed boundary; and
- an explicit statement that one environment is not a universal claim.

The headline score should be a reliability-adjusted useful-work metric. A
candidate with faster p50 but worse p99, success, or cleanup is rejected.

## Delivery sequence

### Milestone A: self-hosted private-tenant developer preview

Delivered at revision `67410ab5b928a335a79701d67eaf859df890da9c`:

1. Resource-scoped SQLite lifecycle ledger with migration from protected legacy
   state, durable idempotency, events, receipts, and reconciliation.
2. Desired-state route reconciliation, safe install drain, and default relay
   byte paths for bounded streaming command, file, and HTTP traffic.
3. One-command installation with pinned engine inputs and live runtime
   attestation.
4. Named-host destructive qualification on two separately administered
   single-host installations, including simultaneous conformance, confirmed
   cleanup, namespace-negative checks, and controller/node-relay crash drills.
5. Exact-revision provider-reset drills on both hosts demonstrating six-file
   replacement-sandbox workspace recovery. Original sandboxes became failed, so
   this is not transparent resume, process continuity, or host-loss recovery.
6. Two complete 24-cell matrices with 2,112 of 2,112 attempts and 3,168 of
   3,168 expected resource cleanups, plus 320 of 320 focused
   immediate-command burst attempts with cleanup confirmed.

Still required before the complete-agent-computer milestone can carry a broader
operational label:

1. Node enrollment, online certificate rotation, capability-key rotation, and
   service-unit upgrade/rollback packaging.
2. Real Python and TypeScript SDKs with streaming, cancellation, retries, and
   stable errors.
3. PTY/SSH and arbitrary OCI-derived environments with signed immutable
   manifests.
4. Reduce and explain checkpoint/restore burst tails, and improve the qualified
   100-way 2.431 s p50 before ComputeSDK submission.
5. Publish disk-full, interrupted-upgrade, backup/restore, rollback, and longer
   soak evidence.

Current release label: **self-hosted private-tenant developer preview**.

### Milestone B: best persistent agent experience

1. Full-state standby and compatible resume with explicit network reconnection.
2. Fast quiescent fork with lineage and external-effect fencing.
3. Wake-on-request previews with WebSocket/SSE and bounded admission.
4. Content-addressed NVMe cache, pre-created host plumbing, warm templates, and
   workload-traced snapshot prefetch.
5. Bounded jobs, TTLs, scheduled wake/stop, cancellation, and artifact
   collection as small lifecycle primitives, not a workflow engine.
6. ComputeSDK qualification after capacity and reliability entry gates pass.
   Add at most one agent-framework adapter requested
   by a design partner; Temporal, MCP, and other adapters remain optional.
7. 24/72-hour soak, resource-exhaustion, interrupted-upgrade, and node-reboot
   evidence.

Release label: **persistent agent computer for private production workloads**.

### Milestone C: fleet and enterprise

1. Multi-node placement, node-loss recovery, immutable object-backed snapshots,
   and workspace backup/restore.
2. Private networking, fixed egress, workload identity, managed secret
   providers, customer-managed keys, and retention/deletion controls.
3. OIDC, organizations, service accounts, RBAC, approvals, quotas, audit export,
   and policy bundles.
4. Region and failure-domain policy, capacity planning, rolling upgrades, and
   disaster exercises.
5. Air-gapped installation and signed artifact mirrors.

Release label: **private enterprise sandbox platform**.

### Milestone D: reviewed shared tenancy

1. Dedicated multitenant isolation profile, side-channel policy, cache and
   device separation, and tenant-aware admission.
2. Independent security assessment and remediation.
3. Continuous adversarial conformance, escape response exercises, and published
   review scope.

Release label: **shared multitenant** only after the evidence exists.

## How Brezel earns “best”

“Best” should be a narrowly defined, independently reproducible result:

> **The simplest self-hosted persistent sandbox for agents, with the strongest
> published lifecycle correctness evidence in its class.**

Earn it with five public proof objects:

1. **Five-minute start:** one documented install and a first useful command on
   a supported dedicated host.
2. **Useful-work benchmark:** ComputeSDK and real repository pipelines with raw
   data, success-adjusted p50/p95/p99, and no tuned private path.[^9]
3. **Persistence demonstration:** a real process and filesystem survive standby,
   reconnect correctly, fork independently, and expose an application again.
4. **Failure report:** at least 10,000 mixed lifecycle and cancellation
   operations, host and service restarts, disk-full tests, and zero unaccounted
   resources. Publish every failure and cleanup outcome.
5. **Security case:** threat model, hardening profile, SBOM, provenance,
   capability tests, credential canaries, and an independent review.

Do not publish a latency target as achieved until it is reproduced. A good
launch page can show live, revision-bound evidence instead of a static “fastest”
badge: time to interactive, standby resume, success rate, leak count, last
qualified commit, and the downloadable raw report.

## Paid-host qualification protocol

Revision `67410ab5b928a335a79701d67eaf859df890da9c` has crossed the first paid-host
gate on two separately administered Ubuntu 24.04 x86-64 KVM machines. Future
paid-host runs retain the same protocol:

1. provision clean supported hosts and pin SSH host identities;
2. record provider profile, region, CPU, memory, storage, kernel, and exact
   revision without publishing hostnames, IP addresses, or machine serials;
3. verify clean source and the live runtime-attestation manifest before work;
4. run simultaneous conformance, benchmark, isolation, cleanup, and bounded
   failure drills appropriate to the intended claim;
5. use baseline/candidate/candidate/baseline matrices for optimization claims;
6. download the complete evidence directory and verify checksums; and
7. delete rented servers, not merely power them off.

If qualification fails, preserve checksummed evidence, reconcile every
resource, and fix locally before allocating another host. A paired-host run does
not establish a cluster, shared state, automatic failover, or high availability.

## Sources

[^1]: Firecracker project. “[Snapshotting](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md).” Accessed September 14, 2026.
[^2]: Temporal Technologies. “[OpenAI Agents SDK Integration for Temporal: Sandbox Support](https://github.com/temporalio/sdk-python/blob/main/temporalio/contrib/openai_agents/README.md#sandbox-support).” Pre-release documentation, accessed September 14, 2026.
[^3]: Temporal Technologies. “[OpenAI Agents SDK Integration for Temporal: Factory Arguments and Secrets](https://github.com/temporalio/sdk-python/blob/main/temporalio/contrib/openai_agents/README.md#factory-arguments).” Accessed September 14, 2026.
[^4]: Firecracker project. “[Handling page faults on snapshot resume](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md).” Accessed September 14, 2026.
[^5]: Nikita Lazarev et al. “[Sabre: Hardware-Accelerated Snapshot Compression for Serverless MicroVMs](https://www.usenix.org/conference/osdi24/presentation/lazarev).” USENIX OSDI 2024.
[^6]: Firecracker project. “[Production Host Setup Recommendations](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md).” Accessed September 14, 2026.
[^7]: OpenSSF. “[SLSA Specification v1.2](https://slsa.dev/spec/v1.2/).” Accessed September 14, 2026.
[^8]: Yuchuan Tian et al. “[SafeClawBench: Separating Semantic, Audit-Evidence, and Sandbox Harm in Tool-Using LLM Agents](https://arxiv.org/abs/2606.18356).” June 2026 preprint.
[^9]: ComputeSDK. “[How the Numbers Are Made](https://www.computesdk.com/methodology/).” Accessed September 14, 2026.
[^10]: StarSling. “[Firecracker, libkrun & sandbox performance benchmarks](https://starsling.dev/hpc-sandbox-benchmarks).” Run dated August 6, 2026; accessed September 14, 2026.
[^11]: Blaxel. “[Sandbox lifecycle](https://docs.blaxel.ai/Sandboxes/Overview#sandbox-lifecycle).” Accessed September 14, 2026. Blaxel documents automatic standby, process and filesystem preservation, and sub-25 ms resume as its managed-service behavior; these are product references, not Brezel results.
[^12]: Modal. “[Sandbox snapshots](https://modal.com/docs/guide/sandbox-snapshots).” Accessed September 14, 2026. The documented limitations illustrate why checkpoint compatibility and lifecycle outcomes must be explicit.
