#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
INSTALL_DIR=${BREZEL_INSTALL_DIR:-"$REPO_DIR/.brezel"}
ENGINE_DIR="$INSTALL_DIR/engine"
STATE_DIR="$INSTALL_DIR/state"
SECRETS_DIR="$INSTALL_DIR/secrets"
WORKSPACE_DIR="$STATE_DIR/workspaces"
LOCK_FILE="$SCRIPT_DIR/engine.lock"
ENGINE_OVERRIDE="$SCRIPT_DIR/engine.override.yaml"
ENGINE_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0001-harden-volume-secrets-and-cleanup.patch"
ENGINE_BUILD_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0002-pin-api-build-images.patch"
ENGINE_ORCHESTRATOR_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0003-acknowledge-delete-after-sandbox-teardown.patch"
ENGINE_CACHE_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0004-bound-snapshot-diff-cache.patch"
ENGINE_NFS_DURABILITY_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0005-make-nfs-writes-crash-durable.patch"
ENGINE_START_ADMISSION_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0006-bound-start-admission-retries.patch"
ENGINE_LOCAL_CAPACITY_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0007-scale-local-resource-pools-and-template-shape.patch"
ENGINE_ENVD_PROCESS_TAG_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0008-fix-envd-process-tag-resolution.patch"
ENGINE_ENVD_PROCESS_REPLAY_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0009-add-bounded-process-output-replay.patch"
ENGINE_CPU_TOPOLOGY_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0010-disable-smt-and-pin-exclusive-cpu-topology.patch"
ENGINE_ROOTFS_READ_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0011-reduce-rootfs-read-amplification.patch"
ENGINE_NBD_MULTIQUEUE_PATCH="$REPO_DIR/third_party/e2b-runtime/patches/0012-harden-nbd-multiqueue-lifecycle.patch"
ENGINE_CAPABILITY_PROBE="$SCRIPT_DIR/engine-capabilities.sh"
CAPACITY_PROBE="$SCRIPT_DIR/capacity-contract.sh"
ENGINE_CAPACITY_PROBE="$SCRIPT_DIR/engine-capacity-contract.sh"
ENGINE_AUTH_CACHE_PROBE="$SCRIPT_DIR/engine-auth-cache-contract.sh"
ENGINE_IMAGE_LOCK="$SCRIPT_DIR/engine.images.lock"
ENGINE_ARTIFACT_LOCK="$SCRIPT_DIR/engine.artifacts.lock"
ARTIFACT_SUPPLY_CHAIN="$SCRIPT_DIR/artifact-supply-chain.sh"
RUNTIME_ATTESTATION="$SCRIPT_DIR/runtime-attestation.sh"
BREZEL_HOST_TUNING_SCRIPT="$SCRIPT_DIR/host-tuning.sh"
BREZEL_ENGINE_CAPACITY_SCRIPT="$ENGINE_CAPACITY_PROBE"
BREZEL_ENGINE_AUTH_CACHE_SCRIPT="$ENGINE_AUTH_CACHE_PROBE"
export BREZEL_HOST_TUNING_SCRIPT
export BREZEL_ENGINE_CAPACITY_SCRIPT
export BREZEL_ENGINE_AUTH_CACHE_SCRIPT

# Keep these defaults identical to the packaged Compose profile. The capacity
# probe validates their physical feasibility before downloads or builds.
BREZEL_GUEST_VCPUS=${BREZEL_GUEST_VCPUS:-2}
BREZEL_GUEST_MEMORY_MIB=${BREZEL_GUEST_MEMORY_MIB:-512}
BREZEL_GUEST_MIN_FREE_DISK_MIB=${BREZEL_GUEST_MIN_FREE_DISK_MIB:-512}
BREZEL_GUEST_MAX_FREE_DISK_MIB=${BREZEL_GUEST_MAX_FREE_DISK_MIB:-25600}
BREZEL_ENGINE_HUGEPAGES=${BREZEL_ENGINE_HUGEPAGES:-9216}
BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL=${BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL:-32}
BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT=${BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT:-32}
BREZEL_WARM_POOL_SIZE=${BREZEL_WARM_POOL_SIZE:-0}
BREZEL_WARM_POOL_TEMPLATE=${BREZEL_WARM_POOL_TEMPLATE:-base}
BREZEL_WARM_POOL_SLOT_TTL_SECONDS=${BREZEL_WARM_POOL_SLOT_TTL_SECONDS:-14400}
BREZEL_WARM_POOL_MAX_CLAIM_TTL_SECONDS=${BREZEL_WARM_POOL_MAX_CLAIM_TTL_SECONDS:-3600}
BREZEL_WARM_POOL_PRIME_CONCURRENCY=${BREZEL_WARM_POOL_PRIME_CONCURRENCY:-1}
BREZEL_WARM_POOL_ALLOW_INTERNET=${BREZEL_WARM_POOL_ALLOW_INTERNET:-false}
BREZEL_WARM_POOL_STRICT=${BREZEL_WARM_POOL_STRICT:-true}
BREZEL_ENGINE_MAX_STARTING_SANDBOXES=${BREZEL_ENGINE_MAX_STARTING_SANDBOXES:-3}
BREZEL_ENGINE_NETWORK_NEW_SLOTS=${BREZEL_ENGINE_NETWORK_NEW_SLOTS:-32}
BREZEL_ENGINE_NETWORK_REUSED_SLOTS=${BREZEL_ENGINE_NETWORK_REUSED_SLOTS:-100}
BREZEL_ENGINE_NBD_POOL_SIZE=${BREZEL_ENGINE_NBD_POOL_SIZE:-64}
BREZEL_LIFECYCLE_HEADROOM_SANDBOXES=${BREZEL_LIFECYCLE_HEADROOM_SANDBOXES:-4}
BREZEL_MIN_SYSTEM_MEMORY_MIB=${BREZEL_MIN_SYSTEM_MEMORY_MIB:-8192}
BREZEL_MIN_SYSTEM_CPUS=${BREZEL_MIN_SYSTEM_CPUS:-2}
BREZEL_ENGINE_FIRECRACKER_SMT=${BREZEL_ENGINE_FIRECRACKER_SMT:-false}
BREZEL_ENGINE_FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY=${BREZEL_ENGINE_FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY:-false}
BREZEL_BUILD_CACHE_TTL=${BREZEL_BUILD_CACHE_TTL:-4h}
BREZEL_BUILD_CACHE_MAX_BYTES=${BREZEL_BUILD_CACHE_MAX_BYTES:-34359738368}
BREZEL_BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT=${BREZEL_BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT:-70}
BREZEL_ENGINE_BASE_TEMPLATE_SOURCE_IMAGE=${BREZEL_ENGINE_BASE_TEMPLATE_SOURCE_IMAGE:-e2bdev/base@sha256:197ad15124a51884aea5a629b96045cd8300bdbbb6df648647004fad99fc59ec}
export BREZEL_GUEST_VCPUS BREZEL_GUEST_MEMORY_MIB BREZEL_GUEST_MIN_FREE_DISK_MIB BREZEL_GUEST_MAX_FREE_DISK_MIB
export BREZEL_ENGINE_HUGEPAGES BREZEL_ENGINE_NETWORK_NEW_SLOTS BREZEL_ENGINE_NETWORK_REUSED_SLOTS BREZEL_ENGINE_NBD_POOL_SIZE
export BREZEL_LIFECYCLE_HEADROOM_SANDBOXES BREZEL_MIN_SYSTEM_MEMORY_MIB BREZEL_MIN_SYSTEM_CPUS
export BREZEL_ENGINE_FIRECRACKER_SMT BREZEL_ENGINE_FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY
export BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT
export BREZEL_WARM_POOL_SIZE BREZEL_WARM_POOL_TEMPLATE BREZEL_WARM_POOL_SLOT_TTL_SECONDS
export BREZEL_WARM_POOL_MAX_CLAIM_TTL_SECONDS BREZEL_WARM_POOL_PRIME_CONCURRENCY
export BREZEL_WARM_POOL_ALLOW_INTERNET BREZEL_WARM_POOL_STRICT
export BREZEL_ENGINE_MAX_STARTING_SANDBOXES
export BREZEL_BUILD_CACHE_TTL BREZEL_BUILD_CACHE_MAX_BYTES BREZEL_BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT
export BREZEL_ENGINE_BASE_TEMPLATE_SOURCE_IMAGE

