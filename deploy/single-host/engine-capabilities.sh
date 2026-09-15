#!/bin/sh
set -eu

# This probe deliberately checks the pinned engine instead of approximating its
# internals in the product controller. "source" binds the release profile to
# audited implementation points. "live" proves that the configured host is
# actually exercising those points for a running sandbox. Neither mode is a
# latency benchmark.

fail() {
  echo "engine capability check failed: $*" >&2
  exit 1
}

require_file() {
  [ -f "$1" ] || fail "missing audited source file $1"
}

require_literal() {
  file=$1
  literal=$2
  description=$3
  require_file "$file"
  grep -Fq -- "$literal" "$file" || fail "$description is absent from $file"
}

check_source() {
  source_root=${1:-}
  [ -n "$source_root" ] || fail "source mode requires the extracted engine directory"
  [ -d "$source_root" ] || fail "engine source directory does not exist: $source_root"

  require_literal "$source_root/packages/orchestrator/pkg/sandbox/fc/client.go" \
    "models.MemoryBackendBackendTypeUffd" "the Firecracker UFFD memory backend"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/fc/client.go" \
    "Operations.LoadSnapshot" "Firecracker snapshot restore"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/fc/client.go" \
    "ResumeVM:            false" "the wait-for-UFFD-before-resume ordering"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/uffd/uffd.go" \
    "userfaultfd.NewUserfaultfdFromFd" "the userfaultfd lazy paging server"

  require_literal "$source_root/packages/orchestrator/pkg/template/build/builder.go" \
    "builders = append(builders, optimizeBuilder)" "the template prefetch optimization phase"
  require_literal "$source_root/packages/orchestrator/pkg/template/build/phases/optimize/builder.go" \
    "WithPrefetch(&metadata.Prefetch" "prefetch mapping persistence"
  require_literal "$source_root/packages/orchestrator/pkg/template/build/phases/optimize/builder.go" \
    "continuing without prefetch" "the upstream best-effort prefetch behavior audited by live qualification"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/sandbox.go" \
    "prefetch.New(sbxLogger, memfile, fcUffd, initMapping" "startup prefetch replay on sandbox resume"
  require_literal "$source_root/packages/shared/pkg/featureflags/flags.go" \
    'NewStringFlag("resume-prefetch-source", "init")' "the qualified init-prefetch default"

  require_literal "$source_root/packages/orchestrator/pkg/sandbox/sandbox.go" \
    "rootfs.NewNBDProvider" "the NBD copy-on-write root filesystem provider"
  require_literal "$source_root/packages/shared/pkg/storage/sandbox.go" \
    'fmt.Sprintf("rootfs-%s-%s.cow"' "per-sandbox copy-on-write root filesystem paths"
  require_literal "$source_root/packages/shared/pkg/storage/sandbox.go" \
    'envDefault:"${ORCHESTRATOR_BASE_PATH}/sandbox"' "the local sandbox cache default"
  require_literal "$source_root/packages/shared/pkg/storage/sandbox.go" \
    'envDefault:"${ORCHESTRATOR_BASE_PATH}/template"' "the local template cache default"

  require_literal "$source_root/packages/orchestrator/pkg/sandbox/network/pool.go" \
    "NewSlotsPoolSize    = 32" "the new network-slot pool"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/network/pool.go" \
    "ReusedSlotsPoolSize = 100" "the reused network-slot pool"
  require_literal "$source_root/packages/orchestrator/pkg/factories/run.go" \
    "network.NewPool(network.NewSlotsPoolSize, network.ReusedSlotsPoolSize" "network pool construction"
  require_literal "$source_root/packages/orchestrator/pkg/server/sandboxes.go" \
    "if err := sbx.Stop(ctx); err != nil" "delete acknowledgement after bounded sandbox teardown"
  require_literal "$source_root/packages/orchestrator/pkg/server/sandboxes.go" \
    "Sandboxes.WaitLifecycle(ctx" "delete acknowledgement after lifecycle resource reclamation"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/map.go" \
    "func (m *Map) WaitLifecycle(ctx context.Context" "one-lifecycle cleanup completion tracking"

  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"BUILD_CACHE_TTL"' "the configurable snapshot-diff cache TTL"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"BUILD_CACHE_MAX_BYTES"' "the snapshot-diff cache byte high water"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT"' "the local disk high water"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'BUILD_CACHE_TTL must be at least 1h' "the safe cache TTL floor"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'BUILD_CACHE_MAX_BYTES must be zero or at least 1 GiB' "the safe cache byte-bound floor"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT must be between 1 and 100' "the cache disk high-water validation"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/template/cache.go" \
    'config.BuildCacheTTL' "the configured TTL at diff-store construction"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/build/cache.go" \
    'func allocatedBytes(path string)' "physical-block cache accounting"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/build/cache.go" \
    'cachePressureObservationFailed' "fail-closed cache observation"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/build/cache.go" \
    'cachePressureAllocatedBytes' "absolute cache pressure"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/build/cache.go" \
    'orchestrator.build.cache.pressure_evictions' "cache pressure eviction metrics"

  require_literal "$source_root/packages/orchestrator/pkg/nfsproxy/chroot/file.go" \
    'syncing NFS write' "fsync before a stable NFS write acknowledgement"
  require_literal "$source_root/packages/orchestrator/pkg/nfsproxy/chroot/file.go" \
    'syncing NFS truncate' "fsync before a stable NFS truncate acknowledgement"
  require_literal "$source_root/packages/orchestrator/pkg/nfsproxy/chroot/fs.go" \
    'syncDirectoryTree' "directory-tree durability after recursive creation"
  require_literal "$source_root/packages/orchestrator/pkg/nfsproxy/chroot/fs.go" \
    'errors.Join(syncPath(f.chroot, newParent), syncPath(f.chroot, oldParent))' "both-parent durability after cross-directory rename"

  require_literal "$source_root/embed/compose/compose.yaml" \
    'TEMPLATE_STORAGE_URL: file:///var/lib/e2b/storage/templates' "local template artifact storage"
  require_literal "$source_root/embed/compose/compose.yaml" \
    'NBD_POOL_SIZE: "64"' "the pinned NBD device pool"
  require_literal "$source_root/embed/compose/compose.yaml" \
    'NETWORK_VERSION: "1"' "the qualified network implementation"

  printf '%s\n' '{"source_contract":"conformant","snapshot_restore":"present","lazy_paging":"present","template_prefetch":"best_effort_requires_live_gate","cow_rootfs":"present","local_template_cache":"present","durable_workspace":{"write_acknowledgement":"fsync_before_success","namespace_acknowledgement":"parent_fsync_before_success"},"snapshot_diff_cache":{"configurable_ttl":"present","minimum_ttl_seconds":3600,"physical_byte_high_water":"present","disk_usage_high_water":"present","observation_failure":"evict_conservatively","metrics":"present"},"network_slot_pool":{"new":32,"reused":100},"nbd_pool":64,"network_version":1}'
}

find_orchestrator_pid() {
  for process in /proc/[0-9]*; do
    [ -r "$process/comm" ] || continue
    [ "$(tr -d '\r\n' < "$process/comm")" = "orchestrator" ] || continue
    printf '%s\n' "${process#/proc/}"
    return 0
  done
  return 1
}

process_environment_value() {
  process_id=$1
  key=$2
  tr '\000' '\n' < "/proc/$process_id/environ" | sed -n "s/^${key}=//p"
}

observe_cache_policy() {
  process_id=$1
  cache_ttl=$(process_environment_value "$process_id" BUILD_CACHE_TTL)
  cache_max_bytes=$(process_environment_value "$process_id" BUILD_CACHE_MAX_BYTES)
  disk_high_water=$(process_environment_value "$process_id" BUILD_CACHE_DISK_USAGE_HIGH_WATER_PERCENT)

  case "$cache_ttl" in
    *h)
      cache_ttl_hours=${cache_ttl%h}
      case "$cache_ttl_hours" in ""|*[!0-9]*) fail "the live cache TTL is malformed" ;; esac
      [ "$cache_ttl_hours" -ge 1 ] && [ "$cache_ttl_hours" -le 168 ] || \
        fail "the live cache TTL is outside the qualified 1h to 168h range"
      ;;
    *) fail "the live cache TTL is not a whole number of hours" ;;
  esac
  case "$cache_max_bytes" in ""|*[!0-9]*) fail "the live cache byte high water is malformed" ;; esac
  [ "$cache_max_bytes" -ge 1073741824 ] || fail "the live cache byte high water is below 1 GiB"
  case "$disk_high_water" in ""|*[!0-9]*) fail "the live disk high water is malformed" ;; esac
  [ "$disk_high_water" -ge 50 ] && [ "$disk_high_water" -le 90 ] || \
    fail "the live disk high water is outside the qualified 50 to 90 percent range"

  cache_root=/orchestrator/build
  [ -d "$cache_root" ] || fail "the live snapshot-diff cache directory is missing"
  cache_allocated_bytes=$(du -sx -B1 "$cache_root" | awk 'NR == 1 {print $1}')
  cache_file_count=$(find "$cache_root" -type f | wc -l | tr -d ' ')
  set -- $(df -P -B1 "$cache_root" | awk 'NR == 2 {print $2, $3, $4}')
  disk_total_bytes=$1
  disk_used_bytes=$2
  disk_available_bytes=$3
  for observed_number in "$cache_allocated_bytes" "$cache_file_count" "$disk_total_bytes" "$disk_used_bytes" "$disk_available_bytes"; do
    case "$observed_number" in ""|*[!0-9]*) fail "the live cache storage observation is malformed" ;; esac
  done

  printf '%s\n' \
    "{\"ttl\":\"$cache_ttl\",\"max_allocated_bytes\":$cache_max_bytes,\"disk_usage_high_water_percent\":$disk_high_water,\"observed\":{\"allocated_bytes\":$cache_allocated_bytes,\"file_count\":$cache_file_count,\"disk_total_bytes\":$disk_total_bytes,\"disk_used_bytes\":$disk_used_bytes,\"disk_available_bytes\":$disk_available_bytes}}"
}

