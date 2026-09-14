#!/bin/sh
set -eu

# Run the complete single-host customer-observed latency matrix. This script is
# intentionally destructive and is separate from install/qualification so an
# operator cannot start a long benchmark accidentally.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
INSTALL_DIR=${BREZEL_INSTALL_DIR:-"$REPO_DIR/.brezel"}
TOKEN_FILE=${BREZEL_SERVICE_TOKEN_FILE:-"$INSTALL_DIR/secrets/service.token"}
BENCH_BINARY=${BREZEL_BENCH_BINARY:-"$REPO_DIR/bin/brezel-bench"}
OUTPUT_ROOT=${BREZEL_BENCH_OUTPUT_DIR:-"$INSTALL_DIR/benchmarks"}
TARGET=${BREZEL_BENCH_TARGET:-}
BREZEL_REVISION=${BREZEL_BENCH_RUNTIME_REVISION:-}
EVIDENCE_CLASS=${BREZEL_BENCH_EVIDENCE_CLASS:-single-host-linux-kvm}
CACHE_STATE=${BREZEL_BENCH_CACHE_STATE:-cached-template}
BACKEND_TEMPLATE=${BREZEL_BENCH_BACKEND_TEMPLATE:-base}
PROJECT=${BREZEL_BENCH_PROJECT:-brezel-benchmark}
SEQUENTIAL_RUNS=${BREZEL_BENCH_SEQUENTIAL_RUNS:-100}
STAGGERED_RUNS=${BREZEL_BENCH_STAGGERED_RUNS:-24}
BURST_RUNS=${BREZEL_BENCH_BURST_RUNS:-24}
STAGGER_INTERVAL=${BREZEL_BENCH_STAGGER_INTERVAL:-200ms}
ATTEMPT_TIMEOUT=${BREZEL_BENCH_ATTEMPT_TIMEOUT:-2m}
CLEANUP_TIMEOUT=${BREZEL_BENCH_CLEANUP_TIMEOUT:-2m}
CASE_TIMEOUT=${BREZEL_BENCH_CASE_TIMEOUT:-45m}
COOLDOWN_SECONDS=${BREZEL_BENCH_COOLDOWN_SECONDS:-15}
IO_BYTES=${BREZEL_BENCH_IO_BYTES:-1048576}
PREVIEW_PORT=${BREZEL_BENCH_PREVIEW_PORT:-8080}
BASE_URL=${BREZEL_BENCH_BASE_URL:-http://127.0.0.1:8080}
BASE_URL=${BASE_URL%/}

# Compose interpolation is also used while capturing the exact running image.
# Keep it aligned with the installed single-host service identity even when the
# benchmark is invoked from a fresh login shell.
export BREZEL_STATE_DIR=${BREZEL_STATE_DIR:-"$INSTALL_DIR/state"}
export BREZEL_SECRETS_DIR=${BREZEL_SECRETS_DIR:-"$INSTALL_DIR/secrets"}
export BREZEL_UID=${BREZEL_UID:-"$(id -u)"}
export BREZEL_GID=${BREZEL_GID:-"$(id -g)"}
SCENARIOS="tti warm-exec resume filesystem-checkpoint filesystem-restore preview-first-byte preview-warm workspace-io"
SCENARIO_COUNT=$(printf '%s\n' $SCENARIOS | wc -l | tr -d ' ')
EXPECTED_CASES=$((SCENARIO_COUNT * 3))

fail() {
  echo "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

validate_identity() {
  value=$1
  name=$2
  case "$value" in
    ""|*[!A-Za-z0-9._-]*) fail "$name may contain only letters, numbers, dots, underscores, and hyphens" ;;
  esac
}

validate_positive_integer() {
  value=$1
  name=$2
  case "$value" in
    ""|*[!0-9]*) fail "$name must be a positive integer" ;;
  esac
  [ "$value" -gt 0 ] || fail "$name must be greater than zero"
  [ "$value" -le 256 ] || fail "$name cannot exceed 256"
}

validate_bounded_integer() {
  value=$1
  name=$2
  maximum=$3
  case "$value" in
    ""|*[!0-9]*) fail "$name must be a positive integer" ;;
  esac
  [ "$value" -gt 0 ] || fail "$name must be greater than zero"
  [ "$value" -le "$maximum" ] || fail "$name cannot exceed $maximum"
}

