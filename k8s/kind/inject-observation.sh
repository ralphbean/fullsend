#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
command -v uuidgen >/dev/null || fail 'Missing uuidgen; install it before injecting an observation'
command -v jq >/dev/null || fail 'Missing jq; install it before injecting an observation'
require_owned
fixture="$LOCAL_DIR/workload/fixture.json"
[[ -f "$fixture" && -f "$LOCAL_DIR/workload/kustomization.yaml" ]] || fail 'Deploy the fixture profile first'

key="$(uuidgen | tr '[:upper:]' '[:lower:]')"
entity="$(uuidgen | tr '[:upper:]' '[:lower:]')"
tmp="$(mktemp "$LOCAL_DIR/workload/fixture.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
jq --arg key "$key" --arg entity "$entity" \
  '.cursor = $key | .observations += [{key: $key, entity: $entity, revision: "1", kind: "opened", behavior: "success", authorized: true}]' \
  "$fixture" > "$tmp"
mv "$tmp" "$fixture"
k apply -k "$LOCAL_DIR/workload" >/dev/null

job="inject-poll-$key"
k -n fullsend-control create job "$job" --from=cronjob/fixture-poller >/dev/null
k -n fullsend-control wait --for=condition=complete "job/$job" --timeout=90s >/dev/null
count="$(k -n fullsend-control exec statefulset/postgres -- psql -U fullsend -d fullsend -Atc \
  "SELECT count(*) FROM observations WHERE observation_key='$key' AND entity='$entity'")"
[[ "$count" == 1 ]] || fail "Expected one persisted observation for $key; found $count"
status '✅' "Observation persisted: key=$key entity=$entity"

dispatch_job="inject-dispatch-$key"
k -n fullsend-control create job "$dispatch_job" --from=cronjob/fixture-dispatcher >/dev/null
k -n fullsend-control wait --for=condition=complete "job/$dispatch_job" --timeout=90s >/dev/null
for _ in {1..20}; do
  row="$(k -n fullsend-control exec statefulset/postgres -- psql -U fullsend -d fullsend -AtF '|' -c \
    "SELECT r.id,r.state,coalesce(a.job_name,''),coalesce(a.job_uid,''),coalesce(a.outcome,'')
     FROM observations o JOIN selected_runs r ON r.observation_id=o.id
     LEFT JOIN attempts a ON a.run_id=r.id WHERE o.observation_key='$key'
     ORDER BY a.number DESC LIMIT 1")"
  IFS='|' read -r run_id run_state runner_job job_uid outcome <<< "$row"
  [[ -n "$job_uid" ]] && break
  sleep 1
done
if [[ -n "$job_uid" ]]; then
  status '✅' "Runner Job launched: $runner_job (UID $job_uid); run $run_id is $run_state; outcome ${outcome:-pending}"
  status 'ℹ️' 'The Job may be removed after reconciliation; its name, UID, and outcome remain in PostgreSQL'
else
  status 'ℹ️' "Run $run_id is ${run_state:-pending}; no runner Job UID yet. The scheduled dispatcher retries every minute"
fi
