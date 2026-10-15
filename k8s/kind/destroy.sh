#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
require_owned

ensure_gateway_cleanup_secrets() {
  local bundle_secret auth_secret gateway_jobs gateway_name local_gateway_name active_gateway_file
  local bundle_dir metadata_file client_secret_file bundle_stage

  bundle_secret="$(k -n fullsend-runner get secret fullsend-gateway-bundle --ignore-not-found -o name)" \
    || fail 'Could not inspect the gateway bundle Secret; refusing to destroy the cluster'
  auth_secret="$(k -n fullsend-runner get secret fullsend-gateway-auth --ignore-not-found -o name)" \
    || fail 'Could not inspect the gateway auth Secret; refusing to destroy the cluster'

  # A fixture cluster that never used the remote gateway has nothing to
  # reconcile. Preserve that path; otherwise require both cleanup Secrets.
  gateway_jobs="$(k -n fullsend-runner get jobs \
    -l 'fullsend.ai/gateway-job' --ignore-not-found -o name)" \
    || fail 'Could not inspect gateway Jobs; refusing to destroy the cluster'
  if [[ -z "$bundle_secret" && -z "$auth_secret" && -z "$gateway_jobs" \
    && ! -s "$STATE_DIR/gateway-runner-image" ]]; then
    return 0
  fi

  [[ -n "$bundle_secret" && -n "$auth_secret" ]] && return 0

  active_gateway_file="$LOCAL_DIR/gateway/openshell/active_gateway"
  bundle_dir="$LOCAL_DIR/gateway/openshell"
  gateway_name="${HYPERSHELL_GATEWAY_NAME:-}"
  if [[ -f "$active_gateway_file" && ! -L "$active_gateway_file" ]]; then
    local_gateway_name=''
    IFS= read -r local_gateway_name < "$active_gateway_file" || true
    [[ -z "$local_gateway_name" ]] || gateway_name="$local_gateway_name"
  fi
  if [[ -z "$gateway_name" || ! "$gateway_name" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]*$ ]]; then
    status '⚠️' 'Gateway cleanup Secrets are missing and the active OpenShell gateway name is unavailable.'
    fail "Provide k8s/.local/gateway/openshell/active_gateway and the mode-600 gateway client-secret file, or install both Secrets with 07-gateway-probe.sh; refusing to destroy the cluster"
  fi

  metadata_file="$bundle_dir/gateways/$gateway_name/metadata.json"
  client_secret_file="$LOCAL_DIR/gateway/$gateway_name-fullsend-poc-client-secret"
  if [[ -z "$bundle_secret" ]]; then
    if [[ ! -f "$metadata_file" || -L "$metadata_file" ]] \
      || ! jq -e '.auth_mode == "oidc"' "$metadata_file" >/dev/null; then
      status '⚠️' "Gateway bundle Secret is missing and local OIDC registration metadata was not found: $metadata_file"
      fail 'Provide the gateway bundle metadata or run 07-gateway-probe.sh to install the cleanup Secret; refusing to destroy the cluster'
    fi
  fi
  if [[ -z "$auth_secret" ]]; then
    if [[ ! -f "$client_secret_file" || -L "$client_secret_file" || ! -s "$client_secret_file" ]] \
      || [[ "$(stat -c '%a' "$client_secret_file" 2>/dev/null || true)" != 600 ]]; then
      status '⚠️' "Gateway auth Secret is missing and its mode-600 local credential was not found: $client_secret_file"
      fail 'Provide the gateway client-secret file or run 07-gateway-probe.sh to install the cleanup Secret; refusing to destroy the cluster'
    fi
  fi

  if [[ -z "$bundle_secret" ]]; then
    status '🔧' 'Restoring missing fullsend-gateway-bundle Secret from the local OIDC registration'
    bundle_stage="$(mktemp -d "$LOCAL_DIR/gateway-destroy.XXXXXX")"
    if ! mkdir -p "$bundle_stage/bundle/gateways/$gateway_name" \
      || ! cp "$metadata_file" "$bundle_stage/bundle/gateways/$gateway_name/metadata.json" \
      || ! printf '%s\n' "$gateway_name" > "$bundle_stage/bundle/active_gateway" \
      || ! tar -C "$bundle_stage/bundle" -czf "$bundle_stage/bundle.tgz" .; then
      rm -rf "$bundle_stage"
      fail 'Could not stage the local gateway registration; refusing to destroy the cluster'
    fi
    if ! k -n fullsend-runner create secret generic fullsend-gateway-bundle \
      --from-file="bundle.tgz=$bundle_stage/bundle.tgz" --dry-run=client -o yaml \
      | k apply -f - >/dev/null; then
      rm -rf "$bundle_stage"
      fail 'Could not restore fullsend-gateway-bundle; refusing to destroy the cluster'
    fi
    rm -rf "$bundle_stage"
  fi
  if [[ -z "$auth_secret" ]]; then
    status '🔧' 'Restoring missing fullsend-gateway-auth Secret from the local client credential'
    k -n fullsend-runner create secret generic fullsend-gateway-auth \
      --from-file="client-secret=$client_secret_file" --dry-run=client -o yaml \
      | k apply -f - >/dev/null \
      || fail 'Could not restore fullsend-gateway-auth; refusing to destroy the cluster'
  fi

  k -n fullsend-runner get secret fullsend-gateway-bundle fullsend-gateway-auth >/dev/null \
    || fail 'Gateway cleanup Secrets are still unavailable; refusing to destroy the cluster'
}

ensure_gateway_cleanup_secrets

# The cleanup Job runs inside kind and needs the same runner image that probe
# and demo Jobs used. A reset can leave the image on the host but not in the
# node, so always load the resolved image before asking the cluster to use it.
runner_image=''
if [[ -s "$STATE_DIR/gateway-runner-image" ]]; then
  runner_image="$(cat "$STATE_DIR/gateway-runner-image")"
else
  runner_image="$(k -n fullsend-runner get jobs -l 'fullsend.ai/gateway-job' \
    -o jsonpath='{.items[0].spec.template.spec.containers[0].image}')"
fi
if [[ -n "$runner_image" ]]; then
  if [[ ! -s "$STATE_DIR/gateway-runner-image" ]]; then
    printf '%s\n' "$runner_image" > "$STATE_DIR/gateway-runner-image"
  fi
  if podman image exists "$runner_image" >/dev/null 2>&1; then
    status '🔧' "Loading gateway cleanup runner image $runner_image into kind"
    kind load docker-image "$runner_image" --name "$KIND_NAME"
  else
    status '🔧' "Gateway cleanup runner image $runner_image is missing locally; building it from this checkout"
    build_gateway_runner
  fi
fi

"$K8S_ROOT/kind/08-gateway-cleanup.sh" --profile "$PROFILE"
status '🔧' 'Deleting owned kind cluster'
kind delete cluster --name "$KIND_NAME" --kubeconfig "$KUBECONFIG_POC"
rm -f "$STATE_DIR/cluster-owned" "$KUBECONFIG_POC"
status '✅' 'Owned cluster deleted'
