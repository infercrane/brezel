# ADR 0007: Validate persisted identity and bound process admission

- Status: accepted
- Date: 2026-09-13

## Context

The private single-host profile stores authoritative metadata in one local JSON
file. Atomic replacement and an exclusive process lock prevent torn writes and
concurrent controllers, but they do not detect a future schema or an object
stored under a key for another project. The API also had per-project resource
limits without a ceiling on simultaneous HTTP work. A traffic spike could
therefore exhaust the controller before an operator could read readiness.

Raw request paths cannot be used for operational logging because authenticated
preview URLs contain opaque bearer capabilities.

## Decision

The file state carries an explicit schema version. Opening a legacy unversioned
file performs the only implicit migration currently supported; a future schema
fails closed. Every open and update validates that environment, sandbox,
workspace, checkpoint, connector, operation, and idempotency indexes agree with
their embedded project and resource identities. Invalid state is never made
current.

The HTTP process enforces a positive in-flight request ceiling. Liveness,
readiness, and metrics bypass that ceiling so an overloaded instance remains
observable. Rejected requests receive `503`, a stable `overloaded` code, and a
short `Retry-After` value.

The runtime generates request IDs and optionally logs only the method, matched
route pattern, response status, and duration. It exports process counters
without project, sandbox, command, file, or preview-token labels.

## Consequences

- A corrupted or cross-project local index blocks startup or commit instead of
  becoming authoritative.
- Schema changes now require an explicit migration decision and tests.
- The single process has deterministic overload behavior and retains a usable
  health surface during saturation.
- Built-in metrics are deliberately low-cardinality and content-free. Richer
  tenant telemetry belongs in a separately authorized observability pipeline.
- This does not make the file store multi-writer, replicated, or recoverable
  after host loss. The clustered profile still requires transactional database
  migrations, distributed leases, and failure-domain qualification.
