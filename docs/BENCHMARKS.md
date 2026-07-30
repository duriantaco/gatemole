# VouchBench

VouchBench covers the release-contract compiler and gate. The experimental
agent kernel has a separate vertical acceptance harness:

```sh
scripts/vouchkernelbench.sh --out /tmp/vouchkernelbench
```

VouchKernelBench proves an allowed brokered filesystem write, a denied and
audited path escape, authorization-before-execution ordering, and exact run and
event recovery across a real daemon restart. Keeping the harnesses separate
prevents the new kernel claim from weakening or silently changing the existing
114-assertion release-gate acceptance floor.

The Vouch Runtime transaction path has a third, independent acceptance harness:

```sh
scripts/vouchtransactionbench.sh
```

VouchTransactionBench performs 10 checks through the real CLI, Unix-socket
daemon, SQLite store, and a temporary Git repository. It proves detached
staging, exact effect inventory, source isolation, post-freeze mutation
rejection, sequence-level control/test coupling detection, append-only history,
and byte-equivalent projection recovery after daemon restart. It does not claim
that production commit, compensation, or universal rollback is implemented.

VouchBench is the repo-local acceptance harness for Vouch's current release-gate claim:

> Tests can pass while release obligations remain uncovered; Vouch makes that gap explicit and blocks or routes the change according to policy.

It is not a benchmark for semantic code understanding. Vouch does not currently prove an implementation is correct. The harness measures whether the compiler, evidence linker, artifact validation, manifest traceability checks, and release policy make deterministic decisions over declared contracts and artifacts.

## Run It

From the repo root:

```sh
scripts/vouchbench.sh
```

The script builds the local CLI, runs isolated temp copies of the fixtures, asserts each scenario, and writes ignored outputs to:

```text
benchmarks/results/vouchbench.latest.json
benchmarks/results/vouchbench.latest.md
```

Use a separate output directory if you want to archive a run:

```sh
scripts/vouchbench.sh --out /tmp/vouchbench
```

Use `--keep` when debugging a failing benchmark. The script fails at the first scenario-level assertion mismatch.

## External Repo Evaluation

The fixture suite is intentionally tailored to Vouch's release-gate behavior. Use it for regression and acceptance. To evaluate a real repo, use the external repo evaluator:

```sh
scripts/vouchbench-repo.sh --repo /path/to/repo --out /tmp/vouchbench-repo
```

This copies the target repo into a temporary snapshot, runs Vouch only inside that snapshot, and writes:

```text
vouchbench-repo.latest.json
vouchbench-repo.latest.md
```

For a repo that can produce JUnit:

```sh
scripts/vouchbench-repo.sh \
  --repo /path/to/repo \
  --test-command "pytest --junitxml .gatemole/artifacts/pytest.xml" \
  --junit .gatemole/artifacts/pytest.xml \
  --out /tmp/vouchbench-repo
```

For an external repo, start non-destructively:

```sh
scripts/vouchbench-repo.sh --repo /path/to/repo --out /tmp/vouchbench-repo
```

That run answers different questions from the fixture acceptance suite:

- can Vouch snapshot, initialize, bootstrap, and compile this repo?
- how many draft contracts and obligations are generated?
- if JUnit is provided, how many required-test obligations link to real test evidence?
- if gate runs, what decision and exit code does this repo produce?

It is not a stable benchmark until the scenario, contracts, evidence, and expected decision are checked into the VouchBench corpus.

## Acceptance Contract

The benchmark is expected to exit non-zero if any of these regress:

- the required scenario corpus changes unexpectedly
- expected Vouch decisions do not match actual decisions
- expected gate process exit codes do not match actual exit codes
- expected coverage counts or missing obligation IDs change
- expected invalid evidence codes are missing
- expected policy rules fired are different
- manifest/spec error counts or required error text change
- aggregate acceptance criteria are no longer met

The current acceptance floor is:

- 10 required scenarios
- 114 scenario assertions
- at least 4 tests-passed scenarios blocked by Vouch-specific checks
- at least 2 medium/high multi-component scenarios with 25 obligations
- canary, human escalation, and auto-merge routes all exercised
- at least 1 invalid-evidence negative control
- at least 1 full-coverage manifest traceability block

`vouchbench.latest.json` is the machine-readable result. It includes:

- `acceptance.passed`: final pass/fail status
- `acceptance.criteria[]`: aggregate criteria such as required corpus, invalid-evidence coverage, and non-blocking route coverage
- `scenarios[].expected`: explicit expected values for that scenario
- `scenarios[].actual`: the compact gate result fields used by assertions
- `scenarios[].assertions[]`: per-field assertion records with expected and actual values

