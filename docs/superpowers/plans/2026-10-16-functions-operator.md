# Functions Operator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Kubernetes operator that runs unmodified AWS Lambda `bootstrap` binaries (Go and .NET Native AOT) fetched from S3, as autoscaled HTTP services (scale‑to‑zero via KEDA HTTP add‑on) or as cron jobs.

**Architecture:** A `Function` CRD is reconciled by a kubebuilder controller into Deployment + Services + `HTTPScaledObject` (service) or a CronJob (job). Every pod runs one shared multi‑arch image: a `fetcher` init container downloads the ETag‑pinned binary into an `emptyDir`, and `funcrt` (our replacement for RIE) implements the Lambda Runtime API, runs N `bootstrap` copies, and translates HTTP ↔ API Gateway v2 events.

**Tech Stack:** Go 1.24, kubebuilder v4 / controller‑runtime, `github.com/aws/aws-lambda-go` (events types, test handlers), `github.com/minio/minio-go/v7` (S3), `github.com/prometheus/client_golang`, KEDA + KEDA HTTP add‑on (Helm), MinIO, KrakenD, kind (podman provider), .NET 8 Native AOT for the sample.

**Spec:** `docs/superpowers/specs/2026-10-16-functions-operator-design.md`

## Global Constraints

- Go module: `github.com/alexanderwink/functions-operator`; remote `https://github.com/alexanderwink/functions-operator.git` (gh account `alexanderwink`).
- API: group `functions.pulsen.se`, version `v1alpha1`, kind `Function`, namespaced.
- S3 object path: `<bucket>/<key>/<version>/linux-<arch>/bootstrap`; `key` defaults to `metadata.name`; `version` is a fixed string or `latest`.
- `spec.architectures` default `[amd64]`; allowed values `amd64`, `arm64`.
- HTTP event format: **API Gateway HTTP API payload v2.0 only** (`events.APIGatewayV2HTTPRequest` / `APIGatewayV2HTTPResponse`).
- Job event: EventBridge Scheduled Event shape unless `spec.payload` is set.
- In‑cluster URL: `http://<name>.<ns>.svc.cluster.local:8080` (ExternalName → `keda-add-ons-http-interceptor-proxy.keda.svc.cluster.local`).
- `memory` is per bootstrap copy; pod memory limit = `memory × concurrency + 64Mi`.
- `latest` ETag poll interval default `30s` (operator flag `--latest-poll-interval`).
- Timeout exceeded → 504 and that copy is killed and restarted; copy crash mid‑request → 502.
- Base image `amazonlinux:2023-minimal`, built for `linux/amd64` and `linux/arm64`.
- No Ingress. No AWS credentials injected. KEDA/HTTP add‑on are prerequisites, not shipped by the operator.
- Local container tool is **podman** (`docker` resolves to podman, kind uses `KIND_EXPERIMENTAL_PROVIDER=podman`). Load images into kind with `podman save … | kind load image-archive /dev/stdin` style (via a temp tar), not `kind load docker-image`.
- Directory is not yet a git repo; Task 1 runs `git init`.

## Review Focus

1. **More concurrent requests than `concurrency` in one pod** → requests queue in `funcrt` and are served in order, failing with 504 only if their own deadline passes while queued. Test: `TestPoolQueuesBeyondConcurrency` (Task 2).
2. **Binary request/response bodies** (images, protobuf) → base64 round‑trips correctly in both directions. Test: `TestToEventBinaryBody`, `TestWriteResponseBase64` (Task 4).
3. **Handler returns an error or panics** → caller gets 502 with the Lambda error JSON, the copy keeps serving (aws-lambda-go reports via `/error` and keeps polling). Test: `TestPoolHandlerError` (Task 2).
4. **Listed architecture missing in S3 / bucket unreachable** → `SourceResolved=False` with a reason, the existing Deployment/CronJob is left untouched (no broken rollout). Test: `TestReconcileSourceMissingKeepsDeployment` (Task 10).
5. **`spec.type` changed service → job (or back)** → objects of the old type are deleted. Test: `TestReconcileTypeSwitchCleansUp` (Task 10).

---

## File Structure

