# Vouch Agent Kernel Execution Plan (Superseded Product Direction)

> This document records the kernel foundation implemented on 2026-07-23. Its
> original identity-and-control-plane product framing is superseded by
> [`AGENT_TRANSACTION_EXECUTION_PLAN.md`](./AGENT_TRANSACTION_EXECUTION_PLAN.md).
> The canonical hierarchy is now defined by [`README.md`](./README.md) and
> [`ROADMAP.md`](./ROADMAP.md): Vouch Agent OS is the complete architecture,
> Vouch Runtime is the customer-side enforcement product, `vouchd` is its
> kernel, and Vouch Control Plane manages Runtime fleets. The run lifecycle,
> durable event store, capability broker, and connector-driver
> boundary remain required substrate for the transaction product.

## Purpose

Evolve Vouch from a release-contract compiler and evidence gate into the
trusted `vouchd` authority kernel inside Vouch Runtime.

The intended system is not another reasoning framework. Existing agents and
frameworks should continue to own prompting, planning, model calls, and their
internal loops. Vouch should own the authority boundary around those agents:

```text
human intent
  -> execution contract
  -> admitted agent run
  -> scoped capabilities
  -> mediated actions and checkpoints
  -> evidence and policy decisions
  -> pause | constrain | escalate | terminate | release
```

The existing Vouch compiler, obligation IR, evidence linker, release policy,
artifact verification, and gate remain the foundation. Release becomes the
final checkpoint in a longer governed execution lifecycle.

Implementation status as of 2026-07-23:

- Milestone 0 is implemented: kernel boundary, threat model, semantics, eight
  schemas, strict models, fixtures, reducer, and error taxonomy.
- Milestone 1 is implemented locally: SQLite event/projection storage, `vouchd`,
  Unix-socket HTTP API, lifecycle CLI, optimistic concurrency, deterministic
  replay, and all-non-terminal restart tests.
- The first Milestone 2 vertical slice is implemented: execution-contract
  capability compilation, scoped filesystem read/write mediation, durable
  decision/execution/result events, denied path-escape tests, and ambiguous
  action recovery to `unknown` without retry.
- The rest of Milestone 2 and later milestones remain planned. In particular,
  process and external-service connector drivers, authenticated approvals,
  revocation, agent adapters, scheduling, identity and distributed operation
  are not yet complete.

## Product Definition

This historical plan defined kernel-readiness through the following
guarantees:

1. Every agent run has a stable identity, owner, parent lineage, lifecycle,
   budget, contract, and isolated state.
2. Privileged actions are mediated through Vouch and cannot silently bypass its
   capability and policy checks.
3. Runs can pause, survive process or host failure, and resume without blindly
   duplicating side effects.
4. Human authority can be delegated, narrowed, inspected, expired, and revoked.
5. Policy is enforced before and during actions, not only at release time.
6. Every material action and decision is attributable and replayable from an
   append-only event history.
7. More than one agent runtime can use the same Vouch contracts and kernel APIs.
8. Concurrent agents are governed by quotas, priorities, deadlines, and cost
   budgets.
9. Agents, memory, credentials, and workspaces are isolated by default.
10. An operator can inspect, approve, pause, resume, cancel, and recover runs.

Under the current hierarchy, these guarantees produce a credible Vouch Runtime
kernel. The complete Vouch Agent OS additionally requires the Vouch Control
Plane to manage a Runtime fleet.

## North-Star Demonstration

The first complete demonstration should govern one real coding-agent run:

```text
1. A human creates a run from a reviewed execution contract.
2. Vouch admits the run and launches an agent through an agent adapter.
3. The agent requests a permitted workspace write; Vouch authorizes and logs it.
4. The agent requests an out-of-scope write; Vouch denies it before execution.
5. The agent requests a protected action such as push or deployment.
6. Vouch checkpoints and pauses the run for human approval.
7. The Vouch daemon is restarted while the run is paused.
8. The operator approves the action and the run resumes from its checkpoint.
9. Tests and scanners emit evidence against compiled obligations.
10. The existing release gate returns block, escalation, canary, or release.
11. The operator can inspect one history explaining every action and decision.
```

This demonstration is the acceptance thread for the whole program. Each
milestone must make part of it real rather than producing disconnected
infrastructure.

## Architecture

