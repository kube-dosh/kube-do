#!/usr/bin/env bash

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

if [[ -z "${KUBE_DO_E2E_CONFIG:-}" ]]; then
  echo "KUBE_DO_E2E_CONFIG must point to an existing ClusterConfig for the target cluster" >&2
  exit 2
fi
if [[ ! -f "$KUBE_DO_E2E_CONFIG" ]]; then
  echo "KUBE_DO_E2E_CONFIG does not exist: $KUBE_DO_E2E_CONFIG" >&2
  exit 2
fi

for command in curl jq sha256sum ansible-playbook; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "required command not found: $command" >&2
    exit 2
  fi
done
if [[ -z "${KUBE_DO_E2E_BIN:-}" ]]; then
  command -v go >/dev/null 2>&1 || {
    echo "required command not found: go (or set KUBE_DO_E2E_BIN to a prebuilt kube-do binary)" >&2
    exit 2
  }
elif [[ ! -x "${KUBE_DO_E2E_BIN}" ]]; then
  echo "KUBE_DO_E2E_BIN is not executable: ${KUBE_DO_E2E_BIN}" >&2
  exit 2
fi

TOKEN=${KUBE_DO_E2E_TOKEN:-e2e-$RANDOM-$RANDOM}
ADDR=${KUBE_DO_E2E_ADDR:-127.0.0.1:18090}
BASE=http://$ADDR
ANSIBLE_DIR=${KUBE_DO_E2E_ANSIBLE_DIR:-$ROOT/ansible}
WORK_ROOT=${KUBE_DO_E2E_WORK_DIR:-$(mktemp -d /tmp/kube-do-e2e.XXXXXX)}
DATA_DIR=${KUBE_DO_E2E_DATA_DIR:-$WORK_ROOT/data}
CACHE_DIR=${KUBE_DO_E2E_CACHE_DIR:-$WORK_ROOT/cache}
BIN=${KUBE_DO_E2E_BIN:-$WORK_ROOT/kube-do}
DAEMON_LOG=$WORK_ROOT/daemon.log
RESPONSE=$WORK_ROOT/response.json
RUN_OPS=${KUBE_DO_E2E_RUN_OPS:-1}
REQUIRE_CONFLICT=${KUBE_DO_E2E_REQUIRE_CONFLICT:-1}
JOB_TIMEOUT=${KUBE_DO_E2E_JOB_TIMEOUT:-30m}
BACKUP_TIMEOUT=${KUBE_DO_E2E_BACKUP_TIMEOUT:-10m}
CERT_TIMEOUT=${KUBE_DO_E2E_CERT_TIMEOUT:-5m}
BACKUP_WAIT_SECONDS=${KUBE_DO_E2E_BACKUP_WAIT_SECONDS:-900}
CERT_WAIT_SECONDS=${KUBE_DO_E2E_CERT_WAIT_SECONDS:-600}
PID=""

mkdir -p "$WORK_ROOT" "$DATA_DIR" "$CACHE_DIR"

cleanup() {
  stop_daemon || true
  if [[ "${KUBE_DO_E2E_KEEP:-0}" != "1" ]]; then
    rm -rf -- "$WORK_ROOT"
  else
    echo "kept E2E work directory: $WORK_ROOT"
  fi
}
trap cleanup EXIT

fail() {
  echo "E2E FAILED: $*" >&2
  if [[ -f "$RESPONSE" ]]; then
    echo "last API response:" >&2
    cat "$RESPONSE" >&2 || true
  fi
  if [[ -f "$DAEMON_LOG" ]]; then
    echo "daemon log:" >&2
    tail -120 "$DAEMON_LOG" >&2 || true
  fi
  exit 1
}

start_daemon() {
  "$BIN" daemon \
    --addr "$ADDR" \
    --artifact-addr "" \
    --cache-dir "$CACHE_DIR" \
    --data-dir "$DATA_DIR" \
    --ansible-dir "$ANSIBLE_DIR" \
    --api-token "$TOKEN" \
    --job-timeout "$JOB_TIMEOUT" \
    --backup-timeout "$BACKUP_TIMEOUT" \
    --cert-timeout "$CERT_TIMEOUT" \
    >>"$DAEMON_LOG" 2>&1 &
  PID=$!
  local deadline=$((SECONDS + 15))
  while (( SECONDS < deadline )); do
    if curl -fsS "$BASE/healthz" >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$PID" 2>/dev/null; then
      fail "daemon exited during startup"
    fi
    sleep 0.2
  done
  fail "daemon did not become healthy"
}

