# Gatemole Public Schemas

This directory contains the machine-readable contracts for public Gatemole
artifacts. Agent-kernel schemas use JSON Schema draft 2020-12 and reject unknown
fields unless a field is explicitly documented as driver-defined data.

## Kernel resources

| Resource | Schema version | File |
| --- | --- | --- |
| Agent image | `gatemole.agent_image.v0` | `gatemole.agent_image.v0.schema.json` |
| Execution contract | `gatemole.execution_contract.v0` | `gatemole.execution_contract.v0.schema.json` |
| Agent run | `gatemole.agent_run.v0` | `gatemole.agent_run.v0.schema.json` |
| Capability grant | `gatemole.capability_grant.v0` | `gatemole.capability_grant.v0.schema.json` |
| Action request | `gatemole.action_request.v0` | `gatemole.action_request.v0.schema.json` |
| Run event | `gatemole.run_event.v0` | `gatemole.run_event.v0.schema.json` |
| Checkpoint | `gatemole.checkpoint.v0` | `gatemole.checkpoint.v0.schema.json` |
| Policy decision | `gatemole.policy_decision.v0` | `gatemole.policy_decision.v0.schema.json` |
| Agent task | `gatemole.agent_task.v0` | `gatemole.agent_task.v0.schema.json` |
| Agent transaction | `gatemole.agent_transaction.v0` | `gatemole.agent_transaction.v0.schema.json` |
| Effect | `gatemole.effect.v0` | `gatemole.effect.v0.schema.json` |
| Verification result | `gatemole.verification_result.v0` | `gatemole.verification_result.v0.schema.json` |
| Approval package | `gatemole.approval_package.v0` | `gatemole.approval_package.v0.schema.json` |
| Commit plan | `gatemole.commit_plan.v0` | `gatemole.commit_plan.v0.schema.json` |
| Transaction event | `gatemole.transaction_event.v0` | `gatemole.transaction_event.v0.schema.json` |

Shared value definitions live in `gatemole.kernel.common.v0.schema.json`; that file
is not itself a kernel resource.

## Runtime configuration

| Resource | Schema version | File |
| --- | --- | --- |
| Agent profiles | `gatemole.agent_profiles.v0` | `gatemole.agent_profiles.v0.schema.json` |
| Verifier profiles | `gatemole.verifier_profiles.v0` | `gatemole.verifier_profiles.v0.schema.json` |

Agent and verifier profile names must be unique. Their loaders enforce semantic
constraints that JSON Schema cannot express, including matching each complete
OCI reference to its descriptor digest.

## Compatibility policy

Within one schema version:

- Required fields are not removed or reinterpreted.
- Enum values are not removed or silently renamed.
- Unknown fields remain errors.
- New optional fields may be introduced only when old readers can safely ignore
  their absence and strict readers are updated before writers emit them.
- Deterministic behavior may be clarified, but existing accepted inputs cannot
  change meaning without a version change.
- Resource identifiers, digests, event ordering, and lifecycle semantics are
  compatibility-sensitive.

A breaking change creates a new version such as `gatemole.agent_run.v1`. Readers
must select decoding and validation from the resource's `version` field; they
must not guess based on field presence.

Before a new writer becomes the default:

1. Publish its schema and migration notes.
2. Add valid, invalid, and previous-version compatibility fixtures.
3. Teach readers to accept the new version.
4. Exercise mixed-version replay and checkpoint behavior.
5. Only then emit the new version by default.

## Driver-defined fields

`ActionRequest.arguments`, `RunEvent.payload`, and selected policy facts are
intentionally open objects because their schemas depend on a named operation or
event type. The enclosing resource remains strict. The kernel must resolve the
corresponding driver/event schema and validate those objects before accepting
or executing them.

## Fixtures

Fixtures under `fixtures/kernel/valid` are canonical examples used by Go tests.
Fixtures under `fixtures/kernel/invalid` exercise strict decoding and semantic
validation that JSON Schema alone cannot express.
