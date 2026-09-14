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

image_keys='RUNTIME_ENGINE_POSTGRES_IMAGE RUNTIME_ENGINE_REDIS_IMAGE RUNTIME_ENGINE_CLICKHOUSE_IMAGE RUNTIME_ENGINE_VECTOR_IMAGE E2B_DB_MIGRATOR_IMAGE E2B_CLIENT_PROXY_IMAGE E2B_CLICKHOUSE_MIGRATOR_IMAGE E2B_TOOLS_IMAGE E2B_NODE_E2B_IMAGE E2B_SEED_IMAGE'

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
  [ "$(read_value "$artifact_lock" architecture)" = linux/amd64 ] || fail "engine artifact lock must target linux/amd64"
  for name in $artifact_names; do
    path=$(read_value "$artifact_lock" "${name}_path")
    expected=$(read_value "$artifact_lock" "${name}_sha256")
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
  verify_host_artifacts "$artifact_lock" "$host_root"
  output_dir=$(dirname "$output")
  mkdir -p "$output_dir"
  temporary=$(mktemp "$output_dir/.distribution-manifest.XXXXXX")
  trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
  {
    printf 'format=open-agent-runtime-distribution-v1\n'
    printf 'engine_commit=%s\n' "$(read_value "$engine_lock" commit)"
    printf 'engine_lock_sha256=%s\n' "$(sha256sum "$engine_lock" | awk '{print $1}')"
    printf 'image_lock_sha256=%s\n' "$(sha256sum "$image_lock" | awk '{print $1}')"
    printf 'artifact_lock_sha256=%s\n' "$(sha256sum "$artifact_lock" | awk '{print $1}')"
    for key in $image_keys; do
      printf 'image.%s=%s\n' "$key" "$(read_value "$image_lock" "$key")"
    done
    for name in $artifact_names; do
      printf 'artifact.%s.path=%s\n' "$name" "$(read_value "$artifact_lock" "${name}_path")"
      printf 'artifact.%s.sha256=%s\n' "$name" "$(read_value "$artifact_lock" "${name}_sha256")"
    done
  } > "$temporary"
  chmod 600 "$temporary"
  mv -f -- "$temporary" "$output"
  trap - EXIT HUP INT TERM
}

usage() {
  echo "usage: $0 source SOURCE_ROOT ENGINE_LOCK IMAGE_LOCK ARTIFACT_LOCK | image-lock IMAGE_LOCK | images IMAGE_LOCK pull|preloaded | host ARTIFACT_LOCK HOST_ROOT | manifest OUTPUT ENGINE_LOCK IMAGE_LOCK ARTIFACT_LOCK HOST_ROOT" >&2
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
    [ "$#" -eq 3 ] || usage
    verify_host_artifacts "$2" "$3"
    ;;
  manifest)
    [ "$#" -eq 6 ] || usage
    write_manifest "$2" "$3" "$4" "$5" "$6"
    ;;
  *) usage ;;
esac
