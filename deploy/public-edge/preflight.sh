#!/bin/sh
set -eu

fail() {
  echo "public edge preflight failed: $*" >&2
  exit 1
}

BREZEL_EDGE_UID=${BREZEL_EDGE_UID:-$(id -u)}
BREZEL_EDGE_GID=${BREZEL_EDGE_GID:-$(id -g)}
export BREZEL_EDGE_UID BREZEL_EDGE_GID
[ "$BREZEL_EDGE_UID" -eq "$(id -u)" ] 2>/dev/null || \
  fail "BREZEL_EDGE_UID must match the protected directory owner"
[ "$BREZEL_EDGE_GID" -eq "$(id -g)" ] 2>/dev/null || \
  fail "BREZEL_EDGE_GID must match the protected directory owner group"

case ${BREZEL_PUBLIC_HOST:-} in
  ""|*[!A-Za-z0-9.-]*|.*|*.) fail "BREZEL_PUBLIC_HOST must be a DNS hostname" ;;
esac
case ${BREZEL_ACME_EMAIL:-} in
  *@*.*) ;;
  *) fail "BREZEL_ACME_EMAIL must be an email address" ;;
esac
data_dir=
config_dir=
for variable in BREZEL_EDGE_DATA_DIR BREZEL_EDGE_CONFIG_DIR; do
  case "$variable" in
    BREZEL_EDGE_DATA_DIR) path=${BREZEL_EDGE_DATA_DIR:-} ;;
    BREZEL_EDGE_CONFIG_DIR) path=${BREZEL_EDGE_CONFIG_DIR:-} ;;
    *) fail "unsupported edge directory variable: $variable" ;;
  esac
  case "$path" in
    /*) ;;
    *) fail "$variable must be an absolute path" ;;
  esac
  case "$path" in
    /|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/var|/var/lib)
      fail "$variable must be a dedicated narrow directory, not $path"
      ;;
  esac
  [ -d "$path" ] && [ ! -L "$path" ] || fail "$path must be an existing non-symlink directory"
  owner=$(stat -c %u "$path")
  [ "$owner" -eq "$(id -u)" ] || fail "$path must be owned by uid $(id -u)"
  mode=$(stat -c %a "$path")
  permissions=$((0$mode))
  [ $((permissions & 022)) -eq 0 ] || fail "$path must not be group- or world-writable"
  if [ "$variable" = BREZEL_EDGE_DATA_DIR ]; then data_dir=$path; else config_dir=$path; fi
done
[ "$data_dir" != "$config_dir" ] || fail "edge data and config directories must be distinct"
command -v docker >/dev/null 2>&1 || fail "docker is required"
docker compose -f "$(dirname "$0")/compose.yaml" config -q
docker compose -f "$(dirname "$0")/compose.yaml" run --rm --no-deps edge \
  caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null
curl -fsS --max-time 2 http://127.0.0.1:8080/readyz >/dev/null || \
  fail "the loopback Brezel API is not ready"
resolved=$(getent ahostsv4 "$BREZEL_PUBLIC_HOST" 2>/dev/null | awk 'NR == 1 {print $1}')
[ -n "$resolved" ] || fail "the public hostname has no IPv4 address"
printf '%s\n' "public edge preflight passed for $BREZEL_PUBLIC_HOST ($resolved)"
