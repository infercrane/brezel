#!/usr/bin/env bash
set -Eeuo pipefail

# Qualify two independent single-host Brezel installations. This deliberately
# does not join their control state, route traffic between them, or exercise a
# scheduler. It proves simultaneous operation, namespace separation, and
# controller/node-relay process-failure containment only.

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
SINGLE_HOST_DIR="$REPO_DIR/deploy/single-host"

fail() {
  printf 'dual-host qualification failed: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

validate_identity() {
  local value=$1 name=$2
  [[ -n "$value" && "$value" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] ||
    fail "$name may contain only letters, numbers, dots, underscores, and hyphens"
}

validate_resource_id() {
  local value=$1 name=$2
  [[ -n "$value" && "$value" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$ ]] ||
    fail "$name is not a valid opaque resource ID"
}

validate_positive_integer() {
  local value=$1 name=$2 maximum=$3
  [[ "$value" =~ ^[0-9]+$ ]] || fail "$name must be a positive integer"
  (( value > 0 && value <= maximum )) || fail "$name must be between 1 and $maximum"
}

file_mode() {
  if stat -c '%a' "$1" >/dev/null 2>&1; then
    stat -c '%a' "$1"
  else
    stat -f '%Lp' "$1"
  fi
}

require_regular_file() {
  local path=$1 name=$2 private=${3:-false} mode
  [[ "$path" = /* ]] || fail "$name must be an absolute path"
  [[ ! -L "$path" && -f "$path" ]] || fail "$name must be a regular file and not a symlink"
  mode=$(file_mode "$path")
  [[ "$mode" =~ ^[0-7]{3,4}$ ]] || fail "could not determine permissions for $name"
  if [[ "$private" = true ]]; then
    (( (8#$mode & 077) == 0 )) || fail "$name must not be group- or world-accessible"
  else
    (( (8#$mode & 022) == 0 )) || fail "$name must not be group- or world-writable"
  fi
}

remote_compose() {
  docker compose -f "$SINGLE_HOST_DIR/compose.yaml" "$@"
}

remote_engine_compose() {
  local install_dir=${BREZEL_INSTALL_DIR:-"$REPO_DIR/.brezel"}
  docker compose --env-file "$install_dir/engine/embed/compose/.env" \
    -f "$install_dir/engine/embed/compose/compose.yaml" "$@"
}

remote_cli() {
  remote_compose exec -T brezeld /usr/local/bin/brezel \
    -url http://127.0.0.1:8080 \
    -token-file /run/brezel-secrets/service.token \
    -project brezel-conformance "$@"
}

remote_fixture_file() {
  local install_dir=${BREZEL_INSTALL_DIR:-"$REPO_DIR/.brezel"}
  printf '%s/qualification/.dual-independent-%s.json\n' "$install_dir" "$1"
}

remote_wait_ready() {
  local attempt
  for ((attempt = 0; attempt < 90; attempt++)); do
    if curl --fail --silent --show-error --max-time 2 http://127.0.0.1:8080/readyz >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

remote_preflight() {
  local label=$1 run_id=$2 install_dir revision engine_revision dirty
  local capacity engine_capacity listeners ca_sha node_cert_sha api_cert_sha runtime_container runtime_image
  local machine_id_sha boot_id_sha
  validate_identity "$label" host_label
  validate_identity "$run_id" run_id
  install_dir=${BREZEL_INSTALL_DIR:-"$REPO_DIR/.brezel"}
  for command_name in curl docker git jq openssl sha256sum ss stat timedatectl; do
    require_command "$command_name"
  done
  [[ "$(uname -s)" = Linux ]] || fail "remote host must run Linux"
  [[ "$(uname -m)" = x86_64 || "$(uname -m)" = amd64 ]] || fail "remote host must be x86-64"
  [[ -c /dev/kvm ]] || fail "remote host is missing /dev/kvm"
  [[ -c /dev/net/tun ]] || fail "remote host is missing /dev/net/tun"
  [[ "$(timedatectl show -p NTPSynchronized --value)" = yes ]] || fail "remote host clock is not NTP-synchronized"
  dirty=$(git -C "$REPO_DIR" status --porcelain)
  [[ -z "$dirty" ]] || fail "remote repository must be clean"
  revision=$(git -C "$REPO_DIR" rev-parse HEAD)
  engine_revision=$(sed -n 's/^commit=//p' "$SINGLE_HOST_DIR/engine.lock")
  [[ "$engine_revision" =~ ^[0-9a-f]{40}$ ]] || fail "engine lock has no valid commit"
  curl --fail --silent --show-error --max-time 5 http://127.0.0.1:8080/readyz >/dev/null
  capacity=$("$SINGLE_HOST_DIR/capacity-contract.sh" live)
  engine_capacity=$(remote_engine_compose exec -T \
    -e "BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL=${BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL:-32}" \
    postgres sh -s -- verify < "$SINGLE_HOST_DIR/engine-capacity-contract.sh")
  listeners=$(ss -H -ltn | awk '$4 ~ /:(8080|8443|8444)$/ {print $4}' | LC_ALL=C sort)
  [[ "$(printf '%s\n' "$listeners" | grep -Ec '^127\.0\.0\.1:(8080|8443|8444)$')" -eq 3 ]] ||
    fail "Brezel API and relay listeners must bind exactly to IPv4 loopback"
  [[ "$(printf '%s\n' "$listeners" | wc -l | tr -d ' ')" -eq 3 ]] ||
    fail "Brezel ports have unexpected additional listeners"
  for secret_file in service.token node-ca.crt node.crt api.crt; do
    require_regular_file "$install_dir/secrets/$secret_file" "$secret_file" true
  done
  ca_sha=$(sha256sum "$install_dir/secrets/node-ca.crt" | awk '{print $1}')
  node_cert_sha=$(sha256sum "$install_dir/secrets/node.crt" | awk '{print $1}')
  api_cert_sha=$(sha256sum "$install_dir/secrets/api.crt" | awk '{print $1}')
  [[ -r /etc/machine-id && -r /proc/sys/kernel/random/boot_id ]] || fail "remote host identity sources are unavailable"
  machine_id_sha=$(sha256sum /etc/machine-id | awk '{print $1}')
  boot_id_sha=$(sha256sum /proc/sys/kernel/random/boot_id | awk '{print $1}')
  runtime_container=$(remote_compose ps -q brezeld)
  [[ -n "$runtime_container" ]] || fail "Brezel API container is not running"
  runtime_image=$(docker inspect --format '{{.Image}}' "$runtime_container")
  [[ "$runtime_image" =~ ^sha256:[0-9a-f]{64}$ ]] || fail "could not resolve the running Brezel image"
  jq -n \
    --arg label "$label" \
    --arg run_id "$run_id" \
    --arg captured_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --argjson captured_epoch_ms "$(date +%s%3N)" \
    --arg revision "$revision" \
    --arg engine_revision "$engine_revision" \
    --arg engine_lock_sha256 "$(sha256sum "$SINGLE_HOST_DIR/engine.lock" | awk '{print $1}')" \
    --arg runtime_image "$runtime_image" \
    --arg ca_sha256 "$ca_sha" \
    --arg node_certificate_sha256 "$node_cert_sha" \
    --arg api_certificate_sha256 "$api_cert_sha" \
    --arg machine_id_sha256 "$machine_id_sha" \
    --arg boot_id_sha256 "$boot_id_sha" \
    --argjson capacity "$capacity" \
    --argjson engine_capacity "$engine_capacity" \
    '{schema_version:1,host_label:$label,run_id:$run_id,captured_at:$captured_at,captured_epoch_ms:$captured_epoch_ms,profile:"independent-single-host",host_identity:{machine_id_sha256:$machine_id_sha256,boot_id_sha256:$boot_id_sha256},repository:{revision:$revision,clean:true},engine:{revision:$engine_revision,lock_sha256:$engine_lock_sha256},runtime_image:$runtime_image,identity:{ca_sha256:$ca_sha256,node_certificate_sha256:$node_certificate_sha256,api_certificate_sha256:$api_certificate_sha256},listeners:{api:"127.0.0.1:8080",node_data:"127.0.0.1:8443",node_control:"127.0.0.1:8444",public:false},capacity:$capacity,engine_capacity:$engine_capacity,kvm:true,tun:true,clock:{ntp_synchronized:true}}'
}

remote_conformance() {
  local label=$1 run_id=$2 target
  validate_identity "$label" host_label
  validate_identity "$run_id" run_id
  target="dual-${run_id}-${label}-conformance"
  remote_compose exec -T brezeld /usr/local/bin/brezel-conformance \
    -base-url http://127.0.0.1:8080 \
    -backend-template base \
    -project brezel-conformance \
    -other-project brezel-conformance-isolation \
    -target "$target" \
    -timeout 10m \
    -execute
}

remote_benchmark() {
  local label=$1 run_id=$2 scenario=$3 runs=$4 revision
  validate_identity "$label" host_label
  validate_identity "$run_id" run_id
  validate_positive_integer "$runs" benchmark_runs 16
  case "$scenario" in
    tti|filesystem-restore|workspace-io) ;;
    *) fail "unsupported dual-host benchmark scenario: $scenario" ;;
  esac
  revision=$(git -C "$REPO_DIR" rev-parse HEAD)
  # Every paired scenario gets its own authenticated, read-only cleanliness
  # gate. The previous scenario may leave a reusable environment revision, but
  # it must not leave an active sandbox or workspace that would distort load,
  # cleanup, or capacity measurements.
  remote_compose exec -T brezeld /usr/local/bin/brezel-bench \
    -base-url http://127.0.0.1:8080 \
    -project brezel-benchmark \
    -preflight-empty-project \
    -timeout 30s >/dev/null
  remote_compose exec -T brezeld /usr/local/bin/brezel-bench \
    -base-url http://127.0.0.1:8080 \
    -project brezel-benchmark \
    -backend-template base \
    -target "dual-${run_id}-${label}" \
    -runtime-revision "$revision" \
    -evidence-class single-host-linux-kvm \
    -cache-state cached-template \
    -scenario "$scenario" \
    -mode burst \
    -runs "$runs" \
    -max-in-flight "$runs" \
    -attempt-timeout 2m \
    -cleanup-timeout 2m \
    -timeout 20m \
    -execute
}

remote_create_fixture() {
  local label=$1 run_id=$2 state_file workspace_id='' sandbox_id='' marker marker_sha state_tmp
  validate_identity "$label" host_label
  validate_identity "$run_id" run_id
  state_file=$(remote_fixture_file "$run_id")
  [[ ! -e "$state_file" ]] || fail "a fixture already exists for run $run_id"
  mkdir -p "$(dirname -- "$state_file")"
  chmod 700 "$(dirname -- "$state_file")"
  cleanup_partial_fixture() {
    set +e
    [[ -z "$sandbox_id" ]] || remote_cli sandbox delete "$sandbox_id" >/dev/null 2>&1
    [[ -z "$workspace_id" ]] || remote_cli workspace delete "$workspace_id" >/dev/null 2>&1
  }
  trap cleanup_partial_fixture RETURN
  workspace_id=$(remote_cli workspace create "dual-${run_id}-${label}" | awk 'NR == 1 {print $1}')
  validate_resource_id "$workspace_id" workspace_id
  sandbox_id=$(remote_cli new --template base --workspace "$workspace_id:/workspace" --ttl 1800 | awk 'NR == 1 {print $1}')
  validate_resource_id "$sandbox_id" sandbox_id
  marker=$(openssl rand -hex 32)
  marker_sha=$(printf '%s' "$marker" | sha256sum | awk '{print $1}')
  remote_cli exec "$sandbox_id" /bin/sh -lc 'printf %s "$1" > /workspace/dual-independent-host && sync' dual-fixture "$marker" >/dev/null
  state_tmp=$(mktemp "$(dirname -- "$state_file")/.dual-independent-state.XXXXXX")
  jq -n \
    --arg label "$label" --arg run_id "$run_id" --arg workspace_id "$workspace_id" \
    --arg sandbox_id "$sandbox_id" --arg marker_sha256 "$marker_sha" \
    '{host_label:$label,run_id:$run_id,workspace_id:$workspace_id,sandbox_id:$sandbox_id,marker_sha256:$marker_sha256}' \
    > "$state_tmp"
  chmod 600 "$state_tmp"
  mv "$state_tmp" "$state_file"
  trap - RETURN
  jq -n \
    --arg label "$label" --arg sandbox_id "$sandbox_id" --arg workspace_id "$workspace_id" \
    --arg marker_sha256 "$marker_sha" \
    '{schema_version:1,host_label:$label,sandbox_id:$sandbox_id,workspace_id:$workspace_id,marker_sha256:$marker_sha256,state:"running"}'
}

remote_curl_status() {
  local method=$1 path=$2 idempotency=${3:-} body=${4:-} install_dir config response status
  install_dir=${BREZEL_INSTALL_DIR:-"$REPO_DIR/.brezel"}
  config=$(mktemp "$install_dir/qualification/.dual-curl-config.XXXXXX")
  response=$(mktemp "$install_dir/qualification/.dual-curl-response.XXXXXX")
  chmod 600 "$config" "$response"
  {
    printf 'silent\nshow-error\nmax-time = 30\n'
    printf 'header = "Authorization: Bearer %s"\n' "$(<"$install_dir/secrets/service.token")"
    printf 'header = "X-Project-ID: brezel-conformance"\n'
    [[ -z "$idempotency" ]] || printf 'header = "Idempotency-Key: %s"\n' "$idempotency"
  } > "$config"
  local -a args=(--config "$config" --output "$response" --write-out '%{http_code}' --request "$method")
  if [[ -n "$body" ]]; then
    args+=(--header 'Content-Type: application/json' --data-binary "@$body")
  fi
  if ! status=$(curl "${args[@]}" "http://127.0.0.1:8080$path"); then
    rm -f -- "$config" "$response"
    return 1
  fi
  rm -f -- "$config" "$response"
  printf '%s\n' "$status"
}

remote_isolation_probe() {
  local label=$1 run_id=$2 foreign_sandbox=$3 foreign_workspace=$4 request_body status
  local inspect_sandbox delete_sandbox run_command read_file inspect_workspace delete_workspace
  validate_identity "$label" host_label
  validate_identity "$run_id" run_id
  validate_resource_id "$foreign_sandbox" foreign_sandbox_id
  validate_resource_id "$foreign_workspace" foreign_workspace_id
  request_body=$(mktemp "${BREZEL_INSTALL_DIR:-"$REPO_DIR/.brezel"}/qualification/.dual-request.XXXXXX")
  chmod 600 "$request_body"
  printf '{"argv":["/bin/true"],"timeout_seconds":10}\n' > "$request_body"
  inspect_sandbox=$(remote_curl_status GET "/v1/sandboxes/$foreign_sandbox")
  delete_sandbox=$(remote_curl_status DELETE "/v1/sandboxes/$foreign_sandbox" "dual-delete-${run_id}-${label}")
  run_command=$(remote_curl_status POST "/v1/sandboxes/$foreign_sandbox/commands" '' "$request_body")
  read_file=$(remote_curl_status GET "/v1/sandboxes/$foreign_sandbox/files?path=%2Fworkspace%2Fdual-independent-host")
  inspect_workspace=$(remote_curl_status GET "/v1/workspaces/$foreign_workspace")
  delete_workspace=$(remote_curl_status DELETE "/v1/workspaces/$foreign_workspace" "dual-workspace-delete-${run_id}-${label}")
  rm -f -- "$request_body"
  for status in "$inspect_sandbox" "$delete_sandbox" "$run_command" "$read_file" "$inspect_workspace" "$delete_workspace"; do
    [[ "$status" = 404 ]] || fail "foreign resource operation returned $status instead of 404"
  done
  jq -n \
    --arg label "$label" \
    --arg foreign_sandbox_sha256 "$(printf '%s' "$foreign_sandbox" | sha256sum | awk '{print $1}')" \
    --arg foreign_workspace_sha256 "$(printf '%s' "$foreign_workspace" | sha256sum | awk '{print $1}')" \
    --argjson inspect_sandbox "$inspect_sandbox" --argjson delete_sandbox "$delete_sandbox" \
    --argjson run_command "$run_command" --argjson read_file "$read_file" \
    --argjson inspect_workspace "$inspect_workspace" --argjson delete_workspace "$delete_workspace" \
    '{schema_version:1,host_label:$label,foreign_resource_hashes:{sandbox:$foreign_sandbox_sha256,workspace:$foreign_workspace_sha256},observed_status:{inspect_sandbox:$inspect_sandbox,delete_sandbox:$delete_sandbox,run_command:$run_command,read_file:$read_file,inspect_workspace:$inspect_workspace,delete_workspace:$delete_workspace},outcome:"passed"}'
}

remote_verify_fixture() {
  local run_id=$1 state_file sandbox_id expected observed state
  validate_identity "$run_id" run_id
  state_file=$(remote_fixture_file "$run_id")
  [[ -f "$state_file" && ! -L "$state_file" ]] || fail "fixture state is missing"
  sandbox_id=$(jq -er '.sandbox_id' "$state_file")
  expected=$(jq -er '.marker_sha256' "$state_file")
  validate_resource_id "$sandbox_id" sandbox_id
  state=$(remote_cli sandbox inspect "$sandbox_id" | jq -er '.state')
  [[ "$state" = running ]] || fail "fixture sandbox is $state instead of running"
  observed=$(remote_cli exec "$sandbox_id" /bin/cat /workspace/dual-independent-host | sha256sum | awk '{print $1}')
  [[ "$observed" = "$expected" ]] || fail "fixture marker changed"
  jq -n --arg state "$state" --arg marker_sha256 "$observed" \
    '{schema_version:1,state:$state,marker_sha256:$marker_sha256,outcome:"passed"}'
}

remote_canary() {
  local label=$1 run_id=$2 duration=$3 state_file sandbox_id expected started finished
  local attempts=0 succeeded=0 failed=0 maximum_ms=0 attempt_started elapsed observed
  validate_identity "$label" host_label
  validate_identity "$run_id" run_id
  validate_positive_integer "$duration" canary_duration_seconds 120
  state_file=$(remote_fixture_file "$run_id")
  [[ -f "$state_file" && ! -L "$state_file" ]] || fail "fixture state is missing"
  sandbox_id=$(jq -er '.sandbox_id' "$state_file")
  expected=$(jq -er '.marker_sha256' "$state_file")
  started=$(date +%s%3N)
  local deadline=$((started + duration * 1000))
  while (( $(date +%s%3N) < deadline )); do
    attempts=$((attempts + 1))
    attempt_started=$(date +%s%3N)
    if observed=$(remote_cli exec "$sandbox_id" /bin/cat /workspace/dual-independent-host 2>/dev/null | sha256sum | awk '{print $1}') && [[ "$observed" = "$expected" ]]; then
      succeeded=$((succeeded + 1))
    else
      failed=$((failed + 1))
    fi
    elapsed=$(( $(date +%s%3N) - attempt_started ))
    (( elapsed <= maximum_ms )) || maximum_ms=$elapsed
    sleep 0.2
  done
  finished=$(date +%s%3N)
  jq -n --arg label "$label" --argjson started_ms "$started" --argjson finished_ms "$finished" \
    --argjson attempts "$attempts" --argjson succeeded "$succeeded" --argjson failed "$failed" \
    --argjson maximum_ms "$maximum_ms" \
    '{schema_version:1,host_label:$label,started_epoch_ms:$started_ms,finished_epoch_ms:$finished_ms,attempts:$attempts,succeeded:$succeeded,failed:$failed,max_latency_ms:$maximum_ms,outcome:(if $failed == 0 and $attempts > 0 then "passed" else "failed" end)}'
  (( attempts > 0 && failed == 0 ))
}

remote_crash_service() {
  local label=$1 run_id=$2 service=$3 container before_started after_started fault_started ready_at finished
  validate_identity "$label" host_label
  validate_identity "$run_id" run_id
  case "$service" in
    brezeld|brezel-node) ;;
    *) fail "only brezeld or brezel-node may be crashed" ;;
  esac
  remote_verify_fixture "$run_id" >/dev/null
  container=$(remote_compose ps -q "$service")
  [[ -n "$container" ]] || fail "$service container is not running"
  before_started=$(docker inspect --format '{{.State.StartedAt}}' "$container")
  fault_started=$(date +%s%3N)
  remote_compose kill -s SIGKILL "$service" >/dev/null
  remote_compose up -d "$service" >/dev/null
  remote_wait_ready || fail "runtime did not recover after crashing $service"
  ready_at=$(date +%s%3N)
  container=$(remote_compose ps -q "$service")
  [[ -n "$container" ]] || fail "$service container did not return"
  after_started=$(docker inspect --format '{{.State.StartedAt}}' "$container")
  [[ "$after_started" != "$before_started" ]] || fail "$service process start identity did not change"
  remote_verify_fixture "$run_id" >/dev/null
  finished=$(date +%s%3N)
  jq -n --arg label "$label" --arg service "$service" \
    --arg before_started "$before_started" --arg after_started "$after_started" \
    --argjson fault_started_ms "$fault_started" --argjson ready_at_ms "$ready_at" --argjson finished_ms "$finished" \
    '{schema_version:1,host_label:$label,fault:(if $service == "brezeld" then "controller_sigkill" else "node_relay_sigkill" end),service:$service,process_started_at:{before:$before_started,after:$after_started},fault_started_epoch_ms:$fault_started_ms,ready_epoch_ms:$ready_at_ms,finished_epoch_ms:$finished_ms,recovery_ms:($ready_at_ms-$fault_started_ms),fixture:{state:"running",marker_preserved:true},outcome:"passed"}'
}

remote_cleanup_fixture() {
  local run_id=$1 state_file sandbox_id workspace_id sandbox_status=not_attempted workspace_status=not_attempted
  validate_identity "$run_id" run_id
  state_file=$(remote_fixture_file "$run_id")
  if [[ ! -e "$state_file" ]]; then
    jq -n '{schema_version:1,fixture_present:false,outcome:"passed"}'
    return 0
  fi
  [[ -f "$state_file" && ! -L "$state_file" ]] || fail "fixture state is not a regular file"
  sandbox_id=$(jq -er '.sandbox_id' "$state_file")
  workspace_id=$(jq -er '.workspace_id' "$state_file")
  validate_resource_id "$sandbox_id" sandbox_id
  validate_resource_id "$workspace_id" workspace_id
  if remote_cli sandbox delete "$sandbox_id" >/dev/null; then sandbox_status=confirmed; else sandbox_status=failed; fi
  if remote_cli workspace delete "$workspace_id" >/dev/null; then workspace_status=confirmed; else workspace_status=failed; fi
  if [[ "$sandbox_status" = confirmed && "$workspace_status" = confirmed ]]; then
    rm -f -- "$state_file"
    jq -n --arg sandbox "$sandbox_status" --arg workspace "$workspace_status" \
      '{schema_version:1,fixture_present:true,cleanup:{sandbox:$sandbox,workspace:$workspace},outcome:"passed"}'
    return 0
  fi
  jq -n --arg sandbox "$sandbox_status" --arg workspace "$workspace_status" \
    '{schema_version:1,fixture_present:true,cleanup:{sandbox:$sandbox,workspace:$workspace},outcome:"failed"}'
  return 1
}

remote_worker() {
  local action=${1:-}
  shift || true
  local install_dir=${BREZEL_INSTALL_DIR:-"$REPO_DIR/.brezel"}
  export BREZEL_STATE_DIR=${BREZEL_STATE_DIR:-"$install_dir/state"}
  export BREZEL_SECRETS_DIR=${BREZEL_SECRETS_DIR:-"$install_dir/secrets"}
  export BREZEL_UID=${BREZEL_UID:-"$(id -u)"}
  export BREZEL_GID=${BREZEL_GID:-"$(id -g)"}
  case "$action" in
    preflight) [[ $# -eq 2 ]] || fail "preflight expects label and run ID"; remote_preflight "$@" ;;
    conformance) [[ $# -eq 2 ]] || fail "conformance expects label and run ID"; remote_conformance "$@" ;;
    benchmark) [[ $# -eq 4 ]] || fail "benchmark expects label, run ID, scenario, and runs"; remote_benchmark "$@" ;;
    create-fixture) [[ $# -eq 2 ]] || fail "create-fixture expects label and run ID"; remote_create_fixture "$@" ;;
    isolation-probe) [[ $# -eq 4 ]] || fail "isolation-probe expects label, run ID, sandbox ID, and workspace ID"; remote_isolation_probe "$@" ;;
    verify-fixture) [[ $# -eq 1 ]] || fail "verify-fixture expects run ID"; remote_verify_fixture "$@" ;;
    canary) [[ $# -eq 3 ]] || fail "canary expects label, run ID, and duration"; remote_canary "$@" ;;
    crash-service) [[ $# -eq 3 ]] || fail "crash-service expects label, run ID, and service"; remote_crash_service "$@" ;;
    cleanup-fixture) [[ $# -eq 1 ]] || fail "cleanup-fixture expects run ID"; remote_cleanup_fixture "$@" ;;
    *) fail "unknown remote worker action" ;;
  esac
}

if [[ "${1:-}" = --remote-worker ]]; then
  shift
  remote_worker "$@"
  exit
fi

usage() {
  cat >&2 <<'EOF'
Usage:
  BREZEL_DUAL_HOST_A=user@host-a \
  BREZEL_DUAL_HOST_B=user@host-b \
  BREZEL_DUAL_SSH_KNOWN_HOSTS=/absolute/pinned-known-hosts \
  BREZEL_DUAL_SSH_IDENTITY_FILE=/absolute/private-key \
  BREZEL_DUAL_EXECUTE=true \
    ./deploy/qualification/dual-independent-hosts.sh

This destructive qualification addresses two independent single-host runtimes.
It does not create or qualify a cluster, scheduler, failover path, replicated
storage system, shared control plane, or high-availability deployment.
EOF
}

[[ $# -eq 0 ]] || { usage; fail "unexpected arguments"; }
[[ "${BREZEL_DUAL_EXECUTE:-}" = true ]] || { usage; fail "set BREZEL_DUAL_EXECUTE=true to acknowledge destructive remote qualification"; }

for command_name in bash find jq mktemp sha256sum ssh stat; do
  require_command "$command_name"
done

HOST_A=${BREZEL_DUAL_HOST_A:-}
HOST_B=${BREZEL_DUAL_HOST_B:-}
KNOWN_HOSTS=${BREZEL_DUAL_SSH_KNOWN_HOSTS:-}
IDENTITY_FILE=${BREZEL_DUAL_SSH_IDENTITY_FILE:-}
SSH_PORT=${BREZEL_DUAL_SSH_PORT:-22}
REMOTE_REPO=${BREZEL_DUAL_REMOTE_REPO:-/opt/brezel}
BENCH_RUNS=${BREZEL_DUAL_BENCH_RUNS:-8}
CANARY_SECONDS=${BREZEL_DUAL_CANARY_SECONDS:-45}
RUN_ID=${BREZEL_DUAL_RUN_ID:-"$(date -u +%Y%m%dT%H%M%SZ)"}
OUTPUT_ROOT=${BREZEL_DUAL_OUTPUT_DIR:-"$REPO_DIR/.brezel/qualification/dual"}

[[ "$HOST_A" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.-]*$ ]] || fail "BREZEL_DUAL_HOST_A must be user@hostname"
[[ "$HOST_B" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9.-]*$ ]] || fail "BREZEL_DUAL_HOST_B must be user@hostname"
[[ "$HOST_A" != "$HOST_B" ]] || fail "host A and host B must differ"
validate_positive_integer "$SSH_PORT" BREZEL_DUAL_SSH_PORT 65535
validate_positive_integer "$BENCH_RUNS" BREZEL_DUAL_BENCH_RUNS 16
validate_positive_integer "$CANARY_SECONDS" BREZEL_DUAL_CANARY_SECONDS 120
(( CANARY_SECONDS >= 20 )) || fail "BREZEL_DUAL_CANARY_SECONDS must be at least 20"
validate_identity "$RUN_ID" BREZEL_DUAL_RUN_ID
[[ "$REMOTE_REPO" =~ ^/[A-Za-z0-9_./-]+$ && "$REMOTE_REPO" != */../* && "$REMOTE_REPO" != */.. ]] || fail "BREZEL_DUAL_REMOTE_REPO must be a normalized absolute path"
[[ "$OUTPUT_ROOT" = /* ]] || fail "BREZEL_DUAL_OUTPUT_DIR must be an absolute path"
require_regular_file "$KNOWN_HOSTS" BREZEL_DUAL_SSH_KNOWN_HOSTS false
require_regular_file "$IDENTITY_FILE" BREZEL_DUAL_SSH_IDENTITY_FILE true

RUN_DIR="$OUTPUT_ROOT/$RUN_ID"
[[ ! -e "$RUN_DIR" ]] || fail "evidence path already exists: $RUN_DIR"
umask 077
mkdir -p "$RUN_DIR/host-a" "$RUN_DIR/host-b"
chmod 700 "$RUN_DIR" "$RUN_DIR/host-a" "$RUN_DIR/host-b"
printf '%s\n' running > "$RUN_DIR/STATUS"

SSH_OPTIONS=(
  -F /dev/null
  -p "$SSH_PORT"
  -i "$IDENTITY_FILE"
  -o BatchMode=yes
  -o IdentitiesOnly=yes
  -o PasswordAuthentication=no
  -o KbdInteractiveAuthentication=no
  -o StrictHostKeyChecking=yes
  -o "UserKnownHostsFile=$KNOWN_HOSTS"
  -o GlobalKnownHostsFile=/dev/null
  -o ConnectTimeout=10
  -o ServerAliveInterval=10
  -o ServerAliveCountMax=3
  -o LogLevel=ERROR
)

FAILURE_REASON='qualification did not complete'
FIXTURE_A=false
FIXTURE_B=false
FINALIZED=false
ACTIVE_PIDS=()

remote() {
  local host=$1 action=$2
  shift 2
  local argument command="cd $REMOTE_REPO && exec bash deploy/qualification/dual-independent-hosts.sh --remote-worker $action"
  for argument in "$@"; do
    [[ "$argument" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$ ]] || fail "unsafe remote worker argument"
    command+=" $argument"
  done
  ssh "${SSH_OPTIONS[@]}" -- "$host" "$command"
}

seal_evidence() {
  rm -f -- "$RUN_DIR/SHA256SUMS"
  (
    cd "$RUN_DIR"
    find . -type f ! -name SHA256SUMS -print | LC_ALL=C sort | while IFS= read -r evidence_file; do
      sha256sum "$evidence_file"
    done > SHA256SUMS
    chmod 600 SHA256SUMS
  )
}

cleanup_fixture_best_effort() {
  local host=$1 destination=$2
  if remote "$host" cleanup-fixture "$RUN_ID" > "$destination" 2> "$destination.stderr"; then
    chmod 600 "$destination" "$destination.stderr"
    return 0
  fi
  chmod 600 "$destination" "$destination.stderr" 2>/dev/null || true
  return 1
}

stop_active_jobs() {
  local pid
  for pid in "${ACTIVE_PIDS[@]}"; do
    kill "$pid" >/dev/null 2>&1 || true
  done
  for pid in "${ACTIVE_PIDS[@]}"; do
    wait "$pid" >/dev/null 2>&1 || true
  done
  ACTIVE_PIDS=()
}

on_exit() {
  local status=$?
  [[ "$FINALIZED" = false ]] || return
  set +e
  local cleanup_failed=false
  stop_active_jobs
  if [[ "$FIXTURE_A" = true ]]; then
    cleanup_fixture_best_effort "$HOST_A" "$RUN_DIR/host-a/cleanup-after-failure.json" || cleanup_failed=true
  fi
  if [[ "$FIXTURE_B" = true ]]; then
    cleanup_fixture_best_effort "$HOST_B" "$RUN_DIR/host-b/cleanup-after-failure.json" || cleanup_failed=true
  fi
  jq -n --arg finished_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg reason "$FAILURE_REASON" \
    --argjson command_status "$status" --argjson cleanup_failed "$cleanup_failed" \
    '{schema_version:1,finished_at:$finished_at,outcome:"failed",reason:$reason,command_status:$command_status,cleanup_failed:$cleanup_failed}' \
    > "$RUN_DIR/failure.json"
  printf '%s\n' failed > "$RUN_DIR/STATUS"
  seal_evidence
  printf 'Partial sealed evidence: %s\n' "$RUN_DIR" >&2
}
trap on_exit EXIT
trap 'FAILURE_REASON="qualification interrupted"; exit 130' HUP INT TERM

run_pair() {
  local action=$1 file=$2
  shift 2
  local -a arguments=("$@")
  local pid_a pid_b status_a=0 status_b=0
  remote "$HOST_A" "$action" a "$RUN_ID" "${arguments[@]}" > "$RUN_DIR/host-a/$file.json" 2> "$RUN_DIR/host-a/$file.stderr" &
  pid_a=$!
  remote "$HOST_B" "$action" b "$RUN_ID" "${arguments[@]}" > "$RUN_DIR/host-b/$file.json" 2> "$RUN_DIR/host-b/$file.stderr" &
  pid_b=$!
  ACTIVE_PIDS=("$pid_a" "$pid_b")
  wait "$pid_a" || status_a=$?
  wait "$pid_b" || status_b=$?
  ACTIVE_PIDS=()
  chmod 600 "$RUN_DIR/host-a/$file.json" "$RUN_DIR/host-a/$file.stderr" "$RUN_DIR/host-b/$file.json" "$RUN_DIR/host-b/$file.stderr"
  (( status_a == 0 && status_b == 0 )) || fail "$action failed on one or both hosts"
}

require_report_overlap() {
  local first=$1 second=$2 first_start first_finish second_start second_finish
  first_start=$(jq -er '.started_at' "$first")
  first_finish=$(jq -er '.finished_at' "$first")
  second_start=$(jq -er '.started_at' "$second")
  second_finish=$(jq -er '.finished_at' "$second")
  [[ "$first_start" < "$second_finish" && "$second_start" < "$first_finish" ]] ||
    fail "paired reports did not overlap in time"
}

FAILURE_REASON='simultaneous preflight failed'
run_pair preflight preflight
jq -e '.schema_version == 1 and .profile == "independent-single-host" and .repository.clean == true and .listeners.public == false and .kvm == true and .tun == true' "$RUN_DIR/host-a/preflight.json" >/dev/null
jq -e '.schema_version == 1 and .profile == "independent-single-host" and .repository.clean == true and .listeners.public == false and .kvm == true and .tun == true' "$RUN_DIR/host-b/preflight.json" >/dev/null
REVISION_A=$(jq -er '.repository.revision' "$RUN_DIR/host-a/preflight.json")
REVISION_B=$(jq -er '.repository.revision' "$RUN_DIR/host-b/preflight.json")
ENGINE_A=$(jq -er '.engine.revision' "$RUN_DIR/host-a/preflight.json")
ENGINE_B=$(jq -er '.engine.revision' "$RUN_DIR/host-b/preflight.json")
CA_A=$(jq -er '.identity.ca_sha256' "$RUN_DIR/host-a/preflight.json")
CA_B=$(jq -er '.identity.ca_sha256' "$RUN_DIR/host-b/preflight.json")
MACHINE_A=$(jq -er '.host_identity.machine_id_sha256' "$RUN_DIR/host-a/preflight.json")
MACHINE_B=$(jq -er '.host_identity.machine_id_sha256' "$RUN_DIR/host-b/preflight.json")
BOOT_A=$(jq -er '.host_identity.boot_id_sha256' "$RUN_DIR/host-a/preflight.json")
BOOT_B=$(jq -er '.host_identity.boot_id_sha256' "$RUN_DIR/host-b/preflight.json")
PREFLIGHT_EPOCH_A=$(jq -er '.captured_epoch_ms' "$RUN_DIR/host-a/preflight.json")
PREFLIGHT_EPOCH_B=$(jq -er '.captured_epoch_ms' "$RUN_DIR/host-b/preflight.json")
[[ "$REVISION_A" = "$REVISION_B" ]] || fail "hosts run different Brezel revisions"
[[ "$ENGINE_A" = "$ENGINE_B" ]] || fail "hosts pin different engine revisions"
[[ "$CA_A" != "$CA_B" ]] || fail "independent hosts unexpectedly share a node CA"
[[ "$MACHINE_A" != "$MACHINE_B" ]] || fail "SSH targets unexpectedly share a machine identity"
[[ "$BOOT_A" != "$BOOT_B" ]] || fail "SSH targets unexpectedly share a kernel boot identity"
PREFLIGHT_CLOCK_DELTA=$((PREFLIGHT_EPOCH_A - PREFLIGHT_EPOCH_B))
(( PREFLIGHT_CLOCK_DELTA >= 0 )) || PREFLIGHT_CLOCK_DELTA=$((-PREFLIGHT_CLOCK_DELTA))
(( PREFLIGHT_CLOCK_DELTA <= 5000 )) || fail "host clocks differ by more than five seconds"

FAILURE_REASON='simultaneous conformance failed'
run_pair conformance conformance
for host in host-a host-b; do
  jq -e '.qualification == "sandbox_runtime_conformant" and (.steps | length > 0) and all(.steps[]; .status == "passed")' "$RUN_DIR/$host/conformance.json" >/dev/null
done
require_report_overlap "$RUN_DIR/host-a/conformance.json" "$RUN_DIR/host-b/conformance.json"

for scenario in tti filesystem-restore workspace-io; do
  FAILURE_REASON="simultaneous $scenario benchmark failed"
  run_pair benchmark "$scenario-burst" "$scenario" "$BENCH_RUNS"
  for host in host-a host-b; do
    jq -e --arg scenario "$scenario" --argjson runs "$BENCH_RUNS" \
      '.schema_version == 3 and .scenario == $scenario and .mode == "burst" and .runs == $runs and .summary.requested == $runs and .summary.succeeded == $runs and .summary.failed == 0 and .summary.cleanup_failed == 0 and .summary.cleanup_resources_failed == 0' \
      "$RUN_DIR/$host/$scenario-burst.json" >/dev/null
  done
  require_report_overlap "$RUN_DIR/host-a/$scenario-burst.json" "$RUN_DIR/host-b/$scenario-burst.json"
done

FAILURE_REASON='fixture creation failed'
FIXTURE_A=true
FIXTURE_B=true
run_pair create-fixture fixture
SANDBOX_A=$(jq -er '.sandbox_id' "$RUN_DIR/host-a/fixture.json")
WORKSPACE_A=$(jq -er '.workspace_id' "$RUN_DIR/host-a/fixture.json")
SANDBOX_B=$(jq -er '.sandbox_id' "$RUN_DIR/host-b/fixture.json")
WORKSPACE_B=$(jq -er '.workspace_id' "$RUN_DIR/host-b/fixture.json")
validate_resource_id "$SANDBOX_A" host_a_sandbox_id
validate_resource_id "$WORKSPACE_A" host_a_workspace_id
validate_resource_id "$SANDBOX_B" host_b_sandbox_id
validate_resource_id "$WORKSPACE_B" host_b_workspace_id
[[ "$SANDBOX_A" != "$SANDBOX_B" && "$WORKSPACE_A" != "$WORKSPACE_B" ]] || fail "hosts returned colliding public resource IDs"

FAILURE_REASON='cross-host namespace isolation failed'
remote "$HOST_A" isolation-probe a "$RUN_ID" "$SANDBOX_B" "$WORKSPACE_B" > "$RUN_DIR/host-a/isolation.json" 2> "$RUN_DIR/host-a/isolation.stderr"
remote "$HOST_B" isolation-probe b "$RUN_ID" "$SANDBOX_A" "$WORKSPACE_A" > "$RUN_DIR/host-b/isolation.json" 2> "$RUN_DIR/host-b/isolation.stderr"
remote "$HOST_A" verify-fixture "$RUN_ID" > "$RUN_DIR/host-a/fixture-after-isolation.json" 2> "$RUN_DIR/host-a/fixture-after-isolation.stderr"
remote "$HOST_B" verify-fixture "$RUN_ID" > "$RUN_DIR/host-b/fixture-after-isolation.json" 2> "$RUN_DIR/host-b/fixture-after-isolation.stderr"
for host in host-a host-b; do
  jq -e '.outcome == "passed" and all(.observed_status[]; . == 404)' "$RUN_DIR/$host/isolation.json" >/dev/null
  jq -e '.outcome == "passed" and .state == "running"' "$RUN_DIR/$host/fixture-after-isolation.json" >/dev/null
done

run_fault_drill() {
  local victim_host=$1 victim_label=$2 healthy_host=$3 healthy_label=$4 service=$5 name=$6
  local canary_pid canary_status=0 fault_status=0
  FAILURE_REASON="$name containment drill failed"
  remote "$healthy_host" canary "$healthy_label" "$RUN_ID" "$CANARY_SECONDS" \
    > "$RUN_DIR/host-$healthy_label/$name-canary.json" 2> "$RUN_DIR/host-$healthy_label/$name-canary.stderr" &
  canary_pid=$!
  ACTIVE_PIDS=("$canary_pid")
  sleep 2
  remote "$victim_host" crash-service "$victim_label" "$RUN_ID" "$service" \
    > "$RUN_DIR/host-$victim_label/$name-fault.json" 2> "$RUN_DIR/host-$victim_label/$name-fault.stderr" || fault_status=$?
  wait "$canary_pid" || canary_status=$?
  ACTIVE_PIDS=()
  (( fault_status == 0 && canary_status == 0 )) || fail "$name crash or healthy-peer canary failed"
  jq -e '.outcome == "passed" and .fixture.state == "running" and .fixture.marker_preserved == true' "$RUN_DIR/host-$victim_label/$name-fault.json" >/dev/null
  jq -e '.outcome == "passed" and .attempts > 0 and .failed == 0 and .succeeded == .attempts' "$RUN_DIR/host-$healthy_label/$name-canary.json" >/dev/null
  local canary_start canary_finish fault_start fault_finish
  canary_start=$(jq -er '.started_epoch_ms' "$RUN_DIR/host-$healthy_label/$name-canary.json")
  canary_finish=$(jq -er '.finished_epoch_ms' "$RUN_DIR/host-$healthy_label/$name-canary.json")
  fault_start=$(jq -er '.fault_started_epoch_ms' "$RUN_DIR/host-$victim_label/$name-fault.json")
  fault_finish=$(jq -er '.finished_epoch_ms' "$RUN_DIR/host-$victim_label/$name-fault.json")
  (( canary_start <= fault_start && canary_finish >= fault_finish )) || fail "$name fault window was not contained inside the healthy-peer canary window"
}

run_fault_drill "$HOST_A" a "$HOST_B" b brezeld controller-crash-a
run_fault_drill "$HOST_B" b "$HOST_A" a brezel-node node-crash-b

FAILURE_REASON='fixture cleanup failed'
cleanup_fixture_best_effort "$HOST_A" "$RUN_DIR/host-a/cleanup.json" || fail "host A fixture cleanup failed"
FIXTURE_A=false
cleanup_fixture_best_effort "$HOST_B" "$RUN_DIR/host-b/cleanup.json" || fail "host B fixture cleanup failed"
FIXTURE_B=false
jq -e '.outcome == "passed"' "$RUN_DIR/host-a/cleanup.json" >/dev/null
jq -e '.outcome == "passed"' "$RUN_DIR/host-b/cleanup.json" >/dev/null

FAILURE_REASON='evidence assembly failed'
jq -n \
  --arg run_id "$RUN_ID" \
  --arg started_at "$(jq -r '.captured_at' "$RUN_DIR/host-a/preflight.json")" \
  --arg finished_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg revision "$REVISION_A" --arg engine_revision "$ENGINE_A" \
  --slurpfile host_a "$RUN_DIR/host-a/preflight.json" \
  --slurpfile host_b "$RUN_DIR/host-b/preflight.json" \
  --slurpfile conformance_a "$RUN_DIR/host-a/conformance.json" \
  --slurpfile conformance_b "$RUN_DIR/host-b/conformance.json" \
  --slurpfile tti_a "$RUN_DIR/host-a/tti-burst.json" \
  --slurpfile tti_b "$RUN_DIR/host-b/tti-burst.json" \
  --slurpfile restore_a "$RUN_DIR/host-a/filesystem-restore-burst.json" \
  --slurpfile restore_b "$RUN_DIR/host-b/filesystem-restore-burst.json" \
  --slurpfile workspace_a "$RUN_DIR/host-a/workspace-io-burst.json" \
  --slurpfile workspace_b "$RUN_DIR/host-b/workspace-io-burst.json" \
  --slurpfile isolation_a "$RUN_DIR/host-a/isolation.json" \
  --slurpfile isolation_b "$RUN_DIR/host-b/isolation.json" \
  --slurpfile controller_fault "$RUN_DIR/host-a/controller-crash-a-fault.json" \
  --slurpfile controller_peer "$RUN_DIR/host-b/controller-crash-a-canary.json" \
  --slurpfile node_fault "$RUN_DIR/host-b/node-crash-b-fault.json" \
  --slurpfile node_peer "$RUN_DIR/host-a/node-crash-b-canary.json" \
  '{schema_version:1,run_id:$run_id,qualification:"dual_independent_single_host_conformant",claim_scope:"simultaneous operation, independent resource namespaces, and controller/node-relay crash containment across two separately administered single-host runtimes",excluded_claims:["cluster","shared control plane","multi-node scheduler","automatic placement","automatic failover","cross-host restore","replicated storage","high availability"],started_at:$started_at,finished_at:$finished_at,release:{brezel_revision:$revision,engine_revision:$engine_revision,clean:true},hosts:[$host_a[0],$host_b[0]],simultaneous_conformance:[$conformance_a[0],$conformance_b[0]],simultaneous_benchmarks:{tti:[$tti_a[0],$tti_b[0]],filesystem_restore:[$restore_a[0],$restore_b[0]],workspace_io:[$workspace_a[0],$workspace_b[0]]},namespace_isolation:[$isolation_a[0],$isolation_b[0]],failure_containment:[{fault:$controller_fault[0],healthy_peer:$controller_peer[0]},{fault:$node_fault[0],healthy_peer:$node_peer[0]}],outcome:"passed"}' \
  > "$RUN_DIR/summary.json"
chmod 600 "$RUN_DIR/summary.json"
jq -e '.qualification == "dual_independent_single_host_conformant" and .outcome == "passed" and (.excluded_claims | index("high availability")) != null and (.excluded_claims | index("automatic failover")) != null' "$RUN_DIR/summary.json" >/dev/null
printf '%s\n' passed > "$RUN_DIR/STATUS"
seal_evidence
FINALIZED=true
trap - EXIT HUP INT TERM
printf 'Dual independent-host qualification passed. Sealed evidence: %s\n' "$RUN_DIR"
