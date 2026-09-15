#!/bin/sh
set -eu

# The embedded engine caches the complete authenticated team, including its
# admission limits, in Redis. Capacity changes are not effective until those
# derived entries are removed. The API must be stopped while this one-shot
# runs so an in-flight refresh cannot restore data read before the SQL commit.

fail() {
  echo "engine auth cache contract failed: $*" >&2
  exit 1
}

MODE=${1:-invalidate}
case "$MODE" in
  invalidate|verify) ;;
  *) echo "usage: $0 invalidate | verify" >&2; exit 2 ;;
esac

command -v redis-cli >/dev/null 2>&1 || fail "redis-cli is required"
REDIS_HOST=${REDIS_HOST:-redis}
REDIS_PORT=${REDIS_PORT:-6379}
CACHE_PATTERN='auth:team:*'

scan_cache() {
  redis-cli --raw -h "$REDIS_HOST" -p "$REDIS_PORT" --scan --pattern "$CACHE_PATTERN"
}

keys=$(mktemp)
trap 'rm -f -- "$keys"' EXIT HUP INT TERM
if [ "$MODE" = invalidate ]; then
  scan_cache > "$keys"
  while IFS= read -r key; do
    [ -n "$key" ] || continue
    case "$key" in
      auth:team:*) ;;
      *) fail "Redis returned a key outside the bounded auth cache prefix" ;;
    esac
    redis-cli --raw -h "$REDIS_HOST" -p "$REDIS_PORT" UNLINK "$key" >/dev/null
  done < "$keys"
fi

scan_cache > "$keys"
[ ! -s "$keys" ] || fail "stale embedded-engine team authentication entries remain"
rm -f -- "$keys"
trap - EXIT HUP INT TERM

printf '%s\n' '{"engine_auth_cache":"empty"}'
