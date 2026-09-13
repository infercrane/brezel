# ADR 0002: Use E2B Runtime as the initial microVM substrate

- Status: proposed pending M0 qualification
- Date: 2026-09-13

## Context

A stateful open-source agent runtime needs image templates, Firecracker
lifecycle, checkpoint create/resume/fork, per-node orchestration, a guest
process/filesystem API, port proxying, volumes, and self-host deployment.

Implementing these from scratch would create a long critical path and a large
security-sensitive maintenance surface. Kubernetes Agent Sandbox, OpenSandbox,
Sandbox0, and embedded microVM libraries are useful alternatives, but E2B
Runtime currently matches the required data-plane shape most closely and is
published under Apache-2.0.

## Decision

Qualify a pinned E2B Runtime release as the initial substrate.

- Use supported APIs and deployment profiles first.
- Contribute generic lifecycle, security, and operability improvements upstream.
- Keep project-specific identity, policy, jobs, rollouts, connectors, audit, and
  product resources in this repository.
- Maintain a small, explicit patch series only when an upstream path is not yet
  available.
- Do not fork until an ADR documents the blocking gap, upstream outcome,
  compatibility cost, security ownership, and migration plan.

## Qualification gates

M0 must verify:

- repository and transitive component licensing;
- template provenance and immutable launch identity;
- create, exec, files, logs, terminal, ports, pause, resume, fork, volume, and
  destroy semantics;
- snapshot confidentiality, CPU compatibility, and failure recovery;
- client-proxy authentication and wake behavior;
- credential and network enforcement hooks;
- cleanup after API, node, Redis, and object-store faults;
- supported upgrade and rollback behavior; and
- performance on a named Linux/KVM host.

## Alternatives

### Kubernetes Agent Sandbox

Strong Kubernetes-native API and isolation-runtime selection. Keep as a future
adapter for organizations that prefer CRD-native lifecycle. Its lifecycle and
snapshot path does not currently replace E2B's complete Firecracker stack for
this MVP.

### OpenSandbox or Sandbox0

Useful references and possible future backends. They require the same conformance
mapping and currently provide less direct alignment with the desired E2B-style
template and resume path.

### Build directly on Firecracker

Rejected for MVP. It would require owning guest images, snapshot storage, VM
orchestration, networking, routing, API, node lifecycle, and compatibility before
the differentiated enterprise and inference work starts.

## Consequences

The first code milestone begins with integration and qualification, not a VMM.
The public API must avoid leaking upstream details that prevent a later backend,
while compatibility helpers may expose well-tested E2B semantics directly.
