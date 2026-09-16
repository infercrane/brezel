#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
INSTALL_DIR=${BREZEL_INSTALL_DIR:-"$REPO_DIR/.brezel"}
TOKEN_FILE="$INSTALL_DIR/secrets/service.token"
TEMPLATE_REFERENCE_FILE="$INSTALL_DIR/artifacts/base-template.reference"
DISTRIBUTION_MANIFEST="$INSTALL_DIR/distribution.manifest"
QUALIFICATION_DIR="$INSTALL_DIR/qualification"
ENGINE_COMPOSE="$INSTALL_DIR/engine/embed/compose/compose.yaml"
ENGINE_ENV="$INSTALL_DIR/engine/embed/compose/.env"
ENGINE_CAPABILITY_PROBE="$SCRIPT_DIR/engine-capabilities.sh"
CAPACITY_PROBE="$SCRIPT_DIR/capacity-contract.sh"
ENGINE_CAPACITY_PROBE="$SCRIPT_DIR/engine-capacity-contract.sh"
MAX_ACTIVE_SANDBOXES_TOTAL=${BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL:-32}
MIN_READY_NETWORK_SLOTS=${BREZEL_MIN_READY_NETWORK_SLOTS:-32}
ENGINE_MAX_STARTING_SANDBOXES=${BREZEL_ENGINE_MAX_STARTING_SANDBOXES:-3}
ENGINE_NETWORK_NEW_SLOTS=${BREZEL_ENGINE_NETWORK_NEW_SLOTS:-32}
ENGINE_NETWORK_REUSED_SLOTS=${BREZEL_ENGINE_NETWORK_REUSED_SLOTS:-100}
ENGINE_NBD_POOL_SIZE=${BREZEL_ENGINE_NBD_POOL_SIZE:-64}
ENGINE_NBD_CONNECTIONS_PER_DEVICE=${BREZEL_ENGINE_NBD_CONNECTIONS_PER_DEVICE:-1}

if [ ! -s "$TOKEN_FILE" ]; then
  echo "runtime service token is missing; run install.sh first" >&2
  exit 1
fi
if [ ! -s "$TEMPLATE_REFERENCE_FILE" ]; then
  echo "immutable base-template reference is missing; run install.sh first" >&2
  exit 1
fi
IFS= read -r TEMPLATE_REFERENCE < "$TEMPLATE_REFERENCE_FILE"
printf '%s\n' "$TEMPLATE_REFERENCE" | grep -Eq '^[a-z0-9_-]+:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' || {
  echo "immutable base-template reference is invalid; run install.sh again" >&2
  exit 1
}
[ -s "$DISTRIBUTION_MANIFEST" ] || {
  echo "installed distribution manifest is missing; run install.sh again" >&2
  exit 1
}
INSTALLED_TEMPLATE_NAME=$(awk -F= '$1 == "artifact.template.name" { if (++count > 1) exit 2; print substr($0, index($0, "=") + 1) } END { if (count != 1) exit 1 }' "$DISTRIBUTION_MANIFEST") || {
  echo "installed distribution manifest has no unique base-template name" >&2
  exit 1
}
case "$INSTALLED_TEMPLATE_NAME" in
  ""|*[!a-z0-9_-]*|[-_]*)
    echo "installed distribution manifest has an invalid base-template name" >&2
    exit 1
    ;;
esac

TARGET=${BREZEL_CONFORMANCE_TARGET:-"developer-single-host-$(hostname)-$(date -u +%Y%m%dT%H%M%SZ)"}
case "$TARGET" in
  ""|*[!A-Za-z0-9._-]*)
    echo "BREZEL_CONFORMANCE_TARGET may contain only letters, numbers, dots, underscores, and hyphens" >&2
    exit 1
    ;;
esac

umask 077
mkdir -p "$QUALIFICATION_DIR"
chmod 700 "$QUALIFICATION_DIR"

export BREZEL_STATE_DIR="$INSTALL_DIR/state"
export BREZEL_SECRETS_DIR="$INSTALL_DIR/secrets"
export BREZEL_UID="$(id -u)"
export BREZEL_GID="$(id -g)"

compose() {
  docker compose -f "$SCRIPT_DIR/compose.yaml" "$@"
}

engine_compose() {
  docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" "$@"
}

