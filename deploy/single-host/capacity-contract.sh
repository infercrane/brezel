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

normalize_index_set() {
  value=$1
  printf '%s\n' "$value" | awk '
    BEGIN { valid=1 }
    {
      count=split($0, parts, ",")
      if (count < 1) valid=0
      for (i=1; i<=count; i++) {
        if (parts[i] !~ /^[0-9]+(-[0-9]+)?$/) { valid=0; continue }
        bounds=split(parts[i], range, "-")
        first=range[1]+0; last=first
        if (bounds == 2) last=range[2]+0
        if (last < first || last-first > 1048576) { valid=0; continue }
        for (n=first; n<=last; n++) print n
      }
    }
    END { if (!valid) exit 2 }
  ' | sort -n -u | paste -sd, -
}

set_has() {
  set=$1
  needle=$2
  case ",$set," in
    *",$needle,"*) return 0 ;;
    *) return 1 ;;
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
MIN_SYSTEM_CPUS=${BREZEL_MIN_SYSTEM_CPUS:-2}
FIRECRACKER_SMT=${BREZEL_ENGINE_FIRECRACKER_SMT:-false}
EXCLUSIVE_CPU_TOPOLOGY=${BREZEL_ENGINE_FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY:-false}
FIRECRACKER_CPUSET_CPUS=${BREZEL_ENGINE_FIRECRACKER_CPUSET_CPUS:-}
FIRECRACKER_CPUSET_MEMS=${BREZEL_ENGINE_FIRECRACKER_CPUSET_MEMS:-}
FIRECRACKER_VCPU_CPUS=${BREZEL_ENGINE_FIRECRACKER_VCPU_CPUS:-}
FIRECRACKER_VMM_CPUS=${BREZEL_ENGINE_FIRECRACKER_VMM_CPUS:-}
CPU_TOPOLOGY_FILE=${BREZEL_TEST_CPU_TOPOLOGY_FILE:-}
MIN_SYSTEM_DISK_MIB=${BREZEL_MIN_SYSTEM_DISK_MIB:-8192}
NETWORK_NEW_SLOTS=${BREZEL_ENGINE_NETWORK_NEW_SLOTS:-32}
NETWORK_REUSED_SLOTS=${BREZEL_ENGINE_NETWORK_REUSED_SLOTS:-100}
NBD_POOL_SIZE=${BREZEL_ENGINE_NBD_POOL_SIZE:-64}
NBD_CONNECTIONS_PER_DEVICE=${BREZEL_ENGINE_NBD_CONNECTIONS_PER_DEVICE:-1}
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
positive_integer "$MIN_SYSTEM_CPUS" BREZEL_MIN_SYSTEM_CPUS
positive_integer "$MIN_SYSTEM_DISK_MIB" BREZEL_MIN_SYSTEM_DISK_MIB
positive_integer "$NETWORK_NEW_SLOTS" BREZEL_ENGINE_NETWORK_NEW_SLOTS
positive_integer "$NETWORK_REUSED_SLOTS" BREZEL_ENGINE_NETWORK_REUSED_SLOTS
positive_integer "$NBD_POOL_SIZE" BREZEL_ENGINE_NBD_POOL_SIZE
positive_integer "$NBD_CONNECTIONS_PER_DEVICE" BREZEL_ENGINE_NBD_CONNECTIONS_PER_DEVICE
[ "$NBD_CONNECTIONS_PER_DEVICE" -le 4 ] || \
  fail "BREZEL_ENGINE_NBD_CONNECTIONS_PER_DEVICE cannot exceed 4"
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
case "$FIRECRACKER_SMT" in
  true|false) ;;
  *) fail "BREZEL_ENGINE_FIRECRACKER_SMT must be true or false" ;;
esac
case "$EXCLUSIVE_CPU_TOPOLOGY" in
  true|false) ;;
  *) fail "BREZEL_ENGINE_FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY must be true or false" ;;
