#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
for tool in kind kubectl podman go git openssl sha256sum jq; do
  command -v "$tool" >/dev/null || fail "Missing $tool; install it before deploying"
done
[[ "$KIND_EXPERIMENTAL_PROVIDER" == podman ]] || fail "This PoC currently supports the tested podman provider only"
podman info >/dev/null || fail "Podman engine unavailable"
if [[ "$PROFILE" == live ]]; then
  for file in "$LOCAL_DIR/live/settings.json" "$LOCAL_DIR/live/repos.yaml" "$LOCAL_DIR/live/repo/.fullsend/config.yaml" "$LOCAL_DIR/live/jira-token" "$LOCAL_DIR/live/jira-email"; do
    [[ -s "$file" ]] || fail "Missing live input: $file"
  done
  jira_url="$(jq -er '.jira_base_url' "$LOCAL_DIR/live/settings.json")"
  project="$(jq -er '.project' "$LOCAL_DIR/live/settings.json")"
  label="$(jq -er '.label' "$LOCAL_DIR/live/settings.json")"
  repo="$(jq -er '.repo' "$LOCAL_DIR/live/settings.json")"
  (cd "$REPO_ROOT" && go run ./cmd/fullsend k8s-poc prepare-live \
    --repos-manifest "$LOCAL_DIR/live/repos.yaml" --repo-dir "$LOCAL_DIR/live/repo" \
    --repo "$repo" --jira-url "$jira_url" --jira-project "$project" \
    --jira-label "$label" --output "$LOCAL_DIR/live/live.json") || fail 'Live configuration preflight failed'
fi
status '✅' 'Prerequisites available'
