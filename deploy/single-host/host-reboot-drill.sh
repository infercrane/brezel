#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
INSTALL_DIR=${BREZEL_INSTALL_DIR:-"$REPO_DIR/.brezel"}
PENDING_FILE="$INSTALL_DIR/qualification/.host-reboot-drill.json"
RUNTIME_ATTESTATION="$SCRIPT_DIR/runtime-attestation.sh"
RUNTIME_ATTESTATION_MANIFEST="$INSTALL_DIR/runtime-attestation.manifest"
BOOT_ID_FILE=/proc/sys/kernel/random/boot_id

export BREZEL_STATE_DIR="$INSTALL_DIR/state"
export BREZEL_SECRETS_DIR="$INSTALL_DIR/secrets"
export BREZEL_UID="$(id -u)"
export BREZEL_GID="$(id -g)"

ACTION=
DECLARED_RESET_METHOD=${BREZEL_HOST_REBOOT_RESET_METHOD:-}
REQUESTED_RESET_METHOD=
RESET_METHOD_STATUS=not_run
REPORT_TARGET=
REPORT_PATH=
STARTED_AT=
STARTED_EPOCH=0
FINISHED_AT=
DURATION_SECONDS=0
BOOT_ID_BEFORE=
BOOT_ID_AFTER=
BOOT_TRANSITION_STATUS=not_run
RUNTIME_ATTESTATION_BEFORE=null
RUNTIME_ATTESTATION_AFTER=null
RUNTIME_IDENTITY_STATUS=not_run
OBSERVED_STATE=not_observed
RECOVERY_MODE=not_attempted
ORIGINAL_SANDBOX_ID=
RECOVERY_SANDBOX_ID=
WORKSPACE_ID=
EXPECTED_SHA256=
ACTUAL_SHA256=
CORPUS_MANIFEST_BEFORE=null
CORPUS_MANIFEST_AFTER=null
CORPUS_STATUS=not_run
STATE_OBSERVATION_STATUS=not_run
MARKER_STATUS=not_run
CLEANUP_STATUS=not_run

usage() {
  cat >&2 <<EOF
usage: $0 prepare --reset-method METHOD
       $0 verify [--reset-method METHOD]

METHOD is an operator-declared, evidence-safe token such as
gcp-compute-reset, provider-power-cycle, or graceful-reboot. It can also be
provided with BREZEL_HOST_REBOOT_RESET_METHOD.
EOF
  exit 2
}

parse_args() {
  [ "$#" -gt 0 ] || usage
  ACTION=$1
  shift
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --reset-method)
        [ "$#" -ge 2 ] || usage
        [ -z "$DECLARED_RESET_METHOD" ] || {
          echo "reset method was provided more than once" >&2
          exit 2
        }
        DECLARED_RESET_METHOD=$2
        shift 2
        ;;
      *) usage ;;
    esac
  done
  case "$ACTION" in
    prepare|verify) ;;
    *) usage ;;
  esac
  if [ -n "$DECLARED_RESET_METHOD" ]; then
    case "$DECLARED_RESET_METHOD" in
      *[!A-Za-z0-9._-]*|"")
        echo "reset method may contain only letters, numbers, dots, underscores, and hyphens" >&2
        exit 2
        ;;
    esac
  fi
  if [ "$ACTION" = prepare ] && [ -z "$DECLARED_RESET_METHOD" ]; then
    echo "prepare requires --reset-method or BREZEL_HOST_REBOOT_RESET_METHOD" >&2
    exit 2
  fi
  REQUESTED_RESET_METHOD=$DECLARED_RESET_METHOD
}

compose() {
  docker compose -f "$SCRIPT_DIR/compose.yaml" "$@"
}

cli() {
  compose exec -T brezeld \
    /usr/local/bin/brezel \
      -url http://127.0.0.1:8080 \
      -token-file /run/brezel-secrets/service.token \
      -project brezel-conformance "$@"
}