if [ "${BREZEL_BENCH_EXECUTE:-}" != true ]; then
  fail "set BREZEL_BENCH_EXECUTE=true to acknowledge that this matrix creates and deletes real microVMs"
fi

[ "$(uname -s)" = Linux ] || fail "single-host benchmark requires Linux"
case "$(uname -m)" in
  x86_64|amd64) ;;
  *) fail "single-host benchmark requires x86-64" ;;
esac
[ -c /dev/kvm ] || fail "/dev/kvm is required"
[ -c /dev/net/tun ] || fail "/dev/net/tun is required"

for command_name in awk curl cut date find findmnt git head jq lscpu paste sed sha256sum sort stat tr uname wc xargs; do
  require_command "$command_name"
done
[ -x "$BENCH_BINARY" ] || fail "brezel-bench binary is missing or not executable: $BENCH_BINARY"
[ -f "$TOKEN_FILE" ] || fail "runtime service token is missing: $TOKEN_FILE"
[ "$(stat -c '%a' "$TOKEN_FILE")" = 600 ] || fail "runtime service token must have mode 0600"

validate_identity "$TARGET" BREZEL_BENCH_TARGET
validate_identity "$PROJECT" BREZEL_BENCH_PROJECT
validate_identity "$BACKEND_TEMPLATE" BREZEL_BENCH_BACKEND_TEMPLATE
validate_positive_integer "$SEQUENTIAL_RUNS" BREZEL_BENCH_SEQUENTIAL_RUNS
validate_positive_integer "$STAGGERED_RUNS" BREZEL_BENCH_STAGGERED_RUNS
validate_positive_integer "$BURST_RUNS" BREZEL_BENCH_BURST_RUNS
validate_positive_integer "$COOLDOWN_SECONDS" BREZEL_BENCH_COOLDOWN_SECONDS
validate_bounded_integer "$IO_BYTES" BREZEL_BENCH_IO_BYTES 33554432
validate_bounded_integer "$PREVIEW_PORT" BREZEL_BENCH_PREVIEW_PORT 65535

[ "$EVIDENCE_CLASS" = single-host-linux-kvm ] || fail "single-host runner requires BREZEL_BENCH_EVIDENCE_CLASS=single-host-linux-kvm"
case "$CACHE_STATE" in
  cold|cached-template|warm-pool|unknown) ;;
  *) fail "unsupported BREZEL_BENCH_CACHE_STATE: $CACHE_STATE" ;;
esac
case "$BASE_URL" in
  http://127.0.0.1:*|https://127.0.0.1:*|http://localhost:*|https://localhost:*|http://\[::1\]:*|https://\[::1\]:*) ;;
  *) fail "single-host benchmark API must use an explicit loopback URL with a port" ;;
esac

if [ -z "$BREZEL_REVISION" ]; then
  BREZEL_REVISION=$(git -C "$REPO_DIR" rev-parse HEAD)
fi
case "$BREZEL_REVISION" in
  ""|*[!A-Za-z0-9._+-]*) fail "BREZEL_BENCH_RUNTIME_REVISION contains unsupported characters" ;;
esac
if [ -n "$(git -C "$REPO_DIR" status --porcelain)" ] && [ "${BREZEL_BENCH_ALLOW_DIRTY:-}" != true ]; then
  fail "repository has tracked or untracked changes; commit them or set BREZEL_BENCH_ALLOW_DIRTY=true for non-publishable development evidence"
fi

if ! curl --fail --silent --show-error --max-time 5 "$BASE_URL/readyz" >/dev/null; then
  fail "runtime API is not ready at $BASE_URL"
fi

started_compact=$(date -u +%Y%m%dT%H%M%SZ)
started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
run_dir="$OUTPUT_ROOT/$TARGET-$started_compact"
[ ! -e "$run_dir" ] || fail "benchmark output already exists: $run_dir"
umask 077
mkdir -p "$run_dir/raw" "$run_dir/stderr"
chmod 700 "$run_dir" "$run_dir/raw" "$run_dir/stderr"
printf '%s\n' running > "$run_dir/STATUS"
cases_file="$run_dir/cases.ndjson"
: > "$cases_file"

read_os_release() {
  field=$1
  sed -n "s/^$field=//p" /etc/os-release 2>/dev/null | head -n 1 | sed 's/^"//;s/"$//'
}

