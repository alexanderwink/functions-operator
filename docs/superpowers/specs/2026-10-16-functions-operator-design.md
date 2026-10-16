# Functions Operator — Design

Date: 2026-10-16
Status: Draft for review

## Intent

We write small AWS Lambda–style functions in Go and .NET (real Lambda handlers using `lambda.Start` / `Amazon.Lambda.RuntimeSupport`) and want to run them **in production on our own on‑prem Kubernetes**, not in AWS. A Kubernetes operator takes a compiled binary from S3‑compatible storage and turns it into either an HTTP service or a scheduled job, without building a container image per function.

The idea is borrowed from how the Serverless Framework runs functions locally (mounting code into a Lambda base image), but hardened for production: a shared base container, real concurrency, autoscaling with scale‑to‑zero.

Local development and testing happen on a `kind` cluster.

## Goals

- A `Function` custom resource describes one function; the operator reconciles it into Kubernetes objects.
- Two function types:
  - **service** — invoked by HTTP requests, autoscaled on in‑flight requests, scales to zero when idle.
  - **job** — invoked on a cron schedule.
- Binaries are unmodified Lambda handlers. The same binary should run in real AWS Lambda (`provided.al2023`) and on this platform.
- **One base container for both languages.** Go builds and .NET **Native AOT** builds both produce a `bootstrap` executable that talks the Lambda Runtime API.
- Versioned rollouts driven by editing the `Function` resource; a `latest` channel that dev environments follow automatically.
- Per‑function environment variables (including from Secrets/ConfigMaps), memory, and timeout.
- Runs end to end on a local `kind` cluster.

## Non‑goals

- No AWS service integration (IAM roles, credentials for DynamoDB/SQS etc.). Functions do not call AWS services.
- No external exposure: no Ingress. Our existing API gateway sits in front and calls the in‑cluster URL.
- No per‑function container image builds, no image registry for function code.
- No support for the managed `dotnet:8` Lambda runtime (handler strings, DLL zips). .NET must be Native AOT.
- No event sources beyond HTTP and cron (no SQS, Kafka, S3 triggers).

## Anti‑goals (would count as failure even if it "works")

- Relying on the AWS Runtime Interface Emulator (RIE) in production — it is a test tool and handles one invocation at a time.
- Two binary versions silently running side by side because a `latest` overwrite wasn't noticed.
- Requests failing outright when a function is scaled to zero instead of waiting for a cold start.
- Requiring function authors to change their handler code to run on this platform.

## Constraints

- On‑prem cluster; storage is S3‑compatible (MinIO locally, and presumably on‑prem in production).
- Autoscaling uses **KEDA + KEDA HTTP add‑on** (chosen by the owner).
- Concurrency inside a pod uses **our own runtime process**, not RIE (chosen by the owner).
- Scale‑to‑zero is required for services.

## Architecture

### Components

1. **Operator** (controller manager) — watches `Function` resources, creates/updates the Kubernetes objects below, polls S3 for `latest`, reports status.
2. **Base image (“function runtime”)** — one image containing our **runtime process** (`funcrt`). The function binary is not in the image; it is fetched at pod start.
3. **Fetcher init container** — downloads the binary for the pinned version from S3 into a shared `emptyDir`, marks it executable, and verifies it against the expected ETag.
4. **KEDA + HTTP add‑on** — installed cluster‑wide; the interceptor fronts all service traffic and drives scaling.
5. **S3‑compatible storage** — MinIO in kind.

### The runtime process (`funcrt`)

`funcrt` is the container's entrypoint. It replaces RIE and implements the **Lambda Runtime API** locally:

- `GET  /2018-06-01/runtime/invocation/next`
- `POST /2018-06-01/runtime/invocation/{id}/response`
- `POST /2018-06-01/runtime/invocation/{id}/error`
- `POST /2018-06-01/runtime/init/error`

It starts **N copies** of `bootstrap` (N = `spec.concurrency`), each with `AWS_LAMBDA_RUNTIME_API` pointing at `funcrt`, plus the standard Lambda environment (`AWS_LAMBDA_FUNCTION_NAME`, `AWS_LAMBDA_FUNCTION_VERSION`, `AWS_LAMBDA_FUNCTION_MEMORY_SIZE`, `_HANDLER`, `LAMBDA_TASK_ROOT`, etc.) and the user's env vars. Each copy handles one invocation at a time, exactly as in Lambda; the pod handles up to N concurrently.

**Service mode:** `funcrt` listens on an HTTP port, converts each incoming request into an **API Gateway HTTP API (payload v2.0)** event (decided by the owner; the only supported format), hands it to the next free `bootstrap`, and converts the handler's response back to HTTP. Requests queue in `funcrt` when all copies are busy.