case "$BREZEL_ENGINE_BASE_TEMPLATE_SOURCE_IMAGE" in
  *@sha256:????????????????????????????????????????????????????????????????) ;;
  *)
    echo "BREZEL_ENGINE_BASE_TEMPLATE_SOURCE_IMAGE must name an immutable OCI image manifest by SHA-256 digest" >&2
    exit 1
    ;;
esac
base_template_digest=${BREZEL_ENGINE_BASE_TEMPLATE_SOURCE_IMAGE##*@sha256:}
printf '%s\n' "$base_template_digest" | grep -Eq '^[0-9a-f]{64}$' || {
  echo "BREZEL_ENGINE_BASE_TEMPLATE_SOURCE_IMAGE must use a lowercase SHA-256 digest" >&2
  exit 1
}

case "$BREZEL_BUILD_CACHE_TTL" in
  *h)
    build_cache_ttl_hours=${BREZEL_BUILD_CACHE_TTL%h}
    case "$build_cache_ttl_hours" in
      ""|*[!0-9]*) echo "BREZEL_BUILD_CACHE_TTL must be a whole number of hours" >&2; exit 1 ;;
    esac
    [ "$build_cache_ttl_hours" -ge 1 ] && [ "$build_cache_ttl_hours" -le 168 ] || {
      echo "BREZEL_BUILD_CACHE_TTL must be between 1h and 168h" >&2
      exit 1
    }
    ;;
  *) echo "BREZEL_BUILD_CACHE_TTL must be a whole number of hours, for example 4h" >&2; exit 1 ;;
esac
case "$BREZEL_BUILD_CACHE_MAX_BYTES" in
  ""|*[!0-9]*) echo "BREZEL_BUILD_CACHE_MAX_BYTES must be an integer byte count" >&2; exit 1 ;;
esac
[ "$BREZEL_BUILD_CACHE_MAX_BYTES" -ge 1073741824 ] || {
  echo "BREZEL_BUILD_CACHE_MAX_BYTES must be at least 1073741824 (1 GiB)" >&2
  exit 1
}
case "$BREZEL_BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT" in
  ""|*[!0-9]*) echo "BREZEL_BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT must be an integer" >&2; exit 1 ;;
esac
[ "$BREZEL_BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT" -ge 50 ] && \
  [ "$BREZEL_BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT" -le 90 ] || {
  echo "BREZEL_BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT must be between 50 and 90" >&2
  exit 1
}

read_lock() {
  key=$1
  sed -n "s/^${key}=//p" "$LOCK_FILE"
}

ENGINE_REPOSITORY=$(read_lock repository)
ENGINE_COMMIT=$(read_lock commit)
ENGINE_PATCH_SHA256=$(read_lock api_patch_sha256)
ENGINE_BUILD_PATCH_SHA256=$(read_lock api_build_patch_sha256)
ENGINE_ORCHESTRATOR_PATCH_SHA256=$(read_lock orchestrator_lifecycle_patch_sha256)
ENGINE_CACHE_PATCH_SHA256=$(read_lock orchestrator_cache_patch_sha256)
ENGINE_NFS_DURABILITY_PATCH_SHA256=$(read_lock orchestrator_nfs_durability_patch_sha256)
ENGINE_START_ADMISSION_PATCH_SHA256=$(read_lock engine_start_admission_patch_sha256)
ENGINE_LOCAL_CAPACITY_PATCH_SHA256=$(read_lock engine_local_capacity_patch_sha256)
ENGINE_CPU_TOPOLOGY_PATCH_SHA256=$(read_lock orchestrator_cpu_topology_patch_sha256)
ENGINE_ROOTFS_READ_PATCH_SHA256=$(read_lock orchestrator_rootfs_read_patch_sha256)
ENGINE_NBD_MULTIQUEUE_PATCH_SHA256=$(read_lock orchestrator_nbd_multiqueue_patch_sha256)
ENGINE_ENVD_PROCESS_TAG_PATCH_SHA256=$(read_lock envd_process_tag_patch_sha256)
ENGINE_ENVD_PROCESS_REPLAY_PATCH_SHA256=$(read_lock envd_process_replay_patch_sha256)
if [ -z "$ENGINE_REPOSITORY" ] || [ -z "$ENGINE_COMMIT" ] || [ -z "$ENGINE_PATCH_SHA256" ] || [ -z "$ENGINE_BUILD_PATCH_SHA256" ] || [ -z "$ENGINE_ORCHESTRATOR_PATCH_SHA256" ] || [ -z "$ENGINE_CACHE_PATCH_SHA256" ] || [ -z "$ENGINE_NFS_DURABILITY_PATCH_SHA256" ] || [ -z "$ENGINE_START_ADMISSION_PATCH_SHA256" ] || [ -z "$ENGINE_LOCAL_CAPACITY_PATCH_SHA256" ] || [ -z "$ENGINE_CPU_TOPOLOGY_PATCH_SHA256" ] || [ -z "$ENGINE_ROOTFS_READ_PATCH_SHA256" ] || [ -z "$ENGINE_NBD_MULTIQUEUE_PATCH_SHA256" ] || [ -z "$ENGINE_ENVD_PROCESS_TAG_PATCH_SHA256" ] || [ -z "$ENGINE_ENVD_PROCESS_REPLAY_PATCH_SHA256" ]; then
  echo "invalid engine.lock" >&2
  exit 1
