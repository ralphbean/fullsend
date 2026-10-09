#!/usr/bin/env bash
set -euo pipefail
set +x
umask 077

openshell gateway login
python3 -c 'import secrets; print("Bearer " + secrets.token_urlsafe(32), end="")' > /tmp/wif-auth
chmod 600 /tmp/wif-auth
export WIF_REFRESH_METRICS_FILE=/evidence/${WIF_JOB_NAME:?}/wif-refresh.json
python3 /wif/wif-refresh-server.py >/tmp/wif-refresh-server.log 2>&1 &
server_pid=$!
cleanup() {
  kill "$server_pid" 2>/dev/null || true
  wait "$server_pid" 2>/dev/null || true
  rm -f /tmp/wif-auth /tmp/wif-refresh-server.log
}
trap cleanup EXIT
for _ in {1..30}; do
  if curl --silent --fail http://127.0.0.1:18765/health >/dev/null; then
    break
  fi
  sleep 1
done
curl --silent --fail http://127.0.0.1:18765/health >/dev/null
"$@"
python3 /wif/verify-wif-refresh.py "$WIF_REFRESH_METRICS_FILE"
printf 'Post-rotation WIF Vertex inference succeeded.\n'
