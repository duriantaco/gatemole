# Gatemole Agent OS Roadmap

## Direction

**Gatemole Agent OS** is the complete architecture: the organization-wide Control
Plane, a fleet of customer-side Gatemole Runtimes, the transaction protocol and
connector model. It is not a separate daemon.

**Gatemole Runtime** is the deployable enforcement boundary. The trusted
**`gatemoled` kernel** lives inside each Runtime and owns authority, durable
transaction state, policy decisions, approvals and commit coordination.

One kernel supports two product experiences:

| Experience | User promise | Current truth |
| --- | --- | --- |
| **Gatemole Developer Runtime** | Run an existing agent locally with bounded authority, isolated effects, review and recovery. | A manually operated local-Git path plus profile initialization, diagnostics and an exact review/apply/reject shell exists. Packaging, maintained adapters and live supervision need product work. |
| **Gatemole Agent OS** | Govern consequential agent actions across customer systems and a fleet of Runtimes. | Enterprise experience and full target architecture only. The action protocol, remote connectors, cross-run policy and Control Plane are planned. |

The experiences are different packaging and operations around the same
enforcement semantics, not separate kernels. The honest current external
product message is:

> **Gatemole Runtime — stateful transaction and outcome integrity for local Git
> actions proposed by autonomous agents.**

The market problem is real, but generic identity, policy gateways, durable
agent runtimes, workflow orchestration and fleet control planes are already
crowded categories. The reasoning and competitive evidence are recorded in
[Product Validation](docs/PRODUCT_VALIDATION.md).

## Product thesis

An agent should be able to propose useful work without possessing ambient
production authority.

```text
admit exact intent and delegated authority
  -> execute an untrusted agent inside a bounded Runtime
  -> turn attempted actions into typed proposed effects
  -> evaluate individual actions and the composed task
  -> stage or hold effects where possible
  -> verify exact preconditions, invariants and postconditions
  -> obtain approval for the frozen outcome when required
  -> commit in dependency order
  -> reconcile, compensate or require manual recovery
```

Gatemole succeeds when a customer safely grants an agent authority to complete a
task it previously allowed the agent only to suggest.

## Current state

The repository contains a substantial foundation:

- strict resources for tasks, images, contracts, runs, capabilities, actions,
  transactions, effects, verifications, approvals and commit plans;
- deterministic reducers and hash-chained event histories;
- SQLite persistence, optimistic concurrency and restart recovery;
- daemon-owned OCI agent and verifier execution;
- isolated Git worktrees, immutable tree staging and exact verifier
  materialization;
- model credential isolation, token limits and receipts;
- OIDC operator identity and signed, independent approvals;
- sequence findings, compare-and-swap local Git publication and crash
  reconciliation.

The supported profile remains one node, one security tenant and publication to
an allowed local Git ref. It does not yet control a remote enterprise system.
Its developer integration is also low-level: the user must operate the daemon,
provide a digest-pinned OCI image and use transaction-oriented inspection
commands.

### Next architecture gap

The production `gatemole run` path now uses one daemon-owned, idempotent admission
operation to persist the exact `AgentTask`, content-bound
`ExecutionContract`, real `AgentRun`, initial capability grants and
`AgentTransaction` in one SQLite transaction.

Immediately before OCI launch, `gatemoled` now reloads a consistent live snapshot
of those resources and compiles a fail-closed execution plan. Caller-supplied
image and command material must match admitted digests; only full-workspace
read/write and explicitly granted provider-scoped model brokering are
currently supported. Expired, terminal, narrowed or otherwise unsupported
authority starts no workload.

Before any broker or agent workload starts, `gatemoled` atomically verifies
that both snapshot heads are still current and records the exact execution in
the run and transaction ledgers. Settlement and restart recovery finish that
same execution on both sides, clear the run's active binding, and cumulatively
charge elapsed wall time plus verified model-call/token usage. A concurrent run
or transaction change therefore starts no workload, and a partial paired
mutation cannot commit.

