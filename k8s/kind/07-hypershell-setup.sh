#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"

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
if [[ -n "$request_file" && ( -e "$request_file" || -L "$request_file" ) ]]; then
  [[ -s "$request_file" && ! -L "$request_file" ]] || fail 'Gateway request file is empty or a symlink'
fi
if [[ -n "$existing_secret_file" ]]; then
  [[ -s "$existing_secret_file" && ! -L "$existing_secret_file" ]] || fail 'Existing client-secret file is missing, empty, or a symlink'
  [[ "$(stat -c '%a' "$existing_secret_file")" == 600 ]] || fail 'Existing client-secret file must have mode 600'
fi
if [[ ! -s "$config_file" && -z "$token_file" ]]; then
  [[ -n "$api_url" && -n "$issuer_url" ]] || fail 'No management login found. Supply --api-url and --issuer-url for device login, or --token-file with --api-url.'
fi
if [[ -n "$request_file" && -s "$request_file" ]]; then
  jq -e --arg name "$gateway_name" \
    '.name == $name and (.placement | type == "object" and ((has("mode") | not)) and (.selector | type == "array" and length > 0 and all(.[]; type == "string" and length > 0))) and (has("cluster_id") | not) and (has("release_id") | not)' \
    "$request_file" >/dev/null \
    || fail 'Gateway request must contain matching name and placement.selector with one or more capability labels; omit cluster_id and release_id'
fi
for tool in jq openshell; do command -v "$tool" >/dev/null || fail "Missing $tool"; done
[[ "$(openshell --version)" == 'openshell 0.1.2-rhaiv.0' ]] || fail 'OpenShell must be 0.1.2-rhaiv.0'

# Keep management tokens and the one-time service-account credential outside Git.
umask 077
mkdir -p "$LOCAL_DIR/gateway"
if [[ -z "$hsctl_bin" ]]; then
  "$K8S_ROOT/kind/07-hypershell-build.sh"
  hsctl_bin="$LOCAL_DIR/bin/hsctl"
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
jq -e 'has("items") and (.items == null or (.items | type) == "array")' <<<"$gateway_list" >/dev/null \
  || fail 'Gateway list response has no valid items field'
gateway_count="$(jq --arg name "$gateway_name" '[(.items // [])[] | select(.name == $name)] | length' <<<"$gateway_list")"
[[ "$gateway_count" -le 1 ]] || fail "Multiple gateways named $gateway_name; refusing to choose one"
if [[ "$gateway_count" == 0 ]]; then
  if [[ -z "$request_file" || ! -s "$request_file" ]]; then
    status 'ℹ️' "Gateway $gateway_name is not registered; listing ManagedCluster placement capabilities so you can prepare its request"
    cluster_list="$("$hsctl_bin" list managedClusters --output json)" || fail 'Cannot list registered ManagedClusters'
    jq -e 'has("items") and (.items == null or (.items | type) == "array")' <<<"$cluster_list" >/dev/null \
      || fail 'ManagedCluster list response has no valid items field'
    jq -r '(.items // [])[] | [.name // "", ((.capabilities // []) | join(","))] | @tsv' <<<"$cluster_list" >&2
    request_path="${request_file:-$LOCAL_DIR/gateway/request.json}"
    fail "Create $request_path with placement.selector labels from the intended cluster above, then rerun with --gateway-request; the API chooses a connected cluster matching all labels"
  fi
  requested_selector="$(jq -c '.placement.selector' "$request_file")"
  cluster_list="$("$hsctl_bin" list managedClusters --output json)" || fail 'Cannot list registered ManagedClusters'
  jq -e 'has("items") and (.items == null or (.items | type) == "array")' <<<"$cluster_list" >/dev/null \
    || fail 'ManagedCluster list response has no valid items field'
  jq -e --argjson selector "$requested_selector" \
    'any((.items // [])[]; . as $cluster | all($selector[]; . as $required | ($cluster.capabilities // []) | index($required) != null))' <<<"$cluster_list" >/dev/null \
    || fail 'No registered ManagedCluster advertises all placement.selector labels; inspect hsctl list managedClusters --output json'
  jq -r --argjson selector "$requested_selector" \
    '(.items // [])[] as $cluster | select(all($selector[]; . as $required | ($cluster.capabilities // []) | index($required) != null)) | [$cluster.name // "", $cluster.id // ""] | @tsv' <<<"$cluster_list" >&2
  status '🔧' "Creating gateway $gateway_name"
  gateway_json="$("$hsctl_bin" create gateway --body "$request_file")" || fail 'Gateway creation failed'
