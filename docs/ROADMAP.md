# Roadmap

Brezel optimizes for a small, dependable interface. Each milestone earns a
larger operational claim through conformance, failure testing, and measurements.
Dates do not authorize claims; evidence does.

## 1. Publishable single-host developer preview

The user path is `new`, `run`, files, preview, stop, start, and delete.

Build next:

- package the implemented relay foundation as an out-of-process `brezel-node`
- add explicit node enrollment plus certificate and signing-key rotation
- reconcile API placement state with the protected node generation ledger
- make command, file, and preview traffic bypass the durable API by default
- safe node drain, restart, reconciliation, and cleanup
- one-command installation on a fresh supported host
- reproducible sequential, staggered, burst, reboot, and soak benchmarks

Exit gate:

- all local unit, race, contract, and security-negative tests pass
- two destructive qualification runs pass around an API restart
- a node restart invalidates old capabilities and previews
- failed and timed-out operations reach a known cleanup state
- current-host p50, p95, p99, success rate, and raw attempts are published

## 2. Complete agent computer

Make the simple interface useful for real coding and research agents.

- arbitrary OCI environment builds with signed manifests
- PTY and SSH transport
- Python and TypeScript SDKs
- full-state checkpoints and fork after state semantics qualify
- batch jobs and bounded agent rollouts
- endpoint-bound secret leases and streaming response scrubbing
- warm capacity, local NVMe artifact cache, snapshot prefetch, and network pools
- stable error taxonomy and framework adapters

Exit gate:

- a fresh host reaches first verified instruction in under 15 minutes
- representative coding-agent and batch workloads run end to end
- credential canaries never enter guest files, snapshots, logs, or receipts
- cancellation and retries do not reuse contaminated state
- latency improvements reproduce against the same revision and host profile

## 3. Production fleet

Scale the same interface without asking application developers to understand
placement or recovery.

- multiple qualified nodes and capability-aware scheduling
- replicated durable state and object-backed checkpoint storage
- workspace backup and restore
- OIDC, organizations, service accounts, RBAC, quotas, and approvals
- demand-driven warm pools and admission-aware placement
- node loss, drain, rolling upgrade, rollback, and disaster exercises
- private ingress, static egress, and managed secret providers
- production load, soak, and failure-injection suites

Exit gate:

- no phantom-running state after node or controller loss
- cross-project isolation tests cover API, routes, objects, cache, and storage
- upgrades and rollback preserve every supported resource
- recovery objectives are measured and documented
- an independent review has no unresolved critical security issue

## 4. Enterprise and private inference

Brezel becomes the secure execution primitive beside private and open-weight
model fleets.

- offline installation and signed artifact mirrors
- customer-managed keys and retention controls
- SAML/SCIM, policy bundles, approval workflows, and audit export
- private vLLM and SGLang routes with token, concurrency, and spend budgets
- accelerator-aware agent sandboxes where isolation and device cleanup qualify
- single-region and disconnected reference architectures

Exit gate:

- install, upgrade, rollback, backup restore, key rotation, identity revocation,
  and full project deletion pass without public internet
- operators can identify the exact environment, policy, connector, substrate,
  and checkpoint revision behind every sandbox
- an enterprise design partner completes security and recovery review

## Research tracks

These remain separate until their exact profiles are qualified:

- hostile shared-multitenant isolation
- confidential-computing attestation
- multi-region checkpoint replication
- semantics-aware checkpoint planning
- trace-driven speculative prewarming
- deeper agent-execution and inference co-scheduling
