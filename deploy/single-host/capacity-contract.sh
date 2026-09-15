#!/bin/sh
set -eu

# Firecracker reserves guest memory from the host's 2 MiB hugetlb pool before
# lazy paging can begin. Keep the public sandbox quota, engine resource pools,
# and host headroom as one fail-closed contract rather than discovering a
# mismatch through mmap, NBD, or network-slot exhaustion.

fail() {
  echo "capacity contract failed: $*" >&2
  exit 1
}

positive_integer() {
  value=$1
  name=$2
  case "$value" in
    ""|*[!0-9]*) fail "$name must be a positive integer" ;;
  esac
  [ "$value" -gt 0 ] || fail "$name must be greater than zero"
}

non_negative_integer() {
  value=$1
  name=$2
  case "$value" in
    ""|*[!0-9]*) fail "$name must be a non-negative integer" ;;
  esac
}

MODE=${1:-plan}
MEMINFO=${BREZEL_TEST_MEMINFO_FILE:-/proc/meminfo}
STORAGE_PATH=${BREZEL_CAPACITY_STORAGE_PATH:-/var/lib}
GUEST_MEMORY_MIB=${BREZEL_GUEST_MEMORY_MIB:-512}
GUEST_VCPUS=${BREZEL_GUEST_VCPUS:-2}
GUEST_MIN_FREE_DISK_MIB=${BREZEL_GUEST_MIN_FREE_DISK_MIB:-512}
GUEST_MAX_FREE_DISK_MIB=${BREZEL_GUEST_MAX_FREE_DISK_MIB:-25600}
HUGEPAGES=${BREZEL_ENGINE_HUGEPAGES:-9216}
MAX_ACTIVE_TOTAL=${BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL:-32}
MAX_ACTIVE_PROJECT=${BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT:-32}
MAX_STARTING_SANDBOXES=${BREZEL_ENGINE_MAX_STARTING_SANDBOXES:-3}
HEADROOM_SANDBOXES=${BREZEL_LIFECYCLE_HEADROOM_SANDBOXES:-4}
MIN_SYSTEM_MEMORY_MIB=${BREZEL_MIN_SYSTEM_MEMORY_MIB:-8192}
MIN_SYSTEM_DISK_MIB=${BREZEL_MIN_SYSTEM_DISK_MIB:-8192}
NETWORK_NEW_SLOTS=${BREZEL_ENGINE_NETWORK_NEW_SLOTS:-32}
NETWORK_REUSED_SLOTS=${BREZEL_ENGINE_NETWORK_REUSED_SLOTS:-100}
NBD_POOL_SIZE=${BREZEL_ENGINE_NBD_POOL_SIZE:-64}
WARM_POOL_SIZE=${BREZEL_WARM_POOL_SIZE:-0}
WARM_POOL_STRICT=${BREZEL_WARM_POOL_STRICT:-true}
WARM_POOL_TEMPLATE=${BREZEL_WARM_POOL_TEMPLATE:-base}
WARM_POOL_SLOT_TTL_SECONDS=${BREZEL_WARM_POOL_SLOT_TTL_SECONDS:-14400}
WARM_POOL_MAX_CLAIM_TTL_SECONDS=${BREZEL_WARM_POOL_MAX_CLAIM_TTL_SECONDS:-3600}
WARM_POOL_PRIME_CONCURRENCY=${BREZEL_WARM_POOL_PRIME_CONCURRENCY:-1}

positive_integer "$GUEST_MEMORY_MIB" BREZEL_GUEST_MEMORY_MIB
positive_integer "$GUEST_VCPUS" BREZEL_GUEST_VCPUS
positive_integer "$GUEST_MIN_FREE_DISK_MIB" BREZEL_GUEST_MIN_FREE_DISK_MIB
positive_integer "$GUEST_MAX_FREE_DISK_MIB" BREZEL_GUEST_MAX_FREE_DISK_MIB
[ "$GUEST_VCPUS" -le 32 ] || fail "BREZEL_GUEST_VCPUS cannot exceed 32"
[ "$GUEST_MEMORY_MIB" -le 262144 ] || fail "BREZEL_GUEST_MEMORY_MIB cannot exceed 262144"
[ "$GUEST_MIN_FREE_DISK_MIB" -le "$GUEST_MAX_FREE_DISK_MIB" ] || \
  fail "the template free disk cannot exceed the project free-disk ceiling"
