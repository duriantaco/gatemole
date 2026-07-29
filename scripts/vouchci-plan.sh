#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/vouchci-plan.sh --paths FILE

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
  echo "vouchci-plan: path list does not exist: $paths_file" >&2
  exit 2
fi

docs=false
vouchbench=false
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
  vouchbench=true
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
    demo_repo/*|benchmarks/results/vouchbench.*)
      vouchbench=true
      ;;
    scripts/vouchbench.sh|scripts/vouchbench-repo.sh)
      vouchbench=true
      ;;
    internal/vouch/artifacts.go|\
    internal/vouch/bootstrap_cli_test.go|\
    internal/vouch/compile.go|\
    internal/vouch/compile_cli_test.go|\
    internal/vouch/compiler.go|\
    internal/vouch/contracts_cli.go|\
    internal/vouch/contracts_cli_test.go|\
    internal/vouch/diagnostics.go|\
    internal/vouch/evidence*.go|\
    internal/vouch/github_summary_test.go|\
    internal/vouch/intent.go|\
    internal/vouch/ir.go|\
    internal/vouch/junit_map*.go|\
    internal/vouch/load.go|\
    internal/vouch/onboarding*.go|\
    internal/vouch/plan.go|\
    internal/vouch/policy.go|\
    internal/vouch/render.go|\
    internal/vouch/sarif.go|\
    internal/vouch/try*.go|\
    internal/vouch/types.go|\
    internal/vouch/validate.go)
      vouchbench=true
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
    internal/vouch/action_cli.go|internal/vouch/kernel_cli*.go)
      kernelbench=true
      production=true
      ;;
    internal/vouch/transaction_cli.go)
      transactionbench=true
      production=true
      ;;
    internal/vouch/approval_cli*.go|\
    internal/vouch/daemon_cli.go|\
    internal/vouch/identity_cli*.go|\
    internal/vouch/transaction_profile_cli*.go|\
    internal/vouch/transaction_run_cli*.go|\
    internal/vouch/transaction_verify_cli*.go)
      runtimebench=true
      production=true
      ;;
    internal/vouch/cli.go)
      vouchbench=true
      runtimebench=true
      production=true
      ;;
    cmd/vouchd/*|cmd/vouch-model-broker/*)
      runtimebench=true
      production=true
      ;;
    cmd/vouch/*)
      select_everything
      ;;
    scripts/vouchkernelbench.sh)
      kernelbench=true
      production=true
      ;;
    scripts/vouchtransactionbench.sh)
      transactionbench=true
      production=true
      ;;
    scripts/vouchruntimebench.sh)
      runtimebench=true
      production=true
      ;;
    build/*|scripts/vouchproductionbench.sh|scripts/vouchproductionfixture.sh)
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
  echo "vouchci-plan: no changed paths were supplied" >&2
  exit 1
fi

printf 'docs=%s\n' "$docs"
printf 'vouchbench=%s\n' "$vouchbench"
printf 'kernelbench=%s\n' "$kernelbench"
printf 'transactionbench=%s\n' "$transactionbench"
printf 'runtimebench=%s\n' "$runtimebench"
printf 'production=%s\n' "$production"
printf 'changed_count=%s\n' "$changed_count"
