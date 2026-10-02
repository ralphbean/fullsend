#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
[[ "$PROFILE" == fixture ]] || fail 'Use 06-live-smoke.sh for the live profile'
require_owned
id="$(date -u +%Y%m%d%H%M%S)-$$"
report="$STATE_DIR/smoke-$id"
mkdir -p "$report"
on_exit() {
  local code="$1"
  if [[ "$code" -ne 0 ]]; then
    k -n fullsend-control get job,pod -o wide > "$report/control-state.txt" 2>&1 || true
    k -n fullsend-runner get job,pod -o wide > "$report/runner-state.txt" 2>&1 || true
    k -n fullsend-control get events --sort-by=.lastTimestamp > "$report/control-events.txt" 2>&1 || true
    status '❌' "Smoke failed; redacted resource diagnostics: $report"
  fi
}
trap 'on_exit $?' EXIT
cp "$STATE_DIR/build.env" "$report/build.env"
cp "$K8S_ROOT/kind/versions.env" "$report/versions.env"
kubectl version --client > "$report/kubectl-version.txt"
kind version > "$report/kind-version.txt"
podman version --format '{{.Client.Version}}' > "$report/podman-version.txt"
go version > "$report/go-version.txt"
podman stats --no-stream --format '{{.Name}} {{.CPUPerc}} {{.MemUsage}}' "${KIND_NAME}-control-plane" > "$report/node-usage.txt"
trigger() {
  local cron="$1" job="$2"
  k -n fullsend-control create job "$job" --from="cronjob/$cron" >/dev/null
  k -n fullsend-control wait --for=condition=complete "job/$job" --timeout=90s >/dev/null
}
status '🔧' 'Running fixture poll and dispatch cycles'
trigger fixture-poller "smoke-poll-$id"
trigger fixture-dispatcher "smoke-dispatch-$id"
db_query() { k -n fullsend-control exec statefulset/postgres -- psql -U fullsend -d fullsend -Atc "$1"; }
for n in 1 2 3 4 5 6 7 8 9 10; do
  counts="$(db_query "SELECT (SELECT count(*) FROM selected_runs WHERE state='succeeded'),(SELECT count(*) FROM selected_runs WHERE state='failed')")"
  [[ "$counts" == '1|1' ]] && break
  k -n fullsend-runner wait --for=condition=complete job/fs-poc-1-1 --timeout=15s >/dev/null 2>&1 || true
  k -n fullsend-runner wait --for=condition=failed job/fs-poc-2-1 --timeout=15s >/dev/null 2>&1 || true
  trigger fixture-dispatcher "smoke-reconcile-$n-$id"
done
counts="$(db_query "SELECT (SELECT count(*) FROM observations),(SELECT count(*) FROM selected_runs WHERE state='succeeded'),(SELECT count(*) FROM selected_runs WHERE state='failed'),(SELECT count(*) FROM observations WHERE selection_state='no_match'),(SELECT count(*) FROM observations WHERE selection_state='denied')")"
printf '%s\n' "$counts" > "$report/counts.txt"
[[ "$counts" == '4|1|1|1|1' ]] || fail "Smoke check failed: unexpected counts $counts"
printf 'command: k8s/kind/06-smoke.sh --profile fixture\nexpected: 4 observations, 1 succeeded, 1 failed, 1 no-match, 1 denied\nobserved: %s\nselection_boundary: fixture kind mapping; live CEL selection is outside this milestone\n' "$counts" > "$report/summary.txt"
k -n fullsend-control get jobs -o wide > "$report/control-jobs.txt"
k -n fullsend-runner get jobs -o wide > "$report/runner-jobs.txt" || true
status '✅' "Smoke cycles completed; report: $report"
