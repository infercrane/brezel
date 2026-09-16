# Linux/KVM qualification: 2026-09-15

This report records the evidence earned by Brezel revision
`67410ab5b928a335a79701d67eaf859df890da9c` with pinned engine revision
`767ceb4b2ec0e598f512767c8da9e5e6618da368`.

The correct release label is **self-hosted private-tenant developer preview**.
The evidence does not qualify a cluster, high availability, hostile shared
multitenancy, or public production.

## Test environment

The coordinator's Google Cloud provisioning record identified two separately
administered `c3-standard-8` machines in `us-central1-b`, each with a 200 GB
`pd-ssd` root disk. The captured host evidence independently records the
operating system, CPU, memory, block device and filesystem, KVM and tun
availability, and runtime identities; it does not attest the cloud control-plane
machine type, zone, or storage-class claims.

Each independent Brezel installation had:

- Ubuntu 24.04, x86-64, kernel `7.0.0-1011-gcp`;
- 8 vCPU and approximately 32 GiB RAM;
- an Intel Xeon Platinum 8481C CPU reported by the guest;
- approximately 200 GB of root filesystem capacity;
- KVM, `/dev/net/tun`, 9,216 2 MiB huge pages, and 32 prepared network slots;
- a 512 MiB base guest and a limit of three simultaneous sandbox starts; and
- the API, node control listener, and node data listener bound to loopback.

Both checkouts were clean and bound to the exact revision above. Runtime
attestation re-observed the running image and executable hashes. The attestation
is an install provenance record, not hardware attestation or reproducible-build
proof.

## Results

### Complete per-host matrices

Both hosts passed the complete 24-cell matrix. Each matrix exercised eight
customer-visible operations in sequential, staggered, and 16-way burst modes.

| Result | Host A | Host B | Combined |
| --- | ---: | ---: | ---: |
| Passing cells | 24 / 24 | 24 / 24 | 48 / 48 |
| Successful attempts | 1,056 / 1,056 | 1,056 / 1,056 | 2,112 / 2,112 |
| Confirmed attempt cleanup | 1,056 / 1,056 | 1,056 / 1,056 | 2,112 / 2,112 |
| Confirmed resource cleanup | 1,584 / 1,584 | 1,584 / 1,584 | 3,168 / 3,168 |
| Censored latency samples | 0 | 0 | 0 |

Selected customer-observed service latency:

| Scenario | Host A sequential p50 / p95 | Host B sequential p50 / p95 | Host A / B burst p50 |
| --- | ---: | ---: | ---: |
| Create through first verified instruction | 70.457 / 79.553 ms | 70.957 / 80.804 ms | 384.931 / 359.690 ms |
| Warm command | 12.058 / 14.764 ms | 12.775 / 14.544 ms | 93.455 / 91.287 ms |
| Resume through verified instruction | 413.699 / 1,252.882 ms | 360.628 / 1,491.089 ms | 1,122.993 / 1,026.206 ms |
| Filesystem checkpoint | 373.435 / 707.069 ms | 339.132 / 776.949 ms | 3,274.995 / 3,857.706 ms |
| Filesystem restore | 96.673 / 1,264.942 ms | 92.432 / 1,399.960 ms | 2,453.265 / 577.153 ms |
| Preview first byte | 5.592 / 7.050 ms | 5.083 / 6.107 ms | 23.610 / 19.911 ms |
| Warm preview | 2.654 / 3.247 ms | 2.624 / 3.312 ms | 12.797 / 13.197 ms |
| 1 MiB durable workspace write and read | 126.550 / 141.238 ms | 133.726 / 150.869 ms | 888.538 / 767.283 ms |

The 16-way checkpoint p95 remained high at 13,041.533 ms on Host A and
12,781.970 ms on Host B. Filesystem-restore burst behavior also differed
materially between the hosts. These results authorize a reliability statement
for this workload and host profile, not a portable tail-latency or competitive
performance claim.

### Immediate-command burst stress

A prior revision reproduced two command-stream closures immediately after VM
restore. Revision `67410ab5` does not publish a sandbox until the authenticated
guest data plane answers a bounded health probe, and it ships a conservative
three-start admission default on this 8-vCPU profile.

The unchanged 16-way create-through-command workload was repeated ten times on
each host:

| Result | Value |
| --- | ---: |
| Attempts | 320 |
| Successful create-through-command attempts | 320 / 320 |
| Confirmed sandbox cleanup | 320 / 320 |
| Pooled p50 | 436.508 ms |
| Pooled p95 | 709.198 ms |
| Pooled p99 | 812.188 ms |
| Maximum | 1,419.422 ms |

This is evidence that the reproduced first-command failure did not recur in 320
attempts. It is not proof that no stream can ever become indeterminate. Brezel
does not automatically replay a command after a stream closes because the
process may have started and replay could duplicate user effects.

### Independent-host failure containment

The paired workflow passed on the same exact revision:

1. both hosts passed 29 of 29 simultaneous conformance steps;
2. simultaneous eight-way create, filesystem-restore, and workspace-I/O cases
   completed with 100% success and confirmed cleanup;
