# Product

## Definition

A self-hosted runtime where AI agents receive an isolated computer, keep useful
state between turns, call approved tools and models, and disappear when the work
is over.

For application developers the product is four nouns:

1. **Environment:** reproducible software, resources, tools, connectors, and
   policy.
2. **Sandbox:** an isolated computer for one agent or session.
3. **Workspace:** files that can outlive or be shared between sandboxes.
4. **Job:** bounded work that can run once or fan out many times.

Images, checkpoints, networks, connectors, rollouts, policies, and receipts
support these concepts but do not lead the user experience.

## Product thesis

The median inference call is becoming a loop: generate, act, inspect, and retry.
Inference alone supplies the reasoning. An agent runtime supplies the safe place
to act, durable context, network access, and lifecycle economics.

The open-source opportunity is not a generic code interpreter. It is a cohesive,
self-hosted agent compute plane that an enterprise can run next to its source,
data, tools, and open-weight models.

## Ideal first users

### AI product team

The team runs coding, research, browser, data, or operations agents and wants an
API instead of building microVM orchestration, persistence, and cleanup.

### Enterprise platform team

The team needs agent execution inside its cloud or data boundary with OIDC,
quotas, network policy, private endpoints, auditable credentials, and no
dependency on a public sandbox provider.

### Open-model platform team

The team operates vLLM, SGLang, TGI, NIM, or another serving stack. It wants
agent compute placed beside inference without giving arbitrary code the model or
cloud credential.

## Core use cases

### Coding agent session

Create a sandbox from a repository image, attach a durable workspace, run shell
and language-server processes, preview a development port, pause while the model
thinks, and resume with the filesystem and processes intact.

### Research or browser agent

Allow a bounded destination set, run a browser and helper services, keep the
session alive across turns, and archive selected outputs rather than the whole
machine.

### Evaluation and RL rollout

Fork a clean template or checkpoint into many isolated workers, run bounded
episodes, collect structured artifacts, cancel stragglers, and retain exact
environment, policy, checkpoint-lineage, and model-connector identities.

### Private data agent

Mount an approved workspace, deny public internet, allow a private tool and
model route, inject credentials outside the VM, and produce a content-minimal
audit trail.

### Long-running operator agent

Keep a stable sandbox identity and persistent workspace while compute moves
between active and standby. Expiration remains explicit and independent from
idleness.

## Developer experience

The happy path should require no infrastructure vocabulary:

```python
with client.sandboxes.create(image="ghcr.io/acme/coder:1") as box:
    box.workspace.attach("repo-main", at="/workspace")
    box.connectors.attach("coding-model")
    box.connectors.attach("github-readonly")
    result = box.run("python agent.py")
```

For an interactive session:

```python
box = client.sandboxes.get("sbx_123")
box.resume()
box.terminal.connect()
box.pause()
```

For batch work:

```python
job = client.jobs.map(
    template="eval-harness:v4",
    inputs=test_cases,
    concurrency=200,
    timeout="15m",
)
```

SDKs return explicit lifecycle and failure states. They never synthesize
progress, silently replace an image, or hide teardown failure.

## Operator experience

A small deployment begins with one command on a KVM-capable Linux machine. A
production deployment uses a declarative configuration for:

- worker pools and placement;
- tenant and project quotas;
- approved images and registries;
- default egress policy;
- secret and identity providers;
- object storage and databases;
- private model routes;
- retention and deletion policy; and
- benchmark and conformance requirements.

The operator sees capacity, sandbox states, failed cleanup, node health, policy
denials, cost drivers, and upstream runtime versions. Customer content is absent
from default telemetry.

## Product editions

The Apache-2.0 project should contain the complete useful system:

- single-host runtime;
- clustered control plane;
- sandbox, volume, job, network, and model-route APIs;
- local identity and policy;
- audit and receipt verification;
- benchmark and conformance suites; and
- deployment automation.

A future commercial distribution can sell operation rather than withholding the
security model:

- hosted regions and capacity;
- zero-downtime fleet upgrades;
- multi-region scheduling and snapshot replication;
- enterprise support and hardened release channels;
- SAML/SCIM, approval workflows, and compliance exports;
- dedicated egress, private connectivity, and managed keys; and
- managed agent-plus-inference placement.

## Differentiation

The project wins by combining capabilities that usually require separate
systems:

- **Self-hostable:** the compute and data plane can remain in the customer's
  environment.
- **Stateful and economical:** the session survives while idle compute is
  released.
- **Inference-aware:** model endpoints are named resources, not arbitrary
  internet destinations.
- **State is explicit:** filesystem and full-state checkpoints have distinct
  compatibility, retention, and fork contracts.
- **Built for durable work:** jobs and rollouts own budgets, retries,
  cancellation, artifacts, and cleanup.
- **Enterprise-safe by default:** no ambient credentials or unrestricted egress.
- **Open substrate:** Firecracker orchestration is inspectable and replaceable.
- **Provable operations:** lifecycle and policy decisions can be verified after
  the run without storing customer content.

Fast resume is necessary, not the moat. The durable position is the integration
of agent compute, storage, network, inference, and policy in a distribution that
an enterprise can actually operate.

## Success metrics

Technical preview:

- installation to first sandbox in under 15 minutes on a supported host;
- create, exec, files, logs, port preview, pause, resume, and destroy pass a
  public conformance suite;
- no resource survives a successful deletion workflow;
- private model access works without placing the real credential in the VM;
- same-node create and resume latency are measured at p50, p95, and p99;
- one hundred concurrent batch tasks complete with bounded retries and cleanup;
- a version upgrade and rollback preserve supported sandbox state; and
- default telemetry contains no prompt, response, source, or terminal content.

Product preview:

- three external design partners run a real agent workload;
- at least one deploys entirely inside its own cloud;
- one workload uses private open-weight inference;
- developers integrate through the SDK without operating Firecracker directly;
- operators can explain every active, standby, failed, expired, and deleting
  sandbox from control-plane state.

## Explicit non-goals for the first release

- worldwide public-cloud capacity;
- training or inference-server implementation;
- a custom hypervisor, kernel, or distributed filesystem;
- GPU access inside untrusted shared-tenant sandboxes;
- transparent cross-host live migration;
- arbitrary TCP egress with credential injection;
- a browser IDE or agent framework; and
- proprietary provider API compatibility without a public conformance profile.
