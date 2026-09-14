# Status

This document is the claim boundary for Brezel. Unit tests establish software
behavior; only the destructive qualification suite establishes behavior on a
named Firecracker host.

## Current label

**Private single-host release candidate.**

The supported evaluation profile is one organization on one dedicated Ubuntu
24.04 x86-64 machine with KVM and `/dev/net/tun`. The current revision has not
been freshly qualified on Linux/KVM after its latest lifecycle and engine
changes. It must not be described as hostile shared-multitenant production.

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
| State | Private, bounded, schema-versioned, exclusively locked, atomic and fsynced |
| Recovery | Durable idempotency, lifecycle events, cleanup intent, expiration reconciliation |
| Evidence | Ed25519-signed DSSE lifecycle receipt; no customer content by default |
| Distribution | Pinned source, patches, images, VM artifacts, installer, conformance, benchmark harness |
| Node relay foundation | Packaged node binary with separate mTLS control and data listeners, route lifecycle CAS, single-operation capabilities, replay defense, generation fencing, and in-flight operation leases; not default wiring |

Release code has no fake backend and no container isolation fallback. The test
backend exists only in `_test.go` files. A remote engine requires TLS; service
and engine credentials are read from protected files.

## Not implemented

- arbitrary OCI environment builds
- interactive PTY, SSH, desktop, or WebSocket transport
- full-state checkpoint and fork
- Python and TypeScript SDKs
- node enrollment, rotation, durable operation receipts, reconciliation, service-unit packaging, and default direct data paths
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

A previous revision completed the suite on a named four-core Xeon host. That is
historical evidence, not qualification of the current commit. See
[Qualification 2026-09-13](QUALIFICATION-2026-09-13.md).

## Performance evidence

The historical host produced these environment-specific observations:

| Scenario | Initial p50 | Final hardening sample p50 |
| --- | ---: | ---: |
| Cached create through verified instruction | 371 ms | 599 ms |
| Same-node resume through verified instruction | 228 ms | 507 ms |

Sustained destructive load exposed meaningful host variance, and the original
250 ms create and 100 ms resume budgets were not met. No portable latency claim
is authorized. The current revision needs a fresh sequential, staggered, burst,
and post-reboot matrix before new numbers are published. See
[Benchmarking](BENCHMARKING.md).

## Next release gate

The relay trust foundation and separate node process are implemented and pass
local protocol, negative, concurrency, restart, and race tests. The next gate
is node enrollment and identity rotation, durable operation receipts, desired
route reconciliation, service-unit packaging, and the default command, file,
and preview handoff. That integrated path must pass the complete Linux/KVM
qualification, failure, and benchmark matrix before this status changes.
