#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
HYPERSHELL_HSCTL_COMMIT=c17b8199916b0ee5b5a255d9928b3bd051efdbc9

usage() {
  fail "Usage: $0 --gateway-name NAME --service-account-name NAME [--gateway-request JSON] [--api-url URL --issuer-url URL | --token-file FILE | --config FILE] [--role openshell-user|openshell-admin] [--existing-client-secret-file FILE] [--openshell-name NAME] [--hsctl-bin FILE] [--openshell-config DIR]"
}

gateway_name=''
request_file=''
service_account_name=''
role=openshell-user
api_url=''
issuer_url=''
token_file=''
hsctl_bin=''
existing_secret_file=''
openshell_name=''
config_file="$LOCAL_DIR/gateway/hsctl-config.json"
openshell_config="$LOCAL_DIR/gateway/openshell"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --gateway-name) [[ $# -ge 2 ]] || usage; gateway_name="$2"; shift 2 ;;
    --gateway-request) [[ $# -ge 2 ]] || usage; request_file="$2"; shift 2 ;;
    --service-account-name) [[ $# -ge 2 ]] || usage; service_account_name="$2"; shift 2 ;;
    --role) [[ $# -ge 2 ]] || usage; role="$2"; shift 2 ;;
    --api-url) [[ $# -ge 2 ]] || usage; api_url="$2"; shift 2 ;;
    --issuer-url) [[ $# -ge 2 ]] || usage; issuer_url="$2"; shift 2 ;;
    --token-file) [[ $# -ge 2 ]] || usage; token_file="$2"; shift 2 ;;
    --existing-client-secret-file) [[ $# -ge 2 ]] || usage; existing_secret_file="$2"; shift 2 ;;
    --openshell-name) [[ $# -ge 2 ]] || usage; openshell_name="$2"; shift 2 ;;
    --config) [[ $# -ge 2 ]] || usage; config_file="$2"; shift 2 ;;
    --hsctl-bin) [[ $# -ge 2 ]] || usage; hsctl_bin="$2"; shift 2 ;;
    --openshell-config) [[ $# -ge 2 ]] || usage; openshell_config="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n "$gateway_name" && -n "$service_account_name" ]] || usage
[[ "$gateway_name" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]*$ ]] || fail 'Gateway name contains unsupported characters'
[[ "$service_account_name" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]*$ ]] || fail 'Service-account name contains unsupported characters'
openshell_name="${openshell_name:-$gateway_name}"
[[ "$openshell_name" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]*$ ]] || fail 'OpenShell registration name contains unsupported characters'
[[ "$role" == openshell-user || "$role" == openshell-admin ]] || fail 'Unsupported service-account role'
[[ "$(basename "$openshell_config")" == openshell ]] || fail '--openshell-config must name an openshell directory'
[[ -z "$request_file" || ( -s "$request_file" && ! -L "$request_file" ) ]] || fail 'Gateway request file is missing or a symlink'
if [[ -n "$existing_secret_file" ]]; then
  [[ -s "$existing_secret_file" && ! -L "$existing_secret_file" ]] || fail 'Existing client-secret file is missing, empty, or a symlink'
  [[ "$(stat -c '%a' "$existing_secret_file")" == 600 ]] || fail 'Existing client-secret file must have mode 600'
fi
if [[ ! -s "$config_file" && -z "$token_file" ]]; then
  [[ -n "$api_url" && -n "$issuer_url" ]] || fail 'No management login found. Supply --api-url and --issuer-url for device login, or --token-file with --api-url.'
fi
if [[ -n "$request_file" ]]; then
  jq -e --arg name "$gateway_name" '.name == $name and (.cluster_id | type == "string" and length > 0) and (.release_id | type == "string")' "$request_file" >/dev/null \
    || fail 'Gateway request must contain matching name, cluster_id, and release_id (which may be empty)'
fi
for tool in git go jq openshell; do command -v "$tool" >/dev/null || fail "Missing $tool"; done
[[ "$(openshell --version)" == 'openshell 0.1.2-rhaiv.0' ]] || fail 'OpenShell must be 0.1.2-rhaiv.0'

# Keep management tokens and the one-time service-account credential outside Git.
umask 077
mkdir -p "$LOCAL_DIR/gateway"
if [[ -z "$hsctl_bin" ]]; then
  source_dir="$LOCAL_DIR/hsctl-src"
  hsctl_bin="$LOCAL_DIR/bin/hsctl"
  if [[ ! -x "$hsctl_bin" ]]; then
    if [[ ! -d "$source_dir/.git" ]]; then
      status '🔧' 'Fetching pinned HyperShell source for hsctl'
      git clone --filter=blob:none --sparse https://github.com/openshift-online/hypershell.git "$source_dir"
      git -C "$source_dir" sparse-checkout set components/cli
    fi
    git -C "$source_dir" fetch origin "$HYPERSHELL_HSCTL_COMMIT"
    git -C "$source_dir" checkout --detach "$HYPERSHELL_HSCTL_COMMIT"
    [[ "$(git -C "$source_dir" rev-parse HEAD)" == "$HYPERSHELL_HSCTL_COMMIT" ]] || fail 'hsctl source revision mismatch'
    mkdir -p "$(dirname "$hsctl_bin")"
    status '🔧' 'Building pinned hsctl'
    (cd "$source_dir/components/cli" && go build -o "$hsctl_bin" ./cmd/hsctl)
  fi
fi
[[ -x "$hsctl_bin" ]] || fail "hsctl binary missing: $hsctl_bin"
export HYPERSHELL_CONFIG="$config_file"
if [[ -s "$config_file" && -n "$api_url" ]]; then
  [[ "$(jq -r '.url // empty' "$config_file")" == "$api_url" ]] || fail 'Existing management config uses a different API URL'
fi
if [[ -n "$token_file" ]]; then
  [[ -s "$token_file" && ! -L "$token_file" ]] || fail 'Management token file is missing or a symlink'
  [[ -n "$api_url" ]] || fail '--api-url is required with --token-file'
  "$hsctl_bin" login --url "$api_url" --token-file "$token_file" >/dev/null
elif [[ ! -s "$config_file" ]]; then
  status '🔐' 'One-time HyperShell management device login is required; later runs reuse its refresh token'
  "$hsctl_bin" login --no-browser --url "$api_url" --issuer-url "$issuer_url"
fi

gateway_list="$("$hsctl_bin" list gateways --output json)" || fail 'Cannot list gateways with the management credential'
gateway_count="$(jq --arg name "$gateway_name" '[.items[] | select(.name == $name)] | length' <<<"$gateway_list")"
[[ "$gateway_count" -le 1 ]] || fail "Multiple gateways named $gateway_name; refusing to choose one"
if [[ "$gateway_count" == 0 ]]; then
  [[ -n "$request_file" ]] || fail "Gateway $gateway_name is absent; supply --gateway-request with its placement and release"
  status '🔧' "Creating gateway $gateway_name"
  gateway_json="$("$hsctl_bin" create gateway --body "$request_file")" || fail 'Gateway creation failed'
else
  status '♻️' "Reusing gateway $gateway_name"
  gateway_json="$(jq -c --arg name "$gateway_name" '.items[] | select(.name == $name)' <<<"$gateway_list")"
fi
gateway_id="$(jq -er '.id | select(type == "string" and length > 0)' <<<"$gateway_json")" || fail 'Gateway has no ID'
if [[ -n "$request_file" ]]; then
  desired_cluster="$(jq -r '.cluster_id' "$request_file")"
  desired_release="$(jq -r '.release_id' "$request_file")"
  jq -e --arg cluster "$desired_cluster" --arg release "$desired_release" \
    '.cluster_id == $cluster and .release_id == $release' <<<"$gateway_json" >/dev/null \
    || fail 'Existing gateway placement or release differs from the requested gateway'
fi

status '🔧' 'Waiting for gateway route and service-account readiness'
for attempt in {1..60}; do
  gateway_json="$("$hsctl_bin" get gateway "$gateway_id")" || fail 'Cannot inspect gateway'
  gateway_phase="$(jq -r '.phase // empty' <<<"$gateway_json")"
  [[ "$gateway_phase" != Degraded && "$gateway_phase" != Failed ]] \
    || fail "Gateway is $gateway_phase; inspect its status and provisioning conditions in the HyperShell console"
  route_address="$(jq -r '(.route_address | select(type == "string" and length > 0)) // .external_dns // empty' <<<"$gateway_json")"
  [[ "$gateway_phase" == Running && -n "$route_address" ]] && break
  if (( attempt < 60 )); then sleep 10; fi
done
[[ "$gateway_phase" == Running && -n "$route_address" ]] || fail 'Gateway did not reach Running phase with a route'

account_list="$("$hsctl_bin" list service-accounts --gateway-id "$gateway_id" --search "$service_account_name" --size 100 --output json)" || fail 'Cannot list service accounts'
account_count="$(jq --arg name "$service_account_name" '[.items[] | select(.name == $name and .status != "revoked" and .status != "expired")] | length' <<<"$account_list")"
[[ "$account_count" -le 1 ]] || fail "Multiple active service accounts named $service_account_name"
credential_file="$LOCAL_DIR/gateway/$gateway_name-$service_account_name-credential.json"
secret_file="$LOCAL_DIR/gateway/$gateway_name-$service_account_name-client-secret"
if [[ "$account_count" == 0 ]]; then
  [[ -z "$existing_secret_file" ]] || fail 'Cannot reuse a client secret when the named service account does not exist'
  [[ ! -e "$credential_file" && ! -e "$secret_file" ]] || fail 'Local credential already exists; inspect it before creating a new service account'
  status '🔧' "Creating service account $service_account_name"
  "$hsctl_bin" create service-account --gateway-id "$gateway_id" --name "$service_account_name" \
    --role "$role" --expires-in 30d --output-file "$credential_file" >/dev/null \
    || fail 'Service-account creation failed; inspect the gateway before retrying'
  account_json="$(jq -c '.' "$credential_file")"
else
  account_json="$(jq -c --arg name "$service_account_name" '.items[] | select(.name == $name and .status != "revoked" and .status != "expired")' <<<"$account_list")"
  [[ -s "$credential_file" || -n "$existing_secret_file" ]] || fail 'Existing service-account secret cannot be recovered; supply its original credential bundle or --existing-client-secret-file'
  status '♻️' "Reusing service account $service_account_name"
fi
account_id="$(jq -er '.id | select(type == "string" and length > 0)' <<<"$account_json")" || fail 'Service account has no ID'
[[ "$(jq -r '.role' <<<"$account_json")" == "$role" ]] || fail 'Service-account role differs from the requested role'
if [[ -s "$credential_file" ]]; then
  [[ ! -L "$credential_file" && ! -L "$secret_file" ]] || fail 'Credential paths must not be symlinks'
  [[ "$(jq -r '.id' "$credential_file")" == "$account_id" ]] || fail 'Local credential belongs to another service account'
  [[ "$(jq -r '.role' "$credential_file")" == "$role" ]] || fail 'Local credential role differs from the requested role'
  secret_stage="$(mktemp "$LOCAL_DIR/gateway/secret.XXXXXX")"
  trap 'rm -f "$secret_stage"' EXIT
  jq -jer '.credential.client_secret | select(type == "string" and length > 0)' "$credential_file" > "$secret_stage" \
    || fail 'Local service-account credential did not contain a client secret'
  mv -f "$secret_stage" "$secret_file"
  chmod 600 "$credential_file" "$secret_file"
else
  secret_file="$existing_secret_file"
fi
for attempt in {1..30}; do
  account_detail="$("$hsctl_bin" get service-account "$account_id" --gateway-id "$gateway_id" --output json)" || fail 'Cannot inspect service account'
  [[ "$(jq -r '.status' <<<"$account_detail")" == ready ]] && break
  if (( attempt < 30 )); then sleep 10; fi
done
[[ "$(jq -r '.status' <<<"$account_detail")" == ready ]] || fail 'Service account is not ready'
connection="$(jq -c '.connection' <<<"$account_detail")"
issuer="$(jq -er '.issuer' <<<"$connection")"
client_id="$(jq -er '.client_id' <<<"$connection")"
audience="$(jq -er '.audience' <<<"$connection")"
gateway_endpoint="$(jq -er '.gateway_endpoint' <<<"$connection")"
[[ "$gateway_endpoint" =~ ^https:// ]] || fail 'Service-account connection has no HTTPS gateway endpoint'

# OpenShell stores gateway metadata under XDG_CONFIG_HOME/openshell.
mkdir -p "$openshell_config"
registration="$openshell_config/gateways/$openshell_name/metadata.json"
if [[ -e "$registration" ]]; then
  jq -e --arg endpoint "$gateway_endpoint" --arg issuer "$issuer" --arg client "$client_id" --arg audience "$audience" \
    '.auth_mode == "oidc" and .gateway_endpoint == $endpoint and .oidc_issuer == $issuer and .oidc_client_id == $client and .oidc_audience == $audience' \
    "$registration" >/dev/null || fail 'Existing OpenShell registration differs; inspect it before changing it'
else
  status '🔧' 'Registering gateway with OpenShell'
  XDG_CONFIG_HOME="$(dirname "$openshell_config")" OPENSHELL_OIDC_CLIENT_SECRET="$(cat "$secret_file")" \
    openshell gateway add --name "$openshell_name" \
    --oidc-issuer "$issuer" --oidc-client-id "$client_id" --oidc-audience "$audience" "$gateway_endpoint" >/dev/null
fi
status '🔧' 'Verifying the named service account against the gateway'
XDG_CONFIG_HOME="$(dirname "$openshell_config")" OPENSHELL_OIDC_CLIENT_SECRET="$(cat "$secret_file")" \
  openshell gateway login "$openshell_name" >/dev/null || fail 'Service-account login failed'
# !!! TEMPORARY TLS VERIFICATION BYPASS !!!
# !!! The managed gateway CA/public-hostname issue is tracked at:
# !!! https://github.com/openshift-online/hypershell/issues/443
status '⚠️' '!!! OPENSHELL_GATEWAY_INSECURE=true: TLS certificate verification is DISABLED for setup verification !!!'
status '⚠️' '!!! Track the CA and public-hostname fix: https://github.com/openshift-online/hypershell/issues/443 !!!'
identity_output="$(XDG_CONFIG_HOME="$(dirname "$openshell_config")" OPENSHELL_GATEWAY_INSECURE=true \
  openshell -g "$openshell_name" whoami --output json)" || fail 'Service-account whoami failed'
# OpenShell can prepend its TLS warning to stdout before the JSON document.
identity="$(sed -n '/^{/,$p' <<<"$identity_output")"
expected_subject="$(jq -er '.subject | select(type == "string" and length > 0)' <<<"$account_detail")" || fail 'Service account has no subject'
[[ "$(jq -er '.subject | select(type == "string" and length > 0)' <<<"$identity")" == "$expected_subject" ]] || fail 'OpenShell authenticated a different service account'
status '✅' "Gateway and service account are configured. Bundle: $openshell_config; client secret: $secret_file"
status 'ℹ️' "Use these paths with 07-gateway-probe.sh --gateway-bundle, --gateway-name, and --client-secret-file"
