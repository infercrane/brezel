# Threat model

## Security objective

Allow model-generated or otherwise untrusted code to use an isolated computer,
explicit storage, and approved network services without compromising the host,
another tenant, platform credentials, or control-plane integrity.

## Deployment profiles

| Profile | Intended use | Isolation claim |
| --- | --- | --- |
| `developer-single-host` | One trusted operator, local or CI agents | MicroVM boundary; no availability or hostile-operator claim |
| `private-single-tenant` | One enterprise tenant across worker nodes | MicroVM plus tenant network and identity controls |
| `shared-multitenant` | Mutually untrusted projects | Available only after dedicated conformance and external review |
| `dedicated-accelerator` | One tenant with GPU or device access | Separate profile; no inherited shared-tenant claim |

The repository currently provides a hardened single-host release candidate.
Documentation and APIs may describe later profiles but must not label them
qualified prematurely. Even the single-host profile is qualified only after
the named Linux/KVM deployment passes the destructive workflow.

## Adversary capabilities

Assume sandbox code, images, model output, tools, and inputs may be malicious.
The adversary may:

- execute arbitrary user-space code and spawn processes;
- exploit interpreters, compilers, package managers, browsers, or native tools;
- attempt hypervisor, guest-agent, kernel, device, or container escape;
- read host or cross-tenant memory, disks, snapshots, logs, routes, or metadata;
- scan networks, reach cloud metadata, rebind DNS, follow redirects, or tunnel
  through an allowed endpoint;
- steal, replay, or exfiltrate model, cloud, source-control, and tool credentials;
- forge guest logs, outputs, activity heartbeats, or completion messages;
- exhaust CPU, memory, PIDs, disk, IOPS, bandwidth, connections, model quota, or
  snapshot storage;
- keep resources alive through traffic or race pause, resume, fork, and delete;
- poison a template, snapshot, volume, shared drive, or cache; and
- exploit stale or vulnerable pinned runtime dependencies.

## Trusted computing base

The trusted computing base includes:

- physical host, firmware, Linux/KVM, Firecracker, and E2B runtime components;
- host networking, storage drivers, and node orchestrator;
- project API, scheduler, lifecycle reconcilers, proxy, connector broker, and
  policy engine;
- PostgreSQL, Redis routing data, and object storage control paths;
- identity, secret, KMS, and signing providers; and
- administrators able to configure or access the platform.

The node relay, its mTLS private keys, API capability-signing keys, replay
cache, and generation ledger are also trusted. A compromise of either endpoint
or its signing material is outside the protection supplied by the relay
protocol itself.

The guest image, guest `root`, commands, agent, application, model output, tool
output, and customer data are untrusted.

## Protected assets

- host and other tenants' isolation;
- source, documents, artifacts, volumes, drives, snapshots, logs, and terminals;
- model, cloud, source-control, and external-service credentials;
- private networks and cloud metadata services;
- control-plane state and lifecycle integrity;
- policy, image, and receipt signing keys;
- capacity, availability, and paid model quota; and
- tenant deletion and retention guarantees.

## Core invariants

1. User code runs only inside the sandbox's qualified isolation profile.
2. A request is usable only after identity, policy, network, storage, and guest
   health checks succeed.
3. Missing or unknown enforcement fails closed.
4. The database is the source of desired state; Redis and node memory are not.
5. Standby, expiration, archive, deletion, and cleanup failure remain distinct.
6. Incoming traffic cannot silently extend expiration or bypass resume policy.
7. Long-lived credentials never enter guest files, environment, process args,
   images, snapshots, logs, or receipts.
8. Model access does not imply general network access.
9. Tenant identity scopes every object, route, secret handle, snapshot, volume,
   drive, log, operation, and cache lookup.
   The implemented API additionally binds every accepted bearer-token digest to
   an explicit server-side project allowlist; changing `X-Project-ID` cannot
   manufacture authority.
10. A guest cannot forge control-plane terminal state or receipt fields.
11. Cleanup failure remains visible and retryable.
12. Customer content collection is opt-in, tenant-scoped, and separate from
    operational telemetry.
13. An API-to-node request requires both a mutually authenticated transport and
    a valid capability for one exact operation and current route generation.
14. A private engine ID is resolved only inside its assigned node and never
    appears in a capability, public response, ordinary log, or receipt.
15. A relay restart changes its boot identity before accepting traffic; a
    process-local replay cache is safe only while that invariant holds.

## MicroVM boundary

Firecracker supplies a hardware-virtualized boundary with a deliberately small
device model, but it does not eliminate hypervisor, KVM, host-kernel, firmware,
or side-channel risk. The exact kernel, Firecracker, E2B, guest, and host versions
must be pinned and covered by a patch-lag policy.

Root inside a guest is still untrusted. Guest management APIs require per-
sandbox identity and are never exposed by public preview routes.

Plain containers may be useful for trusted development tasks but cannot carry
the project's microVM or hostile multi-tenant assurance label.

## Node relay boundary

