#!/usr/bin/env bash
set -euo pipefail
set +x
umask 077
token_file=/var/run/fullsend/wif/token
credential_file="${GOOGLE_APPLICATION_CREDENTIALS:?external-account config is required}"
trap 'rm -f /tmp/fullsend-sts.json /tmp/fullsend-iam.json /tmp/fullsend-vertex.json /tmp/fullsend-curl-auth' EXIT

audience="$(jq -er '.audience' "$credential_file")"
impersonation_url="$(jq -er '.service_account_impersonation_url' "$credential_file")"
[[ -s "$token_file" ]] || { echo 'Projected inference token is missing' >&2; exit 1; }

request_sts() {
  curl --silent --show-error --max-time 30 --output /tmp/fullsend-sts.json \
    --write-out '%{http_code}' --request POST https://sts.googleapis.com/v1/token \
    --data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:token-exchange' \
    --data-urlencode "audience=$audience" \
    --data-urlencode 'scope=https://www.googleapis.com/auth/cloud-platform' \
    --data-urlencode 'requested_token_type=urn:ietf:params:oauth:token-type:access_token' \
    --data-urlencode 'subject_token_type=urn:ietf:params:oauth:token-type:jwt' \
    --data-urlencode "subject_token@$token_file"
}

request_sts_with_propagation_retry() {
  local status=unknown attempt
  for attempt in {1..8}; do
    if status="$(request_sts)" && [[ "$status" == 200 ]]; then
      return 0
    fi
    if (( attempt < 8 )); then
      echo "Google STS has not accepted the current WIF trust yet; retry $attempt/8" >&2
      sleep 10
    fi
  done
  echo "Google STS did not accept the authorized ServiceAccount within the propagation window (last HTTP status: $status)" >&2
  return 1
}

if [[ "${WIF_PROBE_MODE:-}" == expect-denied ]]; then
  status="$(request_sts)"
  if [[ "$status" == 200 ]] || ! jq -e '.error != null' /tmp/fullsend-sts.json >/dev/null; then
    echo 'Google STS did not reject the ungranted Kubernetes ServiceAccount' >&2
    exit 1
  fi
  echo 'Untrusted ServiceAccount was rejected by Google STS.'
  exit 0
fi
[[ "${WIF_PROBE_MODE:-}" == authorize-and-rotate ]] || { echo 'Unsupported WIF probe mode' >&2; exit 1; }

get_vertex_response() {
  local federated_token iam_status inference_token vertex_status request
  if ! request_sts_with_propagation_retry; then
    return 1
  fi
  federated_token="$(jq -er '.access_token' /tmp/fullsend-sts.json)"
  printf 'header = "Authorization: Bearer %s"\n' "$federated_token" > /tmp/fullsend-curl-auth
  chmod 600 /tmp/fullsend-curl-auth
  iam_status="$(curl --silent --show-error --max-time 60 --output /tmp/fullsend-iam.json \
    --write-out '%{http_code}' --request POST "$impersonation_url" \
    --config /tmp/fullsend-curl-auth --header 'Content-Type: application/json' \
    --data '{"scope":["https://www.googleapis.com/auth/cloud-platform"]}')"
  unset federated_token
  rm -f /tmp/fullsend-curl-auth
  if [[ "$iam_status" != 200 ]]; then
    echo "Google service-account impersonation failed (HTTP $iam_status)" >&2
    return 1
  fi
  inference_token="$(jq -er '.accessToken' /tmp/fullsend-iam.json)"
  printf 'header = "Authorization: Bearer %s"\n' "$inference_token" > /tmp/fullsend-curl-auth
  chmod 600 /tmp/fullsend-curl-auth
  request="$(jq -cn '{anthropic_version:"vertex-2023-10-16",messages:[{role:"user",content:"Reply with exactly: WIF probe OK"}],max_tokens:32}')"
  vertex_status="$(curl --silent --show-error --max-time 90 --output /tmp/fullsend-vertex.json \
    --write-out '%{http_code}' --request POST \
    "https://aiplatform.googleapis.com/v1/projects/${ANTHROPIC_VERTEX_PROJECT_ID:?Vertex project is required}/locations/global/publishers/anthropic/models/claude-haiku-4-5@20251001:rawPredict" \
    --config /tmp/fullsend-curl-auth --header 'Content-Type: application/json' --data "$request")"
  unset inference_token
  rm -f /tmp/fullsend-curl-auth
  if [[ "$vertex_status" != 200 ]] || ! jq -e '[.content[]? | select(.type == "text") | .text] | any(.[]; length > 0)' /tmp/fullsend-vertex.json >/dev/null; then
    echo "Vertex inference failed (HTTP $vertex_status)" >&2
    return 1
  fi
}

initial_hash="$(sha256sum "$token_file" | cut -d' ' -f1)"
get_vertex_response
echo 'WIF inference succeeded before projected-token rotation.'

deadline=$((SECONDS + 900))
while (( SECONDS < deadline )); do
  current_hash="$(sha256sum "$token_file" | cut -d' ' -f1)"
  if [[ "$current_hash" != "$initial_hash" ]]; then
    get_vertex_response
    echo 'WIF inference succeeded after projected-token rotation.'
    exit 0
  fi
  sleep 10
done
echo 'Projected inference token did not rotate within the probe window' >&2
exit 1