lscpu_value() {
  label=$1
  lscpu | awk -F: -v wanted="$label" '$1 == wanted {sub(/^[[:space:]]+/, "", $2); print $2; exit}'
}

capture_host() {
  destination=$1
  phase=$2
  docker_server_version=unavailable
  docker_storage_driver=unavailable
  docker_cgroup_version=unavailable
  runtime_image_id=unavailable
  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    docker_server_version=$(docker version --format '{{.Server.Version}}' 2>/dev/null || printf unavailable)
    docker_storage_driver=$(docker info --format '{{.Driver}}' 2>/dev/null || printf unavailable)
    docker_cgroup_version=$(docker info --format '{{.CgroupVersion}}' 2>/dev/null || printf unavailable)
    runtime_container=$(docker compose -f "$SCRIPT_DIR/compose.yaml" ps -q brezeld 2>/dev/null || true)
    if [ -n "$runtime_container" ]; then
      runtime_image_id=$(docker inspect --format '{{.Image}}' "$runtime_container" 2>/dev/null || printf unavailable)
    fi
  fi
  repo_dirty=false
  if [ -n "$(git -C "$REPO_DIR" status --porcelain)" ]; then
    repo_dirty=true
  fi
  virtualization=unknown
  if command -v systemd-detect-virt >/dev/null 2>&1; then
    virtualization=$(systemd-detect-virt 2>/dev/null || true)
    [ -n "$virtualization" ] || virtualization=none
  fi
  cpu_governors=unknown
  if [ -d /sys/devices/system/cpu/cpufreq ]; then
    cpu_governors=$(find /sys/devices/system/cpu/cpufreq -name scaling_governor -type f -exec sed -n '1p' {} \; 2>/dev/null | sort -u | paste -sd, -)
    [ -n "$cpu_governors" ] || cpu_governors=unknown
  fi
  root_source=$(findmnt -n -o SOURCE / 2>/dev/null || printf unknown)
  root_fstype=$(findmnt -n -o FSTYPE / 2>/dev/null || printf unknown)
  root_options=$(findmnt -n -o OPTIONS / 2>/dev/null || printf unknown)
  jq -n \
    --arg phase "$phase" \
    --arg captured_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg os_id "$(read_os_release ID)" \
    --arg os_version "$(read_os_release VERSION_ID)" \
    --arg kernel_release "$(uname -r)" \
    --arg architecture "$(uname -m)" \
    --arg virtualization "$virtualization" \
    --arg cpu_model "$(lscpu_value 'Model name')" \
    --arg cpu_sockets "$(lscpu_value 'Socket(s)')" \
    --arg cpu_cores_per_socket "$(lscpu_value 'Core(s) per socket')" \
    --arg cpu_threads_per_core "$(lscpu_value 'Thread(s) per core')" \
    --arg cpu_governors "$cpu_governors" \
    --arg memory_total_kib "$(awk '/^MemTotal:/ {print $2; exit}' /proc/meminfo)" \
    --arg memory_available_kib "$(awk '/^MemAvailable:/ {print $2; exit}' /proc/meminfo)" \
    --arg load_average "$(cut -d ' ' -f 1-3 /proc/loadavg)" \
    --arg root_source "$root_source" \
    --arg root_fstype "$root_fstype" \
    --arg root_options "$root_options" \
    --arg root_available_bytes "$(findmnt -b -n -o AVAIL / 2>/dev/null || printf unknown)" \
    --arg docker_server_version "$docker_server_version" \
    --arg docker_storage_driver "$docker_storage_driver" \
    --arg docker_cgroup_version "$docker_cgroup_version" \
    --arg runtime_image_id "$runtime_image_id" \
    --arg repo_revision "$(git -C "$REPO_DIR" rev-parse HEAD)" \
    --argjson repo_dirty "$repo_dirty" \
    --arg benchmark_binary_sha256 "$(sha256sum "$BENCH_BINARY" | awk '{print $1}')" \
    --arg engine_lock_sha256 "$(sha256sum "$SCRIPT_DIR/engine.lock" | awk '{print $1}')" \
    --argjson kvm_available "$([ -c /dev/kvm ] && printf true || printf false)" \
    --argjson tun_available "$([ -c /dev/net/tun ] && printf true || printf false)" \
    '{schema_version:1,phase:$phase,captured_at:$captured_at,host:{os:{id:$os_id,version:$os_version},kernel_release:$kernel_release,architecture:$architecture,virtualization:$virtualization,cpu:{model:$cpu_model,sockets:$cpu_sockets,cores_per_socket:$cpu_cores_per_socket,threads_per_core:$cpu_threads_per_core,governors:$cpu_governors},memory:{total_kib:$memory_total_kib,available_kib:$memory_available_kib},load_average:$load_average,root_filesystem:{source:$root_source,type:$root_fstype,options:$root_options,available_bytes:$root_available_bytes},devices:{kvm:$kvm_available,tun:$tun_available}},software:{docker:{server_version:$docker_server_version,storage_driver:$docker_storage_driver,cgroup_version:$docker_cgroup_version,runtime_image_id:$runtime_image_id},repository:{revision:$repo_revision,dirty:$repo_dirty},benchmark_binary_sha256:$benchmark_binary_sha256,engine_lock_sha256:$engine_lock_sha256}}' \
    > "$destination"
  chmod 600 "$destination"
}

