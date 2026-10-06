#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
[[ $# -eq 0 ]] || fail "Usage: $0"
require_owned
k -n fullsend-runner get secret fullsend-gateway-bundle >/dev/null || fail 'Run 07-gateway-probe.sh once to install the gateway bundle'
k -n fullsend-runner get secret fullsend-gateway-auth >/dev/null || fail 'Gateway service-account Secret is missing'

umask 077
archive="$(mktemp "$LOCAL_DIR/gateway-demo.XXXXXX.tgz")"
trap 'rm -f "$archive"' EXIT
tar -C "$K8S_ROOT/fixtures/harness-demo" -czf "$archive" .fullsend repo
k -n fullsend-runner create configmap fullsend-gateway-demo \
  --from-file="bundle.tgz=$archive" --dry-run=client -o json | k apply -f - >/dev/null

build_gateway_runner
suffix="$(uuidgen | tr '[:upper:]' '[:lower:]' | cut -c1-8)"
job_name="fs-demo-$suffix"
status '⚠️' '!!! OPENSHELL_GATEWAY_INSECURE=true: gateway TLS verification is DISABLED for this PoC !!!'
status '⚠️' '!!! Track the CA/public-hostname fix: https://github.com/openshift-online/hypershell/issues/443 !!!'
(cd "$REPO_ROOT" && go run ./cmd/fullsend k8s-poc render-gateway-demo-job \
  --job-name "$job_name" --runner-image "$GATEWAY_RUNNER_IMAGE") | k apply -f - >/dev/null
status 'ℹ️' "Job: fullsend-runner/$job_name; sandbox: fs-$suffix"
status 'ℹ️' "Logs: kubectl --kubeconfig $KUBECONFIG_POC --context $KIND_CONTEXT -n fullsend-runner logs job/$job_name -c runner"
status 'ℹ️' "Evidence: PVC fullsend-runner-evidence, directory /evidence/$job_name"