create_crash_consistency_corpus() {
  corpus_sandbox_id=$1
  corpus_marker=$2
  cli exec --env "BREZEL_CORPUS_MARKER=$corpus_marker" "$corpus_sandbox_id" /bin/sh -c '
    set -eu
    root=/workspace/host-reboot-corpus
    mkdir -p "$root/nested"

    printf "%s" "$BREZEL_CORPUS_MARKER" > "$root/small.txt"

    printf "before:%s" "$BREZEL_CORPUS_MARKER" > "$root/overwrite.txt"
    printf "after:%s" "$BREZEL_CORPUS_MARKER" > "$root/overwrite.txt"

    printf "truncate:%s:discarded-tail" "$BREZEL_CORPUS_MARKER" > "$root/truncate.txt"
    truncate -s 17 "$root/truncate.txt"

    printf "rename:%s" "$BREZEL_CORPUS_MARKER" > "$root/atomic-rename.pending"
    mv "$root/atomic-rename.pending" "$root/atomic-rename.txt"

    printf "nested:%s" "$BREZEL_CORPUS_MARKER" > "$root/nested/path.txt"

    dd if=/dev/zero of="$root/payload-1m.bin" bs=1048576 count=1 2>/dev/null
    printf "%s" "$BREZEL_CORPUS_MARKER" | dd of="$root/payload-1m.bin" bs=64 count=1 conv=notrunc 2>/dev/null

    sync
  ' >/dev/null
}

