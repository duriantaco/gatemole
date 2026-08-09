# Runtime examples

Gatemole's implemented production boundary today is a Git repository on one
trusted node. These examples therefore use software-delivery work rather than
pretending that the current repository can already control SAP, Salesforce or
bank transfers.

## Real-life use case: govern an AI hotfix

### The pain point

It is 02:00 and incident `PAY-1842` is logging valid customers out of a payments
API. A refresh-token regression is already captured in
`tickets/PAY-1842.md`. The team has a real coding agent, called
`payments-coder`, that can inspect the repository, call a model, edit the Go
service and run tests.

A common direct invocation is:

```sh
# Use a disposable clone when collecting a baseline. Do not experiment in the
# production Runtime repository.
cd /tmp/payments-api-baseline
payments-coder --task-file tickets/PAY-1842.md
go test ./internal/auth
git diff
```

The agent may diagnose and fix the bug correctly. That is not the control
problem Gatemole addresses. In this setup, the agent runs inside the same
boundary as the checkout and whatever credentials and network its launcher
inherited. The team normally relies on the agent transcript, the resulting
working tree and surrounding conventions to answer:

- Did the process touch only the two files it reported?
- Did the successful test run against the exact tree being reviewed?
- Can this process or one of its tools publish the branch itself?
- What partial state remains if the agent or launcher crashes?
- Is a human approval still valid if any byte changes afterward?

An agent product may already implement some sandboxing or review controls.
Gatemole's difference is that these decisions are owned by a separate local
kernel and are bound together as one durable transaction, regardless of which
agent performs the work.

### Run the same real agent through Gatemole

`payments-coder` below is the team's actual agent adapter, not the deterministic
test fixture later in this guide. The adapter reads the retained task envelope
from `$GATEMOLE_TASK_PATH`, invokes the normal agent loop and lets it edit
`/workspace`. The platform team packages that command in a digest-pinned OCI
image and registers it once:

```sh
gatemole --repo /srv/repos/payments-api runtime init \
  --agent payments-coder \
  --image registry.acme.example/agents/payments-coder@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  --source-digest sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  -- /usr/local/bin/payments-coder
```

The production daemon must also be configured with the `openai` model-broker
policy, the `auth-tests` verifier profile, OIDC trust, approval trust and an
allowlist containing the release ref. Those are one-time platform controls;
the exact hardened setup is in [Production Runtime Operations](PRODUCTION.md).

Assume the allowed release ref already exists at the current base revision and
is not checked out:

```sh
git -C /srv/repos/payments-api \
  branch agent-release/pay-1842 HEAD
```

The incident operator admits the exact ticket to one run and transaction:

```sh
export GATEMOLE_IDENTITY_TOKEN="$PAYMENTS_OPERATOR_TOKEN"

gatemole --repo /srv/repos/payments-api run \
  --socket /run/gatemole/gatemoled.sock \
  --namespace payments \
  --id tx:pay-1842 \
  --run run:pay-1842 \
  --require-enforcement-profile production \
  --intent-file tickets/PAY-1842.md \
  --agent payments-coder \
  --model-provider openai
```

`gatemoled`, rather than the agent launcher, now creates the task authority and
private worktree and starts the pinned container. The agent receives only the
task-specific broker token; the provider key stays in `gatemoled`. The source
checkout, daemon socket, Git release credential and production service
credentials are absent from the container.

When the agent exits, success means only that the worker completed. It does not
mean that the change is approved or released. The operator inspects the
daemon-owned transaction, evidence identifiers, exact normalized effects and
digest-bound frozen patch:

```sh
gatemole --repo /srv/repos/payments-api review tx:pay-1842 \
  --socket /run/gatemole/gatemoled.sock \
  --namespace payments

gatemole --repo /srv/repos/payments-api diff tx:pay-1842 \
  --socket /run/gatemole/gatemoled.sock \
  --namespace payments

# Advanced event inspection remains available:
gatemole --repo /srv/repos/payments-api tx events \
  --socket /run/gatemole/gatemoled.sock \
  --namespace payments \
  --id tx:pay-1842

# The agent did not edit the source checkout.
git -C /srv/repos/payments-api diff --exit-code
```

The team then runs an independently configured verifier against the immutable
staged tree—not against a path selected by the coding agent—and prepares the
exact release operation:

```sh
VERIFIER_IMAGE="$(cat /etc/gatemole/images/go-verifier.ref)"

gatemole --repo /srv/repos/payments-api tx verify \
  --socket /run/gatemole/gatemoled.sock \
  --namespace payments \
  --id tx:pay-1842 \
  --name auth-tests \
  --image "$VERIFIER_IMAGE" \
  -- /usr/local/bin/run-auth-tests

gatemole --repo /srv/repos/payments-api tx prepare \
  --socket /run/gatemole/gatemoled.sock \
  --namespace payments \
  --id tx:pay-1842 \
  --git-ref refs/heads/agent-release/pay-1842
```

