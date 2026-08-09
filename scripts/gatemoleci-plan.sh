#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/gatemoleci-plan.sh --paths FILE

Classifies changed repository paths for the focused pull-request validation
workflow. The output is suitable for appending to GITHUB_OUTPUT.
EOF
}

if [[ $# -ne 2 || "$1" != "--paths" ]]; then
  usage >&2
  exit 2
fi

paths_file="$2"
if [[ ! -f "$paths_file" ]]; then
  echo "gatemoleci-plan: path list does not exist: $paths_file" >&2
  exit 2
fi

docs=false
gatemolebench=false
kernelbench=false
transactionbench=false
runtimebench=false
production=false
changed_count=0

select_runtime_acceptance() {
  kernelbench=true
  transactionbench=true
  runtimebench=true
}

select_everything() {
  gatemolebench=true
  select_runtime_acceptance
  production=true
}

while IFS= read -r path || [[ -n "$path" ]]; do
  [[ -n "$path" ]] || continue
  changed_count=$((changed_count + 1))

  case "$path" in
    README.md|ROADMAP.md|CONTRIBUTING.md|SECURITY.md|LICENSE|LICENSE.*)
      docs=true
      ;;
    docs/*|assets/*|mkdocs.yml)
      docs=true
      ;;
    schemas/README.md)
      docs=true
      ;;
    schemas/*)
      docs=true
      select_everything
      ;;
    demo_repo/*|benchmarks/results/gatemolebench.*)
      gatemolebench=true
      ;;
    scripts/gatemolebench.sh|scripts/gatemolebench-repo.sh)
      gatemolebench=true
      ;;
    internal/gatemole/artifacts.go|\
    internal/gatemole/bootstrap_cli_test.go|\
    internal/gatemole/compile.go|\
    internal/gatemole/compile_cli_test.go|\
    internal/gatemole/compiler.go|\
    internal/gatemole/contracts_cli.go|\
    internal/gatemole/contracts_cli_test.go|\
    internal/gatemole/diagnostics.go|\
    internal/gatemole/evidence*.go|\
    internal/gatemole/github_summary_test.go|\
    internal/gatemole/intent.go|\
    internal/gatemole/ir.go|\
    internal/gatemole/junit_map*.go|\
    internal/gatemole/load.go|\
    internal/gatemole/onboarding*.go|\
    internal/gatemole/plan.go|\
    internal/gatemole/policy.go|\
    internal/gatemole/render.go|\
    internal/gatemole/sarif.go|\
    internal/gatemole/try*.go|\
    internal/gatemole/types.go|\
    internal/gatemole/validate.go)
      gatemolebench=true
      ;;
    internal/kernel/broker/*|\
    internal/kernel/capability/*|\
    internal/kernel/client/*|\
    internal/kernel/driver/*|\
    internal/kernel/eventlog/*|\
    internal/kernel/reducer/*)
      kernelbench=true
      production=true
      ;;
    internal/kernel/transaction/*|\
    internal/kernel/api/transaction_handlers*|\
    internal/kernel/model/transaction*|\
    internal/kernel/store/transaction_store*)
      transactionbench=true
      production=true
      ;;
    internal/kernel/approval/*|\
    internal/kernel/daemon/*|\
    internal/kernel/identity/*|\
    internal/kernel/model/agent_task*|\
    internal/kernel/modelbroker/*|\
    internal/kernel/sandbox/*|\
    internal/kernel/verification/*)
      runtimebench=true
      production=true
      ;;
    internal/kernel/*)
      runtimebench=true
      production=true
      ;;
    internal/gatemole/action_cli.go|internal/gatemole/kernel_cli*.go)
      kernelbench=true
      production=true
      ;;
    internal/gatemole/transaction_cli.go|internal/gatemole/transaction_review_cli*.go)
      transactionbench=true
      production=true
      ;;
    internal/gatemole/approval_cli*.go|\
    internal/gatemole/daemon_cli.go|\
    internal/gatemole/identity_cli*.go|\
    internal/gatemole/transaction_profile_cli*.go|\
    internal/gatemole/transaction_run_cli*.go|\
    internal/gatemole/transaction_verify_cli*.go)
      runtimebench=true
      production=true
      ;;
    internal/gatemole/cli.go)
      gatemolebench=true
      runtimebench=true
      production=true
      ;;
    cmd/gatemoled/*|cmd/gatemole-model-broker/*)
      runtimebench=true
      production=true
      ;;
    cmd/gatemole/*)
      select_everything
      ;;
    scripts/gatemolekernelbench.sh)
      kernelbench=true
      production=true
      ;;
    scripts/gatemoletransactionbench.sh)
      transactionbench=true
      production=true
      ;;
    scripts/gatemoleruntimebench.sh)
      runtimebench=true
      production=true
      ;;
    build/*|scripts/gatemoleproductionbench.sh|scripts/gatemoleproductionfixture.sh)
      production=true
      ;;
    .gitignore|.editorconfig|CODEOWNERS|.github/CODEOWNERS)
      ;;
    *.md)
      docs=true
      ;;
    *)
      # Unknown code, configuration, workflow and executable paths fail closed.
      select_everything
      ;;
  esac
done < "$paths_file"

if [[ "$changed_count" -eq 0 ]]; then
  echo "gatemoleci-plan: no changed paths were supplied" >&2
  exit 1
fi

printf 'docs=%s\n' "$docs"
printf 'gatemolebench=%s\n' "$gatemolebench"
printf 'kernelbench=%s\n' "$kernelbench"
printf 'transactionbench=%s\n' "$transactionbench"
printf 'runtimebench=%s\n' "$runtimebench"
printf 'production=%s\n' "$production"
printf 'changed_count=%s\n' "$changed_count"
