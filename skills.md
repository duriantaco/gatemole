# Vouch Product Operating Brief

Use this brief when choosing roadmap work, designing features, writing docs or
describing Vouch.

## Product identity

Vouch is the product name. **Vouch Agent OS** is the complete target
architecture: the Vouch Control Plane plus a fleet of customer-side Vouch
Runtimes. **Vouch Runtime** is the first sellable product and the enforcement
boundary for autonomous-agent actions.

Its job is to make an agent task a controlled transaction:

```text
intent
  -> isolated execution
  -> proposed effects
  -> immutable staged state
  -> deterministic policy and verification
  -> independent authority
  -> commit | block | revise | recover
```

The product controls whether an agent's effects may become real. It does not
own the agent's planning, prompting or reasoning loop.

## Product hierarchy

Keep these names distinct:

- **Vouch Agent OS**: the complete architecture containing the Control Plane,
  Runtime fleet, transaction protocol and connector model. It is an umbrella,
  not a process.
- **Vouch Control Plane**: the future central manager for Runtime fleets,
  organization policy, approval routing, audit and incident response. It
  manages connector configuration but does not execute downstream actions.
- **Vouch Runtime**: the current product and customer-side enforcement
  boundary.
- **`vouchd` kernel**: the trusted authority and transaction engine inside each
  Runtime.
- **Vouch Contracts**: an optional module that compiles release intent into
  verification obligations and maps evidence to them.

Never describe Vouch Contracts as the whole product. Never describe the current
Vouch Runtime as a complete Vouch Agent OS or a production multi-tenant Vouch
Control Plane.

## Runtime boundary

Vouch owns:

- Task-scoped transaction and effect state.
- Isolated runtime admission and resource limits.
- Immutable staging and verifier inputs.
- Deterministic action, sequence and release policy.
- Verification orchestration and evidence binding.
- Approval-package integrity and separation of duties.
- Commit receipts, reconciliation and truthful partial failure.
- An authoritative replayable event history.

Vouch integrates with, but does not replace:

- Coding agents and agent frameworks.
- OCI containers, microVMs and host operating systems.
- Identity providers, credential stores and policy engines.
- Git hosting, CI systems, deployment systems and databases.
- Model providers and tool protocols.

Complete mediation matters. If an agent retains direct credentials, unrestricted
egress or access around Vouch, the runtime cannot claim authority over those
paths.

## Current supported profile

The only supported deployment profile is a single-node, single-tenant runtime
on a dedicated trusted host that publishes an approved commit to an allowed
local Git ref. It requires pinned images, daemon-owned verification, OIDC,
signed independent approval, separate release authority and the documented
storage and host controls.

It does not currently push, open or merge pull requests, deploy, coordinate
database/Kubernetes effects, provide remote multi-tenant service, or provide
HA. Stable release packaging is pending.

## Good work

Prefer work that strengthens the runtime path:

- A one-command task experience that preserves kernel authority.
- Public task, transaction, effect, verification and approval APIs.
- A stable agent image/task-envelope protocol.
- Deep, non-bypassable resource connectors with reconciliation.
- Better staging, isolation, recovery and idempotency invariants.
- Operator status, watch, logs, approval and recovery UX.
- Runtime configuration, diagnostics, packaging and upgrades.
- Policy simulation, provenance, audit export and compatibility guarantees.
- Real pilots where Vouch lets a team grant an agent authority it previously
  withheld.

For Vouch Contracts, prefer work that makes runtime verification stronger:

- Contract-to-verifier compilation.
- Obligation IDs included in transaction verification and approval packages.
- Evidence provenance and exact staged-state binding.
- SARIF, coverage, deployment and rollback evidence import.
- Deterministic, auditable policy inputs.

## Wrong turns

Avoid:

- Turning Vouch into another coding-agent framework.
- Generic AI code review, style comments or unsupported correctness claims.
- Treating logs or dashboards as an enforcement boundary.
- Treating a per-tool allow/deny proxy as sufficient transaction control.
- Adding shallow connectors that cannot stage, reconcile or report ambiguity.
- Claiming universal rollback for irreversible or compensatable effects.
- Building a hosted dashboard before the local runtime has a coherent
  developer API.
- Leading the product with the optional Contracts compiler or CI gate.

## Positioning

Use:

- “Vouch Runtime — transaction and outcome control for autonomous-agent
  actions”
- “the controlled boundary between an agent proposal and a real effect”
- “`vouchd` kernel” for the trusted authority and transaction engine
- “isolated, verified and authorized agent execution”
- “Vouch Runtime”
- “Vouch Agent OS” for the complete Control Plane plus Runtime-fleet
  architecture
- “Vouch Contracts” when referring specifically to the optional compiler

Use carefully:

- “enterprise Agent OS” as an external completeness claim only after
  multi-system Runtime enforcement and fleet-level Control Plane operation are
  implemented.
- “Control Plane” only for the central fleet-management product.

Avoid primary positioning as:

- “release-contract compiler”
- “AI code reviewer”
- “CI/CD gate”
- “MCP gateway”
- “identity provider”
- “general workflow engine”

## Decision test

Before implementing a feature, ask:

1. Which task-scoped effect or authority boundary does this strengthen?
2. Is the action staged or mediated before it becomes real?
3. Is the decision deterministic, attributable and replayable?
4. What happens after a crash or an ambiguous non-idempotent result?
5. Can an agent bypass it with ambient credentials, filesystem access or
   network egress?
6. Does this improve the developer's path from intent to controlled outcome?
7. Is this core runtime work, an optional Contracts feature, or future Control
   Plane work?

If the answer depends on an LLM being the sole enforcement authority, redesign
it.

## Near-term order

1. Stabilize the implemented task-oriented CLI, named `AgentImage` profiles and
   read-only `vouch.agent_task.v0` envelope; publish the external SDK/API.
2. Wire execution contracts, budgets, mandatory verifier sets and release
   targets into task creation.
3. Deliver one deep remote-Git/GitHub connector and approval experience.
4. Prove paid design-partner demand for fleet policy, audit and approvals.
5. Build the Vouch Control Plane around customer-side Vouch Runtimes.
6. Add Kubernetes and PostgreSQL transaction packs only after Git release and
   reconciliation are deep.

The primary product metric is permission expansion: a customer safely lets an
agent complete work it previously could only propose.
