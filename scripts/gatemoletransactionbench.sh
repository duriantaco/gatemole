#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH_DIR="$(mktemp -d "${TMPDIR:-/tmp}/gatemole-transaction-bench.XXXXXX")"
REPO_DIR="$BENCH_DIR/repository"
STAGE_DIR="$BENCH_DIR/stages"
DB_PATH="$BENCH_DIR/kernel.db"
SOCKET_PATH="$BENCH_DIR/gatemoled.sock"
GATEMOLE_BIN="$BENCH_DIR/gatemole"
DAEMON_LOG="$BENCH_DIR/gatemoled.log"
DAEMON_PID=""
PASSED=0
TOTAL=10
BENCH_RUNTIME_CONFIG_DIGEST="sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
EMPTY_OUTPUT_DIGEST="sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

cleanup() {
  if [[ -n "$DAEMON_PID" ]] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  rm -rf "$BENCH_DIR"
}
trap cleanup EXIT

pass() {
  PASSED=$((PASSED + 1))
  echo "PASS $PASSED/$TOTAL: $1"
}

fail() {
  echo "FAIL: $1" >&2
  if [[ -f "$DAEMON_LOG" ]]; then
    tail -80 "$DAEMON_LOG" >&2 || true
  fi
  exit 1
}

kernel_post() {
  local path="$1"
  local request_file="$2"
  local response_file="$3"
  curl --silent --show-error --fail-with-body \
    --unix-socket "$SOCKET_PATH" \
    --header "Content-Type: application/json" \
    --header "Gatemole-Runtime-ID: $RUNTIME_ID" \
    --data-binary "@$request_file" \
    "http://gatemoled$path" >"$response_file" || fail "kernel request failed: $path"
}

command -v curl >/dev/null || fail "curl is required for external-execution benchmark requests"

