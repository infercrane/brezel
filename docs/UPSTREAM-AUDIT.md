# Upstream audit

## E2B Runtime

- Reviewed: 2026-09-14
- Repository: <https://github.com/e2b-dev/runtime>
- Commit: `767ceb4b2ec0e598f512767c8da9e5e6618da368`
- Declared license: Apache-2.0 at repository root
- Language/toolchain declared in README: Go 1.26

### Confirmed from the checked-out source and documentation

- control-plane API, per-node orchestrator, client proxy, dashboard API, guest
  `envd`, template manager, and shared packages;
- Firecracker, KVM, cgroups, per-sandbox network namespaces, tap devices, NAT,
  nftables egress rules, and a TCP firewall/proxy;
- pre-booted templates, lazy `userfaultfd` memory restore, copy-on-write block
  storage, pause/resume, checkpoint, fork, and auto-resume through the client
  proxy;
- process, PTY, filesystem, watcher, and port functions through `envd`;
- persistent volume API and node implementation;
- PostgreSQL durable metadata, Redis live routing, object storage for templates
  and snapshots, ClickHouse, and OpenTelemetry;
- Compose-based E2B Embed files, a GCP Terraform profile, and an evaluation
  Kubernetes manifest described by the repository; and
- public API fields for per-sandbox network policy and workload identity.

The internal adapter requests secured guest access and requires create, inspect,
resume, and recovery responses to confirm a non-empty per-sandbox guest token.
The token is fetched transiently and never written into project state. Process
streaming and file transfer use the authenticated guest protocol. Application
HTTP previews require a distinct traffic token, which is injected only on the
internal hop and never returned through the product API.

### Resume fast-path audit

The pinned source contains the following mechanisms. These are implementation
facts, not portable performance claims:

| Mechanism | Pinned implementation | Release evidence |
| --- | --- | --- |
| Snapshot restore | `fc/client.go` loads a Firecracker snapshot with a UFFD memory backend, keeps `ResumeVM` false, waits for the UFFD server, then resumes the VM | Installer checks the exact source contract; live qualification requires a running UFFD-backed sandbox |
| Lazy memory paging | `uffd/uffd.go` receives Firecracker's descriptor and serves page faults from the snapshot memory device | Live qualification requires both the sandbox UFFD socket and an orchestrator-owned `anon_inode:[userfaultfd]` descriptor |
| Startup prefetch | The template builder always runs the optimize phase, which intersects two startup traces and persists the result in template metadata | Upstream treats collection and upload failures as warnings. The release qualification therefore fails if local metadata has no non-empty memory-prefetch map |
| Copy-on-write root filesystem | Normal sandbox resume constructs `rootfs.NewNBDProvider` over a per-sandbox `rootfs-<id>-<nonce>.cow` file | Live qualification requires a non-empty COW file for the exact engine sandbox being tested |
| Local template cache | The single-host profile uses local template storage and the orchestrator cache rooted at `/orchestrator/template` | Live qualification requires a complete local snapshot artifact set and populated local cache metadata |
| Pooled devices and networking | Embed config pins an NBD pool of 64. The audited network implementation has a 32-slot new pool and 100-slot reused pool | Live qualification requires a configurable minimum of ready `ns-*` namespaces; the release default is 16 so active and transient slots do not make the gate flaky |

The source contract is revision-bound in
`deploy/single-host/engine-capabilities.sh` and runs after the pinned patch is
applied, before an image can be built. The same script runs inside the host
namespaces during `qualify.sh`. A source match cannot substitute for the live
gate, and the live gate cannot substitute for the latency benchmark.

The release profile intentionally keeps `NETWORK_VERSION=1` and the default
build-time (`init`) prefetch source. The reviewed tree also contains a version-2
network path and pause/resume last-cycle prefetch flags, but neither is enabled:
they need an isolated correctness qualification and measured A/B result first.
`vm.unprivileged_userfaultfd=1` is not required by this profile because the
host-namespace orchestrator runs as root; enabling it would broaden host attack
surface without helping the release path.

### Important gaps or caveats

- E2B's README describes Embed as an evaluation package, not a production
  deployment pattern. Production self-host operations remain project work.
- The architecture document refers to `orchestrator-ee` for resolving customer
  secrets into egress headers. That package was not present in the reviewed
  `packages/` directory. Metadata APIs alone do not prove an end-to-end open-
  source secret-injection path.
- No durable batch/job coordinator was found by repository search for job or
  batch APIs. Revalidate before implementation because upstream changes quickly.
- The reviewed Embed pins E2B-hosted container images and downloads kernel,
  Firecracker, BusyBox, and `envd` artifacts from E2B's public artifact storage.
  A private or air-gapped distribution needs its own verified and signed mirror.
- The reviewed Embed disables volume-token signing and has no protected-file
  input for its signing key. The product installer applies a pinned patch that
  adds a private-file input and refuses group- or world-readable key files.
- The reviewed volume delete handler removes its database row before deleting
  data asynchronously. The same pinned patch makes data removal synchronous and
  preserves the database row on cleanup failure so product reconciliation can
  retry it.
- The API constructs a PostHog client and enqueues lifecycle analytics. With no
  API key the reviewed implementation silences client logs, but it does not
  select an explicit no-op implementation. Treat outbound behavior as unproven
  until packet-tested and patch a real telemetry-off mode before private release.
- A root Apache-2.0 license is encouraging but does not replace a generated
  dependency, image, and trademark review.
- Public README feature statements are not project conformance results. Every
  lifecycle and security claim still needs a test on the pinned release.
- The Compose service images are version-tag pinned rather than digest pinned.
  The artifact fetcher verifies kernel, Firecracker, BusyBox, orchestrator, and
  `envd` checksums, but a fully reproducible distribution still needs image
  digest resolution and verification.

### Immediate recommendation

Use this exact revision for the single-host evaluation profile. The product
installer fetches it by immutable commit and fails if the checkout differs. Do
not fork it yet. Run the product conformance suite on the exact Linux/KVM host,
document the remaining secret, job, and production-operation gaps, and open
upstream issues before deciding where new engine code lives.
The commercialization and private-deployment requirements are recorded in
[`PRIVACY-COMMERCIALIZATION.md`](PRIVACY-COMMERCIALIZATION.md).

## JuiceFS

- Repository: <https://github.com/juicedata/juicefs>
- Declared license observed on public repository: Apache-2.0
- Intended evaluation: later `Drive` substrate for multi-client shared POSIX
  workspaces

No source checkout or correctness qualification was completed in this audit.
Do not include JuiceFS in the MVP dependency graph until volume lifecycle is
stable and the shared-drive test plan is funded.
