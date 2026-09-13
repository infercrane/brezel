# Open agent runtime strategy

> Research snapshot: 2026-09-13. Vendor performance and scale statements are
> treated as claims until reproduced by this project's conformance suite.

## Executive decision

Build a private computer substrate for agents, not a generic code interpreter
and not an agent framework.

The product gives each agent a durable Linux computer with explicit checkpoint
semantics, controlled access to tools and models, and a verifiable lifecycle.
It starts on one customer-operated Linux host, grows into a single-region fleet,
and later supports air-gapped and confidential-computing profiles.

The shortest durable position is:

> Full Linux state, private model access, and runtime policy in the customer's
> own infrastructure.

Four properties make this a product rather than a microVM wrapper:

1. **A coherent state model.** Filesystem persistence, full-state hibernation,
   fork lineage, shared workspaces, retention, and deletion have different
   contracts.
2. **Safe connectivity.** Agents receive opaque connector handles; an external
   broker admits the request, injects a short-lived credential, and scrubs the
   response.
3. **Durable agent work.** Jobs and rollouts own fan-out, retries, budgets,
   checkpoints, stop conditions, evidence, and cleanup.
4. **Inspectable operation.** Every important transition produces a
   content-minimal event and an optional signed receipt. Private profiles have
   an automated no-unapproved-egress test.

## What the market evidence says

Public product roadmaps are converging on persistent computers rather than
single-request containers. Current products expose independent snapshots,
forking, shared files, wake-on-request, jobs, richer egress rules, credential
brokers, and framework adapters.[1][2][3][4]

The important lesson is not any vendor's API. It is that compute, state,
networking, credentials, and lifecycle economics must work as one system.

### Patterns worth adopting

| Evidence | Product lesson | Project decision |
| --- | --- | --- |
| Daytona documents filesystem, memory, and external-storage persistence separately.[2] | “Persistent” is too vague. | Expose `filesystem` and `full_state` checkpoints with honest compatibility and retention. |
| Daytona uses opaque secret placeholders, destination-bound substitution, and response scrubbing.[3] | Keeping a secret out of environment variables is necessary but insufficient. | Build a connector broker outside the guest; add response scrubbing and per-request lease records. |
| Modal separates filesystem, directory, and memory snapshots and documents different TTLs and limitations.[4] | Snapshot type and retention belong in the public contract. | Give checkpoints an explicit kind, compatibility digest, expiry, lineage, and restore limitations. |
| Modal sidecars separate the untrusted workload from trusted proxies and helpers.[5] | Privileged helpers should not expand the guest TCB. | Use host services first; later support a trusted companion only where the isolation profile can qualify it. |
| Docker's 2026 sandbox environments and Kit spec describe setup, permissions, network, credentials, and agent instructions declaratively.[6] | Reproducible agent environments need more than an OCI image. | Add a signed `Environment` manifest that resolves to immutable image, policy, tool, connector, and resource revisions. |
| Fly Sprites keeps a stable computer identity, wakes on URL traffic, checkpoints the filesystem, and keeps credentials in connectors.[7] | Session identity and easy reconnection matter more than container vocabulary. | Stable sandbox address, authenticated wake, checkpoint history, and tested connectors are first-class. |
| Kubernetes Agent Sandbox is decoupling API from runtime and standardizing Time to First Instruction.[8] | Backend portability and a user-visible latency metric should be designed early. | Keep the public API substrate-neutral and publish TTFI percentiles by runtime profile. |
| Cloudflare uses a durable identity object in front of a replaceable container and documents restore differences.[9] | Durable control identity must survive data-plane replacement. | The control-plane object owns lifecycle; a VM is one replaceable realization of desired state. |

### Category pain, not manufactured competitor criticism

There is not enough high-quality independent discussion of every individual
vendor to claim a vendor-specific complaint pattern. The useful Reddit evidence
is category-level and anecdotal:

- developers ask for a one-command local mode with no mandatory hosted account;
- self-hosting is perceived as Terraform, Nomad, or Kubernetes work before a
  first sandbox appears;
- users confuse filesystem persistence, pause/resume, and durable external
  storage;
- a sandbox without enforced egress still permits data exfiltration;
- hosted backends create privacy and lock-in concerns;
- integrations spend meaningful time discovering SDK behavior and incomplete
  lifecycle semantics; and
- full desktop, browser, package installation, and Docker compatibility often
  push users beyond a small code-interpreter container.[10][11][12][13]

These discussions are not representative surveys. They are inputs to design
partner interviews and conformance tests, not market-size evidence.

## Product contract

### User-facing primitives

Keep the happy path small:

1. **Environment:** reproducible software, resources, tools, connectors,
   policy, and startup actions.
