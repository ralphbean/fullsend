#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"

usage() {
  fail "Usage: $0 --old-gateway-label LABEL --job fs-gateway-XXXXXXXX [--job ...] --acknowledge-unverified-cleanup"
}

old_gateway_label=''
acknowledged=false
jobs=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --old-gateway-label) [[ $# -ge 2 ]] || usage; old_gateway_label="$2"; shift 2 ;;
    --job) [[ $# -ge 2 ]] || usage; jobs+=("$2"); shift 2 ;;
    --acknowledge-unverified-cleanup) acknowledged=true; shift ;;
    *) usage ;;
  esac
done
[[ "$old_gateway_label" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] || usage
[[ "${#jobs[@]}" -gt 0 && "$acknowledged" == true ]] || usage
for job in "${jobs[@]}"; do
  [[ "$job" =~ ^fs-gateway-[0-9a-f]{8}$ ]] || usage
done
require_owned
command -v podman >/dev/null || fail 'Podman is required to export the kind evidence PVC'
umask 077

# This is the explicit force escape hatch when an old gateway cannot be used
# for remote cleanup. It keeps an ownership ledger outside kind and moves only
# named, already-terminal probe directories out of the active cleanup scan.
# It does not claim the remote sandboxes were deleted.
status '⚠️' '!!! REMOTE CLEANUP IS UNVERIFIED for the old gateway !!!'
status '⚠️' '!!! Archiving old probe evidence; this does NOT delete remote sandboxes !!!'
archive_dir="$STATE_DIR/retired-gateways/$old_gateway_label"
[[ ! -e "$archive_dir" ]] || fail "Archive already exists: $archive_dir; inspect it before retrying"

pv_name="$(k -n fullsend-runner get pvc fullsend-runner-evidence -o jsonpath='{.spec.volumeName}')"
[[ "$pv_name" =~ ^pvc-[0-9a-f-]+$ ]] || fail 'Unexpected evidence PV name'
pv_path="$(k get pv "$pv_name" -o jsonpath='{.spec.hostPath.path}')"
[[ "$pv_path" == /var/local-path-provisioner/*_fullsend-runner_fullsend-runner-evidence ]] \
  || fail 'Evidence PVC is not the owned kind local-path volume'
node="${KIND_NAME}-control-plane"

for job in "${jobs[@]}"; do
  active="$(k -n fullsend-runner get job "$job" -o jsonpath='{.status.active}')" \
    || fail "Probe Job $job is missing; inspect its evidence before retirement"
  [[ -z "$active" || "$active" == 0 ]] || fail "Probe Job $job is still active"
  podman exec "$node" test -d "$pv_path/$job" || fail "Evidence directory for $job is absent from the PVC"
  if podman exec "$node" test -e "$pv_path/retired/$old_gateway_label/$job"; then
    fail "Retired evidence for $job already exists on the PVC; inspect it before retrying"
  fi
done

mkdir -p "$archive_dir"
for job in "${jobs[@]}"; do
  mkdir -p "$archive_dir/$job"
  podman cp "$node:$pv_path/$job/." "$archive_dir/$job/"
  [[ -s "$archive_dir/$job/intent.json" ]] || fail "Exported intent for $job is missing; old evidence remains active"
  suffix="${job#fs-gateway-}"
  jq -e --arg a "fs-${suffix}a" --arg b "fs-${suffix}b" '.names == [$a, $b]' \
    "$archive_dir/$job/intent.json" >/dev/null || fail "Intent names do not match owned Job $job"
  k -n fullsend-runner logs "job/$job" -c runner > "$archive_dir/$job/runner.log" \
    || fail "Cannot export runner log for $job; old evidence remains active"
  jq -n --arg gateway "$old_gateway_label" --arg job "$job" \
    --arg archived_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --slurpfile intent "$archive_dir/$job/intent.json" \
    '{old_gateway_label:$gateway,job:$job,archived_at:$archived_at,remote_cleanup:"unverified",sandbox_names:$intent[0].names}' \
    > "$archive_dir/$job/retirement.json"
done

for job in "${jobs[@]}"; do
  podman exec "$node" sh -c 'mkdir -p "$1/retired/$2" && mv "$1/$3" "$1/retired/$2/$3"' \
    sh "$pv_path" "$old_gateway_label" "$job"
done
status '✅' "Retired ${#jobs[@]} old probe evidence directories to $archive_dir and the PVC's retired/$old_gateway_label directory"
status '⚠️' '!!! Remote cleanup remains UNVERIFIED; preserve the archive for operator review !!!'
