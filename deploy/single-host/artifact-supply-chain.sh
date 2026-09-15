#!/bin/sh
set -eu

read_value() {
  file=$1
  key=$2
  sed -n "s/^${key}=//p" "$file"
}

fail() {
  echo "artifact-supply-chain: $*" >&2
  exit 1
}

require_sha256() {
  value=$1
  description=$2
  printf '%s\n' "$value" | grep -Eq '^[0-9a-f]{64}$' || fail "$description is not a lowercase SHA-256 digest"
}

require_regular_file() {
  path=$1
  [ -f "$path" ] && [ ! -L "$path" ] || fail "$path must be a regular, non-symlink file"
}

verify_source() {
  source_root=$1
  engine_lock=$2
  image_lock=$3
  artifact_lock=$4

  for specification in \
    "compose_sha256:embed/compose/compose.yaml" \
    "compose_env_sha256:embed/compose/.env" \
    "fetch_artifacts_sha256:embed/compose/scripts/fetch-artifacts.sh"; do
    key=${specification%%:*}
    relative=${specification#*:}
    expected=$(read_value "$engine_lock" "$key")
    require_sha256 "$expected" "$key"
    require_regular_file "$source_root/$relative"
    actual=$(sha256sum "$source_root/$relative" | awk '{print $1}')
    [ "$actual" = "$expected" ] || fail "$relative digest mismatch: got $actual, want $expected"
  done

  for specification in "image_lock_sha256:$image_lock" "artifact_lock_sha256:$artifact_lock"; do
    key=${specification%%:*}
    path=${specification#*:}
    expected=$(read_value "$engine_lock" "$key")
    require_sha256 "$expected" "$key"
    require_regular_file "$path"
    actual=$(sha256sum "$path" | awk '{print $1}')
    [ "$actual" = "$expected" ] || fail "$path digest mismatch: got $actual, want $expected"
  done
}

image_keys='BREZEL_ENGINE_POSTGRES_IMAGE BREZEL_ENGINE_REDIS_IMAGE BREZEL_ENGINE_CLICKHOUSE_IMAGE BREZEL_ENGINE_VECTOR_IMAGE E2B_DB_MIGRATOR_IMAGE E2B_CLIENT_PROXY_IMAGE E2B_CLICKHOUSE_MIGRATOR_IMAGE E2B_TOOLS_IMAGE E2B_NODE_E2B_IMAGE E2B_SEED_IMAGE'

validate_image_lock() {
  image_lock=$1
  [ "$(read_value "$image_lock" architecture)" = linux/amd64 ] || fail "engine image lock must target linux/amd64"
  for key in $image_keys; do
    value=$(read_value "$image_lock" "$key")
    printf '%s\n' "$value" | grep -Eq '^[^@[:space:]]+(:[^@[:space:]]+)?@sha256:[0-9a-f]{64}$' || \
      fail "$key must name one exact image manifest by SHA-256 digest"
  done
}

verify_images() {
  image_lock=$1
  mode=$2
  validate_image_lock "$image_lock"
  case "$mode" in
    pull|preloaded) ;;
    *) fail "image mode must be pull or preloaded" ;;
  esac
  for key in $image_keys; do
    image=$(read_value "$image_lock" "$key")
    if [ "$mode" = pull ]; then
      docker pull --platform linux/amd64 "$image" >/dev/null || fail "could not pull locked image $key"
    fi
    docker image inspect "$image" >/dev/null 2>&1 || \
      fail "locked image $key is not present locally; preload it or use image mode pull"
  done
}

artifact_names='orchestrator envd firecracker kernel busybox'