stop_daemon() {
  if [[ -z "$PID" ]] || ! kill -0 "$PID" 2>/dev/null; then
    PID=""
    return 0
  fi
  kill -TERM "$PID"
  local deadline=$((SECONDS + 10))
  while kill -0 "$PID" 2>/dev/null && (( SECONDS < deadline )); do
    sleep 0.1
  done
  if kill -0 "$PID" 2>/dev/null; then
    kill -KILL "$PID"
  fi
  wait "$PID" 2>/dev/null || true
  PID=""
}

api_call() {
  local method=$1
  local path=$2
  shift 2
  HTTP_STATUS=$(curl -sS -o "$RESPONSE" -w '%{http_code}' -X "$method" "$BASE$path" \
    -H "Authorization: Bearer $TOKEN" "$@") || fail "curl failed for $method $path"
}

expect_status() {
  local want=$1
  if [[ "$HTTP_STATUS" != "$want" ]]; then
    fail "expected HTTP $want, got $HTTP_STATUS"
  fi
}

jq_assert() {
  if ! jq -e "$@" "$RESPONSE" >/dev/null; then
    fail "JSON assertion failed: $*"
  fi
}

wait_job() {
  local id=$1
  local timeout_seconds=${2:-1800}
  local deadline=$((SECONDS + timeout_seconds))
  local status=""
  while (( SECONDS < deadline )); do
    api_call GET "/api/v1/jobs/$id"
    expect_status 200
    status=$(jq -r '.status' "$RESPONSE")
    case "$status" in
      succeeded)
        return 0
        ;;
      failed|cancelled|interrupted)
        api_call GET "/api/v1/jobs/$id/logs"
        echo "job $id ended with status $status" >&2
        cat "$RESPONSE" >&2 || true
        fail "job $id ended with status $status"
        ;;
    esac
    sleep 1
  done
  fail "job $id did not finish within ${timeout_seconds}s"
}

if [[ -x "$BIN" ]]; then
  echo "using prebuilt kube-do: $BIN"
else
  echo "building kube-do"
  (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -o "$BIN" ./cmd/kube-do)
fi

echo "starting daemon on $ADDR"
start_daemon

HEALTH_STATUS=$(curl -sS -o "$RESPONSE" -w '%{http_code}' "$BASE/healthz")
if [[ "$HEALTH_STATUS" != "200" ]]; then
  fail "healthz returned $HEALTH_STATUS"
fi

NO_AUTH_STATUS=$(curl -sS -o "$RESPONSE" -w '%{http_code}' "$BASE/api/v1/clusters")
if [[ "$NO_AUTH_STATUS" != "401" ]]; then
  fail "unauthenticated request returned $NO_AUTH_STATUS, expected 401"
fi

BAD_AUTH_STATUS=$(curl -sS -o "$RESPONSE" -w '%{http_code}' -H 'Authorization: Bearer wrong-token' "$BASE/api/v1/clusters")
if [[ "$BAD_AUTH_STATUS" != "401" ]]; then
  fail "bad-token request returned $BAD_AUTH_STATUS, expected 401"
fi

echo "registering cluster"
api_call POST /api/v1/clusters --data-binary "@$KUBE_DO_E2E_CONFIG" -H 'Content-Type: application/yaml'
expect_status 201
CLUSTER=$(jq -r '.name' "$RESPONSE")
[[ -n "$CLUSTER" && "$CLUSTER" != "null" ]] || fail "cluster name is empty"

api_call POST /api/v1/clusters --data-binary "@$KUBE_DO_E2E_CONFIG" -H 'Content-Type: application/yaml'
expect_status 409

api_call GET /api/v1/clusters
expect_status 200
jq_assert '.clusters | length == 1'

api_call GET "/api/v1/clusters/$CLUSTER"
expect_status 200
# shellcheck disable=SC2016
jq_assert --arg cluster "$CLUSTER" '.name == $cluster'

api_call POST "/api/v1/clusters/$CLUSTER/nodes" -H 'Content-Type: application/json' --data '{"role":"control-plane","nodes":[{"name":"e2e-cp","address":"192.0.2.10"}]}'
expect_status 400

