# Status

This document is the claim boundary for Brezel. Unit tests establish software
behavior; only the destructive qualification suite establishes behavior on a
named Firecracker host.

## Current label

**Self-hosted private-tenant developer preview.**

The supported evaluation profile is one organization on one dedicated Ubuntu
24.04 x86-64 machine with KVM and `/dev/net/tun`. Revision
`67410ab5b928a335a79701d67eaf859df890da9c` completed destructive qualification
on two separately administered machines, each operating as an independent
single-host deployment. Revision `f9fbc0ede72636349b27f01db49343d8daa87c5c`
also qualified dedicated 100-way Burst and 8-vCPU/16-GiB DAX profiles on one
named GCP KVM host. It must not be described as a cluster, highly available,
hostile shared-multitenant, or public-production system.

## Implemented

| Capability | Current boundary |
| --- | --- |
| Environments | Immutable template revisions; arbitrary OCI builds are not available |
| Sandboxes | Project-scoped create, inspect, list, pause, resume, expiration, and delete |
| Commands | Streamed output, bounded response, deadline, and confirmed exit status; an interrupted stream reconnects to the same process through a bounded generation-bound cursor journal and never reruns the command; unavailable or evicted suffixes fail closed |
| Files | Authenticated guest path, absolute-path validation, bounded upload/download |
| HTTP previews | Opaque 30–900 second lease, state recheck, credential stripping; no WebSockets |
| Workspaces | Host-backed, durable across sandbox replacement, single writer |
| Checkpoints | Filesystem capture and restore with lineage and deletion protection |
| Automatic standby | Activity and grace deadlines, operation fencing, restart recovery |
| Network and connectors | Deny by default; explicit unrestricted-internet opt-in; narrow private model/tool connector preview |
| Authentication | Protected token digests bound to explicit projects |
| Capacity | Per-project resource limits plus a process-wide admission ceiling |
| Warm capacity | Optional crash-recoverable, single-use clean slots for one exact template/network class; customer sandboxes are destroyed after their first claim; Linux/KVM performance qualification remains pending |
| State | Private SQLite WAL ledger, exclusive controller lock, semantic validation, legacy JSON import, and row-scoped guest hot paths |
| Recovery | Durable resource-lifecycle idempotency, lifecycle events, cleanup intent, expiration reconciliation |
| Evidence | Ed25519-signed DSSE lifecycle receipt; no customer content by default |
| Distribution | Pinned source, patches, images, VM artifacts, installer, conformance, benchmark harness |
| Node relay | The single-host package defaults command, file, and preview traffic to separate mTLS control and data listeners with route lifecycle CAS, one-operation capabilities, replay defense, generation fencing, reconciliation, and in-flight leases; the integrated path completed named-host Linux/KVM qualification at revision `67410ab5b928a335a79701d67eaf859df890da9c` |

Release code has no fake backend and no container isolation fallback. The test
backend exists only in `_test.go` files. A remote engine requires TLS; service
and engine credentials are read from protected files.

## Not implemented

- arbitrary OCI environment builds
- interactive PTY, SSH, desktop, or WebSocket transport
- public full-state checkpoint and fork operations
- Python and TypeScript SDKs
- node enrollment, online certificate and signing-key rotation, durable node-operation receipts, and service-unit upgrade or rollback packaging
- node-local snapshot prefetch and multi-class capacity scheduling
- OIDC, organizations, RBAC, approvals, or dynamic quota administration
- multiple nodes, replicated state, workspace backup/restore, or disaster recovery
- GPU passthrough, air-gapped support, or hardware attestation
- hostile shared-multitenant isolation assurance

## Qualification

The repository includes tests for lifecycle, idempotency, cross-project denial,
command and file paths, authenticated preview, workspace persistence and
exclusivity, checkpoint restore, cleanup, receipt verification, API restart,
and repeated qualification. The installer fails closed unless the pinned engine
exposes the expected COW rootfs, UFFD restore, prefetch, cached artifacts, and
ready network namespace capabilities.

