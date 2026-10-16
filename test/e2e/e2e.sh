#!/usr/bin/env bash
# End-to-end suite for the local kind cluster "functions".
#
# Prerequisites: make kind-up deploy-kind upload-samples  (KIND=/path/to/kind if needed)
# Usage:         make e2e   (or test/e2e/e2e.sh)
#
# All traffic is generated in-cluster (service DNS is not reachable from the host): a long-lived curl
# pod and a hey Job in namespace e2e. The script is re-runnable: it re-applies the samples (resetting
# go-hello to v1), recreates its own pods, and every wait is bounded. Prints PASS/FAIL per check and
# exits non-zero if any check failed.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CTX="${KUBE_CONTEXT:-kind-functions}"
export KUBE_CONTEXT="$CTX"
NS=team-a
E2E_NS=e2e
CURL_IMG="${CURL_IMG:-docker.io/curlimages/curl:8.22.0}"
HEY_IMG="${HEY_IMG:-localhost/hey:v0.1.5}"
GATEWAY=http://krakend.gateway.svc.cluster.local:8080
RUN_ID="$(date +%Y%m%d%H%M%S)"
WORK="$(mktemp -d)"

k() { kubectl --context "$CTX" "$@"; }
log() { printf '      %s\n' "$*"; }

FAILED=()
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s: %s\n' "$1" "$2"; FAILED+=("$1"); }

