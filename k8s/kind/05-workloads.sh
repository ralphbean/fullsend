#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"
profile_arg "$@"
[[ "$PROFILE" == fixture ]] || fail 'Use 05-live-workloads.sh for the live profile'
require_owned
for cron in jira-poller jira-dispatcher; do
  if k -n fullsend-control get "cronjob/$cron" >/dev/null 2>&1; then
    k -n fullsend-control patch "cronjob/$cron" --type merge -p '{"spec":{"suspend":true}}' >/dev/null
  fi
done
[[ -f "$STATE_DIR/image-tag" ]] || fail 'Build images first'
tag="$(cat "$STATE_DIR/image-tag")"
mkdir -p "$LOCAL_DIR/migration" "$LOCAL_DIR/workload"
cp "$K8S_ROOT/fixtures/source.json" "$LOCAL_DIR/workload/fixture.json"
cp "$K8S_ROOT/fixtures/selection.json" "$LOCAL_DIR/workload/selection.json"
cat > "$LOCAL_DIR/migration/kustomization.yaml" <<EOF
resources:
  - ../../manifests/base/migration
images:
  - name: localhost/fullsend-poc
    newTag: $tag
EOF
cat > "$LOCAL_DIR/workload/kustomization.yaml" <<EOF
resources:
  - ../../manifests/overlays/kind-fixture
images:
  - name: localhost/fullsend-poc
    newTag: $tag
configMapGenerator:
  - name: fixture-input
    namespace: fullsend-control
    files:
      - fixture.json
      - selection.json
EOF
k -n fullsend-control delete job migrate --ignore-not-found --wait=true >/dev/null
k apply -k "$LOCAL_DIR/migration" >/dev/null
k -n fullsend-control wait --for=condition=complete job/migrate --timeout=120s >/dev/null || fail 'Migration failed'
k apply -k "$LOCAL_DIR/workload" >/dev/null
k -n fullsend-runner delete rolebinding/stub-job-launcher role/stub-job-launcher --ignore-not-found --wait=true >/dev/null
k -n fullsend-control patch cronjob fixture-poller fixture-dispatcher --type merge -p '{"spec":{"suspend":false}}' >/dev/null
status '✅' 'Poller and dispatcher ready'
