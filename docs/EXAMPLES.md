# Runtime examples

Vouch's implemented production boundary today is a Git repository on one
trusted node. These examples therefore use software-delivery work rather than
pretending that the current repository can already control SAP, Salesforce or
bank transfers.

## 1. Run the repository production-acceptance scenario

The fastest end-to-end local-Git example is the same acceptance flow used
before release. It requires Git, Go, Docker, `jq`, and `curl`:

```sh
go test ./...

AGENT_IMAGE="$(
  scripts/vouchproductionfixture.sh \
    --tag vouch-production-fixture:example
)"
VOUCH_PRODUCTION_IMAGE="$AGENT_IMAGE" \
  scripts/vouchproductionbench.sh
```

It creates a temporary authentication repository, production OIDC identities,
an operator/releaser plus an independent reviewer, a policy-controlled model
broker, daemon-owned agent and verifier containers, a frozen approval package,
and an allowed release ref. A successful run ends with output like:

```text
production runtime acceptance passed
transaction=tx:production-bench
state=committed
model_calls=1
model_unknown_calls=1
```

The provider call intentionally receives a test upstream failure, so the
receipt records an unknown call rather than pretending it succeeded. The
scenario also proves that the provider credential never reaches the agent and
that direct agent Internet egress is blocked.

## 2. Runnable deterministic fixture: authentication hotfix

This example creates a small payments-service repository and a digest-pinned
agent image. The agent changes authentication code and its test inside a Vouch
transaction. The source checkout remains unchanged.

Run the example as a non-root user with Git, Go, Docker, and `jq` installed.

### Create the service repository

```sh
EXAMPLE_ROOT="$(mktemp -d)"
REPO="$EXAMPLE_ROOT/payments-api"
mkdir -p "$REPO/internal/auth"

git -C "$REPO" init --initial-branch=main
printf 'package auth\n\nfunc RefreshAllowed() bool { return false }\n' \
  > "$REPO/internal/auth/refresh.go"
printf 'package auth\n\nfunc TestRefreshAllowed() {}\n' \
  > "$REPO/internal/auth/refresh_test.go"
git -C "$REPO" add -- internal/auth
git -C "$REPO" \
  -c user.name='Example Developer' \
  -c user.email='developer@example.invalid' \
  commit -m 'initial payments service'
```

### Build the example agent

The program below is intentionally deterministic so the example is
reproducible. A real coding-agent image follows the same contract: read
`$VOUCH_TASK_PATH`, edit `/workspace`, and exit.

```sh
AGENT_SRC="$EXAMPLE_ROOT/auth-hotfix-agent"
mkdir -p "$AGENT_SRC"

cat > "$AGENT_SRC/main.go" <<'GO'
package main

import (
	"bytes"
	"fmt"
	"os"
)

func replace(path, before, after string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated := bytes.Replace(data, []byte(before), []byte(after), 1)
	if bytes.Equal(data, updated) {
		return fmt.Errorf("%s did not contain the expected text", path)
	}
	return os.WriteFile(path, updated, 0o600)
}

func main() {
	if _, err := os.ReadFile(os.Getenv("VOUCH_TASK_PATH")); err != nil {
		panic(err)
	}
	if err := replace(
		"/workspace/internal/auth/refresh.go",
		"return false",
		"return true",
	); err != nil {
		panic(err)
	}
	if err := replace(
		"/workspace/internal/auth/refresh_test.go",
		"func TestRefreshAllowed() {}",
		"func TestRefreshAllowed() { /* independently verified later */ }",
	); err != nil {
		panic(err)
	}
}
GO

ENGINE_ARCH="$(docker version --format '{{.Server.Arch}}')"
CGO_ENABLED=0 GOOS=linux GOARCH="$ENGINE_ARCH" \
  go build -trimpath \
  -o "$AGENT_SRC/auth-hotfix-agent" \
  "$AGENT_SRC/main.go"

cat > "$AGENT_SRC/Dockerfile" <<'DOCKER'
FROM scratch
COPY auth-hotfix-agent /auth-hotfix-agent
DOCKER

docker build \
  --network=none \
  --pull=false \
  --tag vouch-example-auth-agent:local \
  "$AGENT_SRC"
AGENT_IMAGE="$(
  docker image inspect \
    --format '{{.Id}}' \
    vouch-example-auth-agent:local
)"
```

`AGENT_IMAGE` is the immutable local `sha256:...` image ID. Production
deployments normally use a registry reference of the form
`registry.example/agent@sha256:...`.

### Start the Runtime and run the task

```sh
go install ./cmd/vouch ./cmd/vouchd

RUNTIME="$EXAMPLE_ROOT/runtime"
SOCKET="$RUNTIME/vouchd.sock"
mkdir -p "$RUNTIME"

vouchd \
  --repo "$REPO" \
  --db "$RUNTIME/kernel.db" \
  --socket "$SOCKET" \
  --transaction-root "$RUNTIME/transactions" \
  >"$RUNTIME/vouchd.log" 2>&1 &
DAEMON_PID=$!

while [ ! -S "$SOCKET" ]; do
  sleep 0.1
done

vouch --repo "$REPO" --json run \
  --socket "$SOCKET" \
  --namespace payments \
  --id tx:pay-1842 \
  --run run:pay-1842 \
  --intent "Fix PAY-1842: valid refresh tokens are rejected" \
  --runtime oci \
  --image "$AGENT_IMAGE" \
  -- /auth-hotfix-agent \
  > "$EXAMPLE_ROOT/run.json"

jq '{
  transaction: .projection.transaction.id,
  state: .projection.transaction.state,
  execution: .execution.status,
  effects: (.projection.effects | length),
  decision: .decision.outcome
}' "$EXAMPLE_ROOT/run.json"
```

