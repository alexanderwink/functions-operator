#!/usr/bin/env bash
# Create the local kind cluster "functions" with KEDA, the KEDA HTTP add-on and MinIO.
set -euo pipefail
export KIND_EXPERIMENTAL_PROVIDER=podman
KIND="${KIND:-kind}"
# Require kind >= 0.20 (older versions pick an outdated node image). Override with KIND=/path/to/kind.
kind_ver="$("$KIND" version 2>/dev/null | sed -nE 's/^kind v([0-9]+)\.([0-9]+).*/\1 \2/p')" || true
read -r kmaj kmin <<<"${kind_ver:-0 0}"
if [ "$kmaj" -eq 0 ] && [ "$kmin" -lt 20 ]; then
  echo "error: '$KIND' is kind ${kmaj}.${kmin}; need >= 0.20. Set KIND=/path/to/newer/kind." >&2
  exit 1
fi

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KEDA_CHART_VERSION="${KEDA_CHART_VERSION:-2.21.0}"
HTTP_ADDON_CHART_VERSION="${HTTP_ADDON_CHART_VERSION:-0.16.0}"
CTX=kind-functions

if ! "$KIND" get clusters 2>/dev/null | grep -qx functions; then
  "$KIND" create cluster --config "$DIR/cluster.yaml"
fi
kubectl config use-context "$CTX" >/dev/null

helm repo add kedacore https://kedacore.github.io/charts >/dev/null
helm repo update >/dev/null

helm upgrade --install keda kedacore/keda --version "$KEDA_CHART_VERSION" \
  -n keda --create-namespace --kube-context "$CTX" --wait --timeout 5m

# readinessTimeout replaces the deprecated replicas.waitTimeout in chart 0.16.0.
helm upgrade --install http-add-on kedacore/keda-add-ons-http --version "$HTTP_ADDON_CHART_VERSION" \
  -n keda --kube-context "$CTX" \
  --set interceptor.responseHeaderTimeout=120s \
  --set interceptor.readinessTimeout=60s \
  --wait --timeout 5m

# MinIO: see minio.yaml for why this is a plain manifest rather than the minio/minio chart.
kubectl --context "$CTX" apply -f "$DIR/minio.yaml"
kubectl --context "$CTX" -n minio rollout status deploy/minio --timeout=5m

# Create bucket "functions" idempotently with mc in a one-off pod (same image bundles mc).
kubectl --context "$CTX" -n minio delete pod mc-make-bucket --ignore-not-found >/dev/null
kubectl --context "$CTX" -n minio run mc-make-bucket --image=docker.io/bitnamilegacy/minio:2025.5.24-debian-12-r1 \
  --restart=Never --rm -i --command -- \
  sh -c 'mc alias set local http://minio.minio.svc.cluster.local:9000 minioadmin minioadmin && mc mb --ignore-existing local/functions'

echo "kind cluster 'functions' is ready."