fi
ENGINE_SOURCE_REPOSITORY=${BREZEL_ENGINE_SOURCE_REPOSITORY:-$ENGINE_REPOSITORY}
case "$ENGINE_SOURCE_REPOSITORY" in
  ""|-*) echo "the engine source repository is invalid" >&2; exit 1 ;;
esac
BREZEL_ENGINE_ARTIFACT_BASE_URL=${BREZEL_ENGINE_ARTIFACT_BASE_URL:-https://storage.googleapis.com/e2b-artifact-binaries}
case "$BREZEL_ENGINE_ARTIFACT_BASE_URL" in
  https://*|file:///host/*) ;;
  *)
    echo "BREZEL_ENGINE_ARTIFACT_BASE_URL must use HTTPS or file:///host/<absolute-host-path>" >&2
    exit 1
    ;;
esac
export BREZEL_ENGINE_ARTIFACT_BASE_URL

if [ "$(uname -s)" != Linux ] || { [ "$(uname -m)" != x86_64 ] && [ "$(uname -m)" != amd64 ]; } || [ ! -c /dev/kvm ] || [ ! -c /dev/net/tun ]; then
  echo "This host cannot run the Firecracker profile." >&2
  echo "Use x86_64 Linux with KVM and /dev/net/tun, then run brezel doctor." >&2
  exit 1
fi
command -v git >/dev/null 2>&1 || { echo "git is required" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "Docker Engine is required" >&2; exit 1; }
command -v openssl >/dev/null 2>&1 || { echo "openssl is required" >&2; exit 1; }
command -v patch >/dev/null 2>&1 || { echo "patch is required" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required" >&2; exit 1; }
command -v tar >/dev/null 2>&1 || { echo "tar is required" >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "Docker Compose v2 is required" >&2; exit 1; }
docker buildx version >/dev/null 2>&1 || { echo "Docker Buildx is required" >&2; exit 1; }

# The installed runtime identity is meaningful only when every input belongs to
# one exact commit. Refuse local patches and untracked source before downloads,
# builds, or service replacement, then check the same revision again when the
# running containers are sealed into the install attestation.
if [ -n "$(git -C "$REPO_DIR" status --porcelain --untracked-files=normal)" ]; then
  echo "the Brezel source checkout must be clean before installation" >&2
  exit 1
fi
BREZEL_SOURCE_REVISION=$(git -C "$REPO_DIR" rev-parse HEAD)
printf '%s\n' "$BREZEL_SOURCE_REVISION" | grep -Eq '^[0-9a-f]{40}$' || {
  echo "the Brezel source checkout has no exact commit" >&2
  exit 1
}

# Docker's Linux user parser accepts signed 32-bit IDs. Cloud OS Login and
# directory-backed identities can legitimately allocate larger host IDs, but
# passing one to --user or Compose fails only after an expensive engine build.
# Fail before downloading or mutating runtime state and require the operator to
# use a dedicated, unprivileged service account with representable IDs.
HOST_UID=$(id -u)
HOST_GID=$(id -g)
if [ "$HOST_UID" -gt 2147483647 ] || [ "$HOST_GID" -gt 2147483647 ]; then
  echo "The current host identity cannot be represented by Docker (uid=$HOST_UID gid=$HOST_GID)." >&2
  echo "Run the installer as a dedicated unprivileged service account whose UID and GID are at most 2147483647." >&2
  exit 1
fi

check_ufw_guest_network() {
  if [ "${BREZEL_SKIP_UFW_PREFLIGHT:-}" = "true" ] || ! command -v ufw >/dev/null 2>&1; then
    return
  fi

  run_ufw() {
    if [ "$(id -u)" -eq 0 ]; then
      ufw "$@"
    elif command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then
      sudo -n ufw "$@"
    else
      return 126
    fi
  }

  if ! ufw_status=$(run_ufw status verbose 2>/dev/null); then
    if grep -Eq '^ENABLED=yes$' /etc/ufw/ufw.conf 2>/dev/null; then
      echo "UFW is enabled, but the installer cannot inspect it without non-interactive sudo." >&2
      echo "Grant non-interactive inspection or apply equivalent host-scoped guest rules, then retry." >&2
      exit 1
    fi
    return
  fi
  case "$ufw_status" in
    *"Status: active"*) ;;
    *) return ;;
  esac

  guest_rule_count=$(printf '%s\n' "$ufw_status" | grep -F '10.11.0.0/24' | grep -Ec 'ALLOW (IN|FWD)' || true)
  if [ "$guest_rule_count" -lt 2 ]; then
    egress_interface=$(ip route show default 2>/dev/null | awk 'NR == 1 {for (i = 1; i <= NF; i++) if ($i == "dev") {print $(i + 1); exit}}')
    [ -n "$egress_interface" ] || egress_interface='<egress-interface>'
    echo "UFW is active and blocks the pinned engine's Firecracker guest network." >&2
    echo "Apply these host-scoped rules, then rerun the installer:" >&2
    echo "  sudo ufw allow in from 10.11.0.0/24 to any port 5010:5018 proto tcp comment 'Firecracker guest services'" >&2
    echo "  sudo ufw route allow out on $egress_interface from 10.11.0.0/24 comment 'Firecracker guest egress'" >&2
    echo "Set BREZEL_SKIP_UFW_PREFLIGHT=true only after enforcing equivalent nftables rules." >&2
    exit 1
  fi
}

check_ufw_guest_network

"$CAPACITY_PROBE" plan >/dev/null

if [ "$(sha256sum "$ENGINE_PATCH" | awk '{print $1}')" != "$ENGINE_PATCH_SHA256" ]; then
  echo "engine API patch verification failed" >&2
  exit 1
fi
if [ "$(sha256sum "$ENGINE_BUILD_PATCH" | awk '{print $1}')" != "$ENGINE_BUILD_PATCH_SHA256" ]; then
  echo "engine API build patch verification failed" >&2
  exit 1
fi
if [ "$(sha256sum "$ENGINE_ORCHESTRATOR_PATCH" | awk '{print $1}')" != "$ENGINE_ORCHESTRATOR_PATCH_SHA256" ]; then
  echo "engine orchestrator lifecycle patch verification failed" >&2
  exit 1
