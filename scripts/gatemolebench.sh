#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/gatemolebench.sh [--out DIR] [--keep]

Runs the local Gatemole benchmark harness against reproducible fixtures.

Options:
  --out DIR   Write result JSON and Markdown to DIR.
              Default: benchmarks/results
  --keep      Keep the temporary working directory for inspection.
EOF
}

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="$ROOT/benchmarks/results"
KEEP=0
SCENARIOS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)
      if [[ $# -lt 2 ]]; then
        echo "gatemolebench: --out requires a directory" >&2
        exit 2
      fi
      OUT_DIR="$2"
      shift 2
      ;;
    --keep)
      KEEP=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "gatemolebench: unknown argument $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

mkdir -p "$OUT_DIR"
RUN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/gatemolebench.XXXXXX")"
GATEMOLE="$RUN_DIR/gatemole"

cleanup() {
  if [[ "$KEEP" == "1" ]]; then
    echo "kept benchmark workdir: $RUN_DIR" >&2
  else
    rm -rf "$RUN_DIR"
  fi
}
trap cleanup EXIT

now_ns() {
  python3 -c 'import time; print(time.perf_counter_ns())'
}

write_text_file() {
  local path="$1"
  local content="$2"
  mkdir -p "$(dirname "$path")"
  printf '%s\n' "$content" > "$path"
}

write_obligation_json() {
  local path="$1"
  local obligations_json="$2"
  python3 - "$path" "$obligations_json" <<'PY'
import json
import sys

path, raw = sys.argv[1:]
with open(path, "w", encoding="utf-8") as f:
    json.dump({"status": "pass", "obligations": json.loads(raw)}, f, indent=2, sort_keys=True)
    f.write("\n")
PY
}

write_junit_obligations() {
  local path="$1"
  local obligations_json="$2"
  python3 - "$path" "$obligations_json" <<'PY'
import json
import sys
from xml.sax.saxutils import escape

path, raw = sys.argv[1:]
obligations = json.loads(raw)
with open(path, "w", encoding="utf-8") as f:
    f.write(f'<testsuite name="gatemolebench" tests="{len(obligations)}" failures="0" errors="0" skipped="0">')
    for obligation in obligations:
        value = escape(obligation, {'"': '&quot;'})
        f.write(f'<testcase classname="{value}" name="{value}"></testcase>')
    f.write("</testsuite>\n")
PY
}

copy_demo_repo() {
  local scenario="$1"
  local repo="$RUN_DIR/$scenario/repo"
  mkdir -p "$(dirname "$repo")"
  cp -R "$ROOT/demo_repo" "$repo"
  printf '%s\n' "$repo"
}

write_meta_json() {
  local dir="$1"
  local data="$2"
  mkdir -p "$dir"
  python3 - "$dir/meta.json" "$data" <<'PY'
import json
import sys

path, raw = sys.argv[1:]
data = json.loads(raw)
for key in ("id", "title", "baseline", "expected", "claim"):
    if key not in data:
        raise SystemExit(f"metadata missing required key: {key}")
with open(path, "w", encoding="utf-8") as f:
    json.dump(data, f, indent=2, sort_keys=True)
    f.write("\n")
PY
}

record_timing() {
  local dir="$1"
  local start_ns="$2"
  local end_ns="$3"
  local exit_code="$4"
  python3 - "$dir/timing.json" "$start_ns" "$end_ns" "$exit_code" <<'PY'
import json
import sys

path, start_ns, end_ns, exit_code = sys.argv[1:]
elapsed_ms = (int(end_ns) - int(start_ns)) / 1_000_000
data = {
    "gate_ms": round(elapsed_ms, 3),
    "gate_exit_code": int(exit_code),
}
with open(path, "w", encoding="utf-8") as f:
    json.dump(data, f, indent=2, sort_keys=True)
    f.write("\n")
PY
}

run_gate() {
  local dir="$1"
  local repo="$2"
  local manifest="${3:-}"
  local start_ns
  local end_ns
  local code

  start_ns="$(now_ns)"
  set +e
  if [[ -n "$manifest" ]]; then
    "$GATEMOLE" --repo "$repo" --manifest "$manifest" gate --json > "$dir/gate.json" 2> "$dir/gate.stderr"
  else
    "$GATEMOLE" --repo "$repo" gate --json > "$dir/gate.json" 2> "$dir/gate.stderr"
  fi
  code=$?
  set -e
  end_ns="$(now_ns)"
  record_timing "$dir" "$start_ns" "$end_ns" "$code"
}

attach_artifact() {
  local repo="$1"
  local manifest="$2"
  local id="$3"
  local kind="$4"
  local path="$5"
  local stdout_path="$6"

  "$GATEMOLE" --repo "$repo" manifest attach-artifact \
    --manifest "$manifest" \
    --id "$id" \
    --kind "$kind" \
    --path "$path" \
    --exit-code 0 \
    --out "$manifest" > "$stdout_path"
}

assert_scenario() {
  local dir="$1"
  python3 - "$dir" <<'PY'
from __future__ import annotations

import json
import sys
from pathlib import Path

scenario_dir = Path(sys.argv[1])


def load(path: Path) -> dict:
    with path.open(encoding="utf-8") as f:
        return json.load(f)


def flatten_obligations(value: object) -> list[str]:
    if not isinstance(value, dict):
        return []
    out: list[str] = []
    for spec_id in sorted(value):
        items = value.get(spec_id)
        if isinstance(items, list):
            out.extend(item for item in items if isinstance(item, str))
    return sorted(out)


def invalid_evidence_keys(gate: dict) -> list[str]:
    keys: list[str] = []
    for item in gate.get("invalid_evidence", []):
        if isinstance(item, dict):
            keys.append(f"{item.get('artifact', '')}:{item.get('code', '')}")
    return sorted(keys)


def artifact_statuses(gate: dict) -> dict[str, str]:
    statuses: dict[str, str] = {}
    for item in gate.get("artifact_results", []):
        if isinstance(item, dict) and item.get("id"):
            statuses[str(item["id"])] = str(item.get("status", ""))
    return statuses


def sorted_strings(value: object) -> list[str]:
    if not isinstance(value, list):
        return []
    return sorted(item for item in value if isinstance(item, str))


def same_unordered_strings(left: object, right: object) -> bool:
    return sorted_strings(left) == sorted_strings(right)


def add_assertion(assertions: list[dict], assertion_id: str, passed: bool, expected: object, actual: object, message: str) -> None:
    assertions.append({
        "id": assertion_id,
        "passed": bool(passed),
        "expected": expected,
        "actual": actual,
        "message": message,
    })


meta = load(scenario_dir / "meta.json")
timing = load(scenario_dir / "timing.json")
gate = load(scenario_dir / "gate.json")
expected = meta["expected"]

covered = flatten_obligations(gate.get("covered_obligations"))
missing = flatten_obligations(gate.get("missing_obligations"))
compiled_total = int(gate.get("compilation", {}).get("obligations_built", 0))
total = max(len(covered) + len(missing), compiled_total)
actual = {
    "decision": gate.get("decision", "error"),
    "gate_exit_code": timing["gate_exit_code"],
    "covered_obligations": len(covered),
    "total_obligations": total,
    "missing_obligations": missing,
    "missing_obligations_count": len(missing),
    "invalid_evidence": invalid_evidence_keys(gate),
    "policy_rules_fired": gate.get("policy_rules_fired", []),
    "fired_policy_rule": gate.get("fired_policy_rule", ""),
    "manifest_error_count": len(gate.get("manifest_errors", [])),
    "spec_error_count": len(gate.get("spec_errors", [])),
    "manifest_errors": gate.get("manifest_errors", []),
    "spec_errors": gate.get("spec_errors", []),
    "artifact_statuses": artifact_statuses(gate),
    "reasons": gate.get("reasons", []),
    "gate_ms": timing["gate_ms"],
}

assertions: list[dict] = []
for key in (
    "decision",
    "gate_exit_code",
    "covered_obligations",
    "total_obligations",
    "manifest_error_count",
    "spec_error_count",
):
    if key in expected:
        add_assertion(
            assertions,
            key,
            actual[key] == expected[key],
            expected[key],
            actual[key],
            f"{key} must match expected value",
        )

for key in ("missing_obligations", "invalid_evidence"):
    if key in expected:
        add_assertion(
            assertions,
            key,
            same_unordered_strings(actual[key], expected[key]),
            sorted_strings(expected[key]),
            actual[key],
            f"{key} must match expected set",
        )

if "policy_rules_fired" in expected:
    add_assertion(
        assertions,
        "policy_rules_fired",
        actual["policy_rules_fired"] == expected["policy_rules_fired"],
        expected["policy_rules_fired"],
        actual["policy_rules_fired"],
        "policy rules fired must match expected order",
    )

if "artifact_statuses" in expected:
    expected_statuses = expected["artifact_statuses"]
    actual_subset = {
        artifact_id: actual["artifact_statuses"].get(artifact_id)
        for artifact_id in sorted(expected_statuses)
    }
    add_assertion(
        assertions,
        "artifact_statuses",
        actual_subset == expected_statuses,
        expected_statuses,
        actual_subset,
        "selected artifact statuses must match expected values",
    )

if "reason_substrings" in expected:
    reasons_text = "\n".join(actual["reasons"])
    for item in expected["reason_substrings"]:
        add_assertion(
            assertions,
            f"reason_contains:{item}",
            item in reasons_text,
            item,
            actual["reasons"],
            "gate reasons must include expected text",
        )

if "manifest_error_substrings" in expected:
    manifest_errors_text = "\n".join(actual["manifest_errors"])
    for item in expected["manifest_error_substrings"]:
        add_assertion(
            assertions,
            f"manifest_error_contains:{item}",
            item in manifest_errors_text,
            item,
            actual["manifest_errors"],
            "manifest errors must include expected text",
        )

passed = all(item["passed"] for item in assertions)
row = {
    "id": meta["id"],
    "title": meta["title"],
    "claim": meta["claim"],
    "baseline": meta["baseline"],
    "expected": expected,
    "actual": actual,
    "assertions": assertions,
    "passed": passed,
}
with (scenario_dir / "row.json").open("w", encoding="utf-8") as f:
    json.dump(row, f, indent=2, sort_keys=True)
    f.write("\n")

if not passed:
    print(f"gatemolebench: scenario {meta['id']} failed assertions:", file=sys.stderr)
    for item in assertions:
        if item["passed"]:
            continue
        print(
            f"- {item['id']}: expected {item['expected']!r}, got {item['actual']!r}",
            file=sys.stderr,
        )
    sys.exit(1)
PY
}

set_test_artifact_exit_code() {
  local repo="$1"
  local out="$2"
  python3 - "$repo/.gatemole/manifests/pass.json" "$repo/$out" <<'PY'
import json
import sys

src, dst = sys.argv[1:]
with open(src, encoding="utf-8") as f:
    data = json.load(f)
data["task"]["id"] = "issue-184-nonzero-test-artifact"
data["task"]["summary"] = "password reset release with a failed test artifact command"
data["agent"]["run_id"] = "demo-nonzero-test-artifact-run"
data["verification"]["test_results"] = {"passed": 25, "failed": 1}
for artifact in data["verification"]["artifacts"]:
    if artifact["id"] == "test-results":
        artifact["exit_code"] = 1
        artifact["command"] = "npm test && npm run test:e2e"
with open(dst, "w", encoding="utf-8") as f:
    json.dump(data, f, indent=2, sort_keys=True)
    f.write("\n")
PY
}

disable_canary_manifest() {
  local repo="$1"
  local out="$2"
  python3 - "$repo/.gatemole/manifests/pass.json" "$repo/$out" <<'PY'
import json
import sys

src, dst = sys.argv[1:]
with open(src, encoding="utf-8") as f:
    data = json.load(f)
data["task"]["id"] = "issue-184-no-canary"
data["task"]["summary"] = "password reset release with complete evidence but no canary"
data["agent"]["run_id"] = "demo-no-canary-run"
data["runtime"]["canary"] = {"enabled": False, "initial_percent": 0}
with open(dst, "w", encoding="utf-8") as f:
    json.dump(data, f, indent=2, sort_keys=True)
    f.write("\n")
PY
}

platform_behavior_all_ids='[
  "api.users.behavior.user_profile_response_preserves_public_schema",
  "api.users.behavior.user_search_paginates_deterministically",
  "auth.session.behavior.logout_invalidates_refresh_token",
  "auth.session.behavior.refresh_session_preserves_user_identity",
  "payments.checkout.behavior.checkout_creates_idempotent_payment_intent",
  "payments.checkout.behavior.invoice_total_uses_server_side_price"
]'

