#!/usr/bin/env bash

set -euo pipefail

BREZEL_CLI=${BREZEL_CLI:-brezel}
DEMO_DELAY=${BREZEL_DEMO_DELAY:-0.8}
DEMO_LONG_DELAY=${BREZEL_DEMO_LONG_DELAY:-1.8}

if [[ -t 1 ]]; then
  RESET=$'\033[0m'
  DIM=$'\033[2m'
  BOLD=$'\033[1m'
  GREEN=$'\033[38;5;114m'
  BLUE=$'\033[38;5;111m'
  GOLD=$'\033[38;5;221m'
  MUTED=$'\033[38;5;245m'
else
  RESET=
  DIM=
  BOLD=
  GREEN=
  BLUE=
  GOLD=
  MUTED=
fi

demo_require_runtime() {
  command -v "$BREZEL_CLI" >/dev/null 2>&1 || {
    printf 'missing Brezel CLI: %s\n' "$BREZEL_CLI" >&2
    exit 1
  }
  [[ -n ${BREZEL_API_URL:-} ]] || {
    printf 'BREZEL_API_URL is required\n' >&2
    exit 1
  }
  [[ -n ${BREZEL_SERVICE_TOKEN_FILE:-} && -f ${BREZEL_SERVICE_TOKEN_FILE} ]] || {
    printf 'BREZEL_SERVICE_TOKEN_FILE must name a protected token file\n' >&2
    exit 1
  }
  [[ -n ${BREZEL_PROJECT:-} ]] || {
    printf 'BREZEL_PROJECT is required\n' >&2
    exit 1
  }
}

demo_pause() {
  sleep "$DEMO_DELAY"
}

demo_long_pause() {
  sleep "$DEMO_LONG_DELAY"
}

demo_clear() {
  printf '\033[2J\033[H'
}

demo_title() {
  printf '%s%s%s\n' "$BOLD" "$1" "$RESET"
  printf '%s%s%s\n' "$MUTED" "$2" "$RESET"
  demo_long_pause
}

demo_prompt() {
  printf '\n%s$%s %s%s%s\n' "$GREEN" "$RESET" "$BOLD" "$1" "$RESET"
  demo_pause
}

demo_note() {
  printf '%s%s%s\n' "$MUTED" "$1" "$RESET"
}

demo_short_id() {
  local value=$1
  printf '%s…' "${value:0:14}"
}

demo_create_workspace() {
  local name=$1
  local output
  output=$("$BREZEL_CLI" workspace create "$name")
  DEMO_WORKSPACE=${output%%$'\t'*}
  printf '%sworkspace%s  %s  %sready%s\n' \
    "$BLUE" "$RESET" "$(demo_short_id "$DEMO_WORKSPACE")" "$GREEN" "$RESET"
}

demo_create_sandbox() {
  local output
  output=$("$BREZEL_CLI" new \
    --workspace "$DEMO_WORKSPACE:/workspace" \
    --ttl "${BREZEL_DEMO_TTL:-900}")
  DEMO_SANDBOX=${output%%$'\t'*}
  printf '%ssandbox%s  %s  %srunning%s\n' \
    "$BLUE" "$RESET" "$(demo_short_id "$DEMO_SANDBOX")" "$GREEN" "$RESET"
}

demo_delete_sandbox() {
  [[ -n ${DEMO_SANDBOX:-} ]] || return 0
  "$BREZEL_CLI" delete "$DEMO_SANDBOX" >/dev/null 2>&1 || true
  DEMO_SANDBOX=
}

demo_cleanup() {
  demo_delete_sandbox
  if [[ -n ${DEMO_WORKSPACE:-} ]]; then
    "$BREZEL_CLI" workspace delete "$DEMO_WORKSPACE" >/dev/null 2>&1 || true
    DEMO_WORKSPACE=
  fi
}