The node relay narrows authority after the durable service admits an operation.
It does not accept the public bearer token or trust project, sandbox, node, or
engine identity supplied in an ordinary request.

The transport requires TLS 1.3, a trusted certificate chain, normal DNS or IP
SAN validation on the node, and exact URI SAN identities of the form
`spiffe://brezel/api/<api-id>` and `spiffe://brezel/node/<node-id>`. API and
node certificates have exclusive client and server extended key usages. Local
certificate, private-key, and CA paths must be private regular files; symlinks
and group- or world-readable files fail closed. Redirects are not followed and
the configured HTTP transport does not inherit an ambient proxy.

Transport identity is necessary but insufficient. Every data operation also
uses a short-lived Ed25519-signed bearer capability that authorizes only one of
`command.run`, `file.read`, `file.write`, or `port.proxy`. It binds the exact
node and relay boot, opaque route and generation, project and sandbox, a digest
of the canonical request descriptor, operation-specific limits, a random
identifier, and a maximum 30-second authorization lifetime. Command arguments,
paths, body contents, output, engine IDs, and credentials are not embedded in
the token. The request digest binds those values without disclosing them.

The relay resolves the route through an exclusively locked, crash-safe node
ledger only after capability verification. Route generations increase on bind
and rebind and never reset when a released route ID is reused. An old token
therefore cannot address a new VM assignment. The private engine ID is held in
the ledger binding and redacted from JSON and formatted output. The relay
rechecks the current route and lifecycle state before execution.

A bounded replay cache consumes each capability identifier once until expiry.
It removes expired entries but never evicts an unexpired identifier to make
space; saturation denies new operations. The cache is deliberately
process-local. The relay server generates a fresh random 128-bit boot identity
inside every process, and capability verification rejects every prior process
identity before replay admission.

After authenticating the token but before reading a request body, the relay
checks its static node, boot, route, and operation claims. Once the canonical
request digest matches, it acquires a process-local lease on the exact ready
route generation. The route may drain while work finishes, but it cannot enter
standby, be released, or be rebound until all admitted leases are released.

The implemented protocol caps command requests at 128 KiB, command output at
64 MiB, command duration at one hour, file uploads at 32 MiB, file downloads at
64 MiB, proxied HTTP requests and responses at 32 MiB and 64 MiB respectively,
and request URLs at 16 KiB. It strips internal, authorization, cookie,
forwarding, and hop-by-hop headers. It has no ordinary content logger. Commands
are streamed as bounded NDJSON; file and HTTP port bodies are currently
buffered, which limits scale and increases node memory pressure within those
bounds.

The relay does not yet authorize lifecycle or placement, expose a public
capability-minting endpoint, rotate certificates or signing keys, reconcile its
ledger with a fleet authority, support terminal or WebSocket tunnels, or
provide a separately authenticated data-edge path. The default server still
uses the in-process engine data-plane adapter, so current public command, file,
and preview traffic continues through the durable API. Relay deployment
remains an internal, non-default release milestone until those integration and
Linux/KVM failure tests pass.

## Checkpoint, restore, and fork boundary

Full-state checkpoints can contain process memory, tokens, files, browser
sessions, and other secret material. Filesystem checkpoints contain declared
writable disk state but may still contain credentials or customer content. Both
are customer-confidential data, encrypted at rest, scoped to the tenant, and
deleted through durable reconciliation.

Resume and fork validate:

- tenant and project ownership;
- immutable snapshot manifest;
- source sandbox and policy identity;
- image, guest, Firecracker, CPU, and device compatibility;
- encryption key availability;
- current quota, retention, and organization policy; and
- whether credentials captured before standby must be revoked or rebound.

Forking a compromised or dirty sandbox copies its state. Jobs default to a clean
template, not the previous attempt's snapshot.

A checkpoint cannot undo an external effect. Before checkpoint, restore, or
fork, the runtime records unresolved connector requests and idempotency
identities. The first release requires an explicit quiescent boundary and rejects
an execution edit when it could silently duplicate or discard an unresolved
effect.

## Network and credential boundary

The default is no egress. Approved HTTP traffic passes through a gateway outside
the VM. The gateway:

- authenticates workload identity independently of guest claims;
- resolves DNS independently and blocks metadata, private, loopback,
  link-local, and rebinding targets unless explicitly approved;
- rechecks every redirect and connection;
- applies host, port, scheme, method, path, size, rate, and concurrency limits;
- strips authorization and routing headers supplied by the guest;
- injects short-lived endpoint-bound credentials after admission;
- scrubs configured credential or session material from the response; and
- emits redacted metadata outside the sandbox.

Direct TCP/UDP access is a separate capability with weaker application-layer
control. An allowed model route cannot be reused as a generic forward proxy.

Inbound routes authenticate before wake-up and cap buffered traffic, connection
count, and resume attempts. Public URLs never expose the guest-management API or
the runtime API.

