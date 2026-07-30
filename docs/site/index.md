# Vouch

Vouch is an enforcement kernel and transaction Runtime for autonomous software
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

Vouch does not decide how an agent reasons or writes code. The trusted local
kernel, `vouchd`, orchestrates sandboxed execution and owns authoritative
transaction state, verification, policy decisions and commit coordination.

## Product hierarchy

- **Vouch Developer Runtime** is the local product experience: run an existing
  coding agent in an isolated Git transaction, inspect its effects and control
  release. A low-level, manually operated local-Git integration exists today;
  the self-serve developer preview is still a milestone.
- **Vouch Agent OS** is the planned enterprise product experience and complete
  target architecture: the Control Plane, Runtime fleet, transaction protocol
  and connector model. It is an umbrella, not another process.
- **Vouch Control Plane** is the planned commercial management layer for
  Runtime fleets, organization policy, approvals, audit and incident response.
- **Vouch Runtime** is the deployable customer-side enforcement boundary. A
  narrow single-node local-Git profile is implemented today.
- **`vouchd`** is the trusted transaction kernel inside each Runtime.
- **Vouch Contracts** is an optional verification module that compiles release
  intent into obligations and maps evidence to them.

The Developer Runtime and enterprise **Vouch Agent OS** experience use the same
`vouchd` kernel; they are not separate engines.

## Try the Developer Runtime

The current development workflow requires Git, an OCI engine and a
digest-pinned agent image that is already loaded locally:

```sh
go install ./cmd/vouch

vouch --repo /path/to/service runtime init \
  --agent coding-agent \
  --image registry.example/coding-agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  --source-digest sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  -- /usr/local/bin/agent

vouch --repo /path/to/service daemon
```

In another terminal:

```sh
vouch --repo /path/to/service doctor --agent coding-agent

vouch --repo /path/to/service run \
  --intent "Fix authentication without changing public behavior" \
  --agent coding-agent
```

This development command creates the isolated worktree, runs the agent, freezes
its Git effects and performs sequence validation. Production verification,
approval and local-ref release require the hardened runtime configuration.
Runtime initialization creates a strict repository-owned agent profile and an
ignored local identity at `.vouch/runtime.json`. Commit the agent profile and
`.vouch/.gitignore`, not the Runtime identity. For OCI execution, `vouch run`
preflights the exact Runtime ID, daemon enforcement profile and selected image
before creating authority or a worktree. Admission then durably binds the
Runtime ID and actual profile with the transaction. Doctor reports warnings for
optional or intentionally stopped components and fails on broken configured
requirements; it does not prove that every future task will succeed.

The product `run`, transaction, low-level `kernel` and `action` CLI surfaces
also send that exact Runtime ID on every daemon read and lifecycle call,
including long-running agent and verifier operations. Preflight and admission
validate it in their versioned bodies; later calls use `Vouch-Runtime-ID`. The
binding catches accidental cross-Runtime wiring but is not an authentication
secret.

An agent integration only needs to read `$VOUCH_TASK_PATH`, edit `/workspace`
and exit. It should never receive the daemon socket or production credentials.
Without explicit model authority the container has no network:

```sh
vouch --repo /path/to/service run \
  --intent "Apply the deterministic migration" \
  --agent migration-agent
```

When the daemon has a pinned provider broker, one task can request it
explicitly:

```sh
vouch --repo /path/to/service run \
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

For a runnable deterministic payments-service fixture, including building the
agent image, starting `vouchd`, inspecting effects and understanding why an
authentication change requires approval, read the
[Runtime examples guide](https://github.com/duriantaco/vouch/blob/main/docs/EXAMPLES.md).

Read the
[transaction guide](https://github.com/duriantaco/vouch/blob/main/docs/TRANSACTIONS.md)
and
[production operations guide](https://github.com/duriantaco/vouch/blob/main/docs/PRODUCTION.md)
before treating the runtime as an enforcement boundary.

## Current supported profile

The supported deployment profile is one node and one security tenant on a
dedicated trusted host. It uses daemon-owned, digest-pinned OCI workloads,
immutable Git-tree verification, OIDC, logically independent signed approvals, a separate
releaser and atomic publication to an allowed local Git ref.

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

## Vouch Contracts

Vouch Contracts is useful when a repository needs human-owned semantic release
obligations in addition to runtime isolation:

```sh
vouch --repo /path/to/service contracts try --write
vouch --repo /path/to/service contracts compile
vouch --repo /path/to/service contracts gate
```

Contracts and evidence enrich runtime verification and approval. They are not a
generic AI code reviewer and do not prove arbitrary code correct.

Use the **Contracts** navigation only when working with that optional module.
Vouch Runtime is the current product; `vouchd` is its trusted kernel.