crash_consistency_manifest() {
  manifest_sandbox_id=$1
  raw_manifest=$(cli exec "$manifest_sandbox_id" /bin/sh -c '
    set -eu
    root=/workspace/host-reboot-corpus
    [ ! -e "$root/atomic-rename.pending" ]
    for path in atomic-rename.txt nested/path.txt overwrite.txt payload-1m.bin small.txt truncate.txt; do
      file="$root/$path"
      [ -f "$file" ] && [ ! -L "$file" ]
      digest=$(sha256sum "$file")
      digest=${digest%% *}
      size=$(wc -c < "$file" | tr -d " ")
      printf "%s\t%s\t%s\n" "$path" "$digest" "$size"
    done
  ') || return 1

  manifest=$(printf '%s\n' "$raw_manifest" | jq -Rce '
    [
      split("\n")[]
      | select(length > 0)
      | split("\t")
      | if length == 3 and
           (.[0] | test("^[a-z0-9./-]+$")) and
           (.[1] | test("^[0-9a-f]{64}$")) and
           (.[2] | test("^[0-9]+$"))
        then {path:.[0],sha256:.[1],size_bytes:(.[2] | tonumber)}
        else error("invalid crash-consistency manifest row")
        end
    ]
    | sort_by(.path)
  ') || return 1
  printf '%s' "$manifest" | jq -e '
    length == 6 and
    map(.path) == [
      "atomic-rename.txt",
      "nested/path.txt",
      "overwrite.txt",
      "payload-1m.bin",
      "small.txt",
      "truncate.txt"
    ] and
    all(.[]; (.sha256 | test("^[0-9a-f]{64}$")) and (.size_bytes >= 0)) and
    (map(select(.path == "atomic-rename.txt"))[0].size_bytes == 71) and
    (map(select(.path == "nested/path.txt"))[0].size_bytes == 71) and
    (map(select(.path == "overwrite.txt"))[0].size_bytes == 70) and
    (map(select(.path == "payload-1m.bin"))[0].size_bytes == 1048576) and
    (map(select(.path == "small.txt"))[0].size_bytes == 64) and
    (map(select(.path == "truncate.txt"))[0].size_bytes == 17)
  ' >/dev/null || return 1
  printf '%s\n' "$manifest"
}

current_boot_id() {
  [ -r "$BOOT_ID_FILE" ] || return 1
  boot_id=$(tr -d '\r\n' < "$BOOT_ID_FILE" | tr 'A-F' 'a-f')
  printf '%s\n' "$boot_id" | grep -Eq \
    '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' || return 1
  printf '%s\n' "$boot_id"
}

wait_ready() {
  attempts=0
  while [ "$attempts" -lt 90 ]; do
    if compose exec -T brezeld wget -qO- http://127.0.0.1:8080/readyz >/dev/null 2>&1; then
      return 0
    fi
    attempts=$((attempts + 1))
    sleep 2
  done
  return 1
}

resolve_report() {
  REPORT_TARGET=${BREZEL_HOST_REBOOT_TARGET:-"developer-single-host-$(hostname)-host-reboot-$(date -u +%Y%m%dT%H%M%SZ)"}
  case "$REPORT_TARGET" in
    ""|*[!A-Za-z0-9._-]*)
      echo "BREZEL_HOST_REBOOT_TARGET may contain only letters, numbers, dots, underscores, and hyphens" >&2
      return 1
      ;;
  esac
  umask 077
  mkdir -p "$INSTALL_DIR/qualification"
  REPORT_PATH="$INSTALL_DIR/qualification/$REPORT_TARGET.json"
  if [ -e "$REPORT_PATH" ]; then
    echo "host-reboot report already exists: $REPORT_PATH" >&2
    return 1
  fi
}

write_report() {
  outcome=$1
  qualification=$2
  failure_code=$3
  failure_message=$4
  FINISHED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  now_epoch=$(date +%s)
  if [ "$STARTED_EPOCH" -gt 0 ] && [ "$now_epoch" -ge "$STARTED_EPOCH" ]; then
    DURATION_SECONDS=$((now_epoch - STARTED_EPOCH))
  else
    DURATION_SECONDS=0
  fi
  temporary=$(mktemp "$INSTALL_DIR/qualification/.host-reboot-report.XXXXXX")
  if ! jq -n \
    --arg target "$REPORT_TARGET" \
    --arg outcome "$outcome" \
    --arg qualification "$qualification" \
    --arg failure_code "$failure_code" \
    --arg failure_message "$failure_message" \
    --arg reset_method "${DECLARED_RESET_METHOD:-not-recorded}" \
    --arg requested_reset_method "$REQUESTED_RESET_METHOD" \
    --arg reset_method_status "$RESET_METHOD_STATUS" \
    --arg started_at "$STARTED_AT" \
    --arg finished_at "$FINISHED_AT" \
    --arg boot_id_before "$BOOT_ID_BEFORE" \
    --arg boot_id_after "$BOOT_ID_AFTER" \
    --arg boot_transition_status "$BOOT_TRANSITION_STATUS" \
    --arg runtime_identity_status "$RUNTIME_IDENTITY_STATUS" \
    --argjson runtime_attestation_before "$RUNTIME_ATTESTATION_BEFORE" \
    --argjson runtime_attestation_after "$RUNTIME_ATTESTATION_AFTER" \
    --arg original_sandbox_id "$ORIGINAL_SANDBOX_ID" \
    --arg recovery_sandbox_id "$RECOVERY_SANDBOX_ID" \
    --arg workspace_id "$WORKSPACE_ID" \
    --arg observed_state "$OBSERVED_STATE" \
    --arg recovery_mode "$RECOVERY_MODE" \
    --arg state_observation_status "$STATE_OBSERVATION_STATUS" \
    --arg marker_status "$MARKER_STATUS" \
    --arg expected_sha256 "$EXPECTED_SHA256" \
    --arg actual_sha256 "$ACTUAL_SHA256" \
    --arg corpus_status "$CORPUS_STATUS" \
    --argjson corpus_manifest_before "$CORPUS_MANIFEST_BEFORE" \
    --argjson corpus_manifest_after "$CORPUS_MANIFEST_AFTER" \
    --arg cleanup_status "$CLEANUP_STATUS" \
    --argjson duration_seconds "$DURATION_SECONDS" \
    '{
      schema_version:3,
      target:$target,
      scope:"full host reboot with active sandbox and durable workspace",
      qualification:$qualification,
      outcome:$outcome,
      failure:(if $failure_code == "" then null else {code:$failure_code,message:$failure_message} end),
      started_at:(if $started_at == "" then null else $started_at end),
      finished_at:$finished_at,
      duration_seconds:$duration_seconds,
      fault_injection:{
        reset_method:$reset_method,
        verify_argument:(if $requested_reset_method == "" then null else $requested_reset_method end),
        status:$reset_method_status
      },
      host:{
        boot_id_before:(if $boot_id_before == "" then null else $boot_id_before end),
        boot_id_after:(if $boot_id_after == "" then null else $boot_id_after end),
        changed:($boot_transition_status == "passed")
      },
      runtime_identity:{
        status:$runtime_identity_status,
        before:$runtime_attestation_before,
        after:$runtime_attestation_after
      },
      resources:{
        original_sandbox_id:(if $original_sandbox_id == "" then null else $original_sandbox_id end),
        recovery_sandbox_id:(if $recovery_sandbox_id == "" then null else $recovery_sandbox_id end),
        workspace_id:(if $workspace_id == "" then null else $workspace_id end)
      },
      observed_sandbox_state:$observed_state,
      recovery_mode:$recovery_mode,
      marker:{
        expected_sha256:(if $expected_sha256 == "" then null else $expected_sha256 end),
        actual_sha256:(if $actual_sha256 == "" then null else $actual_sha256 end)
      },
      crash_consistency_corpus:{
        status:$corpus_status,
        expected:$corpus_manifest_before,
        actual:$corpus_manifest_after
      },
      steps:[
        {name:"host_boot_identity_changed",status:$boot_transition_status},
        {name:"runtime_identity_unchanged",status:$runtime_identity_status},
        {name:"honest_post_reboot_state",status:$state_observation_status},
        {name:"durable_workspace_marker",status:$marker_status},
        {name:"crash_consistency_corpus",status:$corpus_status},
        {name:"confirmed_cleanup",status:$cleanup_status}
      ]
    }' > "$temporary"; then
    rm -f "$temporary"
    return 1
  fi
  chmod 600 "$temporary"
  mv "$temporary" "$REPORT_PATH"
}

