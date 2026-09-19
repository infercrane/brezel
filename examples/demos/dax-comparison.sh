#!/usr/bin/env bash

set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
SNAPSHOT="$ROOT/examples/demos/dax-comparison-2026-09-19.json"
EVIDENCE="$ROOT/$(jq -r '.brezel.evidence' "$SNAPSHOT")"

[[ -f $EVIDENCE ]] || {
  printf 'missing Brezel evidence: %s\n' "$EVIDENCE" >&2
  exit 1
}

expected=$(jq -r '(.brezel.seconds * 1000) | round' "$SNAPSHOT")
actual=$(jq -r '.results[0].summary.totalMs.median' "$EVIDENCE")
[[ $expected == "$actual" ]] || {
  printf 'comparison snapshot does not match retained Brezel evidence\n' >&2
  exit 1
}

if [[ -t 1 ]]; then
  reset=$'\033[0m'
  bold=$'\033[1m'
  green=$'\033[38;5;114m'
  gold=$'\033[38;5;221m'
  muted=$'\033[38;5;245m'
else
  reset=
  bold=
  green=
  gold=
  muted=
fi

if [[ ${BREZEL_DEMO_COMPACT_HEADING:-false} != true ]]; then
  printf '%sComputeSDK DAX%s  %slower is better%s\n\n' "$bold" "$reset" "$muted" "$reset"
fi
printf '  Isorun    %s█████████████%s                         32.09s\n' "$muted" "$reset"
printf '  %sBrezel †%s  %s███████████████%s                       %s36.27s%s\n' \
  "$bold" "$reset" "$green" "$reset" "$green" "$reset"
printf '  Blaxel    %s███████████████████%s                   46.44s\n' "$muted" "$reset"
printf '  Daytona   %s███████████████████████████████%s       75.70s\n' "$muted" "$reset"
printf '  Modal     %s███████████████████████████████████████%s 94.59s\n' "$muted" "$reset"
printf '\n%s† Five-run self-run median on the exact public workload.%s\n' "$gold" "$reset"
printf '%s  Independent ComputeSDK verification is pending in PR #791.%s\n' "$muted" "$reset"
