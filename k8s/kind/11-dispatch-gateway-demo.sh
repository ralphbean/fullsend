#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
[[ "$PROFILE" == fixture ]] || fail 'Gateway dispatch demo requires --profile fixture'
require_owned
command -v uuidgen >/dev/null || fail 'Missing uuidgen'
command -v jq >/dev/null || fail 'Missing jq'
fixture="$LOCAL_DIR/workload/fixture.json"
selection="$LOCAL_DIR/workload/selection.json"
[[ -s "$fixture" && -s "$selection" ]] || fail 'Deploy the fixture profile first'
k -n fullsend-runner get secret fullsend-gateway-bundle >/dev/null || fail 'Install the gateway bundle with 07-gateway-probe.sh'
k -n fullsend-runner get secret fullsend-gateway-auth >/dev/null || fail 'Gateway service-account Secret is missing'

umask 077
archive="$(mktemp "$LOCAL_DIR/gateway-dispatch.XXXXXX.tgz")"
fixture_tmp="$(mktemp "$LOCAL_DIR/workload/fixture.XXXXXX")"
selection_tmp="$(mktemp "$LOCAL_DIR/workload/selection.XXXXXX")"
dispatcher_suspended="$(k -n fullsend-control get cronjob fixture-dispatcher -o jsonpath='{.spec.suspend}')"
on_exit() {
  rm -f "$archive" "$fixture_tmp" "$selection_tmp"
  k -n fullsend-control patch cronjob fixture-dispatcher --type merge -p "{\"spec\":{\"suspend\":${dispatcher_suspended:-false}}}" >/dev/null || true
}
trap on_exit EXIT
k -n fullsend-control patch cronjob fixture-dispatcher --type merge -p '{"spec":{"suspend":true}}' >/dev/null
tar -C "$K8S_ROOT/fixtures/harness-demo" -czf "$archive" .fullsend repo
k -n fullsend-runner create configmap fullsend-gateway-demo \
  --from-file="bundle.tgz=$archive" --dry-run=client -o json | k apply -f - >/dev/null
build_gateway_runner

key="$(uuidgen | tr '[:upper:]' '[:lower:]')"
entity="$(uuidgen | tr '[:upper:]' '[:lower:]')"
jq --arg key "$key" --arg entity "$entity" \
  '.cursor = $key | .observations += [{key: $key, entity: $entity, revision: "1", kind: "gateway_demo", behavior: "success", authorized: true}]' \
  "$fixture" > "$fixture_tmp"
jq '.by_kind.gateway_demo = ["gateway-demo"]' "$selection" > "$selection_tmp"
mv "$fixture_tmp" "$fixture"
mv "$selection_tmp" "$selection"
k apply -k "$LOCAL_DIR/workload" >/dev/null
k -n fullsend-control set env cronjob/fixture-dispatcher "GATEWAY_RUNNER_IMAGE=$GATEWAY_RUNNER_IMAGE" >/dev/null

suffix="${key:0:8}"
poll_job="gateway-poll-$suffix"
k -n fullsend-control create job "$poll_job" --from=cronjob/fixture-poller >/dev/null
wait_control_job() {
  local job="$1" status_value
  for _ in {1..45}; do
    status_value="$(k -n fullsend-control get "job/$job" -o jsonpath='{.status.succeeded}|{.status.failed}')"
    case "$status_value" in
      '1|'|'1|0') return 0 ;;
      *'|1') k -n fullsend-control logs "job/$job" >&2 || true; return 1 ;;
    esac
    sleep 2
  done
  k -n fullsend-control logs "job/$job" >&2 || true
  return 1
}
wait_control_job "$poll_job" || fail 'Gateway fixture poll failed'
db_query() { k -n fullsend-control exec statefulset/postgres -- psql -U fullsend -d fullsend -Atc "$1"; }
count="$(db_query "SELECT count(*) FROM observations WHERE observation_key='$key' AND entity='$entity'")"
[[ "$count" == 1 ]] || fail "Expected one persisted gateway observation; found $count"
status '✅' "Gateway observation persisted: key=$key entity=$entity"

for cycle in {1..160}; do
  dispatch_job="gateway-dispatch-$suffix-$cycle"
  k -n fullsend-control create job "$dispatch_job" --from=cronjob/fixture-dispatcher >/dev/null
  if ! wait_control_job "$dispatch_job"; then
    fail "Dispatcher cycle $cycle failed"
  fi
  row="$(db_query "SELECT r.state,coalesce(a.job_name,''),coalesce(a.cleanup_state,'') FROM observations o JOIN selected_runs r ON r.observation_id=o.id LEFT JOIN attempts a ON a.run_id=r.id WHERE o.observation_key='$key' AND r.harness='gateway-demo' ORDER BY a.number DESC LIMIT 1")"
  IFS='|' read -r state job_name cleanup_state <<< "$row"
  [[ -n "$job_name" ]] || fail 'Gateway observation was not selected and reserved'
  if [[ "$cycle" == 1 ]]; then
    status 'ℹ️' "Runner Job: fullsend-runner/$job_name; sandbox derives from that durable attempt name"
    status 'ℹ️' "Evidence: PVC fullsend-runner-evidence at /evidence/$job_name"
  fi
  if [[ "$state" == succeeded && "$cleanup_state" == 'done' ]]; then
    cleanup_job="fs-clean-${job_name#fs-poc-}"
    k -n fullsend-runner get "job/$cleanup_job" -o jsonpath='{.status.succeeded}' | rg -qx 1 || fail 'Remote cleanup Job did not succeed'
    status '✅' "Gateway demo succeeded; $cleanup_job verified remote stop before reservation release"
    status 'ℹ️' "Runner logs: kubectl --kubeconfig $KUBECONFIG_POC --context $KIND_CONTEXT -n fullsend-runner logs job/$job_name -c runner (Job may already be deleted)"
    exit 0
  fi
  [[ "$state" != failed ]] || fail "Gateway demo failed; inspect /evidence/$job_name and runner/cleanup Job logs"
  sleep 8
done
fail 'Gateway demo did not complete within the dispatch timeout; inspect runner and cleanup Jobs'
