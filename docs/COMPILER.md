<p align="center">
  <img src="../assets/vouch.png" alt="Vouch logo" width="180">
</p>

# Compiler Architecture

Vouch is a compiler for release contracts. The current gate is the runtime that
consumes the compiler output.

This is the split:

```text
compiler: intent -> AST -> spec -> obligation IR -> verification plan/artifacts
runtime:  manifest + evidence + policy -> release decision
```

Calling Vouch only a gate is incomplete. Calling it a compiler without naming
the current runtime is also incomplete.

## Source Language

The source language is human-owned YAML under `.vouch/intents/`.

Example:

```yaml
version: vouch.intent.v0
feature: auth.password_reset
owner: platform
owned_paths:
  - src/auth/**
  - tests/auth/**
risk: high
goal: Preserve password-reset safety during agent changes.

behavior:
  - user can request password reset by email
  - response does not reveal whether account exists

security:
  - reset token is stored hashed
  - reset token is never logged

required_tests:
  - token expires
  - unknown email receives same response shape

runtime_metrics:
  - password_reset.requested
  - password_reset.failed

rollback:
  strategy: feature_flag
  flag: password_reset_v2
```

The parser accepts only the intent keys implemented in
[`internal/vouch/intent.go`](../internal/vouch/intent.go): `version`, `feature`,
`owner`, `owned_paths`, `risk`, `goal`, `behavior`, `security`,
`required_tests`, `runtime_metrics`, `runtime_alerts`, and `rollback`.

## Compiler Stages

| Stage | Command | Main code | Output |
| --- | --- | --- | --- |
| Parse intent | `vouch contracts intent parse` | [`ParseIntentASTFile`](../internal/vouch/intent.go) | `vouch.ast.v0` with source spans and diagnostics |
| Analyze intent | repo compile path | [`AnalyzeIntentAST`](../internal/vouch/intent.go) | typed intent values |
| Compile spec | `vouch contracts intent compile` | [`SpecFromIntent`](../internal/vouch/intent.go) | `vouch.spec.v0` JSON |
| Build IR | `vouch contracts ir build` | [`IRFromSpec`](../internal/vouch/ir.go) | `vouch.ir.v0` obligations |
| Build plan | `vouch contracts plan build` | [`VerificationPlanFromIR`](../internal/vouch/plan.go) | `vouch.plan.v0` verification plan |
| Build artifacts | `vouch contracts artifacts build` | [`BuildArtifacts`](../internal/vouch/artifacts.go) | verifier packets, test obligations, release policy artifact |
| Compile repo | `vouch contracts compile` | [`CompileRepo`](../internal/vouch/compile.go) | `.vouch/build/` compiler outputs |

The CLI dispatcher for these commands is
[`Main`](../internal/vouch/cli.go).

## Repo Compile Output

`vouch contracts compile` reads `.vouch/intents/*.yaml` and writes:

- `.vouch/build/ast/*.ast.json`
- `.vouch/specs/*.spec.json`
- `.vouch/build/obligations.ir.json`
- `.vouch/build/verification-plan.json`

The repo-level compiler output structs live in
[`internal/vouch/compile.go`](../internal/vouch/compile.go).

## Obligation IR

`IRFromSpec` lowers a spec into stable obligation IDs. The current obligation
kinds are defined in [`internal/vouch/types.go`](../internal/vouch/types.go):

| Obligation kind | Required evidence kind |
| --- | --- |
| `behavior` | `behavior_trace` |
| `security` | `security_check` |
| `required_test` | `test_coverage` |
| `runtime_signal` | `runtime_metric` |
| `rollback` | `rollback_plan` |

Example IDs:

```text
auth.password_reset.behavior.user_can_request_password_reset_by_email
auth.password_reset.security.reset_token_is_never_logged
auth.password_reset.required_test.token_expires
auth.password_reset.runtime_signal.password_reset_failed
auth.password_reset.rollback.feature_flag_password_reset_v2
```

The ID format is intentional: evidence artifacts must reference exact obligation
IDs.

## Runtime Gate

The runtime starts after compilation.

`CollectEvidenceWithOptions` in
[`internal/vouch/evidence.go`](../internal/vouch/evidence.go) loads:

- compiled specs
- a change manifest
- generated IR and verification plans for touched specs
- linked evidence artifacts
- release policy

It then builds coverage, imports verifier findings, and applies policy.

Default policy is implemented in
[`DefaultReleasePolicy`](../internal/vouch/policy.go). The current decisions
are:

- `block`
- `human_escalation`
- `canary`
- `auto_merge`

`gate` exits non-zero only when the final decision is `block`.

## Evidence Model

Vouch can use manifest-attached artifacts or the simpler repo-level JUnit import
path.

Manifest-backed artifacts are attached with:

```sh
vouch --repo DIR contracts manifest attach-artifact \
  --manifest .vouch/manifests/run-123.json \
  --id pytest \
  --kind test_coverage \
  --path .vouch/artifacts/pytest.xml \
  --exit-code 0 \
  --out .vouch/manifests/run-123.json
```

The simpler JUnit path is:

```sh
vouch --repo DIR contracts evidence import junit .vouch/artifacts/pytest.xml
vouch --repo DIR contracts gate
```

JUnit covers `required_test` obligations only. Missing behavior, security,
runtime, or rollback evidence can still block.

`security_check` artifacts can also be SARIF 2.1.0 logs. Vouch treats SARIF as
scanner evidence only when rules or result properties reference exact compiled
obligation IDs. High or critical mapped SARIF results become blocking findings;
unmapped scanner output is not treated as contract evidence.

## What Is Proven

The repo-local benchmark is VouchBench:

```sh
scripts/vouchbench.sh
```

It proves the current compiler/evidence/policy path is deterministic over the
fixture corpus. See [Benchmarks](BENCHMARKS.md).

It does not prove:

- arbitrary code correctness
- automatic product-intent inference
- production incident reduction
- product-market fit

That broader claim needs shadow-mode pilots on real repos.