```text
                    Humans / CI / Applications
                               |
                         Vouch Control API
                               |
          +--------------------+--------------------+
          |                 vouchd kernel             |
          |                                           |
          |  Run supervisor      Contract compiler   |
          |  Admission/scheduler Policy evaluator    |
          |  Capability broker   Evidence linker     |
          |  Identity/delegation Release gate        |
          +-------------+------------------+----------+
                        |                  |
                State + event log     Action broker
                        |                  |
         +--------------+------+    +------+------------+
         | Agent adapters      |    | Connector drivers  |
         | CLI / Codex /       |    | filesystem / shell |
         | Claude / SDKs       |    | MCP / GitHub /     |
         | LangGraph / custom  |    | cloud / databases  |
         +---------------------+    +--------------------+
```

### `vouchd` authority and Runtime workload subsystems

Keep these boundaries explicit:

- The `vouchd` authority subsystem stores contracts, runs, capabilities, policy, approvals,
  checkpoints, and decisions.
- The Runtime workload subsystem executes model operations through agent
  adapters; connector drivers perform authorized downstream operations.
- Adapters and agents are untrusted clients of the kernel. They may propose an
  action but may not authorize it.
- The policy evaluator is deterministic. Model output may be evidence or input,
  but it is never the enforcement mechanism.

## Core Resources

Introduce a small set of versioned public resources. Start with JSON and publish
JSON Schemas before adding multiple wire formats.

### `AgentImage`

An immutable, content-addressed definition of an agent:

- Agent adapter and entrypoint.
- Model requirements and permitted providers.
- Instructions, skills, and tool declarations.
- Contract references.
- Dependency and source digests.
- Publisher identity and signature metadata.

### `ExecutionContract`

The human-owned source of authority for a task:

- Goal, non-goals, owner, and risk.
- Permitted resources and action classes.
- Budgets, deadline, and concurrency constraints.
- Runtime invariants and approval checkpoints.
- Evidence and completion obligations.
- Escalation, compensation, and release rules.

The existing release intent should be embedded or referenced rather than
replaced.

### `AgentRun`

The process control block for one execution:

- Run ID, image digest, contract digest, and tenant/namespace.
- Human principal, agent principal, parent run, and delegation chain.
- Lifecycle state, reason, priority, deadline, and budgets.
- Workspace, checkpoint, event cursor, and runtime binding.
- Current capability set and outstanding approvals.

Initial lifecycle states:

```text
created
admitted
running
waiting_for_event
waiting_for_agent
waiting_for_approval
blocked
failed
cancelled
completed
```

### `CapabilityGrant`

A revocable, time-bounded authorization:

- Subject run and delegation chain.
- Resource selector and allowed operations.
- Conditions, quotas, expiry, and maximum uses.
- Whether human approval is required.
- Issuer, policy rule, and signature/provenance.

### `ActionRequest`

The agent's proposed side effect:

- Run and capability reference.
- Typed operation, resource, arguments, and declared intent.
- Idempotency key and expected result type.
- Risk and data-classification context.

### `RunEvent`

The canonical append-only history record:

- Run, sequence, timestamp, actor, and parent event.
- Event type and schema version.
- Input/output artifact references rather than uncontrolled large payloads.
- Policy decision, capability used, hashes, cost, and provenance.
- Previous-event digest for tamper evidence.

### `Checkpoint`

A durable restart boundary:

- Runtime state reference and content digest.
- Last committed event and pending action set.
- Agent/runtime version.
- Memory and workspace snapshot references.
- Resume compatibility metadata.

### `PolicyDecision`

A deterministic answer from the kernel:

```text
allow | deny | require_approval | constrain | pause | terminate
```

It must include fired rule IDs, reasons, evaluated facts, and the policy digest.

## Kernel Invariants

These invariants take priority over feature count:

1. Fail closed when identity, policy, schema, or capability validation fails.
2. Persist an action request and decision before executing the effect.
3. Never record a side effect as committed without a result or reconciliation
   receipt from the driver.
4. After a crash during execution, mark the effect `unknown` and reconcile it;
   do not blindly retry a non-idempotent action.
5. Never expose raw long-lived credentials to the model or agent workspace.
6. A child run cannot receive more authority than its parent can delegate.
7. Revocation applies at the next broker boundary and blocks new effects.
8. Agent adapters cannot write authoritative run or decision state directly.
9. Every public resource is schema-versioned and rejects unknown fields.
10. The event history is sufficient to explain the current run state.

The action state machine should be explicit:

```text
requested
  -> denied
  -> approval_required -> approved | rejected | expired
  -> authorized -> executing -> committed | failed | unknown
```

## What Vouch Owns and What It Integrates

### Vouch should own

- Execution-contract language and compiler.
- Run/process and lifecycle semantics.
- Capability and delegation model.
- Policy inputs, decisions, explanations, and enforcement points.
- Action and event schemas.
- Evidence linkage and release decisions.
- Agent-adapter and connector-driver interfaces.
- Audit history and operator-facing explanation.