start_daemon() {
  "$GATEMOLE_BIN" --repo "$REPO_DIR" daemon \
    --db "$DB_PATH" \
    --socket "$SOCKET_PATH" \
    --transaction-root "$STAGE_DIR" \
    --allow-unsafe-host-execution >"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  for _ in $(seq 1 100); do
    if [[ -S "$SOCKET_PATH" ]]; then
      return 0
    fi
    if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
      fail "daemon exited before creating its socket"
    fi
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

mkdir -p "$REPO_DIR/internal/auth"
git -C "$REPO_DIR" init --initial-branch=main >/dev/null
printf 'package auth\n\nfunc Allowed() bool { return false }\n' >"$REPO_DIR/internal/auth/middleware.go"
printf 'package auth\n\nfunc TestAllowed() {}\n' >"$REPO_DIR/internal/auth/middleware_test.go"
git -C "$REPO_DIR" add -- internal/auth/middleware.go internal/auth/middleware_test.go
BASE_TREE="$(git -C "$REPO_DIR" write-tree)"
BASE_COMMIT="$(printf 'base\n' | git -C "$REPO_DIR" -c user.name='Gatemole Bench' -c user.email='gatemole-bench@example.invalid' commit-tree "$BASE_TREE" -F -)"
git -C "$REPO_DIR" update-ref refs/heads/main "$BASE_COMMIT"

cd "$ROOT_DIR"
go build -o "$GATEMOLE_BIN" ./cmd/gatemole
"$GATEMOLE_BIN" --repo "$REPO_DIR" runtime init >/dev/null
RUNTIME_ID="$(jq -r '.runtime_id' "$REPO_DIR/.gatemole/runtime.json")"
start_daemon

"$GATEMOLE_BIN" --repo "$REPO_DIR" --json tx create \
  --id tx:bench-001 \
  --namespace bench \
  --intent "Change authentication behavior with independent verification" \
  --run run:bench-agent \
  --socket "$SOCKET_PATH" >"$BENCH_DIR/create.json"
[[ "$(jq -r '.transaction.state' "$BENCH_DIR/create.json")" == "created" ]] || fail "transaction was not created"
pass "task-scoped transaction created with human-intent digest"

"$GATEMOLE_BIN" --repo "$REPO_DIR" --json tx start \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/start.json"
[[ "$(jq -r '.transaction.state' "$BENCH_DIR/start.json")" == "running" ]] || fail "transaction did not start"
pass "transaction entered the durable running state"

"$GATEMOLE_BIN" --repo "$REPO_DIR" --json tx worktree \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/worktree.json"
WORKTREE="$(jq -r '.workspace.path' "$BENCH_DIR/worktree.json")"
[[ -d "$WORKTREE" ]] || fail "isolated worktree was not created"
case "$WORKTREE" in
  "$REPO_DIR"|"$REPO_DIR"/*) fail "transaction worktree is inside the source repository" ;;
esac
pass "detached worktree created outside the source repository"

RUN_ID="$(jq -r '.transaction.task.run_id' "$BENCH_DIR/create.json")"
COMMAND_DIGEST="$(jq -r '.transaction.task.agent_profile.command_digest' "$BENCH_DIR/create.json")"
EXPECTED_SEQUENCE="$(jq -r '.projection.transaction.event_sequence' "$BENCH_DIR/worktree.json")"
jq -n \
  --argjson expected_sequence "$EXPECTED_SEQUENCE" \
  --arg run_id "$RUN_ID" \
  --arg command_digest "$COMMAND_DIGEST" \
  --arg runtime_config_digest "$BENCH_RUNTIME_CONFIG_DIGEST" \
  '{
    expected_sequence: $expected_sequence,
    actor: {id: "operator:bench", kind: "operator"},
    run_id: $run_id,
    program: "gatemole-manual-transaction",
    command_digest: $command_digest,
    runtime_class: "host",
    runtime_config_digest: $runtime_config_digest
  }' >"$BENCH_DIR/execution-start-request.json"
kernel_post \
  "/v0/namespaces/bench/transactions/tx:bench-001/executions/start" \
  "$BENCH_DIR/execution-start-request.json" \
  "$BENCH_DIR/execution-start.json"
EXECUTION_ID="$(jq -r '.executions[-1].id' "$BENCH_DIR/execution-start.json")"
[[ "$(jq -r '.executions[-1].status' "$BENCH_DIR/execution-start.json")" == "running" ]] || fail "manual execution did not start"

printf 'package auth\n\nfunc Allowed() bool { return true }\n' >"$WORKTREE/internal/auth/middleware.go"
printf 'package auth\n\nfunc TestAllowed() { /* rewritten */ }\n' >"$WORKTREE/internal/auth/middleware_test.go"
jq -n \
  --argjson expected_sequence "$(jq -r '.transaction.event_sequence' "$BENCH_DIR/execution-start.json")" \
  --arg execution_id "$EXECUTION_ID" \
  --arg stdout_digest "$EMPTY_OUTPUT_DIGEST" \
  --arg stderr_digest "$EMPTY_OUTPUT_DIGEST" \
  '{
    expected_sequence: $expected_sequence,
    actor: {id: "operator:bench", kind: "operator"},
    execution_id: $execution_id,
    status: "succeeded",
    exit_code: 0,
    stdout_digest: $stdout_digest,
    stderr_digest: $stderr_digest
  }' >"$BENCH_DIR/execution-finish-request.json"
kernel_post \
  "/v0/namespaces/bench/transactions/tx:bench-001/executions/finish" \
  "$BENCH_DIR/execution-finish-request.json" \
  "$BENCH_DIR/execution-finish.json"
[[ "$(jq -r '.executions[-1].status' "$BENCH_DIR/execution-finish.json")" == "succeeded" ]] || fail "manual execution did not finish successfully"
[[ "$(sed -n '3p' "$REPO_DIR/internal/auth/middleware.go")" == 'func Allowed() bool { return false }' ]] || fail "source repository changed before release"
pass "successful agent changes remained private to the transaction boundary"