## Current Scenarios

| Scenario | Purpose | Baseline | Expected |
| --- | --- | --- | --- |
| `auth_tests_only` | Passing JUnit covers required tests, but behavior, security, runtime, and rollback evidence are absent. | tests pass, would continue without Vouch | `block` |
| `auth_partial_release_evidence` | Tests and some artifacts pass, but required test, security, and runtime obligations remain uncovered. | tests pass, would continue without Vouch | `block` |
| `auth_manifest_traceability_block` | All obligations are covered, but a changed billing file is not owned by any touched spec. | tests pass, would continue without Vouch | `block` |
| `auth_nonzero_test_artifact` | The test artifact command exits non-zero even though artifact files exist. | tests fail, baseline catches this | `block` |
| `auth_full_release_evidence` | All high-risk auth obligations are covered and canary is enabled. | tests pass, no Vouch policy route | `canary` |
| `auth_full_release_without_canary` | All high-risk auth obligations are covered, but canary is disabled. | tests pass, no Vouch policy route | `human_escalation` |
| `platform_multi_component_partial_evidence` | A synthetic medium/high platform repo changes auth, payments, and API. Tests pass, but the payments component lacks security, runtime, and rollback evidence. | tests pass, would continue without Vouch | `block` |
| `platform_multi_component_full_canary` | The same platform repo covers all 25 obligations across auth, payments, and API. | tests pass, no Vouch policy route | `canary` |
| `platform_medium_api_auto_merge` | The platform repo changes only the medium-risk API contract and provides complete API evidence. | tests pass, no Vouch scope proof | `auto_merge` |
| `docs_low_risk_full_evidence` | A low-risk docs contract has complete evidence and should not be falsely blocked. | tests pass, would continue without Vouch | `auto_merge` |

## Medium/High Repo Shape

The `platform_*` scenarios create a repo with three components:

| Component | Risk | Obligations | Owned Paths |
| --- | --- | ---: | --- |
| `auth.session` | high | 9 | `internal/auth/**`, `tests/auth/**` |
| `payments.checkout` | high | 9 | `internal/payments/**`, `tests/payments/**` |
| `api.users` | medium | 7 | `internal/api/**`, `tests/api/**` |

The multi-component platform manifest touches all three components, so the gate evaluates 25 obligations at once. The partial-evidence scenario intentionally covers 22/25 obligations and leaves only `payments.checkout` missing one security invariant, one runtime signal, and one rollback obligation. The complete-evidence scenario covers all 25 and routes to `canary`. The medium-only API scenario proves Vouch can scope the same repo down to 7 obligations and return `auto_merge`.

## Baseline Accounting

The benchmark separates Vouch-only catches from cases tests already catch.

Tests-passed block scenarios support the narrow adoption claim: a test-only baseline would continue, while Vouch blocks due to missing release evidence or manifest traceability.

The non-zero test artifact scenario is a negative control. It proves the harness validates artifact exit codes and invalid evidence, but it is not counted as a Vouch-only catch because tests already failed.

The non-blocking scenarios prove Vouch is not just a blocker. Complete evidence can route to canary, high-risk evidence without canary escalates to a human, and low-risk complete evidence auto-merges.

The platform scenarios add scale and scope checks: a multi-component high-risk release with 25 obligations, and a medium-risk change inside that same larger repo that should not inherit unrelated auth or payments obligations.

## Validity And Limitations

Valid claims from this benchmark:

- Vouch links compiled obligations to evidence deterministically for the fixture corpus.
- Missing evidence, invalid artifact exit codes, manifest traceability errors, and rollout policy routes are detected by explicit assertions.
- The CLI gate exit code is consistent with the release decision for the tested policy behavior.

Invalid claims from this benchmark:

- Vouch finds all bugs.
- Vouch understands product intent without human-owned contracts.
- The fixtures represent every release-risk category.
- The synthetic platform fixture is equivalent to a large production monorepo.
- The measured runtime is a stable performance benchmark.

The corpus is intentionally deterministic. Treat timing as a smoke signal only. The meaningful acceptance result is the assertion set, not the millisecond values.

## Extending The Corpus

Add scenarios when a real user concern appears. Good benchmark cases should state:

- the contract and risk level
- what a test-only or artifact-only baseline would do
- the missing, invalid, or misrouted evidence Vouch should catch
- the exact expected decision and gate exit code
- the exact expected coverage and missing obligation IDs
- the policy rule expected to fire
- why the case represents a realistic release failure mode

Avoid adding synthetic cases that only exercise parser branches. Unit tests are better for those. VouchBench should stay focused on adoption-facing evidence for why a contract/evidence gate is useful.
