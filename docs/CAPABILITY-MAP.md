# Product capability map

This map converts the product thesis into build, integrate, qualify, and defer
decisions. It is a planning document, not a release claim.

## The credible vertical slice

```text
resolve signed environment
  -> create isolated Firecracker sandbox
  -> exec and stream events
  -> use one durable workspace
  -> call one approved private model connector
  -> enter standby and resume with declared state
  -> checkpoint or fork at a safe boundary
  -> destroy every route, lease, VM, and attachment
  -> verify a content-minimal receipt
```

## Decision table

| Capability | User promise | Initial path | Decision | Release gate |
| --- | --- | --- | --- | --- |
| microVM lifecycle | hostile code does not share the host kernel | E2B Runtime and Firecracker | integrate and qualify | escape review, version pin, fault tests |
| environment build | reproducible agent computer | OCI input plus signed resolved manifest | build facade, reuse template builder | digest and provenance verified |
| process and files | normal Linux execution | existing guest daemon | integrate | auth, streaming, limits, error conformance |
| automatic standby | release compute without losing declared state | full-state checkpoint where supported | qualify | exact state and latency profile published |
| filesystem checkpoint | reusable disk state | immutable object-store artifact | integrate and harden | restore, retention, encryption, lineage tests |
| full-state checkpoint | filesystem, processes, and memory | substrate snapshot | qualify separately | CPU/device compatibility and quiescence tests |
| fork | independent branch from a checkpoint | substrate fork plus control-plane lineage | integrate and build | no shared lease/credential confusion |
| durable workspace | data outlives a sandbox | single-writer volume | integrate first | attach, crash, backup, quota, delete tests |
| shared workspace | concurrent agent files | maintained POSIX system such as JuiceFS | evaluate later | full filesystem correctness suite |
| authenticated wake | stable address resumes a sandbox | client proxy plus policy recheck | integrate and harden | bounded queue, rate, auth, denial tests |
| egress policy | no undeclared destination access | host firewall plus HTTP gateway | build/qualify | metadata, DNS, redirect, tunnel tests |
| connectors | use tools without possessing credentials | external broker and opaque handles | preview built; qualify next | proof-of-possession, endpoint binding, lease, response-scrub tests |
| private model routes | approved vLLM/SGLang/TGI access by alias | connector specialization | build narrowly | no general egress, route budget tests |
| durable jobs | bounded fan-out and cancellation | project coordinator | build | retries, stragglers, idempotency, cleanup |
| agent rollouts | reproducible episodes over models and evaluators | job specialization | build after jobs | budget, lineage, evidence, cancellation |
| sandbox groups | co-located private helper computers | scheduler topology primitive | build after single sandbox | group identity and network-isolation tests |
| lifecycle events | observe without polling | operation stream and webhooks | build | ordered IDs, replay, deduplication |
| signed receipts | verify runtime observations | in-toto statement plus DSSE | build | key, tenant, replay, tamper tests |
| operator console | diagnose fleet and cleanup | thin view over control API | defer to production preview | no alternate source of truth |
| Kubernetes backend | portable enterprise deployment | Agent Sandbox/Kata/gVisor adapter | evaluate after Firecracker profile | same API conformance, weaker claims explicit |
| GPU sandbox | dedicated accelerator execution | dedicated single-tenant profile | defer | reset, residue, driver, escape review |
| confidential runtime | hardware-backed attestation | SEV-SNP/TDX profile | research | nonce-bound verified measurement |
| semantic checkpointing | avoid unnecessary snapshots | turn and OS-effect planner | research | recovery correctness before efficiency |
| speculative warming | overlap boot with model thinking | scheduler hints and predictor | research | real trace benefit and bounded waste |

## Product advantages to earn

1. **Private by construction:** a real zero-telemetry and no-unapproved-egress
   profile, not a VPC-only marketing claim.
2. **State users can reason about:** explicit filesystem and full-state
   checkpoints, lineage, retention, and deletion.
3. **Model-aware without owning the model server:** private and open-weight
   routes live in policy and placement, not as arbitrary URLs.
4. **Runtime contracts:** deterministic admission plus evidence that execution
   and cleanup occurred.
5. **Self-hosting that feels like a product:** preflight, one-command preview,
   signed mirrors, upgrades, rollback, backup, and air-gap support.

## Deferred distractions

- universal provider compatibility;
- a general agent framework;
- a hosted global capacity fleet;
- custom hypervisor or distributed filesystem work;
- full browser IDE;
- checkpoint merge;
- shared-tenant accelerators; and
- performance claims copied from another environment.
