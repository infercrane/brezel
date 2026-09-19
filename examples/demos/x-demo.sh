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
  'Give the agent root.' \
  'Keep your laptop and keys out of reach.'

demo_prompt 'brezel workspace create agent-work'
demo_create_workspace "x-demo-$(date +%s)"

demo_prompt 'brezel new --workspace agent-work:/workspace --ttl 900'
demo_create_sandbox

"$BREZEL_CLI" put "$DEMO_SANDBOX" /tmp/brezel-demo "$DEMO_DIR/guest-demo.sh" >/dev/null
demo_prompt "brezel run $(demo_short_id "$DEMO_SANDBOX") bash /tmp/brezel-demo boundary"
"$BREZEL_CLI" run "$DEMO_SANDBOX" bash /tmp/brezel-demo boundary

demo_prompt "brezel run --cwd /workspace $(demo_short_id "$DEMO_SANDBOX") bash /tmp/brezel-demo task"
"$BREZEL_CLI" run --cwd /workspace "$DEMO_SANDBOX" bash /tmp/brezel-demo task

demo_prompt "brezel delete $(demo_short_id "$DEMO_SANDBOX")"
demo_delete_sandbox
printf '%scomputer destroyed%s\n' "$MUTED" "$RESET"

demo_prompt 'brezel new --workspace agent-work:/workspace --ttl 900'
demo_create_sandbox

demo_prompt "brezel run --cwd /workspace $(demo_short_id "$DEMO_SANDBOX") cat result.txt"
"$BREZEL_CLI" run --cwd /workspace "$DEMO_SANDBOX" cat result.txt

printf '\n%sComputer gone. Work intact.%s\n' "$BOLD" "$RESET"
demo_long_pause

demo_cleanup
trap - EXIT INT TERM

demo_clear
"$DEMO_DIR/dax-comparison.sh"
demo_long_pause
printf '\n%sA secure computer for every agent.%s\n' "$BOLD" "$RESET"
printf '%sOpen source at github.com/infercrane/brezel%s\n' "$GREEN" "$RESET"
demo_long_pause
