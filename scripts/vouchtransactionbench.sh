#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BENCH_DIR="$(mktemp -d "${TMPDIR:-/tmp}/vouch-transaction-bench.XXXXXX")"
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
go build -o "$VOUCH_BIN" ./cmd/vouch
"$VOUCH_BIN" --repo "$REPO_DIR" runtime init >/dev/null
start_daemon

"$VOUCH_BIN" --repo "$REPO_DIR" --json tx create \
  --id tx:bench-001 \
  --namespace bench \
  --intent "Change authentication behavior with independent verification" \
  --run run:bench-agent \
  --socket "$SOCKET_PATH" >"$BENCH_DIR/create.json"
[[ "$(jq -r '.transaction.state' "$BENCH_DIR/create.json")" == "created" ]] || fail "transaction was not created"
pass "task-scoped transaction created with human-intent digest"

"$VOUCH_BIN" --repo "$REPO_DIR" --json tx start \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/start.json"
[[ "$(jq -r '.transaction.state' "$BENCH_DIR/start.json")" == "running" ]] || fail "transaction did not start"
pass "transaction entered the durable running state"

"$VOUCH_BIN" --repo "$REPO_DIR" --json tx worktree \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/worktree.json"
WORKTREE="$(jq -r '.workspace.path' "$BENCH_DIR/worktree.json")"
[[ -d "$WORKTREE" ]] || fail "isolated worktree was not created"
case "$WORKTREE" in
  "$REPO_DIR"|"$REPO_DIR"/*) fail "transaction worktree is inside the source repository" ;;
esac
pass "detached worktree created outside the source repository"

printf 'package auth\n\nfunc Allowed() bool { return true }\n' >"$WORKTREE/internal/auth/middleware.go"
printf 'package auth\n\nfunc TestAllowed() { /* rewritten */ }\n' >"$WORKTREE/internal/auth/middleware_test.go"
[[ "$(sed -n '3p' "$REPO_DIR/internal/auth/middleware.go")" == 'func Allowed() bool { return false }' ]] || fail "source repository changed before release"
pass "agent changes remained private to the transaction boundary"

"$VOUCH_BIN" --repo "$REPO_DIR" --json tx stage \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/stage.json"
[[ "$(jq '.projection.effects | length' "$BENCH_DIR/stage.json")" == "2" ]] || fail "effect ledger did not contain two effects"
[[ "$(jq -r '.projection.transaction.state' "$BENCH_DIR/stage.json")" == "staged" ]] || fail "transaction did not become staged"
pass "Git diff normalized into an ordered two-effect ledger"

jq -r '.projection.effects[].resource.pattern' "$BENCH_DIR/stage.json" >"$BENCH_DIR/effect-paths"
diff -u <(printf 'internal/auth/middleware.go\ninternal/auth/middleware_test.go\n') "$BENCH_DIR/effect-paths" >/dev/null || fail "effect inventory did not match the exact diff"
pass "effect inventory exactly matched the staged resource changes"

printf 'package auth\n\nfunc Allowed() bool { panic("changed after freeze") }\n' >"$WORKTREE/internal/auth/middleware.go"
if "$VOUCH_BIN" --repo "$REPO_DIR" --json tx validate \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/tamper.json" 2>"$BENCH_DIR/tamper.err"; then
  fail "post-freeze mutation was accepted"
fi
grep -q 'TRANSACTION_CONFLICT' "$BENCH_DIR/tamper.err" || fail "tamper rejection did not expose the stable error code"
[[ "$("$VOUCH_BIN" --repo "$REPO_DIR" --json tx get --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" | jq -r '.transaction.state')" == "staged" ]] || fail "failed validation mutated transaction state"
pass "post-freeze worktree mutation was rejected without state change"

printf 'package auth\n\nfunc Allowed() bool { return true }\n' >"$WORKTREE/internal/auth/middleware.go"
"$VOUCH_BIN" --repo "$REPO_DIR" --json tx validate \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/validate.json"
[[ "$(jq -r '.decision.outcome' "$BENCH_DIR/validate.json")" == "require_approval" ]] || fail "combined control/test sequence did not require approval"
jq -e '.decision.findings[] | select(.rule_id == "gatemole.sequence.control-and-evidence-coupling")' "$BENCH_DIR/validate.json" >/dev/null || fail "composition finding missing"
pass "sequence policy caught control-and-evidence coupling"

"$VOUCH_BIN" --repo "$REPO_DIR" --json tx get \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/before-restart.json"
EVENT_COUNT="$("$VOUCH_BIN" --repo "$REPO_DIR" --json tx events --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" | jq 'length')"
[[ "$EVENT_COUNT" == "7" ]] || fail "unexpected transaction event count: $EVENT_COUNT"
pass "append-only transaction history captured all seven transitions and effects"

stop_daemon
start_daemon
"$VOUCH_BIN" --repo "$REPO_DIR" --json tx get \
  --namespace bench --id tx:bench-001 --socket "$SOCKET_PATH" >"$BENCH_DIR/after-restart.json"
jq -S '{transaction,effects,last_event_digest}' "$BENCH_DIR/before-restart.json" >"$BENCH_DIR/before-projection.json"
jq -S '{transaction,effects,last_event_digest}' "$BENCH_DIR/after-restart.json" >"$BENCH_DIR/after-projection.json"
diff -u "$BENCH_DIR/before-projection.json" "$BENCH_DIR/after-projection.json" >/dev/null || fail "transaction projection changed across daemon restart"
pass "daemon restart recovered a byte-equivalent transaction projection"

[[ "$(sed -n '3p' "$REPO_DIR/internal/auth/middleware.go")" == 'func Allowed() bool { return false }' ]] || fail "source repository changed during benchmark"
[[ "$PASSED" == "$TOTAL" ]] || fail "benchmark completed with $PASSED/$TOTAL checks"
echo "VouchTransactionBench: $PASSED/$TOTAL passed"
