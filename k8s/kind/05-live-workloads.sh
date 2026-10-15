#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"
profile_arg "$@"
[[ "$PROFILE" == live ]] || fail 'Use --profile live'
require_owned
for file in "$LOCAL_DIR/live/settings.json" "$LOCAL_DIR/live/repos.yaml" "$LOCAL_DIR/live/repo/.fullsend/config.yaml" "$LOCAL_DIR/live/jira-token" "$LOCAL_DIR/live/jira-email"; do
  [[ -s "$file" ]] || fail "Missing live input: $file"
done
[[ -f "$STATE_DIR/image-tag" ]] || fail 'Build images first'
fullsend_harnesses="$(jq -er 'if has("fullsend_harnesses") then (.fullsend_harnesses | if type == "array" then join(",") else error("fullsend_harnesses must be an array") end) else "" end' "$LOCAL_DIR/live/settings.json")"
fullsend_execution=0
if [[ -n "$fullsend_harnesses" ]]; then
  [[ "$fullsend_harnesses" == triage ]] || fail 'Only the triage harness is currently allowed for real execution'
  fullsend_execution=1
  [[ -s "$LOCAL_DIR/live/wif.json" ]] || fail 'Missing live workload identity settings for real Fullsend execution'
  [[ -s "$STATE_DIR/gateway-runner-image" ]] || fail 'Build the gateway runner image first'
  k -n fullsend-runner get serviceaccount runner-inference >/dev/null 2>&1 || fail 'Inference ServiceAccount is missing'
  k -n fullsend-runner get secret fullsend-gateway-bundle >/dev/null 2>&1 || fail 'Install the gateway bundle first'
  k -n fullsend-runner get secret fullsend-gateway-auth >/dev/null 2>&1 || fail 'Gateway service-account Secret is missing'
  k -n fullsend-runner get configmap fullsend-wif-fullsend >/dev/null 2>&1 || fail 'Run the WIF Fullsend setup first'
  k -n fullsend-control get configmap fullsend-mint-dispatch >/dev/null 2>&1 || fail 'Deploy the private mint and its repository dispatch identity first'
  k -n fullsend-runner get configmap fullsend-mint-ca >/dev/null 2>&1 || fail 'Private mint CA is missing; redeploy the private mint first'
fi

bundle=''
normalized_auth_dir=''
declare -A prior_fixture_suspend=()
restore_live_setup() {
  local exit_status=$?
  if (( exit_status != 0 )); then
    for cron in fixture-poller fixture-dispatcher; do
      if [[ -n "${prior_fixture_suspend[$cron]+saved}" ]]; then
        k -n fullsend-control patch "cronjob/$cron" --type merge \
          -p "{\"spec\":{\"suspend\":${prior_fixture_suspend[$cron]}}}" >/dev/null 2>&1 || true
      fi
    done
  fi
  if [[ -n "$bundle" ]]; then rm -f "$bundle"; fi
  if [[ -n "$normalized_auth_dir" ]]; then
    rm -f "$normalized_auth_dir/token" "$normalized_auth_dir/email" || true
    rmdir "$normalized_auth_dir" 2>/dev/null || true
  fi
}
for cron in fixture-poller fixture-dispatcher; do
  if k -n fullsend-control get "cronjob/$cron" >/dev/null 2>&1; then
    prior_fixture_suspend[$cron]="$(k -n fullsend-control get "cronjob/$cron" -o jsonpath='{.spec.suspend}')"
    k -n fullsend-control patch "cronjob/$cron" --type merge -p '{"spec":{"suspend":true}}' >/dev/null
  fi
done
trap restore_live_setup EXIT
jira_url="$(jq -er '.jira_base_url' "$LOCAL_DIR/live/settings.json")"
project="$(jq -er '.project' "$LOCAL_DIR/live/settings.json")"
label="$(jq -er '.label' "$LOCAL_DIR/live/settings.json")"
repo="$(jq -er '.repo' "$LOCAL_DIR/live/settings.json")"
bundle_name=''
repo_revision=''
gcp_audience=''
vertex_project=''
normalized_auth_dir="$(mktemp -d "$LOCAL_DIR/live/jira-auth.XXXXXX")"
python3 - "$LOCAL_DIR/live/jira-token" "$LOCAL_DIR/live/jira-email" \
  "$normalized_auth_dir/token" "$normalized_auth_dir/email" <<'PY'
import os
import pathlib
import sys

for source, destination in ((sys.argv[1], sys.argv[3]), (sys.argv[2], sys.argv[4])):
    value = pathlib.Path(source).read_bytes().strip()
    if not value:
        raise SystemExit("Jira credentials must not be empty")
    path = pathlib.Path(destination)
    path.write_bytes(value)
    os.chmod(path, 0o600)