esac
normalized_cpuset_cpus=
normalized_cpuset_mems=
normalized_vcpu_cpus=
normalized_vmm_cpus=
if [ "$EXCLUSIVE_CPU_TOPOLOGY" = true ]; then
  [ "$FIRECRACKER_SMT" = false ] || fail "exclusive Firecracker CPU topology requires guest SMT to be disabled"
  [ -n "$FIRECRACKER_CPUSET_CPUS" ] || fail "BREZEL_ENGINE_FIRECRACKER_CPUSET_CPUS is required for exclusive placement"
  [ -n "$FIRECRACKER_CPUSET_MEMS" ] || fail "BREZEL_ENGINE_FIRECRACKER_CPUSET_MEMS is required for exclusive placement"
  [ -n "$FIRECRACKER_VCPU_CPUS" ] || fail "BREZEL_ENGINE_FIRECRACKER_VCPU_CPUS is required for exclusive placement"
  [ -n "$FIRECRACKER_VMM_CPUS" ] || fail "BREZEL_ENGINE_FIRECRACKER_VMM_CPUS is required for exclusive placement"
  normalized_cpuset_cpus=$(normalize_index_set "$FIRECRACKER_CPUSET_CPUS") || fail "BREZEL_ENGINE_FIRECRACKER_CPUSET_CPUS is invalid"
  normalized_cpuset_mems=$(normalize_index_set "$FIRECRACKER_CPUSET_MEMS") || fail "BREZEL_ENGINE_FIRECRACKER_CPUSET_MEMS is invalid"
  normalized_vcpu_cpus=$(normalize_index_set "$FIRECRACKER_VCPU_CPUS") || fail "BREZEL_ENGINE_FIRECRACKER_VCPU_CPUS is invalid"
  normalized_vmm_cpus=$(normalize_index_set "$FIRECRACKER_VMM_CPUS") || fail "BREZEL_ENGINE_FIRECRACKER_VMM_CPUS is invalid"
  case "$normalized_cpuset_mems" in ""|*,*) fail "exclusive placement requires exactly one NUMA memory node" ;; esac
  vcpu_count=$(printf '%s\n' "$normalized_vcpu_cpus" | awk -F, '{print NF}')
  [ "$vcpu_count" -eq "$GUEST_VCPUS" ] || fail "exclusive placement assigns $vcpu_count host CPUs to a $GUEST_VCPUS-vCPU guest"
  old_ifs=$IFS
  IFS=,
  for cpu in $normalized_vcpu_cpus; do
    set_has "$normalized_cpuset_cpus" "$cpu" || fail "vCPU CPU $cpu is outside the isolated cpuset"
    set_has "$normalized_vmm_cpus" "$cpu" && fail "CPU $cpu is assigned to both a vCPU and the VMM"
  done
  for cpu in $normalized_vmm_cpus; do
    set_has "$normalized_cpuset_cpus" "$cpu" || fail "VMM CPU $cpu is outside the isolated cpuset"
  done
  IFS=$old_ifs
else
  [ -z "$FIRECRACKER_CPUSET_CPUS$FIRECRACKER_CPUSET_MEMS$FIRECRACKER_VCPU_CPUS$FIRECRACKER_VMM_CPUS" ] ||
    fail "Firecracker cpuset settings require exclusive CPU topology to be enabled"
fi
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
if [ "$EXCLUSIVE_CPU_TOPOLOGY" = true ]; then
  [ "$provisioned_capacity" -eq 1 ] || \
    fail "exclusive Firecracker CPU topology is a single-sandbox profile until a physical-core allocator is qualified"
  [ "$MAX_STARTING_SANDBOXES" -eq 1 ] || \
    fail "exclusive Firecracker CPU topology requires a one-sandbox start limit"
fi
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
required_host_cpus=$((GUEST_VCPUS + MIN_SYSTEM_CPUS))
[ "$host_cpu_count" -ge "$required_host_cpus" ] || \
  fail "host has $host_cpu_count online CPUs; one sandbox requires $GUEST_VCPUS vCPUs and the profile reserves $MIN_SYSTEM_CPUS host CPUs for Firecracker, storage, networking, and system work (require at least $required_host_cpus)"

