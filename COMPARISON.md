# Where Vouch Runtime Fits

Vouch Runtime is the transaction boundary between an autonomous agent and the
systems it may change. It composes with agent frameworks, sandboxes, identity,
policy, CI and supply-chain tools rather than replacing them.

One-sentence version:

> Vouch runs an agent's task as a controlled transaction: isolate execution,
> stage and normalize effects, verify the exact outcome, obtain authority, then
> commit or recover.

## Product boundary

| Layer | Primary job | Relationship to Vouch |
| --- | --- | --- |
| Agent framework or coding agent | Plan, reason, call models and tools, produce a solution | Runs inside or connects through Vouch; remains untrusted at the authority boundary |
| Container, microVM or host OS | Process and resource isolation | Supplies isolation primitives that `gatemoled` configures and constrains |
| Identity provider | Authenticate people, services and workloads | Supplies principals and claims; Vouch applies them to transaction authority |
| Policy engine | Evaluate deterministic policy over structured facts | Can evaluate Vouch transaction/effect facts; does not stage or commit effects |
| CI and scanners | Execute tests, builds and analysis | Produce verifier results and evidence bound to the exact staged state |
| Supply-chain tooling | Sign artifacts and describe provenance | Establishes identity and provenance inputs used by Vouch authority |
| Vouch Contracts | Compile release intent into obligations and map evidence | Optional module that strengthens Vouch Runtime verification |
| Vouch Runtime | Govern the complete task from isolated execution through commit/recovery | Current product |
| Vouch Control Plane | Manage Runtime fleets, organization policy, approval UX, audit and connector configuration | Future commercial layer; it does not execute connector actions |

## Why a per-tool gateway is not enough

A gateway can answer whether one request is allowed. A harmful task can still
be a sequence of individually allowed calls:

```text
change security control
  -> change the test that checks it
  -> approve the new evidence
  -> publish the result
```

Vouch evaluates the ordered effect set and freezes the exact state presented to
verification and approval. A material mutation invalidates that authority.

## Why a sandbox is not enough

A sandbox can constrain a process but does not by itself define:

- The business effects produced by a complete task.
- Which outcome verifiers must run.
- Who may approve the exact proposed outcome.
- Commit ordering and expected resource versions.
- What to do after an ambiguous or partially committed effect.

Vouch uses an OCI boundary in the current profile, but its distinct layer is the
transaction and authority protocol around that execution.

## Why observability is not enforcement

Logs and traces explain what happened after an action. Vouch persists an action
or effect decision before execution and withholds stageable or irreversible
effects until the release policy is satisfied. Telemetry remains valuable
evidence; it is not the commit boundary.

## Why an agent framework is not the product

Agent frameworks should continue to own prompting, planning, memory and model
loops. Vouch should remain runtime- and model-independent. An adapter may
request work and report observations, but it cannot author authoritative policy,
approval or commit receipts.

## Identity and policy systems

Vouch should consume existing OIDC identities and integrate general policy
engines rather than become an identity provider or invent a universal policy
language.

Identity answers “who is this?” A policy engine can answer “is this structured
request allowed?” Vouch adds task-scoped facts and enforcement:

- Sponsor, agent and authority participation.
- Ordered normalized effects.
- Immutable staged-state and effect-set digests.
- Verification independence and freshness.
- Approval-package and commit-plan bindings.
- Commit receipts and reconciliation state.

## CI, signing and provenance

Vouch should compose with established tooling:

- [Sigstore/cosign](https://docs.sigstore.dev/cosign/signing/overview/) can
  establish artifact signer identity.
- [SLSA](https://slsa.dev/spec/v1.2/) can describe build provenance.
- [in-toto](https://in-toto.io/docs/getting-started/) can describe authorized
  supply-chain steps and signed link metadata.
- [OPA/Rego](https://www.openpolicyagent.org/docs/policy-language) can evaluate
  structured Vouch policy input.
- CI systems, tests, scanners and deployment checks can produce evidence.

Those tools do not create Vouch's task-scoped transaction, isolate its mutable
workspace, freeze its complete effect set, bind approval to the exact outcome,
or coordinate commit and recovery.

## Vouch Contracts

Vouch Contracts supplies an optional semantic verification layer:

```text
human-owned release intent
  -> typed AST and diagnostics
  -> obligation IR
  -> verification requirements
  -> evidence coverage
  -> policy facts for the transaction
```

This remains useful for high-risk code where “tests passed” does not establish
that security, rollout, observability or rollback obligations were checked. It
is a module of Vouch Runtime, not the whole product and not a generic AI code
reviewer.

## Current versus future

Today, the supported Vouch Runtime profile is a single-node, single-tenant
local-Git boundary. It can run pinned agent and verifier containers, broker
model access, stage an immutable Git tree, enforce independent authority and
atomically update an allowed local ref.

It does not yet provide remote GitHub merge, deployment or database connectors,
a network multi-tenant service, HA or fleet management.

The future Vouch Control Plane will manage multiple customer-side Vouch
Runtimes, organization policy, approvals, audit and connector configuration.
Connector drivers will continue to execute inside each Runtime. The complete
Vouch Agent OS external claim requires both non-bypassable multi-system Runtime
enforcement and demonstrated Control Plane operation across a Runtime fleet.

## Non-goals

Vouch is not trying to:

- Replace an agent's reasoning framework.
- Replace containers, microVMs or the host OS.
- Become an identity directory or credential vault.
- Be only a CI gate or MCP proxy.
- Read arbitrary code and declare it correct.
- Promise universal rollback or cross-system ACID transactions.

The defensible product is narrower and stronger: a durable transaction and
authority boundary for autonomous work.