positive_integer "$HUGEPAGES" BREZEL_ENGINE_HUGEPAGES
positive_integer "$MAX_ACTIVE_TOTAL" BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL
positive_integer "$MAX_ACTIVE_PROJECT" BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT
positive_integer "$MAX_STARTING_SANDBOXES" BREZEL_ENGINE_MAX_STARTING_SANDBOXES
positive_integer "$HEADROOM_SANDBOXES" BREZEL_LIFECYCLE_HEADROOM_SANDBOXES
positive_integer "$MIN_SYSTEM_MEMORY_MIB" BREZEL_MIN_SYSTEM_MEMORY_MIB
positive_integer "$MIN_SYSTEM_DISK_MIB" BREZEL_MIN_SYSTEM_DISK_MIB
positive_integer "$NETWORK_NEW_SLOTS" BREZEL_ENGINE_NETWORK_NEW_SLOTS
positive_integer "$NETWORK_REUSED_SLOTS" BREZEL_ENGINE_NETWORK_REUSED_SLOTS
positive_integer "$NBD_POOL_SIZE" BREZEL_ENGINE_NBD_POOL_SIZE
non_negative_integer "$WARM_POOL_SIZE" BREZEL_WARM_POOL_SIZE
for bounded in "$MAX_ACTIVE_TOTAL" "$MAX_ACTIVE_PROJECT" "$MAX_STARTING_SANDBOXES" \
  "$HEADROOM_SANDBOXES" "$NETWORK_NEW_SLOTS" "$NETWORK_REUSED_SLOTS" "$NBD_POOL_SIZE"; do
  [ "$bounded" -le 4096 ] || fail "sandbox and resource-pool capacities cannot exceed 4096"
done
[ "$WARM_POOL_SIZE" -le 4096 ] || fail "BREZEL_WARM_POOL_SIZE cannot exceed 4096"
case "$WARM_POOL_STRICT" in
  true|false) ;;
  *) fail "BREZEL_WARM_POOL_STRICT must be true or false" ;;
esac
if [ "$WARM_POOL_SIZE" -gt 0 ] && [ "$WARM_POOL_STRICT" = true ] && [ "$WARM_POOL_SIZE" -ne "$MAX_ACTIVE_TOTAL" ]; then
  fail "strict warm capacity must equal BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL"
fi
if [ "$WARM_POOL_SIZE" -gt 0 ]; then
  positive_integer "$WARM_POOL_SLOT_TTL_SECONDS" BREZEL_WARM_POOL_SLOT_TTL_SECONDS
  positive_integer "$WARM_POOL_MAX_CLAIM_TTL_SECONDS" BREZEL_WARM_POOL_MAX_CLAIM_TTL_SECONDS
  positive_integer "$WARM_POOL_PRIME_CONCURRENCY" BREZEL_WARM_POOL_PRIME_CONCURRENCY
  [ "$WARM_POOL_PRIME_CONCURRENCY" -le "$WARM_POOL_SIZE" ] || \
    fail "warm-pool prime concurrency cannot exceed warm-pool size"
  [ "$WARM_POOL_SLOT_TTL_SECONDS" -gt $((WARM_POOL_MAX_CLAIM_TTL_SECONDS + 30)) ] || \
    fail "warm-pool slot TTL must leave more than 30 seconds beyond the maximum claim TTL"
  case "$WARM_POOL_TEMPLATE" in
    ""|*[!A-Za-z0-9._:-]*|[-.:_]*) fail "BREZEL_WARM_POOL_TEMPLATE is invalid" ;;
  esac
