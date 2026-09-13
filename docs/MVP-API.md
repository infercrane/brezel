# MVP API and semantics

This is the target product contract, not a promise that every endpoint is implemented.
The first SDK may translate compatible calls to E2B Runtime while project-owned
resources use the extension control plane.

The current executable implements the following subset. Consult
[`IMPLEMENTATION-STATUS.md`](IMPLEMENTATION-STATUS.md) before relying on a path:

```text
GET    /healthz
GET    /readyz
GET    /v1/capabilities
GET    /v1/receipt-public-key
POST   /v1/environments
GET    /v1/environments/{environment_revision}
POST   /v1/connectors
GET    /v1/connectors/{connector_revision}
POST   /v1/sandboxes
GET    /v1/sandboxes/{sandbox_id}
DELETE /v1/sandboxes/{sandbox_id}
POST   /v1/sandboxes/{sandbox_id}:pause
POST   /v1/sandboxes/{sandbox_id}:resume
POST   /v1/sandboxes/{sandbox_id}/checkpoints
GET    /v1/sandboxes/{sandbox_id}/events
GET    /v1/sandboxes/{sandbox_id}/receipt
GET    /v1/operations/{operation_id}
POST   /connector/v1/leases/renew
ANY    /connector/v1/proxy/{connector_revision}/{allowed_path}
```

These mutations execute synchronously in the first single-controller profile,
but sandbox mutations return `202` with the terminal Operation representation.
The resource and operation model remains compatible with moving backend work to
a durable asynchronous coordinator. Connector gateway paths are present only
when the preview broker is explicitly configured. Without it, connector
attachment fails before backend execution.

## API conventions

- Base path: `/v1`
- Authentication: bearer service token or OIDC-derived session
- Tenant scope: organization and project resolved server-side
- Mutations: `Idempotency-Key` required
- Long operations: `202 Accepted` with an `operation_id`
- Pagination: opaque cursor
- Timestamps: UTC RFC 3339
- Errors: stable machine code, retryability, message, operation ID, and field
  violations where applicable
- Unknown runtime state: returned as `unknown`, never coerced to `running`

## Environment and image operations

```text
POST   /v1/environments
GET    /v1/environments
GET    /v1/environments/{environment_id}

POST   /v1/images/builds
GET    /v1/images/builds/{build_id}
GET    /v1/images
GET    /v1/images/{image_id}
DELETE /v1/images/{image_id}
```

An image build resolves an OCI source and build steps into an immutable template
manifest. An environment binds that image to resource, startup, tool,
connector, storage, network, and lifecycle-policy revisions. Sandboxes reference
an immutable environment revision, not a mutable image tag.

## Sandbox operations

```text
POST   /v1/sandboxes
GET    /v1/sandboxes
GET    /v1/sandboxes/{sandbox_id}
DELETE /v1/sandboxes/{sandbox_id}

POST   /v1/sandboxes/{sandbox_id}:pause
POST   /v1/sandboxes/{sandbox_id}:resume
POST   /v1/sandboxes/{sandbox_id}:checkpoint
POST   /v1/sandboxes/{sandbox_id}:fork
POST   /v1/sandboxes/{sandbox_id}:archive       # after MVP
POST   /v1/sandboxes/{sandbox_id}:extend

GET    /v1/sandboxes/{sandbox_id}/events
GET    /v1/sandboxes/{sandbox_id}/receipt
```

Process, terminal, file, watcher, and port data paths should preserve the
upstream guest API and SDK streaming behavior. The control plane mints a short-
lived connection token after rechecking tenant, state, and policy.

`extend` changes expiration only when the caller has permission. Ordinary exec,
port, file, or model traffic can reset the idle timer but cannot extend the
absolute expiration.

`checkpoint` requires `kind: filesystem | full_state`. The response identifies
the exact compatibility profile and known unresolved external effects. The
operation fails before capture when the requested kind or quiescent boundary
cannot be enforced.

`fork` requires a checkpoint ID and creates a new sandbox identity. It does not
copy active credential leases. A dependent checkpoint cannot be deleted while a
child requires it unless the child is materialized independently.

## Checkpoint operations

```text
GET    /v1/checkpoints
GET    /v1/checkpoints/{checkpoint_id}
DELETE /v1/checkpoints/{checkpoint_id}
POST   /v1/checkpoints/{checkpoint_id}:materialize
```

