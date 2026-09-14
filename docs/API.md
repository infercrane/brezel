# API

Brezel exposes a substrate-neutral HTTP API. This page lists the implemented
surface only; planned resources belong in [Roadmap](ROADMAP.md).

## Conventions

- Base path: `/v1`
- Authentication: `Authorization: Bearer <opaque-token>`
- Project scope: `X-Project-ID`; the credential must already be bound to it
- Mutations: `Idempotency-Key` is required
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

`/healthz` reports process liveness. `/readyz` also checks private durable state
and the authenticated engine health path. Metrics contain fixed-dimension,
content-free process and lifecycle data without project, sandbox, command,
path, or preview-token labels.

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

## Commands and files

```text
POST /v1/sandboxes/{sandbox_id}/commands
PUT  /v1/sandboxes/{sandbox_id}/files?path={absolute_path}
GET  /v1/sandboxes/{sandbox_id}/files?path={absolute_path}
```

Command responses are NDJSON events with base64-encoded byte chunks followed by
one terminal exit event. Arguments, output, paths, and file contents are not
persisted in normal lifecycle events or receipts.

## HTTP previews

```text
POST /v1/sandboxes/{sandbox_id}/ports/{port}/leases
ANY  /p/{opaque_lease}/{application_path}
```

A preview lease is short-lived and checked against project, sandbox state,
generation, and port. The shared-origin proxy strips cookies, referrers, and
internal credentials. WebSockets are rejected.

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
