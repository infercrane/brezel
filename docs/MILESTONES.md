# Milestones

The sequence earns product claims through conformance. Estimates assume two
experienced infrastructure engineers. Exit criteria, not elapsed time, authorize
the next assurance label.

## M0: substrate qualification

**Estimate:** 2 weeks

Build:

- pin and deploy E2B Runtime on one named Linux/KVM host;
- generate a source, artifact, license, and SBOM inventory;
- probe image build, create, exec, files, logs, ports, filesystem checkpoint,
  full-state checkpoint, resume, fork, volume, and destroy;
- fault the API, PostgreSQL, Redis, object storage, orchestrator, and host;
- verify explicit telemetry-off mode and record all outbound traffic;
- benchmark Time to First Instruction, resume, checkpoint, proxy first byte,
  density, and cleanup at p50, p95, and p99; and
- decide upstream contribution, adapter, patch, or rejection for every gap.

Exit criteria:

- every advertised substrate capability has a reproducible test;
- exact state preserved by each checkpoint path is documented;
- cleanup intent survives component restart;
- host prerequisites, unsupported environments, and patch ownership are known;
- measurements name hardware, load, runtime revision, and cache state; and
- ADR 0002 changes from proposed to accepted or rejected.

Stop if isolation, lifecycle, cleanup, licensing, or the patch burden cannot meet
the project boundary without a large fork.

## M1: private developer preview

**Estimate:** 4 to 6 weeks after M0

Build:

- one-command installer and `doctor` for a KVM-capable Linux host;
- Environment, Sandbox, Workspace, Checkpoint, Connector, and Operation APIs;
- `runtimectl` plus Python and TypeScript SDK previews;
- exec, files, logs, terminal, authenticated previews, automatic standby,
  resume, filesystem checkpoint, fork, and durable deletion;
- deny-by-default egress and one private OpenAI-compatible model connector;
- ordered operation events, webhooks, and machine-actionable errors;
- content-minimal lifecycle audit and optional signed receipt; and
- public conformance and benchmark commands.

Exit criteria:

- a fresh supported host reaches first instruction in under 15 minutes of setup;
- the README happy path needs no Firecracker vocabulary;
- direct egress, metadata access, credential retrieval, anonymous wake, and
  guest-management exposure fail closed;
- standby/resume preserves exactly the state declared by its profile;
- expiration cannot be extended by ordinary traffic;
- destroy removes VM, routes, leases, and attachments or records an actionable
  terminal cleanup failure; and
- default telemetry contains no prompt, response, source, terminal, or artifact
  content.

This is the first public release. It supports one trusted operator and makes no
production shared-multitenant claim.

## M2: durable work and full-state preview

**Estimate:** 6 to 8 weeks

Build:

- qualified `full_state` standby, resume, and fork on compatible nodes;
- checkpoint lineage, retention, materialization, and deletion constraints;
- external secret providers, endpoint-bound leases, and response scrubbing;
- Job API with fan-out, concurrency, retry, cancel, budgets, result collection,
  straggler handling, and cleanup;
- a Rollout preview binding environments, model connectors, task artifacts,
  evaluators, branch strategy, and stop conditions;
- sandbox groups for co-located agent, browser, database, or helper computers;
- framework adapters for OpenAI Agents, Anthropic self-hosted sandboxes, and
  MCP; and
- rich event streams suited to agents as well as humans.

Exit criteria:

- one hundred concurrent attempts complete or fail with bounded cleanup;
- every attempt reaches a known terminal state after cancellation;
- retry never reuses unapproved contaminated state;
- secret canaries never appear in guest files, memory checkpoints, logs, or
  receipts;
- restore and fork reject incompatible or unresolved external-effect state; and
- a real open-weight workload completes through a private model connector.

## M3: single-region production preview

**Estimate:** 8 to 12 weeks

Build:

- multiple qualified worker nodes and capability-aware scheduling;
- demand-based warm pools and published TTFI metrics;
- OIDC, organizations, projects, service accounts, RBAC, quotas, and approvals;
- authenticated wake proxy, private networking, and static egress;
- Vault plus one cloud workload-identity integration;
- model-route-aware placement, token/concurrency/spend budgets, and private vLLM
  or SGLang connectivity;
- node drain, rolling upgrade, rollback, backup, restore, and disaster tooling;
- shared Workspace preview only after POSIX correctness qualification; and
- a lightweight operator console over the same control API.

Exit criteria:

- controller and node restarts do not lose desired state or cleanup intent;
- cross-project API, route, object, checkpoint, workspace, and cache tests pass;
- node loss yields an explicit restore or failure state, never phantom running;
- upgrade and rollback preserve supported resources;
- shared workspace tests cover locks, rename, fsync, crash, and concurrent I/O;
- a design partner runs agent execution beside private open-weight inference;
  and
- an independent security review has no unresolved critical issue.

## M4: enterprise and air-gapped release

**Estimate:** 8 to 12 weeks

Build:

- offline installer and signed artifact mirror;
- customer-managed keys, retention, deletion, backup, and support bundles;
- SAML/SCIM, organization approvals, policy bundles, and audit export;
- SPIFFE/SPIRE and KMS/HSM integration options;
- private ingress, private endpoints, customer-managed DNS, and static egress;
- fleet capacity planning, safe upgrades, and long-term-support releases; and
- reference architectures for one-node, private-region, and disconnected
  deployments.

Exit criteria:

- install, upgrade, rollback, backup restore, key rotation, identity revocation,
  and full tenant deletion succeed without public internet;
- administrators can identify the exact environment, runtime, policy,
  connector, and checkpoint revision behind every sandbox; and
- an enterprise design partner completes security review and a failure/recovery
  exercise for the `private-single-tenant` profile.

## M5: qualified advanced profiles

Research and release separately:

- shared-multitenant isolation after external review;
- dedicated GPU or accelerator sandboxes and device cleanup;
- confidential-computing attestation;
- multi-region checkpoint replication;
- semantics-aware checkpoint planning from tool-turn and OS-effect signals;
- trace-driven speculative prewarming; and
- deeper agent-execution and inference co-scheduling.

Do not publish latency, live-migration, shared-tenant GPU, air-gap, or hardware
attestation claims until the exact profile has public conformance evidence.

## Fastest valuable path

```text
signed environment
  -> isolated Firecracker sandbox
  -> durable workspace
  -> approved private model connector
  -> automatic standby and declared-state resume
  -> safe checkpoint or fork
  -> complete cleanup and lifecycle receipt
```

Do not delay this path for a dashboard, custom filesystem, global scheduler,
accelerator passthrough, or predictive prewarming.