```
cmd/main.go                          # operator (kubebuilder scaffold)
cmd/funcrt/main.go                   # runtime entrypoint
cmd/fetcher/main.go                  # init container entrypoint
api/v1alpha1/function_types.go       # CRD types + validation markers
internal/funcrt/config.go            # env → Config
internal/funcrt/pool.go              # invocation queue, Pool API
internal/funcrt/worker.go            # one bootstrap process + its Runtime API listener
internal/funcrt/apigw.go             # HTTP ↔ APIGW v2 event conversion
internal/funcrt/server.go            # service mode HTTP server, health, metrics
internal/funcrt/job.go               # job mode + scheduled event
internal/funcrt/testdata/echo/main.go# lambda.Start test handler (behaviour driven by payload)
internal/fetcher/fetcher.go          # S3 download with If-Match ETag
internal/source/resolver.go          # S3 HEAD per arch → Resolved
internal/resources/podtemplate.go    # shared pod template (fetcher + funcrt)
internal/resources/service.go        # Deployment, backend Service, ExternalName Service, HTTPScaledObject
internal/resources/job.go            # CronJob
internal/controller/function_controller.go
internal/controller/s3secret.go      # copy S3 read secret into function namespace
images/runtime/Dockerfile            # funcrt + fetcher, multi-arch
samples/go-hello/                    # Go lambda.Start sample
samples/dotnet-hello/                # .NET 8 Native AOT sample + Dockerfile.build
hack/kind/                           # cluster config, install + upload scripts, krakend config
test/e2e/e2e.sh                      # end-to-end assertions against kind
```

---

### Task 1: Scaffold + local kind environment + interceptor spike

**Files:**
- Create: kubebuilder scaffold (`go.mod`, `cmd/main.go`, `api/v1alpha1/`, `internal/controller/`, `config/`, `Makefile`, `Dockerfile`)
- Create: `hack/kind/cluster.yaml`, `hack/kind/up.sh`, `hack/kind/down.sh`, `hack/kind/spike-echo.yaml`
- Create: `.gitignore`

**Interfaces:**
- Produces: `make kind-up` (cluster `functions` + KEDA + HTTP add‑on in namespace `keda` + MinIO in namespace `minio` with bucket `functions`, root creds `minioadmin/minioadmin`, endpoint `minio.minio.svc.cluster.local:9000`), `make kind-down`, `make kind-load IMG=<image>`.

