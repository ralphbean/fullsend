#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"

usage() {
  fail "Usage: $0 --repository-id ID --repository OWNER/REPO [--role coder]"
}

repository_id=''
repository=''
role='coder'
while [[ $# -gt 0 ]]; do
  case "$1" in
    --repository-id) [[ $# -ge 2 ]] || usage; repository_id="$2"; shift 2 ;;
    --repository) [[ $# -ge 2 ]] || usage; repository="$2"; shift 2 ;;
    --role) [[ $# -ge 2 ]] || usage; role="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ "$repository_id" =~ ^[1-9][0-9]*$ && "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ && "$role" == coder ]] || usage
require_owned
[[ -s "$STATE_DIR/mint-image" ]] || fail 'Deploy the private mint first'
command -v jq >/dev/null || fail 'Missing jq'
k -n fullsend-runner get serviceaccount runner >/dev/null || fail 'Runner namespace is missing'
k -n fullsend-mint rollout status deployment/fullsend-mint --timeout=30s >/dev/null || fail 'Private mint is not ready'

sa_name="fs-repo-$repository_id-$role"
if k -n fullsend-runner get "serviceaccount/$sa_name" >/dev/null 2>&1; then
  current="$(k -n fullsend-runner get "serviceaccount/$sa_name" -o json)"
  jq -e --arg repo "$repository_id" --arg role "$role" \
    '.automountServiceAccountToken == false and .metadata.labels["fullsend.ai/repository-id"] == $repo and .metadata.labels["fullsend.ai/role"] == $role and .metadata.annotations["fullsend.ai/mint-identity"] == "true"' \
    <<<"$current" >/dev/null || fail "Existing service account $sa_name conflicts with the approved repository/role identity"
else
  k -n fullsend-runner create serviceaccount "$sa_name" --dry-run=client -o json \
    | jq --arg repo "$repository_id" --arg role "$role" \
      '.automountServiceAccountToken=false | .metadata.labels={"app.kubernetes.io/part-of":"fullsend-kind-poc","fullsend.ai/repository-id":$repo,"fullsend.ai/role":$role} | .metadata.annotations={"fullsend.ai/mint-identity":"true"}' \
    | k create -f - >/dev/null
fi

build_gateway_runner
suffix="$(uuidgen | tr '[:upper:]' '[:lower:]' | cut -c1-8)"
job_name="fs-mint-$suffix"
script_file="$LOCAL_DIR/mint/probe.sh"
mkdir -p "$LOCAL_DIR/mint"
cat > "$script_file" <<'PROBE'
set -euo pipefail
token_file=/var/run/secrets/fullsend/mint/token
initial_hash="$(sha256sum "$token_file" | cut -d' ' -f1)"
mint_and_check() {
  request="$(jq -cn --argjson repository_id "$FULLSEND_REPOSITORY_ID" --arg role "$FULLSEND_ROLE" --arg level write '{repository_id:$repository_id,role:$role,level:$level}')"
  curl --cacert /var/run/secrets/fullsend/mint/ca.crt --fail --silent --show-error --config <(printf 'header = "Authorization: Bearer %s"\n' "$(cat "$token_file")") \
    --header 'Content-Type: application/json' --data "$request" \
    https://fullsend-mint.fullsend-mint.svc/v1/kubernetes/token > /tmp/mint-response.json
  github_token="$(jq -er '.token | select(type == "string" and length > 0)' /tmp/mint-response.json)"
  expected_permissions='{"contents":"write","packages":"read","pull_requests":"write","issues":"write","checks":"read","metadata":"read"}'
  if ! jq -e --argjson id "$FULLSEND_REPOSITORY_ID" --argjson expected "$expected_permissions" \
    '.repository_ids == [$id] and .permissions == $expected' /tmp/mint-response.json >/dev/null; then
    printf '%s ' 'Mint did not confirm the expected GitHub repository and role scope:' >&2
    jq -c '{repository_ids, permissions}' /tmp/mint-response.json >&2
    return 1
  fi
  unset github_token
  rm -f /tmp/mint-response.json
}
mint_and_check
deadline=$((SECONDS + 900))
while (( SECONDS < deadline )); do
  current_hash="$(sha256sum "$token_file" | cut -d' ' -f1)"
  if [[ "$current_hash" != "$initial_hash" ]]; then
    mint_and_check
    printf '%s\n' 'Projected Kubernetes token rotated; second repository-scoped GitHub exchange succeeded.'
    exit 0
  fi
  sleep 10
done
printf '%s\n' 'Projected Kubernetes token did not rotate within 15 minutes.' >&2
exit 1
PROBE

pod_image="$(cat "$STATE_DIR/gateway-runner-image")"
jq -n --arg name "$job_name" --arg image "$pod_image" --arg sa "$sa_name" --arg id "$repository_id" --arg role "$role" \
  --rawfile script "$script_file" \
  '{apiVersion:"batch/v1",kind:"Job",metadata:{name:$name,namespace:"fullsend-runner",labels:{"app.kubernetes.io/part-of":"fullsend-kind-poc","fullsend.ai/mint-probe":"true"}},spec:{backoffLimit:0,activeDeadlineSeconds:1200,template:{metadata:{labels:{"app.kubernetes.io/part-of":"fullsend-kind-poc"}},spec:{serviceAccountName:$sa,automountServiceAccountToken:false,restartPolicy:"Never",securityContext:{runAsNonRoot:true,runAsUser:10001,runAsGroup:10001,seccompProfile:{type:"RuntimeDefault"}},containers:[{name:"probe",image:$image,imagePullPolicy:"IfNotPresent",command:["bash","-euc",$script],env:[{name:"FULLSEND_MINT_URL",value:"https://fullsend-mint.fullsend-mint.svc"},{name:"KUBERNETES_MINT_CA_FILE",value:"/var/run/secrets/fullsend/mint/ca.crt"},{name:"FULLSEND_REPOSITORY_ID",value:$id},{name:"FULLSEND_ROLE",value:$role},{name:"KUBERNETES_MINT_TOKEN_FILE",value:"/var/run/secrets/fullsend/mint/token"}],volumeMounts:[{name:"mint-token",mountPath:"/var/run/secrets/fullsend/mint",readOnly:true},{name:"tmp",mountPath:"/tmp"}],resources:{requests:{cpu:"10m",memory:"32Mi"},limits:{cpu:"200m",memory:"128Mi"}},securityContext:{allowPrivilegeEscalation:false,readOnlyRootFilesystem:true,capabilities:{drop:["ALL"]}}}],volumes:[{name:"mint-token",projected:{sources:[{serviceAccountToken:{path:"token",audience:"fullsend-k8s-mint",expirationSeconds:600}},{configMap:{name:"fullsend-mint-ca",items:[{key:"ca.crt",path:"ca.crt"}]}}]}},{name:"tmp",emptyDir:{}}]}}}}' \
  | k create -f - >/dev/null
status '⏳' "Waiting for repository-scoped mint and projected-token rotation (Job $job_name)"
job_result=''
deadline=$((SECONDS + 1200))
while [[ -z "$job_result" ]] && (( SECONDS < deadline )); do
  job_json="$(k -n fullsend-runner get "job/$job_name" -o json)" || break
  if jq -e '(.status.conditions // []) | any(.[]; .type == "Complete" and .status == "True")' <<<"$job_json" >/dev/null; then
    job_result=complete
  elif jq -e '(.status.conditions // []) | any(.[]; .type == "Failed" and .status == "True")' <<<"$job_json" >/dev/null; then
    job_result=failed
  else
    sleep 10
  fi
done
if [[ "$job_result" != complete ]]; then
  k -n fullsend-runner logs "job/$job_name" > "$STATE_DIR/mint-probe-$job_name.log" 2>&1 || true
  k -n fullsend-runner delete "job/$job_name" --wait=true >/dev/null 2>&1 || true
  fail "Mint probe $job_result; redacted runner output: $STATE_DIR/mint-probe-$job_name.log"
fi
k -n fullsend-runner logs "job/$job_name" > "$STATE_DIR/mint-probe-$job_name.log"
k -n fullsend-runner delete "job/$job_name" --wait=true >/dev/null
status '✅' "Scoped GitHub App token worked before and after projected token rotation; result: $STATE_DIR/mint-probe-$job_name.log"
