#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"

usage() {
  fail "Usage: $0 --gateway-name NAME --client-secret-file FILE --sandbox-image IMAGE@sha256:DIGEST"
}

gateway_name=''
client_secret_file=''
sandbox_image=''
while [[ $# -gt 0 ]]; do
  case "$1" in
    --gateway-name) [[ $# -ge 2 ]] || usage; gateway_name="$2"; shift 2 ;;
    --client-secret-file) [[ $# -ge 2 ]] || usage; client_secret_file="$2"; shift 2 ;;
    --sandbox-image) [[ $# -ge 2 ]] || usage; sandbox_image="$2"; shift 2 ;;
    *) usage ;;
  esac
done
[[ "$gateway_name" =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]*$ ]] || usage
[[ -f "$client_secret_file" && ! -L "$client_secret_file" && -s "$client_secret_file" ]] || fail 'Client-secret file must be a nonempty regular file, not a symlink'
[[ "$sandbox_image" =~ @sha256:[0-9a-f]{64}$ ]] || fail 'Sandbox image must be pinned by sha256 digest'
for tool in openshell openssl jq rg; do command -v "$tool" >/dev/null || fail "Missing $tool"; done
[[ "$(openshell --version)" == 'openshell 0.1.2-rhaiv.0' ]] || fail 'OpenShell must be 0.1.2-rhaiv.0'
[[ -f "$LOCAL_DIR/gateway/openshell/gateways/$gateway_name/metadata.json" ]] || fail "OpenShell registration $gateway_name is missing"
[[ -f "$K8S_ROOT/fixtures/harness-demo/.fullsend/policies/no-network.yaml" ]] || fail 'Pinned demo sandbox policy is missing'

suffix="$(openssl rand -hex 4)"
workspace_a="fs-m4a-$suffix"
workspace_b="fs-m4b-$suffix"
sandbox_a="fs-m4a-$suffix"
sandbox_b="fs-m4b-$suffix"
provider_a="openai-a-$suffix"
provider_b="openai-b-$suffix"
profile_id="fullsend-openai"
policy="$K8S_ROOT/fixtures/harness-demo/.fullsend/policies/no-network.yaml"
profile="$REPO_ROOT/internal/scaffold/fullsend-repo/profiles/fullsend-openai.yaml"
report="$STATE_DIR/milestone-4-workspaces-$suffix.md"
workspace_a_created=false
workspace_b_created=false

os() {
  local workspace="$1"; shift
  OPENSHELL_OIDC_CLIENT_SECRET="$(cat "$client_secret_file")" \
    OPENSHELL_GATEWAY_INSECURE=true \
    RUST_LOG='warn,openshell_cli::tls=error' \
    XDG_CONFIG_HOME="$LOCAL_DIR/gateway" \
    openshell gateway login "$gateway_name" >/dev/null
  OPENSHELL_GATEWAY_INSECURE=true \
    RUST_LOG='warn,openshell_cli::tls=error' \
    XDG_CONFIG_HOME="$LOCAL_DIR/gateway" \
    openshell -g "$gateway_name" --workspace "$workspace" "$@"
}

gateway() {
  OPENSHELL_OIDC_CLIENT_SECRET="$(cat "$client_secret_file")" \
    OPENSHELL_GATEWAY_INSECURE=true \
    RUST_LOG='warn,openshell_cli::tls=error' \
    XDG_CONFIG_HOME="$LOCAL_DIR/gateway" \
    openshell gateway login "$gateway_name" >/dev/null
  OPENSHELL_GATEWAY_INSECURE=true \
    RUST_LOG='warn,openshell_cli::tls=error' \
    XDG_CONFIG_HOME="$LOCAL_DIR/gateway" \
    openshell -g "$gateway_name" "$@"
}

cleanup() {
  local result=$?
  local cleanup_failed=false
  trap - EXIT
  set +e
  if [[ "$workspace_a_created" == true ]]; then
    os "$workspace_a" sandbox delete "$sandbox_a" >/dev/null 2>&1
    os "$workspace_a" provider delete "$provider_a" >/dev/null 2>&1
    os "$workspace_a" profile delete "$profile_id" >/dev/null 2>&1
    if ! gateway workspace delete "$workspace_a" >/dev/null 2>&1; then
      status '⚠️' "Could not remove temporary workspace $workspace_a; inspect gateway state"
      cleanup_failed=true
      result=1
    fi
  fi
  if [[ "$workspace_b_created" == true ]]; then
    os "$workspace_b" sandbox delete "$sandbox_b" >/dev/null 2>&1
    os "$workspace_b" provider delete "$provider_b" >/dev/null 2>&1
    os "$workspace_b" profile delete "$profile_id" >/dev/null 2>&1
    if ! gateway workspace delete "$workspace_b" >/dev/null 2>&1; then
      status '⚠️' "Could not remove temporary workspace $workspace_b; inspect gateway state"
      cleanup_failed=true
      result=1
    fi
  fi
  if [[ "$result" -ne 0 ]]; then
    if [[ "$cleanup_failed" == true ]]; then
      status '⚠️' "Workspace isolation check failed; inspect gateway workspaces $workspace_a and $workspace_b for incomplete cleanup"
    else
      status '⚠️' 'Workspace isolation check failed; its temporary workspaces and resources were removed'
    fi
  fi
  exit "$result"
}
trap cleanup EXIT

status '⚠️' '!!! OPENSHELL_GATEWAY_INSECURE=true: gateway TLS certificate verification is DISABLED for this PoC !!!'
status '⚠️' '!!! Track the CA and public-hostname fix: https://github.com/openshift-online/hypershell/issues/443 !!!'
status '🔧' 'Enabling provider support and creating two temporary gateway workspaces'
if ! setting_output="$(gateway settings set --key providers_v2_enabled --value true --global --yes 2>&1)"; then
  if [[ "$setting_output" == *"unknown setting key 'providers_v2_enabled'"* ]]; then
    status 'ℹ️' 'Gateway uses the current provider mode; legacy providers_v2_enabled setting is absent'
  else
    printf '%s\n' "$setting_output" >&2
    fail 'Could not enable gateway provider support'
  fi
fi
gateway workspace create --name "$workspace_a" --label fullsend.ai/milestone=4 >/dev/null
workspace_a_created=true
gateway workspace create --name "$workspace_b" --label fullsend.ai/milestone=4 >/dev/null
workspace_b_created=true

credential_a="$(openssl rand -hex 32)"
credential_b="$(openssl rand -hex 32)"
[[ "$credential_a" != "$credential_b" ]] || fail 'Generated provider credentials unexpectedly matched'
for workspace in "$workspace_a" "$workspace_b"; do
  os "$workspace" profile import --file "$profile" >/dev/null
done
OPENAI_API_KEY="$credential_a" os "$workspace_a" provider create \
  --name "$provider_a" --type fullsend-openai --credential OPENAI_API_KEY >/dev/null
OPENAI_API_KEY="$credential_b" os "$workspace_b" provider create \
  --name "$provider_b" --type fullsend-openai --credential OPENAI_API_KEY >/dev/null

status '🔧' 'Creating one pinned sandbox per workspace and checking provider attachment without exposing credential values'
for entry in "$workspace_a:$sandbox_a" "$workspace_b:$sandbox_b"; do
  IFS=: read -r workspace sandbox <<< "$entry"
  provider="$provider_a"
  [[ "$workspace" == "$workspace_a" ]] || provider="$provider_b"
  os "$workspace" sandbox create --name "$sandbox" --from "$sandbox_image" \
    --policy "$policy" --provider "$provider" --detach --no-tty --output json >/dev/null
done
credential_hash_a="$(printf '%s' "$credential_a" | sha256sum | cut -d' ' -f1)"
credential_hash_b="$(printf '%s' "$credential_b" | sha256sum | cut -d' ' -f1)"
[[ "$credential_hash_a" != "$credential_hash_b" ]] || fail 'The two generated provider credentials unexpectedly matched'

os "$workspace_a" provider get "$provider_a" >/dev/null
os "$workspace_b" provider get "$provider_b" >/dev/null
for entry in "$workspace_a:$sandbox_a:$provider_a" "$workspace_b:$sandbox_b:$provider_b"; do
  IFS=: read -r workspace sandbox provider <<< "$entry"
  os "$workspace" sandbox get "$sandbox" >/dev/null
done
for entry in "$workspace_a:$provider_b:$sandbox_b" "$workspace_b:$provider_a:$sandbox_a"; do
  IFS=: read -r workspace other_provider other_sandbox <<< "$entry"
  if cross_output="$(os "$workspace" provider get "$other_provider" 2>&1)"; then
    fail "Provider $other_provider was visible from workspace $workspace"
  elif ! rg -iq 'not found|does not exist' <<< "$cross_output"; then
    printf '%s\n' "$cross_output" >&2
    fail "Could not verify that $other_provider is isolated from workspace $workspace"
  fi
  if cross_output="$(os "$workspace" sandbox get "$other_sandbox" 2>&1)"; then
    fail "Sandbox $other_sandbox was visible from workspace $workspace"
  elif ! rg -iq 'not found|does not exist' <<< "$cross_output"; then
    printf '%s\n' "$cross_output" >&2
    fail "Could not verify that $other_sandbox is isolated from workspace $workspace"
  fi
done
status '✅' 'Each sandbox was created with its workspace-scoped provider argument; cross-workspace provider and sandbox lookups were denied'

cat > "$report" <<EOF
# Milestone 4 workspace and provider isolation

- Observed: $(date -u +%Y-%m-%dT%H:%M:%SZ)
- Gateway: $gateway_name (dedicated PoC gateway)
- OpenShell: $(openshell --version)
- Sandbox image: $sandbox_image
- Workspaces: $workspace_a, $workspace_b
- Run-scoped providers: $provider_a in $workspace_a and $provider_b in $workspace_b (profile $profile_id)
- Generated test credential SHA-256 fingerprints: $credential_hash_a, $credential_hash_b (distinct; values were never logged)
- Sandbox scoping: each sandbox create succeeded with its workspace's provider name, and sandbox get succeeded only in that workspace
- Cross-workspace provider lookup: denied for both providers
- Cleanup: temporary sandboxes, providers, profiles, and workspaces are removed by the exit trap
- Gateway authentication: both runner workspaces still share the dedicated PoC gateway service-account credential; this check does not establish tenant-safe gateway authentication
- TLS: verification bypass remains enabled pending HyperShell issue #443
EOF
status '✅' "Recorded credential fingerprints and limits in $report"
