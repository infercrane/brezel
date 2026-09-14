# ADR 0008: Build a perpetual execution substrate around the microVM engine

- Status: accepted
- Date: 2026-09-14

## Context

The product currently uses a pinned E2B Runtime revision to provide the
Firecracker microVM, snapshot, copy-on-write disk, guest agent, and network
mechanics. This was an appropriate bootstrap boundary, but an unchanged engine
integration cannot be the long-term product identity or the basis for claiming
better lifecycle, latency, portability, or operations than the upstream
runtime.

Agent workloads are bursty and stateful. They alternate between model work,
tool execution, human review, and idle periods. Requiring callers to predict
those gaps or manually pause a sandbox exposes an infrastructure mechanism
instead of providing the desired product behavior. A useful execution
substrate must preserve the working session, stop charging for inactive CPU,
resume transparently, and bind every transition to the same tenant, sandbox
generation, environment, policy, and storage identity.

## Decision

Adopt a perpetual-sandbox lifecycle as the product contract while keeping the
pinned microVM engine as the first implementation.

The product owns:

- the public API, resource identities, lifecycle state machine, idempotency,
  quotas, policy, receipts, and failure semantics;
- activity accounting and the decision to enter standby;
- generation-fenced placement and routing readiness;
- checkpoint lineage and the exact state promised by each standby profile;
- durable workspace, volume, and future shared-drive semantics;
- the authenticated guest, preview, credential, and model-routing contracts;
- the benchmark boundary and the evidence required for performance claims;
  and
- the source, image, kernel, guest, and runtime artifact manifest shipped by
  the distribution.

The bundled engine remains responsible initially for:

- Firecracker process and jailer operation;
- KVM, cgroup, namespace, tap, and block-device mechanics;
- snapshot load and `userfaultfd` page serving;
- copy-on-write root filesystems; and
- the privileged guest-management protocol.

Engine calls are confined behind internal interfaces. No upstream resource
name, credential, endpoint, error shape, or lifecycle assumption is part of the
public contract. The engine revision stays immutable and independently
qualified. Generic fixes should be proposed upstream where practical; product
semantics remain outside the engine fork.

## Ordered implementation

1. Qualify the exact pinned engine and record phase-level latency on Linux/KVM.
2. Implement deterministic activity accounting, automatic standby eligibility,
   and transparent, generation-safe resume.
3. Move guest command, file, terminal, and preview traffic to an authenticated
   node-local relay so the durable API is not the byte path.
4. Own the artifact build and mirror, including digests, provenance, SBOMs, and
   an explicit offline source.
5. Add a node-local immutable content-addressed cache, pooled network resources,
   and a measured snapshot prefetch policy.
6. Add portable encrypted snapshot manifests with CPU, kernel, device, guest,
   policy, disk, entropy, and external-effect compatibility checks.
7. Introduce a fleet authority and scheduler only after the single-node data
   path and recovery contract pass their release gates.
8. Replace or fork engine components only when profiling, security, air-gap, or
   lifecycle requirements identify a bounded reason to own them.

Rust is preferred for new node-local components whose profiles show that
memory footprint, packet throughput, page serving, block processing, or guest
startup is material. Go remains appropriate for the public API, durable state
machine, policy, scheduling, reconciliation, and operator tooling. Firecracker
remains the hostile-code VMM.

## Non-decisions

- This does not claim sub-25 millisecond resume, public hostile-multitenant
  production readiness, cross-host restore, or fleet scale.
- This does not authorize a container fallback for untrusted code.
- This does not make network traffic alone authoritative for expiration,
  billing, or tenant identity.
- This does not require a wholesale rewrite of the Go service or Firecracker.
- This does not make Kubernetes, VPP, a shared filesystem, or a warm pool a
  release dependency before the measured single-host path justifies them.

## Consequences

- Application developers receive a stable session abstraction rather than
  engine-specific pause and resume mechanics.
- E2B Runtime is a replaceable, attributed implementation dependency rather
  than the product boundary.
- Automatic standby must never race active guest work, extend hard expiration,
  reuse stale credentials, or publish a route before the matching generation
  is ready.
- Performance work is prioritized by phase telemetry and matched benchmarks.
- The project accepts responsibility for maintaining any private engine patch
  and for proving upgrades against the same conformance suite.
