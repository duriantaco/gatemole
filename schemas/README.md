# Vouch Public Schemas

This directory contains the machine-readable contracts for public Vouch
artifacts. Agent-kernel schemas use JSON Schema draft 2020-12 and reject unknown
fields unless a field is explicitly documented as driver-defined data.

## Kernel resources

| Resource | Schema version | File |
| --- | --- | --- |
| Agent image | `vouch.agent_image.v0` | `vouch.agent_image.v0.schema.json` |
| Execution contract | `vouch.execution_contract.v0` | `vouch.execution_contract.v0.schema.json` |
| Agent run | `vouch.agent_run.v0` | `vouch.agent_run.v0.schema.json` |
| Capability grant | `vouch.capability_grant.v0` | `vouch.capability_grant.v0.schema.json` |
| Action request | `vouch.action_request.v0` | `vouch.action_request.v0.schema.json` |
| Run event | `vouch.run_event.v0` | `vouch.run_event.v0.schema.json` |
| Checkpoint | `vouch.checkpoint.v0` | `vouch.checkpoint.v0.schema.json` |
| Policy decision | `vouch.policy_decision.v0` | `vouch.policy_decision.v0.schema.json` |
| Agent task | `vouch.agent_task.v0` | `vouch.agent_task.v0.schema.json` |
| Agent transaction | `vouch.agent_transaction.v0` | `vouch.agent_transaction.v0.schema.json` |
| Effect | `vouch.effect.v0` | `vouch.effect.v0.schema.json` |
| Verification result | `vouch.verification_result.v0` | `vouch.verification_result.v0.schema.json` |
| Approval package | `vouch.approval_package.v0` | `vouch.approval_package.v0.schema.json` |
| Commit plan | `vouch.commit_plan.v0` | `vouch.commit_plan.v0.schema.json` |
| Transaction event | `vouch.transaction_event.v0` | `vouch.transaction_event.v0.schema.json` |

Shared value definitions live in `vouch.kernel.common.v0.schema.json`; that file
is not itself a kernel resource.

## Runtime configuration

| Resource | Schema version | File |
| --- | --- | --- |
| Agent profiles | `vouch.agent_profiles.v0` | `vouch.agent_profiles.v0.schema.json` |
| Verifier profiles | `vouch.verifier_profiles.v0` | `vouch.verifier_profiles.v0.schema.json` |

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

A breaking change creates a new version such as `vouch.agent_run.v1`. Readers
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