fi
provisioned_capacity=$MAX_ACTIVE_TOTAL
if [ "$WARM_POOL_STRICT" = false ]; then
  provisioned_capacity=$((MAX_ACTIVE_TOTAL + WARM_POOL_SIZE))
fi
[ "$provisioned_capacity" -le 4096 ] || \
  fail "tenant capacity plus non-strict warm capacity cannot exceed 4096"
[ "$MAX_ACTIVE_PROJECT" -le "$MAX_ACTIVE_TOTAL" ] || \
  fail "the per-project active limit cannot exceed the host active limit"
[ "$MAX_STARTING_SANDBOXES" -le "$provisioned_capacity" ] || \
  fail "the local starting-sandbox limit cannot exceed provisioned capacity"
[ "$provisioned_capacity" -le "$NETWORK_NEW_SLOTS" ] || \
  fail "provisioned capacity cannot exceed the $NETWORK_NEW_SLOTS-slot new-sandbox network pool"
[ "$provisioned_capacity" -le "$NETWORK_REUSED_SLOTS" ] || \
  fail "provisioned capacity cannot exceed the $NETWORK_REUSED_SLOTS-slot reused-sandbox network pool"
required_nbd_slots=$((provisioned_capacity + HEADROOM_SANDBOXES))
[ "$required_nbd_slots" -le 4096 ] || \
  fail "tenant capacity plus lifecycle overlap cannot exceed the 4096-device engine ceiling"
[ "$NBD_POOL_SIZE" -ge "$required_nbd_slots" ] || \
  fail "the NBD pool has $NBD_POOL_SIZE slots; require at least $required_nbd_slots for tenant capacity plus lifecycle overlap"

# Each 2 MiB page holds 2 MiB of guest memory. Four additional guest-sized
# reservations cover lifecycle overlap during replacement and checkpoint
# restore without advertising that headroom as tenant capacity.
required_hugepages=$(( ((provisioned_capacity + HEADROOM_SANDBOXES) * GUEST_MEMORY_MIB + 1) / 2 ))
[ "$HUGEPAGES" -ge "$required_hugepages" ] || \
  fail "$HUGEPAGES hugepages cannot back $MAX_ACTIVE_TOTAL active ${GUEST_MEMORY_MIB} MiB sandboxes plus $HEADROOM_SANDBOXES lifecycle-overlap slots; require at least $required_hugepages"

[ -r "$MEMINFO" ] || fail "cannot read host memory information from $MEMINFO"
memory_total_kib=$(awk '/^MemTotal:/ {print $2; exit}' "$MEMINFO")
positive_integer "$memory_total_kib" MemTotal
memory_total_mib=$((memory_total_kib / 1024))
hugepage_memory_mib=$((HUGEPAGES * 2))
required_host_mib=$((hugepage_memory_mib + MIN_SYSTEM_MEMORY_MIB))
[ "$memory_total_mib" -ge "$required_host_mib" ] || \
  fail "host has ${memory_total_mib} MiB RAM; this profile requires at least ${required_host_mib} MiB (${hugepage_memory_mib} MiB sandbox pool plus ${MIN_SYSTEM_MEMORY_MIB} MiB system reserve)"

host_cpu_count=${BREZEL_TEST_CPU_COUNT:-$(getconf _NPROCESSORS_ONLN 2>/dev/null || true)}
positive_integer "$host_cpu_count" host_cpu_count
[ "$host_cpu_count" -ge "$GUEST_VCPUS" ] || \
  fail "host has $host_cpu_count online CPUs; one sandbox requires $GUEST_VCPUS vCPUs"

if [ -n "${BREZEL_TEST_AVAILABLE_DISK_KIB:-}" ]; then
  available_disk_kib=$BREZEL_TEST_AVAILABLE_DISK_KIB
else
  [ -d "$STORAGE_PATH" ] && [ ! -L "$STORAGE_PATH" ] || \
    fail "capacity storage path $STORAGE_PATH must be an existing non-symlink directory"
  available_disk_kib=$(df -Pk "$STORAGE_PATH" | awk 'NR == 2 {print $4; exit}')
