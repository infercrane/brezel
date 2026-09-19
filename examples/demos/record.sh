#!/usr/bin/env bash

set -euo pipefail

ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
OUTPUT_DIR="$ROOT/assets/demos"
TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/brezel-recordings.XXXXXX")

for command_name in asciinema agg ffmpeg jq; do
  command -v "$command_name" >/dev/null 2>&1 || {
    printf 'missing recording dependency: %s\n' "$command_name" >&2
    exit 1
  }
done

[[ -n ${BREZEL_API_URL:-} ]] || {
  printf 'BREZEL_API_URL is required\n' >&2
  exit 1
}
[[ -n ${BREZEL_SERVICE_TOKEN_FILE:-} ]] || {
  printf 'BREZEL_SERVICE_TOKEN_FILE is required\n' >&2
  exit 1
}
[[ -n ${BREZEL_PROJECT:-} ]] || {
  printf 'BREZEL_PROJECT is required\n' >&2
  exit 1
}

mkdir -p "$OUTPUT_DIR"

export BREZEL_CLI=${BREZEL_CLI:-$ROOT/bin/brezel}
export BREZEL_DEMO_DELAY=${BREZEL_DEMO_DELAY:-0.65}
export BREZEL_DEMO_LONG_DELAY=${BREZEL_DEMO_LONG_DELAY:-1.75}
export BREZEL_DEMO_KEY_DELAY=${BREZEL_DEMO_KEY_DELAY:-0.014}
export BREZEL_DEMO_CAPTION_DELAY=${BREZEL_DEMO_CAPTION_DELAY:-0.022}

asciinema rec \
  --overwrite \
  --cols 100 \
  --rows 25 \
  --command "$ROOT/examples/demos/readme-tour.sh" \
  "$TMP_DIR/readme.cast"

agg \
  --theme github-dark \
  --font-size 18 \
  --line-height 1.35 \
  --idle-time-limit 2 \
  --last-frame-duration 3 \
  "$TMP_DIR/readme.cast" \
  "$OUTPUT_DIR/brezel-tour.gif"

asciinema rec \
  --overwrite \
  --cols 104 \
  --rows 27 \
  --command "$ROOT/examples/demos/x-demo.sh" \
  "$TMP_DIR/x.cast"

agg \
  --theme github-dark \
  --font-size 19 \
  --line-height 1.3 \
  --idle-time-limit 2 \
  --last-frame-duration 3 \
  "$TMP_DIR/x.cast" \
  "$TMP_DIR/x.gif"

ffmpeg -hide_banner -loglevel error -y \
  -i "$TMP_DIR/x.gif" \
  -vf "scale=1280:720:force_original_aspect_ratio=decrease:flags=lanczos,pad=1280:720:(ow-iw)/2:(oh-ih)/2:color=0x0d1117,format=yuv420p" \
  -movflags +faststart \
  -c:v libx264 \
  -preset slow \
  -crf 20 \
  "$OUTPUT_DIR/brezel-x-demo.mp4"

printf 'wrote %s\n' "$OUTPUT_DIR/brezel-tour.gif"
printf 'wrote %s\n' "$OUTPUT_DIR/brezel-x-demo.mp4"
