# Benchmarks

VouchBench is the repo-local acceptance harness for the current
compiler/evidence/policy behavior.

```sh
scripts/vouchbench.sh
```

Full benchmark documentation lives in
[`docs/BENCHMARKS.md`](https://github.com/duriantaco/vouch/blob/main/docs/BENCHMARKS.md).

## Current Acceptance Floor

- 10 required scenarios
- 114 scenario assertions
- at least 4 tests-passed scenarios blocked by Gatemole-specific checks
- at least 2 medium/high multi-component scenarios with 25 obligations
- canary, human escalation, and auto-merge routes all exercised
- at least 1 invalid-evidence negative control
- at least 1 full-coverage manifest traceability block

## Valid Claim

Vouch links compiled obligations to evidence deterministically for the fixture
corpus and routes the resulting policy decisions as expected.

## Invalid Claim

VouchBench does not prove arbitrary code correctness, automatic product
understanding, incident reduction, or product-market fit.
