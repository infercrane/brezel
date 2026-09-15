# API

Brezel exposes a substrate-neutral HTTP API. This page lists the implemented
surface only; planned resources belong in [Roadmap](ROADMAP.md).

## Conventions

- Base path: `/v1`
- Authentication: `Authorization: Bearer <opaque-token>`
- Project scope: `X-Project-ID`; the credential must already be bound to it
- Resource lifecycle mutations: `Idempotency-Key` is required; command
  execution, file writes, and preview leases are not replay-safe mutations
- Timestamps: UTC RFC 3339
- Long resource mutations: `202 Accepted` with an operation representation
- Unknown backend state: returned as `unknown`, never coerced to `running`
- Errors: stable code, message, retryability, request ID, and field violations

Tokens are accepted from protected client files by the CLI and stored only as
SHA-256 digests in the server access policy. Health and readiness do not require
tenant credentials.

## Service

```text
GET /healthz
GET /readyz
GET /metrics
GET /v1/capabilities
GET /v1/receipt-public-key
GET /v1/operations/{operation_id}
```

`/healthz` reports process liveness. `/readyz` also checks private durable state,
the authenticated engine and node data paths, and the separate node control
listener when routed execution is configured. Metrics contain fixed-dimension,
content-free process and lifecycle data without project, sandbox, command, path,
or preview-token labels.

## Environments

```text
POST /v1/environments
GET  /v1/environments/{environment_revision}
```

An environment resolves an installed template and policy into an immutable
revision. Arbitrary OCI image builds are not implemented.

## Sandboxes

```text
POST   /v1/sandboxes
GET    /v1/sandboxes
GET    /v1/sandboxes/{sandbox_id}
DELETE /v1/sandboxes/{sandbox_id}
POST   /v1/sandboxes/{sandbox_id}:pause
POST   /v1/sandboxes/{sandbox_id}:resume
GET    /v1/sandboxes/{sandbox_id}/events
GET    /v1/sandboxes/{sandbox_id}/receipt
```

Create accepts an immutable environment revision, optional workspace mounts,
network policy, and lifecycle policy. Admitted guest activity can postpone
automatic standby but cannot extend absolute expiration.

Network policy defaults to no egress. An explicit `allow_internet: true`
request enables direct unrestricted internet egress through the embedded
engine. That mode bypasses connector destination controls and weakens the
sandbox network boundary; use a connector when destination policy or
out-of-guest credential injection is required.

## Commands and files

```text
POST /v1/sandboxes/{sandbox_id}/commands
PUT  /v1/sandboxes/{sandbox_id}/files?path={absolute_path}
GET  /v1/sandboxes/{sandbox_id}/files?path={absolute_path}
```

Command responses are NDJSON events with base64-encoded byte chunks followed by
one terminal exit event. Arguments, output, paths, and file contents are not
persisted in normal lifecycle events or receipts.

Public requests terminate at `brezeld`. After authorization and lifecycle
admission, the packaged single-host profile forwards command and file bytes to
`brezel-node` over its separately authenticated data listener. The relay then
resolves the private engine identity. Clients do not connect directly to a node,
and lifecycle authority remains in the durable API process.

## HTTP previews

```text
POST /v1/sandboxes/{sandbox_id}/ports/{port}/leases
ANY  /p/{opaque_lease}/{application_path}
```

A preview lease is short-lived and checked against project, sandbox state,
generation, and port. The shared-origin proxy strips cookies, referrers, and
internal credentials. WebSockets are rejected.

## Internal node relay

The node relay protocol is an internal, versioned implementation boundary, not
a tenant API. It is intended to carry admitted command, file, and application-
port traffic directly to the assigned node while lifecycle, placement, quota,
and route binding remain authoritative in the durable service.

```text
GET  /healthz
GET  /readyz
GET  /v1/routes/{opaque_route_id}
POST /v1/commands
PUT  /v1/files?path={absolute_path}
GET  /v1/files?path={absolute_path}
ANY  /v1/ports/{port}/{application_path}
```

The transport uses mutual TLS 1.3. The API expects exactly
`spiffe://brezel/node/<node-id>` plus the node's DNS or IP SAN; the node expects
exactly `spiffe://brezel/api/<api-id>`. Certificates also require the matching
server or client extended key usage.

