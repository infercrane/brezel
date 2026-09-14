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
