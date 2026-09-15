# Status

This document is the claim boundary for Brezel. Unit tests establish software
behavior; only the destructive qualification suite establishes behavior on a
named Firecracker host.

## Current label

**Self-hosted private-tenant developer preview.**

The supported evaluation profile is one organization on one dedicated Ubuntu
24.04 x86-64 machine with KVM and `/dev/net/tun`. Revision
`121d7c6952c5bbc0010c365817ef540a1efbaca6` completed destructive qualification
on two separately administered machines, each operating as an independent
single-host deployment. It must not be described as a cluster, highly available,
hostile shared-multitenant, or public-production system.

## Implemented

| Capability | Current boundary |
| --- | --- |
| Environments | Immutable template revisions; arbitrary OCI builds are not available |
| Sandboxes | Project-scoped create, inspect, list, pause, resume, expiration, and delete |
| Commands | Streamed output, bounded response, deadline, real exit status |
| Files | Authenticated guest path, absolute-path validation, bounded upload/download |
| HTTP previews | Opaque 30–900 second lease, state recheck, credential stripping; no WebSockets |
| Workspaces | Host-backed, durable across sandbox replacement, single writer |
| Checkpoints | Filesystem capture and restore with lineage and deletion protection |
| Automatic standby | Activity and grace deadlines, operation fencing, restart recovery |
| Network and connectors | Deny-by-default policy and a narrow private model/tool connector preview |
| Authentication | Protected token digests bound to explicit projects |
| Capacity | Per-project resource limits plus a process-wide admission ceiling |
| State | Private SQLite WAL ledger, exclusive controller lock, semantic validation, legacy JSON import, and row-scoped guest hot paths |
| Recovery | Durable idempotency, lifecycle events, cleanup intent, expiration reconciliation |
| Evidence | Ed25519-signed DSSE lifecycle receipt; no customer content by default |
| Distribution | Pinned source, patches, images, VM artifacts, installer, conformance, benchmark harness |
| Node relay | The single-host package defaults command, file, and preview traffic to separate mTLS control and data listeners with route lifecycle CAS, one-operation capabilities, replay defense, generation fencing, reconciliation, and in-flight leases; the integrated path completed named-host Linux/KVM qualification at revision `121d7c6952c5bbc0010c365817ef540a1efbaca6` |

Release code has no fake backend and no container isolation fallback. The test
backend exists only in `_test.go` files. A remote engine requires TLS; service
and engine credentials are read from protected files.

## Not implemented

- arbitrary OCI environment builds
- interactive PTY, SSH, desktop, or WebSocket transport
- full-state checkpoint and fork
- Python and TypeScript SDKs
- node enrollment, online certificate and signing-key rotation, durable node-operation receipts, and service-unit upgrade or rollback packaging
- warm-capacity management and node-local snapshot prefetch
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

Revision `121d7c6952c5bbc0010c365817ef540a1efbaca6` completed the destructive
workflow on each of two separately administered Ubuntu 24.04 x86-64 KVM hosts.
The paired run covered simultaneous conformance, cached-template burst
observations for time to first verified instruction, filesystem restore, and
workspace I/O, cross-host namespace-negative checks, confirmed cleanup, and
controller/node-relay crash containment. See [Qualification
2026-09-15](QUALIFICATION-2026-09-15.md). The earlier four-core Xeon result
remains historical evidence in [Qualification
2026-09-13](QUALIFICATION-2026-09-13.md).

## Performance evidence

The historical four-core host produced these environment-specific observations:

| Scenario | Initial p50 | Final hardening sample p50 |
| --- | ---: | ---: |
| Cached create through verified instruction | 371 ms | 599 ms |
| Same-node resume through verified instruction | 228 ms | 507 ms |

Sustained destructive load exposed meaningful host variance, and the original
250 ms create and 100 ms resume budgets were not met. No portable latency claim
is authorized. The paired two-host qualification added three simultaneous burst
checks. Both complete 24-cell matrices subsequently finished: Host B passed all
24 cells and all 1,056 attempts; Host A passed 22 cells and 1,040 of 1,056
attempts, failing only the two concurrent filesystem-restore cells. Both hosts
confirmed all 1,584 expected resource cleanups. The exact host-local figures and
limitations are recorded in [Qualification
2026-09-15](QUALIFICATION-2026-09-15.md). They do not authorize a portable or
competitive performance claim.

## Next release gate

The separate node process and default single-host byte path passed repository
tests and the named-host destructive workflow. Row-scoped SQLite lifecycle
operations and bounded snapshot-diff caching have passed repository tests but
remain unqualified on Linux/KVM. The next evidence gate is a complete two-host
rerun at the corrected revision, followed by longer soak, disk-full,
interrupted-upgrade, backup/restore, and rollback exercises. Existing evidence
qualifies independent single-host operation only; it does not establish
multi-node scheduling, shared control, replicated state, automatic failover,
cross-host restore, or high availability.
