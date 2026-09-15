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

positive_integer() {
  value=$1
  name=$2
  case "$value" in
    ""|*[!0-9]*) fail "$name must be a positive integer" ;;
  esac
  [ "$value" -gt 0 ] || fail "$name must be greater than zero"
}

MODE=${1:-verify}
EXPECTED=${BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL:-32}
EXPECTED_VCPUS=${BREZEL_GUEST_VCPUS:-2}
EXPECTED_MEMORY_MIB=${BREZEL_GUEST_MEMORY_MIB:-512}
EXPECTED_FREE_DISK_MIB=${BREZEL_GUEST_MIN_FREE_DISK_MIB:-512}
EXPECTED_MAX_FREE_DISK_MIB=${BREZEL_GUEST_MAX_FREE_DISK_MIB:-25600}
positive_integer "$EXPECTED" BREZEL_MAX_ACTIVE_SANDBOXES_TOTAL
positive_integer "$EXPECTED_VCPUS" BREZEL_GUEST_VCPUS
positive_integer "$EXPECTED_MEMORY_MIB" BREZEL_GUEST_MEMORY_MIB
positive_integer "$EXPECTED_FREE_DISK_MIB" BREZEL_GUEST_MIN_FREE_DISK_MIB
positive_integer "$EXPECTED_MAX_FREE_DISK_MIB" BREZEL_GUEST_MAX_FREE_DISK_MIB
[ "$EXPECTED_FREE_DISK_MIB" -le "$EXPECTED_MAX_FREE_DISK_MIB" ] || \
  fail "BREZEL_GUEST_MIN_FREE_DISK_MIB cannot exceed BREZEL_GUEST_MAX_FREE_DISK_MIB"
case "$MODE" in
  apply|verify) ;;
  *) echo "usage: $0 apply | verify" >&2; exit 2 ;;
esac

command -v psql >/dev/null 2>&1 || fail "psql is required"
export PGUSER=${PGUSER:-${POSTGRES_USER:-postgres}}
export PGDATABASE=${PGDATABASE:-${POSTGRES_DB:-postgres}}

if [ "$MODE" = apply ]; then
  psql -X -v ON_ERROR_STOP=1 \
    -v expected="$EXPECTED" \
    -v expected_vcpus="$EXPECTED_VCPUS" \
    -v expected_memory="$EXPECTED_MEMORY_MIB" \
    -v expected_free_disk="$EXPECTED_FREE_DISK_MIB" \
    -v expected_max_free_disk="$EXPECTED_MAX_FREE_DISK_MIB" <<'SQL' >/dev/null
BEGIN;
SELECT set_config('brezel.expected_capacity', :'expected', true);
SELECT set_config('brezel.expected_vcpus', :'expected_vcpus', true);
SELECT set_config('brezel.expected_memory', :'expected_memory', true);
SELECT set_config('brezel.expected_free_disk', :'expected_free_disk', true);
SELECT set_config('brezel.expected_max_free_disk', :'expected_max_free_disk', true);
DO $brezel$
DECLARE
  seed_team_count bigint;
  seed_team_id uuid;
  seed_team_tier text;
  effective_team_count bigint;
  effective_mismatch_count bigint;
  expected_capacity bigint := current_setting('brezel.expected_capacity')::bigint;
  expected_vcpus bigint := current_setting('brezel.expected_vcpus')::bigint;
  expected_memory bigint := current_setting('brezel.expected_memory')::bigint;
  expected_free_disk bigint := current_setting('brezel.expected_free_disk')::bigint;
  expected_max_free_disk bigint := current_setting('brezel.expected_max_free_disk')::bigint;
