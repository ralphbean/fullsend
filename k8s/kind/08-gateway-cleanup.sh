#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"
profile_arg "$@"
require_owned

cleanup_dispatcher_attempts() {
  local rows attempt_id attempt_name cleanup_name
  rows="$(k -n fullsend-control exec statefulset/postgres -- psql -U fullsend -d fullsend -AtF '|' -c \
    "SELECT a.id,a.job_name FROM attempts a JOIN selected_runs r ON r.id=a.run_id JOIN observations o ON o.id=r.observation_id WHERE (r.harness='gateway-demo' OR (r.harness='triage' AND r.execution='fullsend' AND o.source LIKE 'jira:%')) AND a.cleanup_state!='retired' ORDER BY a.id")"
  [[ -n "$rows" ]] || return 0
  status '🔧' 'Stopping dispatcher runners and verifying every owned remote sandbox is absent'
  while IFS='|' read -r attempt_id attempt_name; do
    [[ -n "$attempt_id" ]] || continue
    [[ "$attempt_name" =~ ^fs-poc-[1-9][0-9]*-[1-9][0-9]*$ ]] || fail "Invalid stored attempt Job name: $attempt_name"
    cleanup_name="fs-clean-${attempt_name#fs-poc-}"
    k -n fullsend-runner delete "job/$attempt_name" --wait=true --ignore-not-found >/dev/null
    k -n fullsend-runner delete "job/$cleanup_name" --wait=true --ignore-not-found >/dev/null
    (cd "$REPO_ROOT" && go run ./cmd/fullsend k8s-poc render-gateway-attempt-cleanup-job \
      --job-name "$attempt_name" --attempt-id "$attempt_id" --runner-image "$runner_image") | k apply -f - >/dev/null
    if ! k -n fullsend-runner wait --for=condition=complete "job/$cleanup_name" --timeout=180s >/dev/null; then
      k -n fullsend-runner logs "job/$cleanup_name" -c runner >&2 || true
      fail "Remote stop for $attempt_name is unverified; keeping the kind cluster"
    fi
    status '✅' "Remote absence verified for dispatcher attempt $attempt_name"
  done <<< "$rows"
}

runner_image=''
if [[ -s "$STATE_DIR/gateway-runner-image" ]]; then
  runner_image="$(cat "$STATE_DIR/gateway-runner-image")"
else
runner_image="$(k -n fullsend-runner get jobs -l 'fullsend.ai/gateway-job in (probe,demo)' \
    -o jsonpath='{.items[0].spec.template.spec.containers[0].image}')"
fi
if [[ -z "$runner_image" ]]; then
  if ! k -n fullsend-runner get secret fullsend-gateway-bundle >/dev/null 2>&1; then
    status '✅' 'No gateway bundle or probe Jobs to reconcile'
    exit 0
  fi
  fail 'Gateway bundle exists but runner image identity is missing; inspect evidence before destroying kind'
fi

status '🔧' 'Stopping gateway probe and demo Jobs before remote sandbox cleanup'
k -n fullsend-runner delete jobs -l 'fullsend.ai/gateway-job in (probe,demo)' --wait=true --ignore-not-found >/dev/null
# !!! TEMPORARY TLS VERIFICATION BYPASS !!!
# !!! The cleanup Job inherits OPENSHELL_GATEWAY_INSECURE=true from its runner
# !!! template. Track the CA and public-hostname fix before removing it:
# !!! https://github.com/openshift-online/hypershell/issues/443
status '⚠️' '!!! Cleanup uses OPENSHELL_GATEWAY_INSECURE=true: gateway TLS certificate verification is DISABLED !!!'
status '⚠️' '!!! Track the CA and public-hostname fix: https://github.com/openshift-online/hypershell/issues/443 !!!'
suffix="$(uuidgen | tr '[:upper:]' '[:lower:]' | cut -c1-8)"
job_name="fs-gateway-$suffix"
(cd "$REPO_ROOT" && go run ./cmd/fullsend k8s-poc render-gateway-cleanup-job \
  --job-name "$job_name" --runner-image "$runner_image") | k apply -f - >/dev/null

status '🔧' "Reconciling remote resources with $job_name"
for _ in {1..180}; do
  succeeded="$(k -n fullsend-runner get job "$job_name" -o jsonpath='{.status.succeeded}')"
  failed="$(k -n fullsend-runner get job "$job_name" -o jsonpath='{.status.failed}')"
  if [[ "$succeeded" == 1 ]]; then
    k -n fullsend-runner logs "job/$job_name" -c runner
    status '✅' 'Gateway cleanup completed'
    cleanup_dispatcher_attempts
    exit 0
  fi
  if [[ -n "$failed" && "$failed" != 0 ]]; then
    k -n fullsend-runner logs "job/$job_name" -c runner || true
    fail "Gateway cleanup failed; inspect Job $job_name and the evidence PVC before deleting kind"
  fi
  sleep 5
done
fail "Gateway cleanup timed out; inspect Job $job_name and the evidence PVC before deleting kind"
