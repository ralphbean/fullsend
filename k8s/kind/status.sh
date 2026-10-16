#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
require_owned
echo "# Control Plane"
k -n fullsend-control get statefulset,cronjob,job,pod
echo
k -n fullsend-mint get deployment
echo
echo "# Data Plane"
k -n fullsend-runner get job,pod