The remaining OS-3 work is narrower data authority, mediated tool/connector
execution, and durable tool/cost accounting. Live lifecycle controls must also
interrupt an active OCI workload rather than only changing metadata. The older
brokered `ActionRequest` endpoints remain disabled by the production profile
until that path is joined to current admission and execution authority.

## Execution principles

1. **One authority path.** Task, run, contract, capabilities, transaction and
   release scope are admitted atomically.
2. **Kernel before driver.** Connector behavior is implemented behind one
   conformance-tested interface, not inside GitHub-specific API handlers.
3. **No ambient credentials.** Agents never receive downstream production
   credentials or unrestricted egress.
4. **External truth before retry.** An ambiguous non-idempotent action is
   reconciled; it is never retried blindly.
5. **Exact approval.** Any material change to authority, effects, verification,
   policy or release target invalidates approval.
6. **Connector-specific recovery.** Reversible, compensatable and irreversible
   effects remain distinct.
7. **Integrate standards.** Use existing identity, policy and orchestration
   systems rather than rebuilding them.
8. **Commercial evidence gates breadth.** A merged feature is not product
   validation.

## Execution workstreams

Each item is a bounded workstream delivered through short, independently
reviewable pull requests. Status describes the current repository, not market
validation.

| Change | Status | Deliverable | Required acceptance |
| --- | --- | --- | --- |
| **OS-0: architecture truth** | Complete | README architecture and glossary; evidence-backed product validation; this executable roadmap; terminology cleanup in kernel docs. | Every architecture box maps to implemented code or is clearly marked planned or external. Documentation makes no unsupported production claim. |
| **OS-1: focused validation lanes** | Complete | At most three required PR lanes: Go checks, affected acceptance and documentation when changed. Run the full race, vulnerability and benchmark collections on `main`, nightly and release. Retain production OCI acceptance for security-critical Runtime changes. | Normal PR gate completes in a bounded target such as 12 minutes. Path rules cannot skip production acceptance when kernel, sandbox, identity, approval, release or connector code changes. |
| **OS-2: atomic task admission** | Complete | One daemon-owned, idempotent endpoint persists the exact task, `ExecutionContract`, real `AgentRun`, initial capability grants and `AgentTransaction` in one database transaction. The server authors authoritative events and digests. | Fault injection after every persistence step creates no orphan resources. Identical idempotency retry returns the same admission; a changed retry conflicts. Every transaction references a digest-matching run and contract. |
| **OS-3: bind execution authority and data boundaries** | In progress | OCI execution transitions the admitted `AgentRun`. Identity, sponsor and parent lineage, contract, capabilities, deadline, allowed resources, data classes, model egress and budgets become authoritative. Task limits may narrow daemon ceilings but never widen them. | Run and transaction state cannot diverge after success, failure or injected crash. Expired authority prevents start. Time, model, tool and cost limits fail closed and are durably charged across restart. Denied mounts, connector reads, resource classes and model-egress destinations are inaccessible rather than merely recorded. |
| **OS-4: durable supervisor and circuit breaker** | Planned; design accepted | Asynchronous workload start, durable desired state and execution lease; functional `watch`, `cancel`, kill and revocation, following [ADR-003](docs/architecture/ADR-003-durable-execution-supervisor.md). “Pause” means stop at a declared safe boundary, not arbitrary process snapshotting. | Repeated start creates one workload. Cancel terminates the agent and broker within a bounded interval and prevents release. No new effect executes after revocation. Restart produces one reconciled outcome. |
| **DX-1: Developer Runtime onboarding** | In progress | Runtime-aware project setup and diagnostics, a versioned CLI release, one maintained coding-agent profile and local daemon setup. Keep generated configuration explicit and repository-owned. | On a clean supported machine, a developer can install Gatemole, initialize a repository, diagnose prerequisites and start one maintained agent without hand-authoring kernel configuration. |
| **DX-2: Developer review shell** | Complete | Readable status and daemon-rendered, digest-checked exact diff plus explicit apply/reject, preserving the underlying transaction, effect, evidence and approval IDs. `apply` and `reject` are thin aliases over release and abort; they do not bypass authority. Reuse OS-4 for `watch` and real cancellation. | Integration tests inspect the exact frozen patch and evidence, reject a changed worktree, apply only through prepared release authority, and reject by aborting and removing the isolated worktree. |
| **OS-5: connector interface and transaction coordinator** | Planned | Introduce `Plan → Stage/Hold → Inspect → Verify → Commit → Reconcile → Compensate`; add a connector registry plus a durable dependency-ordered coordinator. Each connector persists preparation state, commit state and receipts. Unknown results stop dependent work; restart performs reconciliation before retry or compensation. Move local Git behind the interface only after the generic harness passes. | Two fake connectors prove dependency-ordered prepare/commit, stop-on-unknown, restart recovery, reverse compensation and manual-recovery escalation. Faults are injected before dispatch, during dispatch and after an external effect but before its receipt. Connectors cannot mint authority. Existing local-Git production acceptance remains behaviorally unchanged. |
| **OS-6: versioned Runtime action protocol and broker** | Planned | Replace or converge the legacy broker behind a supported `ActionRequest → decision/approval → receipt` protocol. A transaction-scoped workload credential binds sandbox transport, task, run, transaction, delegated identity, capability, deadline and idempotency key. Publish stable status/event cursors, error semantics and one maintained Go client. Direct private-worktree mutation remains an explicitly contained, stageable boundary. | Local transport rejects forged, cross-run, expired and replayed credentials. Approval resumes only the exact immutable request after restart. A fake connector proves allow, deny, approval, expiry and revocation without exposing its credential. N/N-1 protocol and adapter-conformance fixtures pass; every attempted external effect is attributable. |
| **OS-7: lineage-aware temporal policy** | Planned | Persist immutable identity, delegation, action and effect facts across runs and transactions. Evaluate bounded temporal and separation-of-duties rules across sessions, systems, child agents and a shared sponsor lineage. Ordinary stateless decisions may use an ACS/Cedar/OPA adapter, but Gatemole remains authoritative for history and commit. | An AP-style fixture spanning separate sessions and delegated identities is denied for the composed sequence while individually valid actions remain allowed. Restart yields the same decision. Retention and query bounds are explicit, and the policy adapter cannot authorize or commit an effect outside the Runtime transaction. |
| **GH-1: GitHub ref driver** | Planned | After OS-5 and OS-6, add daemon-owned GitHub App authentication, installation/repository allowlist, exact-base branch publication and reconciliation. No pull request or merge yet. | Credentials never enter the task, sandbox, events or logs. Retry is idempotent, stale base fails, and a simulated timeout after remote mutation reconciles to the exact commit without another mutation. |
| **GH-2: protected PR driver** | Planned | Idempotent create/update, transaction marker, exact head/base binding and a concise reviewer summary. Material updates invalidate prepared authority. | Retry creates no duplicate PR. Foreign or stale heads fail closed. Summary explains intent, material effects, verification, risk and required authority without dumping the internal ledger. |
| **GH-3: exact-SHA merge** | Planned | Recheck head SHA, required checks, review policy, approval package and releaser separation immediately before merge; reconcile ambiguous outcomes. | Mutated head, failed check, expired approval and approver-as-releaser fail. Injected timeout yields at most one merge. Receipt identifies the exact reviewed and merged SHA. |
| **OPS-1: release, API and evidence** | Planned | Versioned Runtime configuration and local API contract, daemon container/service definition, audit-bundle export and compatibility/upgrade policy. | A clean supported VM completes the documented transaction. N/N-1 API/config compatibility, restart, backup and restore pass. JSON and Markdown evidence replay to the authoritative projection. |
| **CP-1: pilot Gatemole Control Plane** | Planned | Runtime enrollment and health, signed policy distribution, approval routing, central receipt collection and fleet kill/revocation. Build only after paid-pilot evidence. | Gatemole Control Plane loss cannot duplicate effects or erase local receipts. Cached-policy behavior is explicit and fails safely. One partner operates multiple Runtimes. |

