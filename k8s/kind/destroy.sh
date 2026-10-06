#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
require_owned
"$K8S_ROOT/kind/08-gateway-cleanup.sh" --profile "$PROFILE"
status '🔧' 'Deleting owned kind cluster'
kind delete cluster --name "$KIND_NAME" --kubeconfig "$KUBECONFIG_POC"
rm -f "$STATE_DIR/cluster-owned" "$KUBECONFIG_POC"
status '✅' 'Owned cluster deleted'
