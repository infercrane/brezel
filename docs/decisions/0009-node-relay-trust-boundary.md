# ADR 0009: Establish an authenticated node-relay trust boundary

- Status: accepted; single-host integration qualified at revision `121d7c6952c5bbc0010c365817ef540a1efbaca6`
- Date: 2026-09-14

## Context

Before this decision was integrated, the single-host server authorized tenant
operations and called the private engine through an in-process adapter. Command
output, file contents, and preview traffic therefore traversed the same durable
API process that owned lifecycle state. That coupled the high-volume byte path
to the durable authority, exposed engine integration concerns to a larger
process, and made multi-node routing harder to fence safely.

Moving bytes to a worker is not sufficient by itself. A relay must reject a
valid request addressed to the wrong node, a VM assignment that has since been
replaced, an operation different from the one admitted, a modified request, or
a replay of an already attempted operation. It must do so without placing
private engine IDs or guest credentials in public routing state.

ADR 0008 orders an authenticated node-local relay before fleet scheduling and
before replacing more of the pinned engine. This decision defines that relay's
trust boundary and records the integration that remains.

## Decision

Introduce a product-owned node relay for command execution, file transfer, and
application-port HTTP traffic.

The API-to-node transport uses TLS 1.3 mutual authentication. Node identities
are exact URI SANs of the form `spiffe://brezel/node/<node-id>` and API
identities are `spiffe://brezel/api/<api-id>`. Verification also enforces the
correct exclusive server or client extended key usage and normal CA and DNS or
IP SAN validation. Identity material is loaded only from private regular files;
symlinks and group- or world-readable inputs are rejected. Redirects and
ambient proxy configuration are disabled for relay calls.

The durable service remains responsible for tenant authentication, project
authorization, lifecycle state, expiry, policy, quota, and placement. After it
admits an operation, the API signs an Ed25519 capability with a lifetime of at
most 30 seconds for exactly one operation:

- `command.run`;
- `file.read`;
- `file.write`; or
- `port.proxy`.

The capability binds issuer and key ID, audience, exact node and relay boot
identity, opaque route ID, monotonic generation, project, sandbox, canonical
request digest, random identifier, validity window, and operation-specific
limits. It does not contain commands, paths, bodies, outputs, engine IDs, or
credentials. Those request values are covered by the digest.

The node persists an exclusively locked, checksummed generation ledger. A bind
or rebind receives a new globally monotonic generation. Released bindings do
not reset the durable generation clock. Only the node ledger maps the public
route tuple to the private engine ID, and formatted or JSON representations
redact that engine ID.

The relay authenticates the capability signature and static node, boot, route,
and operation claims before reading request content. After the canonical
request digest is available, it acquires an operation lease on the exact ready
route generation, matches every remaining claim, consumes the identifier in a
bounded replay cache, and resolves the private engine binding. A route may move
to draining while work finishes, but cannot enter standby, be released, or be
rebound while an operation lease remains. The cache never evicts an unexpired
identifier; saturation fails closed. The relay server generates a fresh random
128-bit boot identity inside every process, making prior-process capabilities
invalid.

The internal protocol provides health, readiness, redacted route inspection,
bounded NDJSON command streaming, bounded file read and write, and bounded HTTP
application-port proxying. Internal, authorization, cookie, forwarding, and
hop-by-hop headers are removed. The relay has no ordinary customer-content
logger.

## Current integration boundary

The packaged single-host profile now starts the relay as a separate process and
uses it by default for command, file, and preview traffic. `brezeld` owns the
desired lifecycle, binds and transitions opaque node routes over a separate
mTLS control listener, stores the route generation with the sandbox, and
reconciles that assignment after restart. The relay resolves its boot identity
on each admitted operation before a capability is minted, so a node restart
invalidates prior capabilities without invalidating the durable route.

Lifecycle engine calls intentionally continue through the durable service.
This removes customer byte traffic from that process without distributing
lifecycle authority. Dynamic node enrollment, online certificate and signing
key rotation, durable node-operation receipts, fleet placement, failover, and
cluster operation remain outside the completed integration. The default relay
path completed named-host Linux/KVM qualification at revision
`121d7c6952c5bbc0010c365817ef540a1efbaca6`; see
[Qualification 2026-09-15](../QUALIFICATION-2026-09-15.md). That result covers
independent single-host operation, not a fleet or availability profile.

## Limits and non-decisions

- Capabilities are bearer credentials. Theft before their first use permits the
  one request they authorize until expiry; mTLS protects them only in transit.
- A compromised API, node, host, CA, or signing key is not contained by this
  protocol.
- The replay cache is not distributed and depends on the server-owned fresh
  boot identity generated for every relay process.
- The ledger is a bounded single-process, single-node store, not a fleet source
  of desired lifecycle state.
- Command events stream, but file and HTTP port bodies are currently buffered.
- Terminals, WebSockets, raw TCP, UDP, multi-hop delegation, and public
  capability minting are not authorized by this decision.
- The relay does not make the single-host profile hostile-multitenant
  production-ready.

## Consequences

- The engine ID and guest-management credential can remain confined to the
  assigned node.
- A stale route or reassigned sandbox fails closed through node, boot, route,
  generation, project, sandbox, operation, digest, and replay checks.
- The command, file, and application-port byte path bypasses the durable API
  without moving lifecycle authority to the worker.
- Operators must provision and rotate two independent trust systems: mTLS
  identities and capability-signing keys.
- Node restart and route-rebind behavior becomes a release-critical security
  invariant and requires destructive and concurrency testing.
- The public API remains stable while the internal engine integration and
  node-relay deployment evolve.
