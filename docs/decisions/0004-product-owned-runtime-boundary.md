# ADR 0004: Keep the microVM substrate behind a product-owned runtime contract

- Status: accepted
- Date: 2026-09-13

## Context

The initial executable required public configuration named `E2B_API_URL`,
`E2B_API_KEY`, and `BREZEL_BACKEND=e2b`. That made a replaceable open-source
engine look like the product boundary and allowed a managed third-party endpoint
to become the default. It also asked application developers to select an
implementation detail.

## Decision

The public product contract exposes environments, sandboxes, commands, files,
authenticated HTTP previews, checkpoints, connectors, events, and receipts.
Environment creation does not accept a backend name. The runtime selects its
bundled `microvm` engine.

The default distribution starts the pinned engine on the same Linux/KVM host.
The engine endpoint is loopback-only and its generated credential is copied into
a protected file consumed by the runtime service. No managed sandbox account is
required. Remote engine endpoints are operator-only, must use TLS, and are never
part of the application SDK.

The upstream package and protocol remain attributed and pinned in the source
tree. Concealing a supply-chain dependency is not product ownership.

The runtime service reaches commands, file transfer, and authenticated preview
traffic through an internal product-owned node data-plane interface. The
single-host implementation adapts the bundled engine in process. Each handoff
is content-free, binds the authorized project sandbox, backend identity, and
durable sandbox revision, expires within five minutes or at sandbox expiry, and
is revoked when the admitted operation releases. The adapter rechecks the
current durable revision before it calls the engine. Guest-management and
substrate routing credentials remain private to the engine client.

This handoff is not a network bearer credential or a distributed node identity.
A future out-of-process node protocol must add mutually authenticated transport
and a signed, audience-bound, single-operation lease before it can preserve the
same authorization claim across a process boundary.

## Consequences

- customers install and call one runtime product;
- engine credentials never appear in browser, SDK, API response, durable state,
  event log, or receipt;
- the engine can be replaced after conformance without changing applications;
- the project still avoids unsafe reimplementation of KVM, Firecracker
  orchestration, snapshot restore, and the privileged guest agent; and
- distribution qualification must test both the product path and exact pinned
  engine on Linux/KVM.
