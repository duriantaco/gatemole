#!/usr/bin/env bash
set -euo pipefail

for command in docker git go jq; do
  command -v "$command" >/dev/null || {
    echo "missing required command: $command" >&2
    exit 1
  }
done

DEMO_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/gatemole-paired-demo.XXXXXX")"
REPOSITORY="$DEMO_ROOT/payments-api"
RUNTIME_ROOT="$DEMO_ROOT/runtime"
SOCKET_PATH="$RUNTIME_ROOT/gatemoled.sock"
DATABASE_PATH="$RUNTIME_ROOT/kernel.db"
TRANSACTION_ROOT="$RUNTIME_ROOT/transactions"
GATEMOLE_BIN="$DEMO_ROOT/gatemole"
GATEMOLED_BIN="$DEMO_ROOT/gatemoled"
DAEMON_LOG="$RUNTIME_ROOT/gatemoled.log"
DAEMON_PID=""
IMAGE_TAG="gatemole-paired-execution-demo:run-$$"

cleanup() {
  if [[ -n "$DAEMON_PID" ]] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  docker image rm "$IMAGE_TAG" >/dev/null 2>&1 || true
  rm -rf "$DEMO_ROOT"
}
trap cleanup EXIT

fail() {
  echo "paired execution demo failed: $1" >&2
  if [[ -d "$TRANSACTION_ROOT/.gatemole-agent-evidence" ]]; then
    while IFS= read -r evidence; do
      echo "--- $evidence" >&2
      tail -80 "$evidence" >&2 || true
    done < <(find "$TRANSACTION_ROOT/.gatemole-agent-evidence" -name stderr.log -type f | sort)
  fi
  if [[ -f "$DAEMON_LOG" ]]; then
    tail -80 "$DAEMON_LOG" >&2 || true
  fi
  exit 1
}

progress() {
  printf '\n==> %s\n' "$1" >&2
}

start_daemon() {
  mkdir -p "$RUNTIME_ROOT"
  "$GATEMOLED_BIN" \
    --repo "$REPOSITORY" \
    --db "$DATABASE_PATH" \
    --socket "$SOCKET_PATH" \
    --transaction-root "$TRANSACTION_ROOT" \
    >"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in $(seq 1 100); do
    [[ -S "$SOCKET_PATH" ]] && return 0
    kill -0 "$DAEMON_PID" 2>/dev/null || fail "daemon exited before opening its socket"
    sleep 0.05
  done
  fail "daemon did not become ready"
}

stop_daemon() {
  kill "$DAEMON_PID"
  wait "$DAEMON_PID" || true
  DAEMON_PID=""
  for _ in $(seq 1 100); do
    [[ ! -e "$SOCKET_PATH" ]] && return 0
    sleep 0.05
  done
  fail "daemon socket remained after shutdown"
}

progress "Creating a temporary payments API with a failing refresh-token test"
mkdir -p "$REPOSITORY/internal/auth"
git -C "$REPOSITORY" init --initial-branch=main >/dev/null
printf 'module example.com/payments-api\n\ngo 1.26\n' >"$REPOSITORY/go.mod"
printf 'package auth\n\nfunc RefreshAllowed() bool { return false }\n' \
  >"$REPOSITORY/internal/auth/refresh.go"
printf 'package auth\n\nimport "testing"\n\nfunc TestRefreshAllowed(t *testing.T) {\n\tif !RefreshAllowed() {\n\t\tt.Fatal("valid refresh token was rejected")\n\t}\n}\n' \
  >"$REPOSITORY/internal/auth/refresh_test.go"
git -C "$REPOSITORY" add -- go.mod internal/auth
git -C "$REPOSITORY" \
  -c user.name='Payments Developer' \
  -c user.email='payments@example.invalid' \
  commit -m 'reproduce PAY-1842 refresh-token rejection' >/dev/null

