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
for tool in jq podman; do command -v "$tool" >/dev/null || fail "Missing $tool"; done
k -n fullsend-runner get serviceaccount runner >/dev/null || fail 'Runner namespace is missing'
k -n fullsend-runner get serviceaccount runner-inference >/dev/null || fail 'Inference ServiceAccount is missing'

status '🔐' 'Preparing keyless Google credentials that use short-lived tokens from Kubernetes'
umask 077
mkdir -p "$LOCAL_DIR/gcp"
build_gateway_runner

audience="//iam.googleapis.com/projects/$project_number/locations/global/workloadIdentityPools/$pool/providers/$provider"
credential_file="$LOCAL_DIR/gcp/credential.json"
configmap_name=fullsend-wif-probe
jq -n --arg audience "$audience" --arg service_account "$service_account_email" \
  --arg token_file /var/run/fullsend/wif/token \
  '{type:"external_account",audience:$audience,subject_token_type:"urn:ietf:params:oauth:token-type:jwt",token_url:"https://sts.googleapis.com/v1/token",credential_source:{file:$token_file,format:{type:"text"}},service_account_impersonation_url:("https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/"+$service_account+":generateAccessToken")}' \
  > "$credential_file"

k -n fullsend-runner create configmap "$configmap_name" \
  --from-file=credential.json="$credential_file" \
  --from-file=probe.sh="$K8S_ROOT/gcp/wif-probe.sh" \
  --dry-run=client -o yaml | k apply -f - >/dev/null

suffix="$(uuidgen | tr '[:upper:]' '[:lower:]' | cut -c1-8)"
denied_job="fs-wif-deny-$suffix"
authorized_job="fs-wif-ok-$suffix"
pod_image="$(cat "$STATE_DIR/gateway-runner-image")"

# Build each Job with an audience-bound, short-lived token.
render_job() {
  local name="$1" service_account="$2" mode="$3" destination="$4"
  jq -n --arg name "$name" --arg image "$pod_image" --arg sa "$service_account" \
    --arg mode "$mode" --arg vertex_project "$vertex_project" --arg audience "$audience" \
    '{apiVersion:"batch/v1",kind:"Job",metadata:{name:$name,namespace:"fullsend-runner",labels:{"app.kubernetes.io/part-of":"fullsend-kind-poc","fullsend.ai/wif-probe":"true"}},spec:{backoffLimit:0,activeDeadlineSeconds:1200,template:{metadata:{labels:{"app.kubernetes.io/part-of":"fullsend-kind-poc","fullsend.ai/wif-probe":"true"}},spec:{serviceAccountName:$sa,automountServiceAccountToken:false,restartPolicy:"Never",securityContext:{runAsNonRoot:true,runAsUser:10001,runAsGroup:10001,seccompProfile:{type:"RuntimeDefault"}},containers:[{name:"probe",image:$image,imagePullPolicy:"IfNotPresent",command:["bash","/wif/probe.sh"],env:[{name:"WIF_PROBE_MODE",value:$mode},{name:"ANTHROPIC_VERTEX_PROJECT_ID",value:$vertex_project},{name:"GOOGLE_APPLICATION_CREDENTIALS",value:"/wif/credential.json"}],volumeMounts:[{name:"wif-config",mountPath:"/wif",readOnly:true},{name:"wif-token",mountPath:"/var/run/fullsend/wif",readOnly:true},{name:"tmp",mountPath:"/tmp"}],resources:{requests:{cpu:"10m",memory:"32Mi"},limits:{cpu:"500m",memory:"256Mi"}},securityContext:{allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]}}}],volumes:[{name:"wif-config",configMap:{name:"fullsend-wif-probe",defaultMode:292}},{name:"wif-token",projected:{sources:[{serviceAccountToken:{path:"token",audience:$audience,expirationSeconds:600}}]}},{name:"tmp",emptyDir:{}}]}}}}' \
    > "$destination"
  jq '.spec.activeDeadlineSeconds = 2100' "$destination" > "$destination.tmp"
  mv "$destination.tmp" "$destination"
  k create -f "$destination" >/dev/null
}