### Vouch should integrate

- Model APIs and agent reasoning frameworks.
- Linux/container/microVM sandboxes.
- Durable workflow engines when distributed scale requires them.
- SPIFFE, OIDC, cloud IAM, or equivalent workload identity.
- Vault or cloud secret brokers.
- MCP for tool/resource connectivity.
- A2A for cross-system agent communication.
- PostgreSQL/object storage for production persistence.
- OpenTelemetry for traces, metrics, and logs.

Avoid building a proprietary model abstraction, vector database, container
runtime, secret store, or general workflow engine unless an integration cannot
satisfy a demonstrated kernel invariant.

## Execution Milestones

### Milestone 0: Freeze the contract and kernel boundary

Goal: define the public semantics before writing a daemon.

Deliverables:

- Architecture decision record for `vouchd` authority versus Runtime workload
  and connector subsystems.
- Threat model covering malicious prompts, agents, tools, adapters, evidence,
  and operators.
- JSON Schemas for the eight core resources.
- Lifecycle and action transition tables.
- Error taxonomy and stable machine-readable error codes.
- Compatibility policy for all `vouch.*.v0` resources.
- A checked-in north-star demonstration specification.

Exit criteria:

- Every north-star step maps to a resource, transition, and enforcement point.
- The threat model identifies all trusted components and bypass paths.
- Golden fixtures validate allowed and forbidden state transitions.
- Existing release-contract inputs compile without semantic regression.

### Milestone 1: Durable local run kernel

Goal: make an agent run a durable first-class object on one machine.

Deliverables:

- `vouchd`, initially exposed over a Unix socket and local HTTP API.
- `RunStore` and `EventStore` interfaces.
- A transactional SQLite implementation for local development.
- Append-only run events and materialized run state.
- Commands for create, inspect, list, pause, resume, cancel, and events.
- Deterministic lifecycle reducer: current state is derived from accepted events.
- Recovery on daemon restart.

Suggested CLI:

```text
vouch daemon
vouch image register FILE
vouch run create --image IMAGE --contract CONTRACT
vouch run get RUN_ID
vouch run events RUN_ID
vouch run pause|resume|cancel RUN_ID
```

Exit criteria:

- A run survives daemon restart in every non-terminal lifecycle state.
- Illegal transitions are rejected deterministically.
- Replaying events produces byte-equivalent materialized state.
- Concurrent state updates use optimistic concurrency or transactions rather
  than last-write-wins behavior.

### Milestone 2: Capability and action broker

Goal: establish complete mediation for a small but meaningful action surface.

Deliverables:

- Capability compiler from `ExecutionContract` to initial grants.
- Policy enforcement before every brokered action.
- Typed drivers for filesystem read/write and process execution.
- Per-run workspace roots and path containment.
- Deadlines, call quotas, output limits, and environment allowlists.
- Action receipts with arguments/results stored as hashed artifacts.
- Approval-required decisions and revocation.

Do not begin with arbitrary MCP passthrough. First prove that Vouch can safely
mediate two local drivers with strict typed schemas.

Exit criteria:

- An out-of-scope filesystem write has no side effect.
- A revoked capability cannot authorize a new action.
- A child run cannot acquire a capability outside its delegation ceiling.
- Environment variables and secrets not explicitly granted never reach the
  process driver.
- Every attempted action appears in the event history, including denials.

### Milestone 3: Agent adapter protocol

Goal: govern agents without owning their reasoning loop.

Deliverables:

- Versioned adapter protocol for start, input, event, checkpoint, resume,
  cancel, and health.
- Reference subprocess adapter for arbitrary local agents.
- One real coding-agent adapter.
- Adapter conformance suite and failure simulator.
- Heartbeats, leases, orphan detection, and zombie reaping.
- Explicit adapter trust boundary: adapters submit observations and requests,
  while the kernel owns authoritative state.

Exit criteria:

- The same execution contract runs through two adapters.
- Killing an adapter produces a recoverable or explicitly failed run, never a
  silently running state.
- A malformed or hostile adapter cannot forge a policy decision or committed
  action receipt.
- Adapter version and agent image digest are recorded on every run.

### Milestone 4: Checkpoints, approvals, and safe recovery

Goal: make long-running, interruptible operation reliable.

Deliverables:

- Checkpoint storage and compatibility validation.
- Durable approval queue with approve, reject, modify, expire, and revoke.
- Human identity bound to approval decisions.
- Idempotency keys and driver-specific reconciliation.
- Timers, waits, deadlines, and timeout policy.
- Compensation hooks for reversible operations.