report_failure() {
  failure_code=$1
  failure_message=$2
  if write_report failed host_reboot_workspace_recovery_not_conformant "$failure_code" "$failure_message"; then
    echo "Host-reboot workspace recovery failure report: $REPORT_PATH" >&2
  else
    echo "could not write host-reboot failure report to $REPORT_PATH" >&2
  fi
  echo "$failure_message" >&2
  return 1
}

verify_runtime_attestation() {
  "$RUNTIME_ATTESTATION" verify "$RUNTIME_ATTESTATION_MANIFEST" \
    "$REPO_DIR" "$SCRIPT_DIR/compose.yaml"
}

prepare() {
  if [ -e "$PENDING_FILE" ]; then
    echo "a host-reboot drill is already pending: $PENDING_FILE" >&2
    return 1
  fi
  BOOT_ID_BEFORE=$(current_boot_id) || {
    echo "host boot identity is unavailable or malformed: $BOOT_ID_FILE" >&2
    return 1
  }
  RUNTIME_ATTESTATION_BEFORE=$(verify_runtime_attestation) || {
    echo "running runtime identity could not be verified before the reboot drill" >&2
    return 1
  }
  umask 077
  mkdir -p "$INSTALL_DIR/qualification"
  workspace_id=
  sandbox_id=
  cleanup_prepare() {
    set +e
    if [ -n "$sandbox_id" ]; then
      cli sandbox delete "$sandbox_id" >/dev/null 2>&1
    fi
    if [ -n "$workspace_id" ]; then
      cli workspace delete "$workspace_id" >/dev/null 2>&1
    fi
  }
  trap cleanup_prepare EXIT HUP INT TERM
  workspace_id=$(cli workspace create host-reboot-recovery | awk 'NR == 1 {print $1}')
  # The embedded engine accepts sandbox lifetimes up to one hour. The drill only
  # needs the sandbox to survive a host reboot, so keep the request inside that
  # enforced boundary rather than relying on an unsupported lease.
  sandbox_id=$(cli new --template base --workspace "$workspace_id:/workspace" --ttl 3600 | awk 'NR == 1 {print $1}')
  marker=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
  marker_sha256=$(printf %s "$marker" | sha256sum | awk '{print $1}')
  create_crash_consistency_corpus "$sandbox_id" "$marker"
  CORPUS_MANIFEST_BEFORE=$(crash_consistency_manifest "$sandbox_id") || {
    echo "crash-consistency corpus could not be verified before host reset" >&2
    return 1
  }
  observed_before_sha256=$(printf '%s' "$CORPUS_MANIFEST_BEFORE" | jq -er '.[] | select(.path == "small.txt") | .sha256')
  [ "$observed_before_sha256" = "$marker_sha256" ] || {
    echo "run-specific corpus marker could not be verified before host reset" >&2
    return 1
  }
  boot_id_before_commit=$(current_boot_id) || {
    echo "host boot identity became unavailable while preparing the drill" >&2
    return 1
  }
  [ "$boot_id_before_commit" = "$BOOT_ID_BEFORE" ] || {
    echo "host restarted while preparing the drill; no pending evidence was committed" >&2
    return 1
  }
  started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  started_epoch=$(date +%s)
  temporary=$(mktemp "$INSTALL_DIR/qualification/.host-reboot-drill.XXXXXX")
  jq -n \
    --arg sandbox_id "$sandbox_id" \
    --arg workspace_id "$workspace_id" \
    --arg marker_sha256 "$marker_sha256" \
    --arg started_at "$started_at" \
    --arg boot_id_before "$BOOT_ID_BEFORE" \
    --arg reset_method "$DECLARED_RESET_METHOD" \
    --argjson runtime_attestation "$RUNTIME_ATTESTATION_BEFORE" \
    --argjson crash_consistency_manifest "$CORPUS_MANIFEST_BEFORE" \
    --argjson started_epoch "$started_epoch" \
    '{schema_version:3,sandbox_id:$sandbox_id,workspace_id:$workspace_id,marker_sha256:$marker_sha256,crash_consistency_manifest:$crash_consistency_manifest,started_at:$started_at,started_epoch:$started_epoch,host:{boot_id_before:$boot_id_before},fault_injection:{reset_method:$reset_method},runtime_attestation:$runtime_attestation}' \
    > "$temporary"
  chmod 600 "$temporary"
  mv "$temporary" "$PENDING_FILE"
  trap - EXIT HUP INT TERM
  echo "Host-reboot drill prepared with reset method: $DECLARED_RESET_METHOD"
  echo "Apply that host reset, wait for SSH, then run:"
  echo "  ./deploy/single-host/host-reboot-drill.sh verify"
}

