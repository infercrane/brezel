# Pinned engine patch notice

The file under `patches/` applies to E2B Runtime commit
`767ceb4b2ec0e598f512767c8da9e5e6618da368`, licensed under Apache License 2.0.

The bundled installer builds the patched API locally. The first patch reads
the volume-token signing key from a protected file and changes deletion
ordering so the API removes data synchronously before deleting its durable
database record. This keeps long-lived key material out of ordinary container
environment variables and lets a cleanup failure remain visible and
retryable. The second patch pins the API builder and runtime base images by
manifest digest, removes a moving package-upgrade step, and replaces the
build-time `make` download with a direct Go build. The third patch delays a
successful sandbox-delete acknowledgement until Firecracker, NBD, network,
and lazy-memory resources have been reclaimed. The fourth patch makes the
recoverable snapshot-diff cache retention configurable, adds physical-byte
and local disk high-water eviction, and emits content-free cache pressure
metrics. Cache pressure never evicts an active or unsealed diff.

Upstream source: <https://github.com/e2b-dev/runtime>
