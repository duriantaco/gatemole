# Gatemole Agent Kernel Threat Model

## Scope

This threat model covers the agent-kernel architecture described in
`ADR-001-agent-kernel-boundary.md`. It focuses on kernel authority,
resource mediation, durable execution, evidence, and audit.

The model does not claim that Gatemole can make an arbitrary model correct. It
defines how the system limits, attributes, pauses, and recovers actions even
when reasoning is wrong or hostile.

Current enforcement status: the local kernel mediates typed filesystem reads
and writes beneath a per-run workspace using Go's rooted filesystem API. It
does not yet sandbox an agent process, mediate process/network access, or
authenticate multiple socket users. An agent with direct host access can bypass
this first driver and remains outside the complete-mediation claim.

## Protected assets

- Human and service authority delegated to an agent run.
- Source code, repositories, filesystems, and workspaces.
- Credentials, tokens, signing keys, and identity documents.
- External systems reached through connector drivers.
- Contracts, policies, capability grants, and approval decisions.
- Run state, checkpoints, memory, evidence, and artifacts.
- Event-history integrity and attribution.
- Token, monetary, time, compute, and human-attention budgets.
- Tenant and namespace isolation.

## Trust boundaries

### Trusted computing base

Initially trusted:

- Kernel resource validation and lifecycle reducer.
- Event and materialized-state transaction boundary.
- Capability resolution and policy evaluator.
- Action broker and trusted driver code.
- Local host isolation used to protect `gatemoled` and its store.
- Operator identity provider and configured trust roots.

These components are still treated as fallible and must emit enough state for
diagnosis and recovery.

### Untrusted or partially trusted

- Model output and generated code.
- Agent reasoning loops.
- Agent adapters.
- Child and remote agents.
- MCP/A2A servers and external tools.
- Web pages, repository contents, messages, retrieved documents, and memory
  candidates.
- Evidence supplied by the run until provenance and artifact checks succeed.
- Connector-driver responses until validated and recorded.
- Human-authored inputs that do not carry authenticated authority.

### Administrative trust

Operators may authorize exceptional actions, but their identity, decision,
scope, and expiry are recorded. An approval is not a blanket transfer of future
authority.

## Attacker models

1. A prompt-injected agent tries to exceed its task.
2. A compromised agent adapter forges events or claims an action succeeded.
3. A malicious tool returns instructions intended to change the agent's goal.
4. A child agent attempts to escalate beyond its parent's delegation.
5. A tenant tries to read another tenant's run, memory, or credentials.
6. An attacker replays an expired capability or prior approval.
7. An external service receives a duplicated non-idempotent request after a
   crash.
8. An operator account is compromised or abuses emergency authority.
9. A compromised artifact or checkpoint is substituted during resume.
10. A denial-of-service actor exhausts model, tool, storage, or approval queues.

## Threats and required controls

### Goal hijacking and prompt injection

Threat:

Untrusted content persuades the model to ignore the execution contract or call
a dangerous tool.

Controls:

- Contracts and capabilities are kernel state, not prompt-only instructions.
- Every material action is authorized independently of model justification.
- Resource and operation scopes are explicit and fail closed.
- Tool output is labeled untrusted before entering memory or policy facts.
- High-impact action classes require durable human approval.

Residual risk:

The agent may still produce poor in-scope work. Evidence obligations and release
policy address advancement, not arbitrary correctness.

### Ambient authority and credential theft

Threat:

An agent discovers a token in environment variables, files, process state, or
tool configuration and bypasses the broker.

Controls:

- No long-lived downstream credentials in the agent process or workspace.
- Drivers obtain short-lived, audience-bound credentials after authorization.
- Environment allowlists and per-run workspaces.
- Network egress restrictions aligned with granted resource classes.
- Secret values are never persisted in event payloads.

Residual risk:

Local development without sandboxing cannot provide complete mediation and
must identify itself as advisory.

### Capability escalation and confused deputy

Threat:

A child run, remote agent, or tool induces a more privileged component to act
outside delegated authority.

Controls:

- Delegated grants are intersections of parent authority and contract policy.
- Every action carries subject, actor, resource, operation, audience, and
  delegation chain.
- Drivers bind credentials to the authorized resource and operation.
- Run and tool identities are distinct.
- Revocation and expiry are checked at the broker boundary.

### Forged events, receipts, and evidence

Threat:

An adapter claims an action ran, fabricates passing evidence, rewrites history,
or substitutes an artifact.

Controls:

