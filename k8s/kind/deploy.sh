#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
"$K8S_ROOT/kind/00-preflight.sh" --profile "$PROFILE"
"$K8S_ROOT/kind/01-kind.sh" --profile "$PROFILE"
"$K8S_ROOT/kind/02-images.sh" --profile "$PROFILE"
"$K8S_ROOT/kind/03-config.sh" --profile "$PROFILE"
"$K8S_ROOT/kind/04-postgres.sh" --profile "$PROFILE"
if [[ "$PROFILE" == live ]]; then
  "$K8S_ROOT/kind/05-live-workloads.sh" --profile live
  "$K8S_ROOT/kind/06-live-smoke.sh" --profile live
else
  "$K8S_ROOT/kind/05-workloads.sh" --profile fixture
  "$K8S_ROOT/kind/06-smoke.sh" --profile fixture
fi
