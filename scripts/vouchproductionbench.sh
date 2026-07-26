#!/bin/sh
set -eu

production_image=${VOUCH_PRODUCTION_IMAGE:-}
keep_bench=${VOUCH_PRODUCTION_KEEP:-0}
case "$keep_bench" in
  0|1) ;;
  *)
    echo "VOUCH_PRODUCTION_KEEP must be 0 or 1" >&2
    exit 2
    ;;
esac
case "$production_image" in
  sha256:????????????????????????????????????????????????????????????????) ;;
  *@sha256:????????????????????????????????????????????????????????????????) ;;
  *)
    echo "VOUCH_PRODUCTION_IMAGE is required and must be a local sha256 image ID or digest-pinned reference" >&2
    echo 'build one with: scripts/vouchproductionfixture.sh --tag vouch-production-fixture:acceptance' >&2
    exit 2
    ;;
esac

bench_root=$(mktemp -d "${TMPDIR:-/tmp}/vouch-production-bench.XXXXXX")
bench_root=$(cd "$bench_root" && pwd -P)
touch "$bench_root/.vouch-production-bench-root"
repo="$bench_root/repo"
socket="$bench_root/vouchd.sock"
database="$bench_root/kernel.db"
transactions="$bench_root/transactions"
bin_dir="$bench_root/bin"
broker_build_context="$bench_root/model-broker-build"
daemon_pid=
cleanup_ran=0
broker_image_tag="vouch-model-broker-bench-$$"
identity_issuer="https://vouch-production-bench.example.invalid"
identity_audience="vouch-production-bench"

cleanup() {
  if [ "$cleanup_ran" = 1 ]; then
    return
  fi
  cleanup_ran=1
  if [ -n "$daemon_pid" ]; then
    kill "$daemon_pid" 2>/dev/null || true
    wait "$daemon_pid" 2>/dev/null || true
  fi
  docker image rm "$broker_image_tag" >/dev/null 2>&1 || true
  if [ "$keep_bench" = 0 ] &&
    [ -f "$bench_root/.vouch-production-bench-root" ]; then
    case "${bench_root##*/}" in
      vouch-production-bench.*)
        rm -rf -- "$bench_root"
        ;;
    esac
  else
    echo "production benchmark artifacts retained at $bench_root; this directory contains generated private keys" >&2
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$repo/internal/auth" "$bin_dir" "$broker_build_context"
printf 'package auth\n\nfunc Allowed() bool { return false }\n' > "$repo/internal/auth/middleware.go"
printf 'package auth\n\nfunc TestAllowed() {}\n' > "$repo/internal/auth/middleware_test.go"
printf 'verifier-poison\n' > "$repo/.gitignore"
git -C "$repo" init --initial-branch=main >/dev/null
git -C "$repo" add -- .gitignore internal/auth/middleware.go internal/auth/middleware_test.go
git -C "$repo" -c user.name='Vouch Production Bench' -c user.email='vouch@example.invalid' commit -m fixture >/dev/null
git -C "$repo" branch release/production-bench HEAD

GOCACHE="$bench_root/go-build" go build -o "$bin_dir/vouch" ./cmd/vouch
GOCACHE="$bench_root/go-build" go build -o "$bin_dir/vouchd" ./cmd/vouchd
broker_arch=$(docker version --format '{{.Server.Arch}}')
CGO_ENABLED=0 GOOS=linux GOARCH="$broker_arch" \
  GOCACHE="$bench_root/go-build-linux" \
  go build -o "$broker_build_context/vouch-model-broker" ./cmd/vouch-model-broker
docker build \
  --network=none \
  --pull=false \
  --tag "$broker_image_tag" \
  --file build/model-broker.Dockerfile \
  "$broker_build_context" >/dev/null
broker_image=$(docker image inspect --format '{{.Id}}' "$broker_image_tag")

cat >"$bench_root/model-policy.json" <<'EOF'
{
  "version": "vouch.model_broker_policy.v0",
  "provider": "openai",
  "upstream_base_url": "https://api.openai.invalid",
  "allowed_models": ["gpt-broker-bench"],
  "allowed_tool_types": ["function"],
  "max_requests": 2,
  "max_request_bytes": 1048576,
  "max_response_bytes": 1048576,
  "max_output_tokens_per_request": 100,
  "max_total_input_tokens": 1000,
  "max_total_output_tokens": 200,
  "request_timeout_seconds": 5,
  "force_store_false": true,
  "allow_stateful_requests": false
}
EOF