## Product milestones

### Milestone A: Gatemole Developer Runtime preview

Complete **OS-0 through OS-4**, **DX-1** and **DX-2**. OS-5 through OS-7 are
required for the enterprise connector path, not for an honest local developer
preview.

Exit criteria:

- one maintained agent integration runs through the documented local setup;
- one task creates one durable chain through run, Git effects, verification,
  approval and local-ref outcome;
- expiry and cancellation affect the real workload;
- the developer can inspect the exact diff and evidence, then explicitly apply
  or reject it without database surgery;
- the documentation names the single-node, local-Git and full-workspace limits.

At this point Gatemole may describe the deliverable as a **Gatemole Developer Runtime
preview**, not a complete Gatemole Agent OS.

### Milestone B: Gatemole Runtime GitHub pilot

Complete **OS-5**, **OS-6**, **GH-1 through GH-3** and **OPS-1**. Complete
**OS-7** before the multi-system production pilot; it may proceed in parallel
with the first GitHub connector proof.

GitHub is a technical proving ground, not an assumed standalone market. Native
GitHub agent controls already provide isolated execution, safe write outputs,
protected branches and human merge. The proof must demonstrate Gatemole-specific
properties: task authority, exact outcome binding, connector receipts and
ambiguous-effect reconciliation.