- [ ] **Step 1:** `git init`; install kubebuilder v4 (`brew install kubebuilder`); `kubebuilder init --domain pulsen.se --repo github.com/alexanderwink/functions-operator`; `kubebuilder create api --group functions --version v1alpha1 --kind Function --resource --controller`. Expected: `make build` succeeds.
- [ ] **Step 2:** Write `hack/kind/up.sh`: create kind cluster from `cluster.yaml` (1 node), `helm install keda kedacore/keda -n keda --create-namespace`, `helm install http-add-on kedacore/keda-add-ons-http -n keda` with interceptor values `responseHeaderTimeout=120s` and `replicas.waitTimeout=60s` (check the chart's actual value names for the pinned chart version and pin that version in the script), MinIO via the `minio/minio` Helm chart in standalone mode, then create bucket `functions` with `mc` in a one‑off pod. Add Makefile targets `kind-up`, `kind-down`, `kind-load`.
- [ ] **Step 3: Spike.** `spike-echo.yaml`: an `ealen/echo-server` Deployment+Service `echo-backend` in ns `spike`, an `HTTPScaledObject` with `hosts: [echo.spike.svc.cluster.local, echo.spike.svc.cluster.local:8080]`, min 0, and an ExternalName Service `echo` → interceptor. From a curl pod: `curl -s http://echo.spike.svc.cluster.local:8080/hi`.
  Expected: 200 after cold start with deployment previously at 0 replicas. Record in `hack/kind/SPIKE.md` which `Host` forms the interceptor matched (with/without port) and the HTTPScaledObject apiVersion/field names of the installed version. Later tasks register both host forms regardless.
- [ ] **Step 4: Commit** `chore: scaffold operator and kind environment`.

---

### Task 2: funcrt Runtime API pool

**Files:**
- Create: `internal/funcrt/pool.go`, `internal/funcrt/worker.go`, `internal/funcrt/testdata/echo/main.go`
- Test: `internal/funcrt/pool_test.go`

**Interfaces:**
- Produces:
  ```go
  type PoolConfig struct {
      Bootstrap    string            // path to bootstrap binary
      Concurrency  int
      Timeout      time.Duration
      BasePort     int               // worker i listens on 127.0.0.1:BasePort+i
      Env          []string          // extra env for bootstrap (user env + AWS_LAMBDA_*)
      FunctionName string
  }
  type Result struct {
      Status  int    // 200 ok, 502 handler error/crash, 504 timeout
      Payload []byte // response payload, or Lambda error JSON for 502
  }
  func NewPool(cfg PoolConfig) *Pool
  func (p *Pool) Start(ctx context.Context) error           // starts workers, returns immediately
  func (p *Pool) Invoke(ctx context.Context, payload []byte) Result
  func (p *Pool) Ready() bool                              // ≥1 worker has polled /next
  func (p *Pool) InFlight() int
  func (p *Pool) Queued() int
  ```
- Each worker sets `AWS_LAMBDA_RUNTIME_API=127.0.0.1:<BasePort+i>` and serves `GET /2018-06-01/runtime/invocation/next`, `POST …/invocation/{id}/response`, `POST …/invocation/{id}/error`, `POST /2018-06-01/runtime/init/error`. `/next` response headers: `Lambda-Runtime-Aws-Request-Id`, `Lambda-Runtime-Deadline-Ms` (unix ms), `Lambda-Runtime-Invoked-Function-Arn` = `arn:aws:lambda:local:000000000000:function:<FunctionName>`.
- Invocations go through one shared unbuffered queue; any idle worker's `/next` takes the next one. The deadline is set at `Invoke` time (so queued time counts against the timeout).

- [ ] **Step 1: Test handler.** `testdata/echo/main.go` uses `lambda.Start` with `json.RawMessage` input: `{"echo":X}` → returns X; `{"sleep":"2s"}` → sleeps; `{"error":"boom"}` → returns `errors.New("boom")`; `{"exit":1}` → `os.Exit(1)`; env `ECHO_INIT_FAIL=1` → `os.Exit(2)` before `lambda.Start`. `TestMain` builds it with `go build -tags lambda.norpc -o $TMP/bootstrap`.
- [ ] **Step 2: Write failing tests.**
  ```go
  func TestPoolEcho(t *testing.T)                 // Invoke({"echo":"hi"}) → Status 200, Payload `"hi"`
  func TestPoolQueuesBeyondConcurrency(t *testing.T) // Concurrency 2, 6 parallel {"sleep":"200ms"}, Timeout 5s → all 200, wall time ≥ 600ms
  func TestPoolHandlerError(t *testing.T)         // {"error":"boom"} → 502, payload contains "boom"; next Invoke({"echo":1}) → 200
  func TestPoolReady(t *testing.T)                // Ready() false before Start, eventually true after
  ```
- [ ] **Step 3:** Run `go test ./internal/funcrt/ -run TestPool -v` → FAIL (undefined).
- [ ] **Step 4:** Implement `pool.go`/`worker.go`.
- [ ] **Step 5:** Run → PASS. Commit `feat(funcrt): lambda runtime API worker pool`.

---

### Task 3: funcrt failure handling — timeout, crash, init error

**Files:**
- Modify: `internal/funcrt/worker.go`, `internal/funcrt/pool.go`
- Test: `internal/funcrt/pool_failure_test.go`

**Interfaces:**
- Consumes: Task 2 `Pool`.
- Behaviour: on deadline the worker sends `Result{504}` and kills its process (SIGKILL on the process group), then restarts. If the process exits with an invocation in flight → `Result{502, {"errorType":"Runtime.ExitError",...}}`, restart. Restarts back off 100ms → doubling → max 10s, reset after a successful invocation. `/init/error` or exit before first `/next` counts as an init failure; the worker restarts with backoff and does not count toward `Ready()`.

- [ ] **Step 1: Failing tests.**
  ```go
  func TestPoolTimeoutKillsAndRecovers(t *testing.T) // Timeout 300ms, {"sleep":"5s"} → 504 within <1s; then {"echo":1} → 200
  func TestPoolCrashReturns502(t *testing.T)         // {"exit":1} → 502, errorType "Runtime.ExitError"; then {"echo":1} → 200
  func TestPoolInitFailureNotReady(t *testing.T)     // Env ECHO_INIT_FAIL=1 → Ready() stays false for 1s
  func TestPoolQueuedRequestTimesOut(t *testing.T)   // Concurrency 1, Timeout 300ms, first {"sleep":"5s"}, second {"echo":1} queued → second 504
  ```
- [ ] **Step 2:** Run → FAIL. **Step 3:** Implement. **Step 4:** Run `go test ./internal/funcrt/ -v -race` → PASS.
- [ ] **Step 5:** Commit `feat(funcrt): timeouts, crash and init failure handling`.

---

### Task 4: HTTP ↔ API Gateway v2 conversion + service server

**Files:**
- Create: `internal/funcrt/apigw.go`, `internal/funcrt/server.go`
- Test: `internal/funcrt/apigw_test.go`, `internal/funcrt/server_test.go`

**Interfaces:**
- Produces:
  ```go
  func ToEvent(r *http.Request, requestID string, now time.Time) (events.APIGatewayV2HTTPRequest, error)
  func WriteResponse(w http.ResponseWriter, res Result)
  func NewServer(p *Pool, port, adminPort int) *Server   // port 8080: invocations; adminPort 9090: /healthz, /readyz, /metrics
  func (s *Server) Run(ctx context.Context) error
  ```
- `ToEvent`: `version "2.0"`, `routeKey "$default"`, `rawPath`, `rawQueryString`, `cookies` from `Cookie`, headers lower‑cased with multi‑values joined by `,`, `queryStringParameters` joined by `,`, `requestContext.http{method,path,protocol,sourceIp,userAgent}`, `requestContext.requestId`, `timeEpoch`, `stage "$default"`, `domainName` = Host. Body base64 when `Content-Type` is not text (`text/*`, `application/json`, `application/xml`, `application/x-www-form-urlencoded`, `*+json`, `*+xml`) or not valid UTF‑8.
- `WriteResponse`: 200 + JSON → if payload is a JSON object containing `statusCode`, decode as `APIGatewayV2HTTPResponse` (headers, multiValueHeaders, cookies → `Set-Cookie`, base64 body); otherwise (v2 "simple response") write payload as body with status 200 and `Content-Type: application/json`. 502/504 → that status with payload as JSON body.
- Metrics: `funcrt_inflight`, `funcrt_queued` (gauges), `funcrt_invocations_total{status}`, `funcrt_invocation_duration_seconds` (histogram). `/readyz` = `Pool.Ready()`.

- [ ] **Step 1: Failing tests.**
  ```go
  func TestToEventBasic(t *testing.T)        // GET /orders/7?a=1&a=2, header X-Foo → RawPath "/orders/7", QueryStringParameters["a"]=="1,2", Headers["x-foo"], RequestContext.HTTP.Method "GET", Version "2.0"
  func TestToEventBinaryBody(t *testing.T)   // POST image/png bytes{0xff,0x00} → IsBase64Encoded true, decoded body equal
  func TestToEventJSONBody(t *testing.T)     // application/json → IsBase64Encoded false, Body verbatim
  func TestWriteResponseStructured(t *testing.T) // {"statusCode":201,"headers":{"x-a":"b"},"cookies":["c=1"],"body":"ok"} → 201, X-A b, Set-Cookie c=1, body "ok"
  func TestWriteResponseBase64(t *testing.T) // isBase64Encoded true → raw bytes written
  func TestWriteResponseSimple(t *testing.T) // payload `{"hello":"x"}` (no statusCode) → 200, application/json, body verbatim
  func TestServerEndToEnd(t *testing.T)      // echo handler returning the event's rawPath; GET /p → 200 body contains "/p"; /readyz 200 after start
  ```
  Extend `testdata/echo`: when input has `"rawPath"`, return `APIGatewayV2HTTPResponse{StatusCode:200, Body: rawPath}`.
- [ ] **Step 2:** Run → FAIL. **Step 3:** Implement. **Step 4:** Run → PASS.
- [ ] **Step 5:** Commit `feat(funcrt): API Gateway v2 HTTP translation and server`.

---

### Task 5: funcrt job mode + config + main

**Files:**
- Create: `internal/funcrt/config.go`, `internal/funcrt/job.go`, `cmd/funcrt/main.go`
- Test: `internal/funcrt/config_test.go`, `internal/funcrt/job_test.go`

**Interfaces:**
- Produces (env contract used by Task 9):
  | Env | Meaning | Default |
  |---|---|---|
  | `FUNCRT_MODE` | `service` \| `job` | required |
  | `FUNCRT_BOOTSTRAP` | binary path | `/var/task/bootstrap` |
  | `FUNCRT_CONCURRENCY` | copies | `1` |
  | `FUNCRT_TIMEOUT` | Go duration | `30s` |
  | `FUNCRT_PORT` / `FUNCRT_ADMIN_PORT` | | `8080` / `9090` |
  | `FUNCRT_PAYLOAD` | job payload JSON | empty → scheduled event |
  | `AWS_LAMBDA_FUNCTION_NAME`, `AWS_LAMBDA_FUNCTION_VERSION`, `AWS_LAMBDA_FUNCTION_MEMORY_SIZE` | passed through to bootstrap | set by operator |
  ```go
  func LoadConfig(getenv func(string) string) (Config, error)
  func ScheduledEvent(now time.Time, id string) []byte
  func RunJob(ctx context.Context, p *Pool, payload []byte) error // nil on 200, error otherwise
  ```
- funcrt passes its whole environment minus `FUNCRT_*` to bootstrap, plus `AWS_LAMBDA_RUNTIME_API`, `LAMBDA_TASK_ROOT=/var/task`, `_HANDLER=bootstrap`, `AWS_REGION=local`. Job mode: start pool, wait up to `Timeout` for `Ready()`, invoke once, log result, `os.Exit(0|1)`.
- `ScheduledEvent`: `{"version":"0","id":id,"detail-type":"Scheduled Event","source":"aws.events","account":"000000000000","time":RFC3339(now),"region":"local","resources":[],"detail":{}}`.

- [ ] **Step 1: Failing tests.** `TestLoadConfigDefaults`, `TestLoadConfigRejectsBadMode`, `TestScheduledEventShape` (unmarshal into `events.EventBridgeEvent`, `DetailType=="Scheduled Event"`, `Source=="aws.events"`), `TestRunJobSuccess` (`{"echo":1}` → nil), `TestRunJobHandlerError` (`{"error":"x"}` → non‑nil).
- [ ] **Step 2:** FAIL → implement → `go test ./internal/funcrt/... -race` PASS.
- [ ] **Step 3:** Commit `feat(funcrt): job mode and entrypoint`.

---

### Task 6: Fetcher

**Files:**
- Create: `internal/fetcher/fetcher.go`, `cmd/fetcher/main.go`
- Test: `internal/fetcher/fetcher_test.go`

**Interfaces:**
- Env contract (used by Task 9): `S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_USE_SSL` (`true|false`), `FETCH_BUCKET`, `FETCH_PREFIX` (`<key>/<version>`), `FETCH_ETAG_AMD64`, `FETCH_ETAG_ARM64`, `FETCH_DEST` (default `/var/task/bootstrap`).
  ```go
  var ErrETagMismatch = errors.New("etag mismatch")
  func Fetch(ctx context.Context, c *minio.Client, bucket, object, etag, dest string) error
  func ObjectFor(prefix, arch string) string // prefix + "/linux-" + arch + "/bootstrap"
  ```
- `Fetch` uses `GetObjectOptions.SetMatchETag(etag)`; on 412 returns `ErrETagMismatch`. Writes to `dest.tmp`, chmod 0755, renames. `main` picks `runtime.GOARCH`; on `ErrETagMismatch` writes `etag-mismatch` to `/dev/termination-log` and exits 3; other errors exit 1.

- [ ] **Step 1: Failing tests** against `httptest` fake S3 (or minio-go against a stub server answering GET with ETag/412): `TestFetchWritesExecutable` (mode 0755, content equal), `TestFetchETagMismatch` (`errors.Is(err, ErrETagMismatch)`, dest not created), `TestObjectFor` (`"f/v1","arm64"` → `"f/v1/linux-arm64/bootstrap"`).
- [ ] **Step 2:** FAIL → implement → PASS. **Step 3:** Commit `feat(fetcher): ETag-pinned S3 download`.

---

### Task 7: Runtime image + samples + upload

**Files:**
- Create: `images/runtime/Dockerfile`, `samples/go-hello/main.go`, `samples/go-hello/go.mod`, `samples/dotnet-hello/{Function.cs,dotnet-hello.csproj,Dockerfile.build}`, `hack/kind/upload-sample.sh`
- Modify: `Makefile` (targets `runtime-image`, `samples`, `upload-samples`)

**Interfaces:**
- Produces: image `localhost/functions-runtime:dev` containing `/usr/local/bin/funcrt` (entrypoint) and `/usr/local/bin/fetcher`, multi‑stage `golang:1.24` → `amazonlinux:2023-minimal`, `CGO_ENABLED=0`, built with `podman build --platform linux/amd64,linux/arm64 --manifest`.
- Samples uploaded as `functions/go-hello/{v1,latest}/linux-{amd64,arm64}/bootstrap` and `functions/dotnet-hello/...`. go-hello: APIGW v2 handler returning `{"lang":"go","path":rawPath,"version":$AWS_LAMBDA_FUNCTION_VERSION,"build":<-ldflags -X main.build>}`; sleeps for `?sleep=<duration>` if given; also handles scheduled events (logs `scheduled tick`). `make samples BUILD=<id>` sets the build stamp. dotnet-hello: `Amazon.Lambda.RuntimeSupport` + `Amazon.Lambda.APIGatewayEvents` + source‑generated serializer, `PublishAot=true`, `AssemblyName=bootstrap`, returns `{"lang":"dotnet",...}`; built in `mcr.microsoft.com/dotnet/sdk:8.0` with `clang zlib1g-dev` per target platform.
- `upload-sample.sh <name> <version> <dir>` uploads every `linux-<arch>/bootstrap` under `<dir>` via `mc` through `kubectl port-forward` to MinIO.

- [ ] **Step 1:** Write files. **Step 2:** `make runtime-image samples` → both arch binaries exist; `file samples/out/dotnet-hello/linux-arm64/bootstrap` reports ELF aarch64.
- [ ] **Step 3: Smoke test without k8s:** `podman run --rm -v samples/out/go-hello/linux-arm64:/var/task -e FUNCRT_MODE=job localhost/functions-runtime:dev` → exit 0, log contains `scheduled tick`. Same for dotnet-hello.
- [ ] **Step 4:** `make kind-up upload-samples` → `mc ls` shows both objects. Commit `feat: runtime image and sample functions`.

---

### Task 8: Function API types

**Files:**
- Modify: `api/v1alpha1/function_types.go`
- Test: `api/v1alpha1/function_validation_test.go` (envtest)

**Interfaces:**
- Produces:
  ```go
  type FunctionType string // "service" | "job"
  type FunctionSpec struct {
      Type          FunctionType          `json:"type"`
      Source        Source                `json:"source"`
      Architectures []string              `json:"architectures,omitempty"` // default ["amd64"], enum amd64;arm64, minItems 1
      Env           []corev1.EnvVar       `json:"env,omitempty"`
      Memory        resource.Quantity     `json:"memory"`                  // default 128Mi
      Timeout       metav1.Duration       `json:"timeout"`                 // default 30s
      Concurrency   int32                 `json:"concurrency,omitempty"`   // default 1, min 1, max 64
      Scaling       *Scaling              `json:"scaling,omitempty"`
      Schedule      string                `json:"schedule,omitempty"`
      Payload       *apiextensionsv1.JSON `json:"payload,omitempty"`
  }
  type Source  struct { Bucket string; Key string /*optional*/; Version string /*required*/ }
  type Scaling struct { Min int32 /*default 0*/; Max int32 /*default 10*/ }
  type FunctionStatus struct {
      ResolvedVersion    string            `json:"resolvedVersion,omitempty"`
      ETags              map[string]string `json:"etags,omitempty"` // arch → etag
      ObservedGeneration int64             `json:"observedGeneration,omitempty"`
      Conditions         []metav1.Condition `json:"conditions,omitempty"`
  }
  const ConditionReady = "Ready"; const ConditionSourceResolved = "SourceResolved"
  ```
- CEL rules on spec: `self.type == 'job' ? has(self.schedule) : !has(self.schedule)`; `self.type == 'service' || !has(self.scaling)`; `self.type == 'service' || !has(self.concurrency) || self.concurrency == 1`; `!has(self.scaling) || self.scaling.min <= self.scaling.max`. Printer columns: Type, Version, Ready, Age.

- [ ] **Step 1: Failing envtest tests:** `TestRejectsJobWithoutSchedule`, `TestRejectsServiceWithSchedule`, `TestRejectsJobWithScaling`, `TestDefaults` (created service has architectures `[amd64]`, concurrency 1, timeout 30s).
- [ ] **Step 2:** FAIL → add markers → `make manifests generate test` PASS. **Step 3:** Commit `feat(api): Function CRD`.

---

### Task 9: S3 resolver + resource builders

**Files:**
- Create: `internal/source/resolver.go`, `internal/resources/podtemplate.go`, `internal/resources/service.go`, `internal/resources/job.go`
- Test: `internal/source/resolver_test.go`, `internal/resources/*_test.go`

**Interfaces:**
- Consumes: Task 5 env contract, Task 6 env contract, Task 8 types.
- Produces:
  ```go
  // source
  type Resolved struct { Version string; Prefix string; ETags map[string]string }
  type Resolver interface { Resolve(ctx context.Context, bucket, key, version string, archs []string) (Resolved, error) }
  var ErrObjectMissing = errors.New("object missing")
  func NewS3Resolver(c *minio.Client) Resolver   // StatObject per arch; missing → wraps ErrObjectMissing naming the object

  // resources
  const S3SecretName = "functions-s3-read"      // keys: endpoint, accessKey, secretKey, useSSL
  const LabelFunction = "functions.pulsen.se/function"
  const AnnotationSourceHash = "functions.pulsen.se/source-hash" // sha256 of prefix + sorted etags
  func PodTemplate(fn *v1alpha1.Function, r source.Resolved, runtimeImage string) corev1.PodTemplateSpec
  func Deployment(fn *v1alpha1.Function, r source.Resolved, runtimeImage string) *appsv1.Deployment   // Replicas nil
  func BackendService(fn *v1alpha1.Function) *corev1.Service        // "<name>-backend", port 8080 → 8080
  func PublicService(fn *v1alpha1.Function, interceptorHost string) *corev1.Service // "<name>", ExternalName, port 8080
  func HTTPScaledObject(fn *v1alpha1.Function) *unstructured.Unstructured
  func Hosts(name, ns string) []string // 4 forms × {no port, ":8080"} = 8
  func CronJob(fn *v1alpha1.Function, r source.Resolved, runtimeImage string) *batchv1.CronJob
  ```
- PodTemplate: init container `fetcher` (env from `S3SecretName` + `FETCH_*`), container `funcrt` (env `FUNCRT_*`, `AWS_LAMBDA_FUNCTION_NAME=<name>`, `AWS_LAMBDA_FUNCTION_VERSION=<resolved>`, `AWS_LAMBDA_FUNCTION_MEMORY_SIZE=<MiB>`, then user `spec.env` verbatim), emptyDirs `/var/task` and `/tmp`, readinessProbe `/readyz:9090`, memory limit `memory×concurrency+64Mi`, memory request = limit, CPU request `max(50m, memMiB×concurrency/1769 cores)`, no CPU limit, nodeAffinity `kubernetes.io/arch In archs`, annotation `AnnotationSourceHash`, label `LabelFunction`. `terminationGracePeriodSeconds = timeout + 5s`.
- HTTPScaledObject: use the apiVersion/field names recorded in `hack/kind/SPIKE.md`; `scaleTargetRef` → Deployment `<name>`, service `<name>-backend`, port 8080; replicas min/max from `spec.scaling`; concurrency target = `spec.concurrency`; hosts = `Hosts(...)`.
- CronJob: `concurrencyPolicy: Forbid`, `jobTemplate.spec.activeDeadlineSeconds = timeout + 120s` (fetch + init headroom), `backoffLimit: 0`, `restartPolicy: Never`, `FUNCRT_MODE=job`, `FUNCRT_PAYLOAD` from `spec.payload` when set.

- [ ] **Step 1: Failing tests:**
  ```go
  func TestResolverAllArchs(t *testing.T)      // fake S3 with both objects → ETags has amd64, arm64; Prefix "f/v1"
  func TestResolverMissingArch(t *testing.T)   // arm64 missing → errors.Is(err, ErrObjectMissing), message contains "linux-arm64"
  func TestPodTemplateResources(t *testing.T)  // memory 256Mi, concurrency 4 → limit 1088Mi; FUNCRT_CONCURRENCY "4"; AWS_LAMBDA_FUNCTION_MEMORY_SIZE "256"
  func TestPodTemplateHashChangesWithETag(t *testing.T)
  func TestPodTemplateUserEnvPassThrough(t *testing.T) // valueFrom secretKeyRef preserved
  func TestPodTemplateArchAffinity(t *testing.T)
  func TestHostsIncludesPortForms(t *testing.T)        // len 8, contains "f.ns.svc.cluster.local:8080"
  func TestPublicServiceExternalName(t *testing.T)     // Type ExternalName, ExternalName == interceptorHost
  func TestCronJobDeadlineAndPayload(t *testing.T)     // timeout 30s → activeDeadlineSeconds 150; payload {"a":1} → FUNCRT_PAYLOAD
  ```
- [ ] **Step 2:** FAIL → implement → PASS. **Step 3:** Commit `feat: S3 resolver and resource builders`.

---

### Task 10: Controller reconcile

**Files:**
- Modify: `internal/controller/function_controller.go`, `cmd/main.go`
- Create: `internal/controller/s3secret.go`
- Test: `internal/controller/function_controller_test.go` (envtest, fake `Resolver`, HTTPScaledObject CRD YAML vendored into `config/test-crds/` from the installed add‑on version)

**Interfaces:**
- Consumes: Task 9.
- Produces: `FunctionReconciler{ Client; Scheme; Resolver source.Resolver; RuntimeImage string; InterceptorHost string; S3SecretNamespace string; LatestPollInterval time.Duration; Now func() time.Time }`. Flags in `cmd/main.go`: `--runtime-image`, `--interceptor-host` (default `keda-add-ons-http-interceptor-proxy.keda.svc.cluster.local`), `--s3-secret-namespace` (default operator namespace), `--latest-poll-interval=30s`; S3 client from secret `functions-s3-read` in that namespace.
- Reconcile order:
  1. Copy `functions-s3-read` into the Function's namespace (create/update, label `app.kubernetes.io/managed-by=functions-operator`, no ownerRef).
  2. `Resolve`. On error: set `SourceResolved=False` (reason `ObjectMissing` or `S3Error`), `Ready=False`, **do not touch workloads**, requeue after poll interval.
  3. Service: CreateOrUpdate Deployment (never set `Spec.Replicas` on update), backend Service, public Service, HTTPScaledObject; delete CronJob `<name>` if present. Job: CreateOrUpdate CronJob; delete the four service objects if present. All with ownerRef → Function.
  4. Status: `resolvedVersion`, `etags`, `observedGeneration`, `SourceResolved=True`, `Ready=True` (service: Deployment exists and HSO created; job: CronJob exists).
  5. If `source.version == "latest"` → `RequeueAfter: LatestPollInterval`.
- RBAC markers for deployments, services, secrets, cronjobs, pods (get/list/watch), httpscaledobjects.http.keda.sh, functions + status.

- [ ] **Step 1: Failing tests:**
  ```go
  func TestReconcileServiceCreatesObjects(t *testing.T)        // Deployment, f-backend, f (ExternalName), HSO, Secret copy exist; Ready=True
  func TestReconcileJobCreatesCronJob(t *testing.T)
  func TestReconcileVersionChangeUpdatesHash(t *testing.T)     // v1 → v2 changes AnnotationSourceHash, status.resolvedVersion "v2"
  func TestReconcileLatestRequeuesAndRollsOnETagChange(t *testing.T) // result.RequeueAfter == 30s; fake ETag change → new hash
  func TestReconcileSourceMissingKeepsDeployment(t *testing.T) // after good reconcile, resolver errors → SourceResolved False, Deployment hash unchanged
  func TestReconcileTypeSwitchCleansUp(t *testing.T)           // service → job: Deployment/Services/HSO gone, CronJob present
  func TestReconcileDoesNotOverwriteReplicas(t *testing.T)     // set Deployment replicas 3 externally, reconcile → still 3
  ```
- [ ] **Step 2:** FAIL → implement → `make test` PASS.
- [ ] **Step 3:** Commit `feat(controller): reconcile Function into workloads`.

---

### Task 11: Fast re-resolve on ETag mismatch

**Files:**
- Modify: `internal/controller/function_controller.go`
- Test: `internal/controller/etag_mismatch_test.go`

**Interfaces:**
- Consumes: fetcher exit 3 + termination message `etag-mismatch` (Task 6), `LabelFunction` (Task 9).
- Controller `Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(...))` with a predicate: pod has `LabelFunction` and an init container status `Terminated{ExitCode:3, Message:"etag-mismatch"}`; maps to the Function `<ns>/<label value>`. Reconcile then resolves as normal (the new ETag produces a new hash → rollout).

- [ ] **Step 1: Failing test** `TestPodETagMismatchTriggersReconcile`: Function on `latest`, fake resolver returns new ETag, create Pod with that init status → within 5s Deployment hash matches new ETag (without waiting the poll interval; set interval to 1h in the test).
- [ ] **Step 2:** FAIL → implement → PASS. **Step 3:** Commit `feat(controller): re-resolve on fetcher ETag mismatch`.

---

### Task 12: End-to-end on kind + KrakenD + docs

**Files:**
- Create: `test/e2e/e2e.sh`, `hack/kind/krakend.yaml`, `config/samples/functions_v1alpha1_function_{go,dotnet,job,dev-latest}.yaml`, `README.md`
- Modify: `Makefile` (`e2e`, `deploy-kind`)

**Interfaces:**
- Consumes: everything. `make deploy-kind` builds/loads operator + runtime images, creates `functions-s3-read` secret (`endpoint=minio.minio.svc.cluster.local:9000`, `useSSL=false`, minioadmin creds) in `functions-operator-system`, deploys operator, applies samples into ns `team-a`.
- KrakenD in ns `gateway`: endpoints `/go/{p}` → `http://go-hello.team-a.svc.cluster.local:8080`, `/dotnet/{p}` → `http://dotnet-hello.team-a.svc.cluster.local:8080`, `timeout: 60s`, `no-op` encoding.

- [ ] **Step 1:** Write `e2e.sh` asserting, in order (each fails the script with a message):
  1. go‑hello and dotnet‑hello `Ready=True`; their Deployments at 0 replicas after `scaledownPeriod` (set to 30s in samples).
  2. `curl` through KrakenD `/go/x` → 200, body `"lang":"go"`; `/dotnet/x` → `"lang":"dotnet"` (cold start from zero).
  3. `hey -z 20s -c 40` against go‑hello (concurrency 4) → Deployment replicas > 1 during load; back to 0 within 2 min after.
  4. Job sample (`*/1 * * * *`) → a Job succeeds within 2.5 min; its pod log contains `scheduled tick`.
  5. Change go‑hello `version` v1 → v2 (upload v2 first) → response `"version":"v2"`.
  6. Rebuild with a new `BUILD` and overwrite `latest` for `go-hello-dev` → within 60s `status.etags.arm64` changes and the response's `build` equals the new stamp.
  7. Sample with `timeout: 2s` invoked with `?sleep=5s` (go‑hello honours a `sleep` query param) → 504.
- [ ] **Step 2:** `make kind-up deploy-kind upload-samples e2e` → all checks print `PASS`.
- [ ] **Step 3:** `README.md`: how teams build `bootstrap` (Go `-tags lambda.norpc`, .NET `PublishAot`), S3 layout, `Function` reference, KrakenD timeout requirement (> cold start + function timeout; KrakenD default is 2s), local kind workflow.
- [ ] **Step 4:** Commit `test: kind end-to-end suite and docs`.
