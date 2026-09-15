#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
BREZEL_EDGE_UID=${BREZEL_EDGE_UID:-$(id -u)}
BREZEL_EDGE_GID=${BREZEL_EDGE_GID:-$(id -g)}
export BREZEL_EDGE_UID BREZEL_EDGE_GID

"$SCRIPT_DIR/preflight.sh"
docker compose -f "$SCRIPT_DIR/compose.yaml" up -d --wait