fi
if [ "$(sha256sum "$ENGINE_CACHE_PATCH" | awk '{print $1}')" != "$ENGINE_CACHE_PATCH_SHA256" ]; then
  echo "engine orchestrator cache patch verification failed" >&2
  exit 1
fi
if [ "$(sha256sum "$ENGINE_NFS_DURABILITY_PATCH" | awk '{print $1}')" != "$ENGINE_NFS_DURABILITY_PATCH_SHA256" ]; then
  echo "engine orchestrator NFS durability patch verification failed" >&2
  exit 1
fi
if [ "$(sha256sum "$ENGINE_START_ADMISSION_PATCH" | awk '{print $1}')" != "$ENGINE_START_ADMISSION_PATCH_SHA256" ]; then
  echo "engine start-admission patch verification failed" >&2
  exit 1
fi
if [ "$(sha256sum "$ENGINE_LOCAL_CAPACITY_PATCH" | awk '{print $1}')" != "$ENGINE_LOCAL_CAPACITY_PATCH_SHA256" ]; then
  echo "engine local-capacity patch verification failed" >&2
  exit 1
fi
if [ "$(sha256sum "$ENGINE_CPU_TOPOLOGY_PATCH" | awk '{print $1}')" != "$ENGINE_CPU_TOPOLOGY_PATCH_SHA256" ]; then
  echo "engine Firecracker CPU-topology patch verification failed" >&2
  exit 1
fi
if [ "$(sha256sum "$ENGINE_ROOTFS_READ_PATCH" | awk '{print $1}')" != "$ENGINE_ROOTFS_READ_PATCH_SHA256" ]; then
  echo "engine rootfs-read patch verification failed" >&2
  exit 1
fi
if [ "$(sha256sum "$ENGINE_NBD_MULTIQUEUE_PATCH" | awk '{print $1}')" != "$ENGINE_NBD_MULTIQUEUE_PATCH_SHA256" ]; then
  echo "engine NBD multiqueue patch verification failed" >&2
  exit 1
fi
if [ "$(sha256sum "$ENGINE_ENVD_PROCESS_TAG_PATCH" | awk '{print $1}')" != "$ENGINE_ENVD_PROCESS_TAG_PATCH_SHA256" ]; then
  echo "engine envd process-tag patch verification failed" >&2
  exit 1
fi
if [ "$(sha256sum "$ENGINE_ENVD_PROCESS_REPLAY_PATCH" | awk '{print $1}')" != "$ENGINE_ENVD_PROCESS_REPLAY_PATCH_SHA256" ]; then
  echo "engine envd process-replay patch verification failed" >&2
  exit 1
fi
"$ARTIFACT_SUPPLY_CHAIN" image-lock "$ENGINE_IMAGE_LOCK"

read_image_lock() {
  key=$1
  sed -n "s/^${key}=//p" "$ENGINE_IMAGE_LOCK"
}

BREZEL_ENGINE_POSTGRES_IMAGE=$(read_image_lock BREZEL_ENGINE_POSTGRES_IMAGE)
BREZEL_ENGINE_REDIS_IMAGE=$(read_image_lock BREZEL_ENGINE_REDIS_IMAGE)
BREZEL_ENGINE_CLICKHOUSE_IMAGE=$(read_image_lock BREZEL_ENGINE_CLICKHOUSE_IMAGE)
BREZEL_ENGINE_VECTOR_IMAGE=$(read_image_lock BREZEL_ENGINE_VECTOR_IMAGE)
E2B_DB_MIGRATOR_IMAGE=$(read_image_lock E2B_DB_MIGRATOR_IMAGE)
E2B_CLIENT_PROXY_IMAGE=$(read_image_lock E2B_CLIENT_PROXY_IMAGE)
E2B_CLICKHOUSE_MIGRATOR_IMAGE=$(read_image_lock E2B_CLICKHOUSE_MIGRATOR_IMAGE)
E2B_TOOLS_IMAGE=$(read_image_lock E2B_TOOLS_IMAGE)
E2B_NODE_E2B_IMAGE=$(read_image_lock E2B_NODE_E2B_IMAGE)
E2B_SEED_IMAGE=$(read_image_lock E2B_SEED_IMAGE)
export BREZEL_ENGINE_POSTGRES_IMAGE BREZEL_ENGINE_REDIS_IMAGE BREZEL_ENGINE_CLICKHOUSE_IMAGE BREZEL_ENGINE_VECTOR_IMAGE
export E2B_DB_MIGRATOR_IMAGE E2B_CLIENT_PROXY_IMAGE E2B_CLICKHOUSE_MIGRATOR_IMAGE E2B_TOOLS_IMAGE E2B_NODE_E2B_IMAGE E2B_SEED_IMAGE

umask 077
mkdir -p "$INSTALL_DIR" "$STATE_DIR" "$SECRETS_DIR" "$WORKSPACE_DIR"
chmod 700 "$INSTALL_DIR" "$STATE_DIR" "$SECRETS_DIR" "$WORKSPACE_DIR"

if [ ! -s "$SECRETS_DIR/engine-volume-token.key" ]; then
  printf 'HMAC:' > "$SECRETS_DIR/engine-volume-token.key"
  openssl rand -base64 32 | tr -d '\n' >> "$SECRETS_DIR/engine-volume-token.key"
fi
chmod 600 "$SECRETS_DIR/engine-volume-token.key"
if ! grep -Eq '^HMAC:[A-Za-z0-9+/]+={0,2}$' "$SECRETS_DIR/engine-volume-token.key"; then
  echo "the local volume signing key is malformed; move it aside and rerun the installer" >&2
  exit 1
fi
BREZEL_WORKSPACE_DIR="$WORKSPACE_DIR"
export BREZEL_WORKSPACE_DIR
BREZEL_VOLUME_TOKEN_KEY_FILE="$SECRETS_DIR/engine-volume-token.key"
export BREZEL_VOLUME_TOKEN_KEY_FILE

if [ ! -d "$ENGINE_DIR/.git" ]; then
  git clone --filter=blob:none --no-checkout "$ENGINE_SOURCE_REPOSITORY" "$ENGINE_DIR"
fi
git -C "$ENGINE_DIR" fetch --depth=1 "$ENGINE_SOURCE_REPOSITORY" "$ENGINE_COMMIT"
git -C "$ENGINE_DIR" checkout --detach "$ENGINE_COMMIT"
ACTUAL_COMMIT=$(git -C "$ENGINE_DIR" rev-parse HEAD)
if [ "$ACTUAL_COMMIT" != "$ENGINE_COMMIT" ]; then
  echo "engine revision verification failed" >&2
  exit 1
fi
if [ -n "$(git -C "$ENGINE_DIR" status --porcelain)" ]; then
  echo "engine checkout contains uncommitted changes; refusing to run unverified source" >&2
  exit 1