wait_job() {
  local name="$1" deadline=$((SECONDS + 2200)) job_json result=''
  while (( SECONDS < deadline )); do
    job_json="$(k -n fullsend-runner get "job/$name" -o json)" || break
    if jq -e '(.status.conditions // []) | any(.[]; .type == "Complete" and .status == "True")' <<<"$job_json" >/dev/null; then
      result=complete
      break
    elif jq -e '(.status.conditions // []) | any(.[]; .type == "Failed" and .status == "True")' <<<"$job_json" >/dev/null; then
      result=failed
      break
    fi
    sleep 10
  done
  [[ "$result" == complete ]] || return 1
}

denied_manifest="$LOCAL_DIR/gcp/$denied_job.json"
authorized_manifest="$LOCAL_DIR/gcp/$authorized_job.json"
status '🧪' 'Starting two checks: an ungranted identity should be rejected, and the inference identity should work'
render_job "$denied_job" runner expect-denied "$denied_manifest"
render_job "$authorized_job" runner-inference authorize-and-rotate "$authorized_manifest"

status '⏳' "Proving Google rejects the ungranted runner ServiceAccount (Job $denied_job)"
if ! wait_job "$denied_job"; then
  k -n fullsend-runner logs "job/$denied_job" -c probe > "$STATE_DIR/wif-probe-$denied_job.log" 2>&1 || true
  fail "Wrong-identity check failed; redacted probe output: $STATE_DIR/wif-probe-$denied_job.log"
fi
denied_log="$STATE_DIR/wif-probe-$denied_job.log"
k -n fullsend-runner logs "job/$denied_job" -c probe > "$denied_log"
rg -q '^Untrusted ServiceAccount was rejected by Google STS\.$' "$denied_log" \
  || fail "Wrong-identity check did not record the expected denial: $denied_log"

status '✅' 'The ungranted ServiceAccount was rejected by Google STS, as expected'
status '⏳' "Proving runner-inference can call Vertex before and after Kubernetes rotates its token (Job $authorized_job)"
if ! wait_job "$authorized_job"; then
  k -n fullsend-runner logs "job/$authorized_job" -c probe > "$STATE_DIR/wif-probe-$authorized_job.log" 2>&1 || true
  fail "Authorized WIF check failed; redacted probe output: $STATE_DIR/wif-probe-$authorized_job.log"
fi
authorized_log="$STATE_DIR/wif-probe-$authorized_job.log"
k -n fullsend-runner logs "job/$authorized_job" -c probe > "$authorized_log"
rg -q '^WIF inference succeeded before projected-token rotation\.$' "$authorized_log" \
  || fail "Authorized probe missed its initial inference: $authorized_log"
rg -q '^WIF inference succeeded after projected-token rotation\.$' "$authorized_log" \
  || fail "Authorized probe missed its post-rotation inference: $authorized_log"

status '📝' 'Both checks passed; recording a summary without saving tokens or inference credentials'
report="$STATE_DIR/milestone-6-wif-$(date -u +%Y%m%dT%H%M%SZ).md"
cat > "$report" <<EOF
# Milestone 6 WIF probe

- Wrong Kubernetes ServiceAccount: rejected by Google STS.
- Authorized inference ServiceAccount: Vertex inference succeeded before and after projected-token rotation.
- Assertions, access tokens, and inference service-account credentials were not written to this report.
- Probe Job logs: \`$denied_log\`, \`$authorized_log\`.
EOF
status '🧹' 'Removing the temporary probe Jobs and credential ConfigMap'
k -n fullsend-runner delete job "$denied_job" "$authorized_job" --wait=true >/dev/null
k -n fullsend-runner delete configmap "$configmap_name" --ignore-not-found >/dev/null
status '✅' "Expected denial and successful inference across token rotation confirmed. Summary: $report"
