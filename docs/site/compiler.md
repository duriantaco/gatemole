# Compiler Architecture

Vouch is a compiler for release contracts. The current gate is the runtime that
consumes the compiler output.

```text
compiler: intent -> AST -> spec -> obligation IR -> verification plan/artifacts
runtime:  manifest + evidence + policy -> release decision
```

Calling Vouch only a gate is incomplete. Calling it a compiler without naming
the current runtime is also incomplete.

## Source Language

Source files live under `.vouch/intents/*.yaml`. The parser accepts the keys
implemented in
[`internal/vouch/intent.go`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/intent.go):
`version`, `feature`, `owner`, `owned_paths`, `risk`, `goal`, `behavior`,
`security`, `required_tests`, `runtime_metrics`, `runtime_alerts`, and
`rollback`.

```yaml
version: gatemole.intent.v0
feature: auth.password_reset
owner: platform
owned_paths:
  - src/auth/**
  - tests/auth/**
risk: high
behavior:
  - response does not reveal whether account exists
security:
  - reset token is never logged
required_tests:
  - token expires
runtime_metrics:
  - password_reset.failed
rollback:
  strategy: feature_flag
  flag: password_reset_v2
```

## Compiler Stages

| Stage | Command | Code path | Output |
| --- | --- | --- | --- |
| Parse intent | `vouch contracts intent parse` | [`ParseIntentASTFile`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/intent.go) | `gatemole.ast.v0` with source spans and diagnostics |
| Analyze intent | repo compile path | [`AnalyzeIntentAST`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/intent.go) | typed intent values |
| Compile spec | `vouch contracts intent compile` | [`SpecFromIntent`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/intent.go) | `gatemole.spec.v0` JSON |
| Build IR | `vouch contracts ir build` | [`IRFromSpec`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/ir.go) | `gatemole.ir.v0` obligations |
| Build plan | `vouch contracts plan build` | [`VerificationPlanFromIR`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/plan.go) | `gatemole.plan.v0` verification plan |
| Build artifacts | `vouch contracts artifacts build` | [`BuildArtifacts`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/artifacts.go) | verifier packets, test obligations, release-policy artifact |
| Compile repo | `vouch contracts compile` | [`CompileRepo`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/compile.go) | `.vouch/build/` compiler outputs |

The CLI dispatcher for these commands is
[`Main`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/cli.go).

## Repo Compile Output

`vouch contracts compile` reads `.vouch/intents/*.yaml` and writes:

- `.vouch/build/ast/*.ast.json`
- `.vouch/specs/*.spec.json`
- `.vouch/build/obligations.ir.json`
- `.vouch/build/verification-plan.json`

## Obligation IR

`IRFromSpec` lowers a spec into stable obligation IDs. The current obligation
kinds are defined in
[`internal/vouch/types.go`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/types.go).

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

Evidence artifacts must reference exact obligation IDs.

## Runtime Gate

The runtime starts after compilation.

[`CollectEvidenceWithOptions`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/evidence.go)
loads compiled specs, a change manifest, generated IR/plans for touched specs,
linked evidence artifacts, and release policy. It then builds coverage, imports
verifier findings, and applies policy.

Default policy is implemented in
[`DefaultReleasePolicy`](https://github.com/duriantaco/vouch/blob/main/internal/vouch/policy.go).
The current decisions are:

- `block`
- `human_escalation`
- `canary`
- `auto_merge`

`gate` exits non-zero only when the final decision is `block`.

## Evidence Model

Manifest-backed `security_check` artifacts can be generic exact-ID JSON or
SARIF 2.1.0 scanner logs. SARIF rules and result properties must reference exact
compiled obligation IDs. High or critical mapped SARIF results enter the normal
blocking finding and policy path; unmapped scanner output does not satisfy a
contract obligation.
