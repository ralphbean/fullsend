#!/usr/bin/env bash
set -euo pipefail
K8S_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$K8S_ROOT/.." && pwd)"
export REPO_ROOT
LOCAL_DIR="$K8S_ROOT/.local"
STATE_DIR="$K8S_ROOT/.state"
KUBECONFIG_POC="$LOCAL_DIR/kubeconfig"
KIND_NAME=fullsend-poc
KIND_CONTEXT=kind-fullsend-poc
export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"
source "$K8S_ROOT/kind/versions.env"
mkdir -p "$LOCAL_DIR" "$STATE_DIR"

status() {
  local icon="$1"; shift
  if [[ "${NO_EMOJI:-0}" == 1 ]]; then icon=""; fi
  printf '%s %s\n' "$icon" "$*" >&2
}
fail() { status '❌' "$*"; exit 1; }
k() { kubectl --kubeconfig "$KUBECONFIG_POC" --context "$KIND_CONTEXT" "$@"; }
require_owned() {
  [[ -f "$STATE_DIR/cluster-owned" ]] || fail "Cluster ownership marker missing; refusing to operate on $KIND_NAME"
  [[ -f "$KUBECONFIG_POC" ]] || fail "Dedicated kubeconfig missing"
  k cluster-info >/dev/null || fail "Dedicated PoC context is unavailable"
}
cluster_exists() {
  podman ps -a --filter "name=^${KIND_NAME}-control-plane$" --format '{{.Names}}' | rg -qx "${KIND_NAME}-control-plane"
}
profile_arg() {
  PROFILE=fixture
  if [[ "$#" -gt 0 ]]; then
    [[ "$#" -eq 2 && "$1" == --profile ]] || fail "Usage: $0 [--profile fixture]"
    PROFILE="$2"
  fi
  [[ "$PROFILE" == fixture || "$PROFILE" == live ]] || fail "Supported profiles: fixture, live"
}

build_gateway_runner() {
  local revision content_hash tag base_runner_image
  revision="$(git -C "$REPO_ROOT" rev-parse HEAD)"
  content_hash="$(cd "$REPO_ROOT" && { git diff HEAD --binary; git ls-files --others --exclude-standard | sort | xargs -r sha256sum; } | sha256sum | cut -c1-16)"
  tag="${revision:0:12}-${content_hash}"
  GATEWAY_RUNNER_IMAGE="localhost/fullsend-poc-runner:$tag"
  base_runner_image="localhost/fullsend-poc-runner-base:$tag"
  status '🔧' "Building UBI10 runner image from checkout ($tag)"
  (cd "$REPO_ROOT" && podman build \
    --build-arg "FULLSEND_COMMIT=$revision" \
    -f images/runner/Containerfile -t "$base_runner_image" .)
  podman build --build-arg "BASE_RUNNER_IMAGE=$base_runner_image" \
    -f "$K8S_ROOT/runner/Containerfile" -t "$GATEWAY_RUNNER_IMAGE" "$K8S_ROOT/runner"
  [[ "$(podman run --rm --entrypoint openshell "$GATEWAY_RUNNER_IMAGE" --version)" == 'openshell 0.1.2-rhaiv.0' ]] \
    || fail 'Runner OpenShell version does not match the provisioned gateway'
  [[ "$(podman run --rm --user 10001:10001 --entrypoint id "$GATEWAY_RUNNER_IMAGE" -un)" == runner ]] \
    || fail 'Runner UID 10001 must resolve to a user for OpenShell SSH transfers'
  kind load docker-image "$GATEWAY_RUNNER_IMAGE" --name "$KIND_NAME"
  printf '%s\n' "$GATEWAY_RUNNER_IMAGE" > "$STATE_DIR/gateway-runner-image"
}
