#!/bin/sh
set -eu

# The bundled base environment is fixed at 512 MiB. Firecracker reserves that
# memory from the host's 2 MiB hugetlb pool before lazy paging can begin. Keep
# the public sandbox quota, the engine reservation, and host headroom as one
# fail-closed contract rather than discovering the limit through mmap errors.

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

MODE=${1:-plan}
MEMINFO=${BREZEL_TEST_MEMINFO_FILE:-/proc/meminfo}
GUEST_MEMORY_MIB=${BREZEL_GUEST_MEMORY_MIB:-512}
HUGEPAGES=${BREZEL_ENGINE_HUGEPAGES:-9216}
MAX_ACTIVE_TOTAL=${BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL:-32}
MAX_ACTIVE_PROJECT=${BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT:-32}
HEADROOM_SANDBOXES=4
MIN_SYSTEM_MEMORY_MIB=8192
MAX_NETWORK_SLOTS=32

positive_integer "$GUEST_MEMORY_MIB" BREZEL_GUEST_MEMORY_MIB
positive_integer "$HUGEPAGES" BREZEL_ENGINE_HUGEPAGES
positive_integer "$MAX_ACTIVE_TOTAL" BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL
positive_integer "$MAX_ACTIVE_PROJECT" BREZEL_MAX_ACTIVE_SANDBOXES_PER_PROJECT
[ "$GUEST_MEMORY_MIB" -eq 512 ] || \
  fail "the bundled base environment requires BREZEL_GUEST_MEMORY_MIB=512"
[ "$MAX_ACTIVE_PROJECT" -le "$MAX_ACTIVE_TOTAL" ] || \
  fail "the per-project active limit cannot exceed the host active limit"
[ "$MAX_ACTIVE_TOTAL" -le "$MAX_NETWORK_SLOTS" ] || \
  fail "the active limit cannot exceed the qualified $MAX_NETWORK_SLOTS-slot new-sandbox network pool"

# Each 2 MiB page holds 2 MiB of guest memory. Four additional guest-sized
# reservations cover lifecycle overlap during replacement and checkpoint
# restore without advertising that headroom as tenant capacity.
required_hugepages=$(( (MAX_ACTIVE_TOTAL + HEADROOM_SANDBOXES) * GUEST_MEMORY_MIB / 2 ))
[ "$HUGEPAGES" -ge "$required_hugepages" ] || \
  fail "$HUGEPAGES hugepages cannot back $MAX_ACTIVE_TOTAL active 512 MiB sandboxes plus $HEADROOM_SANDBOXES lifecycle-overlap slots; require at least $required_hugepages"

[ -r "$MEMINFO" ] || fail "cannot read host memory information from $MEMINFO"
memory_total_kib=$(awk '/^MemTotal:/ {print $2; exit}' "$MEMINFO")
positive_integer "$memory_total_kib" MemTotal
memory_total_mib=$((memory_total_kib / 1024))
hugepage_memory_mib=$((HUGEPAGES * 2))
required_host_mib=$((hugepage_memory_mib + MIN_SYSTEM_MEMORY_MIB))
[ "$memory_total_mib" -ge "$required_host_mib" ] || \
  fail "host has ${memory_total_mib} MiB RAM; this profile requires at least ${required_host_mib} MiB (${hugepage_memory_mib} MiB sandbox pool plus ${MIN_SYSTEM_MEMORY_MIB} MiB system reserve)"

case "$MODE" in
  plan)
    ;;
  live)
    hugepage_size_kib=$(awk '/^Hugepagesize:/ {print $2; exit}' "$MEMINFO")
    hugepages_total=$(awk '/^HugePages_Total:/ {print $2; exit}' "$MEMINFO")
    positive_integer "$hugepage_size_kib" Hugepagesize
    positive_integer "$hugepages_total" HugePages_Total
    [ "$hugepage_size_kib" -eq 2048 ] || \
      fail "host hugepage size is ${hugepage_size_kib} KiB; require 2048 KiB"
    [ "$hugepages_total" -ge "$HUGEPAGES" ] || \
      fail "host reserved $hugepages_total hugepages; configured profile requires $HUGEPAGES"
    ;;
  *)
    echo "usage: $0 plan | live" >&2
    exit 2
    ;;
esac

printf '%s\n' \
  "{\"capacity_contract\":\"conformant\",\"mode\":\"$MODE\",\"guest_memory_mib\":$GUEST_MEMORY_MIB,\"max_active_sandboxes\":$MAX_ACTIVE_TOTAL,\"max_active_sandboxes_per_project\":$MAX_ACTIVE_PROJECT,\"hugepages_2m\":$HUGEPAGES,\"required_hugepages_2m\":$required_hugepages,\"lifecycle_headroom_sandboxes\":$HEADROOM_SANDBOXES,\"system_memory_reserve_mib\":$MIN_SYSTEM_MEMORY_MIB,\"host_memory_mib\":$memory_total_mib}"