platform_security_partial_ids='[
  "api.users.security.email_address_is_hidden_from_public_search",
  "auth.session.security.refresh_token_is_rotated_and_hashed",
  "auth.session.security.session_cookie_uses_secure_attributes",
  "payments.checkout.security.card_data_is_never_logged"
]'

platform_security_full_ids='[
  "api.users.security.email_address_is_hidden_from_public_search",
  "auth.session.security.refresh_token_is_rotated_and_hashed",
  "auth.session.security.session_cookie_uses_secure_attributes",
  "payments.checkout.security.card_data_is_never_logged",
  "payments.checkout.security.webhook_signature_is_verified"
]'

platform_tests_all_ids='[
  "api.users.required_test.user_profile_schema",
  "api.users.required_test.user_search_pagination",
  "auth.session.required_test.logout_rejects_old_token",
  "auth.session.required_test.refresh_rotates_token",
  "payments.checkout.required_test.checkout_idempotency",
  "payments.checkout.required_test.invoice_total_cannot_be_client_overridden"
]'

platform_runtime_partial_ids='[
  "api.users.runtime_signal.api_users_requests",
  "auth.session.runtime_signal.auth_session_logout",
  "auth.session.runtime_signal.auth_session_refresh",
  "payments.checkout.runtime_signal.payments_checkout_created"
]'