**Job mode:** `funcrt` sends a single event, waits for the result, and exits with code 0 on success / non‑zero on handler error or timeout.

**Timeout:** each invocation gets `Lambda-Runtime-Deadline-Ms` so `context` deadlines work in handlers. If a copy exceeds the timeout, `funcrt` kills and restarts that copy and returns **504** to the caller.

**Crashes:** if a `bootstrap` copy exits, `funcrt` returns 502 for its in‑flight request and restarts it. If init fails (`/init/error` or repeated crash on start), the pod fails readiness.

**Health/metrics:** readiness = at least one copy has polled `/next`. Expose Prometheus metrics (in‑flight, queued, invocations, errors, duration). Logs from `bootstrap` go to stdout/stderr unchanged.

### Kubernetes objects per `Function`

**type: service**

| Object | Purpose |
|---|---|
| `Deployment` `<name>` | fetcher init container + `funcrt`; replicas owned by KEDA |
| `Service` `<name>-backend` | ClusterIP to the pods |
| `HTTPScaledObject` `<name>` | min/max replicas, target concurrency = `spec.concurrency`, hosts = all in‑cluster forms of `<name>` |
| `Service` `<name>` | `ExternalName` → KEDA interceptor; this is the URL the API gateway uses |

The public in‑cluster URL is `http://<name>.<namespace>.svc.cluster.local:8080`. The interceptor routes by `Host` header, so the `HTTPScaledObject` registers `<name>`, `<name>.<ns>`, `<name>.<ns>.svc`, `<name>.<ns>.svc.cluster.local`.

**API gateway = KrakenD.** KrakenD backends are configured with that URL as `host`; KrakenD sends the backend's host as the `Host` header, which is what the interceptor routes on. Consequences:

- Port 8080 is acceptable since KrakenD backend URLs can carry a port, so the `ExternalName` approach works without port remapping. The first spike must still confirm that the interceptor matches a `Host` that includes `:8080` (or strips the port); if not, register the `host:port` forms too.
- KrakenD's default backend timeout is **2s**. Endpoints in front of a scale‑to‑zero function must set a KrakenD `timeout` larger than cold start + `spec.timeout`. Document this for teams; the interceptor's wait/response timeouts must be set at least as large as the largest function timeout plus cold start.
- Auth: the gateway does auth. `funcrt` does not synthesize `requestContext.authorizer`; handlers that need identity read headers KrakenD propagates (e.g. `propagate_claims`). *(My call.)*

Behaviour must be: request to `<name>` with zero pods waits for cold start and succeeds.

**type: job**

| Object | Purpose |
|---|---|
| `CronJob` `<name>` | fetcher init container + `funcrt` in job mode; `activeDeadlineSeconds` derived from timeout; `concurrencyPolicy: Forbid` by default |

