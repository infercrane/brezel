# Brezel

**A secure computer for agents that starts quickly, remembers its work, and
stays out of the way.**

Brezel is an open-source, self-hosted sandbox runtime for long-running agents.
It provides one small API for isolated execution, files, durable workspaces,
standby and resume, checkpoints, authenticated previews, private model access,
and verifiable lifecycle records.

> **Developer preview.** The current release candidate is for a dedicated
> Ubuntu 24.04 x86-64 host with KVM and `/dev/net/tun`. It has not been
> qualified for hostile shared multitenancy or production availability. See
> [Status](docs/STATUS.md) for the exact boundary.

## The interface

```console
$ brezel new
sbx_01... running

$ brezel run sbx_01... /bin/sh -lc 'python --version && npm test'
Python 3.13.7
...

$ brezel stop sbx_01...
standby

$ brezel start sbx_01...
running
```

Common operations stay deliberately small:

```console
brezel doctor
brezel new [--template base] [--ttl 3600]
brezel run <sandbox> <command> [args...]
brezel put <sandbox> <guest-path> <local-file>
brezel get <sandbox> <guest-path> <local-file|->
brezel open <sandbox> <port>
brezel list
brezel inspect <sandbox>
brezel stop <sandbox>
brezel start <sandbox>
brezel delete <sandbox>
```

The public API is substrate-neutral. Application code never needs Firecracker,
network namespace, snapshot, or guest-credential knowledge.

## What works today

- Firecracker microVM sandboxes with no container fallback in release code
- streamed commands with deadlines, bounded output, and real exit status
- bounded file upload and download
- short-lived authenticated HTTP previews
- automatic standby and same-host resume
- durable single-writer workspaces that outlive sandboxes
- filesystem checkpoints and restore
- deny-by-default network policy and private model or tool connectors
- durable idempotency, cleanup intent, expiration, and lifecycle events
- Ed25519-signed, content-minimal execution receipts
- protected project-bound credentials, quotas, and overload admission
- a destructive Linux/KVM conformance suite and reproducible benchmark harness

Not yet available: arbitrary OCI image builds, interactive PTYs and SSH,
full-state fork, SDKs, multi-node scheduling, replicated storage, OIDC/RBAC,
GPU passthrough, or a hostile shared-multitenant security claim.

## Install on one host

The installer is intentionally fail-closed and destructive: it validates the
host, builds the pinned engine distribution, starts the services, and runs the
real microVM qualification suite twice. Use a disposable or dedicated Ubuntu
24.04 x86-64 machine, not a workstation that carries unrelated workloads.

```console
git clone https://github.com/infercrane/brezel.git
cd brezel

go build -trimpath -o bin/brezel ./cmd/brezel
bin/brezel doctor
./deploy/single-host/install.sh

export BREZEL_SERVICE_TOKEN_FILE="$PWD/.brezel/secrets/service.token"
bin/brezel new
```

The runtime requires Docker Engine, Compose v2, Buildx, Git, OpenSSL, KVM, and
`/dev/net/tun`. The installer fetches only digest- or commit-pinned inputs and
records the resulting distribution identities locally. Mirror and preloaded
artifact modes are documented in
[Artifact supply chain](docs/ARTIFACT-SUPPLY-CHAIN.md).

## Architecture

```text
CLI / API client
      |
      v
Brezel API  --> durable resource and cleanup state
      |
      v
node data path --> pinned Firecracker engine --> isolated guest
      |                                      |
      +--> policy, leases, receipts          +--> durable workspace
```

Brezel owns the public contract, lifecycle semantics, policy, data paths,
installer, qualification, and evidence. The first distribution uses audited,
Apache-licensed E2B Runtime components as its internal Firecracker engine. It
does not require E2B Cloud or an E2B API key. The engine is a replaceable
substrate rather than the product API.

The next trust-boundary milestone moves commands, files, and previews through a
node-local mTLS relay with one-operation capabilities and a protected generation
ledger, so live data no longer traverses the durable API process.

Read [Architecture](docs/ARCHITECTURE.md) and the
[Threat model](docs/THREAT_MODEL.md) before operating a host.

## Verify the repository

macOS can run all unit, contract, race, and security-negative tests. It cannot
qualify the Firecracker host boundary.

```console
make check
```

On a supported Linux/KVM host:

```console
make qualify-single-host
make benchmark-single-host
```

The benchmark records raw attempts and p50, p95, and p99 for verified first
instruction, warm command, pause and resume, checkpoint and restore, preview
first byte, and workspace I/O. Results are host-, revision-, load-, and
cache-specific. Historical evidence and current claim limits are in
[Benchmarking](docs/BENCHMARKING.md) and
[Qualification](docs/QUALIFICATION-2026-09-13.md).

## Documentation

- [Status](docs/STATUS.md)
- [API](docs/API.md)
- [Architecture](docs/ARCHITECTURE.md)
- [Threat model](docs/THREAT_MODEL.md)
- [Roadmap](docs/ROADMAP.md)
- [Benchmarking](docs/BENCHMARKING.md)
- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)

## License

Apache License 2.0. See [LICENSE](LICENSE) and
[third-party notices](third_party/NOTICE.md).
