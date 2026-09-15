# Benchmarking the single-host runtime

This benchmark measures the customer-observed runtime API boundary on one named
deployment. It does not turn a result from one host, image, cache state, or load
shape into a universal performance claim.

## Measured boundaries

The native `brezel-bench` client verifies a random nonce rather than treating
an accepted create request as usable:

| Scenario | Timed boundary | Setup outside the timed boundary |
| --- | --- | --- |
| `tti` | sandbox create request through the first successful command | environment revision resolution |
| `warm-exec` | command request through verified command completion | sandbox create and one priming command |
| `resume` | resume request through the first successful command | sandbox create, priming command, and pause |
| `filesystem-checkpoint` | filesystem checkpoint request through a response binding its kind and source | sandbox create and generated marker write |
| `filesystem-restore` | create from checkpoint through a command reading the exact checkpointed marker | source sandbox create, marker write, and filesystem checkpoint |
| `preview-first-byte` | opaque lease request through the first byte from its authenticated preview path | sandbox create and nonce-serving HTTP server start |
| `preview-warm` | warm preview request through its first byte | sandbox/server start, lease creation, and one exact-body preview request |
| `workspace-io` | fixed-size generated file write followed by an exact-byte read through the public API | workspace create, attachment during sandbox create, and a priming command |

Every sample records the phases that apply, including create, command, file,
checkpoint, restore, port lease, preview first-byte, full preview validation,
and cleanup durations. Preview latency stops when the first byte arrives, but a
sample is successful only after the complete bounded response exactly matches
the generated nonce. Workspace I/O uses generated benchmark bytes and records
both byte counts; it never reads or reports customer content.

Cleanup is excluded from user-visible latency but the report counts cleanup not
required, attempted, confirmed, and failed separately. Cleanup must be confirmed
for every sandbox, checkpoint, and workspace that may have been created. Schema
version 3 includes one content-minimal cleanup record per resource without
publishing its runtime identity. An ambiguous create response with no resource
identity remains an explicit cleanup failure.

Schema version 3 reports successful latency from scheduled arrival through the
verified result. This includes client-side admission delay and avoids coordinated
omission under load. Service latency begins when the request actually starts and
is shown separately. Percentiles use nearest rank without trimming. Failed
observations have their own latency distribution; a failure with no observable
duration increments `latency_censored` instead of disappearing. Concurrent cases
also report the measurement window, time to first successful result, and observed
successful completions per second.

## Load shapes

The single-host matrix runs every scenario under:

- `sequential`: one operation at a time;
- `staggered`: arrivals every 200 milliseconds by default; and
- `burst`: all measured operations become eligible together.

`staggered` is a finite open-loop test: arrival times are fixed in advance and
do not wait for earlier completions. Waiting for the client concurrency permit
is included in scheduled-arrival latency, so overload cannot be hidden as
coordinated omission. This is not a duration-based soak or a capacity search;
the native harness deliberately does not expose a `sustained` label. Add one
only with a monotonic fixed-rate producer independent of completions, bounded
client admission, complete attempted-arrival retention, and explicit overload
and censoring records. Until then, use a separate host load tool for sustained
experiments and retain its offered-load schedule and every failed or censored
observation.

Defaults are 100 sequential attempts and 16 staggered and burst attempts. The
concurrent defaults exercise the distribution's complete 32-active-sandbox
admission boundary: `filesystem-restore` can hold a source and restored
sandbox per attempt. The installer reconciles the embedded engine's tenant
gate to that same value, and both qualification and benchmarking verify the
match before creating a VM. The runner also rejects a concurrency whose
two-sandbox restore peak exceeds either the host or project limit.
`workspace-io` holds one sandbox and one workspace
per attempt. The benchmark project must be dedicated and have no non-terminal
sandboxes or workspaces before a matrix. The runner verifies that condition
through authenticated, read-only API calls before it creates an environment or
VM and retains content-minimal counts in `project-preflight.json`. Operators
must lower concurrent runs to fit stricter configured limits. A
published comparison must use
identical counts, arrival intervals, image, resources, payload size, preview
port, cache declaration, and success definition for every release under
comparison.