2. **Sandbox:** one durable agent computer with a stable identity.
3. **Workspace:** data that outlives compute, single-writer first and shared
   only under a qualified profile.
4. **Job:** bounded asynchronous work over one or many clean sandboxes.

Advanced resources remain inspectable without leading the developer experience:

- **Checkpoint:** immutable `filesystem` or `full_state` restore point;
- **Connector:** an approved external service or model route with no plaintext
  long-lived credential in the guest;
- **Sandbox group:** co-located computers with explicit private connectivity;
- **Rollout:** a reproducible set of agent episodes, model routes, evaluators,
  budgets, branch strategy, and stop conditions;
- **Policy:** deterministic admission and runtime controls;
- **Receipt:** a signed, content-minimal record of what the runtime observed and
  enforced.

Ship maintained environment packs for the common hard cases: coding, browser
and computer use, data analysis, Docker-in-VM, evaluation, and rollout workers.
An environment pack is signed configuration and build input, not a privileged
agent framework hidden inside the runtime.

### The first magical workflow

```text
runtime up
  -> create a signed environment from an OCI image
  -> start a Firecracker sandbox
  -> attach one durable workspace
  -> allow one private OpenAI-compatible model connector
  -> run an agent and expose an authenticated preview
  -> enter standby after idle
  -> wake with the promised state
  -> checkpoint or fork
  -> destroy compute, routes, and leases
  -> verify the lifecycle receipt
```

This must work on one supported host before building a global scheduler or a
large dashboard.

## Architecture decisions

### Initial data plane

Qualify a pinned E2B Runtime release for Firecracker lifecycle, template build,
guest process/filesystem APIs, snapshot mechanics, port routing, and volumes.
Its Apache-2.0 license permits commercial use subject to notices and dependency
review.[14]

Do not expose E2B internals in the public resource identity. The adapter reports
capabilities and exact unsupported states. A future Kubernetes Agent Sandbox,
Kata, gVisor, or trusted local backend must satisfy the same profile-specific
conformance contract before it can be selected.

### Control plane

PostgreSQL owns desired state, immutable revisions, operations, idempotency,
lineage, and cleanup intent. Redis may hold reconstructable routing and short
leases. Object storage holds images, checkpoints, artifacts, receipts, and
backups under tenant-scoped encryption. OpenTelemetry is off or local by default
in private profiles.

Every mutation is asynchronous, idempotent, and event-producing. SDKs and agents
consume event streams or webhooks rather than tight polling. Errors include a
stable code, retryability, failed capability, operation ID, and suggested
operator action.

### Checkpoint contract

`filesystem` preserves the declared writable disk state. `full_state` also
preserves process memory and compatible device state. A checkpoint includes:

- source sandbox and parent checkpoint;
- image, kernel, guest, runtime, CPU, device, and policy digests;
- storage and encryption identity;
- created and expiry times;
- compatibility constraints;
- in-flight external effects known at the checkpoint boundary; and
- content-independent integrity metadata.

Fork creates a new independent sandbox and a lineage edge. A parent checkpoint
cannot be deleted while a dependent sandbox still requires it unless the child
is materialized independently.

Checkpointing cannot undo an API call, message, payment, or other external
effect. The first release allows checkpoint and fork only at an explicit quiescent
boundary. Later work can use the execution record to reject unsafe restore or
branch operations.[15]

### Connector and model path

A connector revision binds:

- destination, protocol, method, and path policy;
- an opaque secret or workload-identity handle;
- allowed headers and request limits;
- response headers or patterns that must be scrubbed;
- rate, concurrency, byte, token, and optional spend budgets;
- region and data-boundary metadata; and
- audit and retention policy.

Private and open-weight model routes are connector specializations. The SDK
uses a local alias; the broker selects the approved endpoint and injects a
short-lived credential outside the VM. Model access never implies unrestricted
network access.

### Jobs and agent rollouts

A job is a durable map/reduce-shaped execution resource. An attempt starts from
an immutable environment or qualified checkpoint and owns its timeout, retry,
result artifacts, and cleanup.

A rollout adds model and evaluation semantics without becoming an agent
framework:

- one environment and one or more model connector revisions;
- dataset or task artifact digests;
- branch/fork strategy;
- concurrency, token, time, and cost budgets;
- evaluator and required evidence;
- stop, cancel, and straggler rules; and
- declared output artifacts.

This is the direct bridge to open-weight evaluation, RL rollouts, coding-agent
benchmarks, and inference co-placement.

### Policy and receipts

The policy engine has two faces. Admission prevents unsupported or forbidden
execution. Evidence records whether required actions and cleanup actually
occurred. This matches current research arguing that agent safety needs both
preventive gates and evidential runtime records.[16]

