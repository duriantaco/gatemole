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
- **Vouch Agent OS** is the target enterprise experience: the same `gatemoled`
  kernel plus non-bypassable action connectors, cross-run policy and a Control
  Plane for a fleet of Runtimes. Those fleet and remote-system capabilities
  are planned, not implemented.

These are not separate engines. Developer adoption exercises the same kernel
semantics that future enterprise deployments require.

## Architecture: where the OS, Runtime and kernel sit

**Vouch Agent OS** names the complete target system; it is not another process
or deployment. The enforcement function lives in one or more customer-side
**Vouch Runtime** installations. Each Runtime contains a trusted **`gatemoled`
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
       |  |              gatemoled kernel                  |  |
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
| **Vouch Runtime** | The deployable enforcement boundary installed in a customer environment. It contains `gatemoled`, agent sandboxes, local durable state and connector drivers. | Runtime identity, a narrow single-node local-Git profile and exact profile-bound admission are implemented |
| **`gatemoled` kernel** | The trusted daemon that owns admission, authoritative lifecycle state, budgets, policy decisions, the transaction journal, approvals and commit coordination. | Runtime preflight, atomic task admission, live OCI authority revalidation and head-pinned launch claim implemented; paired lifecycle and action enforcement are still converging |
| **Agent sandbox** | The isolated, untrusted environment in which an agent loop executes. It receives no downstream production credentials. | OCI implementation available |
| **Agent adapter** | Connects an existing agent framework or command to the kernel. It may request work and actions but cannot authorize itself or create receipts. | Command/profile and lower-level integration exist; supported broker API planned |
| **Connector driver** | Performs typed operations against one downstream system after kernel authorization and reconciles external state. `gatemoled` records authoritative receipts and coordinates recovery. | Generic interface and remote drivers planned. Local Git currently uses a dedicated transaction path, not that future interface |
| **Vouch Contracts** | Optional verification module that turns human-owned intent into evidence obligations used by Runtime policy. | Beta |

The diagram states the intended ownership boundary, not a completion claim.
Today `vouch run` first asks the selected daemon to match the repository's
Runtime identity, report the requested enforcement profile and inspect the
selected digest-pinned image. This preflight happens before
task authority or a worktree is created. Task admission then durably binds the
exact Runtime ID and daemon enforcement profile with the content-bound
contract, real run, initial grants and transaction. Before OCI launch,
`gatemoled` reloads that live authority and fail-closed derives the permitted
image, command, full-workspace access, model-broker access and deadline. It
then atomically verifies the admitted run and transaction heads while
recording execution start, before starting any broker or agent workload.
Execution still advances only the transaction ledger; paired run lifecycle and
durable budget charging are the next Runtime milestone.

The target remote-effect path is:

```text
agent proposes an action
  -> gatemoled authenticates the run and delegated authority
  -> gatemoled evaluates the action and complete transaction sequence
  -> deny | request approval | authorize
  -> connector driver stages or executes with scoped credentials
  -> gatemoled records the receipt and verifies the outcome
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
`gatemoled`, and a digest-pinned agent image.

This is a low-level developer integration, not yet a self-serve desktop agent
environment. `vouch runtime init` creates a strict repository-owned profile
and a local Runtime identity. `vouch doctor` diagnoses Git, OCI, profile,
local-image and daemon readiness. Packaged adapters, daemon supervision,
`watch`, live cancellation and a friendly diff/apply flow remain roadmap work.

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
is deliberately not invented by Vouch. Initialization also creates
`.gatemole/runtime.json`, a random local Runtime instance identity, and rules that
keep it, SQLite/WAL files, locks and the daemon socket out of Git. Commit
`.gatemole/agent-profiles.json` and `.gatemole/.gitignore`; do not commit
`.gatemole/runtime.json`.

This is a hard pre-1.0 state cutover. If a repository still contains `.vouch`,
Gatemole fails before writing any new state; it never merges, renames, or
rehashes old configuration, ledgers, evidence, or signatures automatically.
Archive or remove the legacy directory deliberately, then initialize
`.gatemole`.

Start the repository-local development daemon in one terminal:

```sh
vouch --repo /path/to/service daemon
```

By default, transaction worktrees live in a canonical, repository-scoped
directory beneath the current user's validated runtime or cache directory,
not in a shared predictable `/tmp/gatemole-transactions` path. A custom
`--transaction-root` is validated before the ledger opens.

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
When the socket exists, doctor uses the daemon's authoritative Runtime
preflight instead of treating a separate CLI-side OCI probe as proof.

For a developer, the integration contract is deliberately small:

1. Package the existing agent as a digest-pinned OCI image.
2. Make its command read the retained task at `$GATEMOLE_TASK_PATH`.
3. Let it edit only `/workspace` and return a normal process exit code.
4. Start it with `vouch run`; do not give the container the daemon socket,
   repository credentials or downstream production credentials.
5. Inspect the transaction and its hash-chained events before verification,
   approval and release.

For example, an agent entrypoint can begin with:

```sh
#!/bin/sh
set -eu
test "$GATEMOLE_RUNTIME_ROLE" = agent
test -r "$GATEMOLE_TASK_PATH"
cd /workspace

