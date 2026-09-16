# This file is appended to the pinned engine host-setup script and executed in
# the host namespaces as root. It intentionally has no shebang or `set` so it
# inherits the audited setup script's strict Bash mode.

case "${BREZEL_VM_OVERCOMMIT_MEMORY:-1}" in
  0|1|2) ;;
  *) die "BREZEL_VM_OVERCOMMIT_MEMORY must be 0, 1, or 2" "set it to 1 for lazy Firecracker snapshots, or explicitly choose a supported Linux overcommit policy" ;;
esac

# A UFFD-restored microVM maps its complete guest memfd before it faults pages
# in. Strict commit accounting can reject these sparse mappings while most RAM
# is still available. Always-overcommit is safe here only because Brezel admits
# a bounded number of active sandboxes at the API before provisioning begins.
cat >> /etc/sysctl.d/90-e2b.conf <<EOF ||
vm.overcommit_memory=${BREZEL_VM_OVERCOMMIT_MEMORY:-1}
EOF
  die "cannot persist Brezel's virtual-memory policy" "$ETC_FIX"
sysctl -q -w "vm.overcommit_memory=${BREZEL_VM_OVERCOMMIT_MEMORY:-1}" ||
  die "cannot apply Brezel's virtual-memory policy" "set vm.overcommit_memory explicitly on the dedicated host and rerun installation"

have_overcommit="$(cat /proc/sys/vm/overcommit_memory)" ||
  die "cannot read vm.overcommit_memory after applying it" "check that /proc is mounted and writable on the dedicated host"
[ "$have_overcommit" = "${BREZEL_VM_OVERCOMMIT_MEMORY:-1}" ] ||
  die "vm.overcommit_memory is $have_overcommit after requesting ${BREZEL_VM_OVERCOMMIT_MEMORY:-1}" "remove a conflicting sysctl policy and rerun installation"
log "Brezel virtual-memory policy applied (overcommit_memory=$have_overcommit)"