if [[ "$RUN_OPS" == "1" ]]; then
  echo "starting etcd backup job"
  api_call POST "/api/v1/clusters/$CLUSTER/etcd-backups"
  expect_status 202
  BACKUP_JOB=$(jq -r '.id' "$RESPONSE")
  [[ -n "$BACKUP_JOB" && "$BACKUP_JOB" != "null" ]] || fail "backup job id is empty"
  CERT_JOB=""

  echo "checking same-cluster mutual exclusion"
  api_call POST "/api/v1/clusters/$CLUSTER/certs/check"
  if [[ "$HTTP_STATUS" == "409" ]]; then
    :
  elif [[ "$HTTP_STATUS" == "202" && "$REQUIRE_CONFLICT" != "1" ]]; then
    CERT_JOB=$(jq -r '.id' "$RESPONSE")
  else
    fail "expected 409 while backup is running, got $HTTP_STATUS"
  fi

  wait_job "$BACKUP_JOB" "$BACKUP_WAIT_SECONDS"
  api_call GET "/api/v1/jobs/$BACKUP_JOB"
  expect_status 200
  jq_assert '.result.validated == true and (.result.sha256 | length == 64) and (.result.snapshot.totalKey > 0)'
  BACKUP_PATH=$(jq -r '.result.path' "$RESPONSE")
  BACKUP_SHA=$(jq -r '.result.sha256' "$RESPONSE")
  [[ -f "$BACKUP_PATH" ]] || fail "backup file does not exist: $BACKUP_PATH"
  ACTUAL_SHA=$(sha256sum "$BACKUP_PATH" | awk '{print $1}')
  [[ "$ACTUAL_SHA" == "$BACKUP_SHA" ]] || fail "backup checksum mismatch"
  api_call GET "/api/v1/jobs/$BACKUP_JOB/logs"
  expect_status 200

  if [[ -z "$CERT_JOB" ]]; then
    echo "running certificate check job"
    api_call POST "/api/v1/clusters/$CLUSTER/certs/check"
    expect_status 202
    CERT_JOB=$(jq -r '.id' "$RESPONSE")
  fi
  wait_job "$CERT_JOB" "$CERT_WAIT_SECONDS"
  api_call GET "/api/v1/jobs/$CERT_JOB"
  expect_status 200
  jq_assert '.result.certificateCount > 0 and (.result.nodes | length > 0) and (.result | has("healthy"))'
  api_call GET "/api/v1/jobs/$CERT_JOB/logs"
  expect_status 200
  [[ -s "$RESPONSE" ]] || fail "certificate job log is empty"

  echo "cancelling a completed job"
  api_call POST "/api/v1/jobs/$CERT_JOB/cancel"
  expect_status 409

  echo "restarting daemon and checking persisted job history"
  stop_daemon
  start_daemon
  for job in "$BACKUP_JOB" "$CERT_JOB"; do
    api_call GET "/api/v1/jobs/$job"
    expect_status 200
    jq_assert '.status == "succeeded" and (.result != null)'
    api_call GET "/api/v1/jobs/$job/logs"
    expect_status 200
    [[ -s "$RESPONSE" ]] || fail "persisted log for job $job is empty"
  done
fi

if [[ -n "${KUBE_DO_E2E_NEW_WORKERS:-}" ]]; then
  echo "adding workers through the API"
  api_call POST "/api/v1/clusters/$CLUSTER/nodes" -H 'Content-Type: application/json' --data "$KUBE_DO_E2E_NEW_WORKERS"
  expect_status 202
  ADD_JOB=$(jq -r '.id' "$RESPONSE")
  wait_job "$ADD_JOB" 1800
fi

if [[ "${KUBE_DO_E2E_RUN_INSTALL:-0}" == "1" ]]; then
  echo "running install through the API"
  INSTALL_BODY=${KUBE_DO_E2E_INSTALL_BODY:-'{"prepare":false,"check":false}'}
  api_call POST "/api/v1/clusters/$CLUSTER/install" -H 'Content-Type: application/json' --data "$INSTALL_BODY"
  expect_status 202
  INSTALL_JOB=$(jq -r '.id' "$RESPONSE")
  wait_job "$INSTALL_JOB" 7200
fi

echo "E2E PASSED: cluster=$CLUSTER work_dir=$WORK_ROOT"
