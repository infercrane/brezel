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
build-time `make` download with a direct Go build.

Upstream source: <https://github.com/e2b-dev/runtime>