cli() {
  compose exec -T brezeld \
    /usr/local/bin/brezel \
      -url http://127.0.0.1:8080 \
      -token-file /run/brezel-secrets/service.token \
      -project brezel-conformance "$@"
}

preflight_capacity_contract() {
  # This also validates the configured active limit before it is used in shell
  # arithmetic below.
  PREFLIGHT_CAPACITY_JSON=$("$CAPACITY_PROBE" live)
  case "$MIN_READY_NETWORK_SLOTS" in
    ""|*[!0-9]*)
      echo "BREZEL_MIN_READY_NETWORK_SLOTS must be a positive integer" >&2
      return 1
      ;;
  esac
  if [ "$MIN_READY_NETWORK_SLOTS" -lt "$MAX_ACTIVE_SANDBOXES_TOTAL" ]; then
    echo "BREZEL_MIN_READY_NETWORK_SLOTS cannot be lower than BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL" >&2
    return 1
  fi
  if [ "$MIN_READY_NETWORK_SLOTS" -gt "$ENGINE_NETWORK_NEW_SLOTS" ]; then
    echo "BREZEL_MIN_READY_NETWORK_SLOTS cannot exceed the configured $ENGINE_NETWORK_NEW_SLOTS-slot new-sandbox network pool" >&2
    return 1
  fi

  PREFLIGHT_ENGINE_CAPACITY_JSON=$(engine_compose exec -T \
    -e "BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL=$MAX_ACTIVE_SANDBOXES_TOTAL" \
    -e "BREZEL_GUEST_VCPUS=${BREZEL_GUEST_VCPUS:-2}" \
    -e "BREZEL_GUEST_MEMORY_MIB=${BREZEL_GUEST_MEMORY_MIB:-512}" \
    -e "BREZEL_GUEST_MIN_FREE_DISK_MIB=${BREZEL_GUEST_MIN_FREE_DISK_MIB:-512}" \
    -e "BREZEL_GUEST_MAX_FREE_DISK_MIB=${BREZEL_GUEST_MAX_FREE_DISK_MIB:-25600}" \
    -e "BREZEL_ENGINE_BASE_TEMPLATE_NAME=$INSTALLED_TEMPLATE_NAME" \
    postgres sh -s -- verify < "$ENGINE_CAPACITY_PROBE")
}

ACTIVE_SANDBOX_ID=
ACTIVE_WORKSPACE_ID=
cleanup_active_recovery() {
  set +e
  if [ -n "$ACTIVE_SANDBOX_ID" ]; then
    cli sandbox delete "$ACTIVE_SANDBOX_ID" >/dev/null 2>&1
  fi
  if [ -n "$ACTIVE_WORKSPACE_ID" ]; then
    cli workspace delete "$ACTIVE_WORKSPACE_ID" >/dev/null 2>&1
  fi
}
trap cleanup_active_recovery EXIT HUP INT TERM

run_conformance() {
  run_target=$1
  report_tmp=$(mktemp "$QUALIFICATION_DIR/.report.XXXXXX")
  if ! compose exec -T brezeld \
    /usr/local/bin/brezel-conformance \
      -base-url http://127.0.0.1:8080 \
      -backend-template "$TEMPLATE_REFERENCE" \
      -project brezel-conformance \
      -other-project brezel-conformance-isolation \
      -target "$run_target" \
      -timeout 10m \
      -execute > "$report_tmp"; then
    rm -f -- "$report_tmp"
    return 1
  fi
  chmod 600 "$report_tmp"
  if [ -e "$QUALIFICATION_DIR/$run_target.json" ]; then
    echo "qualification report already exists for $run_target" >&2
    rm -f -- "$report_tmp"
    return 1
  fi
  mv -- "$report_tmp" "$QUALIFICATION_DIR/$run_target.json"
}

wait_ready() {
  attempts=0
  while [ "$attempts" -lt 60 ]; do
    if compose exec -T brezeld wget -qO- http://127.0.0.1:8080/readyz >/dev/null 2>&1; then
      return 0
    fi
    attempts=$((attempts + 1))
    sleep 2
  done
  echo "runtime API did not become ready after restart" >&2
  return 1
}