PY
if [[ "$fullsend_execution" == 1 ]]; then
  repo_revision="$(git -C "$LOCAL_DIR/live/repo" rev-parse HEAD)"
  bundle="$(mktemp "$LOCAL_DIR/live/repo-bundle.XXXXXX.tgz")"
  tar -C "$LOCAL_DIR/live/repo" --exclude=.git -czf "$bundle" .
  bundle_digest="$(sha256sum "$bundle" | cut -d' ' -f1)"
  bundle_size="$(stat -c '%s' "$bundle")"
  (( bundle_size < 900000 )) || fail 'Pinned repository bundle exceeds the ConfigMap size budget'
  bundle_name="fullsend-live-repo-${bundle_digest:0:16}"
  gcp_audience="$(jq -er '.gcp_audience' "$LOCAL_DIR/live/wif.json")"
  vertex_project="$(jq -er '.vertex_project' "$LOCAL_DIR/live/wif.json")"
fi
(
  cd "$REPO_ROOT"
  K8S_POC_FULLSEND_HARNESSES="$fullsend_harnesses" \
  K8S_POC_REPO_BUNDLE="$bundle_name" K8S_POC_REPO_REVISION="$repo_revision" \
  K8S_POC_GCP_AUDIENCE="$gcp_audience" K8S_POC_VERTEX_PROJECT="$vertex_project" \
  go run ./cmd/fullsend k8s-poc prepare-live \
    --repos-manifest "$LOCAL_DIR/live/repos.yaml" --repo-dir "$LOCAL_DIR/live/repo" \
    --repo "$repo" --jira-url "$jira_url" \
    --jira-project "$project" --jira-label "$label" \
    --output "$LOCAL_DIR/live/live.json"
)
if [[ "$fullsend_execution" == 1 ]]; then
  "$K8S_ROOT/kind/17-wif-repo-triage.sh" --profile live
  k -n fullsend-runner create configmap "$bundle_name" \
    --from-file="bundle.tgz=$bundle" --dry-run=client -o json | jq '.immutable = true' | \
    k apply --server-side --field-manager=fullsend-k8s-poc -f - >/dev/null
  k -n fullsend-runner create secret generic jira-live-auth \
    --from-file="token=$normalized_auth_dir/token" --from-file="email=$normalized_auth_dir/email" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
  k -n fullsend-runner create configmap fullsend-live-runtime \
    --from-file=run-live-fullsend.sh="$K8S_ROOT/gcp/run-live-fullsend.sh" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
fi
k -n fullsend-control create secret generic jira-live-auth \
  --from-file="token=$normalized_auth_dir/token" --from-file="email=$normalized_auth_dir/email" \
  --dry-run=client -o yaml | k apply -f - >/dev/null
k -n fullsend-control create configmap jira-live-selection \
  --from-file="live.json=$LOCAL_DIR/live/live.json" --dry-run=client -o yaml | k apply -f - >/dev/null
tag="$(cat "$STATE_DIR/image-tag")"
gateway_runner_image=''
if [[ "$fullsend_execution" == 1 ]]; then gateway_runner_image="$(cat "$STATE_DIR/gateway-runner-image")"; fi
mkdir -p "$LOCAL_DIR/live/workload" "$LOCAL_DIR/live/migration"
cat > "$LOCAL_DIR/live/migration/kustomization.yaml" <<EOF
resources:
  - ../../../manifests/base/migration
images:
  - name: localhost/fullsend-poc
    newTag: $tag
EOF
cat > "$LOCAL_DIR/live/workload/kustomization.yaml" <<EOF
resources:
  - ../../../manifests/overlays/kind-live
images:
  - name: localhost/fullsend-poc
    newTag: $tag
EOF
k -n fullsend-control delete job migrate --ignore-not-found --wait=true >/dev/null
k apply -k "$LOCAL_DIR/live/migration" >/dev/null
k -n fullsend-control wait --for=condition=complete job/migrate --timeout=120s >/dev/null || fail 'Migration failed'
k apply -k "$LOCAL_DIR/live/workload" >/dev/null
if [[ "$fullsend_execution" == 1 ]]; then
  k -n fullsend-control set env cronjob/jira-dispatcher "GATEWAY_RUNNER_IMAGE=$gateway_runner_image" >/dev/null
else
  k -n fullsend-control set env cronjob/jira-dispatcher GATEWAY_RUNNER_IMAGE- >/dev/null
fi
k -n fullsend-runner delete rolebinding/stub-job-launcher role/stub-job-launcher --ignore-not-found --wait=true >/dev/null
k -n fullsend-control patch cronjob jira-poller jira-dispatcher --type merge -p '{"spec":{"suspend":false}}' >/dev/null
status '✅' 'Live Jira poller and dispatcher ready'