normalize_index_set() {
  value=$1
  expanded=$(printf '%s\n' "$value" | awk '
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
  ') || return 1
  [ -n "$expanded" ] || return 1
  printf '%s\n' "$expanded" | sort -n -u | paste -sd, -
}

if [ "${BREZEL_ENGINE_FIRECRACKER_EXCLUSIVE_CPU_TOPOLOGY:-false}" = true ]; then
  requested_cpus=${BREZEL_ENGINE_FIRECRACKER_CPUSET_CPUS:-}
  requested_mems=${BREZEL_ENGINE_FIRECRACKER_CPUSET_MEMS:-}
  [ -n "$requested_cpus" ] || die "exclusive CPU topology has no CPU set" "set BREZEL_ENGINE_FIRECRACKER_CPUSET_CPUS to complete physical-core sibling groups"
  [ -n "$requested_mems" ] || die "exclusive CPU topology has no NUMA memory set" "set BREZEL_ENGINE_FIRECRACKER_CPUSET_MEMS to one qualified NUMA node"
  normalized_cpus=$(normalize_index_set "$requested_cpus") || die "invalid exclusive CPU set" "use Linux cpulist syntax such as 2-19"
  normalized_mems=$(normalize_index_set "$requested_mems") || die "invalid exclusive NUMA memory set" "use a single numeric NUMA node"
  [ -n "$normalized_cpus" ] || die "invalid exclusive CPU set" "use Linux cpulist syntax such as 2-19"
  [ -n "$normalized_mems" ] || die "invalid exclusive NUMA memory set" "use a single numeric NUMA node"
  case "$normalized_mems" in
    ""|*,*) die "exclusive CPU topology requires exactly one NUMA memory node" "choose one NUMA node containing all guest and VMM cores" ;;
  esac

  cgroup_root=/sys/fs/cgroup
  partition=$cgroup_root/e2b
  [ -r "$cgroup_root/cgroup.controllers" ] || die "cgroup v2 is unavailable" "boot the dedicated host with a unified cgroup v2 hierarchy"
  grep -qw cpuset "$cgroup_root/cgroup.controllers" || die "the cgroup v2 cpuset controller is unavailable" "use a kernel with the cpuset controller enabled"
  printf '%s\n' +cpuset > "$cgroup_root/cgroup.subtree_control" || die "cannot enable the root cpuset controller" "remove a conflicting cgroup policy and retry"

  if [ ! -e "$partition" ]; then
    mkdir "$partition" || die "cannot create the Firecracker CPU partition" "check cgroup v2 permissions"
  fi
  [ -d "$partition" ] && [ ! -L "$partition" ] || die "the Firecracker CPU partition is not a real directory" "remove the conflicting /sys/fs/cgroup/e2b path"
  [ ! -s "$partition/cgroup.procs" ] || die "the Firecracker CPU partition already contains direct processes" "stop the prior engine cleanly before changing CPU isolation"

  partition_state=$(cat "$partition/cpuset.cpus.partition") || die "cannot read the Firecracker partition state" "use a kernel with cpuset partition support"
  if [ "$partition_state" = isolated ]; then
    have_cpus=$(normalize_index_set "$(cat "$partition/cpuset.cpus.effective")") || die "cannot parse the live isolated CPU set" "repair the host cgroup policy"
    have_exclusive=$(normalize_index_set "$(cat "$partition/cpuset.cpus.exclusive.effective")") || die "cannot parse the live exclusive CPU set" "use a kernel with cpuset exclusive partitions"
    have_mems=$(normalize_index_set "$(cat "$partition/cpuset.mems.effective")") || die "cannot parse the live NUMA set" "repair the host cgroup policy"
    [ "$have_cpus" = "$normalized_cpus" ] && [ "$have_exclusive" = "$normalized_cpus" ] && [ "$have_mems" = "$normalized_mems" ] ||
      die "the existing isolated CPU partition differs from the requested contract" "stop the engine, remove the empty /sys/fs/cgroup/e2b partition, and reinstall"
  else
    if find "$partition" -mindepth 1 -maxdepth 1 -type d -print -quit | grep -q .; then
      die "the unqualified Firecracker cgroup has child cgroups" "stop the prior engine and remove leaked sandbox cgroups before installation"
    fi
    printf '%s\n' "$requested_mems" > "$partition/cpuset.mems" || die "cannot assign the Firecracker NUMA node" "verify the requested node is online"
    printf '%s\n' "$requested_cpus" > "$partition/cpuset.cpus" || die "cannot assign the Firecracker CPUs" "verify every requested CPU is online"
    printf '%s\n' "$requested_cpus" > "$partition/cpuset.cpus.exclusive" || die "cannot make the Firecracker CPUs exclusive" "use a kernel with cpuset.cpus.exclusive support"
    printf '%s\n' isolated > "$partition/cpuset.cpus.partition" || die "cannot activate the isolated Firecracker partition" "remove overlapping cpuset partitions and retry"
  fi

  have_cpus=$(normalize_index_set "$(cat "$partition/cpuset.cpus.effective")") || die "cannot verify the effective Firecracker CPU set" "repair the host cgroup policy"
  have_exclusive=$(normalize_index_set "$(cat "$partition/cpuset.cpus.exclusive.effective")") || die "cannot verify the exclusive Firecracker CPU set" "use a kernel with cpuset exclusive partitions"
  have_mems=$(normalize_index_set "$(cat "$partition/cpuset.mems.effective")") || die "cannot verify the Firecracker NUMA set" "repair the host cgroup policy"
  [ "$have_cpus" = "$normalized_cpus" ] && [ "$have_exclusive" = "$normalized_cpus" ] && [ "$have_mems" = "$normalized_mems" ] ||
    die "the kernel did not activate the requested Firecracker CPU partition" "inspect cpuset partition state and overlapping allocations"
  log "Brezel isolated Firecracker CPU partition qualified (cpus=$normalized_cpus mems=$normalized_mems)"
fi