fi
"$ARTIFACT_SUPPLY_CHAIN" source "$ENGINE_DIR" "$LOCK_FILE" "$ENGINE_IMAGE_LOCK" "$ENGINE_ARTIFACT_LOCK"

# Pull exact manifests once, or require an operator-preloaded image set. The
# compose override uses pull_policy: never, so startup cannot silently replace
# these bytes after this gate.
"$ARTIFACT_SUPPLY_CHAIN" images "$ENGINE_IMAGE_LOCK" "${BREZEL_ENGINE_IMAGE_MODE:-pull}"

ENGINE_BUILD_DIR=$(mktemp -d "$INSTALL_DIR/engine-build.XXXXXX")
ORCHESTRATOR_BUILD_CONTAINER=
ORCHESTRATOR_ARTIFACT_TMP=
ENVD_BUILD_CONTAINER=
ENVD_ARTIFACT_TMP=
NODE_TLS_DIR=
PUBLIC_DRAIN_STARTED=false
INSTALL_SUCCEEDED=false
cleanup_build_dir() {
  if [ -n "$ORCHESTRATOR_BUILD_CONTAINER" ]; then
    docker rm -f "$ORCHESTRATOR_BUILD_CONTAINER" >/dev/null 2>&1 || true
  fi
  if [ -n "$ORCHESTRATOR_ARTIFACT_TMP" ]; then
    case "$ORCHESTRATOR_ARTIFACT_TMP" in
      "$INSTALL_DIR"/orchestrator-artifact.*) rm -rf -- "$ORCHESTRATOR_ARTIFACT_TMP" ;;
    esac
  fi
  if [ -n "$ENVD_BUILD_CONTAINER" ]; then
    docker rm -f "$ENVD_BUILD_CONTAINER" >/dev/null 2>&1 || true
  fi
  if [ -n "$ENVD_ARTIFACT_TMP" ]; then
    case "$ENVD_ARTIFACT_TMP" in
      "$INSTALL_DIR"/envd-artifact.*) rm -rf -- "$ENVD_ARTIFACT_TMP" ;;
    esac
  fi
  case "$ENGINE_BUILD_DIR" in
    "$INSTALL_DIR"/engine-build.*) rm -rf -- "$ENGINE_BUILD_DIR" ;;
  esac
}
cleanup_install() {
  install_status=$?
  trap - EXIT HUP INT TERM
  set +e
  case "$NODE_TLS_DIR" in
    "$SECRETS_DIR"/.node-tls.*) rm -rf -- "$NODE_TLS_DIR" ;;
  esac
  cleanup_build_dir
  if [ "$PUBLIC_DRAIN_STARTED" = true ] && [ "$INSTALL_SUCCEEDED" != true ]; then
    docker compose -f "$SCRIPT_DIR/compose.yaml" stop brezeld >/dev/null 2>&1
    docker compose -f "$SCRIPT_DIR/compose.yaml" stop brezel-node >/dev/null 2>&1
  fi
  exit "$install_status"
}
trap cleanup_install EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
git -C "$ENGINE_DIR" archive "$ENGINE_COMMIT" | tar -xf - -C "$ENGINE_BUILD_DIR"
patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_PATCH"
patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_BUILD_PATCH"
patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_ORCHESTRATOR_PATCH"
patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_CACHE_PATCH"
patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_NFS_DURABILITY_PATCH"
patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_START_ADMISSION_PATCH"
patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_LOCAL_CAPACITY_PATCH"
patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_ENVD_PROCESS_TAG_PATCH"
patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_ENVD_PROCESS_REPLAY_PATCH"
patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_CPU_TOPOLOGY_PATCH"
patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_ROOTFS_READ_PATCH"
patch --batch --forward --fuzz=0 -d "$ENGINE_BUILD_DIR" -p1 < "$ENGINE_NBD_MULTIQUEUE_PATCH"
"$ENGINE_CAPABILITY_PROBE" source "$ENGINE_BUILD_DIR"

# The upstream base-template service executes a JavaScript helper embedded in
# the released tools image. Brezel patches that helper to make the guest shape
# operator-owned, so running the image's original copy would silently build the
# default 2-vCPU/512-MiB template. Preserve the exact patched helper outside the
# temporary build tree and bind it into the one-shot service below. The service
# verifies this digest before Node evaluates the file.
mkdir -p "$INSTALL_DIR/artifacts"
chmod 700 "$INSTALL_DIR/artifacts"
BASE_TEMPLATE_SCRIPT_TMP=$(mktemp "$INSTALL_DIR/artifacts/.build-base-template.XXXXXX")
cp "$ENGINE_BUILD_DIR/embed/compose/scripts/node/build-base-template.mjs" "$BASE_TEMPLATE_SCRIPT_TMP"
chmod 500 "$BASE_TEMPLATE_SCRIPT_TMP"
BREZEL_ENGINE_BASE_TEMPLATE_SCRIPT="$INSTALL_DIR/artifacts/build-base-template.mjs"
mv -f -- "$BASE_TEMPLATE_SCRIPT_TMP" "$BREZEL_ENGINE_BASE_TEMPLATE_SCRIPT"
BREZEL_ENGINE_BASE_TEMPLATE_SCRIPT_SHA256=$(sha256sum "$BREZEL_ENGINE_BASE_TEMPLATE_SCRIPT" | awk '{print $1}')
export BREZEL_ENGINE_BASE_TEMPLATE_SCRIPT BREZEL_ENGINE_BASE_TEMPLATE_SCRIPT_SHA256
EXPECTED_MIGRATION_TIMESTAMP=$(find "$ENGINE_BUILD_DIR/packages/db/migrations" -maxdepth 1 -type f -printf '%f\n' | sed 's/_.*//' | sort | tail -n 1)
if [ -z "$EXPECTED_MIGRATION_TIMESTAMP" ]; then
  echo "could not resolve the pinned engine migration version" >&2
  exit 1
fi
BREZEL_ENGINE_API_IMAGE="brezel/engine-api:${ENGINE_COMMIT}-hardening-v1"
docker build \
  -t "$BREZEL_ENGINE_API_IMAGE" \
  -f "$ENGINE_BUILD_DIR/packages/api/Dockerfile" \
  --build-arg "COMMIT_SHA=${ENGINE_COMMIT}-hardening-v1" \
  --build-arg "VERSION=${ENGINE_COMMIT}-hardening-v1" \
  --build-arg "EXPECTED_MIGRATION_TIMESTAMP=$EXPECTED_MIGRATION_TIMESTAMP" \
  "$ENGINE_BUILD_DIR/packages"
export BREZEL_ENGINE_API_IMAGE

