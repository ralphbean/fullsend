#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"

usage() {
  fail "Usage: $0 --app-id ID --installation-id ID --repository-id ID [--repository-id ID ...] [--dispatcher-repository-id ID] --repository OWNER/REPO --private-key-file FILE"
}

app_id=''
installation_id=''
repository_ids=()
dispatcher_repository_id=''
repository=''
private_key_file=''
while [[ $# -gt 0 ]]; do
  case "$1" in
    --app-id) [[ $# -ge 2 ]] || usage; app_id="$2"; shift 2 ;;
    --installation-id) [[ $# -ge 2 ]] || usage; installation_id="$2"; shift 2 ;;
    --repository-id) [[ $# -ge 2 ]] || usage; repository_ids+=("$2"); shift 2 ;;
    --dispatcher-repository-id) [[ $# -ge 2 ]] || usage; dispatcher_repository_id="$2"; shift 2 ;;
    --repository) [[ $# -ge 2 ]] || usage; repository="$2"; shift 2 ;;
    --private-key-file) [[ $# -ge 2 ]] || usage; private_key_file="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ "$app_id" =~ ^[1-9][0-9]*$ && "$installation_id" =~ ^[1-9][0-9]*$ && ${#repository_ids[@]} -gt 0 ]] || usage
declare -A seen_repository_ids=()
for repository_id in "${repository_ids[@]}"; do
  [[ "$repository_id" =~ ^[1-9][0-9]*$ ]] || usage
  [[ -z "${seen_repository_ids[$repository_id]+present}" ]] || fail "Repository ID $repository_id was provided more than once"
  seen_repository_ids["$repository_id"]=1
done
if [[ -z "$dispatcher_repository_id" ]]; then
  if (( ${#repository_ids[@]} != 1 )); then
    fail '--dispatcher-repository-id is required when authorizing multiple repository IDs'
  fi
  dispatcher_repository_id="${repository_ids[0]}"
fi
[[ "$dispatcher_repository_id" =~ ^[1-9][0-9]*$ ]] || usage
[[ -n "${seen_repository_ids[$dispatcher_repository_id]+present}" ]] || fail '--dispatcher-repository-id must also be listed with --repository-id'
[[ "$repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || usage
[[ -s "$private_key_file" && -f "$private_key_file" && ! -L "$private_key_file" ]] || fail 'Private key file is missing, empty, or a symlink'
require_owned
for tool in podman jq openssl; do command -v "$tool" >/dev/null || fail "Missing $tool"; done

repository_ids_json="$(jq -cn '$ARGS.positional | map(tonumber)' --args "${repository_ids[@]}")"

umask 077
mkdir -p "$LOCAL_DIR/mint"
mkdir -p "$LOCAL_DIR/mint/tls"
tag_input="$(git -C "$REPO_ROOT" rev-parse HEAD)-$(checkout_content_hash)"
tag="$(printf '%s' "$tag_input" | sha256sum | cut -c1-20)"
image="localhost/fullsend-k8s-mint:$tag"
status '🔧' "Building private mint image ($tag)"
podman build -f "$K8S_ROOT/mint/Containerfile" -t "$image" "$REPO_ROOT"
kind load docker-image "$image" --name "$KIND_NAME"
rendered_workload="$LOCAL_DIR/mint/workload.yaml"
sed "s|localhost/fullsend-k8s-mint:placeholder|$image|g" "$K8S_ROOT/mint/workload.yaml" > "$rendered_workload"
k apply -f "$rendered_workload" >/dev/null

issuer="$(k get --raw /.well-known/openid-configuration | jq -er '.issuer | select(type == "string" and length > 0)')"
stage="$(mktemp "$LOCAL_DIR/mint/jwks.XXXXXX")"
trap 'rm -f "$stage"' EXIT
k get --raw /openid/v1/jwks > "$stage"
jq -e '.keys | type == "array" and length > 0 and all(.[]; has("d") | not)' "$stage" >/dev/null \
  || fail 'Cluster JWKS is empty or contains a private key'
mv -f "$stage" "$LOCAL_DIR/mint/jwks.json"
jq -n --arg issuer "$issuer" --arg repository "$repository" --argjson installation_id "$installation_id" \
  --argjson repository_ids "$repository_ids_json" \
  '{trusted_issuers:[{issuer:$issuer,audience:"fullsend-k8s-mint",jwks_file:"/etc/fullsend-mint/trust/jwks.json",namespaces:{"fullsend-runner":{tenant:$repository,roles:{coder:{installation_id:$installation_id,repository_ids:$repository_ids,allowed_levels:["read","write"]},triage:{installation_id:$installation_id,repository_ids:$repository_ids,allowed_levels:["read","write"]}}}}}]}' \
  > "$LOCAL_DIR/mint/policy.json"
printf '{"coder":"%s","triage":"%s"}\n' "$app_id" "$app_id" > "$LOCAL_DIR/mint/role-app-ids.json"

if [[ ! -s "$LOCAL_DIR/mint/tls/ca.crt" || ! -s "$LOCAL_DIR/mint/tls/tls.crt" || ! -s "$LOCAL_DIR/mint/tls/tls.key" ]]; then
  openssl req -x509 -newkey rsa:3072 -nodes -days 3650 \
    -keyout "$LOCAL_DIR/mint/tls/ca.key" -out "$LOCAL_DIR/mint/tls/ca.crt" \
    -subj '/CN=Fullsend kind mint CA' >/dev/null 2>&1
  openssl req -newkey rsa:3072 -nodes -keyout "$LOCAL_DIR/mint/tls/tls.key" \
    -out "$LOCAL_DIR/mint/tls/tls.csr" -subj '/CN=fullsend-mint.fullsend-mint.svc' >/dev/null 2>&1
  cat > "$LOCAL_DIR/mint/tls/server.ext" <<'EOF'
subjectAltName=DNS:fullsend-mint,DNS:fullsend-mint.fullsend-mint,DNS:fullsend-mint.fullsend-mint.svc,DNS:fullsend-mint.fullsend-mint.svc.cluster.local
extendedKeyUsage=serverAuth
keyUsage=digitalSignature,keyEncipherment
EOF
  openssl x509 -req -in "$LOCAL_DIR/mint/tls/tls.csr" -CA "$LOCAL_DIR/mint/tls/ca.crt" \
    -CAkey "$LOCAL_DIR/mint/tls/ca.key" -CAcreateserial -out "$LOCAL_DIR/mint/tls/tls.crt" \
    -days 825 -sha256 -extfile "$LOCAL_DIR/mint/tls/server.ext" >/dev/null 2>&1
  rm -f "$LOCAL_DIR/mint/tls/tls.csr" "$LOCAL_DIR/mint/tls/server.ext" "$LOCAL_DIR/mint/tls/ca.srl"
fi

k -n fullsend-mint create configmap fullsend-mint-config \
  --from-file=policy.json="$LOCAL_DIR/mint/policy.json" \
  --from-file=jwks.json="$LOCAL_DIR/mint/jwks.json" \
  --from-file=ca.crt="$LOCAL_DIR/mint/tls/ca.crt" \
  --from-file=role-app-ids.json="$LOCAL_DIR/mint/role-app-ids.json" \
  --dry-run=client -o yaml | k apply -f - >/dev/null
k -n fullsend-control create configmap fullsend-mint-dispatch \
  --from-literal="repository-id=$dispatcher_repository_id" --dry-run=client -o yaml | k apply -f - >/dev/null
k -n fullsend-runner create configmap fullsend-mint-ca \
  --from-file=ca.crt="$LOCAL_DIR/mint/tls/ca.crt" --dry-run=client -o yaml | k apply -f - >/dev/null
k -n fullsend-mint create secret generic fullsend-app-pem \
  --from-file="coder.pem=$private_key_file" --from-file="triage.pem=$private_key_file" \
  --dry-run=client -o yaml | k apply -f - >/dev/null
k -n fullsend-mint create secret generic fullsend-mint-tls \
  --from-file=tls.crt="$LOCAL_DIR/mint/tls/tls.crt" --from-file=tls.key="$LOCAL_DIR/mint/tls/tls.key" \
  --dry-run=client -o yaml | k apply -f - >/dev/null

k apply -f "$rendered_workload" >/dev/null
printf '%s\n' "$image" > "$STATE_DIR/mint-image"
k -n fullsend-mint rollout status deployment/fullsend-mint --timeout=120s >/dev/null || fail 'Private mint did not become ready'
k -n fullsend-mint get service fullsend-mint >/dev/null
k -n fullsend-mint delete secret fullsend-app-pem-coder --ignore-not-found --wait=true >/dev/null
status '✅' "Private Kubernetes-identity mint is ready for ${#repository_ids[@]} repositories; dispatcher demo uses repository ID $dispatcher_repository_id; App PEM remains mounted only into the mint"
