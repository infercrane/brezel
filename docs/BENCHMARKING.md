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

Defaults are 100 sequential attempts and 24 staggered and burst attempts. The
concurrent defaults deliberately retain 25 percent headroom under the
distribution's 64-active-sandbox project limit: `filesystem-restore` can hold
two sandboxes and one checkpoint per attempt, for 48 active sandboxes at the
default concurrency. `workspace-io` holds one sandbox and one workspace per
attempt. The benchmark project must be dedicated and empty before a matrix;
operators must lower concurrent runs to fit stricter configured limits. A
published comparison must use
identical counts, arrival intervals, image, resources, payload size, preview
port, cache declaration, and success definition for every release under
comparison.

The benchmark does not clear caches. `cold`, `cached-template`, `warm-pool`, and
`unknown` are explicit operator declarations. A cold-cache report must document
the external reset procedure; changing the label alone does not make a run cold.

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

### Controller microbenchmarks

`internal/store` contains narrow Go benchmarks for control-state engineering.
They are not sandbox startup or guest-performance results. Run them on the same
machine before and after a change:

```sh
go test -run '^$' -bench 'BenchmarkFileStore(View|GetSandbox)$' -benchmem -count=5 ./internal/store
go test -run '^$' -bench '^BenchmarkFileStoreSandboxEventAppend' -benchmem -benchtime=10x -count=5 ./internal/store
```

The keyed-read benchmark measures authorization lookup without copying
unrelated resources. The event benchmark compares the generic durable update
with the specialized content-free append. Both paths retain deep-copy isolation
where data leaves the store; the event path retains atomic replacement, file
and directory `fsync`, and publish-after-persist ordering. Record the Go
version, OS, architecture, CPU, run count, and complete output with any result.

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
87.4 to 102.9 microseconds per operation, about 21.7 kB allocated, and 151
allocations per operation across five runs. This is local engineering evidence,
not a portable performance claim. Linux/KVM release evidence still requires the
complete matrix above.

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
[ComputeSDK methodology](https://github.com/runloopai/computesdk-benchmarks/blob/master/METHODOLOGY.md)
is a useful reference for create-through-first-command measurements. The
[StarSling HPC suite](https://starsling.dev/hpc-sandbox-benchmarks) instead runs
repository builds and versioned Phoronix profiles. Both are valuable, but their
numbers answer different questions and must not be merged into one leaderboard.