The benchmark does not clear caches. `cold`, `cached-template`, `warm-pool`, and
`unknown` are explicit operator declarations. A cold-cache report must document
the external reset procedure; changing the label alone does not make a run cold.

Snapshot diffs under `/orchestrator/build` are a recoverable performance cache,
not a durable workspace or template store. The single-host distribution defaults
to a 4 hour TTL, a 32 GiB physical-allocation high water, and a 70 percent local
filesystem high water. Operators may set these before installation with
`BREZEL_BUILD_CACHE_TTL`, `BREZEL_BUILD_CACHE_MAX_BYTES`, and
`BREZEL_BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT`. The supported installer
range is 1 to 168 whole hours, at least 1 GiB, and 50 to 90 percent respectively.

The byte high water is deliberately soft. Active and unsealed diffs cannot be
evicted without violating sandbox correctness, one large entry can cross the
limit, and deletion is delayed for 60 seconds so current readers can finish.
Physical accounting uses allocated filesystem blocks, making it sparse-file
aware; shared reflink extents may be conservatively counted more than once.
Cache eviction therefore protects capacity without promising a hard disk quota.

## Run the matrix

First complete `make qualify-single-host` on an otherwise dedicated Ubuntu
24.04 x86-64 KVM host. Build or copy the exact `brezel-bench` binary under
test to `bin/brezel-bench`. Do not benchmark from a developer build whose
revision cannot be reproduced.

The runner requires a second explicit destructive-operation acknowledgement:

```bash
BREZEL_BENCH_EXECUTE=true \
BREZEL_BENCH_TARGET=scaleway-em-a116x-ssd-par1-01 \
BREZEL_BENCH_RUNTIME_REVISION=$(git rev-parse HEAD) \
BREZEL_BENCH_CACHE_STATE=cached-template \
./deploy/single-host/benchmark.sh
```

Useful controlled overrides are:

```text
BREZEL_BENCH_BINARY
BREZEL_BENCH_OUTPUT_DIR
BREZEL_BENCH_BASE_URL
BREZEL_BENCH_SEQUENTIAL_RUNS
BREZEL_BENCH_STAGGERED_RUNS
BREZEL_BENCH_BURST_RUNS
BREZEL_BENCH_STAGGER_INTERVAL
BREZEL_BENCH_ATTEMPT_TIMEOUT
BREZEL_BENCH_CLEANUP_TIMEOUT
BREZEL_BENCH_CASE_TIMEOUT
BREZEL_BENCH_COOLDOWN_SECONDS
BREZEL_BENCH_IO_BYTES
BREZEL_BENCH_PREVIEW_PORT
```

When invoking `brezel-bench` directly, `-io-bytes` selects the generated
workspace payload size (1 MiB by default) and `-preview-port` selects the guest
HTTP port (8080 by default). The benchmark image must provide `/bin/sh` and
BusyBox `httpd` for preview scenarios. Missing tooling is a setup failure, not a
latency sample.

The runner rejects tracked or untracked changes by default. For pre-commit
tuning only, `BREZEL_BENCH_ALLOW_DIRTY=true` permits a run and records the
dirty state in both host snapshots. Such evidence is useful for comparison
during development but is not publishable.

Do not interrupt a destructive cell unless necessary. If a run is interrupted,
reconcile and confirm cleanup before starting another matrix.

## Evidence layout

Each invocation creates a new timestamped directory under
`.brezel/benchmarks/` by default:

```text
TARGET-TIMESTAMP/
  STATUS
  project-preflight.json
  project-postflight.json
  engine-cache-before.json
  engine-cache-after.json
  host-before.json
  host-after.json
  cases.ndjson
  summary.json
  SHA256SUMS
  raw/
    tti-sequential.json
    ...
  stderr/
    tti-sequential.log
    ...
```

