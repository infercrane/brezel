#!/bin/sh
set -eu

if mountpoint -q /tmp; then
  echo "/tmp is already a mount point; refusing to alter an undeclared baseline" >&2
  exit 1
fi
mount -t tmpfs -o size=8g,mode=1777,nosuid,nodev tmpfs /tmp
test "$(findmnt -n -o FSTYPE /tmp)" = "tmpfs"
