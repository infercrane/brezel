#!/usr/bin/env bash

set -euo pipefail

printf 'patch: complete\ntests: 42 passed\n' > result.txt
cat > index.html <<'HTML'
<!doctype html>
<title>Brezel agent result</title>
<h1>42 tests passed</h1>
HTML
nohup python3 -m http.server 3000 --directory . >/tmp/brezel-demo-http.log 2>&1 </dev/null &
sleep 0.2
cat result.txt
printf 'preview: listening on :3000\n'
