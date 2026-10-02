#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
"$K8S_ROOT/kind/00-preflight.sh" --profile "$PROFILE"
"$K8S_ROOT/kind/02-images.sh" --build-only --profile "$PROFILE"
if cluster_exists; then "$K8S_ROOT/kind/destroy.sh" --profile "$PROFILE"; fi
"$K8S_ROOT/kind/deploy.sh" --profile "$PROFILE"