AGENT_SOURCE="$DEMO_ROOT/auth-hotfix-agent"
mkdir -p "$AGENT_SOURCE"
cat >"$AGENT_SOURCE/main.go" <<'GO'
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
)

func main() {
	if _, err := os.ReadFile(os.Getenv("GATEMOLE_TASK_PATH")); err != nil {
		panic(err)
	}
	path := "/workspace/internal/auth/refresh.go"
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	if bytes.Contains(data, []byte("return false")) {
		updated := bytes.Replace(data, []byte("return false"), []byte("return true"), 1)
		if err := os.WriteFile(path, updated, 0o600); err != nil {
			panic(err)
		}
		fmt.Fprintln(os.Stderr, "candidate patch saved; simulated agent crash before verification")
		os.Exit(42)
	}
	if !bytes.Contains(data, []byte("return true")) {
		panic("candidate patch is not in the expected state")
	}
	testRoot := "/workspace/.gatemole-demo-test"
	if err := os.MkdirAll(testRoot+"/tmp", 0o700); err != nil {
		panic(err)
	}
	defer os.RemoveAll(testRoot)
	command := exec.Command("go", "test", "./internal/auth")
	command.Dir = "/workspace"
	command.Env = append(
		os.Environ(),
		"GOCACHE="+testRoot+"/cache",
		"GOTMPDIR="+testRoot+"/tmp",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "regression test failed: %v\n%s", err, output)
		os.Exit(1)
	}
	fmt.Print(string(output))
	fmt.Println("resumed the existing candidate patch; regression test passed")
}
GO

progress "Building the digest-pinned agent image (the first run may download golang:1.26-alpine)"
case "$(docker version --format '{{.Server.Arch}}')" in
  amd64|x86_64) ENGINE_ARCH=amd64 ;;
  arm64|aarch64) ENGINE_ARCH=arm64 ;;
  *) fail "Docker server architecture is not supported by this demo" ;;
esac
CGO_ENABLED=0 GOOS=linux GOARCH="$ENGINE_ARCH" \
  go build -trimpath -o "$AGENT_SOURCE/auth-hotfix-agent" "$AGENT_SOURCE/main.go"
cat >"$AGENT_SOURCE/Dockerfile" <<'DOCKER'
FROM golang:1.26-alpine
COPY auth-hotfix-agent /auth-hotfix-agent
DOCKER
docker build --quiet --network=none --pull=false --tag "$IMAGE_TAG" "$AGENT_SOURCE" >/dev/null
AGENT_IMAGE="$(docker image inspect --format '{{.Id}}' "$IMAGE_TAG")"

progress "Building gatemole and gatemoled from this checkout"
go build -o "$GATEMOLE_BIN" ./cmd/gatemole
go build -o "$GATEMOLED_BIN" ./cmd/gatemoled
"$GATEMOLE_BIN" --repo "$REPOSITORY" runtime init >/dev/null

progress "Starting gatemoled with a fresh durable ledger"
start_daemon

RUN_ARGUMENTS=(
  --socket "$SOCKET_PATH"
  --namespace payments
  --id tx:pay-1842
  --run run:pay-1842
  --timeout 2m
  --intent "Fix PAY-1842: valid refresh tokens are rejected"
  --runtime oci
  --image "$AGENT_IMAGE"
  -- /auth-hotfix-agent
)

progress "Attempt 1: the agent saves the auth fix, then simulates a crash"
set +e
"$GATEMOLE_BIN" --repo "$REPOSITORY" --json run "${RUN_ARGUMENTS[@]}" \
  >"$DEMO_ROOT/first-attempt.json" 2>"$DEMO_ROOT/first-attempt.err"
FIRST_EXIT=$?
set -e
[[ "$FIRST_EXIT" -eq 42 ]] || fail "first agent exit=$FIRST_EXIT, want 42"
jq -e '.execution.status == "failed"' "$DEMO_ROOT/first-attempt.json" >/dev/null \
  || fail "first execution was not durably failed"

