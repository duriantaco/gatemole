# Agent Transactions

Vouch Runtime provides the executable transaction path. Its development
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

This is the transaction-ledger path, not yet one fully paired execution
lifecycle. Admission creates the associated `AgentRun`, and launch revalidates
its live authority and atomically pins both ledger heads. The OCI workload
currently advances and settles only the transaction ledger; the run lifecycle
and durable budget usage are not yet advanced with it. A metadata cancellation
before launch can prevent launch, but Vouch does not yet provide a supervisor
that interrupts an already-running production workload.

Vouch can create and publish the prepared commit to an allowed local Git ref.
It does not push to a remote, merge a pull request, deploy software, or execute
a production-database effect. Those connectors remain outside the implemented
profile.

## Walkthrough

Create a strict profile for a digest-pinned image that is already loaded in the
local OCI engine:

```sh
vouch --repo /path/to/service runtime init \
  --agent coding-agent \
  --image registry.example/agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  --source-digest sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  -- /usr/local/bin/agent
```

Initialization writes the shareable agent profile and creates
`.vouch/runtime.json`, a random identity for this local Runtime instance. Vouch
adds ignore rules for the identity, SQLite/WAL state, locks and socket. Commit
`.vouch/agent-profiles.json` and `.vouch/.gitignore`; do not commit
`.vouch/runtime.json`.

Start the local daemon. The convenience command keeps its transaction root
outside the repository:

```sh
vouch --repo /path/to/service daemon
```

Its default is a canonical repository-scoped directory beneath a validated
private per-user runtime or cache directory. If `--transaction-root` is
provided, the CLI resolves it and the daemon validates the complete path,
ownership and permissions before opening the ledger.

In another terminal, diagnose the selected profile and run a task through the
primary Runtime surface:

```sh
vouch --repo /path/to/service doctor --agent coding-agent

vouch --repo /path/to/service run \
  --namespace payments \
  --intent "Upgrade the service without changing authentication behavior" \
  --agent coding-agent
```

For OCI execution, `vouch run` first asks `vouchd` to match the exact local
Runtime ID, report its enforcement profile, check ledger health and inspect the
selected pull-never image. A failed preflight creates neither admission
authority nor a worktree. After a successful preflight, `vouch run` creates and
starts the transaction, creates its isolated worktree, runs the agent, freezes
the exact Git effects, and performs sequence validation. It prints the
generated transaction ID. Inspect it with:

```sh
vouch --repo /path/to/service status <transaction-id> \
  --namespace payments

vouch --repo /path/to/service tx effects \
  --namespace payments --id <transaction-id>

vouch --repo /path/to/service tx events \
  --namespace payments --id <transaction-id>
```

`vouch runtime init` writes `.vouch/agent-profiles.json` using the
[public profile schema](../schemas/gatemole.agent_profiles.v0.schema.json). The
[checked-in fixture](../schemas/fixtures/runtime/valid/agent_profiles.json)
shows the complete shareable document shape. `.vouch/runtime.json` is separate
local control state, not part of that schema.

Every primary `vouch run` persists a strict `gatemole.agent_task.v0` resource. It
binds the transaction, namespace, participating run, exact intent, selected
profile, pinned image and final command. Daemon-owned OCI agents receive the
same envelope in a read-only mount:

```sh
VOUCH_TASK_PATH=/vouch/task.json
VOUCH_TASK_DIGEST=<sha256 digest>
```

The agent should read the task from `VOUCH_TASK_PATH`. Do not put credentials
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
must carry the same ID in the `Vouch-Runtime-ID` header. The product `run`,
transaction, low-level `kernel` and `action` commands load
`.vouch/runtime.json` and use a bound client for every call, including
long-running agent and verifier operations. Health and readiness are the
deliberate unbound endpoints. The header prevents accidental cross-Runtime
wiring; it is not an authentication secret.

## Agent integration contract

An existing coding agent does not need to adopt a Vouch SDK. Its OCI entrypoint
receives:

