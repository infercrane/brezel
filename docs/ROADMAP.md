# Roadmap

Brezel optimizes for a small, dependable interface. Each milestone earns a
larger operational claim through conformance, failure testing, and measurements.
Dates do not authorize claims; evidence does.

## 1. Self-hosted private-tenant developer preview

The user path is `new`, `run`, files, preview, stop, start, and delete.

Delivered and qualified at revision
`121d7c6952c5bbc0010c365817ef540a1efbaca6`:

- an out-of-process `brezel-node` with separate mTLS control and data listeners
- generation-fenced route reconciliation between the API and node ledger
- command, file, and preview traffic through the node relay by default
- safe install drain, restart, reconciliation, and confirmed cleanup boundaries
- one-command installation with pinned inputs and runtime attestation
- destructive qualification on two separately administered single-host runtimes,
  including simultaneous conformance and bounded crash-containment drills

Remaining before a broader operational label:

- explicit node enrollment plus online certificate and signing-key rotation
- service-unit upgrade and rollback packaging
- publication of the complete per-host 24-cell benchmark matrices
- reproducible reboot, disk-full, interrupted-upgrade, and longer soak evidence

Current label: **self-hosted private-tenant developer preview**. The paired-host
qualification is evidence for two independent installations, not a cluster,
automatic failover, or high availability.

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
