#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"
build_only=0
if [[ "${1:-}" == --build-only ]]; then build_only=1; shift; fi
profile_arg "$@"
status '🔧' 'Building control-plane binary from this checkout'
revision="$(git -C "$REPO_ROOT" rev-parse HEAD)"
content_hash="$(cd "$REPO_ROOT" && { git diff HEAD --binary; git ls-files --others --exclude-standard | sort | xargs -r sha256sum; } | sha256sum | cut -c1-16)"
tag="${revision:0:12}-${content_hash}"
printf '%s\n' "$tag" > "$STATE_DIR/image-tag"
printf 'revision=%s\ncontent_hash=%s\nimage_tag=%s\n' "$revision" "$content_hash" "$tag" > "$STATE_DIR/build.env"
(cd "$REPO_ROOT" && CGO_ENABLED=0 go build -trimpath -o "$LOCAL_DIR/fullsend-poc" ./cmd/fullsend)
mkdir -p "$LOCAL_DIR/image-context"
cp "$LOCAL_DIR/fullsend-poc" "$LOCAL_DIR/image-context/fullsend"
podman build --build-arg "CONTROL_BASE_IMAGE=$CONTROL_BASE_IMAGE" -f "$K8S_ROOT/Containerfile" -t "localhost/fullsend-poc:$tag" "$LOCAL_DIR/image-context"
for image in "$STUB_IMAGE" "$POSTGRES_IMAGE" "$KIND_NODE_IMAGE"; do
  if ! podman image exists "$image"; then podman pull "$image" >/dev/null; fi
done
if [[ "$build_only" == 0 ]]; then
  require_owned
  # kind v0.23 with Podman 6 aliases every tag to the first image when
  # multiple images are passed in one load call. Load archives separately.
  for image in "localhost/fullsend-poc:$tag" "$STUB_IMAGE" "$POSTGRES_IMAGE"; do
    kind load docker-image "$image" --name "$KIND_NAME"
  done
fi
status '✅' "Images ready ($tag)"
