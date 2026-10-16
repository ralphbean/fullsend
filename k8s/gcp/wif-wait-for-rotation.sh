#!/usr/bin/env bash
set -euo pipefail
set +x
umask 077

token_file=/sandbox/workspace/.gcp-oidc-token
initial_hash_file=/sandbox/workspace/.wif-initial-assertion.sha256
[[ -s "$token_file" ]] || { echo 'Projected assertion is missing' >&2; exit 1; }
read_assertion() {
  if assertion="$(jq -er '.value' "$token_file" 2>/dev/null)"; then
    printf '%s' "$assertion"
    unset assertion
  else
    cat "$token_file"
  fi
}
initial_assertion="$(read_assertion)"
[[ -n "$initial_assertion" ]] || { echo 'Projected assertion is empty' >&2; exit 1; }
initial_hash="$(printf '%s' "$initial_assertion" | sha256sum | cut -d' ' -f1)"
printf '%s  %s\n' "$initial_hash" "$initial_hash_file" > "$initial_hash_file"
unset initial_assertion

# The runner refreshes the sandbox copy every four minutes. A refresh can race
# with kubelet rotation, so allow the next refresh tick plus a short margin.
rotation_wait_seconds=780
deadline=$((SECONDS + rotation_wait_seconds))
while (( SECONDS < deadline )); do
  current_assertion="$(read_assertion)"
  current_hash="$(printf '%s' "$current_assertion" | sha256sum | cut -d' ' -f1)"
  unset current_assertion
  if [[ "$current_hash" != "$initial_hash" ]]; then
    echo 'Projected WIF assertion rotated.'
    bash /sandbox/workspace/sandbox-vertex-check.sh
    mkdir -p "${FULLSEND_OUTPUT_DIR:?Fullsend output directory is missing}"
    printf '%s\n' '{"status":"ok","source":"kind-gateway-demo"}' \
      > "$FULLSEND_OUTPUT_DIR/agent-result.json"
    exit 0
  fi
  sleep 5
done

echo "Projected WIF assertion did not rotate within ${rotation_wait_seconds} seconds." >&2
exit 1
