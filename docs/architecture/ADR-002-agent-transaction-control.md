# ADR-002: Vouch Agent Transaction Control

- Status: accepted for implementation
- Date: 2026-07-23
- Supersedes: the product framing, but not the kernel boundary, in ADR-001
- Owners: Vouch maintainers

## Context

Vouch now has a durable local run kernel, scoped capabilities, action
mediation, and an append-only event history. Those mechanisms answer important
questions about one action at a time: which run requested it, whether a grant
permits it, and whether the driver returned a receipt.

That is necessary but not the product boundary. Harm often emerges from a
sequence of individually permitted actions. An agent can change a security
control, change the evidence used to check it, approve its own result, and then
release it. A per-call policy engine does not establish whether that composed
task should become real.

Identity and request-level authorization are also becoming incumbent
capabilities. Entra Agent ID manages and governs agent identities; Okta exposes
agent registration and lifecycle controls; AWS AgentCore provides identity and
a Cedar policy engine at its gateway; and MCP specifies OAuth-based resource
authorization. Vouch should integrate those systems instead of duplicating
them.

AuthZEN's Authorization and API Access Prerequisites work validates approvals,
consent, delegated authority, attestation, and risk as authorization
prerequisites. The draft explicitly does not define a workflow engine. The
Cordon research prototype separately identifies the missing task-scoped
boundary for staging, validation, commit, rollback, recovery, and audit.

Primary references checked on 2026-07-23:

- <https://learn.microsoft.com/en-us/entra/agent-id/>
- <https://www.okta.com/newsroom/articles/okta-expands-ai-agent-security-to-any-idp/>
- <https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/identity.html>
- <https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/policy.html>
- <https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization>
- <https://openid.net/openid-foundation-advances-authorization-for-the-agent-era-with-new-authzen-working-group-drafts/>
- <https://arxiv.org/abs/2606.17573>

## Decision

Vouch will be the transaction and verification layer between agents and
real-world systems.

Its core protocol is:

```text
declare intent
  -> run inside an isolated transaction boundary
  -> propose and stage effects
  -> evaluate the complete effect sequence
  -> verify declared postconditions and invariants
  -> obtain approval when delegated authority is insufficient
  -> commit releasable effects in an explicit order
  -> verify the committed outcome
  -> compensate, roll back, or require manual recovery on failure
```

Vouch is not an identity provider, model runtime, planner, general workflow
engine, or universal rollback mechanism. It consumes identity and credentials
from existing systems, governs agents from existing runtimes, and represents
partial failure honestly.

The immediate category name is **Agent Transaction Control**. “Agent OS” is the
long-term system description only when Vouch owns a non-bypassable transaction
boundary across the important resources in a workflow.

## Resource boundaries

The following concepts are distinct:

| Resource | Meaning |
| --- | --- |
| `AgentRun` | Durable process-control boundary for an agent execution |
| `AgentTransaction` | Task-scoped authority and business-effect boundary |
| `ActionRequest` | An attempted tool, command, or API invocation |
| `Effect` | A normalized resource change produced or proposed by an action |
| `VerificationResult` | Deterministic evidence about a precondition, invariant, or postcondition |
| `ApprovalPackage` | Exact immutable effect/verification package presented to an approver |
| `CommitPlan` | Ordered release and compensation plan for validated effects |

An action may produce no effects, one effect, or several effects. A transaction
may span several agent runs. The first local implementation may create one run
per transaction for simplicity, but the public model will not encode a 1:1
assumption.

## Transaction outcomes

The authoritative outcomes are:

```text
commit               complete transaction verified and released
approval_required    valid transaction exceeds delegated authority
revise                intended result is possible but proposal is unsafe
block                 organizational invariant is violated
roll_back             committed outcome failed and effects were recovered
partial_commit        only some effects committed; reconciliation is required
manual_recovery       automatic recovery is unsafe or impossible
```

## Effect semantics

Every effect declares one recovery class:

```text
read_only       no external mutation
stageable       can remain private until commit
reversible      has a reliable native rollback
compensatable   requires a distinct semantic counter-action
irreversible    cannot be reliably undone
```

`reversible` and `compensatable` are not synonyms. Compensation may not restore
the exact prior world. Irreversible effects are held in an outbox and approved
before release; Vouch must never imply they can be rolled back.

An effect follows an explicit lifecycle:

```text
proposed -> staged -> validated -> release_ready -> committing -> committed
                    |               |                |
                    +-> blocked     +-> rejected     +-> failed | unknown

committed -> compensating -> compensated
committed -> manual_recovery_required
```

## Evaluation and authority

Vouch evaluates both:

1. Each action and effect against deterministic resource policy.
2. The ordered transaction prefix and complete proposed sequence against
   temporal and separation-of-duty policy.

An LLM may classify or explain ambiguous effects, but cannot be the sole
authority for a high-impact release. Policy inputs, normalized effects,
verification results, approval bindings, and commit decisions remain
machine-readable and replayable.

An approval binds the digest of:

- Transaction intent and constraints.
- Ordered effect set and commit plan.
- Verification results and evidence references.
- Policy version and unresolved warnings.
- Approver identity, authority, decision, and expiry.

Any material change invalidates approval.

## Commit protocol

Vouch coordinates a saga, not a fictional global ACID transaction:

1. Freeze the intent, effect set, policy, verification results, and commit plan.
2. Recheck grants, approvals, and preconditions immediately before release.
3. Commit effects in declared dependency order.
4. Persist a receipt before advancing to the next dependent effect.
5. Verify committed postconditions.
6. On failure, stop release and execute safe compensation in reverse dependency
   order.
7. Surface `partially_committed` or `manual_recovery_required` whenever the
   known state cannot be restored automatically.

Unknown non-idempotent effects are reconciled, never retried blindly.

## Initial product slice

The first transaction implementation governs a local software change:

```text
intent
  -> isolated Git worktree
  -> brokered filesystem changes
  -> normalized Git/file effects
  -> deterministic sequence checks
  -> test and static-analysis verification
  -> immutable approval package
  -> explicit release-ready decision
```

It will not deploy production infrastructure yet. GitHub, PostgreSQL, and
Kubernetes are the first deep multi-system connectors after this local slice is
durable.

## Consequences

Positive:

- The existing run kernel becomes valuable infrastructure rather than a
  commodity identity directory.
- Vouch can govern composed behavior that single-request authorization misses.
- Verification and the existing release-contract engine become central.
- Partial failure and irreversibility are visible before approval.
- The product can integrate across identity providers, clouds, and agent
  frameworks.

Costs:

- Deep connector semantics matter more than connector count.
- Staging and reconciliation require system-specific drivers.
- Approval UX must describe outcomes, not raw tool calls.
- Complete mediation still requires removal of ambient credentials and direct
  production paths.
- Some transactions will require manual recovery.

## Rejected alternatives

### Agent identity directory

Rejected as the core product because identity, discovery, lifecycle, and
credential brokering are already strong incumbent categories. Vouch will adapt
their principals into a common transaction sponsor and agent identity.

### Per-tool allow/deny gateway

Rejected as sufficient because individually allowed actions can compose into a
forbidden sequence.

### Observability and approval dashboard

Rejected because approval after an effect is already real is not control.
Vouch must stage or withhold release at the data-plane boundary.

### Universal rollback promise

Rejected because many external effects are irreversible or only
compensatable. The state model must expose partial commit and manual recovery.