max_numa_physical_cores=0
host_physical_cores=0
if [ "$EXCLUSIVE_CPU_TOPOLOGY" = true ]; then
  if [ -n "$CPU_TOPOLOGY_FILE" ]; then
    [ -r "$CPU_TOPOLOGY_FILE" ] || fail "cannot read test CPU topology from $CPU_TOPOLOGY_FILE"
    cpu_topology=$(cat "$CPU_TOPOLOGY_FILE")
  else
    command -v lscpu >/dev/null 2>&1 || fail "lscpu is required to qualify exclusive Firecracker CPU topology"
    cpu_topology=$(lscpu -p=CPU,NODE,SOCKET,CORE 2>/dev/null) || \
      fail "could not read host CPU topology"
  fi
  topology_counts=$(printf '%s\n' "$cpu_topology" | awk -F, '
    $0 !~ /^#/ && NF >= 4 {
      node=$2; if (node == "" || node == "-") node=0
      key=node SUBSEP $3 SUBSEP $4
      if (!seen[key]++) { per_node[node]++; total++ }
    }
    END {
      max=0
      for (node in per_node) if (per_node[node] > max) max=per_node[node]
      print max, total
    }
  ')
  max_numa_physical_cores=${topology_counts%% *}
  host_physical_cores=${topology_counts#* }
  positive_integer "$max_numa_physical_cores" max_numa_physical_cores
  positive_integer "$host_physical_cores" host_physical_cores
  required_numa_cores=$((GUEST_VCPUS + 1))
  required_physical_cores=$((GUEST_VCPUS + 4))
  [ "$max_numa_physical_cores" -ge "$required_numa_cores" ] || \
    fail "no NUMA node has $required_numa_cores distinct physical cores for the guest plus VMM"
  [ "$host_physical_cores" -ge "$required_physical_cores" ] || \
    fail "exclusive placement requires at least $required_physical_cores physical cores: guest, VMM, NBD, and housekeeping"
fi

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
    if [ "$EXCLUSIVE_CPU_TOPOLOGY" = true ]; then
      partition=/sys/fs/cgroup/e2b
      [ -d "$partition" ] && [ ! -L "$partition" ] || fail "the exclusive Firecracker cgroup is missing or is a symlink"
      [ "$(cat "$partition/cpuset.cpus.partition" 2>/dev/null || true)" = isolated ] || fail "the Firecracker cgroup is not an isolated cpuset partition"
      live_cpus=$(normalize_index_set "$(cat "$partition/cpuset.cpus.effective" 2>/dev/null || true)") || fail "cannot read the effective Firecracker cpuset"
      live_exclusive=$(normalize_index_set "$(cat "$partition/cpuset.cpus.exclusive.effective" 2>/dev/null || true)") || fail "cannot read the exclusive Firecracker cpuset"
      live_mems=$(normalize_index_set "$(cat "$partition/cpuset.mems.effective" 2>/dev/null || true)") || fail "cannot read the Firecracker NUMA set"
      [ "$live_cpus" = "$normalized_cpuset_cpus" ] && [ "$live_exclusive" = "$normalized_cpuset_cpus" ] && [ "$live_mems" = "$normalized_cpuset_mems" ] ||
        fail "the live Firecracker cpuset partition differs from the configured contract"
      [ ! -s "$partition/cgroup.procs" ] || fail "the Firecracker partition contains direct processes instead of sandbox child cgroups"
      node_hugepages=/sys/devices/system/node/node${normalized_cpuset_mems}/hugepages/hugepages-2048kB
      [ -r "$node_hugepages/nr_hugepages" ] && [ -r "$node_hugepages/free_hugepages" ] || fail "cannot read 2 MiB hugepages for NUMA node $normalized_cpuset_mems"
      node_hugepages_total=$(cat "$node_hugepages/nr_hugepages")
      node_hugepages_free=$(cat "$node_hugepages/free_hugepages")
      positive_integer "$node_hugepages_total" node_hugepages_total
      positive_integer "$node_hugepages_free" node_hugepages_free
      [ "$node_hugepages_total" -ge "$HUGEPAGES" ] || fail "NUMA node $normalized_cpuset_mems has only $node_hugepages_total reserved hugepages; require $HUGEPAGES"
      [ "$node_hugepages_free" -ge "$required_hugepages" ] || fail "NUMA node $normalized_cpuset_mems has only $node_hugepages_free free hugepages; require $required_hugepages"
    fi
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
  "{\"capacity_contract\":\"conformant\",\"mode\":\"$MODE\",\"guest_vcpus\":$GUEST_VCPUS,\"guest_memory_mib\":$GUEST_MEMORY_MIB,\"guest_min_free_disk_mib\":$GUEST_MIN_FREE_DISK_MIB,\"guest_max_free_disk_mib\":$GUEST_MAX_FREE_DISK_MIB,\"max_active_sandboxes\":$MAX_ACTIVE_TOTAL,\"warm_pool_size\":$WARM_POOL_SIZE,\"strict_warm_pool\":$WARM_POOL_STRICT,\"provisioned_capacity\":$provisioned_capacity,\"max_active_sandboxes_per_project\":$MAX_ACTIVE_PROJECT,\"max_starting_sandboxes\":$MAX_STARTING_SANDBOXES,\"network_new_slots\":$NETWORK_NEW_SLOTS,\"network_reused_slots\":$NETWORK_REUSED_SLOTS,\"nbd_pool_size\":$NBD_POOL_SIZE,\"nbd_connections_per_device\":$NBD_CONNECTIONS_PER_DEVICE,\"required_nbd_slots\":$required_nbd_slots,\"hugepages_2m\":$HUGEPAGES,\"required_hugepages_2m\":$required_hugepages,\"lifecycle_headroom_sandboxes\":$HEADROOM_SANDBOXES,\"system_memory_reserve_mib\":$MIN_SYSTEM_MEMORY_MIB,\"system_cpu_reserve\":$MIN_SYSTEM_CPUS,\"firecracker_smt\":$FIRECRACKER_SMT,\"exclusive_cpu_topology\":$EXCLUSIVE_CPU_TOPOLOGY,\"firecracker_cpuset_cpus\":\"$normalized_cpuset_cpus\",\"firecracker_cpuset_mems\":\"$normalized_cpuset_mems\",\"firecracker_vcpu_cpus\":\"$normalized_vcpu_cpus\",\"firecracker_vmm_cpus\":\"$normalized_vmm_cpus\",\"max_numa_physical_cores\":$max_numa_physical_cores,\"host_physical_cores\":$host_physical_cores,\"required_host_cpus\":$required_host_cpus,\"host_memory_mib\":$memory_total_mib,\"host_cpu_count\":$host_cpu_count,\"available_disk_mib\":$available_disk_mib,\"required_disk_mib\":$required_disk_mib}"
