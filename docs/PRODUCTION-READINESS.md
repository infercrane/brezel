# Production readiness

This document prevents a source-complete feature from being confused with an
operationally qualified service.

## Achieved release gate

The repository is a hardened single-host release candidate:

- the only release backend is the pinned Firecracker runtime engine;
- API credentials are hashed in a protected policy and bound to explicit
  projects;
- project resource and concurrent guest-operation limits are mandatory and
  positive;
- state is private, size-bounded, schema-versioned, identity-validated,
  atomically replaced, file- and directory-synced, and protected by an
  exclusive controller lock;
- readiness checks both durable state and the authenticated engine health path;
- create idempotency keys reject changed payloads;
- checkpoint idempotency keys are bound to checkpoint inputs;
- process-wide admission rejects overload while preserving health endpoints,
  and content-free request IDs, access logs, and Prometheus counters avoid
  tenant and preview-capability labels;
- the API container is non-root, read-only, capability-free, and receives only
  protected file-mounted credentials;
- the Docker context excludes runtime state, credentials, VCS history, and
  build output; and
- engine compose images, build images, and privileged host artifacts are
  independently digest-locked, compose cannot pull after verification, and a
  content-minimal installed distribution manifest is retained; and
- destructive qualification exercises a real workspace, microVM, command,
  file, HTTP preview, pause/resume, checkpoint, replacement sandbox, cleanup,
  signed receipt, project isolation, controller restart, and a second full run.

Run the local code gate on any development machine:

```bash
make check
```

Run the deployment gate only on a dedicated Linux/amd64 host with KVM and TUN:

```bash
make qualify-single-host
```

If UFW is active with routed traffic denied, allow only the pinned engine's
guest proxy ports and routed guest egress before installation (replace `eno1`
with the host's default egress interface):

```bash
sudo ufw allow in from 10.11.0.0/24 to any port 5010:5018 proto tcp \
  comment 'Firecracker guest services'
sudo ufw route allow out on eno1 from 10.11.0.0/24 \
  comment 'Firecracker guest egress'
```

The installer detects an active UFW configuration without these rules and
stops before building a guest. Equivalent nftables policy may be acknowledged
with `RUNTIME_SKIP_UFW_PREFLIGHT=true`; the operator then owns that policy's
qualification.

The second command creates and deletes real microVM resources. It writes three
content-free reports under `.runtime/qualification/`: conformance, an active
sandbox/workspace across controller restart, and conformance after restart. A
report qualifies only the exact named host and source revision on which it ran.

Exercise a full operating-system or provider reboot in two explicit phases so
the active sandbox and workspace identities survive the test driver itself:

```bash
./deploy/single-host/host-reboot-drill.sh prepare
sudo systemctl reboot
# After SSH and the runtime return:
./deploy/single-host/host-reboot-drill.sh verify
```

The verifier rejects a false `running` state, accepts either a genuinely live
sandbox or an explicit replacement, verifies the pre-reboot workspace marker,
and confirms cleanup. It proves local workspace recovery only; it does not
prove process continuity, backup/restore, host availability, or an SLA.

## Development-host boundary

macOS has neither Linux KVM nor `/dev/net/tun`; therefore it cannot run
or qualify the Firecracker execution boundary. Unit, race, HTTP integration,
CLI, schema, patch-integrity, and packaging checks are useful but are not a
substitute for the destructive host gate.

The distribution supports exact preloaded OCI manifests plus local source and
host-artifact mirrors. This is not yet an air-gapped claim: module and template
build inputs are not packaged into one signed release bundle, and an
offline-network installation has not passed qualification. See
[`ARTIFACT-SUPPLY-CHAIN.md`](ARTIFACT-SUPPLY-CHAIN.md).

## Required before a private production SLA

- encrypted workspace backup and restore with a measured recovery point and
  recovery time;
- disk-full, engine restart, controller crash, and upgrade rollback drills;
- repeated host-reboot and physical-host-loss drills on the intended provider,
  including recovery-time objectives and off-host workspace restore;
- pinned host kernel, KVM, Firecracker, guest kernel, base image, and image
  vulnerability policy;
- load, soak, admission, noisy-neighbor, and capacity tests at the advertised
  concurrency;
- external identity or customer-managed service-account issuance, KMS-backed
  signing, credential rotation, and operator audit integration;
- production metric scraping, alerting, log-redaction verification, paging,
  and incident runbooks;
  and
- an independent review of the VM, guest-management, network, connector,
  preview, checkpoint, and workspace boundaries.

## Required before a Blaxel-scale claim

- PostgreSQL-backed authoritative state with schema migration and multi-writer
  transaction tests;
- distributed leases/routing and a scheduler that tolerates worker and zone
  loss;
- replicated object storage for templates and snapshots plus a qualified
  durable workspace service;
- multi-node image distribution, placement, autoscaling, draining, rolling
  upgrades, and regional recovery;
- OIDC/service accounts, organizations, roles, policy administration, dynamic
  quotas, billing/metering, abuse controls, and support operations;
- a public SDK compatibility and semantic-versioning program; and
- externally reproducible latency, availability, isolation, and disaster-
  recovery evidence.

Until those gates pass, the honest availability label is **private single-host
release candidate**, not public cloud or hostile shared multitenancy.
