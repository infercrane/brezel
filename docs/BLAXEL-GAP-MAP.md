# Blaxel capability and gap map

This document uses Blaxel's public product and engineering material to identify
the right open-source product shape. It is not an attempt to copy proprietary
code, undocumented interfaces, performance tuning, or trademarks.

## What Blaxel teaches

The useful abstraction is not a short-lived container. It is a session-first
computer with serverless economics:

- strong isolation for arbitrary agent code;
- fast create and resume;
- processes and files that survive an idle interval;
- lifecycle controlled by activity but expiration controlled separately;
- ingress that wakes the sandbox;
- durable and shared storage beyond one machine;
- private network and credential boundaries;
- large fan-out for batch and agent loops; and
- inference close enough that repeated agent calls do not pay avoidable network
  latency.

Blaxel's public architecture also demonstrates that a fleet-grade result needs
bare metal, Firecracker, snapshot-aware scheduling, a guest API, a client proxy,
optimized image artifacts, and high-performance networking. These are systems,
not UI features.

## Build, reuse, or defer

| Capability | Blaxel public shape | Open-source plan | Decision |
| --- | --- | --- | --- |
| microVM lifecycle | Firecracker on bare metal | E2B Runtime | reuse |
| guest process/files API | REST/MCP service inside VM | E2B `envd` | reuse |
| image/template pipeline | kernel + immutable root image + metadata | E2B template builder | reuse |
| pause/resume | memory and filesystem checkpoint | E2B snapshots; qualify exact behavior | reuse and test |
| fork/rollback | checkpoint-derived sandbox | E2B fork where supported | reuse and test |
| wake-on-traffic | regional gateway resumes sandbox | E2B client proxy | reuse and harden |
| persistent single-writer storage | attached durable volume | E2B volumes | reuse and test |
| shared agent filesystem | distributed multi-client drive | evaluate JuiceFS | integrate later |
| batch fan-out | thousands of bounded tasks | durable project job coordinator | build |
| private networking | workload identities and private routes | standard Linux/VPC/Cilium first | build/integrate |
| high-performance data plane | IPv6 and VPP | only after measured need | defer |
| egress allowlist | per-workload outbound rules | qualify E2B nftables and SNI/Host controls; extend policy only where needed | reuse and harden |
| secret injection | credential absent from sandbox | qualify E2B workload-identity hooks; implement an OSS provider where reviewed source is absent | reuse/build gap |
| co-located model gateway | provider/model routes near compute | private model-route registry and gateway | build narrowly |
| organizations and governance | workspaces, roles, policies, quotas | OIDC/RBAC/policy/quotas | build |
| observability | lifecycle, logs, metrics | OpenTelemetry, content-free defaults | integrate |
| global public cloud | managed multi-region capacity | not an OSS MVP requirement | defer/commercial |
| agent framework | session-oriented managed runtime | adapters, not a new framework | defer core |
| training integration | combined agent/inference/training cloud | future ecosystem integration | defer |

## Minimum credible clone of the product category

An open-source release is credible only when this entire path works:

```text
build OCI template
  -> create isolated sandbox
  -> exec and stream logs
  -> read/write files
  -> expose authenticated port
  -> automatically enter standby
  -> resume with promised state
  -> attach durable volume
  -> call one approved private model
  -> destroy every resource
```

A dashboard around `docker exec` is not a Blaxel-like runtime. A Firecracker demo
without durable operations, routing, policy, and cleanup is also not a product.

## Deliberate differences

The project should be stronger than a direct clone in four places:

1. **Self-host first.** Installation, upgrades, backups, and air-gap operation
   are product features, not deployment footnotes.
2. **Open-weight inference first.** Private vLLM/SGLang routes, endpoint-bound
   credentials, and co-placement are first-class.
3. **Policy is portable.** Users can inspect and export the effective sandbox,
   network, image, retention, and model-access policy.
4. **Evidence is verifiable.** Optional signed receipts bind execution and
   cleanup identities without retaining source, prompts, or terminal content.

## What not to copy

- product or company names, logos, written copy, SDK naming, or screenshots;
- unpublished API behavior;
- claimed latency or scale numbers;
- proprietary scheduler, VMM, networking, filesystem, or image code; and
- exact hosted pricing or commercial packaging.

The implementation must be clean-room and source its runtime behavior from
permissively licensed projects and public standards.