else
  if [[ -n "$request_file" && ! -s "$request_file" ]]; then
    status 'ℹ️' 'The named gateway already exists; reusing it without the optional creation request'
    request_file=''
  fi
  status '♻️' "Reusing gateway $gateway_name"
  gateway_json="$(jq -c --arg name "$gateway_name" '(.items // [])[] | select(.name == $name)' <<<"$gateway_list")"
fi
gateway_id="$(jq -er '.id | select(type == "string" and length > 0)' <<<"$gateway_json")" || fail 'Gateway has no ID'
if [[ -n "$request_file" ]]; then
  desired_selector="$(jq -c '.placement.selector' "$request_file")"
  selected_cluster="$(jq -er '.cluster_id | select(type == "string" and length > 0)' <<<"$gateway_json")" \
    || fail 'Gateway response has no resolved cluster_id'
  if [[ -z "${cluster_list:-}" ]]; then
    cluster_list="$("$hsctl_bin" list managedClusters --output json)" || fail 'Cannot list registered ManagedClusters'
    jq -e 'has("items") and (.items == null or (.items | type == "array"))' <<<"$cluster_list" >/dev/null \
      || fail 'ManagedCluster list response has no valid items field'
  fi
  jq -e --arg id "$selected_cluster" --argjson selector "$desired_selector" \
    'any((.items // [])[]; . as $cluster | $cluster.id == $id and all($selector[]; . as $required | ($cluster.capabilities // []) | index($required) != null))' <<<"$cluster_list" >/dev/null \
    || fail 'Gateway was resolved to a cluster that does not match the requested placement.selector'
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
jq -e 'has("items") and (.items == null or (.items | type) == "array")' <<<"$account_list" >/dev/null \
  || fail 'Service-account list response has no valid items field'
account_count="$(jq --arg name "$service_account_name" '[(.items // [])[] | select(.name == $name and .status != "revoked" and .status != "expired")] | length' <<<"$account_list")"
[[ "$account_count" -le 1 ]] || fail "Multiple active service accounts named $service_account_name"
credential_file="$LOCAL_DIR/gateway/$gateway_name-$service_account_name-credential.json"
secret_file="$LOCAL_DIR/gateway/$gateway_name-$service_account_name-client-secret"
if [[ "$account_count" == 0 ]]; then
  [[ -z "$existing_secret_file" ]] || fail 'Cannot reuse a client secret when the named service account does not exist'
  existing_local_credentials=()
  [[ ! -e "$credential_file" ]] || existing_local_credentials+=("$credential_file")
  [[ ! -e "$secret_file" ]] || existing_local_credentials+=("$secret_file")
  if (( ${#existing_local_credentials[@]} > 0 )); then
    fail "Local credential already exists at: ${existing_local_credentials[*]}; inspect it before creating a new service account"
  fi
  status '🔧' "Creating service account $service_account_name"
  create_attempt=1
  create_max_attempts=8
  create_delay=2
  while true; do
    if create_error="$("$hsctl_bin" create service-account --gateway-id "$gateway_id" --name "$service_account_name" \
      --role "$role" --expires-in 30d --output-file "$credential_file" 2>&1 >/dev/null)"; then
      break
    fi

    if [[ "$create_error" != *'API returned 409 (gateway_not_ready)'* ]]; then
      printf '%s\n' "$create_error" >&2
      fail 'Service-account creation failed; inspect the gateway before retrying'
    fi

    if (( create_attempt >= create_max_attempts )); then
      printf '%s\n' "$create_error" >&2
      fail "Gateway stayed unready for service-account provisioning after $create_max_attempts attempts"
    fi

    status '⏳' "Gateway is not ready for service-account provisioning; retry $((create_attempt + 1))/$create_max_attempts in ${create_delay}s"
    sleep "$create_delay"
    create_attempt=$((create_attempt + 1))
    create_delay=$((create_delay * 2))
    if (( create_delay > 30 )); then create_delay=30; fi
  done
  account_json="$(jq -c '.' "$credential_file")"
else
  account_json="$(jq -c --arg name "$service_account_name" '(.items // [])[] | select(.name == $name and .status != "revoked" and .status != "expired")' <<<"$account_list")"
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
    "$registration" >/dev/null || fail "Existing OpenShell registration at $registration differs; inspect it before changing it"
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
