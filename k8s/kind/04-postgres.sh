#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"
profile_arg "$@"
require_owned
k apply -k "$K8S_ROOT/manifests/base/infrastructure" >/dev/null
status '⏳' 'Waiting for PostgreSQL'
k -n fullsend-control rollout status statefulset/postgres --timeout=180s >/dev/null || fail 'PostgreSQL readiness failed'
status '✅' 'PostgreSQL ready'
