#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/gatemolebench-repo.sh --repo DIR [--out DIR] [--test-command CMD] [--junit PATH] [--keep]

Runs a non-destructive Gatemole evaluation against an external repository.

The source repo is copied into a temporary snapshot first. Gatemole writes only to
that snapshot, never to the source repo.

Options:
  --repo DIR          External repository to evaluate.
  --out DIR           Output directory. Default: benchmarks/results
  --test-command CMD  Optional test command to run inside the snapshot.
  --junit PATH        Optional JUnit XML path, relative to the snapshot, to import.
  --keep              Keep the temporary working directory for inspection.
EOF
}

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SOURCE_REPO=""
OUT_DIR="$ROOT/benchmarks/results"
TEST_COMMAND=""
JUNIT_PATH=""
KEEP=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      if [[ $# -lt 2 ]]; then
        echo "gatemolebench-repo: --repo requires a directory" >&2
        exit 2
      fi
      SOURCE_REPO="$2"
      shift 2
      ;;
    --out)
      if [[ $# -lt 2 ]]; then
        echo "gatemolebench-repo: --out requires a directory" >&2
        exit 2
      fi
      OUT_DIR="$2"
      shift 2
      ;;
    --test-command)
      if [[ $# -lt 2 ]]; then
        echo "gatemolebench-repo: --test-command requires a command string" >&2
        exit 2
      fi
      TEST_COMMAND="$2"
      shift 2
      ;;
    --junit)
      if [[ $# -lt 2 ]]; then
        echo "gatemolebench-repo: --junit requires a path" >&2
        exit 2
      fi
      JUNIT_PATH="$2"
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
      echo "gatemolebench-repo: unknown argument $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$SOURCE_REPO" ]]; then
  echo "gatemolebench-repo: --repo is required" >&2
  usage >&2
  exit 2
fi
if [[ ! -d "$SOURCE_REPO" ]]; then
  echo "gatemolebench-repo: repo does not exist: $SOURCE_REPO" >&2
  exit 2
fi

mkdir -p "$OUT_DIR"
RUN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/gatemolebench-repo.XXXXXX")"
SNAPSHOT="$RUN_DIR/repo"
GATEMOLE="$RUN_DIR/gatemole"
STEPS_DIR="$RUN_DIR/steps"
mkdir -p "$STEPS_DIR"

cleanup() {
  if [[ "$KEEP" == "1" ]]; then
    echo "kept external repo eval workdir: $RUN_DIR" >&2
  else
    rm -rf "$RUN_DIR"
  fi
}
trap cleanup EXIT

now_ns() {
  python3 -c 'import time; print(time.perf_counter_ns())'
}

run_step() {
  local name="$1"
  shift
  local start_ns
  local end_ns
  local code
  start_ns="$(now_ns)"
  set +e
  "$@" > "$STEPS_DIR/$name.stdout" 2> "$STEPS_DIR/$name.stderr"
  code=$?
  set -e
  end_ns="$(now_ns)"
  python3 - "$STEPS_DIR/$name.json" "$name" "$start_ns" "$end_ns" "$code" "$STEPS_DIR/$name.stdout" "$STEPS_DIR/$name.stderr" <<'PY'
import json
import sys

path, name, start_ns, end_ns, code, stdout_path, stderr_path = sys.argv[1:]
with open(stdout_path, encoding="utf-8", errors="replace") as f:
    stdout = f.read()
with open(stderr_path, encoding="utf-8", errors="replace") as f:
    stderr = f.read()
data = {
    "name": name,
    "exit_code": int(code),
    "elapsed_ms": round((int(end_ns) - int(start_ns)) / 1_000_000, 3),
    "stdout_path": stdout_path,
    "stderr_path": stderr_path,
    "stdout_tail": stdout[-4000:],
    "stderr_tail": stderr[-4000:],
}
with open(path, "w", encoding="utf-8") as f:
    json.dump(data, f, indent=2, sort_keys=True)
    f.write("\n")
PY
  return 0
}

copy_snapshot() {
  python3 - "$SOURCE_REPO" "$SNAPSHOT" "$RUN_DIR/snapshot.json" <<'PY'
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

source = Path(sys.argv[1]).resolve()
dest = Path(sys.argv[2]).resolve()
snapshot_path = Path(sys.argv[3])
dest.mkdir(parents=True, exist_ok=True)

def git(args):
    return subprocess.run(
        ["git", "-C", str(source), *args],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )

is_git = git(["rev-parse", "--is-inside-work-tree"]).returncode == 0
files = []
mode = "filesystem"
branch = ""
dirty_entries = []

if is_git:
    mode = "git-ls-files"
    branch_proc = git(["branch", "--show-current"])
    branch = branch_proc.stdout.strip()
    status_proc = git(["status", "--short"])
    dirty_entries = [line for line in status_proc.stdout.splitlines() if line.strip()]
    tracked = git(["ls-files", "-z", "--cached", "--others", "--exclude-standard"])
    if tracked.returncode != 0:
        raise SystemExit(tracked.stderr)
    files = [item for item in tracked.stdout.split("\0") if item]
else:
    ignored_dirs = {".git", "node_modules", ".venv", "venv", "target", "dist", "build", "__pycache__"}
    for root, dirs, names in os.walk(source):
        dirs[:] = [name for name in dirs if name not in ignored_dirs]
        for name in names:
            path = Path(root) / name
            files.append(str(path.relative_to(source)))

copied = 0
skipped = 0
for rel in files:
    src = source / rel
    dst = dest / rel
    if not src.exists() or not src.is_file():
        skipped += 1
        continue
    dst.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src, dst)
    copied += 1

data = {
    "source": str(source),
    "snapshot": str(dest),
    "mode": mode,
    "branch": branch,
    "files_copied": copied,
    "files_skipped": skipped,
    "dirty_entries": dirty_entries,
}
with open(snapshot_path, "w", encoding="utf-8") as f:
    json.dump(data, f, indent=2, sort_keys=True)
    f.write("\n")
PY
}

render_results() {
  local json_out="$OUT_DIR/gatemolebench-repo.latest.json"
  local md_out="$OUT_DIR/gatemolebench-repo.latest.md"
  python3 - "$RUN_DIR" "$json_out" "$md_out" "$SOURCE_REPO" "$TEST_COMMAND" "$JUNIT_PATH" <<'PY'
from __future__ import annotations

import datetime as dt
import json
import sys
from pathlib import Path

run_dir = Path(sys.argv[1])
json_out = Path(sys.argv[2])
md_out = Path(sys.argv[3])
source_repo = sys.argv[4]
test_command = sys.argv[5]
junit_path = sys.argv[6]
steps_dir = run_dir / "steps"

def load(path: Path, default=None):
    if not path.exists():
        return default
    with path.open(encoding="utf-8") as f:
        return json.load(f)

def parse_json_stdout(step: dict | None):
    if not step or step.get("exit_code") != 0:
        return None
    stdout_path = Path(step["stdout_path"])
    try:
        with stdout_path.open(encoding="utf-8") as f:
            return json.load(f)
    except Exception:
        return None

step_order = [
    "build_gatemole",
    "init",
    "bootstrap_dry_run",
    "bootstrap",
    "compile",
    "test_command",
    "evidence_import",
    "gate",
]
steps = [load(steps_dir / f"{name}.json") for name in step_order if (steps_dir / f"{name}.json").exists()]
steps_by_name = {step["name"]: step for step in steps}
snapshot = load(run_dir / "snapshot.json", {})
compile_json = parse_json_stdout(steps_by_name.get("compile")) or {}
bootstrap_json = parse_json_stdout(steps_by_name.get("bootstrap_dry_run")) or {}
import_json = parse_json_stdout(steps_by_name.get("evidence_import")) or {}
gate_json = parse_json_stdout(steps_by_name.get("gate")) or {}

summary = {
    "source_repo": source_repo,
    "snapshot": snapshot.get("snapshot", ""),
    "snapshot_mode": snapshot.get("mode", ""),
    "source_branch": snapshot.get("branch", ""),
    "source_dirty_entries": len(snapshot.get("dirty_entries", [])),
    "files_copied": snapshot.get("files_copied", 0),
    "bootstrap_drafts": len(bootstrap_json.get("drafts", [])) if isinstance(bootstrap_json, dict) else 0,
    "compiled_specs": compile_json.get("specs_compiled", 0),
    "compiled_obligations": compile_json.get("obligations_built", 0),
    "junit_links": len(import_json.get("links", [])) if isinstance(import_json, dict) else 0,
    "gate_decision": gate_json.get("decision", ""),
    "gate_exit_code": steps_by_name.get("gate", {}).get("exit_code"),
}
status = "completed"
if steps_by_name.get("compile", {}).get("exit_code", 1) != 0:
    status = "compile_failed"
elif junit_path and steps_by_name.get("evidence_import", {}).get("exit_code", 1) != 0:
    status = "evidence_import_failed"
elif junit_path and steps_by_name.get("gate", {}).get("exit_code") is None:
    status = "gate_not_run"

result = {
    "version": "gatemolebench.repo_eval.v0",
    "generated_at": dt.datetime.now(dt.timezone.utc).isoformat(),
    "status": status,
    "summary": summary,
    "inputs": {
        "repo": source_repo,
        "test_command": test_command,
        "junit": junit_path,
    },
    "snapshot": snapshot,
    "steps": steps,
}

json_out.parent.mkdir(parents=True, exist_ok=True)
with json_out.open("w", encoding="utf-8") as f:
    json.dump(result, f, indent=2, sort_keys=True)
    f.write("\n")

lines = [
    "# GatemoleBench External Repo Evaluation",
    "",
    f"Source repo: `{source_repo}`",
    f"Status: `{status}`",
    "",
    "## Summary",
    "",
    "| Field | Value |",
    "| --- | --- |",
    f"| Snapshot mode | `{summary['snapshot_mode']}` |",
    f"| Source branch | `{summary['source_branch']}` |",
    f"| Source dirty entries | `{summary['source_dirty_entries']}` |",
    f"| Files copied | `{summary['files_copied']}` |",
    f"| Bootstrap drafts | `{summary['bootstrap_drafts']}` |",
    f"| Compiled specs | `{summary['compiled_specs']}` |",
    f"| Compiled obligations | `{summary['compiled_obligations']}` |",
    f"| JUnit links | `{summary['junit_links']}` |",
    f"| Gate decision | `{summary['gate_decision']}` |",
    f"| Gate exit code | `{summary['gate_exit_code']}` |",
    "",
    "## Steps",
    "",
    "| Step | Exit | ms |",
    "| --- | ---: | ---: |",
]
for step in steps:
    lines.append(f"| `{step['name']}` | {step['exit_code']} | {step['elapsed_ms']} |")
lines.extend([
    "",
    "## Notes",
    "",
    "- This is a non-destructive snapshot evaluation, not a committed benchmark fixture.",
    "- A clean result here means Gatemole can bootstrap/compile/gate the snapshot under the provided inputs.",
    "- It does not validate product correctness, and it does not replace the deterministic fixture acceptance suite.",
])
md_out.write_text("\n".join(lines) + "\n", encoding="utf-8")
print(md_out.read_text(encoding="utf-8"))
PY
}

echo "snapshotting external repo..."
copy_snapshot

echo "building local gatemole binary..."
run_step build_gatemole bash -lc "cd '$ROOT' && GOCACHE='${GOCACHE:-$RUN_DIR/gocache}' go build -o '$GATEMOLE' ./cmd/gatemole"

echo "running gatemole init/bootstrap/compile on snapshot..."
run_step init "$GATEMOLE" --repo "$SNAPSHOT" --json init
run_step bootstrap_dry_run "$GATEMOLE" --repo "$SNAPSHOT" --json bootstrap --dry-run
run_step bootstrap "$GATEMOLE" --repo "$SNAPSHOT" --json bootstrap
run_step compile "$GATEMOLE" --repo "$SNAPSHOT" --json compile

if [[ -n "$TEST_COMMAND" ]]; then
  echo "running external repo test command in snapshot..."
  run_step test_command bash -lc "cd '$SNAPSHOT' && $TEST_COMMAND"
fi

if [[ -n "$JUNIT_PATH" ]]; then
  echo "importing JUnit evidence and running manifestless gate..."
  run_step evidence_import "$GATEMOLE" --repo "$SNAPSHOT" --json evidence import junit "$JUNIT_PATH"
  run_step gate "$GATEMOLE" --repo "$SNAPSHOT" gate --json
fi

render_results
echo "wrote:"
echo "  $OUT_DIR/gatemolebench-repo.latest.json"
echo "  $OUT_DIR/gatemolebench-repo.latest.md"
