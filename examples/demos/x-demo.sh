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

demo_scene \
  'Create a disposable computer.' \
  'Its workspace is durable. Its compute is not.'

demo_prompt 'brezel workspace create agent-work'
demo_create_workspace "x-demo-$(date +%s)"

demo_prompt 'brezel new --workspace agent-work:/workspace --ttl 900'
demo_create_sandbox
demo_long_pause

demo_scene \
  'Root inside. Your host stays private.' \
  'No host Docker socket, SSH key, or cloud credential enters the box.'

"$BREZEL_CLI" put "$DEMO_SANDBOX" /tmp/security-check "$DEMO_DIR/guest-boundary-check.sh" >/dev/null
demo_prompt "brezel run $(demo_short_id "$DEMO_SANDBOX") bash /tmp/security-check"
"$BREZEL_CLI" run "$DEMO_SANDBOX" bash /tmp/security-check
demo_long_pause

demo_scene \
  'Destroy the computer. Keep the work.' \
  'The next sandbox starts from the same workspace.'

"$BREZEL_CLI" put "$DEMO_SANDBOX" /workspace/agent-task "$DEMO_DIR/guest-agent-task.sh" >/dev/null
demo_prompt "brezel run --cwd /workspace $(demo_short_id "$DEMO_SANDBOX") bash agent-task"
"$BREZEL_CLI" run --cwd /workspace "$DEMO_SANDBOX" bash agent-task

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

demo_scene \
  'Measure useful work, not boot animations.' \
  'The exact public ComputeSDK DAX workload. Lower is better.'
BREZEL_DEMO_COMPACT_HEADING=true "$DEMO_DIR/dax-comparison.sh"
demo_long_pause
printf '\n%sA secure computer for every agent.%s\n' "$BOLD" "$RESET"
printf '%sOpen source at github.com/infercrane/brezel%s\n' "$GREEN" "$RESET"
demo_long_pause
