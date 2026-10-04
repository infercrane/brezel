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
pid_file=$runtime/chromium.pid
log_file=$runtime/chromium.log

is_running() {
  [ -f "$pid_file" ] || return 1
  pid=$(cat "$pid_file" 2>/dev/null || true)
  case "$pid" in ''|*[!0-9]*) return 1 ;; esac
  kill -0 "$pid" 2>/dev/null
}

case "$command" in
  start)
    if is_running; then
      exit 0
    fi
    install -d -o pwuser -g pwuser -m 0700 "$runtime" "$profile"
    chown pwuser:pwuser "$profile"
    runuser -u pwuser -- env HOME=/home/pwuser \
      nohup /usr/local/bin/brezel-chromium \
        --headless=new \
        --disable-dev-shm-usage \
        --disable-background-networking \
        --disable-component-update \
        --disable-default-apps \
        --disable-sync \
        --metrics-recording-only \
        --no-first-run \
        --no-default-browser-check \
        --remote-debugging-address=0.0.0.0 \
        --remote-debugging-port="$port" \
        --remote-allow-origins=http://127.0.0.1 \
        --user-data-dir="$profile" \
        about:blank >"$log_file" 2>&1 &
    pid=$!
    printf '%s\n' "$pid" >"$pid_file"
    chmod 0600 "$pid_file" "$log_file"
    attempts=0
    until curl -fsS --max-time 1 "http://127.0.0.1:$port/json/version" >/dev/null 2>&1; do
      attempts=$((attempts + 1))
      if [ "$attempts" -ge 100 ] || ! kill -0 "$pid" 2>/dev/null; then
        tail -n 40 "$log_file" >&2 || true
        fail "Chromium did not become ready"
      fi
      sleep 0.1
    done
    ;;
  stop)
    if is_running; then
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