- Only the kernel appends authoritative events.
- The generic local event endpoint rejects privileged capability and action
  event types; those events can only be produced by mediated kernel handlers.
- Drivers return typed receipts through authenticated local or remote channels.
- Event sequence numbers and previous-event digests detect deletion or
  reordering.
- Artifacts, checkpoints, policies, contracts, and images are content-addressed.
- Existing Gatemole artifact-path, hash, signer, and provenance validation remains
  in the release path.

### Crash ambiguity and duplicated effects

Threat:

The daemon or driver crashes after performing an external effect but before
persisting its receipt. Automatic retry duplicates payment, deployment, push,
or message operations.

Controls:

- Persist request and authorization before driver execution.
- Require idempotency keys where the downstream system supports them.
- Record `executing` before dispatch and `committed` only after a valid receipt.
- On ambiguous recovery, transition to `unknown` and invoke driver-specific
  reconciliation.
- Never automatically retry an unknown non-idempotent action.

The local daemon implements this fail-closed startup rule: an interrupted
request that never reached authorization is denied, while an interrupted
authorized or executing action receives an `action.unknown` event and is not
retried.

### Checkpoint and memory poisoning

Threat:

Malicious or stale state changes future reasoning or grants access across
namespaces.

Controls:

- Checkpoints bind run, image, contract, runtime version, and event cursor.
- Content digests are verified before resume.
- Memory has namespace, provenance, retention, and trust labels.
- Cross-run memory attachment requires a capability and policy decision.
- Retrieved content cannot directly mutate authoritative contract or capability
  state.

### Approval spoofing and stale approval

Threat:

An unauthenticated message, approval for a different action, or expired human
decision is reused.

Controls:

- Approval binds approver identity, exact action digest, scope, and expiry.
- Modified action arguments invalidate the approval.
- Approval is single-use unless policy explicitly states otherwise.
- Reject and revoke are durable events.
- The operator sees normalized effect details before deciding.

### Cross-tenant access

Threat:

Identifiers, paths, event queries, or artifact references expose another
tenant's data.

Controls:

- Tenant and namespace are part of every resource key and authorization input.
- Storage queries require tenant predicates by construction.
- Artifact namespaces and encryption keys are isolated.
- IDs are not treated as authorization secrets.
- Cross-tenant tests cover control API, store, driver, and audit exports.

### Resource exhaustion and cascading failure

Threat:

Agents recursively spawn work, retry rate-limited tools, grow context, or flood
approval queues.

Controls:

- Admission control and hard parent-inclusive budgets.
- Maximum child depth, fan-out, concurrency, and action rate.
- Deadlines, leases, cancellation propagation, and zombie reaping.
- Backpressure before work enters model or tool queues.
- Bounded event/artifact payloads and retention policies.

### Policy rollback or incompatible resume

Threat:

A run resumes under incompatible code or policy and silently changes meaning.

Controls:

- Checkpoints pin agent image, adapter, contract, and policy digests.
- Resume performs compatibility validation.
- Policy changes are simulated against prior history before activation.
- Forced migration is an explicit, attributed operator decision.

## Security invariants

1. Denied actions produce no broker-mediated side effect.
2. Child authority is never greater than the delegable parent authority.
3. Terminal run state cannot be reopened by an ordinary lifecycle event.
4. A capability is unusable after expiry, revocation, exhaustion, or subject
   mismatch.
5. An approval authorizes only its exact normalized action digest.
6. Unknown non-idempotent actions require reconciliation or human resolution.
7. Event history cannot be silently reordered without invalidating its chain.
8. Secrets never appear in model context, event payloads, or logs by default.
9. Artifact and checkpoint content must match recorded digests before use.
10. Materialized run state must equal deterministic event replay.

## Out of scope for the first local kernel

- Protection from an administrator with host root access.
- Strong tenant isolation without a sandbox and production identity system.
- Confidential-computing guarantees.
- Detection of every malicious but contract-compliant action.
- Correctness of third-party identity, secret, sandbox, or storage systems.

The local kernel must label these limitations rather than imply production
isolation.

## Required security verification

- Table-driven authorization and lifecycle tests.
- Path traversal, symlink escape, and environment leakage tests.
- Capability expiry, replay, revocation, and delegation property tests.
- Fault injection at every action persistence boundary.
- Tampered event, checkpoint, artifact, policy, and image fixtures.
- Hostile adapter conformance tests.
- Cross-namespace storage and API tests before multi-tenancy ships.
- Red-team scenarios for prompt injection, memory poisoning, tool output, and
  child-agent escalation.