run_active_recovery() {
  recovery_target=$1
  report_tmp=$(mktemp "$QUALIFICATION_DIR/.report.XXXXXX")
  recovery_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  recovery_started_ms=$(date +%s%3N)
  marker="controller-restart-$recovery_started_ms"

  ACTIVE_WORKSPACE_ID=$(cli workspace create controller-restart-recovery | awk 'NR == 1 {print $1}')
  if [ -z "$ACTIVE_WORKSPACE_ID" ]; then
    echo "active restart qualification did not return a workspace ID" >&2
    return 1
  fi
  ACTIVE_SANDBOX_ID=$(cli new --template "$TEMPLATE_REFERENCE" --workspace "$ACTIVE_WORKSPACE_ID:/workspace" --ttl 600 | awk 'NR == 1 {print $1}')
  if [ -z "$ACTIVE_SANDBOX_ID" ]; then
    echo "active restart qualification did not return a sandbox ID" >&2
    return 1
  fi
  cli exec "$ACTIVE_SANDBOX_ID" /bin/sh -c 'printf %s "$1" > /workspace/controller-restart.txt' runtime-recovery "$marker"

  compose restart brezeld >/dev/null
  wait_ready
  if ! cli sandbox inspect "$ACTIVE_SANDBOX_ID" | grep -q '"state": "running"'; then
    echo "active sandbox was not reconciled as running after controller restart" >&2
    return 1
  fi
  observed=$(cli exec "$ACTIVE_SANDBOX_ID" /bin/cat /workspace/controller-restart.txt)
  if [ "$observed" != "$marker" ]; then
    echo "active workspace data did not survive controller restart" >&2
    return 1
  fi

  cli sandbox delete "$ACTIVE_SANDBOX_ID" >/dev/null
  ACTIVE_SANDBOX_ID=
  cli workspace delete "$ACTIVE_WORKSPACE_ID" >/dev/null
  ACTIVE_WORKSPACE_ID=

  recovery_finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  recovery_duration=$(( $(date +%s%3N) - recovery_started_ms ))
  printf '%s\n' \
    "{\"target\":\"$recovery_target\",\"scope\":\"active sandbox identity and durable workspace data across a runtime controller restart\",\"qualification\":\"controller_restart_recovery_conformant\",\"started_at\":\"$recovery_started\",\"finished_at\":\"$recovery_finished\",\"steps\":[{\"name\":\"controller_restart_with_active_sandbox\",\"status\":\"passed\",\"duration_ms\":$recovery_duration}]}" \
    > "$report_tmp"
  chmod 600 "$report_tmp"
  if [ -e "$QUALIFICATION_DIR/$recovery_target.json" ]; then
    echo "qualification report already exists for $recovery_target" >&2
    rm -f -- "$report_tmp"
    return 1
  fi
  mv -- "$report_tmp" "$QUALIFICATION_DIR/$recovery_target.json"
}

resolve_engine_sandbox_id() {
  product_sandbox_id=$1
  state_database="$BREZEL_STATE_DIR/brezel.db"
  [ -s "$state_database" ] || {
    echo "runtime database is unavailable while resolving the engine sandbox ID" >&2
    return 1
  }
  # Qualification is trusted host-side code. It reads the private lifecycle
  # ledger directly so the engine identifier never becomes a public API field.
  engine_sandbox_id=$(python3 - "$state_database" "$product_sandbox_id" <<'PY'
import json
import sqlite3
import sys

database, sandbox_id = sys.argv[1:]
connection = sqlite3.connect(f"file:{database}?mode=ro", uri=True, timeout=5)
try:
    row = connection.execute(
        "SELECT payload FROM resources WHERE kind = ? AND resource_key = ?",
        ("sandbox", sqlite3.Binary(("brezel-conformance\x00" + sandbox_id).encode())),
    ).fetchone()
    if row is not None:
        print(json.loads(row[0])["backend_id"])
finally:
    connection.close()
PY
  )
  case "$engine_sandbox_id" in
    ""|*[!A-Za-z0-9._-]*)
      echo "could not resolve a valid engine sandbox ID for the live capability probe" >&2
      return 1
      ;;
  esac
  printf '%s\n' "$engine_sandbox_id"
}