Every operation request carries:

```text
Authorization: BrezelCapability <signed-token>
X-Brezel-Route-ID: <opaque-route-id>
```

The Ed25519 token is valid for at most 30 seconds and one operation. It binds
issuer, key ID, audience, node ID, node boot identity, route ID and generation,
project ID, sandbox ID, operation, canonical request digest, random
single-use identifier, and operation-specific size, port, or duration bounds.
The relay authenticates the signed envelope and its static node, boot, route,
and operation claims before reading request content. It then reads only the
bounded request shape and computes its canonical digest. The relay acquires an
operation lease and private binding for the exact ready route generation,
matches the complete capability including the digest against that binding,
consumes the identifier in a bounded replay cache, and only then dispatches to
the engine. The lease prevents standby, release, or rebinding while the
operation is running. Saturated replay state fails closed.

Command events use the same bounded NDJSON representation as the public API.
File responses include path, size, and SHA-256 metadata headers. The port relay
permits bounded HTTP methods and removes credentials, cookies, forwarding,
hop-by-hop, and internal routing headers. Current hard ceilings are 128 KiB for
a command request, 64 MiB and one hour for command output and duration, 32 MiB
for file upload, 64 MiB for file download, 32 MiB and 64 MiB for port request
and response, and 16 KiB for a request URL.

The route inspection endpoint returns the relay node ID, a cryptographically
random process boot identity, and the public route binding. It never returns
the engine ID. The relay generates a new 128-bit boot identity in every server
process, so a capability minted for an earlier process cannot be replayed after
a restart. The relay deliberately does not log customer content, capability
tokens, or engine identities.

The packaged node process exposes route lifecycle on a separate bounded mTLS
control listener. These endpoints accept only the exact API peer identity and
apply an already-authorized controller decision to the local generation
ledger:

```text
GET  /readyz
PUT  /internal/v1/routes/{opaque_route_id}
POST /internal/v1/routes/{opaque_route_id}/attach-ready
POST /internal/v1/routes/{opaque_route_id}/drain
POST /internal/v1/routes/{opaque_route_id}/standby
POST /internal/v1/routes/{opaque_route_id}/rebind
POST /internal/v1/routes/{opaque_route_id}/release
POST /internal/v1/routes/{opaque_route_id}/remove
```

Requests bind the route, project, sandbox, generation, and expected state.
Private engine identities occur only in bind or rebind requests and the local
ledger; responses never return them. Bind and monotonic transitions converge
on safe retries. Rebind and remove deliberately reject ambiguous retries until
durable operation receipts are implemented.

This milestone does not move sandbox create, pause, resume, delete, placement,
or reconciliation authority to the relay. It also does not yet provide
certificate or signing-key rotation, durable node-operation receipts, a direct
client-to-node handoff, terminals, WebSockets, raw TCP, or direct client
authorization. `brezeld` remains the public HTTP endpoint and forwards admitted
command, file, and preview operations to the relay; it is not the guest byte
transport itself in the packaged profile.

## Workspaces and checkpoints

```text
POST   /v1/workspaces
GET    /v1/workspaces
GET    /v1/workspaces/{workspace_id}
DELETE /v1/workspaces/{workspace_id}
POST   /v1/sandboxes/{sandbox_id}/checkpoints
DELETE /v1/checkpoints/{checkpoint_id}
```

Workspaces are independent resources with single-writer attachment. Current
checkpoints capture filesystem state. Restoring a checkpoint creates a new
sandbox identity and preserves lineage; active credentials are never copied.

## Connectors

```text
POST /v1/connectors
GET  /v1/connectors/{connector_revision}
POST /connector/v1/leases/renew
ANY  /connector/v1/proxy/{connector_revision}/{allowed_path}
```

Connector gateway paths exist only when the preview broker is explicitly
configured. A connector contains endpoint and policy metadata plus an opaque
credential handle. Secret values never enter the API representation.

Schema files are under [`spec/v1alpha1`](../spec/v1alpha1). The namespace
`sandbox.runtime.dev/v1alpha1` remains provisional while the API is pre-release.