Revision `67410ab5b928a335a79701d67eaf859df890da9c` completed the destructive
workflow on each of two separately administered Ubuntu 24.04 x86-64 KVM hosts.
The paired run covered simultaneous conformance, cached-template burst
observations for time to first verified instruction, filesystem restore, and
workspace I/O, cross-host namespace-negative checks, confirmed cleanup, and
controller/node-relay crash containment. See [Qualification
2026-09-15](QUALIFICATION-2026-09-15.md). The earlier four-core Xeon result
remains historical evidence in [Qualification
2026-09-13](QUALIFICATION-2026-09-13.md).

Each final-revision host also passed a provider-reset drill. The original
sandbox became honestly `failed`; a replacement sandbox recovered the complete
six-file crash-consistency corpus from the same host-backed workspace, followed
by confirmed cleanup. This is same-host crash-durable workspace recovery, not
transparent process resume, backup, or host-loss durability.

## Performance evidence

Both complete 24-cell matrices passed. Together they completed 48 of 48 cells,
2,112 of 2,112 attempts, and 3,168 of 3,168 expected resource cleanups.
Selected host-local ranges across the two machines were:

| Scenario | p50 | p95 | Success |
| --- | ---: | ---: | ---: |
| Sequential cached create through verified instruction | 70.457–70.957 ms | 79.553–80.804 ms | 200 / 200 |
| 16-way burst cached create through verified instruction | 359.690–384.931 ms | 603.629–609.767 ms | 32 / 32 |

A separate immediate-command stress run completed 320 of 320 attempts with
confirmed cleanup and a pooled 436.508 ms p50, 709.198 ms p95, and 812.188 ms
p99. Checkpoint burst p95 nevertheless remained approximately 12.8–13.0
seconds, and filesystem-restore burst behavior varied materially between hosts.
These are reliable host-local observations, not a portable or competitive
performance claim. Finite non-recurrence does not prove exactly-once command
execution or eliminate every possible indeterminate stream outcome. Exact
figures and raw evidence are in [Qualification
2026-09-15](QUALIFICATION-2026-09-15.md) and the public-comparison gates are in
[Benchmarking](BENCHMARKING.md).

The separately sized GCP profiles at revision
`f9fbc0ede72636349b27f01db49343d8daa87c5c` produced:

| Rehearsal | Result | Success and cleanup |
| --- | ---: | ---: |
| 100-way Burst TTI, 10 consecutive waves | 2.431 s p50 / 2.937 s p95 / 3.182 s p99 | 1,000 / 1,000; 1,000 / 1,000 deletions |
| Pinned ComputeSDK DAX, guest workload total | 63.272 s median | 3 / 3; 3 / 3 deletions |

The Burst runner was a neutral macOS HTTPS client. DAX used a fresh
8-vCPU/16-GiB sandbox for every iteration, ran the digest-pinned upstream
script, and allowed internet access only for that disposable benchmark project.
Both projects were empty before and after the runs. These are self-run
rehearsals, not official ComputeSDK leaderboard entries.

## Next release gate

The separate node process, default single-host byte path, row-scoped lifecycle
operations, bounded snapshot-diff cache, readiness gate, and three-start
admission default completed the named-host workflow. Candidate profiles now
exist for DAX and 100 simultaneous command-ready sandboxes, together with
strict workload and cleanup rehearsals. Both passed on the named GCP KVM host at
revision `f9fbc0ede72636349b27f01db49343d8daa87c5c`. The next performance gate
is reducing Burst TTI and DAX's CPU-bound typecheck time without weakening the
all-success requirement. A single-use warm-capacity candidate and a pinned
Node 24 development image now exist in source, but neither changes the current
published evidence until the same named-host conformance, cleanup, failure, and
repeated benchmark gates pass. After that, run the independent provider
harness.
Longer soak, disk-full, interrupted-upgrade, backup/restore, and rollback
exercises remain required. Existing evidence qualifies independent single-host
operation only; it does not establish multi-node scheduling, shared control,
replicated state, automatic failover, cross-host restore, or high availability.
