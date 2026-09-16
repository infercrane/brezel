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

`warm-pool` means the request claimed a never-used clean slot for the exact
template and network class. The report must retain the configured target, clean
occupancy before the wave, claim failures, hard-deadline reset failures,
replacement creates, and confirmed destruction of every claimed backend
resource. A recycled customer sandbox, an in-memory-only reservation, or an
unreported cold fallback invalidates the label.

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
`67410ab5b928a335a79701d67eaf859df890da9c`. After simultaneous conformance, the
coordinator launched cached-template `tti`, `filesystem-restore`, and
`workspace-io` burst cases on both hosts. A paired case passed only when every
requested sample and every resource cleanup succeeded on both hosts and the
measurement windows overlapped.

Those three paired cases are simultaneous independent-host observations. Both
complete 24-cell per-host matrices passed: 48 of 48 cells, 2,112 of 2,112
attempts, and 3,168 of 3,168 expected resource cleanups. A separate unchanged
16-way immediate-command workload completed 320 of 320 attempts across the two
hosts at revision `67410ab5` after adding a guest-readiness gate and reducing
the default concurrent-start limit to three. The dated report preserves exact counts,
latency observations, and checksummed raw evidence. Neither the paired cases nor
the matrices replace an independently operated provider benchmark. They do not
measure a cluster, scheduling, automatic placement or failover, cross-host
restore, replicated storage, high availability, or hostile shared multitenancy.
Brezel makes no portable or competitive performance claim from this
qualification.

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

#### Public reference and Brezel entry gate

The public run dated 2026-09-11 is a useful reference, not a prediction of
Brezel's result:

| Provider | Burst TTI p50 / p95 | DAX total | Publicly useful architecture signal |
| --- | ---: | ---: | --- |
| Isorun | 0.03 / 0.04 s | 33.97 s | Documents KVM sandboxes, cached OCI images, hibernate/resume, snapshots, and forks; scheduler and VMM internals are not public |
| Miosa | 0.22 / 0.25 s | 42.30 s | Documents warm capacity as compatible compute kept ready; the isolation and storage hot paths are not public |
| Blaxel | 0.63 / 0.67 s | 43.65 s | Documents bare-metal Firecracker, host-local boot artifacts, EROFS, memory-backed writable roots, custom scheduling, and prepared VPP networking |
| E2B | 1.28 / 1.61 s | 80.07 s | A result for the hosted E2B service path, not an isolated measurement of the open-source runtime embedded by Brezel |

