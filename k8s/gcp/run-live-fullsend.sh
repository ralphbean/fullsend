#!/usr/bin/env bash
set -euo pipefail
set +x
umask 077

export WIF_REFRESH_METRICS_FILE="/evidence/${WIF_JOB_NAME:?}/wif-refresh.json"
evidence_dir="/evidence/${WIF_JOB_NAME:?}"
output_file="$evidence_dir/runs/${FULLSEND_SANDBOX_NAME:?}/iteration-1/output/agent-result.json"
mkdir -p "$evidence_dir"
started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
python3 - "$evidence_dir/intent.json" "$WIF_JOB_NAME" "$FULLSEND_SANDBOX_NAME" "$started_at" <<'PY'
import json
import pathlib
import sys

pathlib.Path(sys.argv[1]).write_text(json.dumps({"job":sys.argv[2],"sandbox":sys.argv[3],"started_at":sys.argv[4]},separators=(",",":"))+"\n")
PY
python3 -c 'import secrets; print("Bearer " + secrets.token_urlsafe(32), end="")' > /tmp/wif-auth
chmod 600 /tmp/wif-auth
python3 /wif/wif-refresh-server.py >/tmp/wif-refresh-server.log 2>&1 &
server_pid=$!
cleanup() {
  kill "$server_pid" 2>/dev/null || true
  wait "$server_pid" 2>/dev/null || true
  rm -f /tmp/wif-auth /tmp/wif-refresh-server.log
}
trap cleanup EXIT
for _ in {1..30}; do
  if curl --silent --fail http://127.0.0.1:18765/health >/dev/null; then break; fi
  sleep 1
done
curl --silent --fail http://127.0.0.1:18765/health >/dev/null
set +e
"$@"
run_status=$?
set -e
finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
if (( run_status != 0 )); then
  python3 - "$evidence_dir/result.json" "$WIF_JOB_NAME" "$FULLSEND_SANDBOX_NAME" "$started_at" "$finished_at" "$run_status" <<'PY'
import json
import pathlib
import sys

pathlib.Path(sys.argv[1]).write_text(json.dumps({"job":sys.argv[2],"sandbox":sys.argv[3],"started_at":sys.argv[4],"finished_at":sys.argv[5],"status":"failed","exit_code":int(sys.argv[6])},separators=(",",":"))+"\n")
PY
  exit "$run_status"
fi
python3 - "$WIF_REFRESH_METRICS_FILE" <<'PY'
import json
import pathlib
import sys

metrics = json.loads(pathlib.Path(sys.argv[1]).read_text())
if not metrics.get("requests"):
    raise SystemExit("Fullsend run did not refresh the projected inference assertion")
print("Fullsend triage fetched its inference assertion through the projected-token refresh endpoint.")
PY
python3 - "$output_file" "$evidence_dir/result.json" "$WIF_JOB_NAME" "$FULLSEND_SANDBOX_NAME" "$started_at" "$finished_at" <<'PY'
import hashlib
import json
import pathlib
import sys

output = pathlib.Path(sys.argv[1]).read_bytes()
json.loads(output)
result = {"job":sys.argv[3],"sandbox":sys.argv[4],"started_at":sys.argv[5],"finished_at":sys.argv[6],"status":"succeeded","output_sha256":hashlib.sha256(output).hexdigest()}
pathlib.Path(sys.argv[2]).write_text(json.dumps(result,separators=(",",":"))+"\n")
print(json.dumps(result,separators=(",",":")))
PY
