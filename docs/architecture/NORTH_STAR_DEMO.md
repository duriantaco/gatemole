# Gatemole Runtime Kernel North-Star Demonstration

## Status

This is a **target acceptance scenario**, not one currently executable path.

The supported transaction path now atomically admits its task,
`ExecutionContract`, `AgentRun`, initial capabilities and transaction before it
runs an OCI agent. It then stages immutable local Git effects, validates their
sequence, runs daemon-owned verification, binds signed approvals and publishes
to an allowed local ref.

The OCI workload does not yet advance that admitted run or consume its
capabilities through the lower-level brokered action path. The roadmap's OS-3
through OS-7 work closes that execution, supervision, connector and temporal
policy gap before this document becomes an executable end-to-end acceptance
test.

## Target claim

Gatemole Runtime will durably govern one untrusted coding agent through a complete
action, approval, restart, evidence and release path.

The demonstration validates kernel authority and transaction behavior. It does
not validate the future organization-wide Gatemole Control Plane, claim that the
agent's implementation is correct or claim that Gatemole can infer arbitrary
intent.

## Fixture

The fixture contains:

- A small Git repository with a reviewed Gatemole release intent.
- An `AgentImage` using the reference subprocess adapter.
- An `ExecutionContract` granting read/write access only inside the fixture
  workspace.
- A process capability for the repository's test command.
- A protected `git.push`-class action requiring human approval.
- Required test, security, runtime, and rollback obligations.

## Scenario

1. Start `gatemoled` with an empty local store.
2. Register the agent image and execution contract.
3. Create the run and assert state `created`.
4. Admit it and assert state `admitted`.
5. Start the subprocess adapter and assert state `running`.
6. Request a workspace-relative file write.
7. Assert the action is authorized, executed, receipted, and visible in events.
8. Request a path-escape write.
9. Assert the action is denied and the target was never changed.
10. Request the protected push action.
11. Assert the run enters `waiting_for_approval` with a normalized action digest.
12. Stop and restart `gatemoled`.
13. Assert event replay reconstructs identical state and pending approval.
14. Approve as an authenticated fixture operator.
15. Assert the exact action resumes; a mutated request cannot reuse approval.
16. Execute fixture tests and attach their evidence.
17. Run the existing Gatemole release gate.
18. Assert the expected release decision and obligation coverage.
19. Export the run history and verify event order, digests, identities, policy
    rules, action receipts, checkpoint, evidence links, and final decision.

## Negative controls

- Unknown resource field fails strict decoding.
- Illegal lifecycle transition does not append an accepted event.
- Forged adapter decision is ignored.
- Revoked capability denies a new action.
- Child capability exceeding parent scope is rejected.
- Expired approval cannot resume the run.
- Tampered checkpoint cannot load.
- Crash after dispatch but before receipt produces `unknown`, not an automatic
  retry.
- Non-zero test exit produces invalid or missing evidence, never passing
  coverage.

## Acceptance output

`GatemoleKernelBench` will emit a versioned JSON result and Markdown summary with:

- Scenario and assertion counts.
- Run lifecycle states observed.
- Allowed and denied action counts.
- Unauthorized side effects, expected zero.
- Restart and replay result.
- Approval identity and scope validation result.
- Unknown-action reconciliation result.
- Evidence coverage and final release decision.
- Event-chain and materialized-state consistency result.
- Per-step timing as diagnostic data, not a performance claim.

## Delivery sequence

The scenario remains checked in before all steps are executable. Each
kernel/Runtime milestone converts another section from target to an automated
assertion. The final scenario is not replaced by easier component-only
demonstrations, and the README and production guide must not present target
steps as implemented.