load_pending_state() {
  jq -e '
    .schema_version == 3 and
    (.sandbox_id | type == "string" and startswith("sbx_")) and
    (.workspace_id | type == "string" and startswith("wrk_")) and
    (.marker_sha256 | test("^[0-9a-f]{64}$")) and
    (.started_at | type == "string" and length > 0) and
    (.started_epoch | type == "number" and . > 0) and
    (.host.boot_id_before | test("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")) and
    (.fault_injection.reset_method | test("^[A-Za-z0-9._-]+$")) and
    (.runtime_attestation.schema_version == 1) and
    (.runtime_attestation.format == "brezel-runtime-attestation-v1") and
    (.runtime_attestation.source.revision | test("^[0-9a-f]{40}$")) and
    (.runtime_attestation.source.clean == true) and
    (.runtime_attestation.verified_running == true) and
    (.crash_consistency_manifest |
      type == "array" and
      length == 6 and
      map(.path) == ["atomic-rename.txt","nested/path.txt","overwrite.txt","payload-1m.bin","small.txt","truncate.txt"] and
      all(.[];
        (.path | type == "string") and
        (.sha256 | test("^[0-9a-f]{64}$")) and
        (.size_bytes | type == "number" and . >= 0)
      ) and
      (map(select(.path == "atomic-rename.txt"))[0].size_bytes == 71) and
      (map(select(.path == "nested/path.txt"))[0].size_bytes == 71) and
      (map(select(.path == "overwrite.txt"))[0].size_bytes == 70) and
      (map(select(.path == "payload-1m.bin"))[0].size_bytes == 1048576) and
      (map(select(.path == "small.txt"))[0].size_bytes == 64) and
      (map(select(.path == "truncate.txt"))[0].size_bytes == 17)
    )
  ' "$PENDING_FILE" >/dev/null
}

