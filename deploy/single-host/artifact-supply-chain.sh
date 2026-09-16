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

require_template_identity() {
  name=$1
  reference=$2
  case "$name" in
    ""|*[!a-z0-9_-]*|[-_]*) fail "base-template name is invalid" ;;
  esac
  [ "${#name}" -le 64 ] || fail "base-template name is too long"
  printf '%s\n' "$reference" | grep -Eq '^[a-z0-9_-]+:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' || \
    fail "base-template reference must be an immutable templateID:buildID"
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
  envd_override_sha256=${4:-}
  [ "$(read_value "$artifact_lock" architecture)" = linux/amd64 ] || fail "engine artifact lock must target linux/amd64"
  if [ -n "$orchestrator_override_sha256" ]; then
    require_sha256 "$orchestrator_override_sha256" "orchestrator override SHA-256"
  fi
  if [ -n "$envd_override_sha256" ]; then
    require_sha256 "$envd_override_sha256" "envd override SHA-256"
  fi
  for name in $artifact_names; do
    path=$(read_value "$artifact_lock" "${name}_path")
    expected=$(read_value "$artifact_lock" "${name}_sha256")
    if [ "$name" = orchestrator ] && [ -n "$orchestrator_override_sha256" ]; then
      expected=$orchestrator_override_sha256
    fi
    if [ "$name" = envd ] && [ -n "$envd_override_sha256" ]; then
      expected=$envd_override_sha256
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
  orchestrator_cpu_topology_patch_sha256=${12:-}
  orchestrator_rootfs_read_patch_sha256=${13:-}
  orchestrator_nbd_multiqueue_patch_sha256=${14:-}
  orchestrator_cpuset_qualification_patch_sha256=${15:-}
  envd_override_sha256=${16:-}
  envd_process_tag_patch_sha256=${17:-}
  envd_process_replay_patch_sha256=${18:-}
  orchestrator_ext4_dir_index_patch_sha256=${19:-}
  base_template_identity_patch_sha256=${20:-}
  base_template_name=${21:-}
  base_template_reference=${22:-}
  orchestrator_direct_rootfs_patch_sha256=${23:-}
  orchestrator_resume_cleanup_patch_sha256=${24:-}
  verify_host_artifacts "$artifact_lock" "$host_root" "$orchestrator_override_sha256" "$envd_override_sha256"
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
  if [ -n "$orchestrator_cpu_topology_patch_sha256" ]; then
    [ -n "$engine_local_capacity_patch_sha256" ] || fail "the CPU-topology patch requires the local-capacity patch identity"
    require_sha256 "$orchestrator_cpu_topology_patch_sha256" "orchestrator CPU-topology patch SHA-256"
  fi
  if [ -n "$orchestrator_rootfs_read_patch_sha256" ]; then
    [ -n "$orchestrator_cpu_topology_patch_sha256" ] || fail "the rootfs-read patch requires the CPU-topology patch identity"
    require_sha256 "$orchestrator_rootfs_read_patch_sha256" "orchestrator rootfs-read patch SHA-256"
  fi
  if [ -n "$orchestrator_nbd_multiqueue_patch_sha256" ]; then
    [ -n "$orchestrator_rootfs_read_patch_sha256" ] || fail "the NBD-multiqueue patch requires the rootfs-read patch identity"
    require_sha256 "$orchestrator_nbd_multiqueue_patch_sha256" "orchestrator NBD-multiqueue patch SHA-256"
  fi
  if [ -n "$orchestrator_cpuset_qualification_patch_sha256" ]; then
    [ -n "$orchestrator_nbd_multiqueue_patch_sha256" ] || fail "the cpuset-qualification patch requires the NBD-multiqueue patch identity"
    require_sha256 "$orchestrator_cpuset_qualification_patch_sha256" "orchestrator cpuset-qualification patch SHA-256"
  fi
  if [ -n "$orchestrator_ext4_dir_index_patch_sha256" ]; then
    [ -n "$orchestrator_cpuset_qualification_patch_sha256" ] || fail "the ext4-dir-index patch requires the cpuset-qualification patch identity"
    require_sha256 "$orchestrator_ext4_dir_index_patch_sha256" "orchestrator ext4-dir-index patch SHA-256"
  fi
  if [ -n "$base_template_identity_patch_sha256" ]; then
    [ -n "$orchestrator_ext4_dir_index_patch_sha256" ] || fail "the base-template identity patch requires the ext4-dir-index patch identity"
    require_sha256 "$base_template_identity_patch_sha256" "base-template identity patch SHA-256"
    require_template_identity "$base_template_name" "$base_template_reference"
  elif [ -n "$base_template_name" ] || [ -n "$base_template_reference" ]; then
    fail "base-template identity requires its patch identity"
  fi
  if [ -n "$orchestrator_direct_rootfs_patch_sha256" ]; then
    [ -n "$base_template_identity_patch_sha256" ] || fail "the direct-rootfs patch requires the base-template identity patch"
    require_sha256 "$orchestrator_direct_rootfs_patch_sha256" "orchestrator direct-rootfs patch SHA-256"
  fi
  if [ -n "$orchestrator_resume_cleanup_patch_sha256" ]; then
    [ -n "$orchestrator_direct_rootfs_patch_sha256" ] || fail "the resume-cleanup patch requires the direct-rootfs patch identity"
    require_sha256 "$orchestrator_resume_cleanup_patch_sha256" "orchestrator resume-cleanup patch SHA-256"
  fi
  if [ -n "$envd_override_sha256" ]; then
    [ -n "$envd_process_tag_patch_sha256" ] || fail "the envd override requires the process-tag patch identity"
    [ -n "$envd_process_replay_patch_sha256" ] || fail "the envd override requires the process-replay patch identity"
    require_sha256 "$envd_override_sha256" "envd override SHA-256"
  fi
  if [ -n "$envd_process_tag_patch_sha256" ]; then
    [ -n "$envd_override_sha256" ] || fail "the process-tag patch requires the envd override identity"
    require_sha256 "$envd_process_tag_patch_sha256" "envd process-tag patch SHA-256"
  fi
  if [ -n "$envd_process_replay_patch_sha256" ]; then
    [ -n "$envd_process_tag_patch_sha256" ] || fail "the process-replay patch requires the process-tag patch identity"
    [ -n "$envd_override_sha256" ] || fail "the process-replay patch requires the envd override identity"
    require_sha256 "$envd_process_replay_patch_sha256" "envd process-replay patch SHA-256"
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
      if [ "$name" = envd ] && [ -n "$envd_override_sha256" ]; then
        printf 'artifact.envd.upstream_sha256=%s\n' "$installed_sha256"
        installed_sha256=$envd_override_sha256
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
    if [ -n "$orchestrator_cpu_topology_patch_sha256" ]; then
      printf 'artifact.orchestrator.cpu_topology_patch_sha256=%s\n' "$orchestrator_cpu_topology_patch_sha256"
      printf 'artifact.orchestrator.guest_smt=operator-configured-default-disabled\n'
      printf 'artifact.orchestrator.exclusive_cpu_topology=disabled-by-default\n'
    fi
    if [ -n "$orchestrator_rootfs_read_patch_sha256" ]; then
      printf 'artifact.orchestrator.rootfs_read_patch_sha256=%s\n' "$orchestrator_rootfs_read_patch_sha256"
      printf 'artifact.orchestrator.rootfs_read_path=allocation-free-local-and-whole-writable-range\n'
    fi
    if [ -n "$orchestrator_nbd_multiqueue_patch_sha256" ]; then
      printf 'artifact.orchestrator.nbd_multiqueue_patch_sha256=%s\n' "$orchestrator_nbd_multiqueue_patch_sha256"
      printf 'artifact.orchestrator.nbd_connections_per_device=operator-configured-default-one-range-one-to-four\n'
      printf 'artifact.orchestrator.nbd_lifecycle=attempt-owned-idempotent-cleanup\n'
    fi
    if [ -n "$orchestrator_cpuset_qualification_patch_sha256" ]; then
      printf 'artifact.orchestrator.cpuset_qualification_patch_sha256=%s\n' "$orchestrator_cpuset_qualification_patch_sha256"
      printf 'artifact.orchestrator.exclusive_cpu_isolation=qualified-only-when-explicitly-configured\n'
    fi
    if [ -n "$orchestrator_ext4_dir_index_patch_sha256" ]; then
      printf 'artifact.orchestrator.ext4_dir_index_patch_sha256=%s\n' "$orchestrator_ext4_dir_index_patch_sha256"
      printf 'artifact.template.ext4_dir_index=targeted-opt-in-default-disabled\n'
    fi
    if [ -n "$base_template_identity_patch_sha256" ]; then
      printf 'artifact.template.identity_patch_sha256=%s\n' "$base_template_identity_patch_sha256"
      printf 'artifact.template.name=%s\n' "$base_template_name"
      printf 'artifact.template.reference=%s\n' "$base_template_reference"
      printf 'artifact.template.cache_identity=template-id-and-build-id\n'
    fi
    if [ -n "$orchestrator_direct_rootfs_patch_sha256" ]; then
      printf 'artifact.orchestrator.direct_rootfs_patch_sha256=%s\n' "$orchestrator_direct_rootfs_patch_sha256"
      printf 'artifact.orchestrator.rootfs_provider=nbd-default-direct-diagnostic-reflink-explicit-opt-in\n'
    fi
    if [ -n "$orchestrator_resume_cleanup_patch_sha256" ]; then
      printf 'artifact.orchestrator.resume_cleanup_patch_sha256=%s\n' "$orchestrator_resume_cleanup_patch_sha256"
      printf 'artifact.orchestrator.resume_failure_cleanup=bounded-prestart-safe\n'
    fi
    if [ -n "$envd_process_tag_patch_sha256" ]; then
      printf 'artifact.envd.process_tag_patch_sha256=%s\n' "$envd_process_tag_patch_sha256"
      printf 'artifact.envd.live_tag_resolution=complete-map-scan\n'
    fi
    if [ -n "$envd_process_replay_patch_sha256" ]; then
      printf 'artifact.envd.process_replay_patch_sha256=%s\n' "$envd_process_replay_patch_sha256"
      printf 'artifact.envd.process_output_recovery=generation-bound-cursor-journal\n'
    fi
  } > "$temporary"
  chmod 600 "$temporary"
  mv -f -- "$temporary" "$output"
  trap - EXIT HUP INT TERM
}

