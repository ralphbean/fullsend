#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"

usage() {
  fail "Usage: $0 --attempt-id ID --old-gateway-label LABEL --acknowledge-deleted-gateway"
}

attempt_id=''
old_gateway_label=''
acknowledged=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --attempt-id) [[ $# -ge 2 ]] || usage; attempt_id="$2"; shift 2 ;;
    --old-gateway-label) [[ $# -ge 2 ]] || usage; old_gateway_label="$2"; shift 2 ;;
    --acknowledge-deleted-gateway) acknowledged=true; shift ;;
    *) usage ;;
  esac
done
[[ "$attempt_id" =~ ^[1-9][0-9]*$ ]] || usage
[[ "$old_gateway_label" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] || usage
[[ "$acknowledged" == true ]] || usage
require_owned
command -v podman >/dev/null || fail 'Podman is required to inspect the kind evidence PVC'
umask 077

active_gateway_file="$LOCAL_DIR/gateway/openshell/active_gateway"
if [[ -s "$active_gateway_file" ]]; then
  [[ "$(cat "$active_gateway_file")" != "$old_gateway_label" ]] \
    || fail 'The old gateway label is still the active OpenShell gateway'
fi

row="$(k -n fullsend-control exec statefulset/postgres -- psql -U fullsend -d fullsend -AtF '|' -c \
  "SELECT a.job_name,a.state,a.outcome,a.cleanup_state,r.id,r.state,r.harness,o.source
   FROM attempts a JOIN selected_runs r ON r.id=a.run_id
   JOIN observations o ON o.id=r.observation_id WHERE a.id=$attempt_id")"
[[ -n "$row" ]] || fail "Attempt $attempt_id does not exist"
IFS='|' read -r job_name attempt_state outcome cleanup_state run_id run_state harness source <<< "$row"
[[ "$job_name" =~ ^fs-poc-[1-9][0-9]*-[1-9][0-9]*$ ]] || fail 'Attempt is missing or has an unexpected Job name'
[[ "$attempt_state" == cleanup && "$outcome" == failed && "$cleanup_state" == pending ]] \
  || fail 'Attempt is not a failed gateway run awaiting cleanup'
[[ "$run_state" == reserved && "$harness" == gateway-demo && "$source" == fixture-source ]] \
  || fail 'Selected run is not the reserved fixture gateway demo'
[[ -z "$(k -n fullsend-runner get "job/$job_name" --ignore-not-found -o name)" ]] \
  || fail 'Runner Job still exists; stop it before retirement'

cleanup_name="fs-clean-${job_name#fs-poc-}"
cleanup_job="$(k -n fullsend-runner get "job/$cleanup_name" -o json)" \
  || fail 'Failed cleanup Job is missing; inspect its evidence before retirement'
jq -e 'any(.status.conditions[]?; .type == "Failed" and .status == "True")' \
  <<< "$cleanup_job" >/dev/null || fail 'Cleanup Job is not terminally failed'

pv_name="$(k -n fullsend-runner get pvc fullsend-runner-evidence -o jsonpath='{.spec.volumeName}')"
[[ "$pv_name" =~ ^pvc-[0-9a-f-]+$ ]] || fail 'Unexpected evidence PV name'
pv_path="$(k get pv "$pv_name" -o jsonpath='{.spec.hostPath.path}')"
[[ "$pv_path" == /var/local-path-provisioner/*_fullsend-runner_fullsend-runner-evidence ]] \
  || fail 'Evidence PVC is not the owned kind local-path volume'
node="${KIND_NAME}-control-plane"
podman exec "$node" test ! -e "$pv_path/$job_name" \
  || fail 'Runner evidence directory exists; remote sandbox creation cannot be ruled out'

archive_dir="$STATE_DIR/retired-gateways/$old_gateway_label/attempt-$attempt_id"
[[ ! -e "$archive_dir" ]] || fail "Archive already exists: $archive_dir"
mkdir -p "$archive_dir"
printf '%s\n' "$cleanup_job" > "$archive_dir/cleanup-job.json"
k -n fullsend-runner logs "job/$cleanup_name" -c runner > "$archive_dir/cleanup.log" \
  || fail 'Cannot archive failed cleanup logs; reservation remains held'
jq -n --arg gateway "$old_gateway_label" --argjson attempt "$attempt_id" \
  --argjson run "$run_id" --arg job "$job_name" \
  --arg archived_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{old_gateway_label:$gateway,attempt_id:$attempt,run_id:$run,job:$job,
    archived_at:$archived_at,remote_cleanup:"unverified",
    basis:"operator attested gateway deletion; runner Job and evidence directory absent; cleanup Job terminally failed"}' \
  > "$archive_dir/retirement.json"

# Keep this exception distinct from cleanup_state=done. The gateway was
# deleted before its old sandbox could be queried, so no remote absence is
# claimed. Both state changes must commit atomically or neither does.
cat > "$archive_dir/retire.sql" <<SQL
DO \$\$
DECLARE target_run bigint;
BEGIN
  UPDATE attempts a SET state='complete',cleanup_state='retired'
    WHERE a.id=$attempt_id AND a.state='cleanup' AND a.outcome='failed'
      AND a.cleanup_state='pending'
      AND EXISTS (
        SELECT 1 FROM selected_runs r JOIN observations o ON o.id=r.observation_id
        WHERE r.id=a.run_id AND r.state='reserved' AND r.harness='gateway-demo'
          AND o.source='fixture-source')
    RETURNING a.run_id INTO target_run;
  IF target_run IS NULL THEN
    RAISE EXCEPTION 'attempt no longer qualifies for retirement';
  END IF;
  UPDATE selected_runs SET state='failed' WHERE id=target_run AND state='reserved';
  IF NOT FOUND THEN
    RAISE EXCEPTION 'selected run no longer holds its reservation';
  END IF;
END
\$\$;
SQL
k -n fullsend-control exec -i statefulset/postgres -- \
  psql -U fullsend -d fullsend -v ON_ERROR_STOP=1 -f - < "$archive_dir/retire.sql" >/dev/null \
  || fail "Retirement transaction failed; inspect $archive_dir and the database"

verified="$(k -n fullsend-control exec statefulset/postgres -- psql -U fullsend -d fullsend -AtF '|' -c \
  "SELECT a.state,a.cleanup_state,r.state FROM attempts a JOIN selected_runs r ON r.id=a.run_id WHERE a.id=$attempt_id")"
[[ "$verified" == 'complete|retired|failed' ]] || fail 'Retired database state did not match expectations'
status '✅' "Retired preflight-only attempt $attempt_id; its reservation is released and its run failed"
status '⚠️' "Remote cleanup remains UNVERIFIED; preserve $archive_dir"