# Build the lifecycle-critical orchestrator from the same pinned source tree.
# The released upstream binary acknowledges deletion before Firecracker, NBD,
# networking, and lazy-memory resources have been reclaimed. Brezel treats a
# successful delete as a durable resource-reclamation boundary instead.
BREZEL_ENGINE_ORCHESTRATOR_IMAGE="brezel/engine-orchestrator:${ENGINE_COMMIT}-durability-v1"
docker build \
  --platform linux/amd64 \
  -t "$BREZEL_ENGINE_ORCHESTRATOR_IMAGE" \
  -f "$ENGINE_BUILD_DIR/packages/orchestrator/Dockerfile" \
  --build-arg "COMMIT_SHA=${ENGINE_COMMIT}-durability-v1" \
  --build-arg "VERSION=${ENGINE_COMMIT}-durability-v1" \
  "$ENGINE_BUILD_DIR/packages"
ORCHESTRATOR_ARTIFACT_TMP=$(mktemp -d "$INSTALL_DIR/orchestrator-artifact.XXXXXX")
ORCHESTRATOR_BUILD_CONTAINER=$(docker create --entrypoint /orchestrator "$BREZEL_ENGINE_ORCHESTRATOR_IMAGE")
docker cp "$ORCHESTRATOR_BUILD_CONTAINER:/orchestrator" "$ORCHESTRATOR_ARTIFACT_TMP/orchestrator"
docker rm "$ORCHESTRATOR_BUILD_CONTAINER" >/dev/null
ORCHESTRATOR_BUILD_CONTAINER=
chmod 755 "$ORCHESTRATOR_ARTIFACT_TMP/orchestrator"
BREZEL_ENGINE_ORCHESTRATOR_BINARY="$INSTALL_DIR/artifacts/orchestrator"
mv -f -- "$ORCHESTRATOR_ARTIFACT_TMP/orchestrator" "$BREZEL_ENGINE_ORCHESTRATOR_BINARY"
rmdir "$ORCHESTRATOR_ARTIFACT_TMP"
ORCHESTRATOR_ARTIFACT_TMP=
BREZEL_ENGINE_ORCHESTRATOR_SHA256=$(sha256sum "$BREZEL_ENGINE_ORCHESTRATOR_BINARY" | awk '{print $1}')
export BREZEL_ENGINE_ORCHESTRATOR_BINARY BREZEL_ENGINE_ORCHESTRATOR_SHA256

# Build envd from the same pinned and patched source tree. The released envd
# binary needs both corrected live tag lookup and generation-bound, cursorized
# output recovery. Installing only the source patches would leave every
# Firecracker guest on the upstream artifact. The protected binary below
# replaces that fetched artifact before any new guest can start.
BREZEL_ENGINE_ENVD_IMAGE="brezel/engine-envd:${ENGINE_COMMIT}-process-replay-v1"
ENVD_VERSION=$(sed -n 's/.*Version = "\([^"]*\)".*/\1/p' "$ENGINE_BUILD_DIR/packages/envd/pkg/version.go")
[ -n "$ENVD_VERSION" ] || { echo "could not resolve the pinned envd version" >&2; exit 1; }
docker build \
  --platform linux/amd64 \
  -t "$BREZEL_ENGINE_ENVD_IMAGE" \
  -f "$SCRIPT_DIR/envd.Dockerfile" \
  --build-arg "COMMIT_SHA=${ENGINE_COMMIT}" \
  --build-arg "VERSION=${ENVD_VERSION}" \
  "$ENGINE_BUILD_DIR/packages"
ENVD_ARTIFACT_TMP=$(mktemp -d "$INSTALL_DIR/envd-artifact.XXXXXX")
ENVD_BUILD_CONTAINER=$(docker create --entrypoint /envd "$BREZEL_ENGINE_ENVD_IMAGE")
docker cp "$ENVD_BUILD_CONTAINER:/envd" "$ENVD_ARTIFACT_TMP/envd"
docker rm "$ENVD_BUILD_CONTAINER" >/dev/null
ENVD_BUILD_CONTAINER=
chmod 755 "$ENVD_ARTIFACT_TMP/envd"
BREZEL_ENGINE_ENVD_BINARY="$INSTALL_DIR/artifacts/envd"
mv -f -- "$ENVD_ARTIFACT_TMP/envd" "$BREZEL_ENGINE_ENVD_BINARY"
rmdir "$ENVD_ARTIFACT_TMP"
ENVD_ARTIFACT_TMP=
BREZEL_ENGINE_ENVD_SHA256=$(sha256sum "$BREZEL_ENGINE_ENVD_BINARY" | awk '{print $1}')
export BREZEL_ENGINE_ENVD_BINARY BREZEL_ENGINE_ENVD_SHA256

ENGINE_COMPOSE="$ENGINE_DIR/embed/compose/compose.yaml"
ENGINE_ENV="$ENGINE_DIR/embed/compose/.env"
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" config >/dev/null

# Restore and verify the exact released artifact set before replacing envd and
# the orchestrator with source-built Brezel binaries. The one-shot installers
# are deterministic on upgrades even when Compose retains completed containers.
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" run --rm --no-deps preflight

# Stop public admission before host tuning or replacing any live engine
# artifact. Keep it stopped if any later step fails: an installer error must
# not leave an old controller serving against a partially upgraded engine.
# Stop the API before the node so no new data operation can be admitted while
# the relay is taken out of service.
export BREZEL_STATE_DIR="$STATE_DIR" BREZEL_SECRETS_DIR="$SECRETS_DIR"
export BREZEL_UID="$(id -u)" BREZEL_GID="$(id -g)"
PUBLIC_DRAIN_STARTED=true
docker compose -f "$SCRIPT_DIR/compose.yaml" stop brezeld
docker compose -f "$SCRIPT_DIR/compose.yaml" stop brezel-node

# Replacing an executable does not affect an already-running process. Stop the
# engine explicitly in reverse request-flow order. Do not suppress a failed
# stop: continuing would permit concurrent cache writers or mixed revisions.
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" stop ready
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" stop client-proxy
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" stop api
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" stop orchestrator
# PostgreSQL opportunistically maps its shared buffers through the host
# hugetlb pool. Stop the old container before measuring the empty guest pool;
# the override below restarts it with huge_pages=off so database memory can
# never steal Firecracker admission capacity after the gate passes.
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" stop postgres

# Host and engine mutations begin only inside the stopped-service boundary.
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" run --rm --no-deps host-setup
"$CAPACITY_PROBE" live >/dev/null
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" run --rm --no-deps fetch-artifacts
"$ARTIFACT_SUPPLY_CHAIN" host "$ENGINE_ARTIFACT_LOCK" /