platform_runtime_full_ids='[
  "api.users.runtime_signal.api_users_requests",
  "auth.session.runtime_signal.auth_session_logout",
  "auth.session.runtime_signal.auth_session_refresh",
  "payments.checkout.runtime_signal.payments_checkout_created",
  "payments.checkout.runtime_signal.payments_checkout_failed"
]'

platform_rollback_partial_ids='[
  "api.users.rollback.revert_change",
  "auth.session.rollback.feature_flag_session_refresh_v2"
]'

platform_rollback_full_ids='[
  "api.users.rollback.revert_change",
  "auth.session.rollback.feature_flag_session_refresh_v2",
  "payments.checkout.rollback.disable_feature_flag_checkout_v3"
]'

platform_partial_missing_ids='[
  "payments.checkout.security.webhook_signature_is_verified",
  "payments.checkout.runtime_signal.payments_checkout_failed",
  "payments.checkout.rollback.disable_feature_flag_checkout_v3"
]'

platform_api_behavior_ids='[
  "api.users.behavior.user_profile_response_preserves_public_schema",
  "api.users.behavior.user_search_paginates_deterministically"
]'

platform_api_security_ids='[
  "api.users.security.email_address_is_hidden_from_public_search"
]'

platform_api_tests_ids='[
  "api.users.required_test.user_profile_schema",
  "api.users.required_test.user_search_pagination"
]'

platform_api_runtime_ids='[
  "api.users.runtime_signal.api_users_requests"
]'

platform_api_rollback_ids='[
  "api.users.rollback.revert_change"
]'

auth_test_missing_ids='[
  "auth.password_reset.required_test.rate_limit_triggers",
  "auth.password_reset.required_test.token_cannot_be_reused",
  "auth.password_reset.required_test.token_expires",
  "auth.password_reset.required_test.unknown_email_receives_same_response_shape"
]'

auth_tests_only_missing_ids='[
  "auth.password_reset.behavior.reset_token_expires_after_30_minutes",
  "auth.password_reset.behavior.reset_token_is_single_use",
  "auth.password_reset.behavior.response_does_not_reveal_whether_account_exists",
  "auth.password_reset.behavior.user_can_request_password_reset_by_email",
  "auth.password_reset.security.no_account_enumeration",
  "auth.password_reset.security.reset_endpoint_is_rate_limited_by_ip_and_account",
  "auth.password_reset.security.reset_token_is_never_logged",
  "auth.password_reset.security.reset_token_is_stored_hashed",
  "auth.password_reset.runtime_signal.password_reset_completed",
  "auth.password_reset.runtime_signal.password_reset_failed",
  "auth.password_reset.runtime_signal.password_reset_requested",
  "auth.password_reset.rollback.feature_flag_password_reset_v2"
]'

auth_partial_missing_ids='[
  "auth.password_reset.required_test.rate_limit_triggers",
  "auth.password_reset.required_test.token_cannot_be_reused",
  "auth.password_reset.security.reset_endpoint_is_rate_limited_by_ip_and_account",
  "auth.password_reset.security.reset_token_is_never_logged",
  "auth.password_reset.runtime_signal.password_reset_completed",
  "auth.password_reset.runtime_signal.password_reset_requested"
]'

create_platform_repo() {
  local scenario="$1"
  local dir="$RUN_DIR/$scenario"
  local repo="$dir/repo"
  mkdir -p "$repo/internal/auth" "$repo/internal/payments" "$repo/internal/api" "$repo/tests/auth" "$repo/tests/payments" "$repo/tests/api"
  write_text_file "$repo/internal/auth/session.py" 'def refresh_session(user_id): return user_id'
  write_text_file "$repo/internal/payments/checkout.py" 'def create_checkout(cart): return cart'
  write_text_file "$repo/internal/api/users.py" 'def public_user(user): return user'
  write_text_file "$repo/tests/auth/test_session.py" 'def test_refresh_rotates_token(): pass'
  write_text_file "$repo/tests/payments/test_checkout.py" 'def test_checkout_idempotency(): pass'
  write_text_file "$repo/tests/api/test_users.py" 'def test_user_profile_schema(): pass'
  write_text_file "$repo/CODEOWNERS" '/internal/auth/ @security
/internal/payments/ @payments
/internal/api/ @platform'

  "$GATEMOLE" --repo "$repo" init > "$dir/init.stdout"
  "$GATEMOLE" --repo "$repo" contract create \
    --name auth.session \
    --owner security \
    --risk high \
    --paths "internal/auth/**,tests/auth/**" \
    --behavior "refresh session preserves user identity" \
    --behavior "logout invalidates refresh token" \
    --security "refresh token is rotated and hashed" \
    --security "session cookie uses secure attributes" \
    --required-test "refresh rotates token" \
    --required-test "logout rejects old token" \
    --metric "auth.session.refresh" \
    --metric "auth.session.logout" \
    --rollback-strategy "feature_flag" \
    --rollback-flag "session_refresh_v2" > "$dir/contract-auth.stdout"
  "$GATEMOLE" --repo "$repo" contract create \
    --name payments.checkout \
    --owner payments \
    --risk high \
    --paths "internal/payments/**,tests/payments/**" \
    --behavior "checkout creates idempotent payment intent" \
    --behavior "invoice total uses server side price" \
    --security "webhook signature is verified" \
    --security "card data is never logged" \
    --required-test "checkout idempotency" \
    --required-test "invoice total cannot be client overridden" \
    --metric "payments.checkout.created" \
    --metric "payments.checkout.failed" \
    --rollback-strategy "disable_feature_flag" \
    --rollback-flag "checkout_v3" > "$dir/contract-payments.stdout"
  "$GATEMOLE" --repo "$repo" contract create \
    --name api.users \
    --owner platform \
    --risk medium \
    --paths "internal/api/**,tests/api/**" \
    --behavior "user profile response preserves public schema" \
    --behavior "user search paginates deterministically" \
    --security "email address is hidden from public search" \
    --required-test "user profile schema" \
    --required-test "user search pagination" \
    --metric "api.users.requests" \
    --rollback-strategy "revert_change" > "$dir/contract-api.stdout"
  "$GATEMOLE" --repo "$repo" compile > "$dir/compile.stdout"
  printf '%s\n' "$repo"
}

write_platform_artifacts() {
  local repo="$1"
  local behavior_ids="$2"
  local security_ids="$3"
  local test_ids="$4"
  local runtime_ids="$5"
  local rollback_ids="$6"

  mkdir -p "$repo/.gatemole/artifacts"
  write_obligation_json "$repo/.gatemole/artifacts/platform-behavior.json" "$behavior_ids"
  write_obligation_json "$repo/.gatemole/artifacts/platform-security.json" "$security_ids"
  write_junit_obligations "$repo/.gatemole/artifacts/platform-tests.xml" "$test_ids"
  write_obligation_json "$repo/.gatemole/artifacts/platform-runtime.json" "$runtime_ids"
  write_obligation_json "$repo/.gatemole/artifacts/platform-rollback.json" "$rollback_ids"
}