Authentication code and its test changing together require a focused security
approval under the baseline sequence policy. A reviewer signs the exact frozen
approval package with a separate short-lived identity:

```sh
export GATEMOLE_IDENTITY_TOKEN="$PAYMENTS_REVIEWER_TOKEN"

gatemole --repo /srv/repos/payments-api approve tx:pay-1842 \
  --socket /run/gatemole/gatemoled.sock \
  --namespace payments \
  --key /secure/payments-reviewer.key \
  --key-id key:payments-reviewer \
  --approver human:alice \
  --issuer https://login.acme.example/ \
  --class security-reviewer
```

A different identity performs the release:

```sh
export GATEMOLE_IDENTITY_TOKEN="$PAYMENTS_RELEASER_TOKEN"

gatemole --repo /srv/repos/payments-api apply tx:pay-1842 \
  --socket /run/gatemole/gatemoled.sock \
  --namespace payments \
  --actor operator:payments-release \
  --actor-kind operator
```

`apply` uses the existing release authority: it compares the prepared base
revision and atomically updates only `refs/heads/agent-release/pay-1842`. A
changed base, changed candidate, missing approval or wrong identity fails
closed. The coding agent cannot turn its own successful process exit into a
release.

### What changed in the before/after comparison

| Question | Direct invocation | Through Gatemole |
| --- | --- | --- |
| Which agent solved the bug? | `payments-coder` | The same `payments-coder` image and command |
| Where could it write? | The checkout exposed by its launcher | A transaction-private worktree; the source checkout remains unchanged |
| How could it call the model? | Whatever provider credential and network the launcher supplied | Only the admitted broker; the provider credential remains outside the agent |
| Who says what happened? | Primarily the agent/runner transcript and resulting checkout | `gatemoled` writes paired execution receipts, normalized effects and hash-chained events |
| Who checks the fix? | Commonly the agent itself plus later CI | A separately pinned verifier checks the exact frozen tree before approval |
| Who can approve and release? | Depends on surrounding convention and available Git credentials | A trusted reviewer signs the exact package; a different releaser updates the allowed ref |
| What happens on retry? | Depends on the runner and workspace | Identical admission resumes the durable transaction; failed/interrupted receipts remain |

That is Gatemole's current practical value. It does not make
`payments-coder` smarter, prove the fix semantically correct or replace CI. It
makes the agent's authority narrower and makes the path from intent to an exact
Git effect independently inspectable and enforceable.

The current release target is deliberately limited: Gatemole updates an
allowed **local Git ref**. It does not yet push or merge a pull request, deploy
the payments service, mutate a database or operate Kubernetes. Remote effects
need the planned connector and Control Plane layers.

## 1. Optional: run the repository production-acceptance fixture

The fastest end-to-end local-Git example is the same acceptance flow used
before release. It requires Git, Go, Docker, `jq`, and `curl`:

```sh
go test ./...

AGENT_IMAGE="$(
  scripts/gatemoleproductionfixture.sh \
    --tag gatemole-production-fixture:example
)"
GATEMOLE_PRODUCTION_IMAGE="$AGENT_IMAGE" \
  scripts/gatemoleproductionbench.sh
```

It creates a temporary authentication repository, production OIDC identities,
an operator/releaser plus a logically independent signed reviewer, a policy-controlled model
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

## 2. Self-contained recovery proof: PAY-1842 agent crash

This is a deterministic acceptance fixture for the recovery claims above, not
the primary user workflow and not a substitute for evaluating a real agent.
It starts with a payments API that rejects valid refresh tokens and has a
failing regression test. Its fixture agent saves the fix but crashes before
verification. After `gatemoled` is restarted, the same admitted task resumes
the existing private worktree, runs the real Go test, and succeeds:

```sh
scripts/gatemolepairedexecutiondemo.sh
```

Run it as a non-root user with Git, Go, Docker, and `jq` installed. Docker may
fetch `golang:1.26-alpine` on the first run. The script builds Gatemole from the
current checkout and creates all service, image, Runtime, ledger, and worktree
state under a temporary directory; its cleanup does not touch the source
checkout.

The scenario checks each important boundary instead of merely printing a happy
path:

1. The first agent edits `internal/auth/refresh.go` only inside the detached
   transaction worktree, then exits 42 before running tests.
2. The transaction records a failed execution and the paired run settles to
   `waiting_for_agent`, with its active-execution binding cleared.
3. The daemon is stopped and started against the same SQLite ledger and
   transaction root.
4. Exact admission replay starts a second immutable execution receipt in the
   same transaction. The agent finds the saved patch and runs
   `go test ./internal/auth` inside the OCI boundary.
5. Success settles the run to `waiting_for_event`; all four paired execution
   events remain queryable and wall time from both attempts is cumulative.
