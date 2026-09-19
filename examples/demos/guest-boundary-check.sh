#!/usr/bin/env bash

set -euo pipefail

sudo -n id
test ! -S /var/run/docker.sock
printf '[ok] host Docker socket is not mounted\n'
test ! -e /root/.ssh/id_ed25519
test ! -e /home/user/.ssh/id_ed25519
printf '[ok] host SSH keys are not present\n'
if env | grep -Eq '^(AWS_SECRET_ACCESS_KEY|GITHUB_TOKEN|GOOGLE_APPLICATION_CREDENTIALS)='; then
  printf 'unexpected host credential\n' >&2
  exit 1
fi
printf '[ok] host cloud credentials are not present\n'