# Install the verified executables only after the old orchestrator and every
# public admission path are stopped. This prevents a restart in the replacement
# window from creating a guest with mixed engine artifacts.
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" run --rm --no-deps brezel-envd-install
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" run --rm --no-deps brezel-orchestrator-install
"$ARTIFACT_SUPPLY_CHAIN" host "$ENGINE_ARTIFACT_LOCK" / "$BREZEL_ENGINE_ORCHESTRATOR_SHA256" "$BREZEL_ENGINE_ENVD_SHA256"

# Compose does not hash bind-mounted script contents and may otherwise reuse a
# completed one-shot container. Force both derived-state gates to run on every
# installation while the API is stopped. The auth-cache gate deletes only the
# engine's `auth:team:*` entries; other Redis state remains intact.
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" \
  rm -sf brezel-engine-auth-cache brezel-engine-capacity
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" up -d --wait

# Re-read the effective engine admission gate after startup. The one-shot
# service applies the value before API boot; this second pass makes a stale or
# overridden database row a hard installation failure.
docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" \
  run --rm --no-deps brezel-engine-capacity /opt/brezel/engine-capacity-contract.sh verify >/dev/null

# The upstream fetcher also verifies these downloads. Verify them again using
# product-owned lock data rather than trusting checksums embedded only in the
# tools image, then write the exact installed distribution record.
"$ARTIFACT_SUPPLY_CHAIN" host "$ENGINE_ARTIFACT_LOCK" / "$BREZEL_ENGINE_ORCHESTRATOR_SHA256" "$BREZEL_ENGINE_ENVD_SHA256"
"$ARTIFACT_SUPPLY_CHAIN" manifest "$INSTALL_DIR/distribution.manifest" "$LOCK_FILE" "$ENGINE_IMAGE_LOCK" "$ENGINE_ARTIFACT_LOCK" / \
  "$BREZEL_ENGINE_ORCHESTRATOR_SHA256" "$ENGINE_ORCHESTRATOR_PATCH_SHA256" "$ENGINE_CACHE_PATCH_SHA256" \
  "$ENGINE_NFS_DURABILITY_PATCH_SHA256" "$ENGINE_START_ADMISSION_PATCH_SHA256" "$ENGINE_LOCAL_CAPACITY_PATCH_SHA256" \
  "$ENGINE_CPU_TOPOLOGY_PATCH_SHA256" "$ENGINE_ROOTFS_READ_PATCH_SHA256" "$ENGINE_NBD_MULTIQUEUE_PATCH_SHA256" \
  "$BREZEL_ENGINE_ENVD_SHA256" "$ENGINE_ENVD_PROCESS_TAG_PATCH_SHA256" \
  "$ENGINE_ENVD_PROCESS_REPLAY_PATCH_SHA256"

docker compose --env-file "$ENGINE_ENV" -f "$ENGINE_COMPOSE" -f "$ENGINE_OVERRIDE" \
  exec -T ready sh -c 'cat /run/e2b/team-api-key' > "$SECRETS_DIR/engine.token"
if [ ! -s "$SECRETS_DIR/engine.token" ]; then
  echo "the local engine did not produce an access token" >&2
  exit 1
fi

if [ ! -s "$SECRETS_DIR/service.token" ]; then
  openssl rand -hex 32 | tr -d '\n' > "$SECRETS_DIR/service.token"
fi
SERVICE_TOKEN=$(tr -d '\r\n' < "$SECRETS_DIR/service.token")
if [ "${#SERVICE_TOKEN}" -lt 32 ]; then
  echo "the runtime service token must contain at least 32 characters" >&2
  exit 1
fi
TOKEN_TMP=$(mktemp "$SECRETS_DIR/.service-token.XXXXXX")
printf '%s' "$SERVICE_TOKEN" > "$TOKEN_TMP"
chmod 600 "$TOKEN_TMP"
mv -f -- "$TOKEN_TMP" "$SECRETS_DIR/service.token"
SERVICE_TOKEN_SHA256=$(sha256sum "$SECRETS_DIR/service.token" | awk '{print $1}')
ACCESS_POLICY_TMP=$(mktemp "$SECRETS_DIR/.access-policy.XXXXXX")
cat > "$ACCESS_POLICY_TMP" <<EOF
{"version":1,"principals":[{"name":"single-host-operator","token_sha256":"$SERVICE_TOKEN_SHA256","projects":["brezel-default","brezel-conformance","brezel-conformance-isolation","brezel-benchmark"]}]}
EOF
chmod 600 "$ACCESS_POLICY_TMP"
mv -f -- "$ACCESS_POLICY_TMP" "$SECRETS_DIR/access-policy.json"
BREZEL_IMAGE=$(docker build -q -f "$REPO_DIR/Dockerfile" "$REPO_DIR")
if [ ! -s "$SECRETS_DIR/receipt.key" ]; then
  docker run --rm \
    --user "$(id -u):$(id -g)" \
    -v "$SECRETS_DIR:/secrets" \
    --entrypoint /usr/local/bin/brezeld \
    "$BREZEL_IMAGE" \
    keygen -out /secrets/receipt.key
fi

if { [ -s "$SECRETS_DIR/node-capability.key" ] && [ ! -s "$SECRETS_DIR/node-capability.pub" ]; } || \
   { [ ! -s "$SECRETS_DIR/node-capability.key" ] && [ -s "$SECRETS_DIR/node-capability.pub" ]; }; then
  echo "node capability key pair is incomplete; restore its matching file before continuing" >&2
  exit 1
fi
if [ ! -s "$SECRETS_DIR/node-capability.key" ]; then
  docker run --rm \
    --user "$(id -u):$(id -g)" \
    -v "$SECRETS_DIR:/secrets" \
    --entrypoint /usr/local/bin/brezeld \
    "$BREZEL_IMAGE" \
    keygen -out /secrets/node-capability.key -public-out /secrets/node-capability.pub
fi
CAPABILITY_PUBLIC_KEY=$(tr -d '\r\n' < "$SECRETS_DIR/node-capability.pub")
CAPABILITY_POLICY_TMP=$(mktemp "$SECRETS_DIR/.node-capability-keys.XXXXXX")
printf '{"version":1,"issuer":"brezel-api","keys":[{"id":"node-key-1","public_key_base64":"%s"}]}\n' "$CAPABILITY_PUBLIC_KEY" > "$CAPABILITY_POLICY_TMP"
chmod 600 "$CAPABILITY_POLICY_TMP"
mv -f -- "$CAPABILITY_POLICY_TMP" "$SECRETS_DIR/node-capability-keys.json"

NODE_TLS_RENEW=false
for tls_file in node-ca.crt node-ca.key node.crt node.key api.crt api.key; do
  if [ ! -s "$SECRETS_DIR/$tls_file" ]; then
    NODE_TLS_RENEW=true
  fi
