<p align="center">
  <img src="assets/vouch.png" alt="Vouch logo" width="220">
</p>

# Vouch Runtime

Vouch Runtime is the transaction runtime for autonomous software agents. It
runs an agent inside an isolated task boundary, records the resulting effects,
verifies the exact proposed state, obtains any required independent authority,
and publishes an approved outcome atomically.

```text
task intent
  -> isolated agent execution
  -> normalized effect ledger
  -> immutable staged state
  -> policy and independent verification
  -> approval when required
  -> commit | block | revise | recover
```

Vouch does not decide how an agent reasons or writes code. It controls the
boundary between an agent's proposal and a real effect.

## Product hierarchy

- **Vouch Runtime** is the current product and developer surface.
- **`vouchd`** is its trusted local kernel. It owns execution, durable state,
  policy, verification, approval and commit.
- **Vouch Contracts** is an optional verification module. It compiles
  human-owned release intent into obligations and maps evidence to those
  obligations. It strengthens a runtime policy; it is not the primary product.
- **Vouch Control Plane** is the future commercial management layer for runner
  fleets, organization policy, approval UX, audit retention, connectors and
  enterprise operations. It is not implemented in this repository today.
- **Agent OS** is the long-term north star. Vouch should use that description
  only after it provides a non-bypassable transaction boundary across the
  important resources in an agent workflow.

## Runtime quick start

The current runtime requires Git, an OCI engine such as Docker, a running
`vouchd`, and a digest-pinned agent image.

Build the local binaries:

```sh
go install ./cmd/vouch ./cmd/vouchd
```

Start a development daemon for one repository:

```sh
vouchd \
  --repo /path/to/service \
  --db /tmp/vouch-service/kernel.db \
  --socket /tmp/vouch-service/vouchd.sock \
  --transaction-root /tmp/vouch-service/transactions
```

Run an agent in an isolated transaction:

```sh
vouch --repo /path/to/service run \
  --socket /tmp/vouch-service/vouchd.sock \
  --namespace local \
  --intent "Fix authentication without changing public behavior" \
  --runtime oci \
  --image registry.example/coding-agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  -- /usr/local/bin/agent
```

For repeatable integrations, define a strict, repository-owned
`.vouch/agent-profiles.json` and select it by name:

```sh
vouch --repo /path/to/service run \
  --socket /tmp/vouch-service/vouchd.sock \
  --intent "Fix authentication without changing public behavior" \
  --agent coding-agent
```

The public profile schema is
[`schemas/vouch.agent_profiles.v0.schema.json`](schemas/vouch.agent_profiles.v0.schema.json),
with a complete
[example profile](schemas/fixtures/runtime/valid/agent_profiles.json). Vouch
binds the selected profile, final command, pinned image and exact task intent
into a durable task envelope. Daemon-owned OCI agents receive that envelope
read-only at `/vouch/task.json`.

`vouch run` currently creates the transaction and worktree, runs the agent, freezes
its Git effects, and performs deterministic sequence validation. Inspect the
result with:

```sh
vouch --repo /path/to/service status <transaction-id> \
  --socket /tmp/vouch-service/vouchd.sock

# Advanced compatibility surface:
vouch --repo /path/to/service tx effects \
  --socket /tmp/vouch-service/vouchd.sock \
  --namespace local --id <transaction-id>

vouch --repo /path/to/service tx events \
  --socket /tmp/vouch-service/vouchd.sock \
  --namespace local --id <transaction-id>
```

Production verification and preparation use the advanced `tx verify` and
`tx prepare` operations. Reviewers and releasers use the top-level
`vouch approve` and `vouch release` commands under the hardened daemon
configuration. See [Production Runtime Operations](docs/PRODUCTION.md) for the
complete deployment contract; do not infer production safety from the
development example above.

## What the runtime enforces today

The implemented runtime includes:

- Daemon-owned OCI agent and verifier execution.
- Detached, task-scoped Git worktrees.
- Immutable Git-tree staging and exact read-only verifier materialization.
- A normalized, ordered effect ledger and deterministic sequence policy.
- Hash-chained transaction events and crash-recoverable SQLite projections.
- Digest-pinned agent, verifier and model-broker images.
- Policy-controlled model access without exposing the provider credential to
  the agent.
- OIDC operator identity, signed approval packages and separation of approver
  and releaser.
- Atomic compare-and-swap publication to an allowed local Git ref.
- Bounded Git inspection, workload concurrency, output capture and readiness
  checks.

## Supported deployment profile

The current supported profile is deliberately narrow:

- One node and one security tenant.
- A dedicated trusted host or VM and non-root daemon account.
- One repository and SQLite ledger per runtime boundary.
- Preloaded, digest-pinned OCI images.
- Required verifier profiles and independent approval.
- Publication to an allowed local Git ref only.

It does **not** push to a remote, create or merge pull requests, deploy
software, coordinate database or Kubernetes effects, provide multi-tenant
isolation, expose a remote control API, or provide high availability. Stable
versioned release packaging is also still pending. The exact host, storage,
identity, quota and acceptance requirements are documented in
[docs/PRODUCTION.md](docs/PRODUCTION.md).

## Vouch Contracts

Repositories that need semantic release obligations can opt into the Contracts
module:

```sh
vouch --repo /path/to/service contracts try --write
vouch --repo /path/to/service contracts compile
pytest --junitxml .vouch/artifacts/pytest.xml
vouch --repo /path/to/service contracts evidence import junit \
  .vouch/artifacts/pytest.xml
vouch --repo /path/to/service contracts gate
```

Contracts turn human-owned intent into stable obligation IDs and determine
whether supplied evidence covers those obligations. They do not prove arbitrary
code correct and are not a generic AI code reviewer.

## Documentation

- [Agent transactions](docs/TRANSACTIONS.md)
- [Production runtime operations](docs/PRODUCTION.md)
- [Kernel internals](docs/KERNEL.md)
- [Runtime and product roadmap](ROADMAP.md)
- [Transaction-control decision](docs/architecture/ADR-002-agent-transaction-control.md)
- [Threat model](docs/architecture/TRANSACTION_THREAT_MODEL.md)
- [Vouch Contracts](docs/COMPILER.md)
- [Benchmarks and acceptance](docs/BENCHMARKS.md)
- [Contributing](CONTRIBUTING.md)

Published documentation: <https://duriantaco.github.io/vouch/>

## Validation

The repository maintains separate acceptance paths for the Contracts module,
kernel semantics, transactions, runtime execution and the supported production
profile:

```sh
go test ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
go test -race -count=1 ./internal/kernel/...
scripts/vouchbench.sh
scripts/vouchkernelbench.sh --out /tmp/vouchkernelbench
scripts/vouchtransactionbench.sh
scripts/vouchruntimebench.sh
image="$(scripts/vouchproductionfixture.sh --tag vouch-production-fixture:acceptance)"
VOUCH_PRODUCTION_IMAGE="$image" scripts/vouchproductionbench.sh
```

Passing these gates establishes the documented invariants for that revision. It
does not establish correctness for arbitrary agent output or expand the
supported deployment boundary.

## License

Apache-2.0. See [LICENSE](LICENSE).
