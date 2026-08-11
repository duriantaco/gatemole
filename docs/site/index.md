# Gatemole

Gatemole is an enforcement kernel and transaction Runtime for autonomous software
agents. Its customer-side Runtime controls the boundary between an agent's
proposal and an exact effect:

```text
intent
  -> isolated agent execution
  -> normalized effects
  -> immutable staged state
  -> policy and independent verification
  -> approval when required
  -> commit | block | revise | recover
```

Gatemole does not decide how an agent reasons or writes code. The trusted local
kernel, `gatemoled`, orchestrates sandboxed execution and owns authoritative
transaction state, verification, policy decisions and commit coordination.

## Why a team would use it

Suppose incident `PAY-1842` causes a payments API to reject valid refresh
tokens. The team already has a real coding agent, `payments-coder`, that can
diagnose the bug, edit the Go service and run its authentication tests.

Invoked directly in a checkout, that agent works inside the same boundary as
the files, inherited network and credentials:

```sh
cd /srv/repos/payments-api
payments-coder --task-file tickets/PAY-1842.md
```

The fix may be correct. The unresolved production questions are who
independently records its exact effects, who verifies the exact candidate, who
can approve it, who can release it and what survives a crash.

Gatemole runs that **same agent** behind a daemon-owned transaction boundary:

```sh
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

| Direct agent invocation | Same agent through Gatemole |
| --- | --- |
| Checkout or agent-specific sandbox | Daemon-created private Git worktree |
| Provider access supplied to the worker | Transaction broker token; provider credential remains in `gatemoled` |
| Transcript and later diff | Normalized effects, paired execution receipts and hash-chained events |
| Worker commonly runs its own tests | Separately pinned verifier checks the exact frozen tree |
| Approval and Git release are surrounding conventions | Signed exact approval package and a different release identity |
| Recovery depends on the runner | Identical admission resumes durable state and retains failed/interrupted receipts |

Some agent products provide their own sandbox or review UI. Gatemole's specific
offer is that authority and evidence are owned by a separate, agent-independent
kernel. It does not make the agent smarter and does not replace CI.

The implemented release operation updates a pre-existing allowed local Git
ref; remote pull requests and deployments are not implemented. Read the
[complete PAY-1842 before/after and operator workflow](https://github.com/duriantaco/gatemole/blob/main/docs/EXAMPLES.md#real-life-use-case-govern-an-ai-hotfix).

## Product hierarchy

- **Gatemole Developer Runtime** is the local product experience: run an existing
  coding agent in an isolated Git transaction, inspect its effects and control
  release. A low-level, manually operated local-Git integration exists today;
  the self-serve developer preview is still a milestone.
- **Gatemole Agent OS** is the planned enterprise product experience and complete
  target architecture: the Control Plane, Runtime fleet, transaction protocol
  and connector model. It is an umbrella, not another process.
- **Gatemole Control Plane** is the planned commercial management layer for
  Runtime fleets, organization policy, approvals, audit and incident response.
- **Gatemole Runtime** is the deployable customer-side enforcement boundary. A
  narrow single-node local-Git profile is implemented today.
- **`gatemoled`** is the trusted transaction kernel inside each Runtime.
- **Gatemole Contracts** is an optional verification module that compiles release
  intent into obligations and maps evidence to them.

The Developer Runtime and enterprise **Gatemole Agent OS** experience use the same
`gatemoled` kernel; they are not separate engines.

## Try the Developer Runtime

The current development workflow requires Git, an OCI engine and a
digest-pinned agent image that is already loaded locally:

```sh
go install ./cmd/gatemole

gatemole --repo /path/to/service runtime init \
  --agent coding-agent \
  --image registry.example/coding-agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  --source-digest sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  -- /usr/local/bin/agent
```

The primary commands automatically host the default local development Runtime
for each invocation. Start `gatemole daemon` separately only for persistent,
custom-socket, model-broker, production, or edge operation.

Run and review the agent:

```sh
gatemole --repo /path/to/service doctor --agent coding-agent

gatemole --repo /path/to/service run \
  --intent "Fix authentication without changing public behavior" \
  --agent coding-agent

gatemole --repo /path/to/service review <transaction-id>