attach_platform_artifacts() {
  local repo="$1"
  local manifest="$2"
  local dir="$3"

  attach_artifact "$repo" "$manifest" platform-behavior behavior_trace .gatemole/artifacts/platform-behavior.json "$dir/attach-platform-behavior.stdout"
  attach_artifact "$repo" "$manifest" platform-security security_check .gatemole/artifacts/platform-security.json "$dir/attach-platform-security.stdout"
  attach_artifact "$repo" "$manifest" platform-tests test_coverage .gatemole/artifacts/platform-tests.xml "$dir/attach-platform-tests.stdout"
  attach_artifact "$repo" "$manifest" platform-runtime runtime_metric .gatemole/artifacts/platform-runtime.json "$dir/attach-platform-runtime.stdout"
  attach_artifact "$repo" "$manifest" platform-rollback rollback_plan .gatemole/artifacts/platform-rollback.json "$dir/attach-platform-rollback.stdout"
}

add_auth_tests_only() {
  local scenario="auth_tests_only"
  local dir="$RUN_DIR/$scenario"
  local repo
  repo="$(copy_demo_repo "$scenario")"
  SCENARIOS+=("$dir")

  write_meta_json "$dir" "{
    \"id\": \"$scenario\",
    \"title\": \"High-risk auth change with passing JUnit only\",
    \"claim\": \"JUnit tests pass, but behavior, security, runtime, and rollback evidence is missing.\",
    \"baseline\": {
      \"id\": \"tests_only\",
      \"tests_passed\": true,
      \"caught_by_tests\": false,
      \"would_continue_without_gatemole\": true,
      \"description\": \"The baseline sees a passing JUnit artifact and no release-obligation coverage model.\"
    },
    \"expected\": {
      \"decision\": \"block\",
      \"gate_exit_code\": 1,
      \"covered_obligations\": 4,
      \"total_obligations\": 16,
      \"missing_obligations\": $auth_tests_only_missing_ids,
      \"invalid_evidence\": [],
      \"policy_rules_fired\": [\"block_verifier_findings\"],
      \"manifest_error_count\": 0,
      \"spec_error_count\": 0,
      \"artifact_statuses\": {\"junit\": \"valid\"},
      \"reason_substrings\": [
        \"uncovered behavior obligations\",
        \"uncovered security invariants\",
        \"uncovered rollback obligations\",
        \"uncovered runtime signal obligations\"
      ]
    }
  }"

  "$GATEMOLE" --repo "$repo" compile > "$dir/compile.stdout"
  "$GATEMOLE" --repo "$repo" evidence import junit artifacts/junit-pass.xml > "$dir/import.stdout"
  run_gate "$dir" "$repo" ""
  assert_scenario "$dir"
}

add_auth_partial_release_evidence() {
  local scenario="auth_partial_release_evidence"
  local dir="$RUN_DIR/$scenario"
  local repo
  repo="$(copy_demo_repo "$scenario")"
  SCENARIOS+=("$dir")

  write_meta_json "$dir" "{
    \"id\": \"$scenario\",
    \"title\": \"High-risk auth change with partial release evidence\",
    \"claim\": \"Tests pass and some artifacts exist, but required test, security, and runtime obligations remain uncovered.\",
    \"baseline\": {
      \"id\": \"tests_plus_partial_artifacts\",
      \"tests_passed\": true,
      \"caught_by_tests\": false,
      \"would_continue_without_gatemole\": true,
      \"description\": \"The baseline sees passing tests and partial artifacts, but does not require every compiled release obligation to be covered.\"
    },
    \"expected\": {
      \"decision\": \"block\",
      \"gate_exit_code\": 1,
      \"covered_obligations\": 10,
      \"total_obligations\": 16,
      \"missing_obligations\": $auth_partial_missing_ids,
      \"invalid_evidence\": [],
      \"policy_rules_fired\": [\"block_verifier_findings\"],
      \"manifest_error_count\": 0,
      \"spec_error_count\": 0,
      \"artifact_statuses\": {\"test-results\": \"valid\", \"security-results\": \"valid\", \"runtime-signals\": \"valid\"},
      \"reason_substrings\": [
        \"uncovered required tests\",
        \"uncovered security invariants\",
        \"uncovered runtime signal obligations\"
      ]
    }
  }"

  "$GATEMOLE" --repo "$repo" compile > "$dir/compile.stdout"
  run_gate "$dir" "$repo" ".gatemole/manifests/blocked.json"
  assert_scenario "$dir"
}

add_auth_manifest_traceability_block() {
  local scenario="auth_manifest_traceability_block"
  local dir="$RUN_DIR/$scenario"
  local repo
  repo="$(copy_demo_repo "$scenario")"
  SCENARIOS+=("$dir")

  write_meta_json "$dir" "{
    \"id\": \"$scenario\",
    \"title\": \"High-risk auth change with full evidence but an unrouted changed file\",
    \"claim\": \"Complete auth evidence is not enough when the manifest omits ownership for a changed billing file.\",
    \"baseline\": {
      \"id\": \"tests_plus_complete_artifacts\",
      \"tests_passed\": true,
      \"caught_by_tests\": false,
      \"would_continue_without_gatemole\": true,
      \"description\": \"The baseline sees the full passing artifact set but does not check changed-file ownership against contract scope.\"
    },
    \"expected\": {
      \"decision\": \"block\",
      \"gate_exit_code\": 1,
      \"covered_obligations\": 16,
      \"total_obligations\": 16,
      \"missing_obligations\": [],
      \"invalid_evidence\": [],
      \"policy_rules_fired\": [\"block_invalid_spec_or_manifest\"],
      \"manifest_error_count\": 1,
      \"spec_error_count\": 0,
      \"reason_substrings\": [\"invalid specs or manifest\"],
      \"manifest_error_substrings\": [\"src/billing/invoice.ts\"]
    }
  }"

  "$GATEMOLE" --repo "$repo" compile > "$dir/compile.stdout"
  run_gate "$dir" "$repo" ".gatemole/manifests/traceability-blocked.json"
  assert_scenario "$dir"
}