Checkpoint identity is independent of the source sandbox. The resource exposes
kind, source and parent lineage, compatibility digest, retention, dependent
resources, and integrity metadata without exposing captured content.

## Storage operations

```text
POST   /v1/volumes
GET    /v1/volumes
GET    /v1/volumes/{volume_id}
DELETE /v1/volumes/{volume_id}

POST   /v1/drives                              # after MVP
GET    /v1/drives
GET    /v1/drives/{drive_id}
DELETE /v1/drives/{drive_id}
```

A volume is single-writer. A drive is multi-client. The API refuses attachment
patterns that its selected storage profile cannot enforce.

## Connector and model-route operations

```text
POST   /v1/connectors
GET    /v1/connectors
GET    /v1/connectors/{connector_id}
PATCH  /v1/connectors/{connector_id}
DELETE /v1/connectors/{connector_id}

POST   /v1/model-routes
GET    /v1/model-routes
GET    /v1/model-routes/{route_id}
PATCH  /v1/model-routes/{route_id}
DELETE /v1/model-routes/{route_id}
```

A connector stores endpoint, protocol, method/path, request-limit,
response-scrubbing, region, and policy metadata plus an opaque credential
handle. It never returns a secret value. A model route specializes a connector
with model, token, concurrency, and optional spend controls. Updates create a
revision; a sandbox's effective revision is visible in events and receipts.

## Job operations

```text
POST   /v1/jobs
GET    /v1/jobs
GET    /v1/jobs/{job_id}
POST   /v1/jobs/{job_id}:cancel
GET    /v1/jobs/{job_id}/attempts
GET    /v1/jobs/{job_id}/results
```

A job contains:

- immutable image/template or checkpoint reference;
- input artifact references;
- command or entry point;
- maximum concurrency and attempts;
- per-attempt and total timeout;
- retryable error classes;
- network, storage, and model-route policy; and
- declared result artifacts.

Job states are `queued`, `running`, `cancelling`, `succeeded`, `failed`, and
`cancelled`. Attempt state is separate. A job is not `cancelled` until every
attempt is terminal or an explicit cleanup failure is recorded.

## Rollout operations

The Rollout API follows after the Job path is qualified:

```text
POST   /v1/rollouts
GET    /v1/rollouts
GET    /v1/rollouts/{rollout_id}
POST   /v1/rollouts/{rollout_id}:cancel
GET    /v1/rollouts/{rollout_id}/episodes
GET    /v1/rollouts/{rollout_id}/evidence
```

A rollout binds an immutable environment, model connector revisions, task
artifacts, evaluator, branch strategy, concurrency, token/time/cost budgets,
stop conditions, and declared results. It does not automatically train, promote,
or deploy a model.

## Operation operations

```text
GET /v1/operations/{operation_id}
GET /v1/operations/{operation_id}/events
POST /v1/webhooks
```

Operation states are `pending`, `running`, `succeeded`, `failed`, and
`cancelled`. A successful operation records the authoritative resource revision.
Clients may retry the original mutation with the same idempotency key.

Events have ordered IDs and are replayable. Webhook deliveries are signed,
idempotent, retried with bounded backoff, and never contain customer content by
default.

## Sandbox status

```json
{
  "id": "sbx_01J...",
  "state": "standby",
  "revision": 12,
  "image": "img_01J...",
  "runtime_profile": "developer-single-host",
  "created_at": "2026-09-13T10:00:00Z",
  "last_active_at": "2026-09-13T10:12:09Z",
  "expires_at": "2026-09-14T10:00:00Z",
  "checkpoint": {
    "id": "chk_01J...",
    "kind": "full_state",
    "durability": "object-store",
    "compatibility_digest": "sha256:..."
  },
  "attachments": {
    "volume_ids": ["vol_01J..."],
    "drive_ids": [],
    "model_route_revisions": ["route_01J...:4"]
  }
}
```

## SDK design rule

The SDK presents `Sandbox`, `Workspace`, and `Job` first. Operator-only fields
remain accessible without being required in the happy path. Advanced lifecycle
calls are explicit; context-manager cleanup does not imply deleting a durable
workspace.

## Compatibility rule

Common E2B SDK behavior should remain source-compatible where feasible, but the
project will publish a tested compatibility table rather than make a blanket
provider-compatibility claim.
