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
metrics. Cache pressure never evicts an active or unsealed diff. The fifth
patch makes the NFSv3 server's advertised `FILE_SYNC` write stability true:
data and inode changes are synced before success, while create, rename,
remove, and directory operations sync the affected parent directories before
acknowledgement. A sync failure is returned to the guest instead of becoming
a false durability promise. The sixth patch bounds simultaneous local sandbox
starts and makes resource-exhaustion retries bounded, cancellation-aware, and
observable instead of allowing an unbounded admission loop. The seventh patch
makes host resource pools and the immutable base-template CPU, memory, and
writable-root shape explicit operator inputs with validated bounds. The eighth
patch resolves live process tags across the complete guest process table so a
recovery lookup cannot silently miss a non-first process. The ninth patch adds
a bounded, generation-bound process-output journal so an interrupted command
stream can resume from an exact cursor without rerunning customer code; missing,
evicted, or stale-generation output fails closed.
The tenth patch disables guest-visible SMT by default and adds an explicit
single-sandbox performance profile. A host-wide lease prevents a second
Firecracker process from sharing the profile. Each guest vCPU is pinned to a
different host-visible core on one NUMA node, sibling threads of those cores
remain unused by Firecracker, and helper threads use separate cores. It fails
before guest execution when the host cannot enforce the requested topology.
The thirteenth patch qualifies that opt-in profile only when a pre-created
cgroup v2 isolated partition exactly matches an explicit CPU and NUMA contract.
It requires atomic Firecracker launch into a per-sandbox child cgroup, rejects
partial physical-core sibling sets, and continuously verifies cgroup membership
and thread affinity. The default remains disabled, and any missing or drifting
isolation contract stops the sandbox rather than silently sharing CPUs.
The fifteenth patch validates and parameterizes the local base-template name
and reports the exact template-and-build reference emitted by each completed
build. This lets operators keep benchmark variants separate and bind Brezel
environment revisions to an immutable build instead of a moving alias.

Upstream source: <https://github.com/e2b-dev/runtime>
