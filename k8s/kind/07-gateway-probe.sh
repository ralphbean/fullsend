#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"

usage() {
  fail "Usage: $0 --gateway-bundle DIR --gateway-name NAME --client-secret-file FILE --sandbox-image IMAGE@sha256:DIGEST"
}

bundle_dir=''
gateway_name=''
client_secret_file=''
sandbox_image=''
while [[ $# -gt 0 ]]; do
  case "$1" in
    --gateway-bundle) [[ $# -ge 2 ]] || usage; bundle_dir="$2"; shift 2 ;;
    --gateway-name) [[ $# -ge 2 ]] || usage; gateway_name="$2"; shift 2 ;;
    --client-secret-file) [[ $# -ge 2 ]] || usage; client_secret_file="$2"; shift 2 ;;
    --sandbox-image) [[ $# -ge 2 ]] || usage; sandbox_image="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n "$bundle_dir" && -n "$gateway_name" && -n "$client_secret_file" && -n "$sandbox_image" ]] || usage
[[ "$gateway_name" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]*$ ]] || fail 'Gateway name contains unsupported characters'
[[ -f "$bundle_dir/gateways/$gateway_name/metadata.json" ]] || fail "Gateway registration $gateway_name is missing from $bundle_dir"
[[ ! -L "$bundle_dir/gateways/$gateway_name/metadata.json" ]] || fail 'Gateway metadata must not be a symlink'
jq -e '.auth_mode == "oidc"' "$bundle_dir/gateways/$gateway_name/metadata.json" >/dev/null || fail 'Gateway registration must use OIDC'
[[ -f "$client_secret_file" && -s "$client_secret_file" && ! -L "$client_secret_file" ]] || fail 'Client secret file is missing, empty, or a symlink'
[[ "$(stat -c '%a' "$client_secret_file")" == 600 ]] || fail 'Client secret file must have mode 600'
require_owned

# Package only public registration metadata. The host's OpenShell directory
# may contain browser tokens and local gateway signing keys; none belong in
# this Pod. Client credentials come from a separate Kubernetes Secret.
bundle_stage="$(mktemp -d "$LOCAL_DIR/gateway-stage.XXXXXX")"
preflight_dir="$(mktemp -d "$LOCAL_DIR/gateway-preflight.XXXXXX")"
archive="$(mktemp "$LOCAL_DIR/gateway-bundle.XXXXXX.tgz")"
secret_stage="$(mktemp "$LOCAL_DIR/gateway-secret.XXXXXX")"
trap 'rm -rf "$bundle_stage" "$preflight_dir"; rm -f "$archive" "$secret_stage"' EXIT
printf '%s' "$(cat "$client_secret_file")" > "$secret_stage"
[[ -s "$secret_stage" ]] || fail 'Client secret is empty after removing trailing line endings'
mkdir -p "$bundle_stage/gateways"
mkdir -p "$bundle_stage/gateways/$gateway_name"
cp "$bundle_dir/gateways/$gateway_name/metadata.json" "$bundle_stage/gateways/$gateway_name/metadata.json"
printf '%s\n' "$gateway_name" > "$bundle_stage/active_gateway"
command -v openshell >/dev/null || fail 'Install the matching OpenShell client on the host first'
[[ "$(openshell --version)" == 'openshell 0.1.2-rhaiv.0' ]] || fail 'Host OpenShell must be 0.1.2-rhaiv.0'
mkdir -p "$preflight_dir/openshell"
cp -a "$bundle_stage/." "$preflight_dir/openshell/"
# !!! TEMPORARY TLS VERIFICATION BYPASS !!!
# !!! The managed gateway's private CA is unavailable to end users, and its
# !!! certificate does not cover the public route hostname. Track the fix at:
# !!! https://github.com/openshift-online/hypershell/issues/443
# !!! Remove OPENSHELL_GATEWAY_INSECURE when that issue is resolved.
status '⚠️' '!!! OPENSHELL_GATEWAY_INSECURE=true: gateway TLS certificate verification is DISABLED for this PoC !!!'
status '⚠️' '!!! Track the CA and public-hostname fix: https://github.com/openshift-online/hypershell/issues/443 !!!'
status '🔧' 'Checking gateway DNS and service-account authentication from the host'
# This CLI version exchanges the client secret during gateway login, then
# whoami consumes the short-lived token in the isolated preflight directory.
if ! OPENSHELL_OIDC_CLIENT_SECRET="$(cat "$client_secret_file")" \
  OPENSHELL_GATEWAY_INSECURE=true XDG_CONFIG_HOME="$preflight_dir" \
  openshell gateway login "$gateway_name" >/dev/null; then
  fail 'Gateway service-account login failed before launching a Pod'
fi
if ! OPENSHELL_OIDC_CLIENT_SECRET="$(cat "$client_secret_file")" \
  OPENSHELL_GATEWAY_INSECURE=true XDG_CONFIG_HOME="$preflight_dir" \
  openshell -g "$gateway_name" whoami --output json >/dev/null; then
  fail 'Gateway connection failed; check DNS or service-account access before launching a Pod'
fi
tar -C "$bundle_stage" -czf "$archive" .
status '🔧' 'Installing gateway client bundle Secret in fullsend-runner'
k -n fullsend-runner create secret generic fullsend-gateway-bundle \
  --from-file="bundle.tgz=$archive" --dry-run=client -o json | k apply -f - >/dev/null
status '🔧' 'Installing gateway service-account credential Secret in fullsend-runner'
k -n fullsend-runner create secret generic fullsend-gateway-auth \
  --from-file="client-secret=$secret_stage" --dry-run=client -o json | k apply -f - >/dev/null

build_gateway_runner
runner_image="$GATEWAY_RUNNER_IMAGE"

suffix="$(uuidgen | tr '[:upper:]' '[:lower:]' | cut -c1-8)"
job_name="fs-gateway-$suffix"
probe_name="fs-$suffix"
status '🔧' "Launching runner Job $job_name"
(cd "$REPO_ROOT" && go run ./cmd/fullsend k8s-poc render-gateway-probe-job \
  --job-name "$job_name" --probe-name "$probe_name" \
  --runner-image "$runner_image" --sandbox-image "$sandbox_image") | k apply -f - >/dev/null

status 'ℹ️' "Job: fullsend-runner/$job_name; sandbox names: ${probe_name}a, ${probe_name}b"
status 'ℹ️' "Watch: kubectl --kubeconfig $KUBECONFIG_POC --context $KIND_CONTEXT -n fullsend-runner get pods,jobs -w"
status 'ℹ️' "Logs: kubectl --kubeconfig $KUBECONFIG_POC --context $KIND_CONTEXT -n fullsend-runner logs job/$job_name -c runner"
status 'ℹ️' "Evidence: PVC fullsend-runner-evidence, directory /evidence/$job_name"
