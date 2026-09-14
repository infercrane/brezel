#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
INSTALL_DIR=${RUNTIME_INSTALL_DIR:-"$REPO_DIR/.runtime"}
PENDING_FILE="$INSTALL_DIR/qualification/.host-reboot-drill.json"

export RUNTIME_STATE_DIR="$INSTALL_DIR/state"
export RUNTIME_SECRETS_DIR="$INSTALL_DIR/secrets"
export RUNTIME_UID="$(id -u)"
export RUNTIME_GID="$(id -g)"

compose() {
  docker compose -f "$SCRIPT_DIR/compose.yaml" "$@"
}

cli() {
  compose exec -T runtime-api \
    /usr/local/bin/runtimectl \
      -url http://127.0.0.1:8080 \
      -token-file /run/runtime-secrets/service.token \
      -project runtime-conformance "$@"
}

wait_ready() {
  attempts=0
  while [ "$attempts" -lt 90 ]; do
    if compose exec -T runtime-api wget -qO- http://127.0.0.1:8080/readyz >/dev/null 2>&1; then
      return 0
    fi
    attempts=$((attempts + 1))
    sleep 2
  done
  echo "runtime API did not become ready after host restart" >&2
  return 1
}

prepare() {
  if [ -e "$PENDING_FILE" ]; then
    echo "a host-reboot drill is already pending: $PENDING_FILE" >&2
    return 1
  fi
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
  sandbox_id=$(cli new --template base --workspace "$workspace_id:/workspace" --ttl 7200 | awk 'NR == 1 {print $1}')
  marker=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
  marker_sha256=$(printf %s "$marker" | sha256sum | awk '{print $1}')
  cli exec "$sandbox_id" /bin/sh -lc 'printf %s "$1" > /workspace/host-reboot-drill.txt && sync' runtime-host-reboot "$marker" >/dev/null
  started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  started_epoch=$(date +%s)
  temporary=$(mktemp "$INSTALL_DIR/qualification/.host-reboot-drill.XXXXXX")
  jq -n \
    --arg sandbox_id "$sandbox_id" \
    --arg workspace_id "$workspace_id" \
    --arg marker_sha256 "$marker_sha256" \
    --arg started_at "$started_at" \
    --argjson started_epoch "$started_epoch" \
    '{sandbox_id:$sandbox_id,workspace_id:$workspace_id,marker_sha256:$marker_sha256,started_at:$started_at,started_epoch:$started_epoch}' \
    > "$temporary"
  chmod 600 "$temporary"
  mv "$temporary" "$PENDING_FILE"
  trap - EXIT HUP INT TERM
  echo "Host-reboot drill prepared. Reboot the host, wait for SSH, then run:"
  echo "  ./deploy/single-host/host-reboot-drill.sh verify"
}

verify() {
  if [ ! -s "$PENDING_FILE" ]; then
    echo "host-reboot drill state is missing; run prepare first" >&2
    return 1
  fi
  wait_ready
  sandbox_id=$(jq -er '.sandbox_id' "$PENDING_FILE")
  workspace_id=$(jq -er '.workspace_id' "$PENDING_FILE")
  expected_sha256=$(jq -er '.marker_sha256' "$PENDING_FILE")
  started_at=$(jq -er '.started_at' "$PENDING_FILE")
  started_epoch=$(jq -er '.started_epoch' "$PENDING_FILE")
  observed=$(cli sandbox inspect "$sandbox_id")
  observed_state=$(printf %s "$observed" | jq -er '.state')
  recovery_mode=transparent_sandbox_and_workspace
  recovery_sandbox_id=$sandbox_id

  if [ "$observed_state" != "running" ]; then
    recovery_mode=workspace_replacement
    cli sandbox delete "$sandbox_id" >/dev/null
    recovery_sandbox_id=$(cli new --template base --workspace "$workspace_id:/workspace" --ttl 600 | awk 'NR == 1 {print $1}')
  fi

  marker=$(cli exec "$recovery_sandbox_id" /bin/cat /workspace/host-reboot-drill.txt)
  actual_sha256=$(printf %s "$marker" | sha256sum | awk '{print $1}')
  if [ "$actual_sha256" != "$expected_sha256" ]; then
    echo "durable workspace marker changed across host reboot" >&2
    return 1
  fi

  cli sandbox delete "$recovery_sandbox_id" >/dev/null
  cli workspace delete "$workspace_id" >/dev/null
  finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  duration_seconds=$(( $(date +%s) - started_epoch ))
  target=${RUNTIME_HOST_REBOOT_TARGET:-"developer-single-host-$(hostname)-host-reboot-$(date -u +%Y%m%dT%H%M%SZ)"}
  case "$target" in
    ""|*[!A-Za-z0-9._-]*)
      echo "RUNTIME_HOST_REBOOT_TARGET may contain only letters, numbers, dots, underscores, and hyphens" >&2
      return 1
      ;;
  esac
  report="$INSTALL_DIR/qualification/$target.json"
  if [ -e "$report" ]; then
    echo "host-reboot report already exists: $report" >&2
    return 1
  fi
  temporary=$(mktemp "$INSTALL_DIR/qualification/.host-reboot-report.XXXXXX")
  jq -n \
    --arg target "$target" \
    --arg started_at "$started_at" \
    --arg finished_at "$finished_at" \
    --arg observed_state "$observed_state" \
    --arg recovery_mode "$recovery_mode" \
    --argjson duration_seconds "$duration_seconds" \
    '{target:$target,scope:"full host reboot with active sandbox and durable workspace",qualification:"host_reboot_workspace_recovery_conformant",started_at:$started_at,finished_at:$finished_at,duration_seconds:$duration_seconds,observed_sandbox_state:$observed_state,recovery_mode:$recovery_mode,steps:[{name:"honest_post_reboot_state",status:"passed"},{name:"durable_workspace_marker",status:"passed"},{name:"confirmed_cleanup",status:"passed"}]}' \
    > "$temporary"
  chmod 600 "$temporary"
  mv "$temporary" "$report"
  rm -f "$PENDING_FILE"
  echo "Host-reboot workspace recovery report: $report"
}

case "${1:-}" in
  prepare) prepare ;;
  verify) verify ;;
  *)
    echo "usage: $0 prepare|verify" >&2
    exit 2
    ;;
esac
