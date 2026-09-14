# 2026 research and project map

This inventory records evidence behind the product and substrate decisions. It
is not a security endorsement. Every dependency still needs a pinned revision,
license review, threat review, and project conformance result.

## Market signal: Blaxel and Baseten

Baseten's September 2026 acquisition announcement describes Blaxel as a stateful
execution layer for agents: fast isolated sandboxes, persistent storage, and
production networking. It explicitly says inference was the primitive Blaxel did
not own and presents the combined direction as agent execution, inference, and
training on one system.

- [Baseten acquisition announcement](https://www.baseten.co/blog/blaxel-is-joining-baseten-to-build-the-future-of-agentic-cloud/)

Design consequence: the project should be a complete agent compute primitive and
make private inference proximity first-class. A policy-only wrapper would miss
the strategic product.

## AWS Lambda MicroVMs and operator feedback

AWS Lambda MicroVMs makes managed Firecracker lifecycle a hyperscaler primitive:
Dockerfile-derived images are initialized into memory-and-disk snapshots;
instances receive a dedicated HTTPS endpoint and short-lived authentication;
idle policy can suspend and transparently resume state; and AWS names sessions,
jobs, AI sandboxes, and RL environments as direct uses. Its documented eight-
hour runtime and ARM64-only initial profile are AWS product boundaries, not
limits of the underlying architecture.

- [AWS launch architecture](https://aws.amazon.com/blogs/aws/run-isolated-sandboxes-with-full-lifecycle-control-aws-lambda-introduces-microvms/)
- [AWS Lambda MicroVMs guide](https://docs.aws.amazon.com/lambda/latest/dg/lambda-microvms-guide.html)

Design consequence: Firecracker isolation, pre-initialized snapshots, suspend,
resume, and authenticated routing are necessary capabilities but no longer a
standalone product wedge. A self-hosted system must offer a stronger developer
contract above them.

The associated Hacker News discussion is anecdotal operator feedback rather
than technical evidence. Repeated themes include snapshot/fork, usable SSH or
VPN access, credentials hidden at the network boundary, GPU and UDP needs, and
concern that fixed CPU/memory microVM shapes waste capacity under bursty agent
loads. It also reinforces that teams distinguish a raw compute primitive from a
complete developer product.

- [Hacker News discussion](https://news.ycombinator.com/item?id=48642510)

Design consequence: measure utilization and idle economics, but do not divert
the first release into a new elastic hypervisor. Lead with branchable,
evidence-bearing private agent trials; treat resource elasticity as a later
scheduler/runtime research profile.

## Blaxel's public runtime architecture

Blaxel documents an evolution from Kubernetes/Knative, to managed containers, to
bare-metal Firecracker microVMs with a custom orchestrator and networking layer.
Its current public material describes:

- microVM isolation;
- optimized kernel and EROFS image artifacts;
- process, filesystem, port, preview, and MCP APIs;
- automatic idle standby and request-triggered resume;
- memory and filesystem checkpointing;
- a client routing layer;
- persistent volumes and shared Agent Drive;
- outbound policy, static egress, and model gateway;
- jobs and high-concurrency fan-out; and
- organization roles, policies, quotas, and enterprise networking.

Blaxel reports approximately 25 ms resume on its infrastructure. That result is
specific to its implementation and environment, not a transferable project
claim.

- [Anatomy of a runtime](https://blaxel.ai/blog/anatomy-of-a-runtime)
- [Sandbox overview](https://docs.blaxel.ai/Sandboxes/Overview)
- [Sandbox lifecycle](https://blaxel.ai/blog/understand-the-lifecycle-of-a-blaxel-sandbox)
- [Processes](https://docs.blaxel.ai/Sandboxes/Processes)
- [Networking](https://blaxel.ai/platform/networking)
- [Agent Drive](https://blaxel.ai/platform/agent-drive)
- [Storage choices](https://blaxel.ai/blog/choose-the-right-storage-for-your-blaxel-agents)
- [Jobs](https://docs.blaxel.ai/Jobs/Overview)

Design consequence: a credible runtime must combine compute, state, routing, and
operations. Resume latency is a full-stack property involving snapshots, image
format, CPU compatibility, cache locality, scheduling, and networking.

The 2026 changelog adds a clearer direction: independent workspace-level
snapshots, full-state fork, S3-compatible shared storage, method/path-aware
egress policy, dynamic proxy values, reusable long-running application
endpoints, framework adapters, agent-led onboarding, process logs, job metrics,
ephemeral job volumes, private registries, enterprise identity, and telemetry
that is disabled by default. These are public roadmap signals, not an API or
implementation specification.

- [Platform changelog](https://docs.blaxel.ai/changelog)

## Stateful runtime patterns from Daytona

Daytona's public documentation separates interface, control, and compute planes
and distinguishes filesystem, memory, and external-storage persistence. It also
documents linked co-located sandboxes, webhooks instead of polling, OCI snapshot
storage, S3-backed multi-writer volumes, and organization tenancy.

Its credential flow is especially relevant: an opaque placeholder is placed in
the sandbox and an external proxy substitutes the secret only for an approved
HTTPS destination, then scrubs configured sensitive response values.

- [Architecture](https://www.daytona.io/docs/en/architecture/)
- [Persistence](https://www.daytona.io/docs/en/persistence/)
- [Secrets](https://www.daytona.io/docs/en/secrets/)
- [Network limits](https://www.daytona.io/docs/en/network-limits/)
- [Scale](https://www.daytona.io/docs/en/scale/)

Design consequence: checkpoint kind, retention, and compatibility must be
explicit; connector response scrubbing and signed webhooks belong in the plan;
co-located sandbox groups are useful for agents with browsers, databases, or
trusted helpers.

Daytona's repository declares AGPL-3.0 at the time of this review. Learn from
published behavior and standards, but do not copy its source into an
Apache-licensed codebase without a deliberate licensing decision.

## Snapshot and companion patterns from Modal

Modal separately documents filesystem, directory, and memory snapshots with
different retention and limitations. Its experimental sidecars can separate an
agent harness, credential proxy, or local service from the main sandbox, but
sidecars are not compatible with Modal's VM sandboxes and do not share memory
snapshots.

- [Sandbox snapshots](https://modal.com/docs/guide/sandbox-snapshots)
- [Sandbox sidecars](https://modal.com/docs/guide/sandbox-sidecars)
- [Volumes](https://modal.com/docs/guide/volumes)
- [Security and retention](https://modal.com/docs/guide/security)

Design consequence: do not hide materially different state promises behind one
`snapshot` boolean. Trusted companions are a topology feature with their own
isolation and checkpoint contract, not an automatic addition to the first VM
profile.

## Reproducible environments and local UX

Docker Sandboxes' 2026 Kit spec and environment files describe the complete
agent environment declaratively: agent, workspace, setup, permissions,
networking, credentials, ports, and resources. Its MCP gateway keeps OAuth
credentials host-side and applies Cedar policy. The release notes also expose
the value of structured startup progress, validation, signatures, provenance,
and actionable policy errors.

Fly Sprites presents the sandbox as a stable Linux computer with an object-backed
persistent disk, checkpoint history, an authenticated URL that wakes it, and
connectors that keep credentials outside the VM. Cloudflare's Sandbox SDK puts a
durable identity and lifecycle object in front of replaceable containers, and
documents that local and production restore semantics differ.

- [Docker Sandboxes release notes](https://docs.docker.com/ai/sandboxes/release-notes/)
- [Fly Sprites](https://fly.io/sprites/)
- [Cloudflare Sandbox architecture](https://developers.cloudflare.com/sandbox/concepts/architecture/)
- [Cloudflare directory backups](https://developers.cloudflare.com/sandbox/concepts/backup-restore/)

Design consequence: add a signed `Environment` manifest, a `doctor` command,
machine-actionable startup events, stable sandbox identity, authenticated wake,
and a documented difference between developer and production profiles.

## Primary substrate: E2B Runtime

E2B Runtime is the closest permissively licensed open-source substrate for the
target. Its repository documents the backend used across E2B Cloud, Enterprise,
and self-hosted deployments, including:

- Go control API and per-node orchestrator;
- Firecracker microVMs;
- a guest environment daemon;
- a client proxy for sandbox URLs;
- template building and pre-booted snapshots;
- lazy memory restore and copy-on-write root filesystems;
- pause/resume, idle auto-pause, transparent wake, and fork;
- persistent volumes, per-sandbox egress controls, secret metadata, and workload
  identity interfaces;
- PostgreSQL, Redis, object storage, and optional ClickHouse roles; and
- single-host, Terraform, and evaluation Kubernetes deployment paths.

The repository is Apache-2.0 as of this review. One important qualification gap
remains: the public architecture refers to an `orchestrator-ee` component for
resolving customer secrets at egress, but that package was not present in the
reviewed `packages/` tree. The project must not claim open-source endpoint-bound
secret injection until that path is replaced or verified.

- [Repository](https://github.com/e2b-dev/runtime)
- [Architecture](https://github.com/e2b-dev/runtime/blob/main/docs/ARCHITECTURE.md)

Design consequence: qualify and extend E2B Runtime. Do not begin by rebuilding
Firecracker lifecycle. The open-source product should invest in distribution,
durable jobs, enterprise policy, private networking, inference routes, shared
workspaces, and conformance.

## Shared workspace candidate: JuiceFS

JuiceFS provides a distributed POSIX filesystem over object storage with
separate metadata engines, multi-client access, caching, and Kubernetes support.
Its repository is Apache-2.0 as of this review.

- [Repository](https://github.com/juicedata/juicefs)

Design consequence: evaluate it as the `Drive` substrate after the single-writer
volume path is stable. A shared filesystem brings correctness and credential
risks; integration is not enough without crash, locking, coherency, and tenant
tests.

## Alternative and complementary substrates

### Kubernetes Agent Sandbox

The Kubernetes SIG Apps project defines `Sandbox`, `SandboxTemplate`,
`SandboxClaim`, and `SandboxWarmPool` and supports runtime classes such as gVisor
and Kata. It is a strong future backend for Kubernetes-native organizations.

- [Repository](https://github.com/kubernetes-sigs/agent-sandbox)
- [Quickstart](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/examples/quickstart/README.md)
- [Threat model](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/docs/security/threat_model.md)
- [2026 roadmap](https://github.com/kubernetes-sigs/agent-sandbox/blob/main/roadmap.md)

Its roadmap prioritizes a portable runtime backend, first-class routing,
automatic suspend/resume, smart warm-pool selection, claim-time identity,
network and storage policy, SDKs, MCP, operator UI, and Time to First Instruction
measurement.

Design consequence: keep the public API independent from the E2B adapter, make
TTFI a standard benchmark, and retain Kubernetes Agent Sandbox as the strongest
future Kubernetes-native backend candidate.

### NVIDIA OpenShell

OpenShell combines container supervision, Landlock, seccomp, proxy-enforced
egress, endpoint-bound credentials, provider profiles, and multiple compute
drivers. Its policy and secret-injection design is valuable for the egress layer,
but it is not a replacement for the Firecracker data plane.

- [Repository](https://github.com/NVIDIA/OpenShell)
- [Sandbox architecture](https://github.com/NVIDIA/OpenShell/blob/main/architecture/sandbox.md)
- [Governance evidence proposal](https://github.com/NVIDIA/OpenShell/issues/2745)

### Sandbox0

Sandbox0 separates compute from durable encrypted filesystem state and uses
replaceable Kubernetes runtime pods with gVisor support.

- [Repository](https://github.com/sandbox0-ai/sandbox0)

### OpenSandbox

OpenSandbox offers lifecycle, execution, Docker/Kubernetes support, SDKs, MCP,
credential-vault, and code-interpreter features. It is useful for API and
conformance comparison.

- [Repository](https://github.com/opensandbox-group/OpenSandbox)

### BoxLite

BoxLite embeds OCI-backed microVMs across Linux, Apple Silicon, and WSL. It may
be useful for a future local developer backend, but local portability does not
replace the first production Linux/Firecracker profile.

- [Repository](https://github.com/boxlite-ai/boxlite)

### Sandlock

Sandlock combines Landlock, seccomp-BPF, and seccomp user notification for
unprivileged local process confinement. It remains useful for trusted local tools
or a lower-assurance backend, but it must not carry the microVM multi-tenant
claim.

- [Paper](https://arxiv.org/abs/2605.26298)
- [Repository](https://github.com/multikernel/sandlock)

## Agent framework integration

OpenAI's 2026 Agents SDK externalizes sandbox state and defines provider
interfaces. Anthropic documents the operator's responsibility when using
self-hosted sandboxes.

- [OpenAI: next Agents SDK](https://openai.com/index/the-next-evolution-of-the-agents-sdk/)
- [OpenAI sandbox guide](https://openai.github.io/openai-agents-python/sandbox/guide/)
- [Anthropic self-hosted sandboxes](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes)
- [Anthropic security model](https://platform.claude.com/docs/en/managed-agents/self-hosted-sandboxes-security)

Design consequence: build adapters. Do not tie sandbox state or lifecycle to one
agent SDK.

## Security research that changes the design

### Comparative sandbox security

A 2026 comparative study distinguishes microVM, userspace-kernel, and OCI
isolation and emphasizes downstream version pinning and patch cadence.

- [Paper](https://arxiv.org/abs/2606.08433)

Design consequence: runtime version, patch lag, and conformance are release
gates. Selecting Firecracker does not finish the security work.

### Security considerations for AI agents

Research responding to NIST concerns emphasizes confused-deputy risks, indirect
prompt injection, layered controls, and deterministic policy for high-impact
actions.

- [Paper](https://arxiv.org/abs/2603.12230)

Design consequence: the model cannot authorize its own permissions. Tenant and
organization policy remain outside the sandbox and agent.

### Execution and governance receipts

Recent tool- and governance-receipt work supports separating agent claims from
trusted runtime observations.

- [Tool Receipts, Not Zero-Knowledge Proofs](https://arxiv.org/abs/2603.10060)
- [AgentBound](https://arxiv.org/abs/2606.30970)

Design consequence: receipts are useful infrastructure, but not the entire
product and not proof of an honest host without attestation.

### Semantics-aware checkpoint and restore

Crab aligns checkpointing with agent turns and uses OS-visible effects to avoid
checkpointing turns with no recovery-relevant state. SpecBox overlaps sandbox
prewarming with model generation. Separate work on safe execution edits shows
that restore or fork can duplicate unresolved external actions even when VM
state itself is valid.

- [Crab](https://arxiv.org/abs/2604.28138)
- [SpecBox](https://arxiv.org/abs/2607.23933)
- [Safe checkpoint, fork, restore, and merge](https://arxiv.org/abs/2608.22928)

Design consequence: record tool-turn boundaries, dirty-state signals,
idempotency identities, and unresolved external effects now. Begin with explicit
quiescent checkpoints and demand-based warm pools. Treat eBPF-driven checkpoint
planning and semantic prewarming as later, trace-validated optimizations.

### Runtime safety contracts

Current research argues that agent safety requires preventive controls and
evidence that required actions actually occurred. Related work on practically
secure tools combines declared effects with runtime enforcement.

- [Agent Safety Should Be a Runtime Contract](https://arxiv.org/abs/2608.11274)
- [Towards Practically-Secure Tools for AI Agents](https://atlas.cs.brown.edu/pdf/haven:euromlsys:2026.pdf)

Design consequence: add signed tool capability manifests, deterministic
admission, and an evidence chain, while keeping the host policy engine as the
authority.

## Category-level user pain

Independent vendor-specific complaint volume is too small to support confident
claims about any one provider. Reddit discussions instead expose broad,
anecdotal category pain:

- developers want a one-command local path with no mandatory hosted account;
- self-hosting is perceived as Nomad, Terraform, or Kubernetes expertise before
  reaching a first sandbox;
- persistence, pause/resume, and external storage are easily confused;
- isolation without enforced egress does not prevent exfiltration;
- full desktop, browser, package installation, and Docker workflows exceed the
  assumptions of small code-interpreter sandboxes; and
- integrations pay a documentation and API-discovery tax, especially around
  incomplete lifecycle behavior.

- [Self-hosting and persistence discussion](https://www.reddit.com/r/LocalLLaMA/comments/1rse8gr/im_building_an_opensource_e2b_alternative_with/)
- [One-command local sandbox discussion](https://www.reddit.com/r/LocalLLaMA/comments/1vrps78/what_sandbox_are_you_all_using_for_ai_agents/)
- [Provider integration comparison](https://www.reddit.com/r/AI_Agents/comments/1ve5y68/i_compared_5_sandbox_providers_by_making_a/)
- [Full VM and computer-use discussion](https://www.reddit.com/r/LocalLLaMA/comments/1sf2nwq/running_ai_agents_in_sandboxes_vs_isolated_vms/)

Design consequence: optimize first-use UX, publish exact persistence and
isolation contracts, make network policy visible, and test whether an agent can
integrate from the docs without maintainer help. Treat these threads as design
inputs, not representative market research.

## Standards to reuse

### in-toto and DSSE

Use an in-toto statement and DSSE envelope for optional signed execution
receipts rather than inventing a signing container.

- [Attestation framework](https://github.com/in-toto/attestation)
- [Specification](https://github.com/in-toto/attestation/blob/main/spec/README.md)

### SPIFFE and SPIRE

Use SPIFFE workload identities in production profiles where they fit rather than
building a second cluster identity protocol.

- [SPIRE concepts](https://spiffe.io/docs/latest/spire-about/spire-concepts/)
- [SPIFFE workload identity](https://spiffe.io/docs/latest/spiffe/concepts/)

## Resulting opportunity

The defensible open-source product is an integrated, self-hosted agent runtime:

- E2B-powered Firecracker sandboxes and snapshots;
- simple sandbox, workspace, and job experience;
- private network and endpoint-bound credentials;
- private/open-weight model routes near execution;
- explicit volume and shared-drive semantics;
- durable operations, cancellation, and cleanup;
- enterprise deployment, identity, quota, and air-gap support; and
- public benchmarks, conformance, and optional signed receipts.

This is more ambitious than a lightweight process sandbox but far more feasible
than rebuilding every data-plane component before serving one user.