Receipts use in-toto statements and DSSE envelopes rather than a custom signing
format.[17] A signed receipt proves only what the configured runtime claims. It
is not hardware attestation. Confidential profiles later bind a fresh nonce and
platform measurements from SEV-SNP, TDX, or supported accelerator confidential
computing.[18]

## Research features to design for, not prematurely ship

### Semantics-aware checkpoints

Crab reports that more than 75% of studied agent turns did not create
recovery-relevant state, and uses eBPF-observed effects to avoid unnecessary
checkpoints while preserving recovery.[19] Record tool-turn boundaries and dirty
state signals in the MVP. Evaluate an eBPF checkpoint planner only after the
simple quiescent protocol is correct.

### Speculative prewarming

SpecBox predicts upcoming sandbox demand while a model is still generating and
reports improvements to tail latency and memory use.[20] Add a `warm_hint` input
and TTFI telemetry to the scheduler contract now. Start with demand-based warm
pools; semantic prediction remains experimental until real traces show value.

### Safe execution edits

Recent work formalizes when checkpoint, fork, restore, and merge can duplicate
or lose external effects.[15] Do not implement merge in the early product.
Store effect boundaries, idempotency identities, and unresolved external calls
so a future safety checker has the facts it needs.

### Capability-aware tools

Tool descriptions may omit real operating-system and network effects. Future
tool packages should carry a signed capability manifest declaring filesystem,
network, connector, device, and approval needs. Runtime enforcement remains
authoritative even when a semantic risk classifier is present.[21]

## Milestone plan

Dates are estimates for two experienced infrastructure engineers. Exit criteria,
not elapsed time, authorize the next product claim.

### M0: qualify the substrate — 2 weeks

- pin source and build inputs; generate license inventory and SBOM;
- run create, exec, file, terminal, port, checkpoint, resume, fork, volume, and
  destroy probes on named hardware;
- fault API, Redis, object store, orchestrator, and worker independently;
- verify telemetry-off and capture all outbound traffic;
- publish p50/p95/p99 TTFI, resume, checkpoint, and cleanup measurements; and
- accept or reject ADR 0002.

**Exit:** lifecycle, isolation, cleanup, license, and patch ownership are known.
No product facade work hides a failing substrate.

### M1: private developer preview — 4 to 6 weeks

- one-command install on a KVM-capable Linux host;
- Environment, Sandbox, Workspace, Checkpoint, Operation, and Connector APIs;
- CLI plus Python and TypeScript SDKs;
- exec, files, logs, terminal, authenticated previews, automatic standby,
  resume, filesystem checkpoint, fork, and deletion;
- deny-by-default egress and one private OpenAI-compatible model connector;
- explicit zero-telemetry mode, `doctor`, conformance, and benchmark commands;
- lifecycle events, webhooks, and a content-minimal receipt.

**Exit:** a fresh host reaches first instruction in under 15 minutes of operator
setup; the full magical workflow passes; direct egress, metadata access, secret
retrieval, and anonymous wake tests fail closed.

### M2: durable work and full-state preview — 6 to 8 weeks

- qualified `full_state` standby/resume/fork on compatible nodes;
- external secret providers, endpoint-bound leases, and response scrubbing;
- Job API with fan-out, retry, cancel, collection, budgets, and cleanup;
- checkpoint lineage, retention, materialization, and deletion constraints;
- sandbox groups for co-located agent, browser, database, or helper processes;
- maintained coding, browser/computer-use, data, and Docker-in-VM environment
  packs;
- first rollout API and evaluator/artifact integration;
- rich machine-actionable errors and live operation events.

**Exit:** 100 concurrent attempts complete or fail with bounded cleanup; secret
canaries never appear in guest state or snapshots; cancellation reaches a known
terminal state for every attempt.

### M3: single-region production preview — 8 to 12 weeks

- multiple worker nodes with capability-aware scheduling and warm pools;
- OIDC, organizations, projects, service accounts, RBAC, quotas, and approvals;
- node drain, upgrade, rollback, backup, restore, and disaster exercises;
- private VPC routes, static egress, Vault and one cloud identity provider;
- model-route-aware placement and route budgets;
- shared workspace preview only after POSIX correctness qualification;
- lightweight operator console focused on capacity, lifecycle, denials,
  failures, cost drivers, and cleanup.

**Exit:** restart and node-loss tests preserve desired state; cross-project tests
pass; upgrade and rollback preserve supported objects; an independent security
review has no unresolved critical issue.

### M4: enterprise self-hosted release — 8 to 12 weeks

- signed offline artifact mirror and air-gapped installer;
- customer-managed keys, retention, deletion, backup, and support bundles;
- SAML/SCIM, policy bundles, approval workflows, and audit export;
- fleet capacity planning and long-term-support release channel;
- reference architectures for one-node, private-region, and disconnected
  deployments;
