#!/usr/bin/env bash
set -Eeuo pipefail

environment="dev"
local_port=""

usage() {
  echo "Usage: smartapi-ui [dev|prod] [local-port]"
}

for argument in "$@"; do
  case "$argument" in
    dev|prod)
      environment="$argument"
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    ''|*[!0-9]*)
      usage >&2
      exit 2
      ;;
    *)
      if [[ -n "$local_port" ]]; then
        usage >&2
        exit 2
      fi
      local_port="$argument"
      ;;
  esac
done

script_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
admin_script="${script_directory}/${environment}-admin.sh"

if [[ ! -x "$admin_script" ]]; then
  echo "Admin launcher is not executable: $admin_script" >&2
  exit 1
fi

if [[ -n "$local_port" ]]; then
  exec "$admin_script" "$local_port"
fi
exec "$admin_script"