Exit criteria:

- The north-star run pauses, survives daemon restart, and resumes after approval.
- A non-idempotent effect is never automatically repeated after ambiguous
  failure.
- Expired approval and capability grants fail closed.
- Operators can see exactly what effect they are authorizing before approval.

### Milestone 5: Continuous contracts, evidence, and release

Goal: extend Vouch's strongest existing feature across the entire run.

Deliverables:

- Lower `ExecutionContract` into capability, checkpoint, evidence, and release
  obligations.
- Evaluate policy on admission, delegation, action, checkpoint, completion, and
  release events.
- Link action receipts and runtime events to stable obligation IDs.
- Reuse existing artifact hashing, signed evidence, findings, and release gate.
- Add policy simulation over a historical event cursor.
- Explain why a run is allowed, waiting, constrained, blocked, or releasable.

Exit criteria:

- Every gate decision can cite the execution events and artifacts satisfying its
  obligations.
- Replaying the same history with the same policy produces the same decisions.
- A policy update can be simulated against completed runs without mutating them.
- Existing VouchBench release scenarios continue to pass.

### Milestone 6: Scheduling, budgets, and concurrency

Goal: govern multiple competing runs as a system.

Deliverables:

- Admission control for tenant, model, cost, tool, and workspace quotas.
- Priorities, deadlines, concurrency limits, and cancellation propagation.
- Token, monetary, model-call, tool-call, and wall-clock budgets.
- Backpressure and rate-limit-aware queues.
- Fair scheduling across namespaces.
- Run and system health metrics.

Exit criteria:

- A run cannot exceed a hard budget through retries or child delegation.
- High-volume tenants cannot starve other admitted tenants.
- Cancellation propagates to adapters, child runs, and pending broker calls.
- Zombie and lease-expired runs are detected and reconciled.

### Milestone 7: Identity and protocol interoperability

Goal: connect the kernel safely to external ecosystems.

Deliverables:

- OIDC/workload-identity binding for agents and operators.
- Short-lived, audience-bound credentials issued through a broker.
- MCP driver with per-server and per-tool capabilities.
- A2A adapter for discovery, task delegation, status, and artifact exchange.
- Signed inter-agent messages and preserved delegation chains.
- OpenTelemetry-compatible agent, workflow, tool, policy, and cost telemetry.

Exit criteria:

- No static downstream credential is exposed to the agent.
- MCP and A2A calls are authorized at individual operation boundaries.
- Remote actions retain human, parent-agent, and child-agent attribution.
- Cross-protocol traces correlate to the same Vouch run and event sequence.

### Milestone 8: Distributed production Runtime

Goal: make the proven kernel operable by teams.

Deliverables:

- PostgreSQL-backed stores and object storage.
- Namespaces, tenants, roles, quotas, and retention policies.
- High-availability API and supervisor processes with leases.
- Schema migrations and rolling-upgrade compatibility.
- Operator dashboard for runs, approvals, capabilities, costs, and evidence.
- Backup, restore, disaster recovery, and audit export.
- Policy packs and organization-level defaults.

Exit criteria:

- Runtime failover does not lose acknowledged events or duplicate known
  committed effects.
- Tenant isolation is covered by automated authorization tests.
- An operator can diagnose and recover a stuck run without database surgery.
- Upgrade, downgrade boundary, backup, and restore procedures are exercised in
  acceptance tests.

## First Implementation Slice

Implement this before broad integrations:

1. Add schemas and Go types for `AgentImage`, `AgentRun`,
   `ExecutionContract`, `RunEvent`, `CapabilityGrant`, `ActionRequest`,
   `Checkpoint`, and `PolicyDecision`.
2. Add a pure lifecycle reducer with table-driven transition tests.
3. Add SQLite-backed append-only event storage and materialized run state.
4. Add `vouch daemon`, `vouch run create`, `vouch run get`, and
   `vouch run events`.
5. Add one filesystem driver supporting scoped read and write actions.
6. Compile owned-path rules into filesystem capabilities.
7. Log request, decision, execution, and result events with an event hash chain.
8. Demonstrate an allowed workspace write and denied path escape.
9. Restart the daemon and prove the state and history recover exactly.
10. Add the scenario to a new `VouchKernelBench` acceptance harness while
    keeping VouchBench unchanged.

This slice proves the process, persistence, capability, action-broker, and audit
model without prematurely adding a scheduler, remote agent protocol, or cloud
deployment.

## Repository Evolution

Avoid a large package rewrite at the start. Add the kernel vertically, then
extract packages when boundaries are proven.

