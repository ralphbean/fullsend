#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
[[ "$PROFILE" == live ]] || fail 'Use --profile live'
require_owned
for cron in fixture-poller fixture-dispatcher; do
  if k -n fullsend-control get "cronjob/$cron" >/dev/null 2>&1; then
    k -n fullsend-control patch "cronjob/$cron" --type merge -p '{"spec":{"suspend":true}}' >/dev/null
  fi
done
for file in "$LOCAL_DIR/live/settings.json" "$LOCAL_DIR/live/repos.yaml" "$LOCAL_DIR/live/repo/.fullsend/config.yaml" "$LOCAL_DIR/live/jira-token" "$LOCAL_DIR/live/jira-email"; do
  [[ -s "$file" ]] || fail "Missing live input: $file"
done
[[ -f "$STATE_DIR/image-tag" ]] || fail 'Build images first'
jira_url="$(jq -er '.jira_base_url' "$LOCAL_DIR/live/settings.json")"
project="$(jq -er '.project' "$LOCAL_DIR/live/settings.json")"
label="$(jq -er '.label' "$LOCAL_DIR/live/settings.json")"
repo="$(jq -er '.repo' "$LOCAL_DIR/live/settings.json")"
(
  cd "$REPO_ROOT"
  go run ./cmd/fullsend k8s-poc prepare-live \
    --repos-manifest "$LOCAL_DIR/live/repos.yaml" --repo-dir "$LOCAL_DIR/live/repo" \
    --repo "$repo" --jira-url "$jira_url" \
    --jira-project "$project" --jira-label "$label" \
    --output "$LOCAL_DIR/live/live.json"
)
k -n fullsend-control create secret generic jira-live-auth \
  --from-file="token=$LOCAL_DIR/live/jira-token" --from-file="email=$LOCAL_DIR/live/jira-email" \
  --dry-run=client -o yaml | k apply -f - >/dev/null
k -n fullsend-control create configmap jira-live-selection \
  --from-file="live.json=$LOCAL_DIR/live/live.json" --dry-run=client -o yaml | k apply -f - >/dev/null
tag="$(cat "$STATE_DIR/image-tag")"
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
k -n fullsend-runner delete rolebinding/stub-job-launcher role/stub-job-launcher --ignore-not-found --wait=true >/dev/null
k -n fullsend-control patch cronjob jira-poller jira-dispatcher --type merge -p '{"spec":{"suspend":false}}' >/dev/null
status '✅' 'Live Jira poller and dispatcher ready'
