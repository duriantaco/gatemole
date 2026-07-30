# ADR-001: Gatemole Agent Kernel Boundary

- Status: accepted for implementation
- Date: 2026-07-23
- Owners: Gatemole maintainers

## Context

Gatemole currently compiles human-owned release intent into typed specifications,
stable obligations, verification plans, evidence requirements, and release
decisions. That runtime begins near the end of an agent-authored change.

Moving toward the Gatemole Agent OS requires the customer-side Runtime to govern
the execution that produces the change without becoming the component that
reasons about how to implement it. The architectural risk is collapsing four
separate concerns into one service:

- Agent reasoning and planning.
- Durable execution and scheduling.
- Authorization and side-effect mediation.
- Contract, evidence, and release policy.

The authority boundary must remain deterministic even when the agent, model,
tool, agent adapter, or external data is malicious or simply wrong.

## Decision

Gatemole Runtime will contain the `gatemoled` user-space kernel inside a
customer-side enforcement boundary. It will run above a host operating system
and integrate with existing agent frameworks, sandboxes, identity systems,
durable execution engines and protocols. The organization-wide Gatemole Control
Plane will manage Runtime fleets; it is not the local kernel described by this
ADR.

Gatemole owns:

- Execution-contract compilation.
- Agent run identity, lineage, and lifecycle state.
- Capability and delegation semantics.
- Admission and action policy decisions.
- Side-effect mediation through typed connector drivers.
- Checkpoint, event, evidence, and audit semantics.
- Human approval as a durable kernel operation.
- Explanation of why work may advance.

Gatemole does not own:

- Model inference or prompting strategy.
- The agent's planner or reasoning loop.
- Container, microVM, or operating-system isolation implementations.
- A general workflow engine.
- Workload identity, secret storage, or telemetry backends.
- MCP, A2A, or model-provider protocols.

Those systems connect through adapters. An adapter may report observations and
request actions; it cannot create authoritative decisions, capabilities, or
committed action receipts.

## `gatemoled` authority subsystem

The local `gatemoled` authority subsystem is authoritative for:

- `AgentImage`
- `ExecutionContract`
- `AgentRun`
- `CapabilityGrant`
- `PolicyDecision`
- `Checkpoint`
- `RunEvent`
- Human approvals
- Materialized run state

Authority-plane writes occur through validated APIs. Each accepted change emits
an append-only event. Materialized state is a projection that must be
reconstructable from the accepted event sequence.

## Runtime workload and connector subsystems

The Runtime workload and connector subsystems contain:

- Agent adapters that host or connect to agent loops.
- Connector drivers that perform typed filesystem, process, MCP, GitHub,
  database, or cloud operations.
- Sandboxes and per-run workspaces.
- Artifact and checkpoint storage.

Agent sandboxes and adapters are untrusted from the kernel's perspective.
Connector drivers execute privileged operations and therefore belong to the
documented Runtime trust boundary.

## Enforcement sequence

Every privileged action follows the same sequence:

```text
agent or adapter
  -> ActionRequest
  -> schema and identity validation
  -> capability resolution
  -> policy evaluation
  -> deny | require approval | authorize
  -> persist decision
  -> connector driver execution
  -> persist receipt or mark result unknown
  -> link evidence and update run projection
```

Persisting the authorization before execution makes the decision auditable.
Persisting the receipt after execution distinguishes intended effects from
confirmed effects. A crash between those points produces `unknown`, which must
be reconciled instead of retried blindly.

## Complete mediation

Gatemole can only claim authority over resources reached through its broker. A
Runtime whose agent sandbox also possesses direct credentials or unrestricted
host access can bypass the kernel.

Therefore:

- Production adapters receive no ambient privileged credentials.
- Connector credentials are acquired by connector drivers after authorization.
- Sandboxes restrict direct filesystem, process, and network escape paths.
- Protocol integrations are exposed as typed driver operations, not raw
  passthrough connections.
- The audit result states which resource classes were mediated and which were
  outside Gatemole's enforcement boundary.

## Initial deployment shape

The first implementation is local and single-node:

- A `gatemoled` process.
- HTTP-framed control API over a mode-`0600` Unix socket.
- SQLite transactions for events and materialized state.
- Execution contracts compiled into time-bounded capability grants.
- Traversal-resistant filesystem connector driver scoped to a per-run
  workspace.

Local content-addressed artifacts, checkpoints and a subprocess agent adapter
remain subsequent milestones.

Distributed scheduling, PostgreSQL, external object storage, and remote
identity are deferred until local crash and mediation invariants are proven.

## Dependency direction

```text
kernel model and reducer
        ^
        |
store, policy, broker, runtime interfaces
        ^
        |
daemon, CLI, and external adapters
```

The existing contract compiler and evidence runtime remain independent of the
daemon. The kernel calls them through narrow interfaces; compiler packages do
not import storage, daemon or agent-adapter packages.

## Consequences

Positive:

- Gatemole remains agent-framework- and model-independent.
- Deterministic policy stays outside probabilistic reasoning.
- The existing compiler/evidence investment becomes a kernel subsystem.
- Local invariants can be proven before distributed complexity is introduced.
- Runtime and protocol integrations can evolve independently.

Costs:

- Useful operations require broker and connector-driver coverage.
- Complete mediation depends on sandbox and credential configuration outside
  Gatemole itself.
- Event compatibility and recovery become long-term public contracts.
- Connector-specific reconciliation is required for ambiguous non-idempotent
  effects.

## Rejected alternatives

### Build a new agent framework

Rejected because planning, prompting, model routing, and tool-loop frameworks
are already abundant. Owning them would couple Gatemole's authority semantics to
one reasoning architecture.

### Treat telemetry as enforcement

Rejected because observing an action after it occurs cannot prevent the side
effect. Telemetry is evidence; the broker is the enforcement point.

### Start with Kubernetes and a distributed service

Rejected because distribution can hide unresolved lifecycle and crash
semantics. The local kernel must first prove replay, mediation, revocation, and
recovery.

### Let adapters evaluate policy

Rejected because adapters are part of the untrusted execution environment.
They can collect facts but cannot authoritatively allow their own actions.
