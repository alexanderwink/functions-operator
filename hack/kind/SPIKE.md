# Interceptor spike results

Environment: kind (node Kubernetes 1.32, podman), KEDA chart `kedacore/keda` 2.21.0, HTTP add-on chart
`kedacore/keda-add-ons-http` 0.16.0 (app 0.16.0). Manifest: `hack/kind/spike-echo.yaml`.

## (a) Host matching
The interceptor matched **both** forms. Tests with a request sent directly to the interceptor proxy
with an explicit `Host` header, for each HTTPScaledObject `hosts` setting:

| `spec.hosts`                          | `Host: echo.spike.svc.cluster.local` | `Host: ...:8080` |
|---------------------------------------|-----|-----|
| both forms                            | 200 | 200 |
| no-port only                          | 200 | 200 |
| `:8080` only                          | 200 | 200 |
| `other.example` (neither)             | 404 | 404 |

So the port is effectively ignored when matching, in either direction. A real request via
`http://echo.spike.svc.cluster.local:8080/hi` sends `Host: echo.spike.svc.cluster.local:8080` and routed
correctly. Later tasks should still register both host forms (harmless, and robust against other versions).
An unknown host returns 404.

## (b) HTTPScaledObject (installed version)
- `apiVersion: http.keda.sh/v1alpha1`, `kind: HTTPScaledObject` (only version served: `v1alpha1`).
- Top-level spec fields: `hosts` ([]string), `pathPrefixes`, `headers`, `scaleTargetRef`, `replicas`,
  `scaledownPeriod` (int32 seconds), `initialCooldownPeriod`, `scalingMetric`, `timeouts`
  (`conditionWait`, `responseHeader`), `coldStartTimeoutFailoverRef`, `targetPendingRequests` (deprecated).
- `scaleTargetRef`: `name`, `kind`, `apiVersion` (workload reference), plus `service` (required) and one of
  `port` / `portName` (exactly one, CEL-validated).
- `replicas`: `min`, `max` (defaults 0 / 100).
- `scalingMetric`: one of `concurrency.targetValue` (default 100) or `requestRate.{targetValue,granularity,window}`.
  Spike used `concurrency.targetValue: 10`.
- `scaledownPeriod: 30` was accepted.

## (c) Interceptor proxy service
`keda-add-ons-http-interceptor-proxy.keda` namespace `keda`, ClusterIP port **8080**
(FQDN `keda-add-ons-http-interceptor-proxy.keda.svc.cluster.local`). Admin service is
`keda-add-ons-http-interceptor-admin:9090`. An ExternalName Service `echo` -> proxy FQDN works.

## (d) Cold start
Deployment at 0 replicas, then `curl http://echo.spike.svc.cluster.local:8080/hi`: **200** in 6.8 s on the
first run (image pull included) and 2.1 s on a second run with the image cached. HTTPScaledObject with min 0
scaled the deployment to 0 within seconds of creation.

## Chart value names (0.16.0)
`interceptor.responseHeaderTimeout` and `interceptor.readinessTimeout` (replaces the deprecated
`interceptor.replicas.waitTimeout`). Verified in the interceptor env: `KEDA_HTTP_RESPONSE_HEADER_TIMEOUT=120s`,
`KEDA_HTTP_READINESS_TIMEOUT=60s`.

## Environment notes
- KEDA 2.21 warns that K8s 1.32 is below its tested minimum (1.34); it works for our purposes.
- MinIO: `quay.io/minio/*` and `minio/*` Docker Hub images are no longer pullable, so the `minio/minio`
  chart cannot run. `hack/kind/minio.yaml` deploys the last `bitnamilegacy/minio` image (2025.5.24) instead,
  same endpoint, credentials and bucket.
- An old `kind` v0.11 is first on PATH here; scripts prepend `/opt/homebrew/bin` to use kind 0.26.
