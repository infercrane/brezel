#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
REPO_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
LOCK_FILE="$SCRIPT_DIR/engine.lock"
PATCH_DIR="$REPO_DIR/third_party/e2b-runtime/patches"
PATCH_BIN=${BREZEL_GNU_PATCH:-patch}

fail() {
  echo "engine patch-chain verification failed: $*" >&2
  exit 1
}

lock_value() {
  key=$1
  awk -F= -v key="$key" '
    $1 == key {
      if (++count > 1) exit 2
      print substr($0, index($0, "=") + 1)
    }
    END { if (count != 1) exit 1 }
  ' "$LOCK_FILE"
}

command -v "$PATCH_BIN" >/dev/null 2>&1 || fail "GNU patch is unavailable: $PATCH_BIN"
"$PATCH_BIN" --version 2>/dev/null | sed -n '1p' | grep -Fq 'GNU patch' || \
  fail "$PATCH_BIN is not GNU patch"
command -v git >/dev/null 2>&1 || fail "git is required"
command -v tar >/dev/null 2>&1 || fail "tar is required"

repository=$(lock_value repository) || fail "engine.lock has no unique repository"
commit=$(lock_value commit) || fail "engine.lock has no unique commit"
[ "$repository" = "https://github.com/e2b-dev/runtime.git" ] || \
  fail "the engine repository is outside the audited upstream"
printf '%s\n' "$commit" | grep -Eq '^[0-9a-f]{40}$' || fail "the engine commit is not a full SHA-1"

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/brezel-engine-patches.XXXXXX")
cleanup() {
  case "$work_dir" in
    "${TMPDIR:-/tmp}"/brezel-engine-patches.*) rm -rf -- "$work_dir" ;;
    *) echo "refusing to remove unexpected temporary path: $work_dir" >&2 ;;
  esac
}
trap cleanup EXIT HUP INT TERM

source_dir="$work_dir/source"
mkdir "$source_dir"
if [ -n "${BREZEL_ENGINE_SOURCE_DIR:-}" ]; then
  git -C "$BREZEL_ENGINE_SOURCE_DIR" cat-file -e "$commit^{commit}" 2>/dev/null || \
    fail "the supplied engine checkout does not contain $commit"
  git -C "$BREZEL_ENGINE_SOURCE_DIR" archive "$commit" | tar -xf - -C "$source_dir"
else
  upstream_git="$work_dir/upstream.git"
  git init --quiet --bare "$upstream_git"
  git -C "$upstream_git" fetch --quiet --depth=1 --no-tags "$repository" "$commit"
  git -C "$upstream_git" archive FETCH_HEAD | tar -xf - -C "$source_dir"
fi

patch_list="$work_dir/patches.list"
find "$PATCH_DIR" -maxdepth 1 -type f -name '[0-9][0-9][0-9][0-9]-*.patch' -print | \
  LC_ALL=C sort > "$patch_list"
[ -s "$patch_list" ] || fail "the vendored engine patchset is empty"

patch_count=0
while IFS= read -r patch_file; do
  patch_count=$((patch_count + 1))
  patch_output="$work_dir/patch-$patch_count.output"
  if ! LC_ALL=C "$PATCH_BIN" --batch --forward --fuzz=0 --no-backup-if-mismatch -d "$source_dir" -p1 \
    < "$patch_file" > "$patch_output" 2>&1; then
    cat "$patch_output" >&2
    fail "$(basename "$patch_file") does not apply with GNU patch"
  fi
  if grep -Eq 'with fuzz' "$patch_output"; then
    cat "$patch_output" >&2
    fail "$(basename "$patch_file") required fuzz"
  fi
  reject_files=$(find "$source_dir" -type f -name '*.rej' -print)
  [ -z "$reject_files" ] || {
    printf '%s\n' "$reject_files" >&2
    fail "$(basename "$patch_file") left reject artifacts"
  }
done < "$patch_list"

printf 'GNU patch chain verified: commit=%s patches=%s\n' "$commit" "$patch_count"
