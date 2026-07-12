#!/usr/bin/env sh
set -eu

# This script is intentionally run only on the dev VPS. It copies the existing
# dev-only credential material without printing it, then applies non-secret
# SmartCLIProxy settings to the copied configuration.
source_dir=${1:?source runtime directory is required}
target_dir=${2:?target runtime directory is required}

mkdir -p "$target_dir/auths" "$target_dir/logs"
cp "$source_dir/config.yaml" "$target_dir/config.yaml"
cp -a "$source_dir/auths/." "$target_dir/auths/"

sed -i \
  -e 's/^max-retry-credentials:.*/max-retry-credentials: 0/' \
  -e 's/^max-retry-interval:.*/max-retry-interval: 0/' \
  "$target_dir/config.yaml"

grep -q '^retry-budget-ms:' "$target_dir/config.yaml" || cat >> "$target_dir/config.yaml" <<'EOF'

# SmartCLIProxy dev routing policy
retry-budget-ms: 300000
credential-attempt-timeout-ms: 180000
EOF

grep -q '^  smartapi-affinity:' "$target_dir/config.yaml" || sed -i '/^  session-affinity-ttl:/a\  smartapi-affinity: true\n  smartapi-affinity-ttl: "720h"' "$target_dir/config.yaml"
