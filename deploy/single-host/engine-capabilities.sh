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
    "rootfs.NewRuntimeProvider" "the explicit runtime root filesystem provider boundary"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/provider.go" \
    'case "nbd":' "the default NBD runtime root filesystem provider"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/provider.go" \
    'case "direct":' "the opt-in direct raw-file root filesystem provider"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/provider.go" \
    'case "reflink":' "the opt-in immutable-base reflink root filesystem provider"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/provider.go" \
    'os.O_EXCL' "exclusive private raw-file creation before Firecracker"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink.go" \
    'unix.IoctlFileClone' "same-filesystem FICLONE sandbox root filesystem creation"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink.go" \
    'unix.RENAME_NOREPLACE' "atomic no-replace reflink publication"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink.go" \
    'reflink base is missing the filesystem immutable flag' "immutable cached-base enforcement"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink.go" \
    'rememberVerifiedReflinkDigest' "process-local exact-inode reflink digest memory"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink.go" \
    'openPublishedReflinkBase' "published reflink namespace and inode revalidation"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink_test.go" \
    'TestRememberedReflinkDigestRejectsSameSizeTamper' "same-size reflink tamper regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink_test.go" \
    'TestReflinkDigestCacheIsProcessLocalAndColdOpenStillVerifies' "cold-process full reflink verification regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink_test.go" \
    'TestRememberedReflinkDigestSkipsSecondImageRead' "same-process redundant reflink scan regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink.go" \
    'materializeSparseReflinkContents' "sparse immutable reflink base materialization"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink.go" \
    'reflinkBaseIdentityDomain = "brezel-rootfs-sparse-materialization-v1"' "versioned sparse reflink base identity domain"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink.go" \
    'bytes.Equal(buffer[:length], zeroes[:length])' "zero-chunk reflink write suppression"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink_test.go" \
    'TestSparseReflinkIdentityDoesNotReuseLegacyDenseBase' "sparse reflink upgrade-boundary regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink_test.go" \
    'TestSparseReflinkMaterializationPreservesLogicalBytesAndDigest' "sparse reflink byte and digest identity regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/reflink_test.go" \
    'TestSparseReflinkMaterializationColdVerificationRejectsTamper' "sparse reflink cold-verification tamper regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/direct.go" \
    'newOwnedDirectProvider' "provider ownership for private runtime rootfs clones"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/direct.go" \
    'removeOwnedPath' "provider-owned runtime rootfs clone reclamation"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/direct_lifecycle_test.go" \
    'TestBorrowedDirectProviderExportPreservesTemplateRootfs' "borrowed template-rootfs export preservation regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/direct_lifecycle_test.go" \
    'TestOwnedDirectRuntimeCloseRemovesOnlyPrivateClone' "owned runtime-clone cleanup regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/rootfs/direct_lifecycle_test.go" \
    'TestOwnedDirectProviderExportTimeoutPreservesClone' "unconfirmed export-timeout clone-retention regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/sandbox.go" \
    'serveMemoryAfterOverlayReady(ctx, overlayPromise' "rootfs readiness before arming the UFFD listener"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/resume_resource_order_test.go" \
    'TestServeMemoryAfterOverlayReadyDoesNotArmListenerDuringSlowOverlay' "slow-rootfs UFFD ordering regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/resume_resource_order_test.go" \
    'TestServeMemoryAfterOverlayReadyPreservesCancellation' "rootfs-readiness cancellation regression test"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"SANDBOX_ROOTFS_PROVIDER" envDefault:"nbd"' "the default-off direct rootfs provider selection"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"SANDBOX_ROOTFS_REFLINK_CACHE_DIR"' "the explicit reflink-cache boundary"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'validateSandboxRootfsMountBoundary' "the Firecracker mount-shadowing configuration gate"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model_test.go" \
    'reflink base is below Firecracker mountpoint' "the reflink base mount-shadowing regression test"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model_test.go" \
    'sandbox cache is below Firecracker mountpoint' "the per-sandbox cache mount-shadowing regression test"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model_test.go" \
    'nbd rootfs rejects a sandbox cache below the Firecracker mountpoint' "the NBD sandbox-cache mount-shadowing regression test"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model_test.go" \
    'direct rootfs rejects a sandbox cache below the Firecracker mountpoint' "the direct sandbox-cache mount-shadowing regression test"
  require_literal "$source_root/packages/shared/pkg/storage/sandbox.go" \
    'env:"SANDBOX_CACHE_DIR,expand"' "the explicit per-sandbox cache boundary"
  require_literal "$source_root/packages/shared/pkg/storage/sandbox.go" \
    'fmt.Sprintf("rootfs-%s-%s.cow"' "per-sandbox copy-on-write root filesystem paths"
  require_literal "$source_root/packages/shared/pkg/storage/sandbox.go" \
    'envDefault:"${ORCHESTRATOR_BASE_PATH}/sandbox"' "the local sandbox cache default"
  require_literal "$source_root/packages/shared/pkg/storage/sandbox.go" \
    'envDefault:"${ORCHESTRATOR_BASE_PATH}/template"' "the local template cache default"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/block/local.go" \
    'd.f.ReadAt(p[:length], off)' "the allocation-free local rootfs read path"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/block/overlay.go" \
    'if cacheRangeValid && length > o.blockSize && length%o.blockSize == 0 {' "the whole writable-range rootfs read path"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/block/overlay_read_test.go" \
    'TestOverlayReadAtMixedWritableAndBaseBlocks' "the mixed overlay read regression test"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"NBD_CONNECTIONS_PER_DEVICE"' "the operator-owned NBD queue count"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'NBD_CONNECTIONS_PER_DEVICE must be between 1 and 4' "the bounded NBD queue-count validation"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/nbd/path_direct.go" \
    'WithConnectionsPerDevice' "the fixed NBD per-device queue count"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/nbd/path_direct_lifecycle_test.go" \
    'TestDirectPathMountConnectRetryFullyCleansPreviousAttempt' "the NBD retry-cleanup regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/nbd/path_direct_lifecycle_test.go" \
    'TestDirectPathMountCloseIsSerializedAndIdempotent' "the NBD idempotent-close regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/nbd/path_direct_lifecycle_test.go" \
    'TestDirectPathMountFailsClosedWithoutDevicePool' "the absent NBD-pool fail-closed regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/nbd/pool.go" \
    'slotReleased chan struct{}' "release-signaled NBD saturation backpressure"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/nbd/pool.go" \
    'd.allSlotsReserved()' "all-slot NBD saturation detection"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/nbd/pool_backpressure_test.go" \
    'TestSaturatedPoolWaitsForReleaseSignalWithoutPolling' "128-slot NBD saturation regression test"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/nbd/pool_backpressure_test.go" \
    'TestSaturatedPoolBackpressureHonorsCancelAndClose' "NBD saturation cancellation and close regression test"
  require_literal "$source_root/packages/orchestrator/pkg/factories/run.go" \
    'services.RunsTemplateManager()' "the template-manager-aware NBD pool scope"
  require_literal "$source_root/packages/orchestrator/pkg/factories/run_rootfs_test.go" \
    'TestNewRuntimeDevicePoolOnlySuppressesReflinkWithoutTemplateManager' "the reflink-only NBD pool suppression regression test"

  require_literal "$source_root/packages/orchestrator/pkg/sandbox/network/pool.go" \
    "NewSlotsPoolSize    = 32" "the new network-slot pool"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/network/pool.go" \
    "ReusedSlotsPoolSize = 100" "the reused network-slot pool"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"NETWORK_NEW_SLOTS_POOL_SIZE"' "the configurable new network-slot pool"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"NETWORK_REUSED_SLOTS_POOL_SIZE"' "the configurable reused network-slot pool"
  require_literal "$source_root/packages/orchestrator/pkg/factories/run.go" \
    "network.NewPool(config.NetworkNewSlotsPoolSize, config.NetworkReusedSlotsPoolSize" "configured v1 network pool construction"
  require_literal "$source_root/packages/orchestrator/pkg/factories/run.go" \
    "networkv2.WithPoolSizes(config.NetworkNewSlotsPoolSize, config.NetworkReusedSlotsPoolSize)" "configured v2 network pool construction"
  require_literal "$source_root/packages/orchestrator/pkg/server/sandboxes.go" \
    "if err := sbx.Stop(ctx); err != nil" "delete acknowledgement after bounded sandbox teardown"
  require_literal "$source_root/packages/orchestrator/pkg/server/sandboxes.go" \
    "Sandboxes.WaitLifecycle(ctx" "delete acknowledgement after lifecycle resource reclamation"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/map.go" \
    "func (m *Map) WaitLifecycle(ctx context.Context" "one-lifecycle cleanup completion tracking"

  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"MAX_STARTING_INSTANCES_PER_NODE"' "the operator-owned concurrent-start limit"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"FIRECRACKER_SMT"' "the operator-owned guest SMT setting"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY"' "the opt-in exclusive CPU-topology setting"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"FIRECRACKER_CPUSET_CPUS"' "the complete isolated CPU set"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'is required when FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY=true' "the fail-closed exclusive CPU-topology configuration gate"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/cgroup/manager.go" \
    'cpuset.cpus.partition' "the cgroup v2 isolated-partition validator"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/cgroup/manager.go" \
    'cpuset.cpus.exclusive.effective' "the effective exclusive CPU-set validator"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/fc/cpu_affinity.go" \
    'no NUMA node has %d distinct physical cores' "fail-closed same-NUMA physical-core selection"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/fc/cpu_affinity.go" \
    'another Firecracker process holds the exclusive CPU lease' "host-wide single-Firecracker lease"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/fc/cpu_affinity.go" \
    'Firecracker vCPU thread %d was not present after VM start' "complete vCPU-thread placement verification"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/fc/cpu_affinity.go" \
    'stabilizeExclusiveCPUPlacement' "bounded startup reconciliation for late Firecracker helper threads"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/fc/cpu_affinity.go" \
    'Firecracker is outside the qualified exclusive CPU cgroup' "continuous Firecracker cgroup-membership verification"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/fc/cpu_affinity.go" \
    'validateEffectiveSandboxCPUSet(cgroupPath, placement.reservedCPUs, expectedMems)' "continuous per-sandbox effective-cpuset verification"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/fc/cpu_affinity.go" \
    'return fmt.Errorf("Firecracker sandbox %s drifted", check.name)' "fail-closed effective-cpuset drift handling"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/sandbox.go" \
    'exclusive CPU placement requires sandbox cgroup creation' "fail-closed atomic cgroup placement"
  require_literal "$source_root/packages/orchestrator/pkg/factories/run.go" \
    'exclusive CPU placement requires complete startup resource reclamation' "fail-closed exclusive startup reclamation"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/fc/process.go" \
    'monitorExclusiveCPUPlacement' "continuous fail-closed CPU-affinity reconciliation"
  require_literal "$source_root/packages/orchestrator/pkg/sandbox/fc/process.go" \
    'reconcile exclusive Firecracker CPU topology' "runtime affinity-drift failure propagation"
  require_literal "$source_root/packages/orchestrator/pkg/template/build/buildcontext/context.go" \
    'Ext4DirIndex bool' "the resolved ext4 directory-index build option"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'env:"BUILD_EXT4_DIR_INDEX_TEMPLATE_IDS"' "the operator-owned ext4 directory-index template allowlist"
  require_literal "$source_root/packages/orchestrator/pkg/cfg/model.go" \
    'BuildExt4DirIndexForTemplate' "exact template selection for ext4 directory indexing"
  require_literal "$source_root/packages/orchestrator/pkg/template/build/builder.go" \
    'featureflags.BuildExt4DirIndex' "the per-template ext4 directory-index rollout"
  require_literal "$source_root/packages/orchestrator/pkg/template/build/core/rootfs/rootfs.go" \
    'DirIndex: r.buildContext.Rootfs.Ext4DirIndex' "mkfs using the resolved ext4 directory-index option"
  require_literal "$source_root/packages/orchestrator/pkg/template/build/phases/base/hash.go" \
    'ext4-dir-index:v1' "the ext4 directory-index base-layer cache identity"
  require_literal "$source_root/packages/orchestrator/pkg/server/main.go" \
    "resolveStartingSandboxesLimit" "the local concurrent-start limit resolver"
  require_literal "$source_root/packages/api/internal/orchestrator/placement/placement.go" \
    "resourceExhaustedRetryDelay" "bounded capacity-refusal retry backoff"
  require_literal "$source_root/packages/api/internal/orchestrator/placement/config.go" \
    "resourceExhaustedBackoffMax" "the capacity-refusal retry ceiling"

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
  require_literal "$source_root/embed/compose/scripts/node/build-base-template.mjs" \
    "BASE_TEMPLATE_MIN_FREE_DISK_MB" "the configurable base-template free disk target"
  require_literal "$source_root/embed/compose/scripts/node/build-base-template.mjs" \
    "BASE_TEMPLATE_NAME" "the validated base-template identity"
  require_literal "$source_root/embed/compose/scripts/node/build-base-template.mjs" \
    "name: templateName" "the selected base-template name"
  require_literal "$source_root/embed/compose/scripts/node/build-base-template.mjs" \
    "minFreeDiskMb" "the base-template free disk request"

  require_literal "$source_root/packages/envd/internal/services/process/service.go" \
    'if value.Tag == nil || *value.Tag != tag {' "live process-tag lookup continuing past non-matches"
  require_literal "$source_root/packages/envd/internal/services/process/service_test.go" \
    "TestGetProcessByTagScansPastNonMatches" "the multi-process live tag lookup regression test"
  require_literal "$source_root/packages/envd/internal/services/process/service_test.go" \
    "require.Same(t, target, got)" "the live tag lookup target-identity assertion"

  require_literal "$source_root/packages/envd/internal/services/process/replay.go" \
    'replayVersionHeader = "E2b-Process-Replay-Version"' "the process replay protocol version"
  require_literal "$source_root/packages/envd/internal/services/process/replay.go" \
    'return replayRequest{}, fmt.Errorf("%s is required with %s", journalIDHeader, afterSequenceHeader)' "generation binding for every replay cursor"
  require_literal "$source_root/packages/envd/internal/services/process/handler/journal.go" \
    'defaultJournalBytes       = 8 << 20' "the per-process replay byte bound"
  require_literal "$source_root/packages/envd/internal/services/process/handler/journal.go" \
    'defaultJournalStoreBytes  = 32 << 20' "the envd-wide replay byte bound"
  require_literal "$source_root/packages/envd/internal/services/process/connect_test.go" \
    'TestConnect_ReplayFailsExplicitlyAfterEviction' "the fail-closed replay eviction regression test"
  require_literal "$source_root/packages/envd/internal/services/process/handler/journal_test.go" \
    'TestEventJournalAtomicReplayToWaitHandoff' "the gap-free replay-to-live handoff regression test"

  printf '%s\n' '{"source_contract":"conformant","snapshot_restore":"present","lazy_paging":"present","uffd_listener_lifecycle":"armed-after-rootfs-overlay-ready","template_prefetch":"best_effort_requires_live_gate","cow_rootfs":"present","local_template_cache":"present","rootfs_clone_lifecycle":"provider-owned-runtime-clones-borrowed-template-builds","reflink_digest_cache":"process-local-exact-inode-fingerprint-cold-rehash","reflink_base_materialization":"logical-byte-and-sha-identical-zero-chunks-sparse","reflink_base_identity_domain":"brezel-rootfs-sparse-materialization-v1","rootfs_read_path":{"local":"allocation-free","whole_writable_range":"single-cache-read","mixed_range":"layered-fallback"},"durable_workspace":{"write_acknowledgement":"fsync_before_success","namespace_acknowledgement":"parent_fsync_before_success"},"start_admission":{"local_limit":"present","resource_exhausted_backoff":"bounded-cancellation-aware"},"cpu_topology":{"guest_smt":"operator-configurable-default-disabled","exclusive_placement":"qualified-opt-in-disabled-by-default","isolation":"cgroup-v2-isolated-partition-plus-thread-affinity"},"snapshot_diff_cache":{"configurable_ttl":"present","minimum_ttl_seconds":3600,"physical_byte_high_water":"present","disk_usage_high_water":"present","observation_failure":"evict_conservatively","metrics":"present"},"network_slot_pool":{"operator_configurable":true,"default_new":32,"default_reused":100},"nbd_pool":{"operator_configurable":true,"default":64,"scope":"runtime-provider-or-template-manager-only","saturation":"release-signaled-backpressure-all-kernel-slots-usable","connections_per_device":{"default":1,"minimum":1,"maximum":4,"values_above_one":"pending-kvm-ab-qualification"},"lifecycle":"attempt-owned-idempotent-cleanup"},"base_template":{"cpu_memory_and_free_disk":"operator_configurable","ext4_dir_index":"targeted-opt-in-default-disabled"},"envd_process_lookup":{"live_tag_resolution":"complete-map-scan","regression_test":"multi-process"},"envd_process_output_recovery":{"protocol":"generation-bound-cursor-journal","process_bytes":8388608,"store_bytes":33554432,"eviction":"fail-closed"},"network_version":1}'
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

resolve_sandbox_cache_directory() {
  setting_present=$1
  configured_value=${2:-}

  case "$setting_present" in
    true)
      [ -n "$configured_value" ] || fail "the live SANDBOX_CACHE_DIR setting is empty"
      sandbox_cache_directory=$configured_value
      ;;
    false)
      sandbox_cache_directory=/orchestrator/sandbox
      ;;
    *) fail "the SANDBOX_CACHE_DIR presence marker is invalid" ;;
  esac

  case "$sandbox_cache_directory" in
    /*) ;;
    *) fail "the live SANDBOX_CACHE_DIR is not absolute" ;;
  esac
  case "$sandbox_cache_directory" in
    /|*/|*//*|*/./*|*/../*|*/.|*/..)
      fail "the live SANDBOX_CACHE_DIR is not a clean path below root"
      ;;
  esac

  printf '%s\n' "$sandbox_cache_directory"
}