The project preflight records only the project name, non-terminal resource
counts, request IDs, timestamp, and pass/fail result. It omits resource IDs and
fails the matrix before environment creation when either count is nonzero.
The postflight repeats the same authenticated inventory after every matrix cell
and its cleanup have completed. A missing, malformed, or non-empty postflight
fails the complete matrix even if every individual attempt reported cleanup.

The engine-cache records capture the effective TTL and high waters plus
content-free physical allocation, file count, and containing-filesystem totals
before and after the matrix. A missing, malformed, or out-of-policy observation
fails the benchmark. They do not contain paths below the cache root, filenames,
sandbox identifiers, or file contents.

The patched orchestrator also publishes content-free OpenTelemetry instruments
for allocated and effective cache bytes, available filesystem bytes, pressure
evictions by reason, and allocation-observation failures under the
`orchestrator.build.cache.*` namespace. Alert on repeated observation failures
or on pressure with no evictable entry; the latter means active or unsealed
work is holding the recoverable cache above its high water.

The host records intentionally omit hostname, IP addresses, machine serials,
environment variables, tokens, command output, and customer content. They bind
the evidence to OS, kernel, CPU topology, memory, filesystem, KVM/TUN
availability, Docker runtime, repository revision, runtime image, benchmark
binary digest, engine lock digest, and before/after load.

The script runs all eight scenarios under all three load shapes for 24 cells.
It continues after an ordinary measured failure so the matrix does not
hide an unfavorable cell. It stops immediately after unconfirmed cleanup or an
invalid report because creating more resources would amplify an unknown leak.
`STATUS` and `summary.json.outcome` are `passed` only when all 24 cells and all
cleanup operations pass. Raw JSON and stderr are retained for failed runs.

## Two independent-host qualification

The [2026-09-15 qualification](QUALIFICATION-2026-09-15.md) ran two separately
administered single-host deployments at revision
`121d7c6952c5bbc0010c365817ef540a1efbaca6`. After simultaneous conformance, the
coordinator launched cached-template `tti`, `filesystem-restore`, and
`workspace-io` burst cases on both hosts. A paired case passed only when every
requested sample and every resource cleanup succeeded on both hosts and the
measurement windows overlapped.

Those three paired cases are simultaneous independent-host observations. The
two complete 24-cell per-host matrices later finished: Host B passed every cell
and Host A failed the staggered and burst filesystem-restore cells. The dated
report preserves the exact counts, selected latency observations, and diagnosis.
Neither the paired cases nor the matrices replace the repeated-matrix promotion
gate below or an independently operated provider benchmark. They do not measure
a cluster, scheduling, automatic placement or failover, cross-host restore,
replicated storage, high availability, or hostile shared multitenancy. Brezel
makes no portable or competitive performance claim from this qualification.

## Before and after tuning

Use one dedicated host and an `A, B, B, A` run order when evaluating a runtime
change. Build each revision from a clean checkout, run conformance first, keep
the engine revision, template, resource limits, cache declaration, load shape,
and host settings identical, and retain all four matrices. Restarting or
clearing caches is part of the protocol only when it is performed identically
for every run and documented.

Compare each matrix cell independently. A change is acceptable only when it
preserves full success and cleanup confirmation and improves its declared
target without an unexplained p95, p99, throughput, or failure-latency
regression. Never average cold, cached-template, and warm-pool results into one
startup number.

### Machine-checkable promotion gate

`brezel-bench-compare` turns the protocol above into a fail-closed release
decision. It requires at least two complete baseline matrices and two complete
candidate matrices. Replicates must have identical revisions within each set;
both sets must have the same named host, evidence class, cache state, template,
project, case set, and matrix configuration. The gate also resolves each
matrix's `host-before.json` and compares its captured OS, kernel, architecture,
virtualization, CPU topology, memory size, KVM/TUN availability, root
filesystem, Docker configuration, and engine-lock digest. Every input must come
from a clean checkout whose recorded repository revision matches the matrix,
and replicates must use the same benchmark binary digest. Every attempt and
cleanup must have passed, with no censored latency.

