# Agent Transactions

Gatemole Runtime provides the executable transaction path. Its development
profile supports local exploration; its supported enforcement profile is
deliberately limited to a single-node, single-tenant local-Git runtime.

The implemented local-Git transaction path is:

```text
human intent + selected agent profile
  -> Runtime ID, enforcement-profile and image preflight
  -> durable, digest-bound AgentTask
  -> Runtime/profile-bound task admission
  -> durable AgentTransaction
  -> detached Git worktree outside the source repository
  -> daemon-owned agent execution with a read-only task envelope
  -> exact ordered file-effect ledger
  -> frozen immutable Git tree, worktree-state, and effect-set digests
  -> deterministic sequence policy
  -> daemon-owned OCI verification
  -> frozen commit plan and authority package
  -> independent signed approval
  -> release by a separate identity
  -> compare-and-swap update of an allowed local Git ref
```

Admission creates the associated `AgentRun`, and launch revalidates its live
authority before atomically starting the same execution in the run and
transaction ledgers. Settlement atomically finishes both sides: success leaves
the run `waiting_for_event`, while failed, interrupted and start-failed work
leaves it `waiting_for_agent` for an explicit retry. Elapsed wall time and
verified model-call/token usage are charged cumulatively to the run, including
restart recovery, with each charge bounded by the remaining hard limit. The
transaction receipt retains the raw timestamps and counters when a charge
saturates. A metadata cancellation before launch can prevent launch, but
Gatemole does not yet provide a supervisor that interrupts an
already-running production workload. Tool/cost accounting and mediated remote
actions also remain outside this local-Git profile.

Gatemole can create and publish the prepared commit to an allowed local Git ref.
It does not push to a remote, merge a pull request, deploy software, or execute
a production-database effect. Those connectors remain outside the implemented
profile.

## Walkthrough

Create a strict profile for a digest-pinned image that is already loaded in the
local OCI engine:

```sh
gatemole --repo /path/to/service runtime init \
  --agent coding-agent \
  --image registry.example/agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  --source-digest sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  -- /usr/local/bin/agent
```

Initialization writes the shareable agent profile and creates
`.gatemole/runtime.json`, a random identity for this local Runtime instance. Gatemole
adds ignore rules for the identity, SQLite/WAL state, locks and socket. Commit
`.gatemole/agent-profiles.json` and `.gatemole/.gitignore`; do not commit
`.gatemole/runtime.json`.

Start the local daemon. The convenience command keeps its transaction root
outside the repository:

```sh
gatemole --repo /path/to/service daemon
```

Its default is a canonical repository-scoped directory beneath a validated
private per-user runtime or cache directory. If `--transaction-root` is
provided, the CLI resolves it and the daemon validates the complete path,
ownership and permissions before opening the ledger.

In another terminal, diagnose the selected profile and run a task through the
primary Runtime surface:

```sh
gatemole --repo /path/to/service doctor --agent coding-agent

gatemole --repo /path/to/service run \
  --namespace payments \
  --intent "Upgrade the service without changing authentication behavior" \
  --agent coding-agent
```

For OCI execution, `gatemole run` first asks `gatemoled` to match the exact local
Runtime ID, report its enforcement profile, check ledger health and inspect the
selected pull-never image. A failed preflight creates neither admission
authority nor a worktree. After a successful preflight, `gatemole run` creates and
starts the transaction, creates its isolated worktree, runs the agent, freezes
the exact Git effects, and performs sequence validation. It prints the
generated transaction ID. Inspect its readable status, evidence identifiers and
exact frozen patch with:

```sh
gatemole --repo /path/to/service review <transaction-id> \
  --namespace payments

gatemole --repo /path/to/service diff <transaction-id> \
  --namespace payments

# Advanced lifecycle/debugging views:
gatemole --repo /path/to/service status <transaction-id> \
  --namespace payments

gatemole --repo /path/to/service tx effects \
  --namespace payments --id <transaction-id>

gatemole --repo /path/to/service tx events \
  --namespace payments --id <transaction-id>
```

The diff route re-inspects the private worktree, reconstructs the immutable Git
tree and refuses output unless both the staged-state and normalized effect-set
digests still match the authoritative transaction. The response binds the
namespace, transaction, attempt, event sequence, base revision, tree revision
and patch digest. The CLI validates that metadata and the raw patch body before
printing it. `gatemole --json diff ...` exposes the body as `patch_base64` with
the same metadata.