fi
positive_integer "$available_disk_kib" available_disk_kib
available_disk_mib=$((available_disk_kib / 1024))
required_disk_mib=$((required_nbd_slots * GUEST_MIN_FREE_DISK_MIB + MIN_SYSTEM_DISK_MIB))
[ "$available_disk_mib" -ge "$required_disk_mib" ] || \
  fail "host has ${available_disk_mib} MiB free at $STORAGE_PATH; this profile requires at least ${required_disk_mib} MiB for sandbox roots plus system reserve"

case "$MODE" in
  plan)
    ;;
  live)
    hugepage_size_kib=$(awk '/^Hugepagesize:/ {print $2; exit}' "$MEMINFO")
    hugepages_total=$(awk '/^HugePages_Total:/ {print $2; exit}' "$MEMINFO")
    hugepages_free=$(awk '/^HugePages_Free:/ {print $2; exit}' "$MEMINFO")
    positive_integer "$hugepage_size_kib" Hugepagesize
    positive_integer "$hugepages_total" HugePages_Total
    positive_integer "$hugepages_free" HugePages_Free
    [ "$hugepage_size_kib" -eq 2048 ] || \
      fail "host hugepage size is ${hugepage_size_kib} KiB; require 2048 KiB"
    [ "$hugepages_total" -ge "$HUGEPAGES" ] || \
      fail "host reserved $hugepages_total hugepages; configured profile requires $HUGEPAGES"
    [ "$hugepages_free" -ge "$required_hugepages" ] || \
      fail "host has $hugepages_free free hugepages; empty-host qualification requires $required_hugepages"
    if [ -n "${BREZEL_TEST_NBD_MAX:-}" ]; then
      kernel_nbd_max=$BREZEL_TEST_NBD_MAX
    else
      [ -r /sys/module/nbd/parameters/nbds_max ] || fail "cannot read the kernel NBD device ceiling"
      kernel_nbd_max=$(cat /sys/module/nbd/parameters/nbds_max)
    fi
    positive_integer "$kernel_nbd_max" kernel_nbd_max
    [ "$kernel_nbd_max" -ge "$NBD_POOL_SIZE" ] || \
      fail "kernel nbd ceiling is $kernel_nbd_max; configured pool requires $NBD_POOL_SIZE"
    ;;
  *)
    echo "usage: $0 plan | live" >&2
    exit 2
    ;;
esac

printf '%s\n' \
  "{\"capacity_contract\":\"conformant\",\"mode\":\"$MODE\",\"guest_vcpus\":$GUEST_VCPUS,\"guest_memory_mib\":$GUEST_MEMORY_MIB,\"guest_min_free_disk_mib\":$GUEST_MIN_FREE_DISK_MIB,\"guest_max_free_disk_mib\":$GUEST_MAX_FREE_DISK_MIB,\"max_active_sandboxes\":$MAX_ACTIVE_TOTAL,\"warm_pool_size\":$WARM_POOL_SIZE,\"strict_warm_pool\":$WARM_POOL_STRICT,\"provisioned_capacity\":$provisioned_capacity,\"max_active_sandboxes_per_project\":$MAX_ACTIVE_PROJECT,\"max_starting_sandboxes\":$MAX_STARTING_SANDBOXES,\"network_new_slots\":$NETWORK_NEW_SLOTS,\"network_reused_slots\":$NETWORK_REUSED_SLOTS,\"nbd_pool_size\":$NBD_POOL_SIZE,\"required_nbd_slots\":$required_nbd_slots,\"hugepages_2m\":$HUGEPAGES,\"required_hugepages_2m\":$required_hugepages,\"lifecycle_headroom_sandboxes\":$HEADROOM_SANDBOXES,\"system_memory_reserve_mib\":$MIN_SYSTEM_MEMORY_MIB,\"host_memory_mib\":$memory_total_mib,\"host_cpu_count\":$host_cpu_count,\"available_disk_mib\":$available_disk_mib,\"required_disk_mib\":$required_disk_mib}"