done
if [ "$NODE_TLS_RENEW" = false ] && ! openssl x509 -checkend 604800 -noout -in "$SECRETS_DIR/node.crt" >/dev/null 2>&1; then
  NODE_TLS_RENEW=true
fi
if [ "$NODE_TLS_RENEW" = false ] && ! openssl x509 -checkend 604800 -noout -in "$SECRETS_DIR/api.crt" >/dev/null 2>&1; then
  NODE_TLS_RENEW=true
fi
if [ "$NODE_TLS_RENEW" = false ] && ! openssl x509 -noout -text -in "$SECRETS_DIR/node-ca.crt" 2>/dev/null | grep -Eq 'CA:[[:space:]]*TRUE'; then
  NODE_TLS_RENEW=true
fi
if [ "$NODE_TLS_RENEW" = false ] && ! openssl x509 -noout -text -in "$SECRETS_DIR/node-ca.crt" 2>/dev/null | grep -Eq 'Certificate Sign'; then
  NODE_TLS_RENEW=true
fi
if [ "$NODE_TLS_RENEW" = false ] && ! openssl verify -purpose sslserver -CAfile "$SECRETS_DIR/node-ca.crt" "$SECRETS_DIR/node.crt" >/dev/null 2>&1; then
  NODE_TLS_RENEW=true
fi
if [ "$NODE_TLS_RENEW" = false ] && ! openssl verify -purpose sslclient -CAfile "$SECRETS_DIR/node-ca.crt" "$SECRETS_DIR/api.crt" >/dev/null 2>&1; then
  NODE_TLS_RENEW=true
fi
if [ "$NODE_TLS_RENEW" = true ]; then
  NODE_TLS_DIR=$(mktemp -d "$SECRETS_DIR/.node-tls.XXXXXX")
  cleanup_node_tls() {
    case "$NODE_TLS_DIR" in
      "$SECRETS_DIR"/.node-tls.*) rm -rf -- "$NODE_TLS_DIR" ;;
    esac
  }
  openssl ecparam -name prime256v1 -genkey -noout -out "$NODE_TLS_DIR/node-ca.key"
  openssl req -x509 -new -sha256 -key "$NODE_TLS_DIR/node-ca.key" -days 365 \
    -subj '/CN=Brezel node CA' \
    -addext 'basicConstraints=critical,CA:TRUE' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign' \
    -out "$NODE_TLS_DIR/node-ca.crt"
  openssl ecparam -name prime256v1 -genkey -noout -out "$NODE_TLS_DIR/node.key"
  openssl req -new -sha256 -key "$NODE_TLS_DIR/node.key" -subj '/CN=node-a.internal' -out "$NODE_TLS_DIR/node.csr"
  printf '%s\n' \
    'basicConstraints=critical,CA:FALSE' \
    'keyUsage=critical,digitalSignature,keyAgreement' \
    'extendedKeyUsage=serverAuth' \
    'subjectAltName=DNS:node-a.internal,IP:127.0.0.1,URI:spiffe://brezel/node/node-a' \
    > "$NODE_TLS_DIR/node.ext"
  openssl x509 -req -sha256 -in "$NODE_TLS_DIR/node.csr" -CA "$NODE_TLS_DIR/node-ca.crt" \
    -CAkey "$NODE_TLS_DIR/node-ca.key" -CAcreateserial -days 30 -extfile "$NODE_TLS_DIR/node.ext" \
    -out "$NODE_TLS_DIR/node.crt"
  openssl ecparam -name prime256v1 -genkey -noout -out "$NODE_TLS_DIR/api.key"
  openssl req -new -sha256 -key "$NODE_TLS_DIR/api.key" -subj '/CN=api-a' -out "$NODE_TLS_DIR/api.csr"
  printf '%s\n' \
    'basicConstraints=critical,CA:FALSE' \
    'keyUsage=critical,digitalSignature,keyAgreement' \
    'extendedKeyUsage=clientAuth' \
    'subjectAltName=URI:spiffe://brezel/api/api-a' \
    > "$NODE_TLS_DIR/api.ext"
  openssl x509 -req -sha256 -in "$NODE_TLS_DIR/api.csr" -CA "$NODE_TLS_DIR/node-ca.crt" \
    -CAkey "$NODE_TLS_DIR/node-ca.key" -CAcreateserial -days 30 -extfile "$NODE_TLS_DIR/api.ext" \
    -out "$NODE_TLS_DIR/api.crt"
  chmod 600 "$NODE_TLS_DIR/node-ca.crt" "$NODE_TLS_DIR/node-ca.key" "$NODE_TLS_DIR/node.crt" "$NODE_TLS_DIR/node.key" "$NODE_TLS_DIR/api.crt" "$NODE_TLS_DIR/api.key"
  for tls_file in node-ca.crt node-ca.key node.crt node.key api.crt api.key; do
    mv -f -- "$NODE_TLS_DIR/$tls_file" "$SECRETS_DIR/$tls_file"
  done
  cleanup_node_tls
fi

chmod 600 "$SECRETS_DIR/engine.token" "$SECRETS_DIR/service.token" "$SECRETS_DIR/access-policy.json" \
  "$SECRETS_DIR/receipt.key" "$SECRETS_DIR/node-capability.key" "$SECRETS_DIR/node-capability.pub" \
  "$SECRETS_DIR/node-capability-keys.json" "$SECRETS_DIR/node-ca.crt" "$SECRETS_DIR/node-ca.key" \
  "$SECRETS_DIR/node.crt" "$SECRETS_DIR/node.key" "$SECRETS_DIR/api.crt" "$SECRETS_DIR/api.key"

BREZEL_STATE_DIR="$STATE_DIR" BREZEL_SECRETS_DIR="$SECRETS_DIR" \
BREZEL_UID="$(id -u)" BREZEL_GID="$(id -g)" \
  docker compose -f "$SCRIPT_DIR/compose.yaml" up -d --build --wait

"$RUNTIME_ATTESTATION" write "$INSTALL_DIR/runtime-attestation.manifest" \
  "$REPO_DIR" "$SCRIPT_DIR/compose.yaml" "$BREZEL_SOURCE_REVISION"
"$RUNTIME_ATTESTATION" verify "$INSTALL_DIR/runtime-attestation.manifest" \
  "$REPO_DIR" "$SCRIPT_DIR/compose.yaml" "$BREZEL_SOURCE_REVISION" >/dev/null

"$SCRIPT_DIR/qualify.sh"

INSTALL_SUCCEEDED=true
echo "Brezel API is listening on http://127.0.0.1:8080"
echo "Read the local CLI token from $SECRETS_DIR/service.token"