The job event is an **EventBridge "Scheduled Event"**‑shaped JSON (what Lambda receives from a schedule), unless `spec.payload` overrides it. *(Builder's call on exact fields.)*

### The `Function` resource

```yaml
apiVersion: functions.pulsen.se/v1alpha1   # group name: builder's call
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
  architectures: [amd64]     # optional, default [amd64]; arm64 later
  env:
    - name: LOG_LEVEL
      value: info
    - name: DB_PASSWORD
      valueFrom:
        secretKeyRef: { name: orders-db, key: password }
  memory: 256Mi              # per bootstrap copy (like Lambda memory)
  timeout: 30s               # per invocation
  concurrency: 4             # bootstrap copies per pod (service only; job = 1)
  scaling:                   # service only
    min: 0
    max: 10
  schedule: "*/5 * * * *"    # job only
  payload: {}                # job only, optional
status:
  resolvedVersion: v42
  etag: "9b2cf5…"
  conditions: [...]          # Ready, SourceResolved, etc.
```

- `env` uses the standard Kubernetes `EnvVar` type.
- Pod memory limit = `memory × concurrency` (+ small overhead for `funcrt`). *(My call.)*
- CPU: no limit by default; request derived from memory, roughly Lambda‑proportional. *(Builder's call.)*

### S3 layout and versions

```
s3://<bucket>/<function>/<version>/linux-<arch>/bootstrap
s3://<bucket>/<function>/latest/linux-<arch>/bootstrap
```

- **Shared bucket, one folder per function.** All teams share the cluster and the bucket; each function has its own unique top‑level folder. Teams write their own `Function` resources; there is no allowlist or ownership check on `bucket`/`key` (owner's decision: pointing at the wrong binary is the team's responsibility). The operator does **not** enforce that only one `Function` references a key, because dev (`latest`) and prod (pinned) legitimately reference the same function folder. *(My call.)*
- **Architecture.** Production is amd64 only today, but arm64 is planned. The architecture is part of the S3 path from day one. `spec.architectures` (default `[amd64]`) lists which builds exist; the operator verifies each listed object exists, sets node affinity on `kubernetes.io/arch` to those architectures, and the fetcher picks the object matching the node it runs on. The base image is multi‑arch.
- **Pinned versions are immutable by convention.** Teams never overwrite `v42`; only `latest` is overwritten. *(My call.)*
- **Pinned version** (`v42`): nothing changes until someone edits `spec.source.version`. Editing it triggers a normal rolling update (service) or updates the CronJob template (job). Rolling back = pointing at an older version.
- **`latest`** (used by dev): the operator polls the object's ETag (default every 30s, configurable on the operator) and, when it changes, rolls out.
- In both cases the operator writes the resolved S3 path + ETag into the pod template (as an annotation and as an env var for the fetcher). Every pod therefore fetches exactly the object the operator resolved, and the fetcher refuses to start if the ETag doesn't match. This prevents mixed versions after scale‑from‑zero.
- **Race on `latest`:** if `latest` is overwritten between the operator's poll and a pod start, the fetcher fails on ETag mismatch. The operator must notice (pod init failure / fetcher reporting the mismatch) and immediately re‑resolve and roll out rather than waiting for the next poll, so requests waiting on a cold start are delayed, not failed. *(My call; exact signalling is the builder's.)* ETags are compared as opaque strings only (multipart ETags are not MD5).

### S3 access

The operator is configured once with an S3 endpoint and credentials (a Secret in the operator namespace). The fetcher init container needs read access in each function namespace; how credentials reach it (operator copies a read‑only Secret into each namespace vs. fetcher proxies through the operator) is the **builder's call**. One shared read‑only credential for the bucket is acceptable, since teams are trusted not to cross folders.

## Prerequisites (not shipped by the operator)

- **KEDA and the KEDA HTTP add‑on** are installed separately (Helm), cluster‑wide. The operator targets the latest stable HTTP add‑on release at build time and documents the supported version; the local kind setup installs that same version. *(My call.)*
- S3‑compatible storage with the bucket created.

## Local development (kind)

A `make`‑style setup that:

1. Creates a kind cluster.
2. Installs KEDA, the KEDA HTTP add‑on, and MinIO.
3. Builds and loads the operator and base images into kind.
4. Uploads sample functions (one Go, one .NET Native AOT) to MinIO, built for `linux-arm64` (kind on Apple Silicon) and `linux-amd64`.
5. Applies sample `Function` resources: one service, one job.

Note: the dev machine is Apple Silicon (arm64) while production is amd64, which exercises the architecture handling from the start. Samples in kind use `architectures: [arm64]` (or both).

## Success criteria

- `curl http://orders-api.team-a.svc.cluster.local:8080/...` from inside the cluster, and through a KrakenD instance in kind, hits a Go handler and a .NET AOT handler, including from zero replicas.
- Load test shows replicas rising with in‑flight requests and returning to zero after idle.
- A cron `Function` runs on schedule and its handler output is in pod logs.
- Changing `version` rolls out; overwriting `latest` rolls out dev within the poll interval; no pod ever runs an unexpected ETag.
- A handler exceeding `timeout` gets 504 and the copy is replaced.

## Decided by the owner

- Real Lambda handlers (`lambda.Start`), unchanged.
- Production target, on‑prem.
- Own runtime process + KEDA (HTTP add‑on), scale‑to‑zero.
- Versioned S3 folders + `latest`; prod pins a version, dev follows `latest` automatically; versions changed by editing the `Function`.
- Env vars, memory, timeout per function. No AWS credentials.
- Internal Service URL per function; no Ingress (API gateway handles external).
- .NET is Native AOT; one base container for both languages.
- HTTP event format: API Gateway HTTP API v2.0, only.
- API gateway is KrakenD.
- Teams share cluster and bucket, write their own `Function`s; one unique folder per function in the bucket; no cross‑team protection.
- amd64 only now, design for arm64 later.

## Calls I made (owner to check)

- Operator in **Go** with **kubebuilder/controller‑runtime**.
- Job event: **EventBridge Scheduled Event** shape, overridable with `spec.payload`.
- `memory` is per `bootstrap` copy; pod limit = memory × concurrency.
- `latest` poll interval 30s.
- ETag pinned in pod template; fetcher verifies it.
- Timeout → kill and restart the copy, return 504.
- Base OS for the image: something with glibc (Native AOT and Go cgo binaries need it), e.g. `amazonlinux:2023-minimal` to match Lambda's `provided.al2023`.

## Left to the builder

- API group name, exact CRD validation, status conditions.
- How the fetcher gets S3 credentials (least privilege).
- Confirming interceptor `Host` matching with a port (spike).
- Metrics names, logging format of `funcrt`.
- Project layout, test strategy, Makefile targets.
