#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH_DIR="$(mktemp -d "${TMPDIR:-/tmp}/vouch-runtime-bench.XXXXXX")"
REPO_DIR="$BENCH_DIR/repository"
STAGE_DIR="$BENCH_DIR/stages"
DB_PATH="$BENCH_DIR/kernel.db"
SOCKET_PATH="$BENCH_DIR/vouchd.sock"
VOUCH_BIN="$BENCH_DIR/vouch"
DAEMON_LOG="$BENCH_DIR/vouchd.log"
DAEMON_PID=""
PASSED=0
TOTAL=10

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

start_daemon() {
  "$VOUCH_BIN" --repo "$REPO_DIR" daemon \
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
BASE_COMMIT="$(printf 'base\n' | git -C "$REPO_DIR" -c user.name='Vouch Bench' -c user.email='vouch-bench@example.invalid' commit-tree "$BASE_TREE" -F -)"
git -C "$REPO_DIR" update-ref refs/heads/main "$BASE_COMMIT"

cd "$ROOT_DIR"
env GOCACHE="$BENCH_DIR/go-cache" go build -o "$VOUCH_BIN" ./cmd/vouch
"$VOUCH_BIN" --repo "$REPO_DIR" runtime init >/dev/null
start_daemon

COMMAND_SECRET="runtime-command-secret-not-for-ledger"
"$VOUCH_BIN" --repo "$REPO_DIR" --json tx run \
  --id tx:runtime-success \
  --namespace bench \
  --intent "Change authentication behavior with independent verification" \
  --run run:runtime-success \
  --runtime host \
  --unsafe-host \
  --socket "$SOCKET_PATH" \
  -- \
  /bin/sh -c \
  "printf 'package auth\\n\\nfunc Allowed() bool { return true }\\n' > internal/auth/middleware.go; printf 'package auth\\n\\nfunc TestAllowed() { /* rewritten */ }\\n' > internal/auth/middleware_test.go; : '$COMMAND_SECRET'" \
  >"$BENCH_DIR/success.json" 2>"$BENCH_DIR/success.stderr"
[[ "$(jq -r '.execution.status' "$BENCH_DIR/success.json")" == "succeeded" ]] || fail "one-command transaction did not supervise the child"
pass "one command created, ran, staged, and validated the transaction"

[[ "$(jq -r '.execution.exit_code' "$BENCH_DIR/success.json")" == "0" ]] || fail "successful receipt did not bind exit code 0"
jq -e '.execution.stdout_digest | test("^sha256:[a-f0-9]{64}$")' "$BENCH_DIR/success.json" >/dev/null || fail "stdout digest missing"
jq -e '.execution.stderr_digest | test("^sha256:[a-f0-9]{64}$")' "$BENCH_DIR/success.json" >/dev/null || fail "stderr digest missing"
pass "daemon-authored execution receipt bound exit and output digests"

WORKTREE="$(jq -r '.workspace' "$BENCH_DIR/success.json")"
[[ -d "$WORKTREE" ]] || fail "runtime worktree does not exist"
case "$WORKTREE" in
  "$REPO_DIR"|"$REPO_DIR"/*) fail "runtime worktree is inside the source repository" ;;
esac
pass "agent process ran from a detached worktree outside the source repository"

[[ "$(jq '.projection.effects | length' "$BENCH_DIR/success.json")" == "2" ]] || fail "runtime effect ledger did not contain two effects"
jq -r '.projection.effects[].resource.pattern' "$BENCH_DIR/success.json" >"$BENCH_DIR/effect-paths"
diff -u <(printf 'internal/auth/middleware.go\ninternal/auth/middleware_test.go\n') "$BENCH_DIR/effect-paths" >/dev/null || fail "runtime effects did not match the exact Git change"
pass "successful child changes became an exact ordered effect ledger"

[[ "$(jq -r '.decision.outcome' "$BENCH_DIR/success.json")" == "require_approval" ]] || fail "runtime sequence policy missed control/evidence coupling"
jq -e '.decision.findings[] | select(.rule_id == "vouch.sequence.control-and-evidence-coupling")' "$BENCH_DIR/success.json" >/dev/null || fail "runtime finding missing"
pass "combined control and test changes required focused approval"

"$VOUCH_BIN" --repo "$REPO_DIR" --json tx events \
  --namespace bench --id tx:runtime-success --socket "$SOCKET_PATH" >"$BENCH_DIR/events.json"
[[ "$(jq 'length' "$BENCH_DIR/events.json")" == "9" ]] || fail "unexpected runtime event count"
if grep -q "$COMMAND_SECRET" "$BENCH_DIR/events.json"; then
  fail "raw supervised command arguments entered the transaction ledger"
fi
pass "execution start and finish were durable without retaining raw command arguments"

[[ "$(sed -n '3p' "$REPO_DIR/internal/auth/middleware.go")" == 'func Allowed() bool { return false }' ]] || fail "runtime changed the source repository"
pass "source repository remained unchanged after successful supervision"

set +e
"$VOUCH_BIN" --repo "$REPO_DIR" --json tx run \
  --id tx:runtime-failure \
  --namespace bench \
  --intent "Attempt a change that fails" \
  --run run:runtime-failure \
  --runtime host \
  --unsafe-host \
  --socket "$SOCKET_PATH" \
  -- /bin/sh -c 'exit 7' \
  >"$BENCH_DIR/failure.json" 2>"$BENCH_DIR/failure.stderr"
FAILURE_CODE=$?
set -e
[[ "$FAILURE_CODE" == "7" ]] || fail "failed child exit code was not propagated: $FAILURE_CODE"
pass "failed agent command propagated its exit code"

[[ "$(jq -r '.execution.status' "$BENCH_DIR/failure.json")" == "failed" ]] || fail "failed execution status missing"
[[ "$(jq -r '.projection.transaction.state' "$BENCH_DIR/failure.json")" == "running" ]] || fail "failed execution advanced transaction authority"
[[ "$(jq '.projection.effects | length' "$BENCH_DIR/failure.json")" == "0" ]] || fail "failed execution staged effects"
pass "failed agent remained durable and did not stage or validate effects"

"$VOUCH_BIN" --repo "$REPO_DIR" --json tx get \
  --namespace bench --id tx:runtime-success --socket "$SOCKET_PATH" >"$BENCH_DIR/before-restart.json"
stop_daemon
start_daemon
"$VOUCH_BIN" --repo "$REPO_DIR" --json tx get \
  --namespace bench --id tx:runtime-success --socket "$SOCKET_PATH" >"$BENCH_DIR/after-restart.json"
jq -S . "$BENCH_DIR/before-restart.json" >"$BENCH_DIR/before-restart.sorted.json"
jq -S . "$BENCH_DIR/after-restart.json" >"$BENCH_DIR/after-restart.sorted.json"
diff -u "$BENCH_DIR/before-restart.sorted.json" "$BENCH_DIR/after-restart.sorted.json" >/dev/null || fail "runtime transaction changed across daemon restart"
pass "daemon restart recovered the execution receipt and transaction projection"

[[ "$PASSED" == "$TOTAL" ]] || fail "runtime benchmark completed with $PASSED/$TOTAL checks"
echo "VouchRuntimeBench: $PASSED/$TOTAL passed"
