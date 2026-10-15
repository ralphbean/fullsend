#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
[[ "$PROFILE" == live ]] || fail 'Use --profile live'
require_owned
command -v gcloud >/dev/null || fail 'Missing gcloud'
command -v jq >/dev/null || fail 'Missing jq'

repo_id="$(k -n fullsend-control get configmap fullsend-mint-dispatch -o jsonpath='{.data.repository-id}')"
[[ "$repo_id" =~ ^[1-9][0-9]*$ ]] || fail 'Registered repository mint identity is missing'
audience="$(jq -er '.gcp_audience' "$LOCAL_DIR/live/wif.json")"
vertex_project="$(jq -er '.vertex_project' "$LOCAL_DIR/live/wif.json")"
credential_file="$LOCAL_DIR/gcp/fullsend-credential.json"
[[ -s "$credential_file" ]] || fail 'Existing WIF credential configuration is missing'
project_number="$(sed -E 's#^//iam.googleapis.com/projects/([0-9]+)/.*#\1#' <<<"$audience")"
pool="$(sed -E 's#^//iam.googleapis.com/projects/[0-9]+/locations/global/workloadIdentityPools/([^/]+)/providers/[^/]+$#\1#' <<<"$audience")"
provider="$(sed -E 's#^//iam.googleapis.com/projects/[0-9]+/locations/global/workloadIdentityPools/[^/]+/providers/([^/]+)$#\1#' <<<"$audience")"
[[ "$project_number" =~ ^[1-9][0-9]*$ && "$pool" =~ ^[a-z][a-z0-9-]*$ && "$provider" =~ ^[a-z][a-z0-9-]*$ ]] || fail 'WIF audience is malformed'
vertex_service_account="$(jq -er '.service_account_impersonation_url | capture("serviceAccounts/(?<email>[^/:]+@[^/:]+):generateAccessToken").email' "$credential_file")"
[[ "$vertex_service_account" =~ ^[a-z][a-z0-9-]*@[a-z][a-z0-9-]*\.iam\.gserviceaccount\.com$ ]] || fail 'WIF inference identity is malformed'

service_account="fs-repo-${repo_id}-triage"
subject="system:serviceaccount:fullsend-runner:${service_account}"
principal="principal://iam.googleapis.com/projects/${project_number}/locations/global/workloadIdentityPools/${pool}/subject/${subject}"
log_file="$STATE_DIR/wif-repo-triage-setup.log"
state_file="$LOCAL_DIR/live/wif-repo-triage-state.json"
umask 077

condition="$(gcloud iam workload-identity-pools providers describe "$provider" \
  --project="$vertex_project" --location=global --workload-identity-pool="$pool" \
  --format='value(attributeCondition)' 2>"$log_file")" || fail "Could not inspect existing WIF provider; details are in $log_file"
if [[ "$condition" != *"$subject"* ]]; then
  [[ "$condition" == *"fullsend-runner"* && "$condition" == *"runner-inference"* ]] || fail 'Existing WIF provider condition is not the expected inference-only rule; refusing to replace it'
  updated_condition="(${condition}) || (assertion['kubernetes.io']['namespace'] == 'fullsend-runner' && assertion.sub == '${subject}')"
  gcloud iam workload-identity-pools providers update-oidc "$provider" \
    --project="$vertex_project" --location=global --workload-identity-pool="$pool" \
    --attribute-condition="$updated_condition" --quiet >>"$log_file" 2>&1 \
    || fail "Could not add the exact repository/triage subject to the existing WIF provider; details are in $log_file"
  condition="$updated_condition"
fi
gcloud iam service-accounts add-iam-policy-binding "$vertex_service_account" \
  --project="$vertex_project" --role=roles/iam.workloadIdentityUser --member="$principal" \
  --quiet >>"$log_file" 2>&1 || fail "Could not grant the repository/triage identity access to the existing inference service account; details are in $log_file"
jq -n --arg project "$vertex_project" --arg pool "$pool" --arg provider "$provider" \
  --arg service_account "$service_account" --arg principal "$principal" --arg condition "$condition" \
  '{project:$project,pool:$pool,provider:$provider,service_account:$service_account,principal:$principal,attribute_condition:$condition}' \
  > "$state_file"
chmod 600 "$state_file"
status '✅' 'Existing WIF now accepts only the registered repository triage ServiceAccount in addition to runner-inference'