```text
/workspace                 writable detached Git worktree
/vouch/task.json           read-only admitted AgentTask
VOUCH_TASK_PATH            /vouch/task.json
VOUCH_TASK_DIGEST          digest of that exact task
VOUCH_TRANSACTION_ID       kernel transaction identity
VOUCH_RUN_ID               kernel run identity
VOUCH_RUNTIME_ROLE         agent
```

The process edits `/workspace` and exits. It must not receive the source
repository, the `vouchd` socket, Git hosting credentials or downstream
production credentials. Vouch re-inspects the worktree, freezes the exact
effects and records the daemon-authored process receipt.

Example entrypoint:

```sh
#!/bin/sh
set -eu
test "$VOUCH_RUNTIME_ROLE" = agent
test -r "$VOUCH_TASK_PATH"
cd /workspace
exec /opt/acme-agent --task-file "$VOUCH_TASK_PATH"
```

### No model access

Without `--model-provider`, the agent starts with `network=none`; model broker
variables are absent:

```sh
vouch --repo /path/to/service run \
  --namespace payments \
  --intent "Apply the checked-in deterministic migration" \
  --agent migration-agent
```

### Explicit model access

If the daemon has a pinned broker image and policy for `openai`, request that
provider in task admission:

```sh
vouch --repo /path/to/service run \
  --namespace payments \
  --intent "Fix the failing idempotency test" \
  --agent coding-agent \
  --model-provider openai
```

The agent then receives `OPENAI_BASE_URL` and a transaction-scoped
`OPENAI_API_KEY` that authenticate only to the internal broker. The real
provider credential remains in `vouchd`. The broker enforces the configured
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
vouch --repo /path/to/service tx verify \
  --namespace payments --id <transaction-id> \
  --name tests \
  --image registry.example/verifier@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  -- /usr/local/bin/verify

vouch --repo /path/to/service tx prepare \
  --namespace payments --id <transaction-id> \
  --git-ref refs/heads/agent/upgrade-001

vouch --repo /path/to/service approve <transaction-id> \
  --namespace payments \
  --key /secure/approver.key \
  --key-id key:approver \
  --approver human:alice \
  --class security-reviewer

vouch --repo /path/to/service release <transaction-id> \
  --namespace payments
```

These commands require the hardened daemon identity, verifier, approval and
release configuration described in
[Production Runtime Operations](PRODUCTION.md).
For production task creation, require the profile explicitly so a development
daemon is rejected during preflight:

```sh
vouch --repo /srv/vouch/repository run \
  --socket /run/vouch/vouchd.sock \
  --namespace payments \
  --require-enforcement-profile production \
  --intent "Upgrade the approved payments service" \
  --agent coding-agent
```

The lower-level `vouch tx create` command remains a development-only manual
lifecycle entrypoint, but on a real configured Runtime it creates the same
Runtime/profile-bound v1 task admission rather than raw transaction authority.
The raw `POST /v0/namespaces/{namespace}/transactions` creation route exists
only for embedded, unbound compatibility servers and `vouchd` rejects it.
Existing-transaction operations such as `start|worktree|stage|validate` remain
available for connector development, recovery and debugging. They do not
replace the primary task-oriented path. Abort
discards an unreleased isolated worktree after execution; it is not a live
workload-cancellation command:

```sh
vouch --repo /path/to/service tx abort \
  --namespace payments --id <transaction-id>
```

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
- Symlinks are recorded as link text; Vouch does not follow them to read data
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
- The mediated filesystem API permanently denies `.git` and `.vouch` path
  components, independent of any capability grant.

The development profile is still a local, same-user boundary. An agent with
direct access to the source repository, daemon socket, credentials, or
unrestricted network can bypass it.

The repository-local `.vouch/runtime.lock` rejects two daemons under the same
OS UID for the same repository even if environment, socket or ledger paths
differ; a separate ledger lock prevents one database from being opened by two
daemons. These are not cryptographic same-UID daemon attestation or a
cross-host identity system, so use a dedicated OS account for the hardened
boundary. A normal Git clone receives a new ignored `.vouch/runtime.json`;
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
