#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
[[ "$PROFILE" == live ]] || fail 'Use --profile live'
require_owned
id="$(date -u +%Y%m%d%H%M%S)-$$"
report="$STATE_DIR/live-smoke-$id"
mkdir -p "$report"
trigger() {
  local cron="$1" job="$2"
  k -n fullsend-control create job "$job" --from="cronjob/$cron" >/dev/null
  k -n fullsend-control wait --for=condition=complete "job/$job" --timeout=120s >/dev/null
  k -n fullsend-control logs "job/$job" > "$report/$job.log"
}
trigger jira-poller "live-poll-$id" || fail "Live poll failed; inspect control Jobs in $report"
trigger jira-dispatcher "live-dispatch-$id" || fail "Live dispatch failed; inspect control Jobs in $report"
k -n fullsend-control exec statefulset/postgres -- psql -U fullsend -d fullsend -Atc \
  "SELECT count(*) FROM observations WHERE source LIKE 'jira:%'" > "$report/observations.txt"
k -n fullsend-control exec statefulset/postgres -- psql -U fullsend -d fullsend -Atc \
  "SELECT o.entity,o.kind,o.selection_state,COALESCE(r.harness,''),COALESCE(r.state,''),COALESCE(a.job_name,''),COALESCE(a.job_uid,''),COALESCE(a.outcome,'') FROM observations o LEFT JOIN selected_runs r ON r.observation_id=o.id LEFT JOIN attempts a ON a.run_id=r.id WHERE o.source LIKE 'jira:%' ORDER BY o.id" > "$report/runs.txt"
status '✅' "Live Jira cycle completed; report: $report"
