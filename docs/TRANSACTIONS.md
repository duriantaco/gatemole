# Agent Transactions

Vouch Runtime provides the executable transaction path. Its development
profile supports local exploration; its supported enforcement profile is
deliberately limited to a single-node, single-tenant local-Git runtime.

The complete implemented lifecycle is:

```text
human intent + selected agent profile
  -> durable, digest-bound AgentTask
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

Vouch can create and publish the prepared commit to an allowed local Git ref.
It does not push to a remote, merge a pull request, deploy software, or execute
a production-database effect. Those connectors remain outside the implemented
profile.

## Walkthrough

Start the local daemon. Its transaction root must be outside the repository:

```sh
vouch --repo /path/to/service daemon \
  --transaction-root /tmp/vouch-service-transactions
```

Run a task through the primary runtime surface:

```sh
vouch --repo /path/to/service run \
  --namespace payments \
  --intent "Upgrade the service without changing authentication behavior" \
  --runtime oci \
  --image registry.example/agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  -- /usr/local/bin/agent
```

`vouch run` creates and starts the transaction, creates its isolated worktree,
runs the agent, freezes the exact Git effects, and performs sequence
validation. It prints the generated transaction ID. Inspect it with:

```sh
vouch --repo /path/to/service status <transaction-id> \
  --namespace payments

vouch --repo /path/to/service tx effects \
  --namespace payments --id <transaction-id>

vouch --repo /path/to/service tx events \
  --namespace payments --id <transaction-id>
```

For repeatable agent integrations, define `.vouch/agent-profiles.json` using
the [public profile schema](../schemas/vouch.agent_profiles.v0.schema.json) and
run with `--agent NAME` instead of supplying a raw image and command. The
[checked-in fixture](../schemas/fixtures/runtime/valid/agent_profiles.json)
shows the complete document shape.

Every primary `vouch run` persists a strict `vouch.agent_task.v0` resource. It
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
provider, changing the admitted image or command, launching after expiry, or
racing a run cancellation fails before any broker or agent workload starts.

The [real-world examples guide](EXAMPLES.md) contains a complete runnable
authentication-hotfix scenario plus production coding-agent and networkless
migration workflows.

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

The lower-level `vouch tx create` command remains available only in the
development profile. Existing-transaction operations such as
`start|worktree|stage|validate` remain available for connector development,
recovery and debugging. They do not replace the primary task-oriented path,
and production transaction creation requires atomic task admission. Abort
discards an unreleased isolated worktree:

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

The production profile adds daemon-owned OCI execution, pinned images, OIDC
identity, signed independent approvals, a separate releaser, ledger/profile
binding, and local Git-ref commit reconciliation. Identity trust, approval
trust, enforcement profile, verifier profiles, and verifier runtime inputs are
bound into prepared authority; changing any of them requires preparation and
approval again. Legacy client-supervised agent/verification mutation APIs are
disabled in production.

Each verifier receives a new read-only directory materialized by Git object ID
from the exact frozen tree, outside both the source repository and mutable
agent worktree. It does not consume the agent bind mount. The dedicated
same-UID host, Git executable/object database, and config-parent paths remain
trusted.

Operating-system byte and inode quotas must cover the source repository and Git
object database, SQLite/WAL, transaction worktrees, immutable verifier
materializations, and evidence. See the production guide for the complete
deployment boundary and limits.
