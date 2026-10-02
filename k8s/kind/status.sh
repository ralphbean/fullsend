#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
profile_arg "$@"
require_owned
k -n fullsend-control get statefulset,cronjob,job,pod
k -n fullsend-runner get job,pod
