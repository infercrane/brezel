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
than rebuilding Blaxel's entire custom bare-metal stack before serving one user.
