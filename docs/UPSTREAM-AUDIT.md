# Upstream audit

## E2B Runtime

- Reviewed: 2026-09-13
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
- The API constructs a PostHog client and enqueues lifecycle analytics. With no
  API key the reviewed implementation silences client logs, but it does not
  select an explicit no-op implementation. Treat outbound behavior as unproven
  until packet-tested and patch a real telemetry-off mode before private release.
- A root Apache-2.0 license is encouraging but does not replace a generated
  dependency, image, and trademark review.
- Public README feature statements are not project conformance results. Every
  lifecycle and security claim still needs a test on the pinned release.

### Immediate recommendation

Use this exact revision only for an M0 evaluation. Do not vendor or fork it yet.
Build a thin conformance harness against its public API, document the missing
secret and job paths, and open upstream issues before deciding where code lives.
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
