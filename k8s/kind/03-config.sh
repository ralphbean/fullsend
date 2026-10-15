#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"
profile_arg "$@"
require_owned
if [[ ! -f "$LOCAL_DIR/db-password" ]]; then
  umask 077
  printf '%s' "$(openssl rand -hex 24)" > "$LOCAL_DIR/db-password"
fi
password="$(cat "$LOCAL_DIR/db-password")"
printf '%s' "$password" > "$LOCAL_DIR/db-password"
printf 'postgres://fullsend:%s@postgres.fullsend-control.svc.cluster.local:5432/fullsend?sslmode=disable' "$password" > "$LOCAL_DIR/db-url"
chmod 600 "$LOCAL_DIR/db-url"
k apply -f "$K8S_ROOT/manifests/base/infrastructure/namespace.yaml" >/dev/null
k -n fullsend-control create secret generic postgres-auth --from-file="password=$LOCAL_DIR/db-password" --from-file="url=$LOCAL_DIR/db-url" --dry-run=client -o yaml | k apply -f - >/dev/null
status '✅' 'Local database credentials loaded'
