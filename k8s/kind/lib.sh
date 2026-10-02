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
