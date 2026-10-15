#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"

usage() {
  fail "Usage: $0 --project PROJECT --pool POOL --provider PROVIDER [--access-token-file FILE]"
}

project=''
pool=''
provider=''
token_file=''
while [[ $# -gt 0 ]]; do
  case "$1" in
    --project) [[ $# -ge 2 ]] || usage; project="$2"; shift 2 ;;
    --pool) [[ $# -ge 2 ]] || usage; pool="$2"; shift 2 ;;
    --provider) [[ $# -ge 2 ]] || usage; provider="$2"; shift 2 ;;
    --access-token-file) [[ $# -ge 2 ]] || usage; token_file="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n "$project" && -n "$pool" && -n "$provider" ]] || usage
[[ "$project" =~ ^[a-z][a-z0-9-]+$ && "$pool" =~ ^[a-z][a-z0-9-]+$ && "$provider" =~ ^[a-z][a-z0-9-]+$ ]] || fail 'Invalid GCP resource name'
if [[ -n "$token_file" ]]; then
  [[ -s "$token_file" && ! -L "$token_file" ]] || fail 'Access-token file is missing, empty, or a symlink'
fi
require_owned
for tool in gcloud jq; do command -v "$tool" >/dev/null || fail "Missing $tool"; done

status '🔎' 'Checking that the existing Google WIF provider is active and trusts this kind cluster'
args=(--project="$project")
if [[ -n "$token_file" ]]; then args+=(--access-token-file="$token_file"); fi
issuer="$(k get --raw /.well-known/openid-configuration | jq -er '.issuer | select(type == "string" and length > 0)')"
details="$(gcloud "${args[@]}" iam workload-identity-pools providers describe "$provider" \
  --workload-identity-pool="$pool" --location=global --format=json)"
jq -e --arg issuer "$issuer" '.state == "ACTIVE" and .oidc.issuerUri == $issuer' <<<"$details" >/dev/null \
  || fail 'WIF provider is inactive or has a different Kubernetes issuer'

status '🔐' 'Reading the cluster signing keys and confirming the JWKS contains public keys only'
umask 077
mkdir -p "$LOCAL_DIR/gcp"
jwks_file="$LOCAL_DIR/gcp/kind-jwks.json"
stage="$(mktemp "$LOCAL_DIR/gcp/kind-jwks.XXXXXX")"
trap 'rm -f "$stage"' EXIT
k get --raw /openid/v1/jwks > "$stage"
jq -e '.keys | type == "array" and length > 0 and all(.[]; has("d") | not)' "$stage" >/dev/null \
  || fail 'Cluster JWKS is empty or contains a private key'
mv -f "$stage" "$jwks_file"

status '☁️' 'Updating the existing Google WIF provider with the cluster public keys'
gcloud "${args[@]}" iam workload-identity-pools providers update-oidc "$provider" \
  --workload-identity-pool="$pool" --location=global --jwk-json-path="$jwks_file" >/dev/null
status '✅' 'Google can now verify tokens signed by this kind cluster; no private signing key was uploaded'
