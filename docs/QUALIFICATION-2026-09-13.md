# Single-host qualification: 2026-09-13

This report records one reproducible private-release gate. It is not a public
cloud, hostile-multitenant, availability, or cross-vendor benchmark claim.

## Deployment under test

- target: Scaleway Elastic Metal `EM-A116X-SSD`, `fr-par-1`;
- host: Intel Xeon E3-1231 v3 at 3.40 GHz, 4 cores / 8 threads, 31 GiB RAM;
- storage: two approximately 1 TB SSDs in Linux RAID1;
- operating system: Ubuntu 24.04.3 LTS, x86-64;
- isolation: KVM, Firecracker, `/dev/net/tun`;
- engine revision: `767ceb4b2ec0e598f512767c8da9e5e6618da368` plus the
  SHA-256-pinned distribution patch in `deploy/single-host/engine.lock`.

The product API and engine management endpoints were loopback-only. UFW denied
unsolicited public ingress and allowed SSH plus the private Firecracker guest
proxy/egress path.

## Contract and recovery results

Two complete qualification sequences passed. Each sequence ran 29 destructive
checks, held a real sandbox and durable workspace across a controller restart,
and repeated all 29 checks after restart. The checks covered project isolation,
idempotency input binding, command streaming, file transfer, authenticated HTTP
preview, pause/resume, workspace persistence, filesystem checkpoint and restore,
physical cleanup, ordered events, and signed receipts.

After the second sequence and after the load matrix:

- physical template directories remained at 3 (the immutable base layers);
- live engine environments remained at 1 (the base environment);
- product checkpoints remained at 0; and
- nonterminal product sandboxes remained at 0.

The content-free JSON reports are retained locally under the ignored
`.brezel/qualification/clean-host/` directory with SHA-256 checksums.

## Real microVM benchmark

Every measured run completed the full destructive conformance path. Times are
end-to-end operation wall clock from the product client on the same host.
The first matrix was captured before the post-reboot liveness hardening and is
retained as the initial clean-host baseline.

| Concurrency | Runs | Success | Create p50 / p95 / p99 | Resume p50 / p95 / p99 | Checkpoint p50 / p95 / p99 |
| --- | ---: | ---: | --- | --- | --- |
| 1 | 30 | 30 | 371 / 667 / 901 ms | 228 / 537 / 612 ms | 645 / 1,227 / 2,828 ms |
| 2 | 20 | 20 | 507 / 923 / 1,013 ms | 338 / 948 / 2,675 ms | 815 / 2,795 / 3,351 ms |
| 4 | 24 | 24 | 1,014 / 1,720 / 1,784 ms | 962 / 2,274 / 3,222 ms | 1,145 / 3,434 / 3,718 ms |

All 74 benchmark runs succeeded. Measured complete-run throughput was 13.53,
10.08, and 8.94 runs per minute at concurrency 1, 2, and 4 respectively. This
older four-core host lost total throughput as lifecycle concurrency increased.
A private deployment on this hardware should start with one lifecycle-heavy
operation at a time and remeasure on its actual host before raising that limit.

At concurrency one, command streaming was 144 / 241 / 285 ms p50/p95/p99,
authenticated preview first request was 23 / 47 / 54 ms, and warm preview was
24 / 41 / 43 ms.

### Post-reboot hardening samples

The benchmark report schema was then extended with whole-suite latency and
successful runs per wall-clock minute. After making every persisted `running`
observation prove guest liveness, an uncached 20-run sample completed 20/20 at
7.51 runs/minute, with 516 ms create p50 and 465 ms resume p50. A one-second
process-local liveness lease was added to coalesce duplicate probes without
surviving a controller restart. The final 12-run sample completed 12/12 at 6.96
runs/minute:

| Measurement | p50 | p95 | p99 |
| --- | ---: | ---: | ---: |
| Complete conformance run | 8,326 ms | 11,249 ms | 11,249 ms |
| Cached-template create | 599 ms | 1,162 ms | 1,162 ms |
| Same-node full-state resume | 507 ms | 1,210 ms | 1,210 ms |
| Filesystem checkpoint | 843 ms | 3,258 ms | 3,258 ms |
| Command stream | 355 ms | 750 ms | 750 ms |
| Authenticated preview, first request | 81 ms | 104 ms | 104 ms |
| Authenticated preview, warm request | 66 ms | 110 ms | 110 ms |

The later sample was slower across creation, guest commands, checkpointing, and
preview requests, not only at the liveness boundary. The host also reported a
load average around 2.5 after sustained destructive runs. Therefore this report
does not claim that the short liveness lease improved end-to-end performance.
It is retained as a safety optimization with a bounded one-second staleness
window and needs an A/B run on fresh identical hosts before a performance claim.

## Competitive context

These numbers are not directly comparable with hosted vendor claims because
hardware, image, region, warm-pool policy, lifecycle semantics, and measurement
points differ. [Blaxel documents sub-25 ms cold starts and standby
resume](https://docs.blaxel.ai/Sandboxes/Overview). [Modal documents filesystem,
directory, and alpha memory snapshots](https://modal.com/docs/guide/sandbox-snapshots),
while [Daytona documents pre-created running warm
pools](https://www.daytona.io/docs/en/warm-pools/). [E2B describes a Firecracker
microVM per session and snapshot-based boot](https://e2b.dev/), but its current
product page does not define a directly comparable public latency test.

This project currently provides a self-hosted full-state standby path,
filesystem checkpoints, and an independently durable workspace. It does not
yet provide a qualified warm pool, fleet scheduler, or cross-host memory
snapshot restore. Those architecture changes, plus faster modern hardware, are
the credible path toward hosted-provider startup numbers; changing benchmark
definitions is not.

## Full host-reboot result

The provider's first normal reboot path was unavailable even though its API
reported the machine ready. Rescue boot succeeded, both RAID members were
healthy, and a later normal boot returned the installed system. The engine did
not restore the active microVM process. Its persisted detail row still said
`running`, while the guest proxy returned 502 and the engine connect route
confirmed that the live sandbox was absent.

The product adapter was hardened to require a non-mutating authenticated guest
health probe before translating engine `running` into product `running`. After
the change, controller startup reconciled the sandbox to `unknown` instead of
inventing availability. Deleting the lost sandbox, creating a replacement with
the same workspace, and reading the pre-reboot marker proved that the durable
workspace survived; both replacement sandbox and workspace then deleted
cleanly.

This passes honest state detection and local workspace recovery, not transparent
process continuity or host availability. The release label remains **private
single-host release candidate**. Before an SLA, repeat host-loss on reliable
hardware and add disk-full, engine-restart, backup/restore, upgrade rollback,
and longer soak tests.
