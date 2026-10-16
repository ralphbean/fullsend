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
script_start() { status '🔧' "executing k8s/kind/${1##*/}"; }
checkout_content_hash() {
  local path
  (
    cd "$REPO_ROOT"
    {
      git diff HEAD --binary
      while IFS= read -r -d '' path; do
        if [[ -f "$path" ]]; then sha256sum -- "$path"; fi
      done < <(git ls-files --others --exclude-standard -z | sort -z)
    } | sha256sum | cut -c1-16
  )
}
fail() { status '❌' "$*"; exit 1; }
k() { kubectl --kubeconfig "$KUBECONFIG_POC" --context "$KIND_CONTEXT" "$@"; }
require_owned() {
  [[ -f "$STATE_DIR/cluster-owned" ]] || fail "Cluster ownership marker missing; refusing to operate on $KIND_NAME"
  [[ -f "$KUBECONFIG_POC" ]] || fail "Dedicated kubeconfig missing"
  k cluster-info >/dev/null || fail "Dedicated PoC context is unavailable"
}
cluster_exists() {
  podman ps -a --filter "name=^${KIND_NAME}-control-plane$" --format '{{.Names}}' | grep -Fxq "${KIND_NAME}-control-plane"
}
kind_image_loaded() {
  local image="$1" nodes node loaded_images
  nodes="$(kind get nodes --name "$KIND_NAME")" || return 1
  [[ -n "$nodes" ]] || return 1
  while IFS= read -r node; do
    [[ -n "$node" ]] || continue
    loaded_images="$(podman exec "$node" ctr -n k8s.io images list -q)" || return 1
    if ! grep -Fxq -- "$image" <<< "$loaded_images"; then
      return 1
    fi
  done <<< "$nodes"
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
  local revision content_hash tag base_runner_image runner_image_available=0
  revision="$(git -C "$REPO_ROOT" rev-parse HEAD)"
  content_hash="$(checkout_content_hash)"
  tag="${revision:0:12}-${content_hash}"
  GATEWAY_RUNNER_IMAGE="localhost/fullsend-poc-runner:$tag"
  base_runner_image="localhost/fullsend-poc-runner-base:$tag"

  if podman image exists "$GATEWAY_RUNNER_IMAGE" >/dev/null 2>&1; then
    runner_image_available=1
  fi
  if (( runner_image_available )) && kind_image_loaded "$GATEWAY_RUNNER_IMAGE"; then
    status '✅' "Runner image for this checkout ($tag) is available locally and loaded in every kind node; skipping build and load"
  else
    if (( runner_image_available )); then
      status '♻️' "Reusing the locally available runner image for this checkout ($tag)"
    else
      status '🔧' "Building runner image from this checkout ($tag)"
      if podman image exists "$base_runner_image" >/dev/null 2>&1; then
        status '♻️' 'Reusing the matching UBI10 runner base image'
      else
        status '🔧' 'Building the UBI10 runner base image'
        (cd "$REPO_ROOT" && podman build \
          --build-arg "FULLSEND_COMMIT=$revision" \
          -f images/runner/Containerfile -t "$base_runner_image" .)
      fi
      podman build --build-arg "BASE_RUNNER_IMAGE=$base_runner_image" \
        -f "$K8S_ROOT/runner/Containerfile" -t "$GATEWAY_RUNNER_IMAGE" "$K8S_ROOT/runner"
    fi
  fi

  status '🔎' 'Checking that the runner image has the expected OpenShell version and UID setup'
  [[ "$(podman run --rm --entrypoint openshell "$GATEWAY_RUNNER_IMAGE" --version)" == 'openshell 0.1.2-rhaiv.0' ]] \
    || fail 'Runner OpenShell version does not match the provisioned gateway'
  [[ "$(podman run --rm --user 10001:10001 --entrypoint id "$GATEWAY_RUNNER_IMAGE" -un)" == runner ]] \
    || fail 'Runner UID 10001 must resolve to a user for OpenShell SSH transfers'

  if ! kind_image_loaded "$GATEWAY_RUNNER_IMAGE"; then
    status '📦' "Loading runner image into kind ($GATEWAY_RUNNER_IMAGE)"
    kind load docker-image "$GATEWAY_RUNNER_IMAGE" --name "$KIND_NAME"
    kind_image_loaded "$GATEWAY_RUNNER_IMAGE" \
      || fail 'Runner image load completed, but the image is still missing from a kind node'
  fi
  printf '%s\n' "$GATEWAY_RUNNER_IMAGE" > "$STATE_DIR/gateway-runner-image"
}