add_auth_nonzero_test_artifact() {
  local scenario="auth_nonzero_test_artifact"
  local dir="$RUN_DIR/$scenario"
  local repo
  repo="$(copy_demo_repo "$scenario")"
  SCENARIOS+=("$dir")

  write_meta_json "$dir" "{
    \"id\": \"$scenario\",
    \"title\": \"High-risk auth change with complete evidence but a non-zero test artifact exit\",
    \"claim\": \"The harness must treat artifact command exit codes as release evidence, not just artifact file presence.\",
    \"baseline\": {
      \"id\": \"tests_failed\",
      \"tests_passed\": false,
      \"caught_by_tests\": true,
      \"would_continue_without_gatemole\": false,
      \"description\": \"The test runner already failed, so this is a negative-control scenario rather than a Gatemole-only catch.\"
    },
    \"expected\": {
      \"decision\": \"block\",
      \"gate_exit_code\": 1,
      \"covered_obligations\": 12,
      \"total_obligations\": 16,
      \"missing_obligations\": $auth_test_missing_ids,
      \"invalid_evidence\": [\"test-results:non_zero_exit\"],
      \"policy_rules_fired\": [\"block_verifier_findings\"],
      \"manifest_error_count\": 0,
      \"spec_error_count\": 0,
      \"artifact_statuses\": {\"test-results\": \"invalid\"},
      \"reason_substrings\": [
        \"evidence artifact test-results is invalid\",
        \"test suite failed\",
        \"uncovered required tests\"
      ]
    }
  }"

  "$GATEMOLE" --repo "$repo" compile > "$dir/compile.stdout"
  set_test_artifact_exit_code "$repo" ".gatemole/manifests/nonzero-test-artifact.json"
  run_gate "$dir" "$repo" ".gatemole/manifests/nonzero-test-artifact.json"
  assert_scenario "$dir"
}

add_auth_full_release_evidence() {
  local scenario="auth_full_release_evidence"
  local dir="$RUN_DIR/$scenario"
  local repo
  repo="$(copy_demo_repo "$scenario")"
  SCENARIOS+=("$dir")

  write_meta_json "$dir" "{
    \"id\": \"$scenario\",
    \"title\": \"High-risk auth change with complete release evidence\",
    \"claim\": \"All obligations are covered; policy still routes high-risk auth through canary instead of auto-merge.\",
    \"baseline\": {
      \"id\": \"tests_plus_complete_artifacts\",
      \"tests_passed\": true,
      \"caught_by_tests\": false,
      \"would_continue_without_gatemole\": true,
      \"description\": \"The baseline sees complete passing artifacts but does not express the canary release route.\"
    },
    \"expected\": {
      \"decision\": \"canary\",
      \"gate_exit_code\": 0,
      \"covered_obligations\": 16,
      \"total_obligations\": 16,
      \"missing_obligations\": [],
      \"invalid_evidence\": [],
      \"policy_rules_fired\": [\"high_risk_canary\"],
      \"manifest_error_count\": 0,
      \"spec_error_count\": 0,
      \"reason_substrings\": [\"high-risk change has passing evidence and canary enabled\"]
    }
  }"

  "$GATEMOLE" --repo "$repo" compile > "$dir/compile.stdout"
  run_gate "$dir" "$repo" ".gatemole/manifests/pass.json"
  assert_scenario "$dir"
}

add_auth_full_release_without_canary() {
  local scenario="auth_full_release_without_canary"
  local dir="$RUN_DIR/$scenario"
  local repo
  repo="$(copy_demo_repo "$scenario")"
  SCENARIOS+=("$dir")

  write_meta_json "$dir" "{
    \"id\": \"$scenario\",
    \"title\": \"High-risk auth change with complete evidence but no canary\",
    \"claim\": \"Complete evidence without a canary should escalate high-risk auth instead of auto-merging.\",
    \"baseline\": {
      \"id\": \"tests_plus_complete_artifacts\",
      \"tests_passed\": true,
      \"caught_by_tests\": false,
      \"would_continue_without_gatemole\": true,
      \"description\": \"The baseline sees passing tests and complete artifacts, but does not encode high-risk rollout policy.\"
    },
    \"expected\": {
      \"decision\": \"human_escalation\",
      \"gate_exit_code\": 0,
      \"covered_obligations\": 16,
      \"total_obligations\": 16,
      \"missing_obligations\": [],
      \"invalid_evidence\": [],
      \"policy_rules_fired\": [\"high_risk_without_canary\"],
      \"manifest_error_count\": 0,
      \"spec_error_count\": 0,
      \"reason_substrings\": [\"high-risk change passed checks but has no canary\"]
    }
  }"

  "$GATEMOLE" --repo "$repo" compile > "$dir/compile.stdout"
  disable_canary_manifest "$repo" ".gatemole/manifests/no-canary.json"
  run_gate "$dir" "$repo" ".gatemole/manifests/no-canary.json"
  assert_scenario "$dir"
}

add_platform_multi_component_partial_evidence() {
  local scenario="platform_multi_component_partial_evidence"
  local dir="$RUN_DIR/$scenario"
  local repo
  repo="$(create_platform_repo "$scenario")"
  SCENARIOS+=("$dir")

  write_meta_json "$dir" "{
    \"id\": \"$scenario\",
    \"title\": \"Medium/high platform change with partial multi-component evidence\",
    \"claim\": \"A realistic multi-component release can pass tests and still block because one high-risk component lacks security, runtime, and rollback evidence.\",
    \"baseline\": {
      \"id\": \"multi_component_tests_plus_partial_artifacts\",
      \"tests_passed\": true,
      \"caught_by_tests\": false,
      \"would_continue_without_gatemole\": true,
      \"description\": \"The baseline sees passing tests across auth, payments, and API, but does not require complete evidence per compiled obligation and component.\"
    },
    \"expected\": {
      \"decision\": \"block\",
      \"gate_exit_code\": 1,
      \"covered_obligations\": 22,
      \"total_obligations\": 25,
      \"missing_obligations\": $platform_partial_missing_ids,
      \"invalid_evidence\": [],
      \"policy_rules_fired\": [\"block_verifier_findings\"],
      \"manifest_error_count\": 0,
      \"spec_error_count\": 0,
      \"artifact_statuses\": {
        \"platform-behavior\": \"valid\",
        \"platform-security\": \"valid\",
        \"platform-tests\": \"valid\",
        \"platform-runtime\": \"valid\",
        \"platform-rollback\": \"valid\"
      },
      \"reason_substrings\": [
        \"payments.checkout has uncovered security invariants\",
        \"payments.checkout has uncovered runtime signal obligations\",
        \"payments.checkout has uncovered rollback obligations\"
      ]
    }
  }"

  "$GATEMOLE" --repo "$repo" manifest create \
    --task-id platform-204 \
    --summary "agent updates session refresh, checkout, and users API" \
    --agent codex \
    --run-id platform-partial \
    --changed-file internal/auth/session.py \
    --changed-file internal/payments/checkout.py \
    --changed-file internal/api/users.py \
    --external-effect charges_card \
    --external-effect sends_email \
    --out .gatemole/manifests/platform-partial.json > "$dir/manifest.stdout"
  write_platform_artifacts "$repo" \
    "$platform_behavior_all_ids" \
    "$platform_security_partial_ids" \
    "$platform_tests_all_ids" \
    "$platform_runtime_partial_ids" \
    "$platform_rollback_partial_ids"
  attach_platform_artifacts "$repo" ".gatemole/manifests/platform-partial.json" "$dir"
  run_gate "$dir" "$repo" ".gatemole/manifests/platform-partial.json"
  assert_scenario "$dir"
}