cleanup() {
  k -n "$E2E_NS" delete pod e2e-curl --ignore-not-found --wait=false >/dev/null 2>&1 || true
  k -n "$E2E_NS" delete job e2e-hey --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# wait_for <seconds> <command...>: poll every 2s until the command succeeds.
wait_for() {
  local deadline=$((SECONDS + $1)); shift
  until "$@"; do
    [ "$SECONDS" -ge "$deadline" ] && return 1
    sleep 2
  done
}

ready() { [ "$(k -n "$NS" get function "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = True ]; }
replicas() { k -n "$NS" get deploy "$1" -o jsonpath='{.spec.replicas}' 2>/dev/null; }
scaled_to_zero() {
  [ "$(replicas "$1")" = 0 ] || return 1
  [ -z "$(k -n "$NS" get pods -l "functions.pulsen.se/function=$1" -o name 2>/dev/null)" ]
}
etag() { k -n "$NS" get function "$1" -o jsonpath="{.status.etags.$ARCH}"; }
md5of() { if command -v md5sum >/dev/null; then md5sum "$1" | cut -d' ' -f1; else md5 -q "$1"; fi; }

# http <url>: prints "<status> <body>" from the in-cluster curl pod.
http() {
  local out
  out="$(k -n "$E2E_NS" exec e2e-curl -- curl -s --max-time 90 -w '\n%{http_code}' "$1" 2>/dev/null)"
  printf '%s %s' "${out##*$'\n'}" "${out%$'\n'*}"
}

# body_matches <url> <substring>: true once a 200 response body contains substring (stores it in LAST).
LAST=""
body_matches() {
  LAST="$(http "$1")"
  [[ "$LAST" == "200 "* && "$LAST" == *"$2"* ]]
}

# build_go <build-stamp> <outdir>: builds samples/go-hello for the node arch into <outdir>/linux-<arch>/bootstrap.
build_go() {
  mkdir -p "$2/linux-$ARCH"
  (cd "$ROOT/samples/go-hello" && CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
    go build -buildvcs=false -trimpath -tags lambda.norpc -ldflags "-X main.build=$1" -o "$2/linux-$ARCH/bootstrap" .)
}

upload() { "$ROOT/hack/kind/upload-sample.sh" "$@" >/dev/null; }

# ---------------------------------------------------------------------------------------------------
echo "== setup (context $CTX)"
ARCH="$(k get nodes -o jsonpath='{.items[0].metadata.labels.kubernetes\.io/arch}')"
log "node arch: $ARCH"
k apply -f "$ROOT/hack/kind/krakend.yaml" >/dev/null
k apply -k "$ROOT/config/samples" >/dev/null
# apply does not revert fields changed by a previous run's patch, so reset go-hello explicitly.
k -n "$NS" patch function go-hello --type=merge -p '{"spec":{"source":{"version":"v1"}}}' >/dev/null
k create namespace "$E2E_NS" --dry-run=client -o yaml | k apply -f - >/dev/null
k -n "$E2E_NS" delete pod e2e-curl --ignore-not-found --wait=true >/dev/null
k -n "$E2E_NS" delete job e2e-hey --ignore-not-found --wait=true >/dev/null
k -n "$E2E_NS" run e2e-curl --image="$CURL_IMG" --restart=Never --command -- sleep 3600 >/dev/null
if ! k -n "$E2E_NS" wait --for=condition=Ready pod/e2e-curl --timeout=120s >/dev/null ||
   ! k -n gateway rollout status deploy/krakend --timeout=120s >/dev/null; then
  echo "FAIL  setup: curl pod or KrakenD not ready"; exit 1
fi

# ---------------------------------------------------------------------------------------------------
check1() {
  local c="1 Ready and scaled to zero" f
  for f in go-hello dotnet-hello; do
    wait_for 180 ready "$f" || { fail "$c" "$f not Ready=True: $(k -n $NS get function $f -o jsonpath='{.status.conditions}')"; return; }
  done
  log "go-hello and dotnet-hello Ready=True; waiting for scale to zero (scaledownPeriodSeconds 30)"
  for f in go-hello dotnet-hello; do
    wait_for 240 scaled_to_zero "$f" || { fail "$c" "$f deployment replicas=$(replicas $f), not 0"; return; }
  done
  pass "$c"
}

check2() {
  local c="2 cold start through KrakenD" f path want
  for f in go-hello:go dotnet-hello:dotnet; do
    path="${f#*:}"; want="\"lang\":\"$path\""; f="${f%%:*}"
    [ "$(replicas "$f")" = 0 ] || log "warning: $f not at 0 replicas before request"
    local t0=$SECONDS
    LAST="$(http "$GATEWAY/$path/x")"
    log "GET /$path/x -> ${LAST:0:120} ($((SECONDS - t0))s)"
    [[ "$LAST" == "200 "* && "$LAST" == *"$want"* ]] || { fail "$c" "/$path/x: got '$LAST'"; return; }
  done
  pass "$c"
}

check3() {
  local c="3 scale out under load and back to zero" max=0 r
  k -n "$E2E_NS" create job e2e-hey --image="$HEY_IMG" -- \
    /usr/local/bin/hey -z 20s -c 40 "http://go-hello.$NS.svc.cluster.local:8080/load?sleep=50ms" >/dev/null
  local deadline=$((SECONDS + 90))
  while [ "$SECONDS" -lt "$deadline" ]; do
    r="$(replicas go-hello)"; [ "${r:-0}" -gt "$max" ] && max=$r
    [ "$(k -n "$E2E_NS" get job e2e-hey -o jsonpath='{.status.succeeded}')" = 1 ] && break
    sleep 2
  done
  log "max replicas during load: $max"
  k -n "$E2E_NS" logs job/e2e-hey 2>/dev/null | grep -E 'Requests/sec|^ +\[[0-9]{3}\]' | sed 's/^/      hey: /'
  [ "$max" -gt 1 ] || { fail "$c" "replicas never exceeded 1 (max $max)"; return; }
  wait_for 120 scaled_to_zero go-hello || { fail "$c" "go-hello not back to 0 within 2 min (replicas=$(replicas go-hello))"; return; }
  pass "$c"
}

check4() {
  local c="4 scheduled job" since job=""
  since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  ready go-hello-job || wait_for 60 ready go-hello-job || { fail "$c" "go-hello-job not Ready"; return; }
  find_job() {
    job="$(k -n "$NS" get jobs -l functions.pulsen.se/function=go-hello-job \
      -o jsonpath='{range .items[?(@.status.succeeded==1)]}{.metadata.name} {.status.completionTime}{"\n"}{end}' |
      awk -v s="$since" '$2 >= s {print $1}' | tail -1)"
    [ -n "$job" ]
  }
  wait_for 150 find_job || { fail "$c" "no Job succeeded within 2.5 min"; return; }
  log "job $job succeeded"
  k -n "$NS" logs "job/$job" -c funcrt 2>/dev/null | grep -q "scheduled tick" ||
    { fail "$c" "job $job log lacks 'scheduled tick'"; return; }
  pass "$c"
}