usage() {
  echo "usage: $0 source SOURCE_ROOT ENGINE_LOCK IMAGE_LOCK ARTIFACT_LOCK | image-lock IMAGE_LOCK | images IMAGE_LOCK pull|preloaded | host ARTIFACT_LOCK HOST_ROOT [ORCHESTRATOR_SHA256 [ENVD_SHA256]] | manifest OUTPUT ENGINE_LOCK IMAGE_LOCK ARTIFACT_LOCK HOST_ROOT [ORCHESTRATOR_SHA256 ORCHESTRATOR_PATCH_SHA256 ORCHESTRATOR_CACHE_PATCH_SHA256 ORCHESTRATOR_NFS_DURABILITY_PATCH_SHA256 ENGINE_START_ADMISSION_PATCH_SHA256 ENGINE_LOCAL_CAPACITY_PATCH_SHA256 ORCHESTRATOR_CPU_TOPOLOGY_PATCH_SHA256 ORCHESTRATOR_ROOTFS_READ_PATCH_SHA256 ORCHESTRATOR_NBD_MULTIQUEUE_PATCH_SHA256 ORCHESTRATOR_CPUSET_QUALIFICATION_PATCH_SHA256 ENVD_SHA256 ENVD_PROCESS_TAG_PATCH_SHA256 ENVD_PROCESS_REPLAY_PATCH_SHA256 ORCHESTRATOR_EXT4_DIR_INDEX_PATCH_SHA256 BASE_TEMPLATE_IDENTITY_PATCH_SHA256 BASE_TEMPLATE_NAME BASE_TEMPLATE_REFERENCE ORCHESTRATOR_DIRECT_ROOTFS_PATCH_SHA256 ORCHESTRATOR_RESUME_CLEANUP_PATCH_SHA256]" >&2
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
    { [ "$#" -eq 3 ] || [ "$#" -eq 4 ] || [ "$#" -eq 5 ]; } || usage
    verify_host_artifacts "$2" "$3" "${4:-}" "${5:-}"
    ;;
  manifest)
    { [ "$#" -eq 6 ] || [ "$#" -eq 8 ] || [ "$#" -eq 9 ] || [ "$#" -eq 10 ] || [ "$#" -eq 11 ] || [ "$#" -eq 12 ] || [ "$#" -eq 13 ] || [ "$#" -eq 19 ] || [ "$#" -eq 20 ] || [ "$#" -eq 23 ] || [ "$#" -eq 24 ] || [ "$#" -eq 25 ]; } || usage
    write_manifest "$2" "$3" "$4" "$5" "$6" "${7:-}" "${8:-}" "${9:-}" "${10:-}" "${11:-}" "${12:-}" "${13:-}" "${14:-}" "${15:-}" "${16:-}" "${17:-}" "${18:-}" "${19:-}" "${20:-}" "${21:-}" "${22:-}" "${23:-}" "${24:-}" "${25:-}"
    ;;
  *) usage ;;
esac