Declare the metric the change is intended to improve. The gate then checks that
target and independently protects scheduled and service p95/p99 in every cell,
plus observed throughput in concurrent cells:

```sh
bin/brezel-bench-compare \
  -baseline evidence/A1/summary.json \
  -baseline evidence/A2/summary.json \
  -candidate evidence/B1/summary.json \
  -candidate evidence/B2/summary.json \
  -target tti-sequential:scheduled_p50_ms \
  -required-improvement 5 \
  -max-latency-regression 5 \
  -max-throughput-loss 5 \
  > promotion.json
```

The JSON decision is emitted even when the candidate is rejected, and rejection
returns a nonzero exit status. Supported target metrics are
`scheduled_p50_ms`, `scheduled_p95_ms`, `scheduled_p99_ms`, `service_p50_ms`,
`service_p95_ms`, `service_p99_ms`, and `throughput_per_second`. The default
two-millisecond absolute tolerance prevents noise in very small latencies from
dominating the percent guard; it does not relax the declared target
improvement. This deterministic gate is conservative release automation, not a
substitute for publishing raw samples or confidence intervals.

### ComputeSDK compatibility

The dependency-free adapter under `benchmarks/computesdk` implements the
`createCompute().sandbox.create()`, `runCommand()`, and `destroy()` shape used by
the public ComputeSDK sandbox benchmark. It keeps Brezel authentication in a
private token file and fails closed on malformed streams, unconfirmed command
outcomes, and unconfirmed cleanup.

After qualifying a stable HTTPS deployment, run the one-cycle integration
smoke test before wiring the provider entry into a pinned checkout of the public
suite:

```sh
BREZEL_API_URL=https://sandbox.example.internal \
BREZEL_SERVICE_TOKEN_FILE=/run/secrets/brezel-service-token \
BREZEL_PROJECT_ID=brezel-benchmark \
BREZEL_ENVIRONMENT_REVISION=envr_... \
node benchmarks/computesdk/qualified-smoke.mjs
```

Guest internet is disabled when `BREZEL_ALLOW_INTERNET` is omitted or set to
the exact value `false`. The DAX workload downloads packages and source, so set
`BREZEL_ALLOW_INTERNET=true` only for that qualified run. Any other value,
including different capitalization or surrounding whitespace, is rejected
before sandbox creation.

This adapter enables comparable external measurement; it is not yet a
published `@computesdk/brezel` package and does not authorize a competitive
performance claim. Upstream publication still requires a qualified hosted
endpoint, credentials provisioned to the benchmark operator, an official
provider package, and independent review.

ComputeSDK's sandbox suites measure two different boundaries that must remain
separate:

- **Burst TTI** creates 100 sandboxes concurrently and measures from
  `create()` through the first successful command. This exercises regional API
  proximity, admission, placement, warm capacity, node caches, networking,
  readiness, and the command path.
- **DAX** first creates a fresh sandbox, then times one guest command that
  installs system packages, downloads Bun, clones a pinned OpenCode revision,
  installs dependencies, and type-checks it. Sandbox create and destroy are
  outside DAX's reported `totalMs`; CPU quality and contention, writable-root
  storage, package setup, and network egress dominate that number.

The public DAX run dated 2026-09-11 used an advertised 8-vCPU/16-GiB shape but
reported different physical CPU families between providers and only one
iteration per provider in the retained result. It is useful comparative
evidence, not a normalized hardware comparison. Brezel must therefore qualify
a named 8-vCPU/16-GiB environment, retain the guest-reported CPU and memory,
run the exact upstream script repeatedly, and profile phase-level host metrics
before asking to join the public leaderboard.

Do not special-case that script or preinstall its repository and dependencies.
A competitive execution profile may legitimately use a production base image
with common build tools, host-local immutable image artifacts, a fast local
ephemeral writable root, and a separately mounted durable workspace. The
ephemeral root is the package-manager and compiler hot path; a durable
workspace is the acknowledged-data path and must retain its crash guarantees.