# Print only the daemon-bound frozen patch:
gatemole --repo /path/to/service diff <transaction-id>
```

This development command creates the isolated worktree, runs the agent, freezes
its Git effects and performs sequence validation. It prints the transaction ID
and next review command. Production verification,
approval and local-ref release require the hardened runtime configuration.
Runtime initialization creates a strict repository-owned agent profile and an
ignored local identity at `.gatemole/runtime.json`. Commit the agent profile and
`.gatemole/.gitignore`, not the Runtime identity. For OCI execution, `gatemole run`
preflights the exact Runtime ID, daemon enforcement profile and selected image
before creating authority or a worktree. Admission then durably binds the
Runtime ID and actual profile with the transaction. Doctor reports warnings for
optional or intentionally stopped components and fails on broken configured
requirements; it does not prove that every future task will succeed.

`review` reports the authoritative state, exact effect and evidence IDs, and a
digest-checked patch rendered from the frozen Git tree. After explicit
verification, preparation and any required signed approval, `apply` invokes
the existing release authority; `reject` aborts and discards the unreleased
worktree. Neither command is a shortcut around policy, and `reject` is not live
workload cancellation.

The product `run`, transaction, low-level `kernel` and `action` CLI surfaces
also send that exact Runtime ID on every daemon read and lifecycle call,
including long-running agent and verifier operations. Preflight and admission
validate it in their versioned bodies; later calls use `Gatemole-Runtime-ID`. The
binding catches accidental cross-Runtime wiring but is not an authentication
secret.

An agent integration only needs to read `$GATEMOLE_TASK_PATH`, edit `/workspace`
and exit. It should never receive the daemon socket or production credentials.
Without explicit model authority the container has no network:

```sh
gatemole --repo /path/to/service run \
  --intent "Apply the deterministic migration" \
  --agent migration-agent
```

When the daemon has a pinned provider broker, one task can request it
explicitly:

```sh
gatemole --repo /path/to/service run \
  --intent "Fix the failing authentication test" \
  --agent coding-agent \
  --model-provider openai
```

The resulting `OPENAI_API_KEY` is a transaction-scoped broker token, not the
provider credential. A stale run, expired authority or executable mismatch is
rejected before any broker or agent workload starts.

Production callers should add
`--require-enforcement-profile production` to both `doctor` and `run`; a
development daemon then fails preflight before task authority is created.

For the realistic payments incident, the full operator flow and optional
deterministic acceptance fixtures, read the
[Runtime examples guide](https://github.com/duriantaco/gatemole/blob/main/docs/EXAMPLES.md).

Read the
[transaction guide](https://github.com/duriantaco/gatemole/blob/main/docs/TRANSACTIONS.md)
and
[production operations guide](https://github.com/duriantaco/gatemole/blob/main/docs/PRODUCTION.md)
before treating the runtime as an enforcement boundary.

## Current supported profile

The supported deployment profile is one node and one security tenant on a
dedicated trusted host. It uses daemon-owned, digest-pinned OCI workloads,
paired run/transaction execution settlement and recovery, durable supported
budget charging, immutable Git-tree verification, OIDC, logically independent
signed approvals, a separate releaser and atomic publication to an allowed
local Git ref.

It does not push or merge remote changes, deploy software, coordinate database
or Kubernetes effects, isolate multiple tenants, expose a remote control API or
provide high availability. The generic action/connector path and Control Plane
shown in the Agent OS architecture are planned. Stable versioned release
packaging is pending.

The local Runtime identity, repository-local same-host/same-UID lock and ledger
lock prevent accidental wiring errors. Transaction worktrees default to a
validated private per-user, repository-scoped directory. These controls are
not cryptographic same-UID daemon attestation or cross-host fleet identity. A
hardened Runtime should use a dedicated OS account. Copying the ignored
identity together with its ledger to another repository copies the trust
target. Enrollment, attestation and revocation remain future Control Plane
work.

All CLI processes that connect to the hardened Unix socket must use the daemon
account's effective UID. OIDC tokens preserve logical operator, reviewer and
releaser identities, but the current approval CLI does not provide a separate
offline sign-and-submit path.

## Gatemole Contracts

Gatemole Contracts is useful when a repository needs human-owned semantic release
obligations in addition to runtime isolation:

```sh
gatemole --repo /path/to/service contracts try --write
gatemole --repo /path/to/service contracts compile
gatemole --repo /path/to/service contracts gate
```

Contracts and evidence enrich runtime verification and approval. They are not a
generic AI code reviewer and do not prove arbitrary code correct.

Use the **Contracts** navigation only when working with that optional module.
Gatemole Runtime is the current product; `gatemoled` is its trusted kernel.
