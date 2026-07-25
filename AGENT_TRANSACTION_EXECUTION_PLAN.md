# Vouch Agent Transaction Control Execution Plan

> Historical engineering plan and milestone record. The dated implementation
> snapshots below are intentionally preserved; use [`README.md`](./README.md)
> and [`ROADMAP.md`](./ROADMAP.md) for the current product boundary and status.

## Product thesis

Vouch is the transaction and verification layer for autonomous agents:

> An agent proposes a task-scoped transaction. Vouch stages its effects,
> evaluates the complete sequence, verifies the intended outcome, obtains any
> necessary approval, commits the effects, and coordinates recovery when the
> outcome fails.

The immediate category is **Agent Transaction Control**. The product is not an
identity directory, another agent framework, an MCP-only gateway, or a generic
allow/deny policy proxy. Identity providers and cloud policy engines remain
integrations beneath or beside Vouch.

The long-term “agent OS” claim becomes credible when the system provides a
non-bypassable transaction boundary across the important resources in an
agent's workflow. Until then, call the implementation a transaction runtime or
transaction kernel.

## Current foundation

Implemented locally on 2026-07-23:

- Strict public resources for agents, contracts, runs, grants, actions, events,
  checkpoints, and policy decisions.
- Deterministic run reducer and append-only, hash-chained event history.
- Transactional SQLite event/projection persistence and restart recovery.
- `vouchd` over a mode-`0600` Unix socket.
- Capability compilation and traversal-resistant filesystem mediation.
- Persist-before-execute action audit and unknown-without-retry recovery.
- VouchKernelBench acceptance coverage.

These remain the process, authority, durability, and mediation substrate for
the transaction work summarized below.

Implementation update on 2026-07-23:

- Phase T0 resource schemas, semantic validation, event envelope, transaction
  and effect reducers, approval binding, partial-commit truth states, and
  baseline sequence policy are implemented locally.
- The first Phase T1 vertical is implemented: daemon-owned detached Git
  worktrees, exact diff-to-effect normalization, immutable stage/effect digests,
  atomic SQLite event batches, restart replay, and `vouch tx` lifecycle CLI.
- VouchTransactionBench passes 10/10 through the real daemon and Git boundary.
- Isolated verifier execution, authenticated approvals, commit connectors,
  compensation execution, and production non-bypassability are not implemented
  yet.

## Product boundary

Vouch owns:

- Signed task intent and constraints.
- Transaction and effect state machines.
- Action-to-effect normalization.
- Ordered sequence and separation-of-duty policy.
- Staging boundaries and outboxes through deep connectors.
- Verification orchestration and evidence binding.
- Outcome-oriented approval packages.
- Commit ordering, receipts, reconciliation, and compensation.
- Evidence graph from sponsor to committed outcome.

Vouch integrates:

- Entra, Okta, IAM, OIDC, and workload identity.
- Agent frameworks and coding agents.
- Cedar or another deterministic language for ordinary resource policy.
- GitHub, PostgreSQL, and Kubernetes as the first deep connectors.
- Skylos, tests, builds, scanners, and deployment health as verifiers.
- MCP, SDK, HTTP, shell, and filesystem interception paths.

## North-star transaction

```text
Human intent: upgrade a service without changing authentication behavior
  -> create transaction and isolated Git worktree
  -> agent changes code and generates a migration
  -> actions become normalized code, test, and schema effects
  -> sequence policy notices authentication code and tests changed together
  -> build, tests, Skylos, schema diff, and behavioral checks run
  -> approval package explains verified outcomes and unresolved risks
  -> approved commit plan releases GitHub change, migration, then canary
  -> production postconditions pass and transaction commits
  -> or rollout stops and recovery reaches rolled_back / partial_commit /
     manual_recovery with exact remaining work
```

The engineering acceptance thread begins locally and expands without changing
the public transaction semantics.

## Core resources

### `AgentTransaction`

- Transaction ID, namespace, sponsor, and participating agent runs.
- Signed intent and constraints digest.
- State, reason, timestamps, and event cursor.
- Ordered effect IDs and outstanding approvals.
- Frozen validation, approval-package, and commit-plan digests.
- Outcome and recovery status.

### `Effect`

- Originating action and run.
- Typed system, resource, operation, and normalized arguments.
- Sequence, dependencies, estimated scope, and data classification.
- Recovery class and required before-state/compensation references.
- Stage, validation, release, commit, and reconciliation status.
- Idempotency key, receipt, and before/after digests.

### `VerificationResult`

- Named precondition, invariant, or postcondition.
- Exact transaction and staged-state digest.
- Verifier identity, implementation digest, inputs, result, and evidence.
- Independence class: agent-supplied, platform-run, or external attestation.

### `ApprovalPackage`

- Immutable digest over intent, ordered effects, commit plan, policy, and
  verification.
- Human-readable semantic summary and structured risk findings.
- Required approval classes, decisions, expiries, and separation-of-duty rules.

### `CommitPlan`

- Frozen ordered effect releases and dependency graph.
- Per-effect preconditions, idempotency, timeout, and receipt requirements.
- Compensation steps in reverse dependency order.
- Explicit irreversible effects and manual recovery playbooks.

## State machines

Transaction lifecycle:

```text
created -> running -> staged -> validating
validating -> validation_failed | revise_required | blocked
validating -> pending_approval | ready_to_commit
pending_approval -> ready_to_commit | revise_required | blocked
ready_to_commit -> committing
committing -> committed | compensating | partially_committed
compensating -> rolled_back | partially_committed | manual_recovery_required
```

An effect is never silently “done.” It advances through proposed, staged,
validated, release-ready, committing, committed, and, when necessary,
compensating/compensated or unknown/manual-recovery states.

## Engineering phases

