#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTROL_HOST="${AUTO_CONTROL_HOST:-127.0.0.1}"
CONTROL_PORT="${AUTO_CONTROL_PORT:-8080}"
WEB_HOST="${AUTO_WEB_HOST:-127.0.0.1}"
WEB_PORT="${AUTO_WEB_PORT:-5173}"
CONTROL_ADDR="${CONTROL_HOST}:${CONTROL_PORT}"
CONTROL_URL="${AUTO_CONTROL_URL:-http://${CONTROL_ADDR}}"
VITE_CONTROL_TARGET="${VITE_CONTROL_URL:-http://127.0.0.1:${CONTROL_PORT}}"
AUTO_CLEAR_PORTS="${AUTO_CLEAR_PORTS:-0}"
AUTO_STARTUP_TIMEOUT="${AUTO_STARTUP_TIMEOUT:-60}"
CONTROL_PID=""
WEB_PID=""
GO_BIN="${AUTO_GO_BIN:-}"

validate_port() {
  [[ "$1" =~ ^[0-9]+$ ]] && (( $1 > 0 && $1 < 65536 ))
}

listener_pids() {
  if command -v lsof >/dev/null 2>&1; then
    lsof -tiTCP:"$1" -sTCP:LISTEN 2>/dev/null || true
  else
    fuser -n tcp "$1" 2>/dev/null || true
  fi
}

clear_port() {
  local port="$1"
  local pids
  pids="$(listener_pids "$port")"
  [[ -z "$pids" ]] && return

  if [[ "$AUTO_CLEAR_PORTS" != "1" ]]; then
    printf 'Port %s is already in use (PID: %s). Choose other ports or set AUTO_CLEAR_PORTS=1 to stop the listener.\n' "$port" "$(tr '\n' ' ' <<<"$pids")" >&2
    return 1
  fi

  printf 'Clearing port %s (PID: %s)\n' "$port" "$(tr '\n' ' ' <<<"$pids")"
  kill -TERM $pids 2>/dev/null || true
  for _ in {1..20}; do
    [[ -z "$(listener_pids "$port")" ]] && return
    sleep 0.1
  done

  pids="$(listener_pids "$port")"
  [[ -z "$pids" ]] || kill -KILL $pids 2>/dev/null || true
}

stop_process_group() {
  local pid="$1"
  [[ -z "$pid" ]] && return
  kill -0 "$pid" 2>/dev/null || return
  # 子进程与脚本同组（不再用 setsid）：`kill -TERM -- -$pid` 命不中任何进程组，
  # 只会落到下面的单进程 kill，pnpm 的 vite 等孙进程会成孤儿。因此先递归 kill 全部
  # 后代（深层优先），再对进程本身发信号；有 pgrep 时逐层清理，没有时退回单进程 kill。
  if command -v pgrep >/dev/null 2>&1; then
    local child
    for child in $(pgrep -P "$pid" 2>/dev/null || true); do
      stop_process_group "$child"
    done
  fi
  kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
}

resolve_go() {
  if [[ -n "$GO_BIN" ]]; then
    command -v "$GO_BIN" >/dev/null 2>&1 || {
      echo "AUTO_GO_BIN does not resolve to an executable: $GO_BIN" >&2
      return 1
    }
    return
  fi
  if command -v go >/dev/null 2>&1; then
    GO_BIN="go"
    return
  fi
  # Windows' WSL launcher inherits go.exe but not necessarily a Linux `go`.
  # Using the executable explicitly keeps `pnpm dev` usable from PowerShell.
  if command -v go.exe >/dev/null 2>&1; then
    GO_BIN="go.exe"
    return
  fi
  echo "go or go.exe is required to start the control server." >&2
  return 1
}

wait_for_control_server() {
  local health_url="http://${CONTROL_HOST}:${CONTROL_PORT}/api/health"
  local deadline=$((SECONDS + AUTO_STARTUP_TIMEOUT))

  printf 'Waiting for control server readiness at %s\n' "$health_url"
  while true; do
    if curl --silent --show-error --fail --max-time 1 "$health_url" >/dev/null 2>&1 || \
      { command -v curl.exe >/dev/null 2>&1 && curl.exe --silent --show-error --fail --max-time 1 "$health_url" >/dev/null 2>&1; }; then
      return
    fi
    if ! kill -0 "$CONTROL_PID" 2>/dev/null; then
      echo "Control server exited before becoming ready." >&2
      return 1
    fi
    if (( SECONDS >= deadline )); then
      printf 'Control server did not become ready within %s seconds.\n' "$AUTO_STARTUP_TIMEOUT" >&2
      return 1
    fi
    sleep 0.1
  done
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  stop_process_group "$WEB_PID"
  stop_process_group "$CONTROL_PID"
  wait "$WEB_PID" 2>/dev/null || true
  wait "$CONTROL_PID" 2>/dev/null || true
  exit "$status"
}

if ! validate_port "$CONTROL_PORT" || ! validate_port "$WEB_PORT"; then
  echo "AUTO_CONTROL_PORT and AUTO_WEB_PORT must be valid TCP ports." >&2
  exit 1
fi
if [[ "$CONTROL_PORT" == "$WEB_PORT" ]]; then
  echo "Control and web ports must be different." >&2
  exit 1
fi
if [[ "$AUTO_CLEAR_PORTS" != "0" && "$AUTO_CLEAR_PORTS" != "1" ]]; then
  echo "AUTO_CLEAR_PORTS must be 0 or 1." >&2
  exit 1
fi
if ! [[ "$AUTO_STARTUP_TIMEOUT" =~ ^[0-9]+$ ]] || (( AUTO_STARTUP_TIMEOUT < 1 )); then
  echo "AUTO_STARTUP_TIMEOUT must be a positive number of seconds." >&2
  exit 1
fi
if ! command -v curl >/dev/null 2>&1 && ! command -v curl.exe >/dev/null 2>&1; then
  echo "curl or curl.exe is required to wait for the control server to become ready." >&2
  exit 1
fi
if ! command -v lsof >/dev/null 2>&1 && ! command -v fuser >/dev/null 2>&1; then
  echo "lsof or fuser is required to clear occupied ports." >&2
  exit 1
fi
resolve_go

clear_port "$CONTROL_PORT"
clear_port "$WEB_PORT"

trap cleanup EXIT
trap 'exit 130' INT TERM

echo "Starting control server at ${CONTROL_URL}"
(
  cd "$ROOT_DIR/apps/control-server"
  exec env AUTO_HTTP_ADDR="$CONTROL_ADDR" AUTO_CONTROL_URL="$CONTROL_URL" "$GO_BIN" run ./cmd/control-server
) &
CONTROL_PID=$!

wait_for_control_server

echo "Starting web app at http://${WEB_HOST}:${WEB_PORT}/"
(
  cd "$ROOT_DIR/apps/web"
  exec env VITE_CONTROL_URL="$VITE_CONTROL_TARGET" pnpm dev --host "$WEB_HOST" --port "$WEB_PORT" --strictPort
) &
WEB_PID=$!

echo "Press Ctrl+C to stop both services."
wait -n "$CONTROL_PID" "$WEB_PID"
