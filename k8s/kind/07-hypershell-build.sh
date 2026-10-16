#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
script_start "$0"

readonly HYPERSHELL_HSCTL_COMMIT=c17b8199916b0ee5b5a255d9928b3bd051efdbc9
source_dir="$LOCAL_DIR/hsctl-src"
hsctl_bin="$LOCAL_DIR/bin/hsctl"
build_marker="$hsctl_bin.commit"

for tool in git go; do command -v "$tool" >/dev/null || fail "Missing $tool"; done
mkdir -p "$(dirname "$hsctl_bin")"

if [[ -x "$hsctl_bin" && -s "$build_marker" ]] && [[ "$(cat "$build_marker")" == "$HYPERSHELL_HSCTL_COMMIT" ]]; then
  status '✅' "Pinned hsctl is already built at $hsctl_bin"
  exit 0
fi

if [[ ! -d "$source_dir/.git" ]]; then
  status '📥' 'Fetching pinned HyperShell source for hsctl'
  git clone --filter=blob:none --sparse https://github.com/openshift-online/hypershell.git "$source_dir"
  git -C "$source_dir" sparse-checkout set components/cli
fi

status '🔎' 'Checking out the pinned HyperShell CLI source revision'
git -C "$source_dir" fetch origin "$HYPERSHELL_HSCTL_COMMIT"
git -C "$source_dir" checkout --detach "$HYPERSHELL_HSCTL_COMMIT"
[[ "$(git -C "$source_dir" rev-parse HEAD)" == "$HYPERSHELL_HSCTL_COMMIT" ]] || fail 'hsctl source revision mismatch'

status '🔧' 'Building pinned hsctl'
tmp_bin="$hsctl_bin.tmp.$$"
trap 'rm -f "$tmp_bin"' EXIT
(cd "$source_dir/components/cli" && go build -o "$tmp_bin" ./cmd/hsctl)
chmod 755 "$tmp_bin"
mv -f "$tmp_bin" "$hsctl_bin"
printf '%s\n' "$HYPERSHELL_HSCTL_COMMIT" > "$build_marker"
trap - EXIT
status '✅' "Pinned hsctl is ready at $hsctl_bin"