has_nonempty_prefetch() {
  metadata=$1
  [ -s "$metadata" ] || return 1
  # Metadata is small. Removing whitespace makes this independent of the JSON
  # encoder's formatting without introducing jq into the minimal host profile.
  tr -d '[:space:]' < "$metadata" | grep -Eq '"prefetch":\{.*"memory":\{.*"indices":\[[0-9]'
}

check_live() {
  sandbox_id=${1:-}
  case "$sandbox_id" in
    ""|*[!A-Za-z0-9._-]*) fail "live mode requires a validated engine sandbox ID" ;;
  esac

  cow_path=
  for candidate in /orchestrator/sandbox/rootfs-"$sandbox_id"-*.cow; do
    if [ -f "$candidate" ] && [ -s "$candidate" ]; then
      cow_path=$candidate
      break
    fi
  done
  [ -n "$cow_path" ] || fail "no non-empty COW root filesystem is active for $sandbox_id"

  uffd_socket=
  for candidate in /tmp/uffd-"$sandbox_id"-*.sock; do
    if [ -S "$candidate" ]; then
      uffd_socket=$candidate
      break
    fi
  done
  [ -n "$uffd_socket" ] || fail "no UFFD socket is active for $sandbox_id"

  orchestrator_pid=
  uffd_fd_observed=false
  for process in /proc/[0-9]*; do
    [ -r "$process/comm" ] || continue
    [ "$(tr -d '\r\n' < "$process/comm")" = "orchestrator" ] || continue
    orchestrator_pid=${process#/proc/}
    for descriptor in "$process"/fd/*; do
      [ -e "$descriptor" ] || [ -L "$descriptor" ] || continue
      if [ "$(readlink "$descriptor" 2>/dev/null || true)" = "anon_inode:[userfaultfd]" ]; then
        uffd_fd_observed=true
        break
      fi
    done
    [ "$uffd_fd_observed" = true ] && break
  done
  [ -n "$orchestrator_pid" ] || fail "the host orchestrator process is not running"
  [ "$uffd_fd_observed" = true ] || fail "the orchestrator has no live userfaultfd descriptor"
  expected_orchestrator_sha256=$(tr '\000' '\n' < "/proc/$orchestrator_pid/environ" | \
    sed -n 's/^BREZEL_ENGINE_ORCHESTRATOR_SHA256=//p')
  printf '%s\n' "$expected_orchestrator_sha256" | grep -Eq '^[0-9a-f]{64}$' || \
    fail "the live orchestrator is missing its verified binary digest"
  actual_orchestrator_sha256=$(sha256sum "/proc/$orchestrator_pid/exe" | awk '{print $1}')
  [ "$actual_orchestrator_sha256" = "$expected_orchestrator_sha256" ] || \
    fail "the live orchestrator binary digest does not match the source-built artifact"
  tr '\000' '\n' < "/proc/$orchestrator_pid/environ" | grep -Fxq 'NBD_POOL_SIZE=64' || \
    fail "the live orchestrator is not using the qualified 64-device NBD pool"
  tr '\000' '\n' < "/proc/$orchestrator_pid/environ" | grep -Fxq 'NETWORK_VERSION=1' || \
    fail "the live orchestrator is not using qualified network version 1"
  tr '\000' '\n' < "/proc/$orchestrator_pid/environ" | \
    grep -Fxq 'TEMPLATE_STORAGE_URL=file:///var/lib/e2b/storage/templates' || \
    fail "the live orchestrator is not using qualified local template storage"

  min_network_slots=${2:-16}
  case "$min_network_slots" in
    ""|*[!0-9]*) fail "BREZEL_MIN_READY_NETWORK_SLOTS must be a non-negative integer" ;;
  esac
  network_slots=0
  network_wait=0
  while [ "$network_wait" -lt 30 ]; do
    network_slots=0
    for namespace in /var/run/netns/ns-*; do
      [ -e "$namespace" ] || continue
      network_slots=$((network_slots + 1))
    done
    [ "$network_slots" -ge "$min_network_slots" ] && break
    network_wait=$((network_wait + 1))
    sleep 1
  done
  [ "$network_slots" -ge "$min_network_slots" ] || \
    fail "only $network_slots pooled network namespaces are ready; require at least $min_network_slots"

  snapshot_dir=
  prefetch_metadata=
  for metadata in /var/lib/e2b/storage/templates/*/metadata.json; do
    [ -f "$metadata" ] || continue
    artifact_dir=${metadata%/metadata.json}
    if [ -s "$artifact_dir/memfile" ] && [ -s "$artifact_dir/rootfs.ext4" ] && [ -s "$artifact_dir/snapfile" ]; then
      [ -n "$snapshot_dir" ] || snapshot_dir=$artifact_dir
    fi
    if has_nonempty_prefetch "$metadata"; then
      prefetch_metadata=$metadata
      break
    fi
  done
  [ -n "$snapshot_dir" ] || fail "local template storage has no complete memory snapshot artifact set"

  cached_metadata=
  for metadata in /orchestrator/template/*/cache/*/metadata.json; do
    if [ -s "$metadata" ]; then
      cached_metadata=$metadata
      if [ -z "$prefetch_metadata" ] && has_nonempty_prefetch "$metadata"; then
        prefetch_metadata=$metadata
      fi
    fi
  done
  [ -n "$cached_metadata" ] || fail "the running sandbox has not populated the local template cache"
  [ -n "$prefetch_metadata" ] || \
    fail "no usable memory prefetch mapping was produced; upstream template optimization is best effort"

  cache_policy_json=$(observe_cache_policy "$orchestrator_pid")

  printf '%s\n' \
    "{\"live_contract\":\"conformant\",\"snapshot_restore\":\"observed\",\"lazy_paging\":\"observed\",\"template_prefetch\":\"usable_mapping_observed\",\"cow_rootfs\":\"observed\",\"local_template_cache\":\"observed\",\"snapshot_diff_cache\":$cache_policy_json,\"nbd_pool\":64,\"network_version\":1,\"ready_network_namespaces\":$network_slots,\"minimum_ready_network_namespaces\":$min_network_slots}"
}

check_cache() {
  orchestrator_pid=$(find_orchestrator_pid) || fail "the host orchestrator process is not running"
  observe_cache_policy "$orchestrator_pid"
}

case "${1:-}" in
  source)
    shift
    check_source "$@"
    ;;
  live)
    shift
    check_live "$@"
    ;;
  cache)
    shift
    [ "$#" -eq 0 ] || fail "cache mode accepts no arguments"
    check_cache
    ;;
  *)
    echo "usage: $0 source ENGINE_SOURCE_DIR | live ENGINE_SANDBOX_ID [MIN_READY_NETWORK_SLOTS] | cache" >&2
    exit 2
    ;;
esac
