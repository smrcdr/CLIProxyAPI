#!/usr/bin/env bash
set -Eeuo pipefail

local_port="${1:-8324}"
remote_port="20129"
ssh_host="proxy"
remote_management_key="/home/smarer/coding/SmartCLIProxyClaude/runtime-claude/management.key"
ssh_pid=""

if [[ ! "$local_port" =~ ^[0-9]+$ ]] || (( local_port < 1 || local_port > 65535 )); then
  echo "Usage: $0 [local_port]" >&2
  exit 2
fi

cleanup() {
  if [[ -n "$ssh_pid" ]] && kill -0 "$ssh_pid" 2>/dev/null; then
    kill "$ssh_pid" 2>/dev/null || true
    wait "$ssh_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT INT TERM

copy_management_key() {
  local management_key
  management_key="$(
    ssh "$ssh_host" \
      "python3 -c 'import pathlib,sys; key=pathlib.Path(sys.argv[1]).read_text(encoding=\"utf-8\").strip(); assert key and not key.startswith((\"\u00242a\u0024\",\"\u00242b\u0024\",\"\u00242y\u0024\")),\"Plaintext management key is not configured\"; sys.stdout.write(key)' '$remote_management_key'"
  )"

  if command -v wl-copy >/dev/null 2>&1; then
    printf '%s' "$management_key" | wl-copy
  elif command -v xclip >/dev/null 2>&1; then
    printf '%s' "$management_key" | xclip -selection clipboard
  elif command -v xsel >/dev/null 2>&1; then
    printf '%s' "$management_key" | xsel --clipboard --input
  else
    echo "No supported clipboard utility found (wl-copy, xclip, or xsel)." >&2
    return 1
  fi
}

echo "Opening SSH tunnel: 127.0.0.1:${local_port} -> ${ssh_host}:127.0.0.1:${remote_port}"
ssh -N \
  -o ExitOnForwardFailure=yes \
  -o ServerAliveInterval=30 \
  -o ServerAliveCountMax=3 \
  -L "127.0.0.1:${local_port}:127.0.0.1:${remote_port}" \
  "$ssh_host" &
ssh_pid=$!

health_url="http://127.0.0.1:${local_port}/healthz"
admin_url="http://127.0.0.1:${local_port}/smart-management.html"
for _ in {1..60}; do
  if ! kill -0 "$ssh_pid" 2>/dev/null; then
    wait "$ssh_pid" || true
    echo "SSH tunnel stopped before the proxy service became healthy." >&2
    exit 1
  fi
  if curl --silent --fail --max-time 1 "$health_url" >/dev/null; then
    break
  fi
  sleep 0.25
done

if ! curl --silent --fail --max-time 2 "$health_url" >/dev/null; then
  echo "Proxy health check timed out: ${health_url}" >&2
  exit 1
fi

copy_management_key
echo "Management key copied to the clipboard. Paste it into the login form."
echo "SmartCLIProxy proxy admin: ${admin_url}"
if command -v xdg-open >/dev/null 2>&1; then
  xdg-open "$admin_url" >/dev/null 2>&1 &
elif command -v open >/dev/null 2>&1; then
  open "$admin_url" >/dev/null 2>&1 &
else
  echo "Open the URL above in a browser."
fi

echo "Tunnel is active. Press Ctrl-C to stop."
wait "$ssh_pid"
