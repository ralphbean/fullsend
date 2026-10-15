#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"
profile_arg "$@"
if cluster_exists; then
  require_owned
  status '✅' 'Owned kind cluster already exists'
else
  status '🔧' 'Creating kind cluster'
  kind create cluster --name "$KIND_NAME" --config "$K8S_ROOT/kind/cluster.yaml" --image "$KIND_NODE_IMAGE" --kubeconfig "$KUBECONFIG_POC"
  printf '%s\n' "$KIND_NAME" > "$STATE_DIR/cluster-owned"
  require_owned
fi