The implemented HTTP preview uses a 256-bit opaque lease with a 30–900 second
lifetime. Every request rechecks current sandbox state and holds a lifecycle
lease until the upstream response closes. The proxy removes substrate routing
headers, guest-management credentials, cookies, referrers, origins, untrusted
forwarding headers, hop-by-hop response headers, and `Set-Cookie`. Absolute-path
redirects remain inside the opaque preview prefix. Preview lease paths must be
redacted from access logs. WebSockets and raw TCP are rejected until they have a
separate bounded tunnel and revocation design.

### Developer-preview connector limits

The implemented preview lease is a signed bearer capability delivered as
non-secret sandbox configuration. It contains no provider credential and
expires after five minutes by default. Authorization is rechecked against the
current project, sandbox state, and connector attachment on every proxy request.

Bearer renewal does not prove that the requester is the original sandbox. A
stolen unexpired lease can be replayed and renewed while that sandbox remains
running. Consequently, the preview is limited to the trusted-operator profile.
The private and shared profiles require proof-of-possession workload identity,
rate and concurrency enforcement, revocation tests, and lease canaries across
full-state checkpoint and restore.

The gateway buffers request and response bodies to bound memory and scrub exact
credential bytes. It does not yet support streaming responses. Public connector
destinations are DNS-checked at connection time; private address space requires
explicit operator opt-in, while loopback, link-local, multicast, unspecified,
and known metadata addresses remain forbidden.

## Storage boundary

Templates are immutable and signed or allowlisted. Checkpoints, volumes,
drives, artifacts, and logs have separate object identities, encryption scopes,
quotas, retention, and deletion semantics.

The current durable workspace is project-scoped and single-writer. The service
rejects cross-project lookup and attachment, concurrent attachment, and deletion
while attached. The guest receives only the selected mount; the substrate
volume content token and signing key remain outside the guest and product API.
The signing key is read from a private file rather than an ordinary environment
value. A create or delete transport failure becomes `unknown`; reconciliation
must confirm the engine resource or its absence before completing the operation.

The single-host backing directory is not encrypted, replicated, backed up, or
securely erased by this project. A host administrator can read it, physical host
loss can destroy it, and filesystem deletion does not prove media sanitization.
These limits are part of the `developer-single-host` profile.

Tool and MCP packages may declare a signed capability manifest, but declarations
are not enforcement. Admission compares requested filesystem, network,
connector, device, and approval capabilities with deterministic organization
policy before the tool becomes available.

Shared drives add confused-deputy and cross-agent risks. Their release requires
filesystem correctness, authorization, namespace, symlink, lock, and cache
invalidation tests. A drive service's object-store credential must not enter a
sandbox as a long-lived secret.

## Availability and abuse

The target architecture places resource controls at API, scheduler, VM,
network, storage, and model-route boundaries. The implemented profile limits
active sandbox and workspace counts, environment and connector counts, live
guest operations, preview leases, request sizes, command duration, and streamed
output. It also caps process-wide in-flight HTTP work while exempting health,
readiness, and content-free metrics. Request IDs are generated by the runtime;
access logs use matched route patterns so opaque preview capabilities and raw
paths do not enter them. vCPU time, memory, PID, disk bytes, IOPS, bandwidth, snapshot size,
wake-up rate, job fan-out, and model consumption still require independent
engine or future product enforcement and must not be advertised as covered.

Delete wins over create, pause, resume, and retry. Every operation is idempotent
and reconciled after controller or node restart. A lost worker becomes unknown
or failed until proven otherwise; it never remains falsely healthy.

## Receipt trust statement

A signed execution receipt is a claim by the configured runtime about image,
policy, lifecycle, route, artifact, and cleanup identities. It is useful for
audit and debugging but does not prove that a malicious host told the truth.

Hardware-backed `attested` status is unavailable until a supported platform
measurement binds runtime identity, policy digest, and a fresh nonce.

## Out of scope for the first release

- malicious host administrators;
- compromised CPU, firmware, host kernel, KVM, or Firecracker;
- speculative-execution and shared-hardware side channels;
- shared-tenant GPU isolation;
- semantic correctness of an agent or model result;
- high-assurance data-loss prevention for arbitrary encrypted traffic; and
- transparent survival of physical host loss without durable state.

## Required validation

- upstream version inventory, vulnerability response, and maximum patch lag;
- VM escape and guest-management isolation review;
- network namespace, metadata, SSRF, DNS rebinding, redirect, request smuggling,
  proxy bypass, and tunnel tests;
- secret canaries proving credential absence from guest and snapshots;
- cross-tenant API, object, route, cache, log, volume, drive, and snapshot tests;
- pause/resume/fork/delete race and fault injection;
- duplicate-effect and unresolved-connector tests around checkpoint, restore,
  and fork;
- job retry contamination and cancellation tests;
- CPU, memory, disk, IOPS, bandwidth, connection, wake, and snapshot exhaustion;
- receipt tamper, replay, wrong-tenant, and wrong-key tests; and
- independent review before a `shared-multitenant` release.