Exit criteria:

- every GitHub mutation belongs to an admitted Gatemole transaction;
- the agent receives no GitHub write credential;
- mutation after approval invalidates release;
- restart and injected timeouts create no duplicate branch, PR or merge;
- one operator can install and complete the supported transaction without
  database surgery.

### Milestone C: Gatemole Runtime multi-system production pilot

Add deep connectors in this order:

1. Kubernetes deployment, canary health, traffic progression and rollback.
2. PostgreSQL migration dry-run, invariants, native transaction where possible
   and compensation/recovery metadata otherwise.
3. One combined GitHub + Kubernetes + PostgreSQL transaction.

Exit criteria:

- connector credentials and network paths are non-bypassable;
- commit dependencies and receipts span all three systems;
- fault injection after every release boundary produces `committed`,
  `rolled_back`, `partially_committed` or `manual_recovery_required` truthfully;
- unknown non-idempotent work is never duplicated.

This milestone validates the Runtime enforcement layer. It does not by itself
complete the Gatemole Agent OS architecture.

### Milestone D: Gatemole Control Plane pilot

Build **CP-1** only after the commercial gates pass. Expand it incrementally
with organization policy simulation, SSO/RBAC, approval delegation, audit
retention/search/SIEM, connector configuration and usage/cost visibility.

The local Runtime remains authoritative for effects and receipts. A Control
Plane outage may stop new authority according to policy, but must never cause a
known effect to be duplicated or an acknowledged receipt to disappear.

Exit criteria:

- one paid partner manages at least two Runtimes through the Control Plane;
- signed policy and revocation reach each enrolled Runtime;
- approval routing and central receipt collection preserve local authority;
- Control Plane loss follows the declared cached-policy behavior without
  duplicating an effect.

After this exit, the complete system may be presented as a **Gatemole Agent OS
private preview**. Production enterprise claims still require the distributed
operations in Milestone E.

### Milestone E: distributed Runtime and vertical packs

After single-node semantics and paid usage are stable:

- PostgreSQL and object-storage persistence;
- leases, scheduling, quotas, priorities and admission control;
- multi-tenant network API and Runtime workload authentication;
- HA, failover, upgrades, disaster recovery, residency and retention;
- finance/ERP, cloud operations and customer-operations transaction packs.