run_node_restart_recovery() {
  recovery_target=$1
  report_tmp=$(mktemp "$QUALIFICATION_DIR/.report.XXXXXX")
  recovery_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  recovery_started_ms=$(date +%s%3N)
  marker="node-restart-$recovery_started_ms"

  ACTIVE_WORKSPACE_ID=$(cli workspace create node-restart-recovery | awk 'NR == 1 {print $1}')
  [ -n "$ACTIVE_WORKSPACE_ID" ] || {
    echo "node restart qualification did not return a workspace ID" >&2
    return 1
  }
  ACTIVE_SANDBOX_ID=$(cli new --template "$TEMPLATE_REFERENCE" --workspace "$ACTIVE_WORKSPACE_ID:/workspace" --ttl 600 | awk 'NR == 1 {print $1}')
  [ -n "$ACTIVE_SANDBOX_ID" ] || {
    echo "node restart qualification did not return a sandbox ID" >&2
    return 1
  }
  cli exec "$ACTIVE_SANDBOX_ID" /bin/sh -c 'printf %s "$1" > /workspace/node-restart.txt' runtime-recovery "$marker"

  compose restart brezel-node >/dev/null
  wait_ready
  if ! cli sandbox inspect "$ACTIVE_SANDBOX_ID" | grep -q '"state": "running"'; then
    echo "active sandbox was not running after node restart" >&2
    return 1
  fi
  observed=$(cli exec "$ACTIVE_SANDBOX_ID" /bin/cat /workspace/node-restart.txt)
  if [ "$observed" != "$marker" ]; then
    echo "the relay did not preserve the active sandbox route across node restart" >&2
    return 1
  fi

  cli sandbox delete "$ACTIVE_SANDBOX_ID" >/dev/null
  ACTIVE_SANDBOX_ID=
  cli workspace delete "$ACTIVE_WORKSPACE_ID" >/dev/null
  ACTIVE_WORKSPACE_ID=

  recovery_finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  recovery_duration=$(( $(date +%s%3N) - recovery_started_ms ))
  printf '%s\n' \
    "{\"target\":\"$recovery_target\",\"scope\":\"active sandbox route and durable workspace data across an execution node restart\",\"qualification\":\"node_restart_recovery_conformant\",\"started_at\":\"$recovery_started\",\"finished_at\":\"$recovery_finished\",\"steps\":[{\"name\":\"node_restart_with_active_sandbox\",\"status\":\"passed\",\"duration_ms\":$recovery_duration}]}" \
    > "$report_tmp"
  chmod 600 "$report_tmp"
  if [ -e "$QUALIFICATION_DIR/$recovery_target.json" ]; then
    echo "qualification report already exists for $recovery_target" >&2
    rm -f -- "$report_tmp"
    return 1
  fi
  mv -- "$report_tmp" "$QUALIFICATION_DIR/$recovery_target.json"
}

