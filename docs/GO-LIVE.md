# Go live

Brezel can go live as an **open-source, self-hosted private-tenant developer
preview**. It cannot yet be offered as a hostile shared-multitenant managed
service. Those are different releases with different safety gates.

## Launch decision

| Release | Decision | Required boundary |
| --- | --- | --- |
| Public source repository | Ready after the owner checks below | Developer-preview label on every primary entry point |
| Private evaluation for one organization | Ready after exact-revision host qualification | One dedicated KVM host; no untrusted tenants; host-local durability only |
| Public shared sandbox API | Not ready | Requires fleet identity, recovery, abuse controls, security review, and hostile-multitenant assurance |
| Production enterprise deployment | Not ready | Requires OIDC/RBAC, key rotation, backup/restore, upgrade/rollback, operational SLOs, and independent review |

Publishing the source does not upgrade the runtime assurance level. A clone is
qualified only after the destructive suite passes on the machine that will run
it.

## What is ready now

- Apache-2.0 source with pinned and attributed dependencies.
- A compact CLI and HTTP API for create, run, files, preview, standby, resume,
  checkpoint, and delete.
- Firecracker-only release execution with no container fallback.
- Protected project credentials, quotas, bounded operations, explicit failure
  states, reconciliation, and confirmed cleanup.
- A product-owned mTLS node relay for command, file, and preview traffic.
- A reproducible installer, conformance suite, host qualification, benchmark
  harness, threat model, vulnerability policy, and evidence rules.
- An optional TLS public edge with a method-and-path allowlist. Metrics,
  liveness, node control, and engine routes remain private.

The exact capability and evidence boundary is maintained in [Status](STATUS.md).

## Owner checks before making the repository public

1. Confirm that `security@infercrane.com` accepts mail and is monitored.
2. Run CI on the exact commit intended for publication and require it to pass.
3. Scan the complete Git history for credentials, not only the working tree.
4. Run `make qualify-single-host` on a fresh dedicated host at that exact
   commit. Preserve the checksummed result and update [Status](STATUS.md) only
   if the full suite passes.
5. Keep the GitHub repository labelled `developer preview`; do not publish a
   stable release, uptime promise, multitenancy claim, or portable benchmark
   claim.
6. Confirm that Issues and private security reporting are available. Enable
   Discussions only if someone will moderate it.
7. Change repository visibility only as a separate, deliberate owner action.

No source change should silently make a private repository public.

## Qualify an installation

Use a disposable or dedicated Ubuntu 24.04 x86-64 host with KVM,
`/dev/net/tun`, cgroup v2, and enough local SSD capacity for templates,
snapshots, and workspaces. Do not run the destructive workflow on a developer
laptop or a host containing unrelated Docker workloads.

```console
git clone https://github.com/infercrane/brezel.git
cd brezel

make check
make qualify-single-host

export PATH="$PWD/bin:$PATH"
export BREZEL_SERVICE_TOKEN_FILE="$PWD/.brezel/secrets/service.token"
brezel doctor
brezel new --ttl 900
```

The installer leaves `brezeld`, `brezel-node`, and the pinned engine running
under Docker Compose restart policies. The product API listens only on
`127.0.0.1:8080` by default. A successful `/readyz` means the local state,
engine, node data listener, and node control listener are reachable; it does
not prove the host has completed qualification.

## Expose one private-tenant installation

Prefer a private network, VPN, or SSH tunnel. If a public HTTPS endpoint is
needed for an evaluation, point a dedicated DNS name at the host and use the
included Caddy edge:

```console
mkdir -m 700 "$PWD/.brezel/edge-data" "$PWD/.brezel/edge-config"

export BREZEL_PUBLIC_HOST=sandbox.example.com
export BREZEL_ACME_EMAIL=operator@example.com
export BREZEL_EDGE_DATA_DIR="$PWD/.brezel/edge-data"
export BREZEL_EDGE_CONFIG_DIR="$PWD/.brezel/edge-config"

./deploy/public-edge/up.sh
```

Before exposure:

- permit inbound TCP 80 and 443 only to the edge; keep ports 3000, 5007, 8080,
  8443, 8444, databases, Redis, and host metrics unreachable from the internet;
- restrict SSH to operator addresses and keep cloud metadata unreachable from
  guests;
- distribute the service token through a protected file, never a URL,
  command-line flag, environment value, issue, or chat message;
- use one trusted organization only and set conservative sandbox and guest
  operation limits for the host capacity;
- keep guest internet disabled unless an individual workload requires it;
- treat workspaces and snapshots as host-local state with no backup or
  host-loss recovery guarantee; and
- do not put production credentials or irreplaceable customer data in this
  profile.

The public edge intentionally does not expose `/metrics` or `/healthz`.
Operators should scrape `http://127.0.0.1:8080/metrics` over a protected local
or private monitoring path and alert on `/readyz`, HTTP 5xx responses,
admission rejection, failed lifecycle phases, disk pressure, memory pressure,
and Docker restart loops.

## Stop and recover safely

If readiness fails, stop public admission first:

```console
docker compose -f deploy/public-edge/compose.yaml down
```

Do not manually delete engine, workspace, SQLite, node-ledger, or snapshot
files while Brezel is running. Preserve `.brezel/state`, `.brezel/secrets`, the
runtime attestation, logs, and the exact source revision for diagnosis.
Re-running the pinned installer is the supported reconciliation path for this
preview, but interrupted upgrade and rollback are not yet qualified. If a host
is lost, current workspaces and checkpoints must be considered lost.

## Gates for a managed production service

The current release must not be stretched into a public multi-tenant service.
At minimum, that release needs:

- workload identity, OIDC, organizations, RBAC, revocation, and administrative
  audit;
- node enrollment plus online certificate and signing-key rotation;
- multiple nodes, replicated durable state, object-backed snapshots, tested
  backup/restore, and node-loss reconciliation;
- packaged upgrade, rollback, disk-full, interrupted-upgrade, and long-soak
  evidence;
- tenant-aware metering, rate limits, abuse prevention, network isolation, and
  incident controls;
- supported secret providers and proof-of-possession workload credentials;
- capacity planning, SLOs, paging, support ownership, data retention, and
  deletion procedures; and
- an independent security review with no unresolved critical findings.

Until those gates pass, the honest launch message is: **Brezel is a self-hosted
developer preview for one trusted organization on one dedicated KVM host.**
