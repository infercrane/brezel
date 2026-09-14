# Implementation status

This file distinguishes implemented runtime behavior from designed or
qualified product behavior. A green unit test is not a deployment qualification.

## Implemented in this repository

| Capability | State | Evidence |
| --- | --- | --- |
| Immutable Environment registration | Implemented | Domain validation, content-derived revision, file-store and HTTP tests |
| Project-scoped Sandbox lifecycle | Implemented | Create, inspect, pause, resume, delete, expiration reconciliation |
| Explicit standby state kinds | Implemented | Filesystem and full-state remain distinct through validation and adapter calls |
| Activity-driven automatic standby | Implemented for one controller | Durable activity and grace deadlines, active-operation fencing, persisted pause intent, restart recovery, and product-owned timer; fresh Linux/KVM qualification is pending |
| Crash recovery lookup | Implemented | Local ID and project are written to backend metadata and used for reconciliation |
| Independent filesystem checkpoint | Implemented | E2B snapshot adapter, project-scoped create/delete API, dependent-sandbox protection, and confirmed physical build cleanup |
| Filesystem checkpoint restore/fork | Implemented | New sandbox creation accepts one project-scoped checkpoint, preserves lineage, and boots its immutable snapshot reference |
| Independent durable workspace | Implemented for one host | Project-scoped create/list/inspect/delete, single-writer attachment, sandbox-replacement persistence, and cleanup reconciliation |
| Independent full-state checkpoint | Unavailable | Rejected before backend execution; upstream snapshot route lacks the required explicit guarantee |
| Idempotent mutations | Implemented | Durable idempotency index; duplicate create test proves one backend call |
| Idempotency payload binding | Implemented for create and checkpoint operations | Reusing a key with different canonical inputs fails before backend mutation; no-payload lifecycle keys are resource-scoped and legacy entries without a digest remain readable |
| Ordered lifecycle event log | Implemented for one controller | Durable append log; cluster ordering is not claimed |
| Signed lifecycle receipt | Implemented | Ed25519 DSSE envelope with an in-toto statement and tamper test |
| Tenant isolation at API/store boundary | Implemented | Protected token-digest policy binds principals to explicit projects; project-scoped keys and cross-project denial tests |
| Project capacity limits | Implemented for current primitives | Positive startup-configured limits for active sandboxes, workspaces, environments, connectors, and concurrent guest operations |
| Dependency readiness | Implemented | `/readyz` checks private atomic state and the authenticated engine `/health` route; `/healthz` remains process liveness |
| Single-controller durability | Implemented | Exclusive process lock, private state file, schema version and project/resource identity invariants, 128 MiB bound, atomic replacement, file fsync, and directory fsync |
| Admission and local telemetry | Implemented | Positive process-wide in-flight ceiling, health/readiness bypass, generated request IDs, route-pattern-only access logs, unlabeled process counters, and content-free lifecycle/guest phase histograms with fixed operation, phase, and outcome labels |
| Connector definition | Implemented | HTTPS, method/path, and opaque secret-handle validation |
| Connector credential enforcement | Developer preview | Five-minute signed lease, current-state checks, route policy, external file resolver, header stripping, DNS/metadata guard, response scrub |
| Streamed process execution | Implemented | Real Connect protocol adapter, bounded output, deadline, exit-event requirement, no argument or output persistence |
| File upload/download | Implemented | Authenticated guest path, clean absolute paths, 32 MiB upload and 64 MiB download limits, content-free events |
| Authenticated HTTP preview | Implemented for HTTP | Opaque 30–900 second lease, tenant/state recheck, internal credential stripping, no cookies, WebSocket explicitly rejected |
| Product-owned node data-plane boundary | Implemented in process | Commands, files, and previews require a revision-bound, expiring, revocable authorization handoff; engine identifiers and guest credentials remain internal |
| Release engine | Bundled microVM adapter only | Protected token file, TLS for remote engines, redirects disabled, secured guest-management token required, and a process-local one-second guest liveness lease that is empty after controller/host restart so persisted stale `running` state fails closed |
| Test backend | Tests only | Defined exclusively in `_test.go`; cannot be selected by release code |
| Deployment conformance | Implemented for the current runtime subset | Real command/file/HTTP paths, pause/resume and workspace persistence, exclusivity, lifecycle, idempotency, cross-project denial, checkpoint, cleanup, receipt verification, controller restart, and repeated qualification |
| Engine resume fast paths | Source contract implemented; live requalification pending | Revision-bound checks cover Firecracker snapshot restore, UFFD lazy paging, build-time prefetch, NBD COW rootfs, local template caching, and pooled network slots. Qualification now fails closed unless the installed host exposes the active COW/UFFD path, a usable prefetch map, cached snapshot artifacts, and ready network namespaces |
| CLI | Implemented | Host doctor, sandbox and workspace create/list/lifecycle, checkpoint create/delete/restore, exec, file put/get, and port lease commands; bearer token accepted only from a private file |
| Single-host distribution | Implemented; post-change qualification pending | Digest-locked source, images, API build inputs, Firecracker, kernel, guest, and orchestrator artifacts; mirror and preloaded modes; protected distribution manifest. A prior revision ran on one named host, but the current revision still requires fresh Linux/KVM qualification; see `QUALIFICATION-2026-09-13.md` |

## Not implemented yet

- environment/image builds from arbitrary OCI sources;
- PTY, terminal, watcher, structured log, and WebSocket preview data paths;
- concurrent shared drives and replicated workspace storage;
- proof-of-possession lease renewal, managed secret providers, and streaming response scrubbing;
- full-state fork, sandbox groups, jobs, rollouts, and webhooks;
- OIDC, organizations, fine-grained RBAC, dynamic quota administration, and approvals;
- PostgreSQL/Redis/object-store clustered profile;
- an out-of-process node relay with mTLS, audience-bound operation leases, a
  node generation ledger, and direct terminal/file/preview byte paths;
- an external host-isolation/escape review, multi-host recovery, and production soak suite;
- SDKs; and
- any portable performance promise, hostile shared-multitenant, air-gap, GPU,
  or hardware-attestation claim. Named-host performance observations are
  recorded separately and are not service-level guarantees.

## Current deployment profile

The runnable distribution is a **hardened single-host agent runtime release
candidate** over a bundled, pinned Firecracker engine. Its JSON state is private,
bounded, exclusively locked, atomically replaced, and directory-synced across
restart, but it is intentionally not a clustered database. Run it only on a
dedicated host in a controlled private evaluation after that named host passes
the destructive qualification workflow.

The current bearer-renewal connector is limited to the trusted-operator profile;
see ADR 0003. A prior revision passed the destructive contract on one named
Linux/amd64 KVM host. Automatic standby, the node boundary, and the owned
artifact manifest were added afterward, so the current revision is not host
qualified yet. The next release gates are a fresh qualification and benchmark
matrix, reliable host-loss recovery, arbitrary environment builds,
proof-of-possession connector identity, and an external isolation review.
