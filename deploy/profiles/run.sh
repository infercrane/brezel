#!/bin/sh
set -eu

if [ "$#" -lt 2 ]; then
  echo "usage: $0 PROFILE COMMAND [ARG ...]" >&2
  exit 2
fi

profile=$1
shift
case "$profile" in
  /*) ;;
  *) profile=$(CDPATH= cd -- "$(dirname -- "$profile")" && pwd)/$(basename -- "$profile") ;;
esac
[ -f "$profile" ] && [ ! -L "$profile" ] || {
  echo "profile must be an existing non-symlink file" >&2
  exit 2
}

# Profile files contain data-only NAME=value assignments. Export them for the
# complete child process tree so Compose and the installer cannot silently
# fall back to the default 2-vCPU/512-MiB shape.
set -a
# shellcheck disable=SC1090
. "$profile"
set +a

export BREZEL_PROFILE_FILE=$profile
exec "$@"