run_engine_fast_path_qualification() {
  fast_path_target=$1
  if [ -e "$QUALIFICATION_DIR/$fast_path_target.json" ]; then
    echo "qualification report already exists for $fast_path_target" >&2
    return 1
  fi
  fast_path_started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  fast_path_started_ms=$(date +%s%3N)

  ACTIVE_SANDBOX_ID=$(cli new --template "$TEMPLATE_REFERENCE" --ttl 600 | awk 'NR == 1 {print $1}')
  if [ -z "$ACTIVE_SANDBOX_ID" ]; then
    echo "engine fast-path qualification did not return a sandbox ID" >&2
    return 1
  fi
  cli exec "$ACTIVE_SANDBOX_ID" /bin/true >/dev/null
  engine_sandbox_id=$(resolve_engine_sandbox_id "$ACTIVE_SANDBOX_ID")
  min_network_slots=$MIN_READY_NETWORK_SLOTS
  max_starting_sandboxes=$ENGINE_MAX_STARTING_SANDBOXES

  # The empty-host capacity result was captured before any qualification VM
  # was admitted. Reuse that evidence here; a live guest legitimately owns
  # part of the hugepage pool at this point.
  capacity_json=$PREFLIGHT_CAPACITY_JSON
  engine_capacity_json=$(engine_compose exec -T \
    -e "BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL=$MAX_ACTIVE_SANDBOXES_TOTAL" \
    -e "BREZEL_GUEST_VCPUS=${BREZEL_GUEST_VCPUS:-2}" \
    -e "BREZEL_GUEST_MEMORY_MIB=${BREZEL_GUEST_MEMORY_MIB:-512}" \
    -e "BREZEL_GUEST_MIN_FREE_DISK_MIB=${BREZEL_GUEST_MIN_FREE_DISK_MIB:-512}" \
    -e "BREZEL_GUEST_MAX_FREE_DISK_MIB=${BREZEL_GUEST_MAX_FREE_DISK_MIB:-25600}" \
    -e "BREZEL_ENGINE_BASE_TEMPLATE_NAME=$INSTALLED_TEMPLATE_NAME" \
    postgres sh -s -- verify < "$ENGINE_CAPACITY_PROBE")

  if ! capability_json=$(engine_compose exec -T orchestrator \
    nsenter -t 1 -m -u -i -n -p -C -- /bin/sh -s -- live "$engine_sandbox_id" "$min_network_slots" "$max_starting_sandboxes" \
      "$ENGINE_NETWORK_NEW_SLOTS" "$ENGINE_NETWORK_REUSED_SLOTS" "$ENGINE_NBD_POOL_SIZE" \
      "$ENGINE_NBD_CONNECTIONS_PER_DEVICE" \
      "${BREZEL_ENGINE_FIRECRACKER_SMT:-false}" "${BREZEL_ENGINE_FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY:-false}" \
      "${BREZEL_ENGINE_FIRECRACKER_CPUSET_CPUS:-}" "${BREZEL_ENGINE_FIRECRACKER_CPUSET_MEMS:-}" \
      "${BREZEL_ENGINE_FIRECRACKER_VCPU_CPUS:-}" "${BREZEL_ENGINE_FIRECRACKER_VMM_CPUS:-}" \
    < "$ENGINE_CAPABILITY_PROBE"); then
    echo "the installed engine did not satisfy the live fast-path contract" >&2
    return 1
  fi

  cli sandbox delete "$ACTIVE_SANDBOX_ID" >/dev/null
  ACTIVE_SANDBOX_ID=

  fast_path_finished=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  fast_path_duration=$(( $(date +%s%3N) - fast_path_started_ms ))
  report_tmp=$(mktemp "$QUALIFICATION_DIR/.report.XXXXXX")
  printf '%s\n' \
    "{\"target\":\"$fast_path_target\",\"scope\":\"installed engine snapshot, paging, rootfs, cache, network, and host-capacity fast paths\",\"qualification\":\"engine_fast_path_conformant\",\"started_at\":\"$fast_path_started\",\"finished_at\":\"$fast_path_finished\",\"duration_ms\":$fast_path_duration,\"observed\":{\"engine\":$capability_json,\"capacity\":$capacity_json,\"engine_capacity\":$engine_capacity_json}}" \
    > "$report_tmp"
  chmod 600 "$report_tmp"
  mv -- "$report_tmp" "$QUALIFICATION_DIR/$fast_path_target.json"
}

# Capacity is an admission prerequisite, not a property to discover after the
# destructive suite has already created a VM. Keep this before every CLI call
# that can create a sandbox or workspace.
preflight_capacity_contract
run_conformance "$TARGET"

# A valid credential must not be able to manufacture a new tenant identity by
# changing X-Project-ID. brezel reads the token from the protected mount.
if compose exec -T brezeld \
  /usr/local/bin/brezel \
    -url http://127.0.0.1:8080 \
    -token-file /run/brezel-secrets/service.token \
    -project runtime-unbound-project \
    list >/dev/null 2>&1; then
  echo "project binding qualification failed: unbound project was accepted" >&2
  exit 1
fi

# Source inspection proves the pinned implementation contains these fast
# paths. This live gate proves the installed host is actually using them and
# that the best-effort upstream template optimizer produced a usable mapping.
run_engine_fast_path_qualification "$TARGET-engine-fast-path"

# Keep an actual microVM and workspace active while the controller is replaced.
# This detects reconciliation paths that a clean restart cannot exercise.
run_active_recovery "$TARGET-active-restart"

# Replace the separately authenticated byte-path process while an assigned VM
# remains live. The fresh relay boot identity invalidates old capabilities;
# the next operation must resolve the current boot and use the durable route.
run_node_restart_recovery "$TARGET-node-restart"

# Repeat the destructive suite after recovery to catch lock-release, decode,
# dependency readiness, idempotency-index, and engine reconnection failures.
run_conformance "$TARGET-post-restart"

trap - EXIT HUP INT TERM
echo "Qualification reports: $QUALIFICATION_DIR/$TARGET.json, $QUALIFICATION_DIR/$TARGET-engine-fast-path.json, $QUALIFICATION_DIR/$TARGET-active-restart.json, $QUALIFICATION_DIR/$TARGET-node-restart.json, and $QUALIFICATION_DIR/$TARGET-post-restart.json"