capture_host "$run_dir/host-before.json" before

matrix_failed=false
stop_for_cleanup_risk=false
for scenario in $SCENARIOS; do
  for mode in sequential staggered burst; do
    case "$mode" in
      sequential)
        runs=$SEQUENTIAL_RUNS
        max_in_flight=1
        ;;
      staggered)
        runs=$STAGGERED_RUNS
        max_in_flight=$STAGGERED_RUNS
        ;;
      burst)
        runs=$BURST_RUNS
        max_in_flight=$BURST_RUNS
        ;;
    esac
    key="$scenario-$mode"
    raw_tmp="$run_dir/raw/.$key.temporary"
    raw_file="$run_dir/raw/$key.json"
    stderr_file="$run_dir/stderr/$key.log"
    echo "Running $key ($runs attempts, max in flight $max_in_flight)" >&2
    set +e
    BREZEL_SERVICE_TOKEN_FILE="$TOKEN_FILE" "$BENCH_BINARY" \
      -base-url "$BASE_URL" \
      -project "$PROJECT" \
      -backend-template "$BACKEND_TEMPLATE" \
      -target "$TARGET" \
      -runtime-revision "$BREZEL_REVISION" \
      -evidence-class "$EVIDENCE_CLASS" \
      -cache-state "$CACHE_STATE" \
      -scenario "$scenario" \
      -mode "$mode" \
      -runs "$runs" \
      -max-in-flight "$max_in_flight" \
      -stagger "$STAGGER_INTERVAL" \
      -attempt-timeout "$ATTEMPT_TIMEOUT" \
      -cleanup-timeout "$CLEANUP_TIMEOUT" \
      -io-bytes "$IO_BYTES" \
      -preview-port "$PREVIEW_PORT" \
      -timeout "$CASE_TIMEOUT" \
      -execute > "$raw_tmp" 2> "$stderr_file"
    command_status=$?
    set -e
    chmod 600 "$raw_tmp" "$stderr_file"

    if jq -e \
      --arg scenario "$scenario" \
      --arg mode "$mode" \
      --arg target "$TARGET" \
      --arg revision "$BREZEL_REVISION" \
      --argjson runs "$runs" \
      --argjson max_in_flight "$max_in_flight" \
      --argjson io_bytes "$IO_BYTES" \
      --argjson preview_port "$PREVIEW_PORT" \
      '.schema_version == 3 and .scenario == $scenario and .mode == $mode and .runs == $runs and .max_in_flight == $max_in_flight and .metadata.target == $target and .metadata.runtime_revision == $revision and (.metadata.scenario_setup_definition | type == "string" and length > 0) and (.metadata.verification_definition | type == "string" and length > 0) and (if $scenario == "workspace-io" then .metadata.io_bytes == $io_bytes else true end) and (if ($scenario == "preview-first-byte" or $scenario == "preview-warm") then .metadata.preview_port == $preview_port else true end) and (.samples | type == "array" and length == $runs) and all(.samples[]; (.index | type == "number") and (.success | type == "boolean") and (.cleanup_succeeded | type == "boolean") and ((.cleanup_resources // []) | type == "array")) and (.summary | type == "object") and (.summary.requested == $runs) and (.summary.scheduled | type == "number") and (.summary.started | type == "number") and (.summary.completed | type == "number") and (.summary.succeeded | type == "number") and (.summary.failed | type == "number") and (.summary.success_rate | type == "number") and (.summary.succeeded + .summary.failed == .summary.requested) and (.summary.cleanup_not_required | type == "number") and (.summary.cleanup_succeeded | type == "number") and (.summary.cleanup_failed | type == "number") and (.summary.cleanup_not_required + .summary.cleanup_succeeded + .summary.cleanup_failed == .summary.requested) and (.summary.cleanup_resources_expected | type == "number") and (.summary.cleanup_resources_succeeded | type == "number") and (.summary.cleanup_resources_failed | type == "number") and (.summary.cleanup_resources_succeeded + .summary.cleanup_resources_failed == .summary.cleanup_resources_expected) and (.summary.latency | type == "object") and (.summary.latency.samples == .summary.succeeded) and (.summary.service_latency | type == "object") and (.summary.service_latency.samples == .summary.succeeded) and (.summary.failure_latency | type == "object") and (.summary.latency_censored | type == "number") and (.summary.failure_latency.samples + .summary.latency_censored == .summary.failed)' \
      "$raw_tmp" >/dev/null 2>&1; then
      mv "$raw_tmp" "$raw_file"
      jq -c \
        --arg key "$key" \
        --arg raw "raw/$key.json" \
        --arg stderr "stderr/$key.log" \
        --argjson command_status "$command_status" \
        '{key:$key,report_schema_version:.schema_version,scenario:.scenario,mode:.mode,runs:.runs,max_in_flight:.max_in_flight,command_exit_status:$command_status,raw_report:$raw,stderr_log:$stderr,preparation_error_category:(.preparation_error_category // null),requested:.summary.requested,scheduled:.summary.scheduled,started:.summary.started,completed:.summary.completed,succeeded:.summary.succeeded,failed:.summary.failed,success_rate:.summary.success_rate,measurement_window_ns:(.summary.measurement_window_ns // null),time_to_first_success_ns:(.summary.time_to_first_success_ns // null),observed_completions_per_second:(.summary.observed_completions_per_second // null),scheduled_latency:.summary.latency,service_latency:.summary.service_latency,failure_latency:.summary.failure_latency,latency_censored:.summary.latency_censored,successful_bytes:{written:(.summary.successful_bytes_written // 0),read:(.summary.successful_bytes_read // 0)},cleanup:{attempts:{not_required:.summary.cleanup_not_required,attempted:([.samples[] | select((.cleanup_attempts // 0) > 0)] | length),confirmed:.summary.cleanup_succeeded,failed:.summary.cleanup_failed},resources:{expected:.summary.cleanup_resources_expected,confirmed:.summary.cleanup_resources_succeeded,failed:.summary.cleanup_resources_failed}},error_categories:(.summary.errors // {}),outcome:(if $command_status == 0 and (.preparation_error_category // "") == "" and .summary.failed == 0 and .summary.cleanup_failed == 0 and .summary.cleanup_resources_failed == 0 then "passed" else "failed" end)}' \
        "$raw_file" >> "$cases_file"
      cleanup_failed=$(jq -r '(.summary.cleanup_failed // 0) + (.summary.cleanup_resources_failed // 0)' "$raw_file")
      report_passed=$(jq -r --argjson command_status "$command_status" 'if $command_status == 0 and (.preparation_error_category // "") == "" and .summary.failed == 0 and .summary.cleanup_failed == 0 and .summary.cleanup_resources_failed == 0 then "true" else "false" end' "$raw_file")
      if [ "$report_passed" != true ]; then
        matrix_failed=true
      fi
      if [ "$cleanup_failed" -gt 0 ]; then
        stop_for_cleanup_risk=true
      fi
    else
      invalid_file="$run_dir/raw/$key.invalid"
      mv "$raw_tmp" "$invalid_file"
      jq -nc \
        --arg key "$key" \
        --arg scenario "$scenario" \
        --arg mode "$mode" \
        --arg raw "raw/$key.invalid" \
        --arg stderr "stderr/$key.log" \
        --argjson runs "$runs" \
        --argjson max_in_flight "$max_in_flight" \
        --argjson command_status "$command_status" \
        '{key:$key,scenario:$scenario,mode:$mode,runs:$runs,max_in_flight:$max_in_flight,command_exit_status:$command_status,raw_report:$raw,stderr_log:$stderr,outcome:"invalid_report"}' \
        >> "$cases_file"
      matrix_failed=true
      stop_for_cleanup_risk=true
    fi

    if [ "$stop_for_cleanup_risk" = true ]; then
      echo "Stopping matrix after $key because cleanup was not confirmed or its report was invalid." >&2
      break 2
    fi
    sleep "$COOLDOWN_SECONDS"
  done
