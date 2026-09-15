#!/bin/sh
set -eu

# The embedded engine has its own tenant admission gate. Keep it identical to
# Brezel's public host limit so accepted work cannot fail later at a hidden,
# lower quota. This script runs either inside the Postgres service or in the
# one-shot capacity reconciler defined by engine.override.yaml.

fail() {
  echo "engine capacity contract failed: $*" >&2
  exit 1
}

MODE=${1:-verify}
EXPECTED=${BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL:-32}
case "$EXPECTED" in
  ""|*[!0-9]*) fail "BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL must be a positive integer" ;;
esac
[ "$EXPECTED" -gt 0 ] || fail "BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL must be greater than zero"
case "$MODE" in
  apply|verify) ;;
  *) echo "usage: $0 apply | verify" >&2; exit 2 ;;
esac

command -v psql >/dev/null 2>&1 || fail "psql is required"
export PGUSER=${PGUSER:-${POSTGRES_USER:-postgres}}
export PGDATABASE=${PGDATABASE:-${POSTGRES_DB:-postgres}}

if [ "$MODE" = apply ]; then
  psql -X -v ON_ERROR_STOP=1 -v expected="$EXPECTED" <<'SQL' >/dev/null
BEGIN;
DO $brezel$
DECLARE
  team_count bigint;
  non_base_team_count bigint;
  base_tier_count bigint;
BEGIN
  SELECT count(*) INTO team_count FROM public.teams;
  IF team_count < 1 THEN
    RAISE EXCEPTION 'Brezel embedded engine has no seeded team';
  END IF;

  SELECT count(*) INTO non_base_team_count FROM public.teams WHERE tier <> 'base_v1';
  IF non_base_team_count <> 0 THEN
    RAISE EXCEPTION 'Brezel embedded engine contains % team(s) outside its base tier', non_base_team_count;
  END IF;

  SELECT count(*) INTO base_tier_count FROM public.tiers WHERE id = 'base_v1';
  IF base_tier_count <> 1 THEN
    RAISE EXCEPTION 'Brezel embedded engine base tier is missing or ambiguous';
  END IF;
END
$brezel$;

UPDATE public.tiers
SET concurrent_instances = :'expected'::bigint
WHERE id = 'base_v1';
COMMIT;
SQL
fi

observed=$(psql -X -v ON_ERROR_STOP=1 -Atc \
  "SELECT CASE WHEN count(*) > 0 AND count(*) FILTER (WHERE concurrent_sandboxes <> $EXPECTED) = 0 THEN $EXPECTED ELSE -1 END FROM public.team_limits")
case "$observed" in
  ""|*[!0-9]*) fail "the embedded engine returned an invalid sandbox limit" ;;
esac
[ "$observed" -eq "$EXPECTED" ] || \
  fail "embedded engine admits $observed sandboxes but Brezel admits $EXPECTED"

printf '%s\n' \
  "{\"engine_capacity_contract\":\"conformant\",\"mode\":\"$MODE\",\"max_active_sandboxes\":$observed}"