verify() {
  resolve_report || return 1
  if [ ! -s "$PENDING_FILE" ]; then
    STARTED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    STARTED_EPOCH=$(date +%s)
    BOOT_ID_AFTER=$(current_boot_id 2>/dev/null || true)
    report_failure pending_state_missing "host-reboot drill state is missing; run prepare first"
    return 1
  fi
  if ! load_pending_state; then
    STARTED_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    STARTED_EPOCH=$(date +%s)
    BOOT_ID_AFTER=$(current_boot_id 2>/dev/null || true)
    report_failure pending_state_invalid "host-reboot drill state is malformed or from an unsupported schema"
    return 1
  fi

  ORIGINAL_SANDBOX_ID=$(jq -er '.sandbox_id' "$PENDING_FILE")
  RECOVERY_SANDBOX_ID=$ORIGINAL_SANDBOX_ID
  WORKSPACE_ID=$(jq -er '.workspace_id' "$PENDING_FILE")
  EXPECTED_SHA256=$(jq -er '.marker_sha256' "$PENDING_FILE")
  CORPUS_MANIFEST_BEFORE=$(jq -c '.crash_consistency_manifest' "$PENDING_FILE")
  STARTED_AT=$(jq -er '.started_at' "$PENDING_FILE")
  STARTED_EPOCH=$(jq -er '.started_epoch' "$PENDING_FILE")
  BOOT_ID_BEFORE=$(jq -er '.host.boot_id_before' "$PENDING_FILE")
  pending_reset_method=$(jq -er '.fault_injection.reset_method' "$PENDING_FILE")
  RUNTIME_ATTESTATION_BEFORE=$(jq -c '.runtime_attestation' "$PENDING_FILE")
  DECLARED_RESET_METHOD=$pending_reset_method
  if [ -n "$REQUESTED_RESET_METHOD" ] && [ "$REQUESTED_RESET_METHOD" != "$pending_reset_method" ]; then
    RESET_METHOD_STATUS=failed
    report_failure reset_method_mismatch "verify reset method does not match the method recorded by prepare"
    return 1
  fi
  RESET_METHOD_STATUS=passed

  BOOT_ID_AFTER=$(current_boot_id 2>/dev/null || true)
  if [ -z "$BOOT_ID_AFTER" ]; then
    BOOT_TRANSITION_STATUS=failed
    report_failure boot_identity_unavailable "post-reset host boot identity is unavailable or malformed"
    return 1
  fi
  if [ "$BOOT_ID_AFTER" = "$BOOT_ID_BEFORE" ]; then
    BOOT_TRANSITION_STATUS=failed
    report_failure host_not_rebooted "host boot identity did not change; no reboot was proven"
    return 1
  fi
  BOOT_TRANSITION_STATUS=passed

  if ! wait_ready; then
    report_failure runtime_not_ready "runtime API did not become ready after host restart"
    return 1
  fi
  if ! RUNTIME_ATTESTATION_AFTER=$(verify_runtime_attestation); then
    RUNTIME_IDENTITY_STATUS=failed
    RUNTIME_ATTESTATION_AFTER=null
    report_failure runtime_identity_unverified "running runtime identity could not be verified after host restart"
    return 1
  fi
  if ! jq -en --argjson before "$RUNTIME_ATTESTATION_BEFORE" --argjson after "$RUNTIME_ATTESTATION_AFTER" '$before == $after' >/dev/null; then
    RUNTIME_IDENTITY_STATUS=failed
    report_failure runtime_identity_changed "running source or service identity changed across host restart"
    return 1
  fi
  RUNTIME_IDENTITY_STATUS=passed

  if ! observed=$(cli sandbox inspect "$ORIGINAL_SANDBOX_ID"); then
    STATE_OBSERVATION_STATUS=failed
    report_failure sandbox_state_unavailable "original sandbox state could not be observed after host restart"
    return 1
  fi
  if ! OBSERVED_STATE=$(printf %s "$observed" | jq -er '.state | select(type == "string" and length > 0)'); then
    STATE_OBSERVATION_STATUS=failed
    OBSERVED_STATE=invalid
    report_failure sandbox_state_invalid "original sandbox returned an invalid post-reboot state"
    return 1
  fi
  STATE_OBSERVATION_STATUS=passed
  case "$OBSERVED_STATE" in
    running)
      RECOVERY_MODE=transparent_sandbox_and_workspace
      ;;
    failed|deleted)
      RECOVERY_MODE=workspace_replacement
      # Preserve the immutable failed receipt. A deleted sandbox has no live
      # resource to remove. Only the new, bounded-lifetime sandbox is cleaned
      # below after marker verification.
      if ! RECOVERY_SANDBOX_ID=$(cli new --template base --workspace "$WORKSPACE_ID:/workspace" --ttl 600 | awk 'NR == 1 {print $1}'); then
        report_failure recovery_sandbox_create_failed "replacement sandbox could not attach the durable workspace"
        return 1
      fi
      ;;
    *)
      RECOVERY_MODE=unsupported_post_reboot_state
      report_failure unexpected_sandbox_state "original sandbox entered an unsupported post-reboot state"
      return 1
      ;;
  esac

  if ! CORPUS_MANIFEST_AFTER=$(crash_consistency_manifest "$RECOVERY_SANDBOX_ID"); then
    CORPUS_MANIFEST_AFTER=null
    CORPUS_STATUS=failed
    MARKER_STATUS=failed
    report_failure crash_consistency_corpus_missing "crash-consistency corpus is missing, unreadable, or malformed after host restart"
    return 1
  fi
  ACTUAL_SHA256=$(printf '%s' "$CORPUS_MANIFEST_AFTER" | jq -er '.[] | select(.path == "small.txt") | .sha256')
  if [ "$ACTUAL_SHA256" != "$EXPECTED_SHA256" ]; then
    CORPUS_STATUS=failed
    MARKER_STATUS=failed
    report_failure durable_marker_changed "durable workspace marker changed across host reboot"
    return 1
  fi
  MARKER_STATUS=passed
  if ! jq -en --argjson before "$CORPUS_MANIFEST_BEFORE" --argjson after "$CORPUS_MANIFEST_AFTER" '$before == $after' >/dev/null; then
    CORPUS_STATUS=failed
    report_failure crash_consistency_corpus_changed "crash-consistency corpus changed across host reboot"
    return 1
  fi
  CORPUS_STATUS=passed

  if ! cli sandbox delete "$RECOVERY_SANDBOX_ID" >/dev/null; then
    CLEANUP_STATUS=failed
    report_failure sandbox_cleanup_failed "recovery sandbox cleanup could not be confirmed"
    return 1
  fi
  if ! cli workspace delete "$WORKSPACE_ID" >/dev/null; then
    CLEANUP_STATUS=failed
    report_failure workspace_cleanup_failed "durable workspace cleanup could not be confirmed"
    return 1
  fi
  CLEANUP_STATUS=passed

  write_report passed host_reboot_workspace_recovery_conformant "" ""
  rm -f "$PENDING_FILE"
  echo "Host-reboot workspace recovery report: $REPORT_PATH"
}

parse_args "$@"
case "$ACTION" in
  prepare) prepare ;;
  verify) verify ;;
esac
