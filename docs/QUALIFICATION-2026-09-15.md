# Two independent-host qualification: 2026-09-15

This report records the claim earned by Brezel revision
`121d7c6952c5bbc0010c365817ef540a1efbaca6`. It covers two separately
administered Ubuntu 24.04 x86-64 KVM machines, each running a complete
single-host Brezel installation with `/dev/net/tun`.

The release label is **self-hosted private-tenant developer preview**.

## Attested release boundary

Before destructive work, the coordinator verified that both hosts:

- used distinct SSH targets, machine identities, kernel boot identities, and
  node certificate authorities;
- ran the exact clean Brezel revision named above;
- pinned the same engine revision;
- ran identical attested `brezeld` and `brezel-node` executable identities;
- bound the live container image and executable hashes to that clean source
  revision in each host's protected runtime-attestation manifest;
- exposed the API and node control/data listeners on loopback only;
- reported KVM, `/dev/net/tun`, controller readiness, and reconciled engine
  capacity; and
- had synchronized clocks within the qualification harness's bounded
  uncertainty limit.

The runtime manifest records observed source, image, and executable identities.
It is not a signature, hardware attestation, or reproducible-image proof.

## Qualification result

The paired workflow passed:

1. authenticated empty-project checks on each host before creating resources;
2. simultaneous destructive conformance on both independent runtimes;
3. overlapping cached-template burst observations for time to first verified
   instruction, filesystem restore, and workspace I/O;
4. namespace-negative probes showing that public sandbox and workspace IDs from
   one host were not resolvable through the other host, while both local fixture
   markers remained intact;
5. a `brezeld` crash on host A while a command canary ran without failure on
   host B, followed by local controller recovery and marker verification;
6. a `brezel-node` crash on host B while a command canary ran without failure on
   host A, followed by local relay recovery and marker verification; and
7. fixture cleanup plus authenticated empty-project checks on both hosts after
   the drills.

Each paired benchmark case was accepted only after every configured sample
succeeded, every cleanup was confirmed, and both host measurement windows
overlapped. The generated evidence set is content-minimal and checksummed. A
sanitized evidence archive link will be added after publication.

## Performance figures

Both per-host 24-cell matrices completed and their local SHA-256 manifests were
verified. They are retained as a pre-optimization baseline rather than a
promotion result.

| Host | Cases | Attempts | Resource cleanup | Outcome |
| --- | ---: | ---: | ---: | --- |
| Host A | 22 / 24 | 1,040 / 1,056 | 1,584 / 1,584 | Failed |
| Host B | 24 / 24 | 1,056 / 1,056 | 1,584 / 1,584 | Passed |

Host A failed only the 16-way staggered and burst filesystem-restore cells: 10
of 16 and 6 of 16 attempts succeeded, respectively. The host still confirmed
cleanup for every expected resource. At the end of the run its engine build
directory retained approximately 57.95 GiB of snapshot-diff cache, and profiling
also exposed lifecycle operations whose cost grew with ledger history. The
subsequent cache-bound and row-scoped lifecycle changes are not qualified by
this report and require a new revision-bound run.

On the clean Host B, selected customer-observed service latency was:

| Scenario | Sequential p50 | Sequential p95 | 16-way burst p50 |
| --- | ---: | ---: | ---: |
| Create through verified instruction | 158 ms | 174 ms | 2,197 ms |
| Warm command | 13.6 ms | 16.4 ms | 90.5 ms |
| Resume through verified instruction | 342 ms | 1,690 ms | 4,751 ms |
| Filesystem checkpoint | 513 ms | 1,132 ms | 12,236 ms |
| Filesystem restore | 444 ms | 1,787 ms | 11,281 ms |
| Preview first byte | 5.2 ms | 6.1 ms | 19.9 ms |
| Warm preview | 2.6 ms | 3.1 ms | 11.2 ms |
| Workspace write and read | 80.7 ms | 86.7 ms | 775 ms |

These are host-local observations from one revision and configuration. They do
not include a neutral internet client, 100-way provider load, matched managed
service resources, or repeated daily samples. Brezel therefore makes no
portable or competitive performance claim from them. The three simultaneous
paired cases above remain qualification observations and do not replace an
independently operated provider benchmark.

## Exact claim scope

> Revision `121d7c6952c5bbc0010c365817ef540a1efbaca6` completed destructive
> qualification on each of two separately administered Ubuntu 24.04 x86-64 KVM
> hosts. Both hosts simultaneously passed conformance and cached-template burst
> observations for time to first verified instruction, filesystem restore, and
> workspace I/O, with confirmed cleanup. Controller and node-relay crash drills
> preserved the healthy peer and recovered the affected local fixture.

> This qualifies two independent single-host deployments. It does not qualify a
> cluster, shared control plane, multi-node scheduler, automatic placement or
> failover, cross-host restore, replicated storage, high availability, hostile
> shared multitenancy, image reproducibility, or portable performance.

The result also does not establish public-production availability or an SLA.
