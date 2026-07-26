# Agent Transaction Threat-Model Delta

- Date: 2026-07-23
- Extends: `THREAT_MODEL.md`
- Architecture: `ADR-002-agent-transaction-control.md`

## Scope

The kernel threat model covers run authority and individual brokered actions.
This delta covers attacks on a task-scoped sequence of staged, verified,
approved, committed, and compensated effects.

## Additional protected assets

- The binding between human intent and the complete effect sequence.
- Staged state that must remain private until commit.
- The ordering and dependencies in a commit plan.
- Before-state snapshots and compensation material.
- Verification evidence and postcondition truth.
- Approval-package integrity and freshness.
- Accurate classification of partial and irreversible outcomes.

## Composition attacks

Threat:

Every action is individually permitted, but their ordered combination violates
separation of duties or an organizational invariant.

Controls:

- Normalize actions into typed effects.
- Evaluate every new effect against the complete ordered transaction prefix.
- Evaluate the frozen complete sequence again before approval and commit.
- Support temporal rules and incompatible-role rules.
- Block self-approval and self-attestation for protected verification classes.

Initial required scenarios:

```text
change security control -> change its test -> release
create vendor -> change bank details -> initiate payment
modify migration -> delete protected data -> deploy
grant permission -> consume newly granted permission in the same transaction
```

## Stage escape and hidden effects

Threat:

An agent writes directly to a real repository, database, cloud API, or network
endpoint while Vouch records only the staged copy.

Controls:

- Remove ambient production credentials from the runtime.
- Give the runtime access to transaction-scoped worktrees, clones, namespaces,
  and credentials only.
- Restrict egress to broker endpoints and explicit read-only sources.
- Compare staged resources and broker receipts to expected effect inventory.
- Mark deployments without complete mediation as monitor-only.

## Control-and-evidence coupling

Threat:

The same agent weakens a control and changes the evidence used to prove that
the control still works.

Controls:

- Detect effects that touch both protected control code and its verification.
- Require independent immutable verification for protected postconditions.
- Record verifier identity, code digest, input digest, environment, and output
  digest.
- Require focused approval when tests or policy checks are weakened, removed,
  or rewritten with their subject.

## Approval time-of-check/time-of-use

Threat:

Effects, dependencies, policy, evidence, or external preconditions change after
approval but before commit.

Controls:

- Approval binds an immutable package digest.
- Commit recomputes and compares the package digest.
- Recheck expiring identity, grants, locks, resource versions, and policy.
- Any material drift returns the transaction to validation or approval.
- Use optimistic version checks or native locks when a connector supports them.

## Verification forgery and weak postconditions

Threat:

The agent supplies passing output that was never produced, ran against the
wrong state, or does not establish the promised outcome.

Controls:

- Execute protected verifiers outside the agent's writable environment.
- Bind evidence to transaction, effect set, verifier, and staged-state digests.
- Distinguish checks that passed from postconditions actually established.
- Treat missing, stale, or unverifiable evidence as a failed prerequisite.
- Never allow model confidence to substitute for deterministic evidence on a
  high-impact effect.

## Incorrect reversibility claims

Threat:

A connector labels an effect reversible even though rollback is lossy,
time-limited, or dependent on unavailable before-state.

Controls:

- Recovery class is connector-owned metadata reviewed with the driver.
- Stage creation validates that required snapshot or compensation material
  exists.
- Commit plans disclose residual recovery limitations.
- Failed or ambiguous compensation ends in `partially_committed` or
  `manual_recovery_required`, never a false `rolled_back` state.

## Outbox bypass for irreversible effects

Threat:

Email, money movement, destructive deletion, or another irreversible effect is
released before verification and approval.

Controls:

- Irreversible drivers implement prepare/hold and explicit release operations.
- The transaction coordinator rejects irreversible effects whose connector
  cannot provide a reliable hold boundary.
- Release requires a fresh approval-package digest and commit authorization.
- The system records whether cancellation is still possible.

## Partial commit and cascading compensation

Threat:

A later effect fails after earlier effects committed; automatic compensation
causes further harm or masks the actual state.

Controls:

- Commit in dependency order and persist each receipt before continuing.
- Stop on first unknown or failed dependent effect.
- Compensate only operations whose plan and preconditions were validated before
  commit.
- Execute compensation in reverse dependency order.
- Preserve both original and compensation receipts.
- Require operator recovery when state cannot be proven.

## Approval manipulation and overload

Threat:

An agent hides material effects in a noisy package or repeatedly requests
approval until a human accepts.

Controls:

- Present outcome-oriented semantic differences and unresolved risks.
- Highlight irreversible effects, expanded authority, changed controls, and
  changed verification.
- Rate-limit repeated or materially equivalent requests.
- Preserve rejects and requested revisions in the transaction history.
- Require a different principal when separation-of-duty policy applies.

## Transaction security invariants

1. No staged effect becomes externally visible without an authorized commit
   transition.
2. The ordered effect set evaluated by policy is byte-identical to the set
   approved and offered to commit.
3. A new effect invalidates prior validation and approval.
4. Protected verification is bound to the exact staged-state digest.
5. Irreversible effects are approved before release, not “rolled back” later.
6. A transaction never reports `committed` until all required postconditions
   pass.
7. A transaction never reports `rolled_back` while any committed effect remains
   unreconciled.
8. Unknown non-idempotent releases are never automatically retried.
9. Self-approval and forbidden control/evidence combinations fail closed.
10. The event and effect history is sufficient to explain the outcome and the
    exact remaining recovery work.

## Required verification

- Sequence-policy tests over allowed and forbidden prefixes.
- Approval invalidation after any material mutation.
- Fault injection before and after every commit receipt.
- Reverse-order compensation tests.
- Irreversible-effect hold and release tests.
- Staged-state digest and verifier-provenance tamper tests.
- Direct-access/bypass tests for every production connector.
- Explicit `partially_committed` and `manual_recovery_required` acceptance
  scenarios.