Proposed initial layout:

```text
cmd/vouch/                 existing CLI plus run commands
cmd/vouchd/                local daemon entrypoint
internal/vouch/            existing compiler, evidence, and release runtime
internal/kernel/model/     public kernel resource types and validation
internal/kernel/reducer/   lifecycle and action state reducers
internal/kernel/store/     event/run store interfaces and SQLite adapter
internal/kernel/policy/    admission and action policy boundary
internal/kernel/broker/    action mediation and driver dispatch
internal/kernel/runtime/   agent adapter protocol and supervision
internal/kernel/drivers/   filesystem/process, later MCP and remote drivers
schemas/                   versioned JSON Schemas and compatibility fixtures
```

Dependency direction:

```text
kernel model/reducer
        ^
        |
store, policy, broker, runtime
        ^
        |
daemon and CLI
```

The kernel may call the existing Vouch compiler and evidence APIs through narrow
interfaces. The compiler should not depend on daemon, storage, or adapter code.

## Verification Strategy

### Unit and property tests

- All legal and illegal lifecycle transitions.
- Capability containment and delegation ceilings.
- Path, environment, and resource selector normalization.
- Event hash-chain validation.
- Budget monotonicity: spending and delegated authority cannot increase after
  accounting errors or replay.
- Deterministic policy evaluation and explanation.

### Crash and fault tests

Inject failure:

- Before persisting an action request.
- After authorization but before driver execution.
- During driver execution.
- After effect completion but before receipt persistence.
- During checkpoint creation.
- While waiting for approval.
- During daemon and adapter restart.

Every point must resolve to a known recoverable state or `unknown` requiring
reconciliation.

### Security tests

- Path traversal and symlink escape.
- Environment and credential leakage.
- Forged adapter events and receipts.
- Capability replay, expiry, and revocation.
- Child-agent privilege escalation.
- Cross-tenant resource access.
- Prompt-driven attempts to bypass the broker.
- Untrusted tool output entering memory or policy input.

### Acceptance harnesses

Keep two separate claims:

- `VouchBench`: release contracts, evidence, traceability, and policy routing.
- `VouchKernelBench`: run durability, action mediation, delegation, budgets,
  approvals, and recovery.

Do not claim agent-task quality from either harness. They validate kernel
authority behavior, not model intelligence.

## Metrics

Track metrics that reveal whether `vouchd` is functioning as an authority
kernel:

- Percentage of privileged actions mediated by the broker.
- Unauthorized actions prevented before side effects.
- Runs recovered successfully after injected failure.
- Duplicate non-idempotent effects after recovery; target zero.
- Policy decision latency and broker overhead.
- Runs requiring human approval and approval wait time.
- Capability overgrant and unused-grant rates.
- Budget overruns; target zero for hard budgets.
- Unattributed events or artifacts; target zero.
- Event replay and materialized-state mismatches; target zero.
- Operator time to explain and recover a blocked run.

Model answer quality, benchmark score, and task completion rate matter to the
agent application, but they are not the primary kernel correctness metrics.

## Scope Guards

Do not let the program turn into:

- A generic agent framework or prompt library.
- A generic code-review agent.
- A proprietary replacement for MCP or A2A.
- A new container, workflow, identity, telemetry, or secret-management system.
- A dashboard-first product without a trustworthy enforcement path.
- A policy layer that agents can bypass by invoking tools directly.
- A distributed platform before the local durability and action invariants are
  proven.

## Decision Gates

At the end of each milestone, answer:

1. Which new authority boundary is now enforced?
2. Can an untrusted agent or adapter bypass it?
3. What state survives a crash, and what can be replayed safely?
4. Which human-owned contract or policy governs the behavior?
5. Is every decision and effect attributable?
6. Does the north-star demonstration now cover another real step?
7. Did the existing compiler/evidence/release behavior remain compatible?

If a milestone adds orchestration features without strengthening an authority,
durability, isolation or audit boundary, it is not moving Vouch Runtime toward
the complete Vouch Agent OS architecture.

## Immediate Next Actions

1. Review and approve the product definition and ten OS guarantees in this
   document.
2. Write the kernel-authority/Runtime-workload architecture decision record.
3. Write the threat model before choosing daemon or adapter APIs.
4. Specify the eight versioned resources and their lifecycle invariants.
5. Create golden fixtures for the north-star run.
6. Implement the lifecycle reducer and event store.
7. Deliver the first filesystem-broker demonstration.

The first engineering goal is not “run many agents.” It is “prove that one
untrusted agent can be durably governed through one complete, non-bypassable
action and recovery path.”
