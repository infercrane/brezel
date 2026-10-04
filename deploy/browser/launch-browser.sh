#!/bin/sh
set -eu

fail() {
  echo "brezel-browser: $*" >&2
  exit 1
}

command=${1:-}
shift || true

port=9222
profile=/workspace/browser-profile
while [ "$#" -gt 0 ]; do
  case "$1" in
    --port)
      [ "$#" -ge 2 ] || fail "--port requires a value"
      port=$2
      shift 2
      ;;
    --profile)
      [ "$#" -ge 2 ] || fail "--profile requires a value"
      profile=$2
      shift 2
      ;;
    *) fail "unknown argument: $1" ;;
  esac
done

case "$port" in
  ''|*[!0-9]*) fail "port must be an integer" ;;
esac
[ "$port" -ge 1024 ] && [ "$port" -le 65535 ] || fail "port must be between 1024 and 65535"
case "$profile" in
  /*) ;;
  *) fail "profile must be an absolute path" ;;
esac
case "$profile" in
  *".."*) fail "profile path is invalid" ;;
esac
printf '%s' "$profile" | grep -q '[[:cntrl:]]' && fail "profile path is invalid"

runtime=/tmp/brezel-browser
browser_pid_file=$runtime/chromium.pid
relay_pid_file=$runtime/relay.pid
log_file=$runtime/chromium.log
relay_log_file=$runtime/relay.log

pid_is_running() {
  pid_file=$1
  [ -f "$pid_file" ] || return 1
  pid=$(cat "$pid_file" 2>/dev/null || true)
  case "$pid" in ''|*[!0-9]*) return 1 ;; esac
  kill -0 "$pid" 2>/dev/null
}

is_running() {
  pid_is_running "$browser_pid_file" && pid_is_running "$relay_pid_file"
}

stop_pid() {
  pid_file=$1
  if pid_is_running "$pid_file"; then
    pid=$(cat "$pid_file")
    kill "$pid" 2>/dev/null || true
    attempts=0
    while kill -0 "$pid" 2>/dev/null; do
      attempts=$((attempts + 1))
      if [ "$attempts" -ge 50 ]; then
        kill -KILL "$pid" 2>/dev/null || true
        break
      fi
      sleep 0.1
    done
  fi
  rm -f "$pid_file"
}

case "$command" in
  start)
    if is_running; then
      exit 0
    fi
    stop_pid "$relay_pid_file"
    stop_pid "$browser_pid_file"
    install -d -o pwuser -g pwuser -m 0700 "$runtime" "$profile"
    chown pwuser:pwuser "$profile"
    # Chromium's nested user-namespace sandbox is deliberately disabled in
    # this profile. The browser is already isolated inside a dedicated
    # Firecracker microVM, and Ubuntu 24.04 rejects Chromium's unprivileged
    # user namespace unless the host installs a matching AppArmor policy. CDP
    # remains reachable only through Brezel's path-bound capability.
    debug_port=$((port + 1))
    if [ "$port" -eq 65535 ]; then
      debug_port=65534
    fi
    nohup setpriv --reuid=pwuser --regid=pwuser --init-groups \
      env HOME=/home/pwuser /usr/local/bin/brezel-chromium \
        --headless=new \
        --no-sandbox \
        --disable-dev-shm-usage \
        --disable-background-networking \
        --disable-component-update \
        --disable-default-apps \
        --disable-sync \
        --metrics-recording-only \
        --no-first-run \
        --no-default-browser-check \
        --remote-debugging-address=127.0.0.1 \
        --remote-debugging-port="$debug_port" \
        --remote-allow-origins='*' \
        --user-data-dir="$profile" \
        about:blank >"$log_file" 2>&1 &
    browser_pid=$!
    printf '%s\n' "$browser_pid" >"$browser_pid_file"
    nohup /usr/local/bin/brezel-cdp-relay \
      --listen-port "$port" --target-port "$debug_port" \
      >"$relay_log_file" 2>&1 &
    relay_pid=$!
    printf '%s\n' "$relay_pid" >"$relay_pid_file"
    chmod 0600 "$browser_pid_file" "$relay_pid_file" "$log_file" "$relay_log_file"
    attempts=0
    until curl -fsS --max-time 1 "http://127.0.0.1:$port/json/version" >/dev/null 2>&1; do
      attempts=$((attempts + 1))
      if [ "$attempts" -ge 100 ] || ! kill -0 "$browser_pid" 2>/dev/null || ! kill -0 "$relay_pid" 2>/dev/null; then
        tail -n 40 "$log_file" >&2 || true
        tail -n 40 "$relay_log_file" >&2 || true
        stop_pid "$relay_pid_file"
        stop_pid "$browser_pid_file"
        fail "Chromium did not become ready"
      fi
      sleep 0.1
    done
    ;;
  stop)
    stop_pid "$relay_pid_file"
    stop_pid "$browser_pid_file"
    ;;
  status)
    if is_running; then
      echo running
    else
      echo stopped
      exit 1
    fi
    ;;
  *) fail "expected start, stop, or status" ;;
esac