verifier_program='
    set -eu
    test "$(id -u)" -ne 0
    test "$VOUCH_RUNTIME_ROLE" = verifier
    if printf forbidden > internal/auth/verifier-write-probe.go 2>/dev/null; then exit 93; fi
    test ! -e verifier-poison
    grep -q "return true" internal/auth/middleware.go
  '
jq -n \
  --arg image "$production_image" \
  --arg command "$verifier_program" \
  '{
    version: "vouch.verifier_profiles.v0",
    profiles: [{
      name: "authentication-invariant",
      image: $image,
      command: ["/bin/sh", "-c", $command],
      timeout_seconds: 900
    }]
  }' >"$bench_root/verifier-profiles.json"
chmod 600 "$bench_root/verifier-profiles.json"

"$bin_dir/vouch" --repo "$repo" approval keygen \
  --key-id key:production-bench \
  --principal human:production-reviewer \
  --issuer "$identity_issuer" \
  --class security-reviewer \
  --private-key "$bench_root/approval.key" \
  --trust-file "$bench_root/approval-trust.json" >/dev/null

"$bin_dir/vouch" --repo "$repo" identity keygen \
  --key-id key:production-identity \
  --issuer "$identity_issuer" \
  --audience "$identity_audience" \
  --private-key "$bench_root/identity.key" \
  --trust-file "$bench_root/identity-trust.json" >/dev/null

operator_token=$("$bin_dir/vouch" --repo "$repo" identity issue \
  --key "$bench_root/identity.key" \
  --key-id key:production-identity \
  --issuer "$identity_issuer" \
  --audience "$identity_audience" \
  --principal operator:local \
  --kind operator \
  --namespaces production-bench \
  --roles viewer,operator \
  --ttl 30m)

reviewer_token=$("$bin_dir/vouch" --repo "$repo" identity issue \
  --key "$bench_root/identity.key" \
  --key-id key:production-identity \
  --issuer "$identity_issuer" \
  --audience "$identity_audience" \
  --principal human:production-reviewer \
  --kind human \
  --namespaces production-bench \
  --roles viewer,approver,operator \
  --ttl 30m)

VOUCH_BENCH_PROVIDER_TOKEN='provider-secret-must-never-reach-agent' "$bin_dir/vouchd" \
  --repo "$repo" \
  --db "$database" \
  --socket "$socket" \
  --transaction-root "$transactions" \
  --runtime-profile production \
  --allowed-images "$production_image" \
  --approval-trust "$bench_root/approval-trust.json" \
  --identity-trust "$bench_root/identity-trust.json" \
  --verifier-profiles "$bench_root/verifier-profiles.json" \
  --allowed-git-refs 'refs/heads/release/*' \
  --model-broker-image "$broker_image" \
  --model-broker-policy "$bench_root/model-policy.json" \
  --model-provider-token-env VOUCH_BENCH_PROVIDER_TOKEN \
  >"$bench_root/daemon.log" 2>&1 &
daemon_pid=$!

attempt=0
while [ ! -S "$socket" ]; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 240 ]; then
    cat "$bench_root/daemon.log" >&2
    echo "production daemon did not become ready" >&2
    exit 1
  fi
  sleep 0.05
done

attempt=0
while ! curl --silent --fail --unix-socket "$socket" \
  http://localhost/readyz >"$bench_root/ready.json"; do
  attempt=$((attempt + 1))
  if [ "$attempt" -ge 240 ]; then
    cat "$bench_root/daemon.log" >&2
    echo "production daemon did not become ready" >&2
    exit 1
  fi
  sleep 0.05
done
test "$(jq -r '.status' "$bench_root/ready.json")" = ready

