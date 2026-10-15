#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"

usage() {
  fail "Usage: $0 --project-number NUMBER --pool POOL --provider PROVIDER --service-account-email EMAIL --vertex-project PROJECT_ID"
}

project_number=''
pool=''
provider=''
service_account_email=''
vertex_project=''
while [[ $# -gt 0 ]]; do
  case "$1" in
    --project-number) [[ $# -ge 2 ]] || usage; project_number="$2"; shift 2 ;;
    --pool) [[ $# -ge 2 ]] || usage; pool="$2"; shift 2 ;;
    --provider) [[ $# -ge 2 ]] || usage; provider="$2"; shift 2 ;;
    --service-account-email) [[ $# -ge 2 ]] || usage; service_account_email="$2"; shift 2 ;;
    --vertex-project) [[ $# -ge 2 ]] || usage; vertex_project="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ "$project_number" =~ ^[1-9][0-9]*$ && "$pool" =~ ^[a-z][a-z0-9-]*$ && "$provider" =~ ^[a-z][a-z0-9-]*$ ]] || usage
[[ "$service_account_email" =~ ^[a-z][a-z0-9-]*@[a-z][a-z0-9-]*\.iam\.gserviceaccount\.com$ ]] || usage
[[ "$vertex_project" =~ ^[a-z][a-z0-9-]*$ ]] || usage
require_owned
for tool in jq podman python3 uuidgen; do command -v "$tool" >/dev/null || fail "Missing $tool"; done
k -n fullsend-runner get serviceaccount runner-inference >/dev/null || fail 'Inference ServiceAccount is missing'
k -n fullsend-runner get secret fullsend-gateway-bundle >/dev/null || fail 'Run 07-gateway-probe.sh to install the gateway connection bundle'
k -n fullsend-runner get secret fullsend-gateway-auth >/dev/null || fail 'Gateway service-account Secret is missing'

status '🔐' 'Preparing keyless Google credentials for the inference ServiceAccount'
umask 077
mkdir -p "$LOCAL_DIR/gcp"
build_gateway_runner
audience="//iam.googleapis.com/projects/$project_number/locations/global/workloadIdentityPools/$pool/providers/$provider"
credential_file="$LOCAL_DIR/gcp/fullsend-credential.json"
jq -n --arg audience "$audience" --arg service_account "$service_account_email" \
  '{type:"external_account",audience:$audience,subject_token_type:"urn:ietf:params:oauth:token-type:jwt",token_url:"https://sts.googleapis.com/v1/token",service_account_impersonation_url:("https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/"+$service_account+":generateAccessToken"),credential_source:{file:"/sandbox/workspace/.gcp-oidc-token",format:{type:"json",subject_token_field_name:"value"}}}' \
  > "$credential_file"

archive="$(mktemp "$LOCAL_DIR/wif-fullsend.XXXXXX.tgz")"
manifest=''
suspended=''
restore_dispatcher() {
  rm -f "$archive"
  if [[ -n "$manifest" ]]; then rm -f "$manifest"; fi
  if [[ -n "$suspended" ]]; then
    status '🔄' 'Restoring the fixture dispatcher schedule'
    k -n fullsend-control patch cronjob fixture-dispatcher --type merge \
      -p "{\"spec\":{\"suspend\":$suspended}}" >/dev/null || true
  fi
}
trap restore_dispatcher EXIT

status '⏸️' 'Pausing the fixture dispatcher so it cannot overlap with this demonstration run'
suspended="$(k -n fullsend-control get cronjob fixture-dispatcher -o jsonpath='{.spec.suspend}')"
k -n fullsend-control patch cronjob fixture-dispatcher --type merge -p '{"spec":{"suspend":true}}' >/dev/null

status '📦' 'Packaging the inference demo and preparing the WIF and gateway configuration for the Job'
tar -C "$K8S_ROOT/fixtures/inference-demo" -czf "$archive" .fullsend repo
k -n fullsend-runner create configmap fullsend-gateway-demo \
  --from-file="bundle.tgz=$archive" --dry-run=client -o json | k apply -f - >/dev/null
k -n fullsend-runner create configmap fullsend-wif-fullsend \
  --from-file=credential.json="$credential_file" \
  --from-file=run-inference.sh="$K8S_ROOT/gcp/run-wif-fullsend.sh" \
  --from-file=wif-refresh-server.py="$K8S_ROOT/gcp/wif-refresh-server.py" \
  --from-file=verify-wif-refresh.py="$K8S_ROOT/gcp/verify-wif-refresh.py" \
  --from-file=sandbox-vertex-check.sh="$K8S_ROOT/gcp/sandbox-vertex-check.sh" \
  --from-file=wif-wait-for-rotation.sh="$K8S_ROOT/gcp/wif-wait-for-rotation.sh" \
  --dry-run=client -o yaml | k apply -f - >/dev/null

suffix="$(uuidgen | tr '[:upper:]' '[:lower:]' | cut -c1-8)"
job_name="fs-demo-$suffix"
sandbox_name="fs-$suffix"
manifest="$LOCAL_DIR/gcp/$job_name.json"
pod_image="$(cat "$STATE_DIR/gateway-runner-image")"
(cd "$REPO_ROOT" && go run ./cmd/fullsend k8s-poc render-gateway-demo-job \
  --job-name "$job_name" --runner-image "$pod_image") > "$manifest"
jq --arg job "$job_name" --arg audience "$audience" --arg vertex_project "$vertex_project" \
  --arg sandbox "$sandbox_name" --arg evidence "/evidence/$job_name" '
  .metadata.labels["fullsend.ai/wif-fullsend"] = "true" |
  .spec.activeDeadlineSeconds = 2400 |
  .spec.template.spec.serviceAccountName = "runner-inference" |
  .spec.template.spec.volumes += [
    {name:"wif-config",configMap:{name:"fullsend-wif-fullsend",defaultMode:288}},
    {name:"wif-token",projected:{sources:[{serviceAccountToken:{path:"token",audience:$audience,expirationSeconds:600}}]}}
  ] |
  .spec.template.spec.containers[0].command = ["sh","/wif/run-inference.sh","/usr/local/bin/fullsend","k8s-poc","gateway-demo","--job-name",$job,"--sandbox-name",$sandbox,"--demo-dir","/demo","--evidence-dir",$evidence] |
  .spec.template.spec.containers[0].env += [
    {name:"WIF_JOB_NAME",value:$job},
    {name:"GOOGLE_APPLICATION_CREDENTIALS",value:"/wif/credential.json"},
    {name:"GCP_OIDC_TOKEN_FILE",value:"/var/run/fullsend/wif/token"},
    {name:"FULLSEND_GCP_OIDC_URL",value:"http://127.0.0.1:18765/token"},
    {name:"FULLSEND_GCP_OIDC_AUTH_FILE",value:"/tmp/wif-auth"},
    {name:"ANTHROPIC_VERTEX_PROJECT_ID",value:$vertex_project},
    {name:"CLOUD_ML_REGION",value:"global"}
  ] |
  .spec.template.spec.containers[0].volumeMounts += [
    {name:"wif-config",mountPath:"/wif",readOnly:true},
    {name:"wif-token",mountPath:"/var/run/fullsend/wif",readOnly:true}
  ]
' "$manifest" > "$manifest.tmp"
mv "$manifest.tmp" "$manifest"

status '🚀' 'Starting the real Fullsend gateway demo with a projected Kubernetes identity token'
k create -f "$manifest" >/dev/null
status '⏳' "Waiting for Fullsend and HyperShell to complete inference before and after token rotation (Job $job_name; up to about 15 minutes)"

deadline=$((SECONDS + 1850))
result=''
while (( SECONDS < deadline )); do
  job_json="$(k -n fullsend-runner get "job/$job_name" -o json)"
  if jq -e '(.status.conditions // []) | any(.[]; .type == "Complete" and .status == "True")' <<<"$job_json" >/dev/null; then
    result=complete
    break
  elif jq -e '(.status.conditions // []) | any(.[]; .type == "Failed" and .status == "True")' <<<"$job_json" >/dev/null; then
    result=failed
    break
  fi
  sleep 10
done
log_file="$STATE_DIR/wif-fullsend-$job_name.log"
status '🔎' 'Checking the runner log for token rotation and validated inference output'
k -n fullsend-runner logs "job/$job_name" -c runner > "$log_file" 2>&1 || true
[[ "$result" == complete ]] || fail "Fullsend WIF integration did not complete; inspect $log_file and PVC evidence /evidence/$job_name"
rg -q '^Fullsend fetched an inference assertion before and after projected-token rotation\.$' "$log_file" \
  || fail "Fullsend refresh did not span assertion rotation; inspect $log_file"
rg -q '"output_sha256":"[0-9a-f]{64}"' "$log_file" \
  || fail "Fullsend did not record validated post-rotation inference output; inspect $log_file"

status '📝' 'The end-to-end checks passed; writing a summary with log and PVC evidence locations'
report="$STATE_DIR/milestone-6-fullsend-$job_name.md"
cat > "$report" <<EOF
# Milestone 6 Fullsend WIF integration

- Fullsend run: $job_name completed through the configured HyperShell gateway.
- The trusted runner refreshed the inference-only projected assertion into the remote sandbox.
- The remote sandbox confirmed the assertion changed, exchanged the refreshed assertion, impersonated the scoped inference service account, and completed Vertex inference.
- The runner recorded assertion SHA-256 values only; neither assertions nor access tokens appear in the report.
- Runner log: $log_file.
- PVC evidence: /evidence/$job_name.
EOF
status '🧹' 'Removing the temporary demo Job and its configuration'
k -n fullsend-runner delete job "$job_name" --wait=true >/dev/null
k -n fullsend-runner delete configmap fullsend-wif-fullsend fullsend-gateway-demo --ignore-not-found >/dev/null
status '✅' "Fullsend completed inference across token rotation. Temporary resources are removed; dispatcher schedule restoration runs during exit cleanup. Summary: $report"
