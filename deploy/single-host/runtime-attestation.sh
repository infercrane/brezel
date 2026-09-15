#!/bin/sh
set -eu

fail() {
  echo "runtime-attestation: $*" >&2
  exit 1
}

require_sha256() {
  value=$1
  description=$2
  printf '%s\n' "$value" | grep -Eq '^[0-9a-f]{64}$' ||
    fail "$description is not a lowercase SHA-256 digest"
}

file_mode() {
  if stat -c '%a' "$1" >/dev/null 2>&1; then
    stat -c '%a' "$1"
  else
    stat -f '%Lp' "$1"
  fi
}

file_owner() {
  if stat -c '%u' "$1" >/dev/null 2>&1; then
    stat -c '%u' "$1"
  else
    stat -f '%u' "$1"
  fi
}

require_private_manifest() {
  path=$1
  [ -f "$path" ] && [ ! -L "$path" ] || fail "$path must be a regular, non-symlink file"
  mode=$(file_mode "$path")
  case "$mode" in
    ''|*[!0-7]*) fail "could not determine permissions for $path" ;;
  esac
  [ $((0$mode & 077)) -eq 0 ] || fail "$path must not be group- or world-accessible"
  [ "$(file_owner "$path")" = "$(id -u)" ] || fail "$path must be owned by the installing user"
}

manifest_value() {
  manifest=$1
  key=$2
  value=$(awk -F= -v expected="$key" '
    $1 == expected {
      count++
      value = substr($0, length(expected) + 2)
    }
    END {
      if (count != 1 || value == "") exit 42
      print value
    }
  ' "$manifest") || fail "$manifest must contain exactly one non-empty $key entry"
  printf '%s\n' "$value"
}

validate_manifest_shape() {
  manifest=$1
  expected_lines=7
  actual_lines=$(awk 'END {print NR}' "$manifest")
  [ "$actual_lines" -eq "$expected_lines" ] || fail "$manifest has an unexpected number of entries"
  while IFS= read -r line; do
    key=${line%%=*}
    case "$key" in
      format|source.revision|source.clean|service.brezeld.image_sha256|service.brezeld.binary_sha256|service.brezel-node.image_sha256|service.brezel-node.binary_sha256) ;;
      *) fail "$manifest contains unknown entry $key" ;;
    esac
  done < "$manifest"
}

repository_revision() {
  repository=$1
  [ "$(git -C "$repository" rev-parse --is-inside-work-tree 2>/dev/null || true)" = true ] ||
    fail "$repository is not a Git checkout"
  [ -z "$(git -C "$repository" status --porcelain --untracked-files=normal)" ] ||
    fail "source repository must be clean"
  revision=$(git -C "$repository" rev-parse HEAD)
  printf '%s\n' "$revision" | grep -Eq '^[0-9a-f]{40}$' || fail "source repository has no exact commit"
  printf '%s\n' "$revision"
}

observe_service() {
  compose_file=$1
  service=$2
  binary=$3
  container=$(docker compose -f "$compose_file" ps -q "$service")
  [ -n "$container" ] || fail "$service container is not running"
  [ "$(docker inspect --format '{{.State.Running}}' "$container")" = true ] ||
    fail "$service container is not running"
  image=$(docker inspect --format '{{.Image}}' "$container")
  case "$image" in
    sha256:*) image=${image#sha256:} ;;
    *) fail "$service container has no content-addressed image identity" ;;
  esac
  require_sha256 "$image" "$service image identity"
  binary_sha=$(docker exec "$container" sha256sum "/usr/local/bin/$binary" | awk 'NR == 1 {print $1}')
  require_sha256 "$binary_sha" "$service executable"
  printf '%s %s\n' "$image" "$binary_sha"
}

