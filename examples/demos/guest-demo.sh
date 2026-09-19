#!/usr/bin/env bash

set -euo pipefail

case "${1:-}" in
  boundary)
    sudo -n id
    test ! -S /var/run/docker.sock
    printf '✓ host Docker socket: not mounted\n'
    test ! -e /root/.ssh/id_ed25519
    test ! -e /home/user/.ssh/id_ed25519
    printf '✓ host SSH key: not present\n'
    if env | grep -Eq '^(AWS_SECRET_ACCESS_KEY|GITHUB_TOKEN|GOOGLE_APPLICATION_CREDENTIALS)='; then
      printf 'unexpected host credential\n' >&2
      exit 1
    fi
    printf '✓ host cloud credentials: not present\n'
    ;;
  task)
    printf 'patch: complete\ntests: 42 passed\n' > result.txt
    cat result.txt
    cat > index.html <<'HTML'
<!doctype html>
<title>Brezel agent result</title>
<h1>42 tests passed</h1>
HTML
    ;;
  *)
    printf 'usage: guest-demo.sh boundary|task\n' >&2
    exit 2
    ;;
esac