legacy_status=$(curl --silent --unix-socket "$socket" \
  --header "Authorization: Bearer $operator_token" \
  --header 'Content-Type: application/json' \
  --output "$bench_root/legacy-host-api.json" \
  --write-out '%{http_code}' \
  --data '{}' \
  http://localhost/v0/namespaces/production-bench/runs/run:legacy-host/events)
test "$legacy_status" = 403
test "$(jq -r '.code' "$bench_root/legacy-host-api.json")" = KERNEL_CAPABILITY_DENIED
test "$(jq -r '.operation' "$bench_root/legacy-host-api.json")" = legacy_host_runtime

VOUCH_IDENTITY_TOKEN="$operator_token" "$bin_dir/vouch" --repo "$repo" --json tx run \
  --socket "$socket" \
  --id tx:production-bench \
  --namespace production-bench \
  --intent 'Change authentication under a production transaction boundary' \
  --run run:production-bench \
  --runtime oci \
  --image "$production_image" \
  -- \
  /bin/sh -c '
    set -eu
    test "$(id -u)" -ne 0
    test "$VOUCH_RUNTIME_ROLE" = agent
    if printf forbidden > /vouch-rootfs-probe 2>/dev/null; then exit 91; fi
    if env | grep -q "provider-secret-must-never-reach-agent"; then exit 95; fi
    python -c "import socket
s=socket.socket()
s.settimeout(0.25)
try:
 s.connect((\"1.1.1.1\", 80))
except OSError:
 pass
else:
 raise SystemExit(92)"
    python -c "import json, os, urllib.error, urllib.request
body=json.dumps({
 \"model\":\"gpt-broker-bench\",
 \"input\":\"sensitive benchmark prompt absent from receipts\",
 \"max_output_tokens\":10,
 \"store\":True
}).encode()
request=urllib.request.Request(
 os.environ[\"OPENAI_BASE_URL\"]+\"/responses\",
 data=body,
 headers={
  \"Authorization\":\"Bearer \"+os.environ[\"OPENAI_API_KEY\"],
  \"Content-Type\":\"application/json\"
 }
)
try:
 urllib.request.urlopen(request, timeout=10)
except urllib.error.HTTPError as error:
 if error.code != 502:
  raise
else:
 raise SystemExit(94)"
    printf "package auth\n\nfunc Allowed() bool { return true }\n" > internal/auth/middleware.go
    printf "package auth\n\nfunc TestAllowed() { /* independently checked */ }\n" > internal/auth/middleware_test.go
    printf "ignored live-worktree content must not enter the verifier snapshot\n" > verifier-poison
  ' >"$bench_root/run.json"

workspace=$(jq -r '.workspace' "$bench_root/run.json")
case "$workspace" in
  "$transactions"/*) ;;
  *)
    echo "daemon returned a transaction workspace outside the staging root" >&2
    exit 1
    ;;
esac
test -f "$workspace/verifier-poison"
git -C "$workspace" check-ignore --quiet -- verifier-poison

if VOUCH_IDENTITY_TOKEN="$operator_token" "$bin_dir/vouch" --repo "$repo" --json tx verify \
  --socket "$socket" \
  --namespace production-bench \
  --id tx:production-bench \
  --name authentication-invariant \
  --image "$production_image" \
  -- /bin/true >"$bench_root/substituted-verifier.json" 2>"$bench_root/substituted-verifier.stderr"; then
  echo "production accepted a verifier command outside the daemon-owned profile" >&2
  exit 1
fi
grep -q 'KERNEL_CAPABILITY_DENIED' "$bench_root/substituted-verifier.stderr"

VOUCH_IDENTITY_TOKEN="$operator_token" "$bin_dir/vouch" --repo "$repo" --json tx verify \
  --socket "$socket" \
  --namespace production-bench \
  --id tx:production-bench \
  --name authentication-invariant \
  --image "$production_image" \
  -- /bin/sh -c "$verifier_program" >"$bench_root/verify.json"

test -f "$workspace/verifier-poison"

main_before_symref=$(git -C "$repo" rev-parse refs/heads/main)
git -C "$repo" symbolic-ref refs/heads/release/symref-escape refs/heads/main
if VOUCH_IDENTITY_TOKEN="$operator_token" "$bin_dir/vouch" --repo "$repo" --json tx prepare \
  --socket "$socket" \
  --namespace production-bench \
  --id tx:production-bench \
  --git-ref refs/heads/release/symref-escape \
  >"$bench_root/symref-prepare.json" 2>"$bench_root/symref-prepare.stderr"; then
  echo "production accepted a symbolic release ref" >&2
  exit 1
fi
grep -q 'KERNEL_SCHEMA_INVALID' "$bench_root/symref-prepare.stderr"
grep -q 'invalid Git release target' "$bench_root/symref-prepare.stderr"
test "$(git -C "$repo" rev-parse refs/heads/main)" = "$main_before_symref"
git -C "$repo" symbolic-ref --delete refs/heads/release/symref-escape

VOUCH_IDENTITY_TOKEN="$operator_token" "$bin_dir/vouch" --repo "$repo" --json tx prepare \
  --socket "$socket" \
  --namespace production-bench \
  --id tx:production-bench \
  --git-ref refs/heads/release/production-bench >"$bench_root/prepare.json"

VOUCH_IDENTITY_TOKEN="$reviewer_token" "$bin_dir/vouch" --repo "$repo" --json tx approve \
  --socket "$socket" \
  --namespace production-bench \
  --id tx:production-bench \
  --key "$bench_root/approval.key" \
  --key-id key:production-bench \
  --approver human:production-reviewer \
  --issuer "$identity_issuer" \
  --class security-reviewer >"$bench_root/approve.json"

if VOUCH_IDENTITY_TOKEN="$reviewer_token" "$bin_dir/vouch" --repo "$repo" --json tx release \
  --socket "$socket" \
  --namespace production-bench \
  --id tx:production-bench \
  --actor human:production-reviewer \
  --actor-kind human \
  >"$bench_root/self-release.json" 2>"$bench_root/self-release.stderr"; then
  echo "production allowed the approving reviewer to release the transaction" >&2
  exit 1
fi
grep -q 'KERNEL_APPROVAL_INVALID' "$bench_root/self-release.stderr"

VOUCH_IDENTITY_TOKEN="$operator_token" "$bin_dir/vouch" --repo "$repo" --json tx release \
  --socket "$socket" \
  --namespace production-bench \
  --id tx:production-bench >"$bench_root/release.json"

VOUCH_IDENTITY_TOKEN="$operator_token" "$bin_dir/vouch" --repo "$repo" --json tx events \
  --socket "$socket" \
  --namespace production-bench \
  --id tx:production-bench >"$bench_root/events.json"

test "$(jq -r '.projection.transaction.state' "$bench_root/release.json")" = committed
test "$(jq -r '.publish.status' "$bench_root/release.json")" = applied
test "$(git -C "$repo" rev-parse refs/heads/main)" = "$main_before_symref"
git -C "$repo" show refs/heads/release/production-bench:internal/auth/middleware.go | grep -q 'return true'
grep -q 'return false' "$repo/internal/auth/middleware.go"
test ! -e "$repo/internal/auth/verifier-write-probe.go"
test "$(jq -r '.execution.model_broker.calls' "$bench_root/run.json")" = 1
test "$(jq -r '.execution.model_broker.unknown_calls' "$bench_root/run.json")" = 1
jq -e '.execution.model_broker.receipt_ledger_digest | test("^sha256:[a-f0-9]{64}$")' "$bench_root/run.json" >/dev/null
jq -e --arg issuer "$identity_issuer" '
  any(.[]; .actor.issuer == $issuer and
    (.actor.claims_digest | test("^sha256:[a-f0-9]{64}$")))
' "$bench_root/events.json" >/dev/null
if grep -R -E -q 'sensitive benchmark prompt|provider-secret-must-never-reach-agent' "$transactions/.vouch-model-evidence"; then
  echo "model receipt ledger leaked a prompt or provider secret" >&2
  exit 1
fi

printf 'production runtime acceptance passed\n'
printf 'image=%s\n' "$production_image"
printf 'transaction=tx:production-bench\n'
printf 'state=%s\n' "$(jq -r '.projection.transaction.state' "$bench_root/release.json")"
printf 'release=%s\n' "$(jq -r '.prepared.commit_revision' "$bench_root/release.json")"
printf 'model_calls=%s\n' "$(jq -r '.execution.model_broker.calls' "$bench_root/run.json")"
printf 'model_unknown_calls=%s\n' "$(jq -r '.execution.model_broker.unknown_calls' "$bench_root/run.json")"
if [ "$keep_bench" = 1 ]; then
  printf 'artifacts=%s\n' "$bench_root"
fi
