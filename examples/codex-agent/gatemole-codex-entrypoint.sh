#!/bin/sh
set -eu

if [ "${GATEMOLE_RUNTIME_ROLE:-}" != "agent" ]; then
  echo "gatemole-codex: GATEMOLE_RUNTIME_ROLE must be agent" >&2
  exit 64
fi
if [ "${GATEMOLE_TASK_PATH:-}" != "/gatemole/task.json" ] ||
  [ ! -r "$GATEMOLE_TASK_PATH" ]; then
  echo "gatemole-codex: admitted task envelope is unavailable" >&2
  exit 64
fi
if [ -z "${OPENAI_BASE_URL:-}" ] || [ -z "${OPENAI_API_KEY:-}" ]; then
  echo "gatemole-codex: task has no admitted model-broker authority" >&2
  exit 64
fi

model="gpt-5.6"
if [ "$#" -gt 0 ]; then
  if [ "$#" -ne 2 ] || [ "$1" != "--model" ] || [ -z "$2" ]; then
    echo "usage: gatemole-codex [--model MODEL]" >&2
    exit 64
  fi
  model="$2"
fi

intent="$(
  node -e '
    const fs = require("node:fs");
    const task = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
    if (typeof task.intent !== "string" || task.intent.trim() === "") {
      throw new Error("task intent is missing");
    }
    process.stdout.write(task.intent);
  ' "$GATEMOLE_TASK_PATH"
)"

# Codex supports a per-exec API key and an OpenAI base-URL override. The API
# key here is Gatemole's transaction token, not the upstream provider key.
export CODEX_API_KEY="$OPENAI_API_KEY"
export CODEX_HOME=/tmp/codex
mkdir -p "$CODEX_HOME"

# The inner sandbox is deliberately bypassed because this process is already
# inside Gatemole's read-only, capability-dropped OCI boundary. Its only
# writable mount is the transaction worktree and its only network peer is the
# transaction-specific model broker.
exec codex exec \
  --ephemeral \
  --dangerously-bypass-approvals-and-sandbox \
  --ignore-user-config \
  --ignore-rules \
  --skip-git-repo-check \
  --cd /workspace \
  --model "$model" \
  --config "openai_base_url=\"${OPENAI_BASE_URL}\"" \
  "$intent"
