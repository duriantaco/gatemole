#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf '%s\n' 'usage: scripts/vouchproductionfixture.sh [--tag IMAGE_TAG]'
}

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE_TAG="vouch-production-fixture:local"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)
      [[ $# -ge 2 ]] || { echo 'vouchproductionfixture: --tag requires a value' >&2; exit 2; }
      IMAGE_TAG="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "vouchproductionfixture: unknown argument $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

BUILD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/vouch-production-fixture.XXXXXX")"
cleanup() {
  rm -rf "$BUILD_DIR"
}
trap cleanup EXIT

cat >"$BUILD_DIR/main.go" <<'EOF'
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const expectedTaskIntent = "Change authentication under a production transaction boundary"

func main() {
	if os.Getuid() == 0 {
		fail(90, "fixture must run as a non-root user")
	}
	switch os.Getenv("VOUCH_RUNTIME_ROLE") {
	case "agent":
		runAgent()
	case "verifier":
		runVerifier()
	default:
		fail(89, "unexpected VOUCH_RUNTIME_ROLE")
	}
}

func runAgent() {
	taskPath := os.Getenv("VOUCH_TASK_PATH")
	if taskPath != "/vouch/task.json" {
		fail(86, "agent task path is not the fixed read-only mount")
	}
	taskData, err := os.ReadFile(taskPath)
	if err != nil {
		fail(86, fmt.Sprintf("read agent task envelope: %v", err))
	}
	var task struct {
		Intent string `json:"intent"`
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(taskData, &task); err != nil {
		fail(86, fmt.Sprintf("decode agent task envelope: %v", err))
	}
	if task.Intent != expectedTaskIntent {
		fail(86, "agent task envelope contains an unexpected intent")
	}
	taskDigest := os.Getenv("VOUCH_TASK_DIGEST")
	if !validSHA256Digest(taskDigest) || taskDigest != task.Digest {
		fail(86, "agent task digest is invalid or does not match the envelope")
	}
	if err := os.WriteFile(taskPath, []byte("forbidden"), 0o600); err == nil {
		fail(87, "agent task envelope is writable")
	}
	if err := os.WriteFile("/vouch-rootfs-probe", []byte("forbidden"), 0o600); err == nil {
		fail(91, "container root filesystem is writable")
	}
	for _, environment := range os.Environ() {
		if strings.Contains(environment, "provider-secret-must-never-reach-agent") {
			fail(99, "provider credential reached the agent environment")
		}
	}
	if connection, err := net.DialTimeout("tcp", "1.1.1.1:80", 250*time.Millisecond); err == nil {
		_ = connection.Close()
		fail(92, "agent has direct network egress")
	}

	baseURL := strings.TrimRight(os.Getenv("OPENAI_BASE_URL"), "/")
	token := os.Getenv("OPENAI_API_KEY")
	if baseURL == "" || token == "" {
		fail(93, "model broker environment is unavailable")
	}
	body := []byte(`{"model":"gpt-broker-bench","input":"sensitive benchmark prompt absent from receipts","max_output_tokens":10,"store":true}`)
	request, err := http.NewRequest(http.MethodPost, baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		fail(93, "cannot construct model broker request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		fail(94, "model broker request failed")
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		fail(94, fmt.Sprintf("unexpected model broker status %d", response.StatusCode))
	}

	mustWrite(
		"internal/auth/middleware.go",
		"package auth\n\nfunc Allowed() bool { return true }\n",
	)
	mustWrite(
		"internal/auth/middleware_test.go",
		"package auth\n\nfunc TestAllowed() { /* independently checked */ }\n",
	)
	mustWrite(
		"verifier-poison",
		"ignored live-worktree content must not enter the verifier snapshot\n",
	)
}

func validSHA256Digest(value string) bool {
	const prefix = "sha256:"
	if len(value) != len(prefix)+64 ||
		!strings.HasPrefix(value, prefix) ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil
}

func runVerifier() {
	if err := os.WriteFile(
		"internal/auth/verifier-write-probe.go",
		[]byte("forbidden"),
		0o600,
	); err == nil {
		fail(95, "verifier workspace is writable")
	}
	content, err := os.ReadFile("internal/auth/middleware.go")
	if err != nil || !bytes.Contains(content, []byte("return true")) {
		fail(96, "staged authentication change is unavailable")
	}
	if _, err := os.Stat("verifier-poison"); !os.IsNotExist(err) {
		fail(98, "ignored live-worktree content entered the verifier snapshot")
	}
}

func mustWrite(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		fail(97, fmt.Sprintf("write %s: %v", path, err))
	}
}

func fail(code int, message string) {
	_, _ = fmt.Fprintln(os.Stderr, message)
	os.Exit(code)
}
EOF

docker_arch="$(docker version --format '{{.Server.Arch}}')"
if [[ -z "$docker_arch" ]]; then
  echo 'vouchproductionfixture: Docker server architecture is unavailable' >&2
  exit 1
fi

GO111MODULE=off CGO_ENABLED=0 GOOS=linux GOARCH="$docker_arch" go build \
  -trimpath \
  -ldflags='-s -w -buildid=' \
  -o "$BUILD_DIR/vouch-production-fixture" \
  "$BUILD_DIR/main.go"

docker build \
  --network=none \
  --pull=false \
  --quiet \
  --tag "$IMAGE_TAG" \
  --file "$ROOT_DIR/build/production-fixture.Dockerfile" \
  "$BUILD_DIR" >/dev/null

docker image inspect --format '{{.Id}}' "$IMAGE_TAG"