### Controller microbenchmarks

`internal/store` contains narrow Go benchmarks for control-state engineering.
They are not sandbox startup or guest-performance results. Run them on the same
machine before and after a change:

```sh
go test -run '^$' -bench 'BenchmarkFileStore(View|GetSandbox)$' -benchmem -count=5 ./internal/store
go test -run '^$' -bench '^BenchmarkFileStoreSandboxEventAppend' -benchmem -benchtime=10x -count=5 ./internal/store
go test -run '^$' -bench '^BenchmarkSQLiteStoreHotPath$' -benchmem -benchtime=200ms -count=5 ./internal/store
```

The keyed-read benchmark measures authorization lookup without copying
unrelated resources. The event benchmark compares the generic durable update
with the specialized content-free append. Both paths retain deep-copy isolation
where data leaves the store; the event path retains atomic replacement, file
and directory `fsync`, and publish-after-persist ordering. Record the Go
version, OS, architecture, CPU, run count, and complete output with any result.

On 2026-09-15, an Apple M4 development machine running Darwin arm64 measured
the SQLite keyed read at 15.7 to 18.4 microseconds, activity transaction at
67.2 to 94.8 microseconds, and event transaction at 95.3 to 107.3 microseconds
with 10,000 unrelated resources across five runs. The prior JSON whole-state
update was 18.9 to 21.9 milliseconds and its specialized event replacement was
15.3 to 17.8 milliseconds in the same run. This demonstrates constant-scale
controller hot paths and a roughly two-order-of-magnitude local write-path
improvement. It is not a Firecracker startup result, does not predict hosted
latency, and authorizes no competitive claim.

### Relay authorization microbenchmark

`internal/node` contains a protocol benchmark for capability issuance,
Ed25519 verification, replay admission, route-generation leasing, canonical
request validation, and command-event dispatch. It excludes TLS, network, API
admission, and guest execution, so it is useful for detecting relay regressions
but is not sandbox startup or end-to-end latency.

```sh
go test ./internal/node -run '^$' \
  -bench '^BenchmarkRelayCommandAuthorization$' -benchmem -count=5
```

On 2026-09-14, an Apple M4 development machine running Darwin arm64 observed
93.5 to 101.9 microseconds per operation, about 21.7 kB allocated, and 151
allocations per operation across five runs. This later sample supersedes an
earlier same-day 123.6 to 129.5 microsecond observation and is another reason
to retain complete repeated evidence rather than quote one local result.
This is local engineering evidence, not a portable performance claim. Linux/KVM
release evidence still requires the complete matrix above.

## Publishing rules

A result is publishable only when:

1. all 24 cells completed and every per-attempt and per-resource cleanup succeeded;
2. the repository, engine lock, runtime image, and benchmark binary identities
   are preserved;
3. host type, region, OS, CPU, memory, storage, image, resource shape, cache
   state, run counts, and arrival policy are disclosed;
4. scheduled-arrival and service p50, p95, p99, maximum, success rate, failed
   latency, censored failures, measurement window, time to first success,
   cleanup result, and observed throughput are published together;
5. at least one independent rerun shows comparable results; and
6. the report says exactly which boundary was timed.

Keep startup latency separate from workload throughput. The
[ComputeSDK methodology](https://www.computesdk.com/methodology/),
[DAX implementation](https://github.com/computesdk/benchmarks/blob/master/benchmarks/sandbox/dax.bench.ts),
and [DAX guest script](https://github.com/computesdk/benchmarks/blob/master/benchmarks/scripts/dax-benchmark.sh)
are useful references for create-through-first-command and build-workload
measurements. The
[StarSling HPC suite](https://starsling.dev/hpc-sandbox-benchmarks) instead runs
repository builds and versioned Phoronix profiles. Both are valuable, but their
numbers answer different questions and must not be merged into one leaderboard.
