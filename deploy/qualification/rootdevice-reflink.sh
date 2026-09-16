#!/bin/sh
set -eu

if [ "$(uname -s)" != Linux ]; then
  echo "rootdevice qualification requires Linux" >&2
  exit 1
fi
if [ "$(id -u)" -ne 0 ]; then
  echo "run as root on a disposable qualification host" >&2
  exit 1
fi
for command in losetup mkfs.xfs mount umount truncate go; do
  command -v "$command" >/dev/null 2>&1 || { echo "missing command: $command" >&2; exit 1; }
done

work_parent=${BREZEL_ROOTDEVICE_WORK_PARENT:-/var/tmp}
case "$work_parent" in
  /*) ;;
  *) echo "BREZEL_ROOTDEVICE_WORK_PARENT must be absolute" >&2; exit 1 ;;
esac
work=$(mktemp -d "$work_parent/brezel-rootdevice.XXXXXX")
chmod 700 "$work"
image="$work/xfs.img"
mountpoint="$work/mount"
loop=
mounted=false

cleanup() {
  if [ "$mounted" = true ]; then
    chattr -i "$mountpoint/base.img" 2>/dev/null || true
    umount "$mountpoint" 2>/dev/null || true
  fi
  if [ -n "$loop" ]; then losetup -d "$loop" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM

truncate -s 1G "$image"
loop=$(losetup --find --show "$image")
mkfs.xfs -q -f -m reflink=1 "$loop"
mkdir "$mountpoint"
mount -o noatime,nosuid,nodev "$loop" "$mountpoint"
mounted=true
chmod 700 "$mountpoint"
printf '%s\n' 'BREZEL DEDICATED ROOTDEVICE QUALIFICATION FILESYSTEM' > "$mountpoint/.brezel-rootdevice-dedicated"
chmod 600 "$mountpoint/.brezel-rootdevice-dedicated"

go build -trimpath -o "$work/brezel-rootdevice-qualify" ./cmd/brezel-rootdevice-qualify
BREZEL_ROOTDEVICE_INTEGRATION_ROOT="$mountpoint/integration"
mkdir -m 700 "$BREZEL_ROOTDEVICE_INTEGRATION_ROOT"
BREZEL_ROOTDEVICE_INTEGRATION_ROOT="$BREZEL_ROOTDEVICE_INTEGRATION_ROOT" go test -count=1 ./internal/rootdevice
rmdir "$BREZEL_ROOTDEVICE_INTEGRATION_ROOT"

"$work/brezel-rootdevice-qualify" \
  -root "$mountpoint" -iterations 64 -concurrency 16 \
  -base-bytes 67108864 -disk-full