### Phase T0: Freeze transaction semantics

Deliver:

- ADR-002 and transaction threat-model delta.
- Strict schemas and Go models for the five core transaction resources.
- Deterministic transaction and effect reducers.
- Transition fixtures for commit, block, rollback, partial commit, and manual
  recovery.
- Explicit terminology separating runs, actions, effects, and transactions.

Exit criteria:

- Illegal state changes fail closed.
- Adding or mutating an effect invalidates validation and approval.
- `committed`, `rolled_back`, and `partially_committed` have testable meanings.
- Replay produces byte-equivalent projections.

### Phase T1: Local staged software transaction

Deliver:

- Transaction CLI/API create, inspect, list, stage, validate, and abort.
- Isolated Git worktree manager without changing the caller's active worktree.
- Brokered filesystem effects normalized into an effect ledger.
- Frozen staged-state digest and semantic Git diff.
- Deterministic temporal policy interface and initial code safety rules.
- VouchTransactionBench acceptance harness.

Exit criteria:

- A coding agent cannot write outside the transaction worktree through Vouch.
- Source changes remain private until an explicit release step.
- The effect ledger exactly matches the staged diff.
- A protected control-and-test change triggers focused scrutiny.
- Restart reconstructs the same transaction and effect projection.

### Phase T2: Verification-backed release

Deliver:

- Verifier SDK and isolated verifier runner.
- Test, build, Skylos, dependency, and semantic-diff verifiers.
- Postcondition declaration and evidence binding.
- Immutable approval packages and authenticated decisions.
- GitHub App with staged PR/update/merge release semantics.
- Signed transaction evidence bundle.

Exit criteria:

- Verification binds to the exact staged-state digest.
- Mutating staged state invalidates results and approvals.
- Protected approval cannot be supplied by the acting agent.
- A GitHub effect is not released until its plan is current and authorized.

### Phase T3: Deep three-system transaction

Deliver one complete connector pack:

```text
GitHub + PostgreSQL + Kubernetes
```

- GitHub branch/PR/merge staging and reconciliation.
- PostgreSQL clone, migration dry-run, invariant checks, transaction where
  possible, snapshot/compensation metadata otherwise.
- Kubernetes isolated namespace, manifest diff, canary, health verification,
  traffic progression, and rollback.
- Cross-system commit dependency graph and receipts.
- Fault injection at every release boundary.

Exit criteria:

- One customer grants an agent a permission it previously refused to grant.
- A failure after any release step produces a truthful committed, rolled-back,
  partially-committed, or manual-recovery outcome.
- No unknown non-idempotent action is automatically repeated.

### Phase T4: Enterprise control plane

Deliver:

- Self-hosted data plane and highly available central control plane.
- Entra/Okta/IAM identity adapters, SSO, RBAC, and approval delegation.
- Policy versioning/simulation, SIEM export, evidence retention, and audit
  bundles.
- Credential brokering, restricted egress, emergency revocation, and data
  residency controls.
- Multiple coding-agent and runtime adapters.

Exit criteria:

- Production paths are non-bypassable under the documented deployment model.
- Control-plane failover neither loses acknowledged receipts nor duplicates
  known effects.
- Operators can recover stuck transactions without database surgery.

### Phase T5: Vertical transaction packs

Only after software transactions work deeply, expand to:

- Cloud and incident operations.
- Customer operations and refunds.
- Finance, vendor, procurement, and payment workflows.

Each pack requires connector-owned effect/recovery semantics, sequence-policy
templates, verifiers, approval UX, and recovery playbooks. Connector count is
not a success metric.

## Commercial validation running beside engineering

Find five teams considering coding or DevOps agents with write access. Ask for
the last workflow they refused to automate. Continue beyond the local product
only when:

- Three organizations provide a real workflow and policy.
- Two allow monitor-mode integration.
- One agrees to a paid pilot.
- The buyer values task-level verification beyond logs and raw approvals.
- Vouch unlocks additional agent authority.

The primary value metric is permission expansion: the customer lets an agent
safely complete work it previously could only suggest.

## Immediate implementation slice

Execute in this order:

1. Add strict transaction schemas, types, validation, and fixtures.
2. Add pure transaction/effect reducers and sequence-policy interface.
3. Implement isolated Git worktree creation and inspection.
4. Convert filesystem/Git changes into normalized staged effects.
5. Persist transaction events and projections beside the run event store.
6. Expose `vouch tx create|get|effects|validate|abort` locally.
7. Generate a deterministic approval-package preview.
8. Add restart, tamper, sequence-attack, partial-commit, and stage-escape tests.
9. Add VouchTransactionBench without weakening existing suites.

No source-repository commit, push, merge, deployment, or irreversible action is
performed automatically. Those become explicit connector release operations
only after transaction semantics, approval binding, and recovery are proven.

## Metrics

- Percentage of material effects staged before release.
- Effect inventory coverage: broker effects versus actual resource diff.
- Automatic-commit, focused-approval, revise, and block rates.
- False-block rate and median human review time.
- Verification coverage by declared postcondition.
- Unknown, partial-commit, compensation-failure, and manual-recovery rates.
- Duplicate non-idempotent releases; target zero.
- Permission-expansion events at design partners.
- Replay/projection mismatches and unattributed effects; target zero.

## Scope guards

- Do not compete on identity directories, generic credential vaulting, or
  single-call allow/deny policy.
- Do not call telemetry an enforcement boundary.
- Do not depend on MCP as the only interception path.
- Do not claim universal rollback or distributed atomicity.
- Do not allow an LLM to be the sole high-impact authority.
- Do not add shallow connectors before GitHub, PostgreSQL, and Kubernetes work
  deeply.
- Do not claim a production agent OS while agents retain direct production
  credentials or unrestricted egress.