"$GATEMOLE_BIN" --repo "$REPOSITORY" --json kernel run get \
  --socket "$SOCKET_PATH" --namespace payments --id run:pay-1842 \
  >"$DEMO_ROOT/run-after-failure.json"
jq -e '.run.state == "waiting_for_agent" and .run.active_execution_id == null' \
  "$DEMO_ROOT/run-after-failure.json" >/dev/null \
  || fail "failed execution did not settle the run for retry"
printf '    receipt=failed  run=waiting_for_agent  active_execution=null\n' >&2

progress "Restarting gatemoled against the same ledger and transaction worktree"
stop_daemon
start_daemon

progress "Attempt 2: the agent resumes the saved patch and runs go test ./internal/auth"
set +e
"$GATEMOLE_BIN" --repo "$REPOSITORY" --json run "${RUN_ARGUMENTS[@]}" \
  >"$DEMO_ROOT/retry.json" 2>"$DEMO_ROOT/retry.err"
RETRY_EXIT=$?
set -e
[[ "$RETRY_EXIT" -eq 0 ]] || fail "retry agent exit=$RETRY_EXIT, want 0"
jq -e '.execution.status == "succeeded"' "$DEMO_ROOT/retry.json" >/dev/null \
  || fail "resumed execution did not succeed"
printf '    receipt=succeeded  regression_test=passed\n' >&2

progress "Inspecting both immutable receipts, the paired run events, and budget usage"
"$GATEMOLE_BIN" --repo "$REPOSITORY" --json kernel run get \
  --socket "$SOCKET_PATH" --namespace payments --id run:pay-1842 \
  >"$DEMO_ROOT/run-after-retry.json"
"$GATEMOLE_BIN" --repo "$REPOSITORY" --json kernel run events \
  --socket "$SOCKET_PATH" --namespace payments --id run:pay-1842 \
  >"$DEMO_ROOT/run-events.json"
"$GATEMOLE_BIN" --repo "$REPOSITORY" --json tx get \
  --socket "$SOCKET_PATH" --namespace payments --id tx:pay-1842 \
  >"$DEMO_ROOT/transaction.json"

jq -e '.run.state == "waiting_for_event" and .run.active_execution_id == null' \
  "$DEMO_ROOT/run-after-retry.json" >/dev/null \
  || fail "successful retry did not settle the run"
jq -e '[.[] | select(.type | startswith("run.execution_"))] | length == 4' \
  "$DEMO_ROOT/run-events.json" >/dev/null \
  || fail "run does not contain two paired execution attempts"
jq -e '[.executions[].status] == ["failed", "succeeded"]' \
  "$DEMO_ROOT/transaction.json" >/dev/null \
  || fail "transaction receipts do not preserve both attempts"
git -C "$REPOSITORY" diff --exit-code >/dev/null \
  || fail "agent changed the source checkout instead of its private worktree"

progress "Demo complete; durable incident report"
jq -n \
  --slurpfile failed "$DEMO_ROOT/run-after-failure.json" \
  --slurpfile resumed "$DEMO_ROOT/run-after-retry.json" \
  --slurpfile events "$DEMO_ROOT/run-events.json" \
  --slurpfile transaction "$DEMO_ROOT/transaction.json" \
  '{
    incident: "PAY-1842 refresh-token rejection",
    first_attempt: $transaction[0].executions[0].status,
    state_after_failure: $failed[0].run.state,
    daemon_restarted: true,
    retry: $transaction[0].executions[1].status,
    regression_test: "go test ./internal/auth (passed on retry)",
    final_run_state: $resumed[0].run.state,
    active_execution_id: ($resumed[0].run.active_execution_id // null),
    cumulative_budget_usage: $resumed[0].run.budget_usage,
    paired_execution_events: [
      $events[0][]
      | select(.type | startswith("run.execution_"))
      | {sequence, type, execution_id: .payload.execution_id,
         status: .payload.status, usage: .payload.usage}
    ],
    source_checkout_unchanged: true
  }'
