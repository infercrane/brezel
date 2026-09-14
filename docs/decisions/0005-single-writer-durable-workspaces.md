# ADR 0005: Expose single-writer durable workspaces with confirmed cleanup

- Status: accepted
- Date: 2026-09-13

## Context

Sandbox root filesystems survive standby but remain coupled to one sandbox.
Long-running agents also need project data that survives sandbox deletion and
replacement. The pinned engine has a volume path, but its Embed profile disables
it, passes signing material through environment values, and deletes metadata
before asynchronous physical cleanup.

Those defaults cannot support a product claim that workspace state is
independent, credentials stay out of ordinary environment values, or deletion
failure remains visible and retryable.

## Decision

Add a substrate-neutral `Workspace` resource to the product API and enable it
only when the exact deployment explicitly advertises the capability.

The first profile is single-writer:

- a workspace is scoped to one project and has an independent lifecycle;
- only a `ready` workspace can be attached;
- at most one non-terminal sandbox can reference it;
- deletion is rejected while attached;
- create and delete intent is persisted before engine mutation; and
- unconfirmed backend outcomes become `unknown` and are reconciled.

The single-host installer applies one digest-pinned engine patch. It adds a
private-file input for the volume-token signing key and makes physical cleanup
synchronous before engine metadata deletion. The product never returns engine
volume IDs, names, content tokens, or signing material.

## Consequences

Agent state can now survive sandbox replacement without making the root
filesystem immortal. Failed cleanup remains observable and retryable. Stock or
misconfigured engine deployments fail closed because durable workspaces are not
advertised by default.

The profile is still local-host storage. It does not promise shared writers,
replication, backup, host-loss recovery, secure media erase, or cluster fencing.
Those require separate storage profiles and conformance suites.