done

capture_host "$run_dir/host-after.json" after
finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
case_count=$(wc -l < "$cases_file" | tr -d ' ')
if [ "$case_count" -ne "$EXPECTED_CASES" ]; then
  matrix_failed=true
fi
jq -s \
  --arg started_at "$started_at" \
  --arg finished_at "$finished_at" \
  --arg target "$TARGET" \
  --arg runtime_revision "$BREZEL_REVISION" \
  --arg evidence_class "$EVIDENCE_CLASS" \
  --arg cache_state "$CACHE_STATE" \
  --arg backend_template "$BACKEND_TEMPLATE" \
  --arg project "$PROJECT" \
  --arg scenarios "$SCENARIOS" \
  --arg stagger_interval "$STAGGER_INTERVAL" \
  --argjson sequential_runs "$SEQUENTIAL_RUNS" \
  --argjson staggered_runs "$STAGGERED_RUNS" \
  --argjson burst_runs "$BURST_RUNS" \
  --argjson cooldown_seconds "$COOLDOWN_SECONDS" \
  --argjson io_bytes "$IO_BYTES" \
  --argjson preview_port "$PREVIEW_PORT" \
  --argjson expected_cases "$EXPECTED_CASES" \
  '{schema_version:3,started_at:$started_at,finished_at:$finished_at,target:$target,runtime_revision:$runtime_revision,evidence_class:$evidence_class,cache_state:$cache_state,backend_template:$backend_template,project:$project,configuration:{scenarios:($scenarios | split(" ")),sequential_runs:$sequential_runs,staggered_runs:$staggered_runs,burst_runs:$burst_runs,stagger_interval:$stagger_interval,cooldown_seconds:$cooldown_seconds,io_bytes:$io_bytes,preview_port:$preview_port},expected_cases:$expected_cases,completed_cases:length,passed_cases:([.[] | select(.outcome == "passed")] | length),failed_cases:([.[] | select(.outcome != "passed")] | length),planned_attempts:([.[].runs] | add // 0),requested_attempts:([.[].requested // 0] | add // 0),scheduled_attempts:([.[].scheduled // 0] | add // 0),started_attempts:([.[].started // 0] | add // 0),completed_attempts:([.[].completed // 0] | add // 0),successful_attempts:([.[].succeeded // 0] | add // 0),failed_attempts:([.[].failed // 0] | add // 0),successful_bytes:{written:([.[].successful_bytes.written // 0] | add // 0),read:([.[].successful_bytes.read // 0] | add // 0)},latency_censored:([.[].latency_censored // 0] | add // 0),cleanup:{attempts:{not_required:([.[].cleanup.attempts.not_required // 0] | add // 0),attempted:([.[].cleanup.attempts.attempted // 0] | add // 0),confirmed:([.[].cleanup.attempts.confirmed // 0] | add // 0),failed:([.[].cleanup.attempts.failed // 0] | add // 0)},resources:{expected:([.[].cleanup.resources.expected // 0] | add // 0),confirmed:([.[].cleanup.resources.confirmed // 0] | add // 0),failed:([.[].cleanup.resources.failed // 0] | add // 0)}},outcome:(if length == $expected_cases and all(.[]; .outcome == "passed") then "passed" else "failed" end),host_metadata:{before:"host-before.json",after:"host-after.json"},cases:.}' \
  "$cases_file" > "$run_dir/summary.json"
chmod 600 "$run_dir/summary.json" "$cases_file"

if [ "$matrix_failed" = true ]; then
  printf '%s\n' failed > "$run_dir/STATUS"
else
  printf '%s\n' passed > "$run_dir/STATUS"
fi
(
  cd "$run_dir"
  find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS
  chmod 600 SHA256SUMS
)

if [ "$matrix_failed" = true ]; then
  echo "Benchmark matrix failed or stopped early. Evidence was retained at $run_dir" >&2
  exit 1
fi
echo "Benchmark matrix passed. Evidence: $run_dir"