BEGIN
  SELECT count(*), min(team_id::text)::uuid
  INTO seed_team_count, seed_team_id
  FROM (
    SELECT DISTINCT team_id
    FROM public.team_api_keys
    WHERE name = 'local dev seed token'
  ) AS seed_teams;
  IF seed_team_count <> 1 OR seed_team_id IS NULL THEN
    RAISE EXCEPTION 'Brezel embedded engine requires exactly one local seed-key owner, found %', seed_team_count;
  END IF;

  SELECT tier INTO seed_team_tier FROM public.teams WHERE id = seed_team_id;
  IF seed_team_tier IS DISTINCT FROM 'base_v1' THEN
    RAISE EXCEPTION 'Brezel embedded engine seed-key owner uses unexpected tier %', seed_team_tier;
  END IF;

  INSERT INTO public.project_limits (
    team_id,
    max_length_hours,
    concurrent_sandboxes,
    concurrent_template_builds,
    max_vcpu,
    max_ram_mb,
    disk_mb,
    events_ttl_days,
    default_free_disk_size_mb,
    max_disk_size_mb,
    max_free_disk_size_mb,
    updated_at
  )
  SELECT
    seed_team_id,
    tier.max_length_hours,
    expected_capacity,
    tier.concurrent_template_builds,
    expected_vcpus,
    expected_memory,
    expected_free_disk,
    tier.events_ttl_days,
    expected_free_disk,
    expected_max_free_disk,
    expected_max_free_disk,
    now()
  FROM public.tiers AS tier
  WHERE tier.id = 'base_v1'
  ON CONFLICT (team_id) DO UPDATE
  SET concurrent_sandboxes = EXCLUDED.concurrent_sandboxes,
      max_vcpu = EXCLUDED.max_vcpu,
      max_ram_mb = EXCLUDED.max_ram_mb,
      disk_mb = EXCLUDED.disk_mb,
      default_free_disk_size_mb = EXCLUDED.default_free_disk_size_mb,
      max_disk_size_mb = EXCLUDED.max_disk_size_mb,
      max_free_disk_size_mb = EXCLUDED.max_free_disk_size_mb,
      updated_at = EXCLUDED.updated_at;

  IF NOT FOUND THEN
    RAISE EXCEPTION 'Brezel embedded engine base tier is missing';
  END IF;

  SELECT count(*), count(*) FILTER (
    WHERE concurrent_sandboxes <> expected_capacity
       OR max_vcpu <> expected_vcpus
       OR max_ram_mb <> expected_memory
       OR default_free_disk_size_mb <> expected_free_disk
       OR max_free_disk_size_mb <> expected_max_free_disk
  )
  INTO effective_team_count, effective_mismatch_count
  FROM public.team_limits
  WHERE id = seed_team_id;
  IF effective_team_count <> 1 OR effective_mismatch_count <> 0 THEN
    RAISE EXCEPTION 'Brezel embedded engine effective limits do not match the operator resource contract';
  END IF;
END
$brezel$;
COMMIT;
SQL
fi

conformant=$(psql -X -v ON_ERROR_STOP=1 -Atc \
  "WITH seed_team AS (SELECT DISTINCT team_id FROM public.team_api_keys WHERE name = 'local dev seed token') SELECT (SELECT count(*) = 1 FROM seed_team) AND count(*) = 1 AND count(*) FILTER (WHERE concurrent_sandboxes <> $EXPECTED OR max_vcpu <> $EXPECTED_VCPUS OR max_ram_mb <> $EXPECTED_MEMORY_MIB OR default_free_disk_size_mb <> $EXPECTED_FREE_DISK_MIB OR max_free_disk_size_mb <> $EXPECTED_MAX_FREE_DISK_MIB) = 0 FROM public.team_limits WHERE id IN (SELECT team_id FROM seed_team)")
[ "$conformant" = t ] || fail "embedded engine effective limits do not match the operator resource contract"

printf '%s\n' \
  "{\"engine_capacity_contract\":\"conformant\",\"mode\":\"$MODE\",\"max_active_sandboxes\":$EXPECTED,\"guest_vcpus\":$EXPECTED_VCPUS,\"guest_memory_mib\":$EXPECTED_MEMORY_MIB,\"guest_min_free_disk_mib\":$EXPECTED_FREE_DISK_MIB,\"guest_max_free_disk_mib\":$EXPECTED_MAX_FREE_DISK_MIB}"