- external review of the exact `private-single-tenant` profile.

**Exit:** install, upgrade, rollback, key rotation, identity revocation, restore,
and full tenant deletion succeed without public internet.

### M5: qualified advanced profiles

- shared multi-tenant isolation after external review;
- dedicated accelerator sandboxes and cleanup;
- confidential-computing attestation;
- multi-region checkpoint replication;
- semantics-aware checkpoint planning; and
- trace-driven speculative prewarming.

These remain separately labeled previews until each exact profile has public
conformance evidence.

## What not to build yet

- an agent planner, chat UI, or framework;
- an inference server or training scheduler;
- a custom hypervisor, kernel, distributed filesystem, or global network;
- transparent process migration across arbitrary CPU types;
- checkpoint merge;
- shared-tenant GPU isolation;
- a broad provider-compatibility promise; or
- a dashboard before the CLI workflow is reliable.

## Validation with design partners

Recruit three teams with different hard requirements:

1. a coding or browser agent needing persistent state and previews;
2. a private-data enterprise needing self-hosting and strict connectors; and
3. an open-weight model team needing high-volume rollouts near vLLM or SGLang.

For each partner, capture setup time, TTFI, active/standby ratio, checkpoint and
resume latency, retry and cleanup failures, storage growth, model-route latency,
operator interventions, and integration time. Do not collect prompts, source, or
terminal content by default.

The product is ready for a public developer preview when two independent users
complete the core workflow from documentation, one without direct maintainer
help. It is ready for an enterprise claim only after a customer operates the
private profile through an upgrade, failure, and recovery exercise.

## Sources

1. Blaxel, “Platform changelog and release notes,” <https://docs.blaxel.ai/changelog>.
2. Daytona, “Persistence,” <https://www.daytona.io/docs/en/persistence/>.
3. Daytona, “Secrets,” <https://www.daytona.io/docs/en/secrets/>.
4. Modal, “Sandbox snapshots,” <https://modal.com/docs/guide/sandbox-snapshots>.
5. Modal, “Sandbox sidecars,” <https://modal.com/docs/guide/sandbox-sidecars>.
6. Docker, “Docker Sandboxes release notes,” <https://docs.docker.com/ai/sandboxes/release-notes/>.
7. Fly.io, “Sprites: Linux computers for agents,” <https://fly.io/sprites/>.
8. Kubernetes SIG Apps, “Agent Sandbox roadmap,” <https://github.com/kubernetes-sigs/agent-sandbox/blob/main/roadmap.md>.
9. Cloudflare, “Sandbox SDK architecture” and “Directory backups,” <https://developers.cloudflare.com/sandbox/concepts/architecture/> and <https://developers.cloudflare.com/sandbox/concepts/backup-restore/>.
10. Reddit r/LocalLLaMA, “What sandbox are you all using for AI agents?”, <https://www.reddit.com/r/LocalLLaMA/comments/1vrps78/what_sandbox_are_you_all_using_for_ai_agents/>.
11. Reddit r/LocalLLaMA, “I'm building an open-source E2B alternative,” <https://www.reddit.com/r/LocalLLaMA/comments/1rse8gr/im_building_an_opensource_e2b_alternative_with/>.
12. Reddit r/AI_Agents, “I compared 5 sandbox providers,” <https://www.reddit.com/r/AI_Agents/comments/1ve5y68/i_compared_5_sandbox_providers_by_making_a/>.
13. Reddit r/LocalLLaMA, “Running AI agents in sandboxes vs. isolated VMs,” <https://www.reddit.com/r/LocalLLaMA/comments/1sf2nwq/running_ai_agents_in_sandboxes_vs_isolated_vms/>.
14. E2B, “Runtime,” <https://github.com/e2b-dev/runtime>.
15. Zheng et al., “When Can Agents Safely Checkpoint, Fork, Restore, and Merge?”, <https://arxiv.org/abs/2608.22928>.
16. Ng et al., “Agent Safety Should Be a Runtime Contract,” <https://arxiv.org/abs/2608.11274>.
17. in-toto, “Attestation Framework,” <https://github.com/in-toto/attestation>.
18. C8s authors, “C8s: A Framework for Confidential Kubernetes,” <https://arxiv.org/abs/2604.26974>.
19. Wu et al., “Crab: A Semantics-Aware Checkpoint/Restore Runtime for Agent Sandboxes,” <https://arxiv.org/abs/2604.28138>.
20. Zhang et al., “SpecBox: Speculative Sandbox Scheduling for Efficient LLM Agent Serving,” <https://arxiv.org/abs/2607.23933>.
21. Hines et al., “Towards Practically-Secure Tools for AI Agents,” <https://atlas.cs.brown.edu/pdf/haven:euromlsys:2026.pdf>.
