#!/usr/bin/env bash
set -euo pipefail
set +x
umask 077

credential_file="${GOOGLE_APPLICATION_CREDENTIALS:?sandbox external-account config is missing}"
token_file=/sandbox/workspace/.gcp-oidc-token
initial_hash_file=/sandbox/workspace/.wif-initial-assertion.sha256
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
[[ -s "$token_file" && -s "$initial_hash_file" ]] || { echo 'Projected assertion or initial hash is missing' >&2; exit 1; }
initial_hash="$(cut -d' ' -f1 "$initial_hash_file")"
current_hash="$(jq -er '.value' "$token_file" | sha256sum | cut -d' ' -f1)"
[[ "$current_hash" != "$initial_hash" ]] || { echo 'Projected assertion did not rotate during the Fullsend run' >&2; exit 1; }

audience="$(jq -er '.audience' "$credential_file")"
impersonation_url="$(jq -er '.service_account_impersonation_url' "$credential_file")"
jq -er '.value' "$token_file" > "$tmpdir/subject.jwt"
sts_status="$(curl --silent --show-error --max-time 60 --output "$tmpdir/sts.json" --write-out '%{http_code}' \
  --request POST https://sts.googleapis.com/v1/token \
  --data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
  --data-urlencode "audience=$audience" \
  --data-urlencode 'scope=https://www.googleapis.com/auth/cloud-platform' \
  --data-urlencode 'requested_token_type=urn:ietf:params:oauth:token-type:access_token' \
  --data-urlencode 'subject_token_type=urn:ietf:params:oauth:token-type:jwt' \
  --data-urlencode "subject_token@$tmpdir/subject.jwt")"
[[ "$sts_status" == 200 ]] || { echo "Post-rotation Google STS exchange failed (HTTP $sts_status)" >&2; exit 1; }

federated_token="$(jq -er '.access_token' "$tmpdir/sts.json")"
printf 'header = "Authorization: Bearer %s"\n' "$federated_token" > "$tmpdir/auth"
chmod 600 "$tmpdir/auth"
iam_status="$(curl --silent --show-error --max-time 60 --output "$tmpdir/iam.json" --write-out '%{http_code}' \
  --request POST "$impersonation_url" --config "$tmpdir/auth" \
  --header 'Content-Type: application/json' \
  --data '{"scope":["https://www.googleapis.com/auth/cloud-platform"]}')"
unset federated_token
[[ "$iam_status" == 200 ]] || { echo "Inference service-account impersonation failed (HTTP $iam_status)" >&2; exit 1; }

inference_token="$(jq -er '.accessToken' "$tmpdir/iam.json")"
printf 'header = "Authorization: Bearer %s"\n' "$inference_token" > "$tmpdir/auth"
unset inference_token
request="$(jq -cn '{anthropic_version:"vertex-2023-10-16",messages:[{role:"user",content:"Reply with exactly: rotated WIF inference OK"}],max_tokens:32}')"
vertex_status="$(curl --silent --show-error --max-time 90 --output "$tmpdir/vertex.json" --write-out '%{http_code}' \
  --request POST \
  "https://aiplatform.googleapis.com/v1/projects/${ANTHROPIC_VERTEX_PROJECT_ID:?Vertex project is missing}/locations/global/publishers/anthropic/models/claude-haiku-4-5@20251001:rawPredict" \
  --config "$tmpdir/auth" --header 'Content-Type: application/json' --data "$request")"
if [[ "$vertex_status" != 200 ]] || ! jq -e '[.content[]? | select(.type == "text") | .text] | any(.[]; length > 0)' "$tmpdir/vertex.json" >/dev/null; then
  echo "Post-rotation Vertex inference failed (HTTP $vertex_status)" >&2
  exit 1
fi

printf 'Post-rotation WIF Vertex inference succeeded. Assertion SHA-256: %s\n' "$current_hash"
