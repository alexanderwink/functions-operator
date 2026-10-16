# functions-operator

Run unchanged AWS Lambda handlers (Go and .NET Native AOT, `provided.al2023` style `bootstrap`
binaries) on Kubernetes, with scale-to-zero.

You upload a `bootstrap` binary to S3 and create a `Function` resource. The operator turns it into:

- **`type: service`**: a Deployment scaled by the KEDA HTTP add-on (including to and from zero), reachable
  in-cluster at `http://<name>.<namespace>.svc.cluster.local:8080` (or `<name>.<namespace>`, `<name>.<namespace>.svc`;
  the bare `<name>` is not routed, see [Calling a function](#calling-a-function)). Each HTTP request is converted to an
  **API Gateway HTTP API (payload v2.0)** event, the only supported format.
- **`type: job`**: a CronJob that invokes the handler once per schedule with an EventBridge
  *Scheduled Event* (or your own `payload`).

Every pod runs `funcrt`, our implementation of the Lambda Runtime API, which starts `concurrency` copies of
your `bootstrap`. A fetcher init container downloads the binary from S3 and checks it against the exact ETag
the operator resolved, so a pod never runs an unexpected build.

There is no Ingress: external traffic goes through the API gateway (KrakenD).

## Contents

- [Building a bootstrap](#building-a-bootstrap)
- [S3 layout](#s3-layout)
- [Function reference](#function-reference)
- [KrakenD in front of a function](#krakend-in-front-of-a-function)
- [Installing the operator](#installing-the-operator)
- [Local kind workflow](#local-kind-workflow)

## Building a bootstrap

Your function is a single Linux executable named `bootstrap`, exactly as for Lambda's `provided.al2023`
runtime. Handlers use the normal Lambda libraries (`lambda.Start`, `LambdaBootstrapBuilder`); nothing
operator-specific is needed. The runtime image is `amazonlinux:2023-minimal`, so glibc and libstdc++ are
available. Build one binary per architecture you list in `spec.architectures`.

### Go

Build with the `lambda.norpc` tag (the custom-runtime API, without the legacy RPC mode):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags lambda.norpc -o out/linux-amd64/bootstrap .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o out/linux-arm64/bootstrap .
```

See `samples/go-hello` (handler signature taking `events.APIGatewayV2HTTPRequest` and returning
`events.APIGatewayV2HTTPResponse`).

### .NET (Native AOT)

Publish with Native AOT and name the assembly `bootstrap`:

```xml
<PropertyGroup>
  <OutputType>Exe</OutputType>
  <AssemblyName>bootstrap</AssemblyName>
  <PublishAot>true</PublishAot>
  <InvariantGlobalization>true</InvariantGlobalization>
  <StripSymbols>true</StripSymbols>
</PropertyGroup>
```

Use `Amazon.Lambda.RuntimeSupport` (`LambdaBootstrapBuilder`) with a source-generated
`System.Text.Json` serializer, and `APIGatewayHttpApiV2ProxyRequest`/`Response` from
`Amazon.Lambda.APIGatewayEvents`. Native AOT cannot cross-compile between OS/architectures, so build inside a
Linux container of the target architecture:

```sh
podman build --platform linux/amd64 -f Dockerfile.build -t my-fn-build:amd64 .
c=$(podman create --platform linux/amd64 my-fn-build:amd64 /bootstrap)
podman cp "$c":/bootstrap out/linux-amd64/bootstrap && podman rm "$c"
```

**glibc.** A Native AOT binary links against the build machine's glibc and needs at least that version at
run time. The runtime image (AL2023) has glibc **2.34**. Build on an image with glibc <= 2.34, ideally an
AL2023-based build image (for example `public.ecr.aws/amazonlinux/amazonlinux:2023` with the .NET SDK
installed), so the binary cannot depend on newer symbols. The sample's `Dockerfile.build` uses
`mcr.microsoft.com/dotnet/sdk:8.0` (Debian 12, glibc 2.36); it works for the sample, but a handler that pulls
in newer glibc symbols would fail to start with a `GLIBC_2.3x not found` error.

See `samples/dotnet-hello` (`dotnet-hello.csproj`, `Dockerfile.build`).

### Environment your handler sees

The standard Lambda variables are set: `AWS_LAMBDA_RUNTIME_API`, `AWS_LAMBDA_FUNCTION_NAME`,
`AWS_LAMBDA_FUNCTION_VERSION` (the resolved version, or the literal `latest`),
`AWS_LAMBDA_FUNCTION_MEMORY_SIZE`, `_HANDLER`, `LAMBDA_TASK_ROOT`, `AWS_REGION` (`local` unless you set it), plus
your `spec.env`. Invocation deadlines
(`Lambda-Runtime-Deadline-Ms`) are set, so `context` deadlines work. No AWS credentials are injected.
Logs written to stdout/stderr appear unchanged in the pod logs.

## S3 layout

One shared bucket, one top-level folder per function:

```
s3://<bucket>/<key>/<version>/linux-<arch>/bootstrap
s3://<bucket>/<key>/latest/linux-<arch>/bootstrap
```

- `<key>` defaults to the Function's `metadata.name`; `<arch>` is `amd64` or `arm64`.
- **Pinned versions** (`v1`, `v42`, ...) are immutable by convention: never overwrite them. Production pins a
  version; rolling out or back means editing `spec.source.version`.
- **`latest`** is overwritten by your dev pipeline. Functions on `latest` re-check the ETag every 30s
  (operator flag `--latest-poll-interval`) and roll out when it changes.
- Upload every architecture listed in `spec.architectures` for every version. The operator reports
  `SourceResolved=False` (reason `ObjectMissing`) if one is missing, and leaves running workloads unchanged.

Example upload with the MinIO client:

```sh
mc cp out/linux-amd64/bootstrap s3/functions/orders-api/v42/linux-amd64/bootstrap
mc cp out/linux-amd64/bootstrap s3/functions/orders-api/latest/linux-amd64/bootstrap
```

## Function reference

```yaml
apiVersion: functions.pulsen.se/v1alpha1
kind: Function
metadata:
  name: orders-api
  namespace: team-a
spec:
  type: service              # service | job
  source:
    bucket: functions
    key: orders-api          # optional, defaults to metadata.name
    version: v42             # or "latest"
  architectures: [amd64]     # optional, default [amd64]
  env:
    - name: LOG_LEVEL
      value: info
    - name: DB_PASSWORD
      valueFrom:
        secretKeyRef: {name: orders-db, key: password}
  memory: 256Mi              # per bootstrap copy
  timeout: 30s               # per invocation
  concurrency: 4             # bootstrap copies per pod (services only)
  scaling:                   # services only
    min: 0
    max: 10
    scaledownPeriodSeconds: 300
```

| Field | Default | Description |
|---|---|---|
| `type` | required | `service` (HTTP, autoscaled) or `job` (cron). |
| `source.bucket` | required | S3 bucket. |
| `source.key` | `metadata.name` | Function folder in the bucket. One path segment: `^[A-Za-z0-9][A-Za-z0-9._-]*$`, max 128. |
| `source.version` | required | A fixed version or `latest`. One path segment, same pattern as `key`. |
| `architectures` | `[amd64]` | Builds that exist (`amd64`, `arm64`). Pods get node affinity to these; the fetcher downloads the build for the node's architecture. |
| `env` | none | Standard Kubernetes `EnvVar` list (`value` or `valueFrom`), max 256. Reserved names are rejected: anything starting with `FUNCRT_` or `AWS_LAMBDA_`, and `LAMBDA_TASK_ROOT` and `_HANDLER`. |
| `memory` | `128Mi` | Memory per `bootstrap` copy, must be > 0. Pod memory limit and request = `memory × concurrency + 64Mi`. CPU request is derived from memory (Lambda-proportional, 1 vCPU per 1769Mi, at least 50m); there is no CPU limit. |
| `timeout` | `30s` | Execution time per invocation, must be > 0, counted from when a `bootstrap` copy takes the invocation (also the `Lambda-Runtime-Deadline-Ms` the handler sees). A request that exceeds it gets **504** and that copy is killed and restarted. Waiting for a free copy (all `concurrency` copies busy) is limited separately to `timeout`; exceeding that also gives 504 but kills nothing. For jobs, a timeout fails the run. See [Errors](#errors). |
| `concurrency` | `1` | `bootstrap` copies per pod (1-64), each handling one invocation at a time, as in Lambda. Also the KEDA target concurrency per pod. Jobs must use 1. |
| `scaling.min` / `scaling.max` | `0` / `10` | Replica bounds (services only). `min: 0` enables scale to zero. `min <= max`. |
| `scaling.scaledownPeriodSeconds` | `300` | Idle time before scaling down towards `min` (>= 0). |
| `schedule` | none | Cron expression; required for jobs, forbidden for services. |
| `payload` | Scheduled Event | Jobs only: arbitrary JSON sent instead of the EventBridge Scheduled Event. |

Status:

| Field | Meaning |
|---|---|
| `status.resolvedVersion` | Version deployed (`latest` stays `latest`). |
| `status.etags.<arch>` | ETag of each deployed `bootstrap`; every pod fetches exactly this object. |
| `status.conditions` | `SourceResolved` (S3 objects found) and `Ready` (workloads applied). |

`kubectl get fn -A` shows type, version and readiness.

Objects created per service Function: Deployment `<name>`, Service `<name>-backend` (ClusterIP),
HTTPScaledObject `<name>`, and Service `<name>` (ExternalName to the KEDA interceptor; this is the URL to
call). Per job Function: CronJob `<name>` (`concurrencyPolicy: Forbid`, `backoffLimit: 0`: a failed run is
not retried; the next schedule runs as usual). The operator also copies the S3 read Secret
`functions-s3-read` into the Function's namespace for the fetcher.

`metadata.name` must start with a letter (DNS-1035) and be at most 55 characters for services (so
`<name>-backend` fits) and 52 for jobs.

On pod shutdown (rollout, scale-in) `funcrt` keeps serving for 5s so endpoint removal propagates, then
drains in-flight requests for up to `timeout + 2s`; `terminationGracePeriodSeconds` is
`timeout + 8s`.

### Errors

Clients get API Gateway-style generic bodies, never the Lambda error document:

| Situation | Status | Body |
|---|---|---|
| Handler returned an error (`/invocation/error`) | 500 | `{"message":"Internal Server Error"}` |
| `bootstrap` crashed, or returned an invalid response | 502 | `{"message":"Bad Gateway"}` |
| `timeout` exceeded (execution or waiting for a copy) | 504 | `{"message":"Gateway Timeout"}` |

The error document (`errorType`, `errorMessage`) is logged by `funcrt` at warn level with the
`requestId`. A response with your own `statusCode` (for example a 400 or a 500 you return) is passed through
unchanged. For jobs, any of these fails the run and the error document is logged.

### Calling a function

Use a namespace-qualified host: `http://<name>.<namespace>.svc.cluster.local:8080`, `<name>.<namespace>.svc`
or `<name>.<namespace>` (each with or without `:8080`). The KEDA interceptor routes on the `Host` header and
its routing table is shared by all namespaces, so the bare `<name>` (and `<name>:8080`) is deliberately not
registered.

## KrakenD in front of a function

Point a KrakenD backend `host` at the function's in-cluster URL. KrakenD sends that host as the `Host`
header, which the KEDA interceptor routes on.

```json
{
  "endpoint": "/orders/{p}",
  "timeout": "60s",
  "output_encoding": "no-op",
  "input_query_strings": ["*"],
  "input_headers": ["*"],
  "backend": [{
    "host": ["http://orders-api.team-a.svc.cluster.local:8080"],
    "url_pattern": "/{p}",
    "encoding": "no-op"
  }]
}
```

**Set `timeout`.** KrakenD's default endpoint timeout is **2s**, which is shorter than a cold start from
zero replicas. The endpoint `timeout` must be larger than *cold start + `spec.timeout`*. Cold start (pod
scheduling, S3 download, runtime init) is a few seconds; measure it for your function. The KEDA interceptor's
`responseHeaderTimeout` must also be at least that large (the kind setup uses 120s).

Use `no-op` encoding so status codes (including 500/502/504 from the runtime), headers and body pass through
unchanged. Authentication belongs in KrakenD; the runtime does not fill `requestContext.authorizer`, so pass
identity to handlers as headers (for example with `propagate_claims`). A complete example is
`hack/kind/krakend.yaml`.

## Installing the operator

### Prerequisites (not shipped by the operator)

- **KEDA** and the **KEDA HTTP add-on**, installed cluster-wide (tested with KEDA 2.21 and HTTP add-on
  0.16.0). The operator refuses to start if the `HTTPScaledObject` CRD (`http.keda.sh/v1alpha1`) is missing.
  Set the interceptor's `interceptor.responseHeaderTimeout` (Helm value) above your largest
  cold start + function timeout, and `interceptor.readinessTimeout` (how long the interceptor holds a
  request while a function scales from zero) above your largest cold start (the kind setup uses 120s and
  60s).
- **S3-compatible storage** with the bucket created.
- A Secret **`functions-s3-read`** in the operator namespace (default `functions-operator-system`) with keys
  `endpoint` (host:port, no scheme), `accessKey`, `secretKey` and `useSSL` (`true`/`false`). One read-only
  credential for the bucket is enough; it is copied into each Function namespace for the fetcher.

```sh
kubectl -n functions-operator-system create secret generic functions-s3-read \
  --from-literal=endpoint=s3.example.internal:9000 --from-literal=useSSL=true \
  --from-literal=accessKey=... --from-literal=secretKey=...
```

### Deploy

Build and push the operator image and the multi-arch runtime image, then deploy:

```sh
make docker-build docker-push IMG=<registry>/functions-operator:<tag>
make runtime-image RUNTIME_IMG=<registry>/functions-runtime:<tag> && podman manifest push <registry>/functions-runtime:<tag>
make deploy IMG=<registry>/functions-operator:<tag> RUNTIME_IMG=<registry>/functions-runtime:<tag>
```

The manager requires `--runtime-image`; `config/default` sets it to the placeholder `RUNTIME_IMG`, which
`make deploy` replaces with `$(RUNTIME_IMG)` (substitute it yourself if you build the overlay by hand).
Flags:

| Flag | Default | |
|---|---|---|
| `--runtime-image` | required | Image with `funcrt` and the fetcher, used for every function pod. |
| `--interceptor-host` | `keda-add-ons-http-interceptor-proxy.keda.svc.cluster.local` | Target of each function's ExternalName Service. |
| `--s3-secret-namespace` | operator namespace | Where `functions-s3-read` lives. |
| `--latest-poll-interval` | `30s` | ETag poll interval for `latest`. |

## Local kind workflow

Requirements: podman (kind runs with `KIND_EXPERIMENTAL_PROVIDER=podman`), kind >= 0.20, kubectl, helm, Go.
If an older kind is first on your `PATH`, pass `KIND=/path/to/kind` to every make target, e.g.
`KIND=/opt/homebrew/bin/kind`.

```sh
make kind-up KIND=/opt/homebrew/bin/kind          # cluster "functions" + KEDA, HTTP add-on, MinIO
make deploy-kind KIND=/opt/homebrew/bin/kind      # images, operator, S3 secret, samples (ns team-a), KrakenD (ns gateway)
make upload-samples                               # bootstrap binaries to MinIO (builds them if needed)
make e2e                                          # end-to-end checks
make kind-down KIND=/opt/homebrew/bin/kind        # delete the cluster
```

What the targets do:

- `kind-up`: creates the kind cluster `functions` and installs KEDA, the HTTP add-on (with
  `responseHeaderTimeout=120s`) and MinIO (`minio.minio.svc.cluster.local:9000`, `minioadmin`/`minioadmin`,
  bucket `functions`).
- `deploy-kind`: builds the operator, runtime and `hey` images for the native architecture, loads them into
  kind (`podman save` + `kind load image-archive`), creates the `functions-s3-read` Secret, deploys the
  operator with `config/kind` and applies `config/samples` into `team-a` and `hack/kind/krakend.yaml`.
- `samples` (`make samples BUILD=<id>`): builds `go-hello` and `dotnet-hello` for amd64 and arm64 into
  `samples/out/<fn>/linux-<arch>/bootstrap`. `BUILD` is stamped into the binaries and returned in responses.
- `upload-samples`: uploads them to `functions/<fn>/v1/...` and `functions/<fn>/latest/...`
  (`SAMPLE_VERSION=v2` for another version).

The samples use `architectures: [arm64]` because the kind node on Apple silicon is arm64; change it to
`amd64` on an x86 machine.

| Sample | Shows |
|---|---|
| `go-hello` | Go service, `concurrency: 4`, scale 0-5, `scaledownPeriodSeconds: 30` |
| `dotnet-hello` | .NET Native AOT service |
| `go-hello-job` | Job, `*/1 * * * *`, logs `scheduled tick` |
| `go-hello-dev` | Follows `latest` |
| `go-hello-timeout` | `timeout: 2s` (`?sleep=5s` gives 504) |

Service DNS is not reachable from the host, so call functions from a pod:

```sh
kubectl -n team-a run curl --rm -it --restart=Never --image=docker.io/curlimages/curl:8.22.0 -- \
  curl -s http://krakend.gateway.svc.cluster.local:8080/go/hello
```

### End-to-end suite

`test/e2e/e2e.sh` (`make e2e`, alias `make test-e2e`) runs in-cluster (a curl pod and a `hey` Job in namespace `e2e`) and checks:

1. `go-hello` and `dotnet-hello` are `Ready=True` and scaled to zero.
2. Requests through KrakenD (`/go/x`, `/dotnet/x`) cold-start from zero and return 200.
3. `hey -z 20s -c 40` against `go-hello` scales it above one replica, and it returns to zero within 2 minutes.
4. The job sample succeeds within 2.5 minutes and logs `scheduled tick`.
5. Changing `go-hello` from `v1` to `v2` serves `"version":"v2"`.
6. Overwriting `latest` updates `go-hello-dev`'s `status.etags` within 60s and the new build is served.
7. `go-hello-timeout` with `?sleep=5s` returns 504, and the next request succeeds.
8. With `go-hello-dev` at zero replicas, overwriting `latest` and requesting at once through KrakenD
   (`/dev/x`) returns 200 with the new build within the KrakenD timeout.
9. Changing `go-hello` from `v1` to `v2` while `hey` loads it: every response is 200.

It prints `PASS`/`FAIL` per check and exits non-zero on failure. It is re-runnable: it re-applies the samples
(resetting `go-hello` to `v1`) and recreates its own pods.

The `hey` image is built locally from `rakyll/hey` v0.1.5 (`hack/kind/hey/Dockerfile`) because the
published `hey` images are amd64-only.

## Development

```sh
make test        # unit tests + envtest (API validation, controller)
make lint
make manifests generate
```

## License

Copyright 2026. Licensed under the Apache License, Version 2.0.