3. cross-host namespace probes could not resolve the other host's resources;
4. a controller `SIGKILL` on Host A recovered readiness and its fixture in
   4,083 ms while Host B completed 133 of 133 canary commands; and
5. a node-relay `SIGKILL` on Host B recovered readiness and its fixture in
   925 ms while Host A completed 135 of 135 canary commands.

These are two independent deployments. There is no shared scheduler, shared
state, replicated storage, automatic placement, or failover path.

### Provider-reset workspace recovery

After the matrices and paired workflow, each host independently ran the
provider-reset drill against the same exact runtime revision. In both cases:

1. the reset changed the Linux boot identity;
2. the runtime attestation before and after the reset matched;
3. the original sandbox was reported `failed`, not falsely `running`;
4. a replacement sandbox attached the same host-backed workspace;
5. all six crash-consistency files matched their pre-reset sizes and SHA-256
   digests, including overwrite, truncate, atomic rename, nested creation, and a
   1 MiB payload; and
6. replacement sandboxes and workspaces were deleted with confirmed cleanup.

The complete drills took 84 seconds on Host A and 165 seconds on Host B,
including the provider reset and recovery. This qualifies same-host
crash-durable workspace recovery for the tested profile. It does not qualify
transparent sandbox or process resume, physical host-loss recovery, backup, or
replicated durability.

## Evidence

The sanitized raw reports are retained in
[`evidence/qualification-2026-09-15`](../evidence/qualification-2026-09-15).
Each evidence set has its own SHA-256 manifest:

- [`full-matrix-host-a`](../evidence/qualification-2026-09-15/full-matrix-host-a)
- [`full-matrix-host-b`](../evidence/qualification-2026-09-15/full-matrix-host-b)
- [`burst-stress-host-a`](../evidence/qualification-2026-09-15/burst-stress-host-a)
- [`burst-stress-host-b`](../evidence/qualification-2026-09-15/burst-stress-host-b)
- [`dual-independent-hosts`](../evidence/qualification-2026-09-15/dual-independent-hosts)
- [`host-reboot-host-a`](../evidence/qualification-2026-09-15/host-reboot-host-a)
- [`host-reboot-host-b`](../evidence/qualification-2026-09-15/host-reboot-host-b)
- [`computesdk-gcp-f9fbc0e`](../evidence/qualification-2026-09-15/computesdk-gcp-f9fbc0e)

The paired-host and reboot reports contain no service token, private key,
public IP address, email address, workload content, or absolute coordinator
path. The ComputeSDK reports retain the now-ephemeral public benchmark endpoint
for provenance, but contain no credential or customer workload.

## ComputeSDK profile qualification

Revision `f9fbc0ede72636349b27f01db49343d8daa87c5c` was installed on a separate
32-vCPU GCP N2 KVM host in `us-east4`. Both mutually exclusive profiles ran the
29-step destructive single-host workflow, engine fast-path check, active API
restart, node restart, and post-restart checks before measurement.

The 2-vCPU/512-MiB profile then completed ten consecutive 100-sandbox waves:
1,000 of 1,000 sandboxes reached a successful `node -v`, every wave held all
100 sandboxes simultaneously, and all 1,000 deletions were confirmed. Aggregate
TTI was 2.431 s p50, 2.937 s p95, and 3.182 s p99 from a neutral HTTPS client.

The host was reinstalled with the 8-vCPU/16-GiB DAX profile. All three
sandboxes were deleted and the project was empty afterward, but this report is
now reclassified nonconformant. One attempt printed `gyp ERR! configure error`
and an install-script failure while the enclosing upstream shell still returned
zero. The retained 63.272 s guest median and 68.435 s adapter median are
historical diagnostics, not performance evidence. The rehearsal runner now
rejects these unambiguous hidden dependency failures.

The 100-way Burst result clears its internal functional entry gate; DAX does
not. Neither is an official ComputeSDK provider run. The public harness still
needs a stable regional endpoint and provider integration, and the current
Burst latency is not a leaderboard-leading result. See
[Benchmarking](BENCHMARKING.md).

## Exact claim scope

> Revision `67410ab5b928a335a79701d67eaf859df890da9c` passed the complete
> 24-cell matrix on each of two separately administered Ubuntu 24.04 x86-64 KVM
> hosts: 48 of 48 cells, 2,112 of 2,112 attempts, and 3,168 of 3,168 expected
> resource cleanups. An additional 320 of 320 immediate-command burst attempts
> succeeded with complete cleanup. The paired workflow passed simultaneous
> conformance, namespace isolation, and controller/node-relay crash containment.
> Both hosts then passed provider-reset workspace-recovery drills with exact
> six-file corpus verification and confirmed cleanup.

> This qualifies the tested private single-host profile. It does not qualify a
> cluster, shared control plane, multi-node scheduler, automatic placement or
> failover, cross-host restore, replicated storage, high availability, hostile
> shared multitenancy, public-production availability, or an SLA.