"$GATEMOLE_BIN" --repo "$REPO_DIR" --json tx stage \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/stage.json"
[[ "$(jq '.projection.effects | length' "$BENCH_DIR/stage.json")" == "2" ]] || fail "effect ledger did not contain two effects"
[[ "$(jq --arg execution_id "$EXECUTION_ID" '[.projection.effects[].origin_execution_id == $execution_id] | all' "$BENCH_DIR/stage.json")" == "true" ]] || fail "effects were not bound to the successful execution"
[[ "$(jq -r '.projection.transaction.state' "$BENCH_DIR/stage.json")" == "staged" ]] || fail "transaction did not become staged"
pass "Git diff normalized into an execution-bound two-effect ledger"

jq -r '.projection.effects[].resource.pattern' "$BENCH_DIR/stage.json" >"$BENCH_DIR/effect-paths"
diff -u <(printf 'internal/auth/middleware.go\ninternal/auth/middleware_test.go\n') "$BENCH_DIR/effect-paths" >/dev/null || fail "effect inventory did not match the exact diff"
pass "effect inventory exactly matched the staged resource changes"

printf 'package auth\n\nfunc Allowed() bool { panic("changed after freeze") }\n' >"$WORKTREE/internal/auth/middleware.go"
if "$GATEMOLE_BIN" --repo "$REPO_DIR" --json tx validate \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/tamper.json" 2>"$BENCH_DIR/tamper.err"; then
  fail "post-freeze mutation was accepted"
fi
grep -q 'TRANSACTION_CONFLICT' "$BENCH_DIR/tamper.err" || fail "tamper rejection did not expose the stable error code"
[[ "$("$GATEMOLE_BIN" --repo "$REPO_DIR" --json tx get --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" | jq -r '.transaction.state')" == "staged" ]] || fail "failed validation mutated transaction state"
pass "post-freeze worktree mutation was rejected without state change"

printf 'package auth\n\nfunc Allowed() bool { return true }\n' >"$WORKTREE/internal/auth/middleware.go"
"$GATEMOLE_BIN" --repo "$REPO_DIR" --json tx validate \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/validate.json"
[[ "$(jq -r '.decision.outcome' "$BENCH_DIR/validate.json")" == "require_approval" ]] || fail "combined control/test sequence did not require approval"
jq -e '.decision.findings[] | select(.rule_id == "gatemole.sequence.control-and-evidence-coupling")' "$BENCH_DIR/validate.json" >/dev/null || fail "composition finding missing"
pass "sequence policy caught control-and-evidence coupling"

"$GATEMOLE_BIN" --repo "$REPO_DIR" --json tx get \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/before-restart.json"
EVENT_COUNT="$("$GATEMOLE_BIN" --repo "$REPO_DIR" --json tx events --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" | jq 'length')"
[[ "$EVENT_COUNT" == "9" ]] || fail "unexpected transaction event count: $EVENT_COUNT"
pass "append-only transaction history captured execution, transitions, and effects"

stop_daemon
start_daemon
"$GATEMOLE_BIN" --repo "$REPO_DIR" --json tx get \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/after-restart.json"
jq -S '{transaction,effects,last_event_digest}' "$BENCH_DIR/before-restart.json" >"$BENCH_DIR/before-projection.json"
jq -S '{transaction,effects,last_event_digest}' "$BENCH_DIR/after-restart.json" >"$BENCH_DIR/after-projection.json"
diff -u "$BENCH_DIR/before-projection.json" "$BENCH_DIR/after-projection.json" >/dev/null || fail "transaction projection changed across daemon restart"
pass "daemon restart recovered a byte-equivalent transaction projection"

[[ "$(sed -n '3p' "$REPO_DIR/internal/auth/middleware.go")" == 'func Allowed() bool { return false }' ]] || fail "source repository changed during benchmark"
[[ "$PASSED" == "$TOTAL" ]] || fail "benchmark completed with $PASSED/$TOTAL checks"
echo "GatemoleTransactionBench: $PASSED/$TOTAL passed"
