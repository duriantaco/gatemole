#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLANNER="$ROOT/scripts/gatemoleci-plan.sh"
TEST_DIR="$(mktemp -d "${TMPDIR:-/tmp}/gatemoleci-plan-test.XXXXXX")"
trap 'rm -rf "$TEST_DIR"' EXIT

run_plan() {
  local name="$1"
  shift
  printf '%s\n' "$@" > "$TEST_DIR/$name.paths"
  "$PLANNER" --paths "$TEST_DIR/$name.paths"
}

assert_output() {
  local output="$1"
  local expected="$2"
  if ! grep -qx "$expected" <<< "$output"; then
    echo "gatemoleci-plan-test: expected '$expected' in:" >&2
    echo "$output" >&2
    exit 1
  fi
}

touch "$TEST_DIR/empty.paths"
if "$PLANNER" --paths "$TEST_DIR/empty.paths" >/dev/null 2>&1; then
  echo "gatemoleci-plan-test: an empty change set must fail closed" >&2
  exit 1
fi

docs_plan="$(run_plan docs docs/PRODUCTION.md README.md)"
assert_output "$docs_plan" "docs=true"
assert_output "$docs_plan" "gatemolebench=false"
assert_output "$docs_plan" "production=false"

contracts_plan="$(run_plan contracts internal/gatemole/compiler.go demo_repo/README.md)"
assert_output "$contracts_plan" "gatemolebench=true"
assert_output "$contracts_plan" "kernelbench=false"
assert_output "$contracts_plan" "production=false"

kernel_plan="$(run_plan kernel internal/kernel/identity/oidc.go)"
assert_output "$kernel_plan" "runtimebench=true"
assert_output "$kernel_plan" "kernelbench=false"
assert_output "$kernel_plan" "transactionbench=false"
assert_output "$kernel_plan" "production=true"

broker_plan="$(run_plan broker internal/kernel/broker/broker.go)"
assert_output "$broker_plan" "kernelbench=true"
assert_output "$broker_plan" "transactionbench=false"
assert_output "$broker_plan" "runtimebench=false"

transaction_plan="$(run_plan transaction internal/kernel/transaction/prepare.go)"
assert_output "$transaction_plan" "kernelbench=false"
assert_output "$transaction_plan" "transactionbench=true"
assert_output "$transaction_plan" "runtimebench=false"

schema_plan="$(run_plan schema schemas/gatemole.agent_task.v0.schema.json)"
assert_output "$schema_plan" "docs=true"
assert_output "$schema_plan" "gatemolebench=true"
assert_output "$schema_plan" "production=true"

critical_paths=(
  internal/kernel/sandbox/oci.go
  internal/kernel/identity/oidc.go
  internal/gatemole/approval_cli.go
  internal/gatemole/transaction_cli.go
  build/production-fixture.Dockerfile
  connectors/github/driver.go
  .gatemole/agent-profiles.json
  .gitattributes
  scripts/gatemoleci-plan.sh
  .github/workflows/pr.yml
  go.mod
)
critical_index=0
for critical_path in "${critical_paths[@]}"; do
  critical_plan="$(run_plan "critical-$critical_index" "$critical_path")"
  assert_output "$critical_plan" "production=true"
  critical_index=$((critical_index + 1))
done

unknown_plan="$(run_plan unknown connectors/github/driver.go)"
assert_output "$unknown_plan" "gatemolebench=true"
assert_output "$unknown_plan" "kernelbench=true"

echo "gatemoleci-plan-test: all routing cases passed"
