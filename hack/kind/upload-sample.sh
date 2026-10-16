#!/usr/bin/env bash
# Upload a sample function's bootstrap binaries to the local kind MinIO.
#   usage: upload-sample.sh <name> <version> <dir>
# <dir> contains linux-<arch>/bootstrap; each is uploaded to
#   functions/<name>/{<version>,latest}/linux-<arch>/bootstrap  (only latest when <version> is "latest")
# Approach: a short-lived pod in the cluster (image bitnamilegacy/minio, which bundles `mc`) with the
# binaries streamed in via `kubectl cp`. No port-forward or host networking needed, so it works the same
# on podman/macOS and Linux.
set -euo pipefail
[ $# -eq 3 ] || { echo "usage: $0 <name> <version> <dir>" >&2; exit 2; }
NAME=$1 VERSION=$2 DIR=$3
CTX="${KUBE_CONTEXT:-kind-functions}"
IMAGE="${MC_IMAGE:-docker.io/bitnamilegacy/minio:2025.5.24-debian-12-r1}"
POD="mc-upload-$$"
K=(kubectl --context "$CTX" -n minio)

shopt -s nullglob
dirs=("$DIR"/linux-*/bootstrap)
[ ${#dirs[@]} -gt 0 ] || { echo "no linux-*/bootstrap under $DIR" >&2; exit 1; }

cleanup() { "${K[@]}" delete pod "$POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT

"${K[@]}" run "$POD" --image="$IMAGE" --restart=Never --command -- sleep 600 >/dev/null
"${K[@]}" wait --for=condition=Ready pod/"$POD" --timeout=120s >/dev/null

"${K[@]}" exec "$POD" -- mc alias set local http://minio.minio.svc.cluster.local:9000 minioadmin minioadmin >/dev/null
"${K[@]}" exec "$POD" -- mc mb --ignore-existing local/functions >/dev/null
for f in "${dirs[@]}"; do
  arch=$(basename "$(dirname "$f")")
  # kubectl cp needs tar in the image; stream through exec instead.
  "${K[@]}" exec -i "$POD" -- sh -c "cat > /tmp/bootstrap-$arch" < "$f"
  vers=("$VERSION"); [ "$VERSION" = latest ] || vers+=(latest)
  for ver in "${vers[@]}"; do
    key="local/functions/$NAME/$ver/$arch/bootstrap"
    "${K[@]}" exec "$POD" -- mc cp "/tmp/bootstrap-$arch" "$key" >/dev/null
    echo "uploaded $key"
  done
done
"${K[@]}" exec "$POD" -- mc ls --recursive "local/functions/$NAME/"
