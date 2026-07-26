# Vouch Runtime Roadmap

## Product direction

Vouch Runtime is the transaction runtime for autonomous agents. The current
product is not the older release-contract compiler, and it is not yet a complete
Agent OS.

The product hierarchy is:

| Layer | Role | Status |
| --- | --- | --- |
| Vouch Runtime | Isolate, stage, verify, authorize, commit and recover an agent task | Current product |
| `vouchd` | Trusted transaction kernel and local enforcement point | Implemented |
| Vouch Contracts | Optional contract-to-obligation verification module | Implemented beta module |
| Vouch Control Plane | Fleet, policy, approvals, audit, connectors and enterprise administration | Future commercial layer |
| Agent OS | Non-bypassable transaction boundary across the important resources in an agent workflow | Long-term north star |

The immediate technical category remains **Agent Transaction Control**. Vouch
should earn the Agent OS description through enforcement coverage rather than
adopt it as a premature marketing claim.

## Product thesis

Agents will receive useful authority only when organizations can control the
complete task, not merely approve isolated tool calls after the fact.

```text
human intent
  -> admitted agent execution
  -> staged, normalized effects
  -> exact-state verification
  -> outcome-oriented approval
  -> ordered commit and receipts
  -> postconditions and recovery
```

Vouch succeeds when a team grants an agent permission to complete work it
previously allowed the agent only to suggest.

## Implemented foundation

The repository already contains:

- Strict versioned resources for agent images, execution contracts, runs,
  capabilities, actions, transactions, effects, verifications, approvals,
  commit plans, checkpoints and events.
- Deterministic reducers and append-only hash-chained histories.
- SQLite event/projection persistence, optimistic concurrency and restart
  recovery.
- `vouchd` over a mode-`0600` Unix socket.
- Daemon-owned OCI execution with non-root identities, read-only roots,
  resource limits and bounded output.
- Transaction-specific model brokering with provider credential isolation,
  model/tool policy, token budgets and durable receipts.
- Detached Git worktrees, bounded raw staging, immutable tree revisions and
  exact read-only verifier materializations.
- Normalized effect ledgers and initial sequence/separation policy.
- OIDC role and namespace enforcement.
- Immutable approval packages, signed independent approvals and a distinct
  release identity.
- Compare-and-swap publication to allowed local Git refs and crash
  reconciliation.
- Readiness checks and separate kernel, transaction, runtime, Contracts and
  production acceptance harnesses.
- The optional Vouch Contracts compiler, obligation IR, evidence import and
  release-policy module.

## Current supported profile

The supported runtime profile is deliberately narrow:

- One node, one security tenant and one trusted host/VM.
- One non-root daemon boundary using SQLite and local storage.
- Preloaded digest-pinned agent, verifier and broker images.
- Daemon-owned mandatory verifiers.
- OIDC operator identity, signed approvals and separation of duties.
- Publication to an allowed local Git branch ref.

The profile does not include remote push or pull-request merge, deployment,
database effects, submodules, multi-tenancy, a network control API, distributed
scheduling or HA. Stable versioned binary packaging and upgrade compatibility
are still pending. The complete requirements remain in
[docs/PRODUCTION.md](docs/PRODUCTION.md).

## Execution order

### Phase 0: Coherent developer surface

Make the implemented runtime understandable and usable without exposing its
internal state-machine choreography.

- Implemented: task execution is the primary top-level `vouch run` workflow;
  `status`, `approve` and `release` are product-facing commands; the older run
  kernel is under `vouch kernel`; and the compiler/evidence surface is grouped
  under `vouch contracts`.
- Add `watch`, product-level `effects`, `events`, `cancel` and
  `doctor` experiences around one transaction ID.
- Add a strict versioned runtime configuration file.
- Ship stable binary/container installation and a service definition.
- Publish an explicit CLI and schema compatibility policy.

Exit criteria:

- A developer can install Vouch, start a local runtime, run a supported coding
  agent and understand the next required action from one quick start.
- Normal use does not require manually authoring event envelopes, digests or
  sequence cursors.

### Phase 1: Public runtime API and agent protocol

Turn the internal foundation into an embeddable product.

- Publish public versioned API resource packages and an OpenAPI document.
- Publish a supported Go client; add another SDK only after the API stabilizes.
- Add idempotent task creation and mutation requests.
- Add bounded event/status streaming and pagination.
- Implemented locally: strict named profiles make `AgentImage` executable with
  a pinned OCI reference and fixed entrypoint.
- Implemented locally: `vouch.agent_task.v0` durably retains and digest-binds
  exact intent, transaction/run/profile/image/command identity and is mounted
  read-only at `/vouch/task.json`. External encrypted artifact storage remains
  control-plane work.