The [PAY-1842 operator example](EXAMPLES.md#real-life-use-case-govern-an-ai-hotfix)
shows why a team would put its real coding agent behind this lifecycle. The
checked-in script below is the separate, deterministic acceptance proof for a
failed attempt, daemon restart, resumed patch and passing Go regression test:

```sh
scripts/gatemolepairedexecutiondemo.sh
```

It uses stable IDs `tx:pay-1842` and `run:pay-1842`. On a long-lived Runtime,
the equivalent run-side inspection is:

```sh
gatemole --repo /path/to/payments-api --json kernel run get \
  --socket /path/to/gatemoled.sock \
  --namespace payments --id run:pay-1842 \
  | jq '.run | {
      state,
      active_execution_id: (.active_execution_id // null),
      budget_usage
    }'

gatemole --repo /path/to/payments-api --json kernel run events \
  --socket /path/to/gatemoled.sock \
  --namespace payments --id run:pay-1842 \
  | jq '[.[]
      | select(.type | startswith("run.execution_"))
      | {sequence, type, execution_id: .payload.execution_id,
         status: .payload.status, usage: .payload.usage}]'

gatemole --repo /path/to/payments-api --json tx get \
  --socket /path/to/gatemoled.sock \
  --namespace payments --id tx:pay-1842 \
  | jq '[.executions[] | {id, status, started_at, completed_at}]'
```

The proof produces four run events: start/failed for the crashed attempt, then
start/succeeded for the retry. The final projection is `waiting_for_event` with
no `active_execution_id`; both transaction receipts remain immutable and
`budget_usage.wall_time_seconds` is their cumulative charge. On daemon-crash
recovery, a running attempt instead settles as `interrupted` and the run becomes
`waiting_for_agent`. Model calls left in flight at restart are finalized as
`unknown` before their conservative token charge is applied. The complete
scenario and representative output are in [Runtime examples](EXAMPLES.md#2-self-contained-recovery-proof-pay-1842-agent-crash).

`gatemole runtime init` writes `.gatemole/agent-profiles.json` using the
[public profile schema](../schemas/gatemole.agent_profiles.v0.schema.json). The
[checked-in fixture](../schemas/fixtures/runtime/valid/agent_profiles.json)
shows the complete shareable document shape. `.gatemole/runtime.json` is separate
local control state, not part of that schema.

Every primary `gatemole run` persists a strict `gatemole.agent_task.v0` resource. It
binds the transaction, namespace, participating run, exact intent, selected
profile, pinned image and final command. Daemon-owned OCI agents receive the
same envelope in a read-only mount:

```sh
GATEMOLE_TASK_PATH=/gatemole/task.json
GATEMOLE_TASK_DIGEST=<sha256 digest>
```

The agent should read the task from `GATEMOLE_TASK_PATH`. Do not put credentials
or other secrets in task intent; the envelope is intentionally retained in the
durable transaction history.

The current task-admission request and result use the v1 wire contract.
Admission binds the Runtime ID and exact `development` or `production`
enforcement profile into the transaction admission record and checks both
against the SQLite ledger. A configured Runtime never creates new v0
admissions. A nonempty legacy ledger adopted in development may replay an
already-persisted v0 admission with the exact same idempotency input; that
compatibility path cannot create new v0 authority and is not accepted as a
production migration strategy. A replayed v0 admission is readable history
only. A configured Runtime rejects raw run/transaction creation, legacy
capability compilation, run actions and events, and every transaction mutation
unless they resolve to current v1 authority.

Preflight and admission v1 carry the expected Runtime ID in validated request
bodies. Every later lifecycle mutation and read against a configured daemon
must carry the same ID in the `Gatemole-Runtime-ID` header. The product `run`,
transaction, low-level `kernel` and `action` commands load
`.gatemole/runtime.json` and use a bound client for every call, including
long-running agent and verifier operations. Health and readiness are the
deliberate unbound endpoints. The header prevents accidental cross-Runtime
wiring; it is not an authentication secret.

## Agent integration contract

An existing coding agent does not need to adopt a Gatemole SDK. Its OCI entrypoint
receives:

```text
/workspace                 writable detached Git worktree
/gatemole/task.json           read-only admitted AgentTask
GATEMOLE_TASK_PATH            /gatemole/task.json
GATEMOLE_TASK_DIGEST          digest of that exact task
GATEMOLE_TRANSACTION_ID       kernel transaction identity
GATEMOLE_RUN_ID               kernel run identity
GATEMOLE_RUNTIME_ROLE         agent
```

The process edits `/workspace` and exits. It must not receive the source
repository, the `gatemoled` socket, Git hosting credentials or downstream
production credentials. Gatemole re-inspects the worktree, freezes the exact
effects and records the daemon-authored process receipt.

Example entrypoint:

```sh
#!/bin/sh
set -eu
test "$GATEMOLE_RUNTIME_ROLE" = agent
test -r "$GATEMOLE_TASK_PATH"
cd /workspace
exec /opt/acme-agent --task-file "$GATEMOLE_TASK_PATH"
```

### No model access

Without `--model-provider`, the agent starts with `network=none`; model broker
variables are absent:

```sh
gatemole --repo /path/to/service run \
  --namespace payments \
  --intent "Apply the checked-in deterministic migration" \
  --agent migration-agent
```

### Explicit model access

If the daemon has a pinned broker image and policy for `openai`, request that
provider in task admission:

```sh
gatemole --repo /path/to/service run \
  --namespace payments \
  --intent "Fix the failing idempotency test" \
  --agent coding-agent \
  --model-provider openai
```

The agent then receives `OPENAI_BASE_URL` and a transaction-scoped
`OPENAI_API_KEY` that authenticate only to the internal broker. The real
provider credential remains in `gatemoled`. The broker enforces the configured
origin, model allowlist and request/token ceilings and writes a hash-chained
receipt ledger.

```python
import json
import os
import urllib.request

request = urllib.request.Request(
    os.environ["OPENAI_BASE_URL"] + "/responses",
    data=json.dumps({
        "model": "policy-allowed-model",
        "input": "Review the current worktree change.",
        "store": False,
    }).encode(),
    headers={
        "Authorization": "Bearer " + os.environ["OPENAI_API_KEY"],
        "Content-Type": "application/json",
    },
)
with urllib.request.urlopen(request, timeout=30) as response:
    result = json.load(response)
```

The model name must also be allowed by daemon policy. Selecting an unconfigured
provider, changing the admitted image or command, launching after expiry, or a
terminal run transition that wins the atomic launch claim fails before any
broker or agent workload starts.

The [Runtime examples guide](EXAMPLES.md) contains a runnable deterministic
authentication-hotfix fixture plus illustrative coding-agent and networkless
migration deployment patterns.

Verification, preparation and authority remain explicit operations:

```sh
gatemole --repo /path/to/service tx verify \
  --namespace payments --id <transaction-id> \
  --name tests \
  --image registry.example/verifier@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  -- /usr/local/bin/verify

gatemole --repo /path/to/service tx prepare \
  --namespace payments --id <transaction-id> \
  --git-ref refs/heads/agent/upgrade-001

gatemole --repo /path/to/service approve <transaction-id> \
  --namespace payments \
  --key /secure/approver.key \
  --key-id key:approver \
  --approver human:alice \
  --class security-reviewer

gatemole --repo /path/to/service apply <transaction-id> \
  --namespace payments
```

`apply` invokes the same release operation as `release`; it still requires the
prepared commit plan, exact signed approval package and distinct release
authority.

These commands require the hardened daemon identity, verifier, approval and
release configuration described in
[Production Runtime Operations](PRODUCTION.md).
For production task creation, require the profile explicitly so a development
daemon is rejected during preflight:

```sh
gatemole --repo /srv/gatemole/repository run \
  --socket /run/gatemole/gatemoled.sock \
  --namespace payments \
  --require-enforcement-profile production \
  --intent "Upgrade the approved payments service" \
  --agent coding-agent
```

The lower-level `gatemole tx create` command remains a development-only manual
lifecycle entrypoint, but on a real configured Runtime it creates the same
Runtime/profile-bound v1 task admission rather than raw transaction authority.
The raw `POST /v0/namespaces/{namespace}/transactions` creation route exists
only for embedded, unbound compatibility servers and `gatemoled` rejects it.
Existing-transaction operations such as `start|worktree|stage|validate` remain
available for connector development, recovery and debugging. They do not
replace the primary task-oriented path. The task-oriented `reject` command is
an alias over `tx abort`: it discards an unreleased isolated worktree after
execution; it is not a live
workload-cancellation command:

```sh
gatemole --repo /path/to/service reject <transaction-id> \
  --namespace payments
```

## Attempts and recovery

Current task admissions begin at attempt `1`. A terminal failed, interrupted,
or start-failed agent execution may be followed by another execution in the
same attempt. Only the latest successful execution can contribute staged
effects; every task-bound effect records that execution ID. A failed process
receipt therefore cannot be used to freeze partial workspace changes.

Repeating `gatemole run` with the same transaction ID and identical admission
input resumes from the durable transaction head. It reuses the isolated
worktree, retries a failed execution, or continues staging and sequence
validation after a successful execution. A successful execution whose final
Git tree equals the base revision ends in the terminal
`completed_no_effect` state.

Starting a transaction again from `staged`, `validation_failed`, or
`revise_required` begins the next numbered attempt. Active effects,
verification results, and release authority are removed from the releasable
projection, but remain queryable under `superseded_attempts`; execution
receipts and the append-only event history are never rewritten.

A failed, indeterminate, or expired verifier can be run again with the same
name. The old result moves to `superseded_verifications`, the frozen effect and
staged-state digests stay unchanged, and only the replacement result is
eligible for authority preparation.

If an approval package or its evidence expires before release, revoke it and
return the same frozen stage to validation:

```sh
gatemole --repo /path/to/service tx renew \
  --namespace payments --id <transaction-id>
```

Renewal preserves current verification results, supersedes expired ones, and
archives the old commit plan and approval package. Rerun any expired verifier,
then call `tx prepare` again to mint fresh authority. These retries apply only
before external release; unknown or non-idempotent connector outcomes still
require reconciliation and are never blindly retried.

Sequence validation and every verifier re-inspect the frozen state. Mutation
after staging returns `TRANSACTION_CONFLICT` and leaves authority unchanged.

## Current sequence checks

The baseline deterministic policy detects:

- A protected authentication/security/permission control changed with its test
  or fixture in the same transaction.
- Authority granted and then consumed by the same transaction.
- Irreversible effects that must be approved before release.
- PostgreSQL/database deletes above the configured row threshold.

The first rule requests focused approval; the self-grant rule blocks. These are
the initial enforcement shape, not a complete software-security policy pack.
Deep connectors will replace path/name heuristics with typed semantic facts.

## Security and limitations

- Worktrees are registered to the source repository and pinned to their base
  revision.
- Paths are NUL-delimited from Git, normalized, sorted, and validated.
- Symlinks are recorded as link text; Gatemole does not follow them to read data
  outside the worktree.
- Staging writes the agent view through a private Git index to an immutable
  tree object, then derives effects and digests from that tree rather than a
  mutable filesystem walk.
- Whole-tree scanning and verifier materialization are bounded to 100,000
  entries, 256 MiB per blob, and 1 GiB aggregate. Gitlinks/submodules are
  rejected because they are not a fully pinned file tree.
- Effect staging and the final `transaction.staged` transition are one SQLite
  transaction, so a crash cannot expose a half-recorded ledger.
- The API does not accept arbitrary client-authored effect events; the daemon
  re-inspects Git and authors authoritative effects.
- The mediated filesystem API permanently denies `.git` and `.gatemole` path
  components, independent of any capability grant.

The development profile is still a local, same-user boundary. An agent with
direct access to the source repository, daemon socket, credentials, or
unrestricted network can bypass it.

The repository-local `.gatemole/runtime.lock` rejects two daemons under the same
OS UID for the same repository even if environment, socket or ledger paths
differ; a separate ledger lock prevents one database from being opened by two
daemons. These are not cryptographic same-UID daemon attestation or a
cross-host identity system, so use a dedicated OS account for the hardened
boundary. A normal Git clone receives a new ignored `.gatemole/runtime.json`;
deliberately copying the complete ignored identity and ledger state to another
repository deliberately clones the trust target. Fleet enrollment,
attestation and revocation remain Control Plane work.

The same-UID socket boundary also means every connected operator, reviewer and
releaser CLI runs under the daemon OS account. OIDC tokens preserve logical
identity and separation of duties. The current approval command does not yet
offer a separate offline sign-and-submit path, so this profile cannot claim
filesystem-level reviewer-key isolation from that account.

The production profile adds daemon-owned OCI execution, pinned images, OIDC
identity, logically independent signed approvals, a separate releaser, ledger/profile
binding, and local Git-ref commit reconciliation. Identity trust, approval
trust, enforcement profile, verifier profiles, and verifier runtime inputs are
bound into prepared authority; changing any of them requires preparation and
approval again. Legacy client-supervised agent/verification mutation APIs are
disabled in production.

At startup, trust documents, verifier profiles and model-broker policy are
captured through bounded, no-follow, inode-stable reads. The daemon parses,
digests and later enforces those exact retained bytes; replacing a source path
after startup does not rotate live authority.

Each verifier receives a new read-only directory materialized by Git object ID
from the exact frozen tree, outside both the source repository and mutable
agent worktree. It does not consume the agent bind mount. The dedicated
host, daemon OS account, same-UID container boundary, Git executable/object
database, and config-parent paths remain trusted.

Operating-system byte and inode quotas must cover the source repository and Git
object database, SQLite/WAL, transaction worktrees, immutable verifier
materializations, and evidence. See the production guide for the complete
deployment boundary and limits.
