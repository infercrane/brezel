#!/usr/bin/env bash

set -euo pipefail

DEMO_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=demo-lib.sh
source "$DEMO_DIR/demo-lib.sh"

demo_require_runtime
DEMO_WORKSPACE=
DEMO_SANDBOX=
trap demo_cleanup EXIT INT TERM

demo_clear
demo_title \
  'Give every agent its own computer.' \
  'Real Firecracker isolation. Durable work. One small CLI.'

demo_prompt 'brezel workspace create agent-work'
demo_create_workspace "readme-demo-$(date +%s)"

demo_prompt 'brezel new --workspace agent-work:/workspace --ttl 900'
demo_create_sandbox

demo_prompt "brezel run $(demo_short_id "$DEMO_SANDBOX") sudo -n id"
"$BREZEL_CLI" run "$DEMO_SANDBOX" sudo -n id

"$BREZEL_CLI" put "$DEMO_SANDBOX" /tmp/brezel-demo "$DEMO_DIR/guest-demo.sh" >/dev/null
demo_prompt "brezel run --cwd /workspace $(demo_short_id "$DEMO_SANDBOX") bash /tmp/brezel-demo task"
"$BREZEL_CLI" run --cwd /workspace "$DEMO_SANDBOX" bash /tmp/brezel-demo task

demo_prompt "brezel run $(demo_short_id "$DEMO_SANDBOX") start-preview :3000"
"$BREZEL_CLI" run "$DEMO_SANDBOX" bash -lc \
  'nohup python3 -m http.server 3000 --directory /workspace >/tmp/brezel-demo-http.log 2>&1 </dev/null & sleep 0.2'
preview_output=$("$BREZEL_CLI" open --ttl 300 "$DEMO_SANDBOX" 3000)
[[ -n $preview_output ]]
printf '%spreview%s  short-lived URL ready  %s300s%s\n' \
  "$BLUE" "$RESET" "$GREEN" "$RESET"

demo_prompt "brezel delete $(demo_short_id "$DEMO_SANDBOX")"
demo_delete_sandbox
printf '%scompute destroyed%s\n' "$MUTED" "$RESET"

demo_prompt 'brezel new --workspace agent-work:/workspace --ttl 900'
demo_create_sandbox

demo_prompt "brezel run --cwd /workspace $(demo_short_id "$DEMO_SANDBOX") cat result.txt"
"$BREZEL_CLI" run --cwd /workspace "$DEMO_SANDBOX" cat result.txt

demo_cleanup
trap - EXIT INT TERM
printf '%scleanup confirmed%s\n' "$MUTED" "$RESET"
printf '\n%sComputer gone. Work intact.%s\n' "$BOLD" "$RESET"
printf '%sgithub.com/infercrane/brezel%s\n' "$GREEN" "$RESET"
demo_long_pause
