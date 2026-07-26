# ADR-001: Vouch Agent Kernel Boundary

- Status: accepted for implementation
- Date: 2026-07-23
- Owners: Vouch maintainers

## Context

Vouch currently compiles human-owned release intent into typed specifications,
stable obligations, verification plans, evidence requirements, and release
decisions. That runtime begins near the end of an agent-authored change.

Moving toward an agent operating system requires Vouch to govern the execution
that produces the change without becoming the component that reasons about how
to implement it. The architectural risk is collapsing four separate concerns
into one service:

- Agent reasoning and planning.
- Durable execution and scheduling.
- Authorization and side-effect mediation.
- Contract, evidence, and release policy.

The authority boundary must remain deterministic even when the agent, model,
tool, runtime adapter, or external data is malicious or simply wrong.

## Decision

Vouch will be a user-space agent kernel and control plane. It will run above a
host operating system and integrate with existing agent runtimes, sandboxes,
identity systems, durable execution engines, and protocols.

Vouch owns:

- Execution-contract compilation.
- Agent run identity, lineage, and lifecycle state.
- Capability and delegation semantics.
- Admission and action policy decisions.
- Side-effect mediation through typed resource drivers.
- Checkpoint, event, evidence, and audit semantics.
- Human approval as a durable kernel operation.
- Explanation of why work may advance.

Vouch does not own:

- Model inference or prompting strategy.
- The agent's planner or reasoning loop.
- Container, microVM, or operating-system isolation implementations.
- A general workflow engine.
- Workload identity, secret storage, or telemetry backends.
- MCP, A2A, or model-provider protocols.

Those systems connect through adapters. An adapter may report observations and
request actions; it cannot create authoritative decisions, capabilities, or
committed action receipts.

## Control plane

The control plane is authoritative for:

- `AgentImage`
- `ExecutionContract`
- `AgentRun`
- `CapabilityGrant`
- `PolicyDecision`
- `Checkpoint`
- `RunEvent`
- Human approvals
- Materialized run state

Control-plane writes occur through validated APIs. Each accepted change emits
an append-only event. Materialized state is a projection that must be
reconstructable from the accepted event sequence.

## Data plane

The data plane contains:

- Runtime adapters that host or connect to agent loops.
- Resource drivers that perform filesystem, process, MCP, GitHub, database, or
  cloud operations.
- Sandboxes and per-run workspaces.
- Artifact and checkpoint storage.

The data plane is replaceable and untrusted from the kernel's perspective.

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
  -> resource driver execution
  -> persist receipt or mark result unknown
  -> link evidence and update run projection
```

Persisting the authorization before execution makes the decision auditable.
Persisting the receipt after execution distinguishes intended effects from
confirmed effects. A crash between those points produces `unknown`, which must
be reconciled instead of retried blindly.

## Complete mediation

Vouch can only claim authority over resources reached through its broker. A
runtime that also possesses direct credentials or unrestricted host access can
bypass the kernel.

Therefore:

- Production adapters receive no ambient privileged credentials.
- Resource credentials are acquired by drivers after authorization.
- Sandboxes restrict direct filesystem, process, and network escape paths.
- Protocol integrations are exposed as typed driver operations, not raw
  passthrough connections.
- The audit result states which resource classes were mediated and which were
  outside Vouch's enforcement boundary.

## Initial deployment shape

The first implementation is local and single-node:

- A `vouchd` process.
- HTTP-framed control API over a mode-`0600` Unix socket.
- SQLite transactions for events and materialized state.
- Execution contracts compiled into time-bounded capability grants.
- Traversal-resistant filesystem driver scoped to a per-run workspace.

Local content-addressed artifacts, checkpoints, and a subprocess runtime
adapter remain subsequent milestones.

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
not import storage, daemon, or runtime-adapter packages.

## Consequences

Positive:

- Vouch remains runtime- and model-independent.
- Deterministic policy stays outside probabilistic reasoning.
- The existing compiler/evidence investment becomes a kernel subsystem.
- Local invariants can be proven before distributed complexity is introduced.
- Runtime and protocol integrations can evolve independently.

Costs:

- Useful operations require broker and driver coverage.
- Complete mediation depends on sandbox and credential configuration outside
  Vouch itself.
- Event compatibility and recovery become long-term public contracts.
- Driver reconciliation is required for ambiguous non-idempotent effects.

## Rejected alternatives

### Build a new agent framework

Rejected because planning, prompting, model routing, and tool-loop frameworks
are already abundant. Owning them would couple Vouch's authority semantics to
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