add_platform_multi_component_full_canary() {
  local scenario="platform_multi_component_full_canary"
  local dir="$RUN_DIR/$scenario"
  local repo
  repo="$(create_platform_repo "$scenario")"
  SCENARIOS+=("$dir")

  write_meta_json "$dir" "{
    \"id\": \"$scenario\",
    \"title\": \"Medium/high platform change with complete multi-component evidence\",
    \"claim\": \"A multi-component high-risk release with complete evidence should pass to canary instead of being treated like a tiny toy repo.\",
    \"baseline\": {
      \"id\": \"multi_component_tests_plus_complete_artifacts\",
      \"tests_passed\": true,
      \"caught_by_tests\": false,
      \"would_continue_without_gatemole\": true,
      \"description\": \"The baseline sees all component tests and artifacts passing but does not encode the high-risk canary release posture.\"
    },
    \"expected\": {
      \"decision\": \"canary\",
      \"gate_exit_code\": 0,
      \"covered_obligations\": 25,
      \"total_obligations\": 25,
      \"missing_obligations\": [],
      \"invalid_evidence\": [],
      \"policy_rules_fired\": [\"high_risk_canary\"],
      \"manifest_error_count\": 0,
      \"spec_error_count\": 0,
      \"reason_substrings\": [\"high-risk change has passing evidence and canary enabled\"]
    }
  }"

  "$GATEMOLE" --repo "$repo" manifest create \
    --task-id platform-205 \
    --summary "agent updates session refresh, checkout, and users API with complete release evidence" \
    --agent codex \
    --run-id platform-full \
    --changed-file internal/auth/session.py \
    --changed-file internal/payments/checkout.py \
    --changed-file internal/api/users.py \
    --external-effect charges_card \
    --external-effect sends_email \
    --out .gatemole/manifests/platform-full.json > "$dir/manifest.stdout"
  write_platform_artifacts "$repo" \
    "$platform_behavior_all_ids" \
    "$platform_security_full_ids" \
    "$platform_tests_all_ids" \
    "$platform_runtime_full_ids" \
    "$platform_rollback_full_ids"
  attach_platform_artifacts "$repo" ".gatemole/manifests/platform-full.json" "$dir"
  run_gate "$dir" "$repo" ".gatemole/manifests/platform-full.json"
  assert_scenario "$dir"
}

add_platform_medium_api_auto_merge() {
  local scenario="platform_medium_api_auto_merge"
  local dir="$RUN_DIR/$scenario"
  local repo
  repo="$(create_platform_repo "$scenario")"
  SCENARIOS+=("$dir")

  write_meta_json "$dir" "{
    \"id\": \"$scenario\",
    \"title\": \"Medium-risk API-only change in a multi-component repo\",
    \"claim\": \"The benchmark should prove Gatemole can isolate a medium-risk component inside a larger repo and auto-merge it when evidence is complete.\",
    \"baseline\": {
      \"id\": \"api_tests_plus_complete_artifacts\",
      \"tests_passed\": true,
      \"caught_by_tests\": false,
      \"would_continue_without_gatemole\": true,
      \"description\": \"The baseline sees passing API tests but does not prove the gate scoped obligations to only the touched medium-risk contract.\"
    },
    \"expected\": {
      \"decision\": \"auto_merge\",
      \"gate_exit_code\": 0,
      \"covered_obligations\": 7,
      \"total_obligations\": 7,
      \"missing_obligations\": [],
      \"invalid_evidence\": [],
      \"policy_rules_fired\": [\"low_medium_auto_merge\"],
      \"manifest_error_count\": 0,
      \"spec_error_count\": 0,
      \"reason_substrings\": [\"low/medium risk change passed required evidence\"]
    }
  }"

  "$GATEMOLE" --repo "$repo" manifest create \
    --task-id platform-206 \
    --summary "agent updates users API only" \
    --agent codex \
    --run-id platform-api \
    --changed-file internal/api/users.py \
    --out .gatemole/manifests/platform-api.json > "$dir/manifest.stdout"
  write_platform_artifacts "$repo" \
    "$platform_api_behavior_ids" \
    "$platform_api_security_ids" \
    "$platform_api_tests_ids" \
    "$platform_api_runtime_ids" \
    "$platform_api_rollback_ids"
  attach_platform_artifacts "$repo" ".gatemole/manifests/platform-api.json" "$dir"
  run_gate "$dir" "$repo" ".gatemole/manifests/platform-api.json"
  assert_scenario "$dir"
}

add_docs_low_risk_full_evidence() {
  local scenario="docs_low_risk_full_evidence"
  local dir="$RUN_DIR/$scenario"
  local repo="$dir/repo"
  SCENARIOS+=("$dir")

  mkdir -p "$repo"
  printf '# Docs\n' > "$repo/README.md"

  write_meta_json "$dir" "{
    \"id\": \"$scenario\",
    \"title\": \"Low-risk docs change with complete evidence\",
    \"claim\": \"A low-risk fully evidenced change should not be falsely blocked.\",
    \"baseline\": {
      \"id\": \"tests_plus_complete_artifacts\",
      \"tests_passed\": true,
      \"caught_by_tests\": false,
      \"would_continue_without_gatemole\": true,
      \"description\": \"The baseline sees passing documentation checks and all declared docs obligations are covered.\"
    },
    \"expected\": {
      \"decision\": \"auto_merge\",
      \"gate_exit_code\": 0,
      \"covered_obligations\": 5,
      \"total_obligations\": 5,
      \"missing_obligations\": [],
      \"invalid_evidence\": [],
      \"policy_rules_fired\": [\"low_medium_auto_merge\"],
      \"manifest_error_count\": 0,
      \"spec_error_count\": 0,
      \"reason_substrings\": [\"low/medium risk change passed required evidence\"]
    }
  }"

  "$GATEMOLE" --repo "$repo" init > "$dir/init.stdout"
  "$GATEMOLE" --repo "$repo" contract create \
    --name docs.readme \
    --owner docs \
    --risk low \
    --paths README.md \
    --behavior "readme documents usage" \
    --security "no secrets introduced" \
    --required-test "documentation smoke check" \
    --metric "gatemole.gate.decision" \
    --rollback-strategy "revert_change" > "$dir/contract.stdout"
  "$GATEMOLE" --repo "$repo" compile > "$dir/compile.stdout"
  "$GATEMOLE" --repo "$repo" manifest create \
    --task-id docs-safe \
    --summary "docs update" \
    --agent codex \
    --run-id bench-docs \
    --changed-file README.md \
    --out .gatemole/manifests/docs.json > "$dir/manifest.stdout"

  mkdir -p "$repo/.gatemole/artifacts"
  write_text_file "$repo/.gatemole/artifacts/behavior.json" '{"status":"pass","obligations":["docs.readme.behavior.readme_documents_usage"]}'
  write_text_file "$repo/.gatemole/artifacts/security.json" '{"status":"pass","obligations":["docs.readme.security.no_secrets_introduced"]}'
  write_text_file "$repo/.gatemole/artifacts/runtime.json" '{"status":"pass","obligations":["docs.readme.runtime_signal.gatemole_gate_decision"]}'
  write_text_file "$repo/.gatemole/artifacts/rollback.json" '{"status":"pass","obligations":["docs.readme.rollback.revert_change"]}'
  write_text_file "$repo/.gatemole/test-map.json" '{"version":"gatemole.test_map.v0","mappings":{"docs.readme.required_test.documentation_smoke_check":["tests/docs/test_readme.py::test_documentation_smoke_check"]}}'
  write_text_file "$repo/.gatemole/artifacts/tests.xml" '<testsuite name="docs" tests="1" failures="0" errors="0" skipped="0"><testcase classname="tests.docs.test_readme" name="test_documentation_smoke_check" file="tests/docs/test_readme.py"></testcase></testsuite>'

  "$GATEMOLE" --repo "$repo" manifest attach-artifact \
    --manifest .gatemole/manifests/docs.json \
    --id behavior \
    --kind behavior_trace \
    --path .gatemole/artifacts/behavior.json \
    --exit-code 0 \
    --out .gatemole/manifests/docs.json > "$dir/attach-behavior.stdout"
  "$GATEMOLE" --repo "$repo" manifest attach-artifact \
    --manifest .gatemole/manifests/docs.json \
    --id security \
    --kind security_check \
    --path .gatemole/artifacts/security.json \
    --exit-code 0 \
    --out .gatemole/manifests/docs.json > "$dir/attach-security.stdout"
  "$GATEMOLE" --repo "$repo" manifest attach-artifact \
    --manifest .gatemole/manifests/docs.json \
    --id tests \
    --kind test_coverage \
    --path .gatemole/artifacts/tests.xml \
    --test-map .gatemole/test-map.json \
    --exit-code 0 \
    --out .gatemole/manifests/docs.json > "$dir/attach-tests.stdout"
  "$GATEMOLE" --repo "$repo" manifest attach-artifact \
    --manifest .gatemole/manifests/docs.json \
    --id runtime \
    --kind runtime_metric \
    --path .gatemole/artifacts/runtime.json \
    --exit-code 0 \
    --out .gatemole/manifests/docs.json > "$dir/attach-runtime.stdout"
  "$GATEMOLE" --repo "$repo" manifest attach-artifact \
    --manifest .gatemole/manifests/docs.json \
    --id rollback \
    --kind rollback_plan \
    --path .gatemole/artifacts/rollback.json \
    --exit-code 0 \
    --out .gatemole/manifests/docs.json > "$dir/attach-rollback.stdout"

  run_gate "$dir" "$repo" ".gatemole/manifests/docs.json"
  assert_scenario "$dir"
}