verify_host_artifacts() {
  artifact_lock=$1
  host_root=$2
  orchestrator_override_sha256=${3:-}
  [ "$(read_value "$artifact_lock" architecture)" = linux/amd64 ] || fail "engine artifact lock must target linux/amd64"
  if [ -n "$orchestrator_override_sha256" ]; then
    require_sha256 "$orchestrator_override_sha256" "orchestrator override SHA-256"
  fi
  for name in $artifact_names; do
    path=$(read_value "$artifact_lock" "${name}_path")
    expected=$(read_value "$artifact_lock" "${name}_sha256")
    if [ "$name" = orchestrator ] && [ -n "$orchestrator_override_sha256" ]; then
      expected=$orchestrator_override_sha256
    fi
    case "$path" in
      /*) ;;
      *) fail "${name}_path must be absolute" ;;
    esac
    case "$path" in
      *'/../'*|*'/..') fail "${name}_path may not traverse its host root" ;;
    esac
    require_sha256 "$expected" "${name}_sha256"
    full_path=${host_root%/}/$(printf '%s' "$path" | sed 's|^/||')
    require_regular_file "$full_path"
    actual=$(sha256sum "$full_path" | awk '{print $1}')
    [ "$actual" = "$expected" ] || fail "$path digest mismatch: got $actual, want $expected"
  done
}

write_manifest() {
  output=$1
  engine_lock=$2
  image_lock=$3
  artifact_lock=$4
  host_root=$5
  orchestrator_override_sha256=${6:-}
  orchestrator_patch_sha256=${7:-}
  orchestrator_cache_patch_sha256=${8:-}
  orchestrator_nfs_durability_patch_sha256=${9:-}
  engine_start_admission_patch_sha256=${10:-}
  engine_local_capacity_patch_sha256=${11:-}
  verify_host_artifacts "$artifact_lock" "$host_root" "$orchestrator_override_sha256"
  if [ -n "$orchestrator_patch_sha256" ]; then
    require_sha256 "$orchestrator_patch_sha256" "orchestrator patch SHA-256"
  fi
  if [ -n "$orchestrator_cache_patch_sha256" ]; then
    [ -n "$orchestrator_patch_sha256" ] || fail "the cache patch requires the lifecycle patch identity"
    require_sha256 "$orchestrator_cache_patch_sha256" "orchestrator cache patch SHA-256"
  fi
  if [ -n "$orchestrator_nfs_durability_patch_sha256" ]; then
    [ -n "$orchestrator_cache_patch_sha256" ] || fail "the NFS durability patch requires the cache patch identity"
    require_sha256 "$orchestrator_nfs_durability_patch_sha256" "orchestrator NFS durability patch SHA-256"
  fi
  if [ -n "$engine_start_admission_patch_sha256" ]; then
    [ -n "$orchestrator_nfs_durability_patch_sha256" ] || fail "the start-admission patch requires the NFS durability patch identity"
    require_sha256 "$engine_start_admission_patch_sha256" "engine start-admission patch SHA-256"
  fi
  if [ -n "$engine_local_capacity_patch_sha256" ]; then
    [ -n "$engine_start_admission_patch_sha256" ] || fail "the local-capacity patch requires the start-admission patch identity"
    require_sha256 "$engine_local_capacity_patch_sha256" "engine local-capacity patch SHA-256"
  fi
  output_dir=$(dirname "$output")
  mkdir -p "$output_dir"
  temporary=$(mktemp "$output_dir/.distribution-manifest.XXXXXX")
  trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
  {
    printf 'format=brezel-distribution-v1\n'
    printf 'engine_commit=%s\n' "$(read_value "$engine_lock" commit)"
    printf 'engine_lock_sha256=%s\n' "$(sha256sum "$engine_lock" | awk '{print $1}')"
    printf 'image_lock_sha256=%s\n' "$(sha256sum "$image_lock" | awk '{print $1}')"
    printf 'artifact_lock_sha256=%s\n' "$(sha256sum "$artifact_lock" | awk '{print $1}')"
    for key in $image_keys; do
      printf 'image.%s=%s\n' "$key" "$(read_value "$image_lock" "$key")"
    done
    for name in $artifact_names; do
      printf 'artifact.%s.path=%s\n' "$name" "$(read_value "$artifact_lock" "${name}_path")"
      installed_sha256=$(read_value "$artifact_lock" "${name}_sha256")
      if [ "$name" = orchestrator ] && [ -n "$orchestrator_override_sha256" ]; then
        printf 'artifact.orchestrator.upstream_sha256=%s\n' "$installed_sha256"
        installed_sha256=$orchestrator_override_sha256
      fi
      printf 'artifact.%s.sha256=%s\n' "$name" "$installed_sha256"
    done
    if [ -n "$orchestrator_patch_sha256" ]; then
      printf 'artifact.orchestrator.patch_sha256=%s\n' "$orchestrator_patch_sha256"
      printf 'artifact.orchestrator.lifecycle=acknowledged-after-resource-reclamation\n'
    fi
    if [ -n "$orchestrator_cache_patch_sha256" ]; then
      printf 'artifact.orchestrator.cache_patch_sha256=%s\n' "$orchestrator_cache_patch_sha256"
      printf 'artifact.orchestrator.snapshot_diff_cache=bounded-recoverable-cache\n'
    fi
    if [ -n "$orchestrator_nfs_durability_patch_sha256" ]; then
      printf 'artifact.orchestrator.nfs_durability_patch_sha256=%s\n' "$orchestrator_nfs_durability_patch_sha256"
      printf 'artifact.orchestrator.nfs_write_stability=fsync-before-file-sync-acknowledgement\n'
      printf 'artifact.orchestrator.nfs_namespace_stability=parent-directory-fsync-before-acknowledgement\n'
    fi
    if [ -n "$engine_start_admission_patch_sha256" ]; then
      printf 'artifact.engine.start_admission_patch_sha256=%s\n' "$engine_start_admission_patch_sha256"
      printf 'artifact.orchestrator.start_admission=operator-pinned-local-limit\n'
      printf 'artifact.api.capacity_retry=capped-exponential-backoff-with-jitter\n'
    fi
    if [ -n "$engine_local_capacity_patch_sha256" ]; then
      printf 'artifact.engine.local_capacity_patch_sha256=%s\n' "$engine_local_capacity_patch_sha256"
      printf 'artifact.orchestrator.local_resource_pools=operator-sized\n'
      printf 'artifact.template.resource_shape=operator-sized\n'
    fi
  } > "$temporary"
  chmod 600 "$temporary"
  mv -f -- "$temporary" "$output"
  trap - EXIT HUP INT TERM
}

usage() {
  echo "usage: $0 source SOURCE_ROOT ENGINE_LOCK IMAGE_LOCK ARTIFACT_LOCK | image-lock IMAGE_LOCK | images IMAGE_LOCK pull|preloaded | host ARTIFACT_LOCK HOST_ROOT [ORCHESTRATOR_SHA256] | manifest OUTPUT ENGINE_LOCK IMAGE_LOCK ARTIFACT_LOCK HOST_ROOT [ORCHESTRATOR_SHA256 ORCHESTRATOR_PATCH_SHA256 ORCHESTRATOR_CACHE_PATCH_SHA256 ORCHESTRATOR_NFS_DURABILITY_PATCH_SHA256 ENGINE_START_ADMISSION_PATCH_SHA256 ENGINE_LOCAL_CAPACITY_PATCH_SHA256]" >&2
  exit 2
}

command=${1:-}
case "$command" in
  source)
    [ "$#" -eq 5 ] || usage
    verify_source "$2" "$3" "$4" "$5"
    ;;
  image-lock)
    [ "$#" -eq 2 ] || usage
    validate_image_lock "$2"
    ;;
  images)
    [ "$#" -eq 3 ] || usage
    verify_images "$2" "$3"
    ;;
  host)
    { [ "$#" -eq 3 ] || [ "$#" -eq 4 ]; } || usage
    verify_host_artifacts "$2" "$3" "${4:-}"
    ;;
  manifest)
    { [ "$#" -eq 6 ] || [ "$#" -eq 8 ] || [ "$#" -eq 9 ] || [ "$#" -eq 10 ] || [ "$#" -eq 11 ] || [ "$#" -eq 12 ]; } || usage
    write_manifest "$2" "$3" "$4" "$5" "$6" "${7:-}" "${8:-}" "${9:-}" "${10:-}" "${11:-}" "${12:-}"
    ;;
  *) usage ;;
esac