check_live() {
  sandbox_id=${1:-}
  case "$sandbox_id" in
    ""|*[!A-Za-z0-9._-]*) fail "live mode requires a validated engine sandbox ID" ;;
  esac

  orchestrator_pid=
  uffd_fd_observed=false
  for process in /proc/[0-9]*; do
    [ -r "$process/comm" ] || continue
    [ "$(tr -d '\r\n' < "$process/comm")" = "orchestrator" ] || continue
    for descriptor in "$process"/fd/*; do
      [ -e "$descriptor" ] || [ -L "$descriptor" ] || continue
      if [ "$(readlink "$descriptor" 2>/dev/null || true)" = "anon_inode:[userfaultfd]" ]; then
        orchestrator_pid=${process#/proc/}
        uffd_fd_observed=true
        break
      fi
    done
    [ "$uffd_fd_observed" = true ] && break
  done
  [ -n "$orchestrator_pid" ] || fail "the host orchestrator process is not running with a live userfaultfd descriptor"

  if tr '\000' '\n' < "/proc/$orchestrator_pid/environ" | grep -q '^SANDBOX_CACHE_DIR='; then
    sandbox_cache_directory=$(resolve_sandbox_cache_directory true \
      "$(process_environment_value "$orchestrator_pid" SANDBOX_CACHE_DIR)")
  else
    sandbox_cache_directory=$(resolve_sandbox_cache_directory false)
  fi
  [ -d "$sandbox_cache_directory" ] && [ ! -L "$sandbox_cache_directory" ] || \
    fail "the live SANDBOX_CACHE_DIR is not a real directory"
  canonical_sandbox_cache_directory=$(readlink -f -- "$sandbox_cache_directory") || \
    fail "the live SANDBOX_CACHE_DIR cannot be resolved"
  [ "$canonical_sandbox_cache_directory" = "$sandbox_cache_directory" ] || \
    fail "the live SANDBOX_CACHE_DIR contains a symlink or is not canonical"

  cow_path=
  for candidate in "$sandbox_cache_directory"/rootfs-"$sandbox_id"-*.cow; do
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
  expected_starting_limit=${3:-}
  case "$expected_starting_limit" in
    ""|*[!0-9]*) fail "the expected local starting-sandbox limit must be a positive integer" ;;
  esac
  [ "$expected_starting_limit" -gt 0 ] || fail "the expected local starting-sandbox limit must be greater than zero"
  actual_starting_limit=$(process_environment_value "$orchestrator_pid" MAX_STARTING_INSTANCES_PER_NODE)
  case "$actual_starting_limit" in
    ""|*[!0-9]*) fail "the live local starting-sandbox limit is missing or malformed" ;;
  esac
  [ "$actual_starting_limit" -eq "$expected_starting_limit" ] || \
    fail "the live local starting-sandbox limit is $actual_starting_limit; expected $expected_starting_limit"
  expected_network_new=${4:-}
  expected_network_reused=${5:-}
  expected_nbd_pool=${6:-}
  for expected_capacity in "$expected_network_new" "$expected_network_reused" "$expected_nbd_pool"; do
    case "$expected_capacity" in
      ""|*[!0-9]*) fail "live resource-pool expectations must be positive integers" ;;
    esac
    [ "$expected_capacity" -gt 0 ] || fail "live resource-pool expectations must be greater than zero"
  done
  expected_orchestrator_sha256=$(tr '\000' '\n' < "/proc/$orchestrator_pid/environ" | \
    sed -n 's/^BREZEL_ENGINE_ORCHESTRATOR_SHA256=//p')
  printf '%s\n' "$expected_orchestrator_sha256" | grep -Eq '^[0-9a-f]{64}$' || \
    fail "the live orchestrator is missing its verified binary digest"
  actual_orchestrator_sha256=$(sha256sum "/proc/$orchestrator_pid/exe" | awk '{print $1}')
  [ "$actual_orchestrator_sha256" = "$expected_orchestrator_sha256" ] || \
    fail "the live orchestrator binary digest does not match the source-built artifact"
  actual_network_new=$(process_environment_value "$orchestrator_pid" NETWORK_NEW_SLOTS_POOL_SIZE)
  actual_network_reused=$(process_environment_value "$orchestrator_pid" NETWORK_REUSED_SLOTS_POOL_SIZE)
  actual_nbd_pool=$(process_environment_value "$orchestrator_pid" NBD_POOL_SIZE)
  for actual_capacity in "$actual_network_new" "$actual_network_reused" "$actual_nbd_pool"; do
    case "$actual_capacity" in
      ""|*[!0-9]*) fail "a live local resource-pool size is missing or malformed" ;;
    esac
    [ "$actual_capacity" -gt 0 ] || fail "live local resource-pool sizes must be greater than zero"
  done
  [ "$actual_network_new" -eq "$expected_network_new" ] || \
    fail "the live new network-slot pool is $actual_network_new; expected $expected_network_new"
  [ "$actual_network_reused" -eq "$expected_network_reused" ] || \
    fail "the live reused network-slot pool is $actual_network_reused; expected $expected_network_reused"
  [ "$actual_nbd_pool" -eq "$expected_nbd_pool" ] || \
    fail "the live NBD pool is $actual_nbd_pool; expected $expected_nbd_pool"
  expected_nbd_connections=${7:-}
  case "$expected_nbd_connections" in
    ""|*[!0-9]*) fail "the expected NBD queue count must be an integer from one to four" ;;
  esac
  [ "$expected_nbd_connections" -ge 1 ] && [ "$expected_nbd_connections" -le 4 ] || \
    fail "the expected NBD queue count must be between one and four"
  actual_nbd_connections=$(process_environment_value "$orchestrator_pid" NBD_CONNECTIONS_PER_DEVICE)
  case "$actual_nbd_connections" in
    ""|*[!0-9]*) fail "the live NBD queue count is missing or malformed" ;;
  esac
  [ "$actual_nbd_connections" -eq "$expected_nbd_connections" ] || \
    fail "the live NBD queue count is $actual_nbd_connections; expected $expected_nbd_connections"
  expected_firecracker_smt=${8:-}
  expected_exclusive_cpu_topology=${9:-}
  for expected_boolean in "$expected_firecracker_smt" "$expected_exclusive_cpu_topology"; do
    case "$expected_boolean" in
      true|false) ;;
      *) fail "live Firecracker topology expectations must be true or false" ;;
    esac
  done
  actual_firecracker_smt=$(process_environment_value "$orchestrator_pid" FIRECRACKER_SMT)
  actual_exclusive_cpu_topology=$(process_environment_value "$orchestrator_pid" FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY)
  [ "$actual_firecracker_smt" = "$expected_firecracker_smt" ] || \
    fail "the live Firecracker SMT setting is $actual_firecracker_smt; expected $expected_firecracker_smt"
  [ "$actual_exclusive_cpu_topology" = "$expected_exclusive_cpu_topology" ] || \
    fail "the live exclusive CPU-topology setting is $actual_exclusive_cpu_topology; expected $expected_exclusive_cpu_topology"
  expected_cpuset_cpus=${10:-}
  expected_cpuset_mems=${11:-}
  expected_vcpu_cpus=${12:-}
  expected_vmm_cpus=${13:-}
  actual_cpuset_cpus=$(process_environment_value "$orchestrator_pid" FIRECRACKER_CPUSET_CPUS)
  actual_cpuset_mems=$(process_environment_value "$orchestrator_pid" FIRECRACKER_CPUSET_MEMS)
  actual_vcpu_cpus=$(process_environment_value "$orchestrator_pid" FIRECRACKER_VCPU_CPUS)
  actual_vmm_cpus=$(process_environment_value "$orchestrator_pid" FIRECRACKER_VMM_CPUS)
  [ "$actual_cpuset_cpus" = "$expected_cpuset_cpus" ] || fail "the live Firecracker cpuset differs from the configured contract"
  [ "$actual_cpuset_mems" = "$expected_cpuset_mems" ] || fail "the live Firecracker NUMA set differs from the configured contract"
  [ "$actual_vcpu_cpus" = "$expected_vcpu_cpus" ] || fail "the live Firecracker vCPU set differs from the configured contract"
  [ "$actual_vmm_cpus" = "$expected_vmm_cpus" ] || fail "the live Firecracker VMM set differs from the configured contract"
  if [ "$expected_exclusive_cpu_topology" = true ]; then
    [ -n "$expected_cpuset_cpus" ] && [ -n "$expected_cpuset_mems" ] && [ -n "$expected_vcpu_cpus" ] && [ -n "$expected_vmm_cpus" ] ||
      fail "exclusive CPU topology is enabled without the complete cpuset contract"
  else
    [ -z "$expected_cpuset_cpus$expected_cpuset_mems$expected_vcpu_cpus$expected_vmm_cpus" ] ||
      fail "cpuset values are armed while exclusive CPU topology is disabled"
  fi
  [ -r /sys/module/nbd/parameters/nbds_max ] || \
    fail "the live host does not expose the kernel NBD device ceiling"
  kernel_nbd_max=$(cat /sys/module/nbd/parameters/nbds_max)
  case "$kernel_nbd_max" in
    ""|*[!0-9]*) fail "the live kernel NBD device ceiling is malformed" ;;
  esac
  [ "$kernel_nbd_max" -ge "$expected_nbd_pool" ] || \
    fail "the live kernel exposes $kernel_nbd_max NBD devices; expected at least $expected_nbd_pool"
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
    "{\"live_contract\":\"conformant\",\"snapshot_restore\":\"observed\",\"lazy_paging\":\"observed\",\"template_prefetch\":\"usable_mapping_observed\",\"cow_rootfs\":\"observed\",\"local_template_cache\":\"observed\",\"max_starting_sandboxes\":$actual_starting_limit,\"firecracker_smt\":$actual_firecracker_smt,\"exclusive_cpu_topology\":$actual_exclusive_cpu_topology,\"snapshot_diff_cache\":$cache_policy_json,\"nbd_pool\":$actual_nbd_pool,\"nbd_connections_per_device\":$actual_nbd_connections,\"network_new_slots\":$actual_network_new,\"network_reused_slots\":$actual_network_reused,\"network_version\":1,\"ready_network_namespaces\":$network_slots,\"minimum_ready_network_namespaces\":$min_network_slots}"
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
    echo "usage: $0 source ENGINE_SOURCE_DIR | live ENGINE_SANDBOX_ID MIN_READY_NETWORK_SLOTS MAX_STARTING_SANDBOXES NETWORK_NEW_SLOTS NETWORK_REUSED_SLOTS NBD_POOL_SIZE NBD_CONNECTIONS_PER_DEVICE FIRECRACKER_SMT EXCLUSIVE_CPU_TOPOLOGY CPUSET_CPUS CPUSET_MEMS VCPU_CPUS VMM_CPUS | cache" >&2
    exit 2
    ;;
esac