Finance packs should interoperate with existing ERP segregation-of-duties
controls and payment standards such as AP2. They must prove a cross-system or
delegation-lineage gap rather than duplicate native SAP controls.

## Commercial validation

Engineering and customer discovery run in parallel.

### Problem gate

- Interview 20 enterprises with live agent pilots.
- Pass only if at least 8 describe consequential write authority as a top-three
  blocker, 5 provide a recent refused workflow and 3 accept a scoped pilot.

### Existing-controls gate

For each workflow, model the strongest solution using the customer's existing
identity provider, cloud/API gateway, native application controls and
Camunda/Temporal or equivalent workflow engine.

Continue only when customers identify a material composed-effect,
exact-outcome or recovery gap that existing controls cannot cover acceptably.

### Non-bypassability gate

At least 3 pilot customers must be willing to remove scoped credentials from
the agent and route every relevant write through a Gatemole Runtime. Otherwise the
product is advisory middleware.

### Permission-expansion and recovery gate

Each pilot must complete at least 20 real transactions, including 5 tasks the
customer previously restricted to humans, with:

- zero unauthorized, duplicate or unattributed material effects;
- zero replay mismatches;
- explicit reconciliation for every ambiguous external result;
- measured false-block rate and approval latency.

### Commercial gate

Obtain two paid pilots, not only free design partnerships, before building the
Gatemole Control Plane.

Stop or reposition if customers are satisfied by native controls, refuse
non-bypassable mediation, cannot name a material cross-action gap or value
receipts and recovery only as optional compliance evidence.

## Validation strategy

Required pull-request validation should stay proportionate:

1. **Go checks:** formatting, module consistency, vet and unit tests.
2. **Affected acceptance:** the smallest kernel, Runtime, connector or
   Contracts scenario that proves the changed invariant.
3. **Documentation:** strict documentation build only when documentation or
   public schemas change.

Full race, vulnerability, all-benchmark and production matrices run on
`main`, nightly and releases, with path-sensitive production acceptance still
required for security-critical changes. A new feature should add one focused
acceptance scenario rather than another overlapping benchmark family.

## Gatemole Contracts

Gatemole Contracts remains an optional verification module. Its Runtime priorities
are:

- bind obligation and evidence requirements into atomic task admission;
- run required verification under daemon-owned profiles;
- bind exact coverage and evidence provenance into approval packages;
- add formats only when a real connector or pilot requires them.

Do not expand Contracts into a parallel product, generic AI reviewer or
unbounded authoring project.

## Explicit deferrals

- A new identity provider, credential vault or agent directory.
- A new generic policy language or competing guardrail standard.
- A general workflow/process orchestration engine.
- Broad MCP passthrough without typed effects and recovery semantics.
- SAP, Salesforce, email and broad cloud connectors before the heterogeneous
  software transaction works.
- Cross-session finance policies before identity lineage and effect history are
  authoritative.
- Universal rollback or fictional cross-system ACID guarantees.
- Arbitrary process snapshot/resume.
- Multi-node scheduling, production multi-tenancy and HA before paid evidence.
- A large dashboard, marketplace or multiple SDKs before the public API and
  connector contract stabilize.
- Further generic observability and evaluation features.

## Metrics

Technical invariants:

- duplicate non-idempotent effects: **zero**;
- unattributed material effects: **zero**;
- replay/projection mismatch: **zero**;
- authority exercised after expiry or revocation: **zero**;
- unknown outcome automatically retried: **zero**.

Product metrics:

- permission-expansion events;
- transactions and material effects mediated;
- automatic, approval, revise and block rates;
- false-block rate and approval latency;
- postcondition verification coverage;
- unknown, partial-commit, compensation-failure and manual-recovery rates;
- pilot conversion and paid renewal.
