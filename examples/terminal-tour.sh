#!/usr/bin/env bash
set -euo pipefail

# A real end-to-end tour for a qualified Brezel host. The caller supplies only
# the API URL, protected token file, and project through the normal CLI config.
command -v brezel >/dev/null

demo_dir="$(mktemp -d)"
sandbox_id=""
cleanup() {
  if [[ -n "$sandbox_id" ]]; then
    brezel delete "$sandbox_id" >/dev/null 2>&1 || true
  fi
  rm -r "$demo_dir"
}
trap cleanup EXIT

printf '{"answer":42}\n' >"$demo_dir/input.json"

create_output="$(brezel new --ttl 900 --standby-after 120)"
sandbox_id="${create_output%%$'\t'*}"
printf 'created  %s\n' "$create_output"

brezel put "$sandbox_id" /workspace/input.json "$demo_dir/input.json" >/dev/null
brezel run --cwd /workspace "$sandbox_id" python3 -c \
  'import json; print(json.load(open("input.json"))["answer"])'

brezel run "$sandbox_id" sh -lc \
  'mkdir -p /workspace/site && printf "Brezel is running.\n" >/workspace/site/index.html && nohup python3 -m http.server 3000 --directory /workspace/site >/tmp/preview.log 2>&1 &'
brezel open --ttl 300 "$sandbox_id" 3000

brezel stop "$sandbox_id" >/dev/null
printf 'standby %s\n' "$sandbox_id"
brezel start "$sandbox_id" >/dev/null
printf 'resumed %s\n' "$sandbox_id"