render_results() {
  local json_out="$OUT_DIR/gatemolebench.latest.json"
  local md_out="$OUT_DIR/gatemolebench.latest.md"

  python3 - "$json_out" "$md_out" "${SCENARIOS[@]}" <<'PY'
from __future__ import annotations

import datetime as dt
import json
import statistics
import sys
from pathlib import Path

json_out = Path(sys.argv[1])
md_out = Path(sys.argv[2])
scenario_dirs = [Path(p) for p in sys.argv[3:]]

required_scenarios = [
    "auth_tests_only",
    "auth_partial_release_evidence",
    "auth_manifest_traceability_block",
    "auth_nonzero_test_artifact",
    "auth_full_release_evidence",
    "auth_full_release_without_canary",
    "platform_multi_component_partial_evidence",
    "platform_multi_component_full_canary",
    "platform_medium_api_auto_merge",
    "docs_low_risk_full_evidence",
]


def load(path: Path) -> dict:
    with path.open(encoding="utf-8") as f:
        return json.load(f)


def criterion(criteria: list[dict], criterion_id: str, passed: bool, expected: object, actual: object, description: str) -> None:
    criteria.append({
        "id": criterion_id,
        "passed": bool(passed),
        "expected": expected,
        "actual": actual,
        "description": description,
    })


rows = [load(path / "row.json") for path in scenario_dirs]
rows_by_id = {row["id"]: row for row in rows}
assertion_count = sum(len(row["assertions"]) for row in rows)
assertions_passed = sum(
    1
    for row in rows
    for item in row["assertions"]
    if item["passed"]
)
gate_times = [row["actual"]["gate_ms"] for row in rows]
tests_passed_rows = [row for row in rows if row["baseline"]["tests_passed"]]
tests_failed_rows = [row for row in rows if not row["baseline"]["tests_passed"]]
tests_passed_block_rows = [
    row for row in rows
    if row["baseline"]["tests_passed"]
    and row["expected"]["decision"] == "block"
    and row["actual"]["decision"] == "block"
]
nonblocking_rows = [row for row in rows if row["expected"]["decision"] != "block"]
full_coverage_block_rows = [
    row for row in rows
    if row["actual"]["decision"] == "block"
    and row["actual"]["covered_obligations"] == row["actual"]["total_obligations"]
]
medium_high_scale_rows = [
    row for row in rows
    if row["actual"]["total_obligations"] >= 25
]
invalid_evidence_rows = [
    row for row in rows
    if len(row["actual"]["invalid_evidence"]) > 0
]
manifest_traceability_rows = [
    row for row in rows
    if row["actual"]["decision"] == "block"
    and row["actual"]["manifest_error_count"] > 0
]

summary = {
    "scenario_count": len(rows),
    "assertions_passed": assertions_passed,
    "assertion_count": assertion_count,
    "expected_decisions_met": sum(1 for row in rows if row["actual"]["decision"] == row["expected"]["decision"]),
    "expected_gate_exit_codes_met": sum(1 for row in rows if row["actual"]["gate_exit_code"] == row["expected"]["gate_exit_code"]),
    "tests_passed_scenarios": len(tests_passed_rows),
    "tests_failed_scenarios": len(tests_failed_rows),
    "tests_passed_expected_block_scenarios": len([row for row in rows if row["baseline"]["tests_passed"] and row["expected"]["decision"] == "block"]),
    "tests_passed_scenarios_gatemole_blocked": len(tests_passed_block_rows),
    "tests_failed_scenarios_gatemole_blocked": len([row for row in tests_failed_rows if row["actual"]["decision"] == "block"]),
    "nonblocking_policy_routes": len(nonblocking_rows),
    "nonblocking_policy_routes_met": sum(1 for row in nonblocking_rows if row["actual"]["decision"] == row["expected"]["decision"]),
    "full_coverage_block_scenarios": len(full_coverage_block_rows),
    "medium_high_scale_scenarios": len(medium_high_scale_rows),
    "max_obligations_in_scenario": max([row["actual"]["total_obligations"] for row in rows], default=0),
    "invalid_evidence_scenarios": len(invalid_evidence_rows),
    "manifest_traceability_block_scenarios": len(manifest_traceability_rows),
    "median_gate_ms": round(statistics.median(gate_times), 3) if gate_times else 0,
    "max_gate_ms": round(max(gate_times), 3) if gate_times else 0,
}

criteria: list[dict] = []
criterion(
    criteria,
    "required_scenario_corpus",
    sorted(rows_by_id) == sorted(required_scenarios),
    sorted(required_scenarios),
    sorted(rows_by_id),
    "the benchmark must keep the required scenario corpus intact",
)
criterion(
    criteria,
    "all_scenario_assertions_passed",
    assertions_passed == assertion_count and assertion_count > 0,
    f"{assertion_count}/{assertion_count}",
    f"{assertions_passed}/{assertion_count}",
    "every scenario-level machine-readable assertion must pass",
)
criterion(
    criteria,
    "tests_passed_blocks_preserved",
    len(tests_passed_block_rows) >= 4,
    ">=4",
    len(tests_passed_block_rows),
    "at least four tests-passed scenarios must still be blocked by Gatemole-specific checks",
)
criterion(
    criteria,
    "nonblocking_routes_preserved",
    summary["nonblocking_policy_routes_met"] >= 3,
    ">=3",
    summary["nonblocking_policy_routes_met"],
    "canary, human escalation, and auto-merge non-blocking routes must all be exercised",
)
criterion(
    criteria,
    "medium_high_repo_scale_preserved",
    len(medium_high_scale_rows) >= 2 and summary["max_obligations_in_scenario"] >= 25,
    {
        "min_scenarios_with_25_obligations": 2,
        "min_max_obligations": 25,
    },
    {
        "scenarios_with_25_obligations": len(medium_high_scale_rows),
        "max_obligations": summary["max_obligations_in_scenario"],
    },
    "the corpus must include medium/high multi-component repo scenarios with at least 25 obligations",
)
criterion(
    criteria,
    "invalid_evidence_detection_preserved",
    len(invalid_evidence_rows) >= 1,
    ">=1",
    len(invalid_evidence_rows),
    "the corpus must include at least one invalid artifact evidence block",
)
criterion(
    criteria,
    "full_coverage_manifest_block_preserved",
    len(full_coverage_block_rows) >= 1 and len(manifest_traceability_rows) >= 1,
    ">=1 full-coverage manifest block",
    {
        "full_coverage_block_scenarios": len(full_coverage_block_rows),
        "manifest_traceability_block_scenarios": len(manifest_traceability_rows),
    },
    "the corpus must prove full coverage can still block on manifest traceability",
)

acceptance = {
    "passed": all(item["passed"] for item in criteria),
    "criteria": criteria,
}
result = {
    "version": "gatemolebench.v1",
    "generated_at": dt.datetime.now(dt.timezone.utc).isoformat(),
    "acceptance": acceptance,
    "summary": summary,
    "scenarios": rows,
}
json_out.parent.mkdir(parents=True, exist_ok=True)
with json_out.open("w", encoding="utf-8") as f:
    json.dump(result, f, indent=2, sort_keys=True)
    f.write("\n")

status = "pass" if acceptance["passed"] else "fail"
lines = [
    "# GatemoleBench Latest",
    "",
    f"Generated: `{result['generated_at']}`",
    "",
    "## Acceptance",
    "",
    f"- Status: `{status}`",
    f"- Assertions passed: {assertions_passed}/{assertion_count}",
    f"- Required scenarios present: {len(rows_by_id)}/{len(required_scenarios)}",
    "",
    "| Criterion | Result | Actual | Expected |",
    "| --- | --- | --- | --- |",
]
for item in criteria:
    result_word = "pass" if item["passed"] else "fail"
    actual = json.dumps(item["actual"], sort_keys=True) if isinstance(item["actual"], (dict, list)) else str(item["actual"])
    expected = json.dumps(item["expected"], sort_keys=True) if isinstance(item["expected"], (dict, list)) else str(item["expected"])
    lines.append(f"| `{item['id']}` | `{result_word}` | `{actual}` | `{expected}` |")

lines.extend([
    "",
    "## Baseline Accounting",
    "",
    f"- Tests-passed scenarios: {summary['tests_passed_scenarios']}/{summary['scenario_count']}",
    f"- Tests-failed negative controls: {summary['tests_failed_scenarios']}/{summary['scenario_count']}",
    f"- Tests-passed scenarios expected to block: {summary['tests_passed_expected_block_scenarios']}",
    f"- Tests-passed scenarios Gatemole blocked: {summary['tests_passed_scenarios_gatemole_blocked']}/{summary['tests_passed_expected_block_scenarios']}",
    f"- Non-blocking policy routes matched: {summary['nonblocking_policy_routes_met']}/{summary['nonblocking_policy_routes']}",
    f"- Full-coverage block scenarios: {summary['full_coverage_block_scenarios']}",
    f"- Medium/high scale scenarios: {summary['medium_high_scale_scenarios']} with max {summary['max_obligations_in_scenario']} obligations",
    f"- Invalid-evidence scenarios: {summary['invalid_evidence_scenarios']}",
    f"- Median gate runtime: {summary['median_gate_ms']} ms",
    f"- Max gate runtime: {summary['max_gate_ms']} ms",
    "",
    "## Scenarios",
    "",
    "| Scenario | Baseline | Tests | Expected | Gatemole | Exit | Coverage | Assertions |",
    "| --- | --- | --- | --- | --- | ---: | --- | --- |",
])
for row in rows:
    tests = "pass" if row["baseline"]["tests_passed"] else "fail"
    assertions_met = sum(1 for item in row["assertions"] if item["passed"])
    assertions_total = len(row["assertions"])
    coverage = f"{row['actual']['covered_obligations']}/{row['actual']['total_obligations']}"
    lines.append(
        f"| `{row['id']}` | `{row['baseline']['id']}` | `{tests}` | "
        f"`{row['expected']['decision']}` | `{row['actual']['decision']}` | "
        f"{row['actual']['gate_exit_code']} | `{coverage}` | {assertions_met}/{assertions_total} |"
    )
lines.extend([
    "",
    "## Claims Exercised",
    "",
])
for row in rows:
    lines.append(f"- `{row['id']}`: {row['claim']}")

md_out.parent.mkdir(parents=True, exist_ok=True)
md_out.write_text("\n".join(lines) + "\n", encoding="utf-8")
print(md_out.read_text(encoding="utf-8"))

if not acceptance["passed"]:
    print("gatemolebench: acceptance failed", file=sys.stderr)
    for item in criteria:
        if not item["passed"]:
            print(f"- {item['id']}: expected {item['expected']!r}, got {item['actual']!r}", file=sys.stderr)
    sys.exit(1)
PY
}

echo "building local gatemole binary..."
(cd "$ROOT" && GOCACHE="${GOCACHE:-$RUN_DIR/gocache}" go build -o "$GATEMOLE" ./cmd/gatemole)

echo "running benchmark scenarios..."
add_auth_tests_only
add_auth_partial_release_evidence
add_auth_manifest_traceability_block
add_auth_nonzero_test_artifact
add_auth_full_release_evidence
add_auth_full_release_without_canary
add_platform_multi_component_partial_evidence
add_platform_multi_component_full_canary
add_platform_medium_api_auto_merge
add_docs_low_risk_full_evidence

render_results
echo "wrote:"
echo "  $OUT_DIR/gatemolebench.latest.json"
echo "  $OUT_DIR/gatemolebench.latest.md"