# Invoke your existing agent loop here. All intended file effects stay in this
# detached transaction worktree.
exec /opt/my-agent --task-file "$GATEMOLE_TASK_PATH"
```

`vouch runtime init` writes the strict, repository-owned
`.gatemole/agent-profiles.json` and local-only `.gatemole/runtime.json`. Select the
agent profile by name:

```sh
vouch --repo /path/to/service run \
  --intent "Fix authentication without changing public behavior" \
  --agent coding-agent
```

The public profile schema is
[`schemas/gatemole.agent_profiles.v0.schema.json`](schemas/gatemole.agent_profiles.v0.schema.json),
with a complete
[example profile](schemas/fixtures/runtime/valid/agent_profiles.json). Vouch
binds the selected profile, final command, pinned image and exact task intent
into a durable task envelope. For OCI execution, `vouch run` first preflights
the exact local Runtime ID, enforcement profile and image with `gatemoled`; a
failure creates neither task authority nor a worktree. Admission v1 then binds
that Runtime ID and the daemon's actual enforcement profile into the durable
transaction. Daemon-owned OCI agents receive the task envelope read-only at
`/gatemole/task.json`.

The product CLI loads `.gatemole/runtime.json` and binds every daemon call from
the `run`, transaction, low-level `kernel` and `action` surfaces to that ID.
Preflight and current task admission carry the expected ID in their validated
bodies; lifecycle mutations, reads and long-running agent/verifier operations
also carry `Gatemole-Runtime-ID`. A configured daemon rejects a missing or
different header before those handlers run. This is exact Runtime correlation,
not a secret or an authentication credential.

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

Production callers should make the expected profile explicit:

```sh
vouch --repo /srv/vouch/repository run \
  --socket /run/gatemole/gatemoled.sock \
  --namespace engineering \
  --require-enforcement-profile production \
  --intent "Fix the approved authentication regression" \
  --agent coding-agent
```

A development daemon cannot satisfy that command, so it fails during preflight
before admission or worktree creation.

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
- A repository-local Runtime identity, stable Runtime and ledger locks,
  same-UID Unix peer authentication, daemon-authoritative preflight, and
  Runtime/profile-bound v1 admission.
- A private, owner-validated transaction staging root and immutable startup
  snapshots for trust, verifier and model-broker policy inputs.
- Atomic compare-and-swap publication to an allowed local Git ref.
- Bounded Git inspection, workload concurrency, output capture and readiness
  checks.

## Supported deployment profile

The current supported profile is deliberately narrow:

- One node and one security tenant.
- A dedicated trusted host or VM and non-root daemon account.
- One repository and SQLite ledger per runtime boundary.
- One local Runtime identity bound to that ledger and enforcement profile.
- Preloaded, digest-pinned OCI images.
- Required verifier profiles and logically independent signed approval.
- Publication to an allowed local Git ref only.

It does **not** push to a remote, create or merge pull requests, deploy
software, coordinate database or Kubernetes effects, provide multi-tenant
isolation, expose a remote control API, or provide high availability. Stable
versioned release packaging is also still pending. The exact host, storage,
identity, quota and acceptance requirements are documented in
[docs/PRODUCTION.md](docs/PRODUCTION.md).

The Runtime identity, repository-local `.gatemole/runtime.lock`, and ledger lock
prevent an accidental same-host, same-UID daemon or ledger mix-up even when
environment or database paths differ. They are not fleet enrollment,
cryptographic same-UID daemon attestation or cross-host attestation. Use a
dedicated OS account for a hardened Runtime. Deliberately copying all ignored
identity and ledger state to another repository copies the trust target too.
Organization enrollment, attested Runtime identity and fleet revocation belong
to the planned Control Plane.

Every CLI process that connects to the hardened Unix socket must use that
daemon account's effective UID. OIDC tokens still distinguish operators,
reviewers and releasers, but the current approval CLI has no separate offline
sign-and-submit path; see the production guide before claiming OS-level
reviewer-key custody separation.

## Vouch Contracts

Repositories that need semantic release obligations can opt into the Contracts
module:

```sh
vouch --repo /path/to/service contracts try --write
vouch --repo /path/to/service contracts compile
pytest --junitxml .gatemole/artifacts/pytest.xml
vouch --repo /path/to/service contracts evidence import junit \
  .gatemole/artifacts/pytest.xml
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
image="$(scripts/vouchproductionfixture.sh --tag gatemole-production-fixture:acceptance)"
GATEMOLE_PRODUCTION_IMAGE="$image" scripts/vouchproductionbench.sh
```

Passing these gates establishes the documented invariants for that revision. It
does not establish correctness for arbitrary agent output or expand the
supported deployment boundary.

## License

Apache-2.0. See [LICENSE](LICENSE).
