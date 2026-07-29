<p align="center">
  <img src="assets/vouch.png" alt="Vouch logo" width="220">
</p>

# Vouch

Vouch is an enforcement kernel and transaction Runtime for autonomous agents.
Agents may propose work, but Vouch controls the boundary between that proposal
and an exact effect.

Vouch does not replace an agent framework, model provider, identity provider,
container runtime, Kubernetes or the host operating system. It controls the
authority and transaction boundary around agent work.

One kernel serves two product experiences:

- **Vouch Developer Runtime** is the local experience for developers who want
  to run an existing coding agent in an isolated Git transaction, inspect its
  exact effects and control release. A low-level, manually operated local-Git
  integration exists today; the self-serve developer preview is still a
  milestone.
- **Vouch Agent OS** is the target enterprise experience: the same `vouchd`
  kernel plus non-bypassable action connectors, cross-run policy and a Control
  Plane for a fleet of Runtimes. Those fleet and remote-system capabilities
  are planned, not implemented.

These are not separate engines. Developer adoption exercises the same kernel
semantics that future enterprise deployments require.

## Architecture: where the OS, Runtime and kernel sit

**Vouch Agent OS** names the complete target system; it is not another process
or deployment. The enforcement function lives in one or more customer-side
**Vouch Runtime** installations. Each Runtime contains a trusted **`vouchd`
kernel**. The planned **Vouch Control Plane** manages a fleet of Runtimes.

```text
                         administrators / approvers
                                   |
       +---------------- Vouch Agent OS ------------------+
       |          the complete target Vouch system       |
       |                                                  |
       |  Vouch Control Plane [planned]                   |
       |  fleet | organization policy | approval routing |
       |  audit | incident response | fleet revocation   |
       |                 |                 ^              |
       |      signed policy/authority      | receipts     |
       |                 v                 |              |
task   |  +------- Vouch Runtime [customer-side] ------+  |
------>|  | customer-side enforcement boundary         |  |
       |  |                                             |  |
       |  |              vouchd kernel                  |  |
       |  |  admission | live authority | Git journal  |  |
       |  |  Git effects | sequence policy | approval  |  |
       |  |  local-ref commit                  [current]|  |
       |  |                                             |  |
       |  |  paired lifecycle | durable budgets        |  |
       |  |  action broker | cross-run policy          |  |
       |  |  reconciliation                    [planned]|  |
       |  |       | launches                           |  |
       |  |       v                                    |  |
       |  |  agent sandbox ---> private Git worktree   |  |
       |  |  + agent adapter                 [current]  |  |
       |  |       |                                     |  |
       |  |       +-- ActionRequest -> connector API    |  |
       |  |                                  [planned]  |  |
       |  +--------------------------------------|------+  |
       +-----------------------------------------|---------+
                                                 v
             external systems via planned drivers:
             GitHub | Kubernetes | PostgreSQL | SAP | cloud
```

The operating-system analogy is precise:

| Name | Meaning | Status |
| --- | --- | --- |
| **Vouch Developer Runtime** | The local product experience around one Runtime: package an agent, execute it in an isolated Git transaction, inspect exact effects and control release. | Low-level integration, Runtime profile initialization and diagnostics implemented; packaged adapters, `watch`, live cancellation and review UX planned |
| **Vouch Agent OS** | The enterprise product experience and complete target architecture: Control Plane, Runtime fleet, transaction protocol and connector model. It is an umbrella, not a process. | Product direction |
| **Vouch Control Plane** | Organization-wide fleet, policy, approval, audit and incident management. It manages Runtimes but does not execute agent actions. | Planned |
| **Vouch Runtime** | The deployable enforcement boundary installed in a customer environment. It contains `vouchd`, agent sandboxes, local durable state and connector drivers. | Narrow single-node local-Git profile implemented |
| **`vouchd` kernel** | The trusted daemon that owns admission, authoritative lifecycle state, budgets, policy decisions, the transaction journal, approvals and commit coordination. | Atomic task admission, live OCI authority preflight and head-pinned launch claim implemented; paired lifecycle and action enforcement are still converging |
| **Agent sandbox** | The isolated, untrusted environment in which an agent loop executes. It receives no downstream production credentials. | OCI implementation available |
| **Agent adapter** | Connects an existing agent framework or command to the kernel. It may request work and actions but cannot authorize itself or create receipts. | Command/profile and lower-level integration exist; supported broker API planned |
| **Connector driver** | Performs typed operations against one downstream system after kernel authorization and reconciles external state. `vouchd` records authoritative receipts and coordinates recovery. | Generic interface and remote drivers planned. Local Git currently uses a dedicated transaction path, not that future interface |
| **Vouch Contracts** | Optional verification module that turns human-owned intent into evidence obligations used by Runtime policy. | Beta |

The diagram states the intended ownership boundary, not a completion claim.
Today `vouch run` atomically admits its task, content-bound contract, real run,
initial grants and transaction. Before OCI launch, `vouchd` reloads that live
authority and fail-closed derives the permitted image, command, full-workspace
access, model-broker access and deadline. It then atomically verifies the
admitted run and transaction heads while recording execution start, before
starting any broker or agent workload. Execution still advances only the
transaction ledger; paired run lifecycle and durable budget charging are the
next Runtime milestone.

The target remote-effect path is:

