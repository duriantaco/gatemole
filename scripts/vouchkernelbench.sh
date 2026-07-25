#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf '%s\n' 'usage: scripts/vouchkernelbench.sh [--out DIR] [--keep]'
}

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="$ROOT/benchmarks/results"
KEEP=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)
      [[ $# -ge 2 ]] || { echo 'vouchkernelbench: --out requires a directory' >&2; exit 2; }
      OUT_DIR="$2"
      shift 2
      ;;
    --keep)
      KEEP=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "vouchkernelbench: unknown argument $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

mkdir -p "$OUT_DIR"
RUN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/vouchkernelbench.XXXXXX")"
REPO="$RUN_DIR/repo"
VOUCH="$RUN_DIR/vouch"
VOUCHD="$RUN_DIR/vouchd"
SOCKET="$REPO/.vouch/vouchd.sock"
DATABASE="$REPO/.vouch/kernel.db"
DAEMON_PID=""

stop_daemon() {
  if [[ -n "$DAEMON_PID" ]] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill -TERM "$DAEMON_PID"
    wait "$DAEMON_PID"
  fi
  DAEMON_PID=""
}

cleanup() {
  stop_daemon
  if [[ "$KEEP" == "1" ]]; then
    echo "kept kernel benchmark workdir: $RUN_DIR" >&2
  else
    rm -rf "$RUN_DIR"
  fi
}
trap cleanup EXIT

start_daemon() {
  "$VOUCHD" --repo "$REPO" --db "$DATABASE" --socket "$SOCKET" \
    >> "$RUN_DIR/vouchd.stdout" 2>> "$RUN_DIR/vouchd.stderr" &
  DAEMON_PID=$!
  for _ in $(seq 1 100); do
    if [[ -S "$SOCKET" ]]; then
      return 0
    fi
    if ! kill -0 "$DAEMON_PID" 2>/dev/null; then
      echo 'vouchkernelbench: daemon exited during startup' >&2
      return 1
    fi
    sleep 0.05
  done
  echo 'vouchkernelbench: daemon socket did not become ready' >&2
  return 1
}

mkdir -p "$REPO/workspace"
printf '%s\n' 'governed kernel output' > "$REPO/input.txt"

python3 - "$REPO" <<'PY'
import datetime
import json
import pathlib
import sys

repo = pathlib.Path(sys.argv[1])
now = datetime.datetime.now(datetime.timezone.utc)
deadline = (now + datetime.timedelta(hours=1)).isoformat().replace('+00:00', 'Z')
timestamp = now.isoformat().replace('+00:00', 'Z')
contract_digest = 'sha256:' + ('d' * 64)
run = {
    'version': 'vouch.agent_run.v0',
    'id': 'run:kernelbench-001',
    'namespace': 'kernelbench',
    'image_digest': 'sha256:' + ('a' * 64),
    'contract_digest': contract_digest,
    'principal': {'id': 'run:kernelbench-001', 'kind': 'run', 'issuer': 'vouchd'},
    'delegation_chain': [{'id': 'human:kernelbench', 'kind': 'human'}],
    'state': 'created',
    'deadline': deadline,
    'budget_limits': {'max_tool_calls': 20},
    'budget_usage': {
        'input_tokens': 0, 'output_tokens': 0, 'model_calls': 0,
        'tool_calls': 0, 'cost_micros': 0, 'wall_time_seconds': 0,
    },
    'workspace': 'workspace',
    'capability_ids': [],
    'outstanding_approval_ids': [],
    'event_sequence': 1,
    'created_at': timestamp,
    'updated_at': timestamp,
}
contract = {
    'version': 'vouch.execution_contract.v0',
    'id': 'contract:kernelbench',
    'digest': contract_digest,
    'owner': {'id': 'human:kernelbench', 'kind': 'human'},
    'goal': 'Write only inside the benchmark workspace.',
    'non_goals': ['Never write outside the workspace.'],
    'risk': 'high',
    'resources': [{
        'id': 'workspace',
        'selector': {'kind': 'filesystem', 'pattern': 'workspace/**'},
        'operations': ['filesystem.read', 'filesystem.write'],
        'conditions': {'workspace_root': 'workspace', 'max_output_bytes': 1048576},
    }],
    'budgets': {'max_tool_calls': 20},
    'deadline': deadline,
}
for name, value in [('run.json', run), ('contract.json', contract)]:
    with (repo / name).open('w', encoding='utf-8') as handle:
        json.dump(value, handle, indent=2, sort_keys=True)
        handle.write('\n')
PY

(
  cd "$ROOT"
  go build -o "$VOUCH" ./cmd/vouch
  go build -o "$VOUCHD" ./cmd/vouchd
)

start_daemon
"$VOUCH" --repo "$REPO" --json run create --file "$REPO/run.json" > "$RUN_DIR/create.json"
"$VOUCH" --repo "$REPO" --json run transition --namespace kernelbench --id run:kernelbench-001 --to admitted > "$RUN_DIR/admitted.json"
"$VOUCH" --repo "$REPO" --json run transition --namespace kernelbench --id run:kernelbench-001 --to running > "$RUN_DIR/running.json"
"$VOUCH" --repo "$REPO" --json run grant --namespace kernelbench --id run:kernelbench-001 --contract "$REPO/contract.json" > "$RUN_DIR/grants.json"
"$VOUCH" --repo "$REPO" --json action fs-write \
  --namespace kernelbench --run run:kernelbench-001 \
  --path workspace/allowed.txt --input "$REPO/input.txt" \
  --action-id action:kernelbench-allowed --idempotency-key idem:kernelbench-allowed \
  > "$RUN_DIR/allowed.json"

set +e
"$VOUCH" --repo "$REPO" --json action fs-write \
  --namespace kernelbench --run run:kernelbench-001 \
  --path workspace/../escaped.txt --input "$REPO/input.txt" \
  --action-id action:kernelbench-escape --idempotency-key idem:kernelbench-escape \
  > "$RUN_DIR/denied.json" 2> "$RUN_DIR/denied.stderr"
DENIED_CODE=$?
set -e

"$VOUCH" --repo "$REPO" --json run get --namespace kernelbench --id run:kernelbench-001 > "$RUN_DIR/before-run.json"
"$VOUCH" --repo "$REPO" --json run events --namespace kernelbench --id run:kernelbench-001 > "$RUN_DIR/before-events.json"
stop_daemon
start_daemon
"$VOUCH" --repo "$REPO" --json run get --namespace kernelbench --id run:kernelbench-001 > "$RUN_DIR/after-run.json"
"$VOUCH" --repo "$REPO" --json run events --namespace kernelbench --id run:kernelbench-001 > "$RUN_DIR/after-events.json"

python3 - "$RUN_DIR" "$REPO" "$OUT_DIR" "$DENIED_CODE" <<'PY'
import json
import pathlib
import sys

run_dir = pathlib.Path(sys.argv[1])
repo = pathlib.Path(sys.argv[2])
out_dir = pathlib.Path(sys.argv[3])
denied_code = int(sys.argv[4])

def load(name):
    with (run_dir / name).open(encoding='utf-8') as handle:
        return json.load(handle)

before_run = load('before-run.json')
after_run = load('after-run.json')
before_events = load('before-events.json')
after_events = load('after-events.json')
allowed = load('allowed.json')
event_types = [event['type'] for event in after_events]
assertions = {
    'allowed_write_committed': allowed.get('status') == 'committed',
    'allowed_file_exact': (repo / 'workspace' / 'allowed.txt').read_bytes() == (repo / 'input.txt').read_bytes(),
    'escape_exit_denied': denied_code == 1 and 'KERNEL_CAPABILITY_DENIED' in (run_dir / 'denied.stderr').read_text(),
    'escape_has_no_effect': not (repo / 'escaped.txt').exists(),
    'denial_audited': event_types[-2:] == ['action.requested', 'action.denied'],
    'authorization_precedes_execution': event_types.index('action.authorized') < event_types.index('action.executing') < event_types.index('action.committed'),
    'projection_survives_restart': before_run == after_run,
    'history_survives_restart': before_events == after_events,
}
result = {
    'name': 'VouchKernelBench',
    'passed': all(assertions.values()),
    'assertions_passed': sum(assertions.values()),
    'assertions_total': len(assertions),
    'assertions': assertions,
    'run_state': after_run['run']['state'],
    'event_sequence': after_run['run']['event_sequence'],
    'event_types': event_types,
}
out_dir.mkdir(parents=True, exist_ok=True)
with (out_dir / 'vouchkernelbench.latest.json').open('w', encoding='utf-8') as handle:
    json.dump(result, handle, indent=2, sort_keys=True)
    handle.write('\n')
markdown = [
    '# VouchKernelBench', '',
    f"Result: {'PASS' if result['passed'] else 'FAIL'}",
    f"Assertions: {result['assertions_passed']}/{result['assertions_total']}", '',
]
for name, passed in assertions.items():
    markdown.append(f"- {'PASS' if passed else 'FAIL'} `{name}`")
(out_dir / 'vouchkernelbench.latest.md').write_text('\n'.join(markdown) + '\n', encoding='utf-8')
print(f"VouchKernelBench: {result['assertions_passed']}/{result['assertions_total']} assertions passed")
if not result['passed']:
    raise SystemExit(1)
PY
