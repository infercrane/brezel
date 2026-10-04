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
  /workspace/?*) ;;
  *) fail "profile must be below /workspace" ;;
esac
case "$profile" in
  *".."*) fail "profile path is invalid" ;;
esac
printf '%s' "$profile" | grep -q '[[:cntrl:]]' && fail "profile path is invalid"
printf '%s' "$profile" | grep -Eq '^/workspace/[A-Za-z0-9._/-]+$' || fail "profile path is invalid"
[ ! -L "$profile" ] || fail "profile must not be a symbolic link"

runtime=/tmp/brezel-browser-$(id -u)
browser_pid_file=$runtime/chromium.pid
relay_pid_file=$runtime/relay.pid
log_file=$runtime/chromium.log
relay_log_file=$runtime/relay.log
active_profile=$runtime/profile

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

restore_profile() {
  rm -rf "$active_profile"
  if [ "$(id -u)" -eq 0 ]; then
    install -d -o "$browser_user" -g "$browser_group" -m 0700 "$active_profile"
  else
    install -d -m 0700 "$active_profile"
  fi
  if [ -d "$profile" ]; then
    cp -a "$profile/." "$active_profile/"
  fi
  if [ "$(id -u)" -eq 0 ]; then
    chown -R "$browser_user:$browser_group" "$active_profile"
  fi
  # These process-scoped files must never cross a VM boundary.
  rm -f "$active_profile"/SingletonCookie \
    "$active_profile"/SingletonLock \
    "$active_profile"/SingletonSocket
}

persist_profile() {
  [ -d "$active_profile" ] || return 0
  rm -f "$active_profile"/SingletonCookie \
    "$active_profile"/SingletonLock \
    "$active_profile"/SingletonSocket
  staging=$profile.brezel-next-$$
  previous=$profile.brezel-previous-$$
  rm -rf "$staging" "$previous"
  if [ "$(id -u)" -eq 0 ]; then
    install -d -o "$browser_user" -g "$browser_group" -m 0700 "$staging"
  else
    install -d -m 0700 "$staging"
  fi
  cp -a "$active_profile/." "$staging/"
  had_previous=false
  if [ -e "$profile" ]; then
    mv "$profile" "$previous"
    had_previous=true
  fi
  if ! mv "$staging" "$profile"; then
    if [ "$had_previous" = true ]; then
      mv "$previous" "$profile"
    fi
    fail "could not persist the browser profile"
  fi
  if [ "$had_previous" = true ]; then
    rm -rf "$previous"
  fi
}

case "$command" in
  start)
    if is_running; then
      exit 0
    fi
    stop_pid "$relay_pid_file"
    stop_pid "$browser_pid_file"
    if [ "$(id -u)" -eq 0 ]; then
      browser_user=pwuser
      browser_group=pwuser
      browser_home=/home/pwuser
      install -d -o "$browser_user" -g "$browser_group" -m 0700 "$runtime" "$profile"
      chown "$browser_user:$browser_group" "$profile"
    else
      browser_user=$(id -un)
      browser_group=$(id -gn)
      browser_home=${HOME:-/tmp}
      install -d -m 0700 "$runtime" "$profile"
    fi
    restore_profile
    # Chromium's nested user-namespace sandbox is deliberately disabled in
    # this profile. The browser is already isolated inside a dedicated
    # Firecracker microVM, and Ubuntu 24.04 rejects Chromium's unprivileged
    # user namespace unless the host installs a matching AppArmor policy. CDP
    # remains reachable only through Brezel's path-bound capability.
    debug_port=$((port + 1))
    if [ "$port" -eq 65535 ]; then
      debug_port=65534
    fi
    set -- /usr/local/bin/brezel-chromium \
      --headless=new \
      --no-sandbox \
      --disable-dev-shm-usage \
      --disable-background-networking \
      --disable-component-update \
      --disable-default-apps \
      --disable-sync \
      --dns-over-https-mode=off \
      --disable-features=DnsOverHttps,AsyncDns,UseDnsHttpsSvcbAlpn \
      --metrics-recording-only \
      --no-first-run \
      --no-default-browser-check \
      --remote-debugging-address=127.0.0.1 \
      --remote-debugging-port="$debug_port" \
      --remote-allow-origins='*' \
      --user-data-dir="$active_profile" \
      about:blank
    if [ "$(id -u)" -eq 0 ]; then
      nohup setpriv --reuid="$browser_user" --regid="$browser_group" --init-groups \
        env HOME="$browser_home" "$@" >"$log_file" 2>&1 &
    else
      nohup env HOME="$browser_home" "$@" >"$log_file" 2>&1 &
    fi
    browser_pid=$!
    printf '%s\n' "$browser_pid" >"$browser_pid_file"
    if [ "$(id -u)" -eq 0 ]; then
      nohup setpriv --reuid="$browser_user" --regid="$browser_group" --init-groups \
        /usr/local/bin/brezel-cdp-relay \
        --listen-port "$port" --target-port "$debug_port" \
        >"$relay_log_file" 2>&1 &
    else
      nohup /usr/local/bin/brezel-cdp-relay \
        --listen-port "$port" --target-port "$debug_port" \
        >"$relay_log_file" 2>&1 &
    fi
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
    if [ "$(id -u)" -eq 0 ]; then
      browser_user=pwuser
      browser_group=pwuser
    else
      browser_user=$(id -un)
      browser_group=$(id -gn)
    fi
    persist_profile
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