```text
agent proposes an action
  -> vouchd authenticates the run and delegated authority
  -> vouchd evaluates the action and complete transaction sequence
  -> deny | request approval | authorize
  -> connector driver stages or executes with scoped credentials
  -> vouchd records the receipt and verifies the outcome
  -> commit | compensate | reconcile | require manual recovery
```

This broker-and-connector path is not implemented in the current production
profile; the older action endpoints are disabled there. Today the contained
effect boundary is mutation of a private Git worktree followed by exact
inspection, verification, approval and a local-ref compare-and-swap.

An agent with direct downstream credentials can bypass Vouch. A future
multi-system deployment is therefore enforcement-grade only when production
credentials and network paths are available exclusively through Vouch
connector drivers.

The current local-Git transaction is the first kernel transaction-and-effect
slice. A complete Vouch Agent OS external claim requires both non-bypassable,
multi-system Runtime enforcement and a Vouch Control Plane managing a fleet of
those Runtimes.

## Developer Runtime: current local-Git integration

The current runtime requires Git, an OCI engine such as Docker, a running
`vouchd`, and a digest-pinned agent image.

This is a low-level developer integration, not yet a self-serve desktop agent
environment. `vouch runtime init` creates a strict repository-owned profile and
`vouch doctor` diagnoses Git, OCI, profile, local-image and daemon readiness.
Packaged adapters, daemon supervision, `watch`, live cancellation and a
friendly diff/apply flow remain roadmap work.

Build the CLI from source:

```sh
go install ./cmd/vouch
```

In a Git repository with a `HEAD` commit, register an agent image that is
already present in the local OCI engine:

```sh
vouch --repo /path/to/service runtime init \
  --agent coding-agent \
  --image registry.example/coding-agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  --source-digest sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  -- /usr/local/bin/agent
```

`--source-digest` identifies the source or build input used for that image; it
is deliberately not invented by Vouch. Start the repository-local development
daemon in one terminal:

```sh
vouch --repo /path/to/service daemon
```

In another terminal, diagnose the selected integration and run it:

```sh
vouch --repo /path/to/service doctor --agent coding-agent

vouch --repo /path/to/service run \
  --intent "Fix authentication without changing public behavior" \
  --agent coding-agent
```

Doctor warnings, such as an intentionally stopped daemon or omitting
`--agent`, do not make the command fail. A selected missing image, invalid
profile, unavailable OCI engine or unready existing daemon does.

For a developer, the integration contract is deliberately small:

1. Package the existing agent as a digest-pinned OCI image.
2. Make its command read the retained task at `$VOUCH_TASK_PATH`.
3. Let it edit only `/workspace` and return a normal process exit code.
4. Start it with `vouch run`; do not give the container the daemon socket,
   repository credentials or downstream production credentials.
5. Inspect the transaction and its hash-chained events before verification,
   approval and release.

For example, an agent entrypoint can begin with:

```sh
#!/bin/sh
set -eu
test "$VOUCH_RUNTIME_ROLE" = agent
test -r "$VOUCH_TASK_PATH"
cd /workspace

# Invoke your existing agent loop here. All intended file effects stay in this
# detached transaction worktree.
exec /opt/my-agent --task-file "$VOUCH_TASK_PATH"
```

`vouch runtime init` writes the strict, repository-owned
`.vouch/agent-profiles.json`. Select that profile by name:

```sh
vouch --repo /path/to/service run \
  --intent "Fix authentication without changing public behavior" \
  --agent coding-agent
```

The public profile schema is
[`schemas/vouch.agent_profiles.v0.schema.json`](schemas/vouch.agent_profiles.v0.schema.json),
with a complete
[example profile](schemas/fixtures/runtime/valid/agent_profiles.json). Vouch
binds the selected profile, final command, pinned image and exact task intent
into a durable task envelope. Before launch, `vouchd` atomically admits that
task with its derived execution contract, real run, initial grants and
transaction. Daemon-owned OCI agents receive the task envelope read-only at
`/vouch/task.json`.

Model egress is absent unless the task explicitly requests the configured
daemon broker, for example with `--model-provider openai`. The provider
credential never enters the agent container:

```sh
# No model authority: network=none and no model credential in the container.
vouch --repo /path/to/service run \
  --intent "Apply the deterministic migration" \
  --agent migration-agent

# Explicit model authority: only the configured OpenAI-compatible broker is
# reachable. OPENAI_API_KEY is a transaction-scoped broker token, not the
# provider credential.
vouch --repo /path/to/service run \
  --intent "Fix the failing authentication test" \
  --agent coding-agent \
  --model-provider openai
```

At launch, the kernel derives the run identity, image, command, workspace
access, broker access and deadline from live admitted state. A stale run, an
expired grant, a changed image or command, a narrower unsupported workspace
grant, or an ungranted model provider fails before any broker or agent
container starts.

For concrete scenarios rather than placeholders, see
[Runtime examples](docs/EXAMPLES.md). It includes a runnable deterministic
authentication-hotfix fixture, illustrative model-assisted and networkless
deployment patterns, and an explicit description of which enterprise
connector examples are not implemented.

`vouch run` then creates the isolated worktree, runs the agent, freezes its Git
effects, and performs deterministic sequence validation. Inspect the result
with:

```sh
vouch --repo /path/to/service status <transaction-id> \
  --namespace local

# Advanced compatibility surface:
vouch --repo /path/to/service tx effects \
  --namespace local --id <transaction-id>

vouch --repo /path/to/service tx events \
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
- [Product validation and competitive assessment](docs/PRODUCT_VALIDATION.md)
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