The result is:

```json
{
  "transaction": "tx:pay-1842",
  "state": "validating",
  "execution": "succeeded",
  "effects": 2,
  "decision": "require_approval"
}
```

The response contains the detached workspace, daemon-authored execution
receipt, normalized effects, and sequence decision. Inspect the durable state:

```sh
vouch --repo "$REPO" status tx:pay-1842 \
  --socket "$SOCKET" \
  --namespace payments

vouch --repo "$REPO" tx effects \
  --socket "$SOCKET" \
  --namespace payments \
  --id tx:pay-1842

vouch --repo "$REPO" tx events \
  --socket "$SOCKET" \
  --namespace payments \
  --id tx:pay-1842

# The agent never edited the developer's source checkout.
git -C "$REPO" diff --exit-code
```

Because authentication code and its test changed together, the baseline
sequence policy routes this transaction to focused approval. It does not
silently publish the change. Stop the example daemon with:

```sh
kill "$DAEMON_PID"
```

## 3. Illustrative deployment pattern: model-assisted coding agent

Unlike the first two sections, this is not a self-contained shipped example.
It assumes that the operator has supplied the named agent and verifier images,
OIDC configuration and production trust material.

Assume a payments team has:

- repository `/srv/repos/payments-api`;
- ticket file `tickets/PAY-1842.md`;
- repository-owned agent profile `payments-coder`;
- a daemon-configured `openai` broker policy;
- independent verifier image and security approver.

The operator uses its short-lived OIDC token for admission, execution,
verification, and preparation:

```sh
export VOUCH_IDENTITY_TOKEN="$PAYMENTS_OPERATOR_TOKEN"
```

The allowed release ref must already exist at the transaction's exact base
revision and must not be checked out:

```sh
git -C /srv/repos/payments-api \
  branch agent-release/pay-1842 HEAD
```

The developer starts exactly one admitted task:

```sh
vouch --repo /srv/repos/payments-api run \
  --socket /run/vouch/vouchd.sock \
  --namespace payments \
  --id tx:pay-1842 \
  --run run:pay-1842 \
  --intent-file tickets/PAY-1842.md \
  --agent payments-coder \
  --model-provider openai
```

The coding agent receives the task and detached worktree. It can reach only
the transaction-specific model broker. `OPENAI_API_KEY` inside the container is
a short-lived broker token; the provider key remains in `vouchd`. Direct
Internet access, the source checkout, daemon socket, GitHub token and production
database credentials are absent.

After the agent exits, the team independently verifies the frozen tree:

```sh
VERIFIER_IMAGE="$(cat /etc/vouch/images/go-verifier.ref)"

vouch --repo /srv/repos/payments-api tx verify \
  --socket /run/vouch/vouchd.sock \
  --namespace payments \
  --id tx:pay-1842 \
  --name auth-tests \
  --image "$VERIFIER_IMAGE" \
  -- /usr/local/bin/run-auth-tests

vouch --repo /srv/repos/payments-api tx prepare \
  --socket /run/vouch/vouchd.sock \
  --namespace payments \
  --id tx:pay-1842 \
  --git-ref refs/heads/agent-release/pay-1842
```

The verifier image and command must exactly match a daemon-owned verifier
profile. A security reviewer then signs the exact frozen approval package:

```sh
export VOUCH_IDENTITY_TOKEN="$PAYMENTS_REVIEWER_TOKEN"

vouch --repo /srv/repos/payments-api tx approve \
  --socket /run/vouch/vouchd.sock \
  --namespace payments \
  --id tx:pay-1842 \
  --key /secure/payments-reviewer.key \
  --key-id key:payments-reviewer \
  --approver human:alice \
  --issuer https://login.acme.example/ \
  --class security-reviewer
```

The issuer must exactly match the trusted OIDC issuer. A different release
identity performs release:

```sh
export VOUCH_IDENTITY_TOKEN="$PAYMENTS_RELEASER_TOKEN"

vouch --repo /srv/repos/payments-api tx release \
  --socket /run/vouch/vouchd.sock \
  --namespace payments \
  --id tx:pay-1842 \
  --actor operator:payments-release \
  --actor-kind operator
```

Release updates only the pre-existing allowed local Git ref with
compare-and-swap. Vouch does not push or merge a remote pull request in the
current profile. The trust setup is described in the
[production operations guide](PRODUCTION.md).

## 4. Illustrative deployment pattern: networkless migration generation

A schema team can run a deterministic migration generator without any model
authority:

```sh
vouch --repo /srv/repos/orders-api run \
  --socket /run/vouch/vouchd.sock \
  --namespace database \
  --id tx:orders-297 \
  --intent-file tickets/ORDERS-297.md \
  --agent sql-migration-generator
```

Because `--model-provider` is absent, the agent container starts with
`network=none` and no broker variables. It can create a migration and tests in
the detached worktree, but it receives no database credentials and cannot
apply the migration to production. Review, verification and release operate on
the exact generated Git effects.

## 5. Planned multi-system examples

The accounts-payable scenario—create vendor, change bank details, approve an
invoice and transfer money—is the intended multi-system Agent OS direction,
not an implemented connector in this repository. Today Vouch cannot honestly
claim to execute or roll back SAP, Salesforce, cloud or bank operations.

That future flow requires typed connector drivers, scoped downstream
credentials, separation-of-duty policies, outcome reconciliation and
compensation. The current Git Runtime proves transaction primitives intended to
support those future connectors, but the connector work remains on the roadmap.
