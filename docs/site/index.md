# Vouch Runtime

Vouch Runtime is the transaction runtime for autonomous software agents. It
controls the boundary between an agent's proposal and a real effect:

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
kernel, `vouchd`, owns execution, durable transaction state, verification,
authority and commit.

## Product hierarchy

- **Vouch Runtime** is the current product.
- **`vouchd`** is its trusted transaction kernel.
- **Vouch Contracts** is an optional verification module that compiles release
  intent into obligations and maps evidence to them.
- **Vouch Control Plane** is the future commercial management layer for fleets,
  organization policy, approvals, audit and enterprise connectors.
- **Agent OS** is the long-term north star, not a claim about the current
  implementation.

## Try the runtime

The current development workflow requires Git, an OCI engine, a running daemon
and a digest-pinned agent image:

```sh
go install ./cmd/vouch ./cmd/vouchd

vouchd \
  --repo /path/to/service \
  --db /tmp/vouch-service/kernel.db \
  --socket /tmp/vouch-service/vouchd.sock \
  --transaction-root /tmp/vouch-service/transactions
```

In another terminal:

```sh
vouch --repo /path/to/service run \
  --socket /tmp/vouch-service/vouchd.sock \
  --namespace local \
  --intent "Fix authentication without changing public behavior" \
  --runtime oci \
  --image registry.example/coding-agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  -- /usr/local/bin/agent
```

This development command creates the isolated worktree, runs the agent, freezes
its Git effects and performs sequence validation. Production verification,
approval and local-ref release require the hardened runtime configuration.
For repeatable integrations, select a strict repository-owned agent profile
with `--agent NAME`; Vouch then binds the profile, pinned image, final command
and exact intent into a durable task envelope mounted read-only at
`/vouch/task.json`.

Read the
[transaction guide](https://github.com/duriantaco/vouch/blob/main/docs/TRANSACTIONS.md)
and
[production operations guide](https://github.com/duriantaco/vouch/blob/main/docs/PRODUCTION.md)
before treating the runtime as an enforcement boundary.

## Current supported profile

The supported deployment profile is one node and one security tenant on a
dedicated trusted host. It uses daemon-owned, digest-pinned OCI workloads,
immutable Git-tree verification, OIDC, signed independent approvals, a separate
releaser and atomic publication to an allowed local Git ref.

It does not push or merge remote changes, deploy software, coordinate database
or Kubernetes effects, isolate multiple tenants, expose a remote control API or
provide high availability. Stable versioned release packaging is pending.

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
The runtime and `vouchd` remain the product center.
