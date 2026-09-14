# ADR 0010: Use an embedded SQLite lifecycle ledger for the single-host profile

- Status: accepted; implemented, host qualification pending
- Date: 2026-09-15

## Context

The original single-host store kept the complete authoritative state in one
bounded JSON document. Atomic replacement, file and directory synchronization,
semantic validation, and an exclusive process lock made that design safe for a
small preview, but every mutation eventually scaled with all resources. Guest
activity and content-free event recording therefore paid an unrelated
whole-state copy and rewrite cost.

The single-host profile still needs one embedded authority with simple backup
semantics. Introducing a network database would expand the operational and
failure surface without providing useful multi-writer availability.

## Decision

Use SQLite through the pure-Go `modernc.org/sqlite` driver as the default
single-host lifecycle ledger. Configure WAL journaling, full synchronous
durability, foreign-key enforcement, a bounded busy timeout, and one database
connection. Retain an external nonblocking file lock so a second Brezel
controller cannot open the same authority.

Store each environment, sandbox, operation, checkpoint, workspace, and
connector as an independently keyed row. Store sandbox events by project,
resource, and monotonically increasing sequence. Store idempotency decisions in
a dedicated keyed table. Validate the complete semantic state at startup and
after generic state mutations. Use resource-scoped transactions for sandbox
lookup, activity, and event hot paths.

When a database has no schema version and a protected legacy JSON file exists,
import it in one transaction. Never modify or delete the source. Once the
database records its schema version, never import that source again. Reject
unknown database schema versions, malformed resources, unsafe permissions,
symlinks, failed integrity checks, or concurrent controller ownership.

## Consequences

- Guest hot-path writes no longer grow with the number of unrelated resources.
- WAL and shared-memory files become protected state and must be included in
  permission, backup, disk-full, and recovery procedures.
- Low-frequency service mutations that use the generic state callback still
  decode the complete logical state. This is acceptable only for the stated
  private single-host boundary and remains a scaling limit.
- SQLite is not the multi-node source of truth. A future fleet store must
  preserve project scoping, idempotency, confirmed cleanup, route generations,
  event ordering, and non-destructive migration semantics.
- Local microbenchmarks do not establish sandbox startup or production
  performance. Linux/KVM qualification and failure tests remain mandatory.