check5() {
  local c="5 version change v1 -> v2"
  # Pinned versions are immutable by convention: a fixed stamp and a reproducible build give the
  # same object (and ETag) on every run.
  build_go "v2" "$WORK/v2" || { fail "$c" "build failed"; return; }
  upload go-hello v2 "$WORK/v2" || { fail "$c" "upload failed"; return; }
  k -n "$NS" patch function go-hello --type=merge -p '{"spec":{"source":{"version":"v2"}}}' >/dev/null
  resolved_v2() { [ "$(k -n "$NS" get function go-hello -o jsonpath='{.status.resolvedVersion}')" = v2 ]; }
  wait_for 60 resolved_v2 || { fail "$c" "status.resolvedVersion did not become v2"; return; }
  wait_for 120 body_matches "$GATEWAY/go/x" '"version":"v2"' || { fail "$c" "got '$LAST'"; return; }
  log "GET /go/x -> ${LAST:0:120}"
  pass "$c"
}

check6() {
  local c="6 latest follows S3 overwrite" before want stamp="dev-$RUN_ID"
  wait_for 60 ready go-hello-dev || { fail "$c" "go-hello-dev not Ready"; return; }
  before="$(etag go-hello-dev)"
  build_go "$stamp" "$WORK/dev" || { fail "$c" "build failed"; return; }
  want="$(md5of "$WORK/dev/linux-$ARCH/bootstrap")" # single-part upload: ETag = MD5
  [ "$want" != "$before" ] || { fail "$c" "new build has the same ETag"; return; }
  upload go-hello latest "$WORK/dev" || { fail "$c" "upload failed"; return; }
  local t0=$SECONDS
  etag_is_new() { [ "$(etag go-hello-dev)" = "$want" ]; }
  wait_for 60 etag_is_new || { fail "$c" "status.etags.$ARCH not updated within 60s ($(etag go-hello-dev), want $want)"; return; }
  log "status.etags.$ARCH $before -> $want after $((SECONDS - t0))s"
  wait_for 120 body_matches "http://go-hello-dev.$NS.svc.cluster.local:8080/x" "\"build\":\"$stamp\"" ||
    { fail "$c" "response build != $stamp: '$LAST'"; return; }
  [[ "$LAST" == *'"version":"latest"'* ]] || { fail "$c" "expected version latest: '$LAST'"; return; }
  log "GET go-hello-dev/x -> ${LAST:0:120}"
  pass "$c"
}

check7() {
  local c="7 timeout returns 504"
  wait_for 60 ready go-hello-timeout || { fail "$c" "go-hello-timeout not Ready"; return; }
  LAST="$(http "http://go-hello-timeout.$NS.svc.cluster.local:8080/x?sleep=5s")"
  log "GET go-hello-timeout/x?sleep=5s -> ${LAST:0:120}"
  [[ "$LAST" == "504"* ]] || { fail "$c" "got '$LAST'"; return; }
  # The timed-out copy is killed and replaced; the next request must succeed.
  wait_for 30 body_matches "http://go-hello-timeout.$NS.svc.cluster.local:8080/x" '"lang":"go"' ||
    { fail "$c" "request after timeout failed: '$LAST'"; return; }
  log "next request -> ${LAST:0:120}"
  pass "$c"
}

echo "== checks"
check1
check2
check3
check4
check5
check6
check7

echo
if [ ${#FAILED[@]} -gt 0 ]; then
  echo "FAILED: ${#FAILED[@]} check(s): ${FAILED[*]}"
  exit 1
fi
echo "ALL CHECKS PASSED"