6. `git diff --exit-code` proves the developer's checkout remains unchanged.

The execution IDs and timing vary, but the report has this shape:

```json
{
  "incident": "PAY-1842 refresh-token rejection",
  "first_attempt": "failed",
  "state_after_failure": "waiting_for_agent",
  "daemon_restarted": true,
  "retry": "succeeded",
  "regression_test": "go test ./internal/auth (passed on retry)",
  "final_run_state": "waiting_for_event",
  "active_execution_id": null,
  "cumulative_budget_usage": {
    "wall_time_seconds": 15
  },
  "paired_execution_events": [
    {"type": "run.execution_started"},
    {"type": "run.execution_finished", "status": "failed"},
    {"type": "run.execution_started"},
    {"type": "run.execution_finished", "status": "succeeded"}
  ],
  "source_checkout_unchanged": true
}
```

The exact setup is executable source in
[`scripts/gatemolepairedexecutiondemo.sh`](../scripts/gatemolepairedexecutiondemo.sh).
The following single-attempt walkthrough unpacks the same integration contract
for readers adapting their own agent image.

### Manual single-attempt walkthrough

This version creates a small payments-service repository and a digest-pinned
agent image. The agent changes authentication code and its test inside a
Gatemole transaction. The source checkout remains unchanged.

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
`$GATEMOLE_TASK_PATH`, edit `/workspace`, and exit.

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
	if _, err := os.ReadFile(os.Getenv("GATEMOLE_TASK_PATH")); err != nil {
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
  --tag gatemole-example-auth-agent:local \
  "$AGENT_SRC"
AGENT_IMAGE="$(
  docker image inspect \
    --format '{{.Id}}' \
    gatemole-example-auth-agent:local
)"
```

`AGENT_IMAGE` is the immutable local `sha256:...` image ID. Production
deployments normally use a registry reference of the form
`registry.example/agent@sha256:...`.

### Start the Runtime and run the task

```sh
go install ./cmd/gatemole ./cmd/gatemoled

# This fixture uses an ad-hoc local image ID below, so initialize only the
# repository-local Runtime identity. Named production profiles are registered
# with the full `gatemole runtime init --agent ...` form shown in the README.
gatemole --repo "$REPO" runtime init

RUNTIME="$EXAMPLE_ROOT/runtime"
SOCKET="$RUNTIME/gatemoled.sock"
mkdir -p "$RUNTIME"

gatemoled \
  --repo "$REPO" \
  --db "$RUNTIME/kernel.db" \
  --socket "$SOCKET" \
  --transaction-root "$RUNTIME/transactions" \
  >"$RUNTIME/gatemoled.log" 2>&1 &
DAEMON_PID=$!

while [ ! -S "$SOCKET" ]; do
  sleep 0.1
done

gatemole --repo "$REPO" --json run \
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
gatemole --repo "$REPO" review tx:pay-1842 \
  --socket "$SOCKET" \
  --namespace payments

gatemole --repo "$REPO" diff tx:pay-1842 \
  --socket "$SOCKET" \
  --namespace payments

gatemole --repo "$REPO" tx events \
  --socket "$SOCKET" \
  --namespace payments \
  --id tx:pay-1842

gatemole --repo "$REPO" --json kernel run get \
  --socket "$SOCKET" \
  --namespace payments \
  --id run:pay-1842 \
  | jq '.run | {state, active_execution_id, budget_usage}'

gatemole --repo "$REPO" --json kernel run events \
  --socket "$SOCKET" \
  --namespace payments \
  --id run:pay-1842 \
  | jq '[.[]
      | select(.type | startswith("run.execution_"))
      | {type, execution_id: .payload.execution_id,
         status: .payload.status, usage: .payload.usage}]'

# The agent never edited the developer's source checkout.
git -C "$REPO" diff --exit-code
```

Because authentication code and its test changed together, the baseline
sequence policy routes this transaction to focused approval. It does not
silently publish the change. The paired run is `waiting_for_event`, has no
active execution, and contains one start/finish pair whose finish usage was
added to the run budget. Stop the example daemon with:

```sh
kill "$DAEMON_PID"
```

## 3. Illustrative deployment pattern: networkless migration generation

A schema team can run a deterministic migration generator without any model
authority:

```sh
gatemole --repo /srv/repos/orders-api run \
  --socket /run/gatemole/gatemoled.sock \
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

## 4. Planned multi-system examples

The accounts-payable scenario—create vendor, change bank details, approve an
invoice and transfer money—is the intended multi-system Agent OS direction,
not an implemented connector in this repository. Today Gatemole cannot honestly
claim to execute or roll back SAP, Salesforce, cloud or bank operations.

That future flow requires typed connector drivers, scoped downstream
credentials, separation-of-duty policies, outcome reconciliation and
compensation. The current Git Runtime proves transaction primitives intended to
support those future connectors, but the connector work remains on the roadmap.
