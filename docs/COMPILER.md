<p align="center">
  <img src="../assets/gatemole.png" alt="Gatemole logo" width="180">
</p>

# Compiler Architecture

Gatemole is a compiler for release contracts. The current gate is the runtime that
consumes the compiler output.

This is the split:

```text
compiler: intent -> AST -> spec -> obligation IR -> verification plan/artifacts
runtime:  manifest + evidence + policy -> release decision
```

Calling Gatemole only a gate is incomplete. Calling it a compiler without naming
the current runtime is also incomplete.

## Source Language

The source language is human-owned YAML under `.gatemole/intents/`.

Example:

```yaml
version: gatemole.intent.v0
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
[`internal/gatemole/intent.go`](../internal/gatemole/intent.go): `version`, `feature`,
`owner`, `owned_paths`, `risk`, `goal`, `behavior`, `security`,
`required_tests`, `runtime_metrics`, `runtime_alerts`, and `rollback`.

## Compiler Stages

| Stage | Command | Main code | Output |
| --- | --- | --- | --- |
| Parse intent | `gatemole contracts intent parse` | [`ParseIntentASTFile`](../internal/gatemole/intent.go) | `gatemole.ast.v0` with source spans and diagnostics |
| Analyze intent | repo compile path | [`AnalyzeIntentAST`](../internal/gatemole/intent.go) | typed intent values |
| Compile spec | `gatemole contracts intent compile` | [`SpecFromIntent`](../internal/gatemole/intent.go) | `gatemole.spec.v0` JSON |
| Build IR | `gatemole contracts ir build` | [`IRFromSpec`](../internal/gatemole/ir.go) | `gatemole.ir.v0` obligations |
| Build plan | `gatemole contracts plan build` | [`VerificationPlanFromIR`](../internal/gatemole/plan.go) | `gatemole.plan.v0` verification plan |
| Build artifacts | `gatemole contracts artifacts build` | [`BuildArtifacts`](../internal/gatemole/artifacts.go) | verifier packets, test obligations, release policy artifact |
| Compile repo | `gatemole contracts compile` | [`CompileRepo`](../internal/gatemole/compile.go) | `.gatemole/build/` compiler outputs |

The CLI dispatcher for these commands is
[`Main`](../internal/gatemole/cli.go).

## Repo Compile Output

`gatemole contracts compile` reads `.gatemole/intents/*.yaml` and writes:

- `.gatemole/build/ast/*.ast.json`
- `.gatemole/specs/*.spec.json`
- `.gatemole/build/obligations.ir.json`
- `.gatemole/build/verification-plan.json`

The repo-level compiler output structs live in
[`internal/gatemole/compile.go`](../internal/gatemole/compile.go).

## Obligation IR

`IRFromSpec` lowers a spec into stable obligation IDs. The current obligation
kinds are defined in [`internal/gatemole/types.go`](../internal/gatemole/types.go):

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
[`internal/gatemole/evidence.go`](../internal/gatemole/evidence.go) loads:

- compiled specs
- a change manifest
- generated IR and verification plans for touched specs
- linked evidence artifacts
- release policy

It then builds coverage, imports verifier findings, and applies policy.

Default policy is implemented in
[`DefaultReleasePolicy`](../internal/gatemole/policy.go). The current decisions
are:

- `block`
- `human_escalation`
- `canary`
- `auto_merge`

`gate` exits non-zero only when the final decision is `block`.

## Evidence Model

Gatemole can use manifest-attached artifacts or the simpler repo-level JUnit import
path.

Manifest-backed artifacts are attached with:

```sh
gatemole --repo DIR contracts manifest attach-artifact \
  --manifest .gatemole/manifests/run-123.json \
  --id pytest \
  --kind test_coverage \
  --path .gatemole/artifacts/pytest.xml \
  --exit-code 0 \
  --out .gatemole/manifests/run-123.json
```

The simpler JUnit path is:

```sh
gatemole --repo DIR contracts evidence import junit .gatemole/artifacts/pytest.xml
gatemole --repo DIR contracts gate
```

JUnit covers `required_test` obligations only. Missing behavior, security,
runtime, or rollback evidence can still block.

`security_check` artifacts can also be SARIF 2.1.0 logs. Gatemole treats SARIF as
scanner evidence only when rules or result properties reference exact compiled
obligation IDs. High or critical mapped SARIF results become blocking findings;
unmapped scanner output is not treated as contract evidence.

## What Is Proven

The repo-local benchmark is GatemoleBench:

```sh
scripts/gatemolebench.sh
```

It proves the current compiler/evidence/policy path is deterministic over the
fixture corpus. See [Benchmarks](BENCHMARKS.md).

It does not prove:

- arbitrary code correctness
- automatic product-intent inference
- production incident reduction
- product-market fit

That broader claim needs shadow-mode pilots on real repos.
