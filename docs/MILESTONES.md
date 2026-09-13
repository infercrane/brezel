# Milestones

The sequence starts from a working upstream microVM runtime and earns production
claims through conformance. Estimates assume two experienced infrastructure
engineers. One engineer should reduce scope rather than pretend the schedule is
unchanged.

## M0: substrate qualification

**Time box:** 2 weeks

Build:

- pin and deploy E2B Runtime on one bare-metal Linux/KVM host;
- document upstream components, APIs, versions, patches, and licenses;
- run template build, create, exec, files, logs, ports, pause, resume, fork,
  volume, and destroy probes;
- trace state across API, PostgreSQL, Redis, object storage, orchestrator, guest,
  and proxy;
- create the first compatibility and lifecycle test harness;
- benchmark cached create, warm exec, snapshot, resume, proxy first byte, memory,
  disk, and idle resource use; and
- make an upstream-or-adapter decision for every gap.

Exit criteria:

- every advertised upstream capability has a reproducible passing test;
- crash and restart behavior is known for API, orchestrator, Redis, and host;
- exact host prerequisites and unsupported environments are documented;
- latency distributions are published with hardware and load, not as universal
  claims; and
- ADR 0002 is updated from proposed to accepted or rejected.

Stop condition: if upstream pause/resume, isolation, cleanup, or licensing cannot
meet the project boundary without a large fork, revisit the substrate before
building a product layer.

## M1: single-host developer preview

**Time box:** 4 to 6 weeks after M0

Build:

- reproducible installer around E2B Embed or its supported single-host profile;
- `runtimectl`, a small API facade, and Python and TypeScript SDK previews;
- image, sandbox, operation, volume, and model-route resources;
- create, exec, filesystem, logs, terminal, ports, pause, resume, and destroy;
- idle standby separate from expiration;
- idempotent mutations and durable cleanup reconciliation;
- deny-by-default egress with an HTTP allowlist;
- one approved OpenAI-compatible private model route;
- content-minimal lifecycle audit and optional signed execution receipt;
- benchmark and conformance commands; and
- examples for a coding agent and a private-model evaluation.

Exit criteria:

- fresh supported host reaches a first sandbox in under 15 minutes;
- the SDK happy path fits in the README and needs no Firecracker knowledge;
- direct egress, metadata access, credential retrieval, and guest-management
  exposure tests fail closed;
- standby/resume preserves the state that the profile explicitly promises;
- expiration cannot be extended by anonymous or unqualified traffic;
- destroy cleans VM, routes, leases, attachments, and snapshots or reports an
  actionable terminal failure; and
- no customer content appears in default telemetry.

This is the first public release. It is for a trusted operator and does not make
a production multi-tenant SLA claim.

## M2: single-region production preview

**Time box:** 6 to 10 weeks

Build:

- multiple qualified bare-metal worker nodes;
- PostgreSQL source of truth, Redis live routing, S3-compatible artifacts, and
  OpenTelemetry;
- OIDC, organizations, projects, service accounts, RBAC, and quotas;
- scheduler with CPU compatibility, snapshot locality, image cache, region,
  capacity, and failure-domain awareness;
- authenticated client proxy with bounded wake-on-request;
- durable job API with map, concurrency, timeout, retry, cancel, and collection;
- node drain, rolling upgrade, backup, restore, and disaster exercises;
- policy bundles, registry restrictions, retention, and deletion controls; and
- Helm/Terraform or equivalent supported deployment automation.

Exit criteria:

- 100 concurrent batch attempts complete under a published load profile;
- API/controller/node restarts do not lose desired state or cleanup intent;
- cross-project authorization, route, object, snapshot, and volume tests pass;
- node loss produces an explicit restore or failure state, never phantom running;
- upgrade and rollback preserve supported objects; and
- an independent security review has no unresolved critical findings.

## M3: collaborative state and inference proximity

**Time box:** 6 to 10 weeks, driven by design partners

Build:

- shared `Drive` preview using a maintained POSIX filesystem such as JuiceFS;
- coherent attach/detach, snapshot/backup, quota, and tenant policy;
- model-route-aware placement and private connectivity to vLLM or SGLang;
- per-route concurrency, request-size, token, and spend limits;
- credential broker integrations for Vault and one cloud secret manager;
- MCP and framework adapters; and
- optional checkpoint reuse for evaluation and rollout jobs.

Exit criteria:

- filesystem correctness suite covers locking, rename, fsync, crash, and
  concurrent readers/writers;
- a sandbox cannot recover the long-lived model credential;
- model routes do not grant general network access;
- a real open-weight agent workload shows measured latency and economics with
  execution and inference placed in one region; and
- state recovery succeeds after worker replacement.

## M4: enterprise and air-gapped release

Build:

- offline installation and signed artifact mirror;
- SAML/SCIM and organization approval workflows;
- SPIFFE/SPIRE workload identity option;
- KMS/HSM signing and credential integrations;
- static egress, private endpoints, customer-managed DNS, and policy exports;
- searchable audit and retention controls;
- capacity planning and fleet upgrade tooling; and
- hardened release channel with patch-lag policy and security advisories.

Exit criteria:

- an installation succeeds without outbound internet;
- key rotation, identity revocation, backup restore, and full tenant deletion are
  exercised;
- administrators can prove which upstream runtime and image revision ran every
  sandbox; and
- an enterprise design partner completes its security review.

## M5: advanced runtime research

Qualify separately and ship only with evidence:

- high-throughput VPP or equivalent networking;
- CPU feature templates and cross-node snapshot movement;
- cross-region snapshot replication;
- confidential-computing attestation;
- dedicated GPU sandboxes and accelerator cleanup;
- model-training or RL data-plane integration; and
- deeply co-scheduled agent and inference capacity.

Do not put 25 ms resume, live migration, shared-tenant GPU isolation, or hardware
attestation in release copy until a public profile and conformance result exists.

## The fastest valuable path

The product becomes useful at M1. The critical vertical slice is:

```text
OCI image
  -> isolated Firecracker sandbox
  -> persistent state across idle pause/resume
  -> approved private model call
  -> declared output
  -> complete cleanup and audit
```

Do not delay this slice for a dashboard, multi-region scheduler, custom
filesystem, GPU passthrough, or custom virtual network.
