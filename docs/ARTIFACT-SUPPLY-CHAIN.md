# Artifact supply chain

The single-host distribution owns a verifiable installation manifest around
the pinned microVM engine. This is a release-integrity boundary, not a claim
that the installer is fully air-gapped or that every build is bit-for-bit
reproducible yet.

## Release inputs

The installer accepts only the following engine inputs:

- the exact upstream Git commit in `deploy/single-host/engine.lock`;
- the two local patches whose SHA-256 digests are in that lock;
- the upstream compose, environment, and host-artifact fetcher bytes whose
  independent SHA-256 digests are in that lock;
- the exact linux/amd64 OCI manifests in
  `deploy/single-host/engine.images.lock`; and
- the exact orchestrator, guest agent, Firecracker, kernel, and BusyBox bytes
  in `deploy/single-host/engine.artifacts.lock`.

The API build patch replaces mutable builder and runtime base tags, removes
the moving Alpine upgrade step, and builds the API without downloading `make`.
The product image also uses exact Dockerfile frontend, builder, and runtime
manifests. Go module downloads remain checksum-bound by `go.sum`, but they are
still network dependencies unless the operator supplies a module proxy or
pre-populated build cache.

`artifact-supply-chain.sh` verifies the source files before applying either
patch. It then pulls, or requires preloaded copies of, every engine image by
its exact manifest digest. Compose has `pull_policy: never`; it cannot silently
replace the checked images while starting the engine. After the engine fetcher
finishes, the installer independently hashes every privileged host artifact
against the product lock rather than trusting only checksums embedded inside
the upstream tools image.

On success, `.brezel/distribution.manifest` records the engine revision, lock
digests, exact image references, and exact host-artifact identities. It
contains no credentials and is mode `0600` because it belongs with operator
qualification evidence.

## Operator mirrors

An operator can mirror source without changing its identity:

```bash
BREZEL_ENGINE_SOURCE_REPOSITORY=/srv/git/e2b-runtime.git \
  ./deploy/single-host/install.sh
```

The mirror must contain the locked commit. The installer still checks the
commit ID and the independently locked compose, environment, and fetcher
digests.

Host artifacts can come from an operator HTTPS mirror:

```bash
BREZEL_ENGINE_ARTIFACT_BASE_URL=https://artifacts.example.internal/e2b \
  ./deploy/single-host/install.sh
```

For a directory already present on the host, use its path through the
upstream fetcher's host-root mount:

```bash
BREZEL_ENGINE_ARTIFACT_BASE_URL=file:///host/srv/runtime-artifacts/e2b \
  ./deploy/single-host/install.sh
```

The mirror must retain the upstream object layout. Mirror trust does not
replace content verification: every installed object must match the product
lock.

OCI images may be imported into Docker from an operator-controlled archive or
registry before installation. The preloaded mode forbids registry pulls and
fails if any exact digest is absent:

```bash
BREZEL_ENGINE_IMAGE_MODE=preloaded \
BREZEL_ENGINE_SOURCE_REPOSITORY=/srv/git/e2b-runtime.git \
BREZEL_ENGINE_ARTIFACT_BASE_URL=file:///host/srv/runtime-artifacts/e2b \
  ./deploy/single-host/install.sh
```

## Remaining work before an air-gapped claim

Preloaded mode is a building block, not an air-gapped qualification. A public
air-gapped claim still requires:

- an owned, signed release bundle containing every OCI image, source object,
  host artifact, module, base-template input, SBOM, and license notice;
- signature and provenance verification rooted in an operator-configured
  trust policy, not only SHA-256 lock files shipped beside the payload;
- removal or mirroring of every template-build package and image dependency;
- installation and upgrade tests with network access physically disabled;
- rollback, vulnerability-policy, key-rotation, and mirror-compromise drills;
  and
- a documented process for publishing patched source and corresponding
  binaries in compliance with all component licenses.

Until those gates pass, describe this as a digest-locked, mirrorable
single-host distribution.