write_manifest() {
  output=$1
  repository=$2
  compose_file=$3
  expected_revision=$4
  [ "$output" = "${output#/}" ] && fail "manifest output must be an absolute path"
  [ -f "$compose_file" ] && [ ! -L "$compose_file" ] || fail "$compose_file must be a regular, non-symlink file"
  revision=$(repository_revision "$repository")
  [ "$revision" = "$expected_revision" ] || fail "source revision changed during installation"
  set -- $(observe_service "$compose_file" brezeld brezeld)
  brezeld_image=$1
  brezeld_binary=$2
  set -- $(observe_service "$compose_file" brezel-node brezel-node)
  node_image=$1
  node_binary=$2
  output_dir=$(dirname "$output")
  [ -d "$output_dir" ] && [ ! -L "$output_dir" ] || fail "$output_dir must be a regular directory and not a symlink"
  temporary=$(mktemp "$output_dir/.runtime-attestation.XXXXXX")
  trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
  {
    printf 'format=brezel-runtime-attestation-v1\n'
    printf 'source.revision=%s\n' "$revision"
    printf 'source.clean=true\n'
    printf 'service.brezeld.image_sha256=%s\n' "$brezeld_image"
    printf 'service.brezeld.binary_sha256=%s\n' "$brezeld_binary"
    printf 'service.brezel-node.image_sha256=%s\n' "$node_image"
    printf 'service.brezel-node.binary_sha256=%s\n' "$node_binary"
  } > "$temporary"
  chmod 600 "$temporary"
  mv -f -- "$temporary" "$output"
  trap - EXIT HUP INT TERM
}

verify_manifest() {
  manifest=$1
  repository=$2
  compose_file=$3
  expected_revision=${4:-}
  require_private_manifest "$manifest"
  validate_manifest_shape "$manifest"
  [ "$(manifest_value "$manifest" format)" = brezel-runtime-attestation-v1 ] || fail "unsupported manifest format"
  [ "$(manifest_value "$manifest" source.clean)" = true ] || fail "manifest does not attest a clean source tree"
  revision=$(repository_revision "$repository")
  attested_revision=$(manifest_value "$manifest" source.revision)
  [ "$revision" = "$attested_revision" ] || fail "running installation is not bound to the checked-out source revision"
  if [ -n "$expected_revision" ]; then
    [ "$revision" = "$expected_revision" ] || fail "source revision does not match the required revision"
  fi
  set -- $(observe_service "$compose_file" brezeld brezeld)
  brezeld_image=$1
  brezeld_binary=$2
  set -- $(observe_service "$compose_file" brezel-node brezel-node)
  node_image=$1
  node_binary=$2
  [ "$brezeld_image" = "$(manifest_value "$manifest" service.brezeld.image_sha256)" ] || fail "running brezeld image does not match the install attestation"
  [ "$brezeld_binary" = "$(manifest_value "$manifest" service.brezeld.binary_sha256)" ] || fail "running brezeld executable does not match the install attestation"
  [ "$node_image" = "$(manifest_value "$manifest" service.brezel-node.image_sha256)" ] || fail "running brezel-node image does not match the install attestation"
  [ "$node_binary" = "$(manifest_value "$manifest" service.brezel-node.binary_sha256)" ] || fail "running brezel-node executable does not match the install attestation"
  printf '{"schema_version":1,"format":"brezel-runtime-attestation-v1","source":{"revision":"%s","clean":true},"services":{"brezeld":{"image_sha256":"%s","binary_sha256":"%s"},"brezel-node":{"image_sha256":"%s","binary_sha256":"%s"}},"verified_running":true}\n' \
    "$revision" "$brezeld_image" "$brezeld_binary" "$node_image" "$node_binary"
}

usage() {
  echo "usage: $0 write OUTPUT REPOSITORY COMPOSE_FILE EXPECTED_REVISION | verify MANIFEST REPOSITORY COMPOSE_FILE [EXPECTED_REVISION]" >&2
  exit 2
}

for command_name in awk docker git grep id mktemp stat; do
  command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"
done

command=${1:-}
case "$command" in
  write)
    [ "$#" -eq 5 ] || usage
    write_manifest "$2" "$3" "$4" "$5"
    ;;
  verify)
    { [ "$#" -eq 4 ] || [ "$#" -eq 5 ]; } || usage
    verify_manifest "$2" "$3" "$4" "${5:-}"
    ;;
  *) usage ;;
esac
