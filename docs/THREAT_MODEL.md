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

The first release supports `developer-single-host`. Documentation and APIs may
describe later profiles but must not label them qualified prematurely.

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
- project API, scheduler, lifecycle reconcilers, proxy, and policy engine;
- PostgreSQL, Redis routing data, and object storage control paths;
- identity, secret, KMS, and signing providers; and
- administrators able to configure or access the platform.

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
10. A guest cannot forge control-plane terminal state or receipt fields.
11. Cleanup failure remains visible and retryable.
12. Customer content collection is opt-in, tenant-scoped, and separate from
    operational telemetry.

## MicroVM boundary

Firecracker supplies a hardware-virtualized boundary with a deliberately small
device model, but it does not eliminate hypervisor, KVM, host-kernel, firmware,
or side-channel risk. The exact kernel, Firecracker, E2B, guest, and host versions
must be pinned and covered by a patch-lag policy.

Root inside a guest is still untrusted. Guest management APIs require per-
sandbox identity and are never exposed by public preview routes.

Plain containers may be useful for trusted development tasks but cannot carry
the project's microVM or hostile multi-tenant assurance label.

## Snapshot and fork boundary

Snapshots can contain process memory, tokens, files, browser sessions, and other
secret material. They are customer-confidential data, encrypted at rest, scoped
to the tenant, and deleted through durable reconciliation.

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

## Network and credential boundary

The default is no egress. Approved HTTP traffic passes through a gateway outside
the VM. The gateway:

- authenticates workload identity independently of guest claims;
- resolves DNS independently and blocks metadata, private, loopback,
  link-local, and rebinding targets unless explicitly approved;
- rechecks every redirect and connection;
- applies host, port, scheme, method, path, size, rate, and concurrency limits;
- strips authorization and routing headers supplied by the guest;
- injects short-lived endpoint-bound credentials after admission; and
- emits redacted metadata outside the sandbox.

Direct TCP/UDP access is a separate capability with weaker application-layer
control. An allowed model route cannot be reused as a generic forward proxy.

Inbound routes authenticate before wake-up and cap buffered traffic, connection
count, and resume attempts. Public URLs never expose `envd` or the control API.

## Storage boundary

Templates are immutable and signed or allowlisted. Root snapshots, volumes,
drives, artifacts, and logs have separate object identities, encryption scopes,
quotas, retention, and deletion semantics.

Shared drives add confused-deputy and cross-agent risks. Their release requires
filesystem correctness, authorization, namespace, symlink, lock, and cache
invalidation tests. A drive service's object-store credential must not enter a
sandbox as a long-lived secret.

## Availability and abuse

Resource controls exist at API, scheduler, VM, network, storage, and model-route
boundaries. The system limits sandbox count, vCPU, memory, PID, disk, IOPS,
bandwidth, ports, connections, snapshot size, wake-ups, job fan-out, retries,
runtime, and model consumption.

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
- job retry contamination and cancellation tests;
- CPU, memory, disk, IOPS, bandwidth, connection, wake, and snapshot exhaustion;
- receipt tamper, replay, wrong-tenant, and wrong-key tests; and
- independent review before a `shared-multitenant` release.
