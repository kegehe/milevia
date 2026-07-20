#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTROL_HOST="${AUTO_CONTROL_HOST:-127.0.0.1}"
CONTROL_PORT="${AUTO_CONTROL_PORT:-8080}"
WEB_HOST="${AUTO_WEB_HOST:-127.0.0.1}"
WEB_PORT="${AUTO_WEB_PORT:-5173}"
CONTROL_ADDR="${CONTROL_HOST}:${CONTROL_PORT}"
CONTROL_URL="http://${CONTROL_ADDR}"
CONTROL_PID=""
WEB_PID=""

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
  kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
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
if ! command -v lsof >/dev/null 2>&1 && ! command -v fuser >/dev/null 2>&1; then
  echo "lsof or fuser is required to clear occupied ports." >&2
  exit 1
fi

clear_port "$CONTROL_PORT"
clear_port "$WEB_PORT"

trap cleanup EXIT
trap 'exit 130' INT TERM

echo "Starting control server at ${CONTROL_URL}"
(
  cd "$ROOT_DIR/apps/control-server"
  exec setsid env AUTO_HTTP_ADDR="$CONTROL_ADDR" AUTO_CONTROL_URL="$CONTROL_URL" go run ./cmd/control-server
) &
CONTROL_PID=$!

echo "Starting web app at http://${WEB_HOST}:${WEB_PORT}/"
(
  cd "$ROOT_DIR/apps/web"
  exec setsid env VITE_CONTROL_URL="$CONTROL_URL" pnpm dev --host "$WEB_HOST" --port "$WEB_PORT" --strictPort
) &
WEB_PID=$!

echo "Press Ctrl+C to stop both services."
wait -n "$CONTROL_PID" "$WEB_PID"
