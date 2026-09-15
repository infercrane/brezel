# ADR 0011: Use crash-recoverable single-use warm capacity

- Status: accepted; implemented but not yet qualified on Linux/KVM
- Date: 2026-09-16

## Context

Cached Firecracker templates still pay allocation, network-slot assignment,
root attachment, restore, guest boot, and readiness work for every create. Under
a burst, the host must bound those concurrent operations to preserve success,
which turns a large arrival wave into several start cohorts. Raising admission
alone moves latency into CPU, storage, and proxy contention and previously
caused command-stream failures.

Reusing a sandbox after customer execution would be faster but would make
cross-customer erasure depend on an incomplete cleanup procedure. An in-memory
pool alone would also lose ownership decisions across controller restarts.

## Decision

Optionally pre-create a fixed number of clean Firecracker sandboxes for one
exact immutable template and one exact network policy. Record every reservation,
backend identity, claim, and customer binding in a separate private SQLite WAL
ledger with full synchronous durability and an exclusive process lock.

A slot can be claimed exactly once. Claiming stores the customer binding before
publication, verifies the guest is running, and resets the engine's hard timeout
to the remaining duration of the original customer deadline. Retries use that
stored deadline and cannot extend it. Deletion destroys the backend sandbox and
creates a new clean slot; customer state is never scrubbed and returned to the
pool.

Strict mode treats the pool as the complete physical sandbox budget. It rejects
requests that need another template, network policy, injected environment, or
workspace before touching the backend, and it rejects an arrival when no clean
slot exists. The capacity contract requires strict pool size to equal the
advertised active-sandbox ceiling. Non-strict mode remains available only when
the capacity contract reserves both the active ceiling and the idle pool.

## Consequences

- Burst create latency can remove Firecracker construction from the request
  path without weakening customer-state isolation.
- Priming makes service startup proportional to configured clean capacity; the
  API fails closed until priming and recovery finish.
- Pool ledger durability adds a small serialized claim cost. It is an explicit
  reliability tradeoff and must be measured on the target NVMe device.
- One pool accelerates one exact template and network shape. Multiple classes
  require an explicit capacity scheduler rather than implicit fallback.
- A warm-pool benchmark is a separate evidence class. It must report priming,
  occupancy, claim success, replacement cleanup, and the same end-to-end
  command-ready boundary as cold or cached-template tests.
- This implementation is not a qualified performance claim until the complete
  Linux/KVM conformance, failure, cleanup, and repeated benchmark gates pass.