Sources: [ComputeSDK Burst TTI](https://www.computesdk.com/benchmarks/sandboxes/burst-tti/),
[ComputeSDK DAX](https://www.computesdk.com/benchmarks/sandboxes/dax/),
[Isorun documentation](https://docs.isorun.ai/),
[Miosa glossary](https://miosa.ai/docs/glossary), and
[Blaxel's runtime architecture](https://blaxel.ai/blog/anatomy-of-a-runtime).
Only Blaxel publishes enough internals in these sources for an architectural
comparison. Do not reverse-engineer the Isorun or Miosa leaderboard position
into an undocumented hypervisor, scheduler, cache, or pool design.

Brezel cleared its internal functional entry gate at revision
`f9fbc0ede72636349b27f01db49343d8daa87c5c` on a named 32-vCPU GCP N2 KVM
host in `us-east4`. The 100-way profile completed ten consecutive waves with
1,000 of 1,000 successful command-ready sandboxes and confirmed deletion. Its
aggregate TTI was 2.431 s p50, 2.937 s p95, and 3.182 s p99 from a neutral HTTPS
client. Every DAX sandbox was cleaned up, but the historical three-attempt DAX
report is reclassified nonconformant: one attempt printed an unambiguous native
dependency build failure while the enclosing upstream shell returned zero.
The retained timing is diagnostic only and is not compared as a qualified
provider result. The runner now rejects this failure mode.

The Burst result is materially slower than the leading providers and the
published E2B median of 1.28 s. This is a directional comparison only: Brezel
has not run inside ComputeSDK's official provider harness, host CPUs differ,
and the public table uses one iteration per scheduled provider run.

The retained phase medians explain the DAX gap more usefully than the rank:

| Phase | Brezel historical diagnostic | Blaxel public | Isorun public |
| --- | ---: | ---: | ---: |
| Prepare | 6.407 s | 1.241 s | 1.759 s |
| Bun download | 0.405 s | 0.385 s | 0.259 s |
| Bun unpack | 0.725 s | 0.465 s | 0.427 s |
| Clone | 2.597 s | 1.515 s | 1.054 s |
| Install | 14.160 s | 9.286 s | 9.177 s |
| Typecheck | 37.551 s | 23.857 s | 18.820 s |
| Provider-observed command total | 68.435 s (nonconformant report) | 43.653 s | 33.973 s |
| Guest internal phase total | 63.272 s (nonconformant report) | not compared | not compared |

The provider-observed total is the like-for-like timing boundary: host wall
time around `runCommand()`. The guest internal total excludes command setup,
post-total rendering, EXIT cleanup, transport, stream drain, persistence, and
SDK decoding. The retained per-attempt difference was 4.373 to 5.163 seconds;
it is an overhead envelope, not a network-latency measurement. The phase data
remains useful for diagnosis but not ranking. Approximately 64
percent of the historical Brezel gap to Isorun is the typecheck
phase, 17 percent is dependency installation, and 16 percent is preparation.
The calculation is directional because individual phase medians do not sum to
the median of per-run totals and the providers ran on different physical CPUs.
It nevertheless rules out API-language or controller serialization as the
primary DAX target. CPU quality and writable-root I/O are the first-order work;
the pinned Node 24 development image addresses preparation consistency but has
not yet been qualified as a latency improvement.

The entry gate is therefore:

1. qualify an immutable DAX-compatible environment with 8 vCPU, 16 GiB memory,
   at least 16 GiB of fast ephemeral writable root, a separate durable
   workspace, and host CPU headroom beyond the eight guest vCPUs;
2. qualify at least 100 simultaneous command-ready creates with headroom, zero
   admission failures, complete cleanup, and no host swap or disk saturation;
3. deploy an HTTPS data edge near the benchmark runner, reuse transport
   connections, and remove every redundant readiness round trip;
4. repeat the exact upstream workloads from a neutral client, retain all raw
   results, and require 100% success before requesting public inclusion; and
5. keep correctness gates on cancellation, standby, recovery, and cleanup even
   though the external suites do not score them.

The immediate performance program follows two distinct paths. Burst TTI needs
pre-created host plumbing, a small clean warm-template pool, command-ready
create semantics, a direct regional data path, and a 100-slot admission
profile. DAX needs modern high-clock CPUs, no vCPU overcommit, fast local
ephemeral storage for package-manager and compiler work, node-local immutable
artifacts, and phase-level CPU, memory, disk, and network telemetry. A single
optimization should not be expected to win both suites.

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

#### Local DAX optimization lab

Use the local lab to reject weak image and writable-root ideas before renting a
KVM host. It downloads and verifies the exact pinned upstream DAX script, runs
it unchanged in a fresh container, rejects hidden native-build failures, and
records the Docker engine, image identity, resource limits, bounded output
tails, and known limitations in JSON:

```sh
make benchmark-dax-local
```

The default target performs three preparation probes against the pinned plain
Node image and three against Brezel's general agent-development image. The
probe intentionally supplies a nonexistent local repository, so the unchanged
upstream script stops at clone after reporting preparation. Only that exact
expected clone failure is accepted. No benchmark source, Bun binary, dependency
cache, or OpenCode checkout is baked into the candidate.

On the 2026-09-16 macOS/ARM64 development machine, the retained three-run
median fell from 7.415 s to 1.507 s, a 5.908 s or 79.7 percent reduction in the
preparation phase. The candidate moves common source-build tools and apt index
acquisition into the immutable image. This is useful local A/B evidence for the
image decision, not a DAX score or a production claim. The host used Docker
Desktop, ARM64 containers, 8 CPUs, approximately 6.1 GiB of container memory,
and overlay storage; it did not reproduce x86-64, Firecracker, KVM, NBD, NUMA,
or the public suite's 16 GiB memory shape.

After Docker Desktop was increased to approximately 19.5 GiB, three complete
16-GiB runs per image also passed the exact workload and cleanup gates. Plain
image totals were 378.101 s, 345.721 s, and 267.576 s; candidate totals were
341.203 s, 315.607 s, and 306.132 s. Their medians were 345.721 s and 315.607 s
respectively, an 8.7 percent local reduction. Preparation medians fell from
13.478 s to 2.460 s, or 81.8 percent. Install medians differed by only 1.3
percent, while baseline typecheck ranged from 107.913 s to 217.174 s. The large
desktop typecheck variance prevents attributing the whole-workload delta to the
candidate image; only the preparation change is considered causal local
evidence. A single native-volume candidate run completed in 326.327 s and did
not beat the candidate overlay median, so it does not justify a storage change.

The local harness can also sample Docker CPU, memory, process, block-I/O, and
network counters and align them to the unchanged upstream phase markers. This
instrumentation is diagnostic and perturbs timing, so do not mix instrumented
and uninstrumented totals. A valid instrumented candidate run on the same
machine separated the two remaining large phases: `install` averaged about
34 percent of one CPU while receiving approximately 775 MB and writing
approximately 2.73 GB, whereas `typecheck` averaged approximately 816 percent
CPU, peaked near the Docker engine's available compute, used approximately
15.75 GB, and received no network bytes. The evidence supports a writable-root
and package-egress investigation for `install`, and CPU placement plus host CPU
quality for `typecheck`; it does not justify attributing either phase to the Go
API or SQLite path.

A controlled local CPU-sensitivity diagnostic compared the same candidate
image, overlay root, 16-GiB limit, and instrumented workload with four and
eight visible CPUs. The single retained pair completed in 471.737 s and
239.180 s respectively. Dependency installation changed by only 1.02x
(`121.338 s` to `118.535 s`), while typecheck changed by 3.28x (`311.856 s`
to `94.986 s`). This is one diagnostic pair, not a stable speedup estimate.
It is sufficient to prioritize guest CPU quality and topology for typecheck
while treating dependency installation as a separate network and writable-root
investigation. Causal or release decisions require at least five randomized,
paired, uninstrumented runs on the same qualified Linux/KVM host.

A subsequent instrumented tmpfs run completed in 317.918 s. Its install phase
was 118.221 s, effectively unchanged from the comparable overlay run at
118.535 s. One noisy pair cannot prove equivalence, but it rejects the simple
hypothesis that Docker Desktop overlay storage alone explains the install gap.
The Firecracker NBD path is different and still requires source-level call-count
benchmarks followed by randomized KVM A/B runs.

#### Command-path diagnostics

The single-host distribution has an opt-in command timing trace for separating
public HTTP, API-to-node relay, node relay, and guest-backend latency. It is off
by default. Enable it only on an otherwise quiet diagnostic deployment:

```sh
export BREZEL_BENCHMARK_DIAGNOSTICS=true
docker compose -f deploy/single-host/compose.yaml up -d --build
docker compose -f deploy/single-host/compose.yaml logs -f brezeld brezel-node
```

Each process writes newline-delimited JSON to standard error. The schema has
only fixed component, phase, and outcome enums plus monotonic nanosecond
durations and raw stdout/stderr byte and event counters. It has no project,
sandbox, execution, route, command, path, content, credential, host, process,
or wall-clock fields. The guest connection phase distinguishes cache `hit`,
`miss`, and `error` outcomes and records the lookup duration. Command samples
cover:

- `public_handler`: handler acceptance through the first flushed event, the
  terminal event, and the point at which the handler is ready to return;
- `relay_client`: API-side route lookup and the node response stream through
  its decoded EOF;
- `relay_server`: admitted relay handler work through backend completion and
  the point at which the relay handler is ready to return; and
- `guest_backend`: credential resolution and the envd command stream through
  terminal consumption.

`accepted_to_eof_ready_ns` is a server-side completion boundary for handlers.
It does not claim that a remote client has observed TCP EOF. Stream byte counts
are unencoded stdout/stderr payload bytes, not NDJSON, base64, TLS, or HTTP wire
bytes. Because the trace deliberately carries no resource correlation field,
use a dedicated project with one measured command at a time when comparing
samples across the two processes.

Diagnostic logging adds clock reads, synchronization, encoding, and I/O to the
measured path. An instrumented run is therefore for bottleneck attribution
only and is **not publishable benchmark evidence**. Disable
`BREZEL_BENCHMARK_DIAGNOSTICS`, rebuild or restart both services, and repeat the
unchanged workload with randomized paired uninstrumented runs before making a
performance claim.

The two-point model turns the controlled CPU pair into experiment targets. It
fits a conservative serial-floor plus inverse-CPU model per phase, refuses to
invent scaling for phases that became slower, and reports the remaining gap to
explicit leaderboard thresholds. Its output is a simulation, never benchmark
evidence:

```sh
node benchmarks/computesdk/dax-bottleneck-model.mjs \
  /tmp/brezel-dax-local-4cpu-profile.json \
  /tmp/brezel-dax-local-8cpu-profile.json \
  --target-cpus 8,16,32,44 \
  > /tmp/brezel-dax-local-bottleneck-model.json
```

After assigning Docker Desktop at least 16 GiB of memory, run the complete
workload and controlled writable-root experiments locally:

```sh
node benchmarks/computesdk/local-dax-lab.mjs \
  --probe full --build-candidate --image candidate \
  --storage overlay --iterations 3 \
  --output /tmp/brezel-dax-local-candidate.json

node benchmarks/computesdk/local-dax-lab.mjs \
  --probe full --image candidate --storage volume --iterations 3 \
  --output /tmp/brezel-dax-local-volume.json

node benchmarks/computesdk/local-dax-lab.mjs \
  --probe full --image candidate --storage tmpfs --iterations 3 \
  --output /tmp/brezel-dax-local-tmpfs.json

node benchmarks/computesdk/local-dax-lab.mjs \
  --probe full --image candidate --storage overlay --iterations 1 \
  --telemetry-interval-ms 1000 \
  --output /tmp/brezel-dax-local-profile.json

node benchmarks/computesdk/local-dax-sensitivity.mjs \
  /tmp/brezel-dax-local-4cpu-profile.json \
  /tmp/brezel-dax-local-8cpu-profile.json \
  > /tmp/brezel-dax-local-cpu-sensitivity.json
```

Local promotion requires every requested run to complete every upstream phase,
reach the pinned OpenCode commit, and contain no structured or hidden execution
failure. A winning local candidate must still be built as an immutable x86-64
image and pass repeated full-workload qualification on the same Linux/KVM host
as its baseline before changing a production profile. Only the qualified HTTPS
provider path can produce leaderboard-comparable evidence. The local phase
markers are useful for diagnosis, but neither `commandCloseWallMs`,
`lifecycleWallMs`, nor the sum of those internal markers is the ComputeSDK
leaderboard's provider-observed total-duration boundary.

#### Rehearsal commands

Install and qualify exactly one host-wide benchmark profile at a time:

```sh
./deploy/profiles/run.sh deploy/profiles/computesdk-dax.env make qualify-single-host
./deploy/profiles/run.sh deploy/profiles/burst-100-capacity.env make qualify-single-host
```

For DAX, use a dedicated empty project and run the pinned upstream workload at
least three times:

```sh
BREZEL_API_URL=https://sandbox.example.net \
BREZEL_SERVICE_TOKEN_FILE=/run/secrets/brezel-service-token \
BREZEL_PROJECT_ID=brezel-dax \
# Resolve or create the project-scoped immutable revision first; aliases such
# as "base" are not accepted by the adapter.
BREZEL_ENVIRONMENT_REVISION=envr_... \
BREZEL_ALLOW_INTERNET=true \
BREZEL_SOURCE_REVISION="$(git rev-parse HEAD)" \
BREZEL_BENCHMARK_REGION=us-east4 \
node benchmarks/computesdk/dax-rehearsal.mjs > dax-report.json
```

For Burst TTI, reinstall and requalify the 100-way profile, then run:

```sh
BREZEL_API_URL=https://sandbox.example.net \
BREZEL_SERVICE_TOKEN_FILE=/run/secrets/brezel-service-token \
BREZEL_PROJECT_ID=brezel-burst \
BREZEL_ENVIRONMENT_REVISION=envr_... \
BREZEL_SOURCE_REVISION="$(git rev-parse HEAD)" \
node benchmarks/computesdk/burst-rehearsal.mjs > burst-report.json
```

Each rehearsal refuses a nonempty project, validates the guest shape, holds all
100 burst sandboxes simultaneously, requires confirmed deletion, and exits
nonzero on any task or cleanup failure. The DAX internet exception belongs only
on a disposable benchmark project. The HTTPS edge uses host networking on a
dedicated benchmark host so it can reach the loopback API; this exception is
not a general multitenant deployment recommendation.

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
15.3 to 17.8 milliseconds in the same run. This demonstrates bounded history
sensitivity for the indexed controller hot paths and a roughly two-order-of-
magnitude local write-path improvement over the measured whole-state
implementation. It is not a Firecracker startup result, does not predict hosted
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

A result is eligible for a performance headline or promotion decision only
when:

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

Failed and partial evidence may still be published when it is labeled as such,
retains failure and cleanup accounting, and is not used as a performance
headline. Failure publication is part of the methodology, not an exception to
it.

Keep startup latency separate from workload throughput. The
[ComputeSDK methodology](https://www.computesdk.com/methodology/),
[DAX implementation](https://github.com/computesdk/benchmarks/blob/master/benchmarks/sandbox/dax.bench.ts),
and [DAX guest script](https://github.com/computesdk/benchmarks/blob/master/benchmarks/scripts/dax-benchmark.sh)
are useful references for create-through-first-command and build-workload
measurements. The
[StarSling HPC suite](https://starsling.dev/hpc-sandbox-benchmarks) instead runs
repository builds and versioned Phoronix profiles. Both are valuable, but their
numbers answer different questions and must not be merged into one leaderboard.