- Wire execution contracts, budgets, verifier profiles and release targets into
  transaction creation.
- Provide a generic command adapter and one polished coding-agent adapter.

Exit criteria:

- An external application can submit and observe a transaction without
  importing `internal/` packages or reproducing CLI orchestration.
- The exact task, agent image, runtime policy and verifier set are immutable and
  attributable.

### Phase 2: Deep remote-Git workflow

Build the first complete customer workflow rather than many shallow connectors.

- GitHub App installation and short-lived credential brokering.
- Staged branch and pull-request creation/update with idempotency and receipts.
- Exact commit/status/check bindings.
- Reviewer-facing approval package and decision UX.
- Merge/reconciliation semantics that expose conflicts and unknown outcomes.
- Audit bundle export for one transaction.

Exit criteria:

- A design partner lets an agent open or update a protected change through
  Vouch that it would not let the agent publish directly.
- No GitHub effect becomes real outside the frozen policy, verification and
  approval package.

### Phase 3: Vouch Control Plane

Build the future commercial layer around self-hosted runtime data planes.

- Runtime registration, health and fleet inventory.
- Organization policy distribution and versioning.
- SSO/RBAC, approval routing and delegation.
- Audit retention, search, export and SIEM integration.
- Usage, model-cost and budget visibility.
- Connector and verifier profile management.
- Remote revocation and incident controls.
- Managed or enterprise-supported deployment options.

The commercial boundary is management and assurance at organization scale.
The local runtime should remain useful on its own. Likely charging units are
controlled agent transactions or active agents, with infrastructure/model
compute accounted for separately; pricing must be validated with paid pilots.

Exit criteria:

- One paid design partner manages multiple runtime workers through the control
  plane.
- Control-plane loss does not cause acknowledged effects to be duplicated or
  authoritative local receipts to disappear.

### Phase 4: Multi-system transaction packs

Expand only through connectors with explicit stage, commit, reconciliation and
recovery semantics.

Priority order:

1. Kubernetes deployment, canary health and rollback.
2. PostgreSQL migration staging, invariants and compensation metadata.
3. A combined GitHub + Kubernetes + PostgreSQL transaction.

Every connector must classify effects as read-only, stageable, reversible,
compensatable or irreversible. Unknown non-idempotent effects are reconciled,
never retried blindly.

### Phase 5: Distributed runtime

Broaden the deployment claim only after the single-node semantics remain stable.

- PostgreSQL and object-storage persistence.
- Leases, scheduling, quotas, priorities and admission control.
- Multi-tenant network API and data-plane authentication.
- HA, failover, upgrade and disaster-recovery procedures.
- Data-residency and retention controls.
- Multiple runtime and agent adapters.

Only after these paths are non-bypassable should Vouch describe the deployed
system as an Agent OS.

## Vouch Contracts roadmap

Vouch Contracts remains an optional verification module, not a parallel product
identity. Its priorities should support runtime authority:

- Compile obligations into daemon-owned verifier requirements.
- Bind obligation coverage and evidence provenance into approval packages.
- Import SARIF, coverage, deployment, metrics and rollback evidence.
- Publish schemas and compatibility tests for compiler artifacts.
- Add scoped signer and exception policy.
- Improve source diagnostics and authoring only where real runtime pilots show
  contract-maintenance cost.

The Contracts module does not inspect arbitrary diffs and pronounce code
correct. AI verifiers may contribute evidence but cannot be the sole authority
for high-impact effects.

## Commercial validation

Find teams considering coding or operations agents with write access and ask
for the last workflow they refused to automate. Do not broaden the product
until:

- Three organizations provide a real workflow and its policy constraints.
- Two permit monitor-mode or local-runtime integration.
- One agrees to a paid control-plane or enterprise pilot.
- The buyer values task-level control beyond logs and raw tool approvals.
- Vouch produces measurable permission expansion.

Primary metrics:

- Transactions and material effects mediated before release.
- Permission-expansion events.
- Automatic, focused-approval, revise and block rates.
- False-block rate and approval latency.
- Verification coverage of declared postconditions.
- Unknown, partial-commit, compensation-failure and manual-recovery rates.
- Duplicate non-idempotent effects; target zero.
- Replay mismatch or unattributed effect; target zero.

## Non-goals

Vouch should not claim to:

- Be a general-purpose coding agent or reasoning framework.
- Be an identity provider or credential vault.
- Be only an MCP gateway or per-call policy proxy.
- Prove arbitrary code correct.
- Replace production security review or incident response.
- Offer universal rollback or fictional cross-system ACID semantics.
- Provide production multi-tenancy or HA before those paths exist.
