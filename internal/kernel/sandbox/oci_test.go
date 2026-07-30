package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
)

const pinnedImage = "registry.example.invalid/vouch/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestOCIInvocationEnforcesIsolationAndLimits(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	config := validOCIConfig(workspace)
	invocation, err := config.Invocation()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(invocation.Arguments, "\n")
	for _, required := range []string{
		"--pull=never",
		"--network=none",
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges:true",
		"--pids-limit=256",
		"--memory=4294967296",
		"--cpus=2.000",
		"--user=1000:1000",
		"--workdir=/workspace",
		"--env=HOME=/tmp",
		"--env=VOUCH_RUNTIME_ROLE=agent",
		"--tmpfs=/tmp:rw,nosuid,nodev",
		"--mount=type=bind,src=" + workspace + ",dst=/workspace",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("invocation missing %q:\n%s", required, joined)
		}
	}
	if strings.Contains(joined, "--privileged") ||
		strings.Contains(joined, "/var/run/docker.sock") {
		t.Fatalf("unsafe OCI argument present:\n%s", joined)
	}
	imageIndex := stringIndex(invocation.Arguments, pinnedImage)
	if imageIndex < 0 {
		t.Fatalf("pinned image missing: %#v", invocation.Arguments)
	}
	if !reflect.DeepEqual(
		invocation.Arguments[imageIndex+1:],
		[]string{"agent", "--task", "upgrade"},
	) {
		t.Fatalf("agent argv changed: %#v", invocation.Arguments[imageIndex+1:])
	}
}

func TestOCIInvocationOverridesImageEntrypointForBoundProfile(t *testing.T) {
	config := validOCIConfig(filepath.Join(t.TempDir(), "workspace"))
	config.Entrypoint = "/opt/vouch-agent"
	config.Command = []string{"serve", "--task", "upgrade"}
	invocation, err := config.Invocation()
	if err != nil {
		t.Fatal(err)
	}
	entrypointIndex := stringIndex(invocation.Arguments, "--entrypoint=/opt/vouch-agent")
	imageIndex := stringIndex(invocation.Arguments, pinnedImage)
	if entrypointIndex < 0 || entrypointIndex >= imageIndex {
		t.Fatalf("profile entrypoint is not enforced before the image: %#v", invocation.Arguments)
	}
	if !reflect.DeepEqual(
		invocation.Arguments[imageIndex+1:],
		[]string{"serve", "--task", "upgrade"},
	) {
		t.Fatalf("profile arguments changed: %#v", invocation.Arguments[imageIndex+1:])
	}

	boundDigest, err := config.RuntimeConfigDigest()
	if err != nil {
		t.Fatal(err)
	}
	config.Entrypoint = "/opt/different-agent"
	changedDigest, err := config.RuntimeConfigDigest()
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == boundDigest {
		t.Fatal("entrypoint override is not bound by the runtime configuration digest")
	}
}

func TestOCIRejectsMutableImagesRootAndWeakLimits(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*OCIConfig)
	}{
		{"tagged image", func(config *OCIConfig) { config.Image = "agent:latest" }},
		{"root user", func(config *OCIConfig) { config.UID = 0 }},
		{"weak memory", func(config *OCIConfig) { config.MemoryBytes = MinimumMemoryBytes - 1 }},
		{"weak pid limit", func(config *OCIConfig) { config.PIDsLimit = 15 }},
		{"oversized tmpfs", func(config *OCIConfig) { config.TmpfsBytes = config.MemoryBytes + 1 }},
		{"relative engine", func(config *OCIConfig) { config.EnginePath = "docker" }},
		{"mount injection", func(config *OCIConfig) { config.Workspace = "/tmp/work,readonly" }},
		{"argument NUL", func(config *OCIConfig) { config.Command = []string{"agent", "bad\x00arg"} }},
		{"entrypoint NUL", func(config *OCIConfig) { config.Entrypoint = "agent\x00bad" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validOCIConfig(filepath.Join(t.TempDir(), "workspace"))
			test.mutate(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("unsafe OCI configuration was accepted")
			}
		})
	}
}

func TestOCIRuntimeDigestBindsSecurityConfiguration(t *testing.T) {
	config := validOCIConfig(filepath.Join(t.TempDir(), "workspace"))
	first, err := config.RuntimeConfigDigest()
	if err != nil {
		t.Fatal(err)
	}
	second, err := config.RuntimeConfigDigest()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("runtime digest is not deterministic: %s != %s", first, second)
	}
	config.PIDsLimit++
	changed, err := config.RuntimeConfigDigest()
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("runtime limit change did not change the runtime digest")
	}
}

func TestOCIInvocationMountsExactTaskEnvelopeReadOnly(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	command := []string{"agent", "--task", "upgrade"}
	commandData, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	task, err := model.NewAgentTask(
		"task:runtime", "tx:runtime", "engineering", "run:runtime",
		"Upgrade the dependency and preserve API compatibility.",
		model.AgentTaskProfileBinding{
			ID: "agent-profile:runtime", Digest: digestValue([]byte("profile")),
			RuntimeClass:  "oci",
			ImageDigest:   "sha256:" + strings.Repeat("a", 64),
			CommandDigest: digestValue(commandData),
		},
		time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	taskDirectory := filepath.Join(root, "task")
	if err := os.Mkdir(taskDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(taskDirectory, 0o700) })
	taskData, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(taskDirectory, "task.json")
	if err := os.WriteFile(taskPath, taskData, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(taskPath, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(taskDirectory, 0o555); err != nil {
		t.Fatal(err)
	}

	config := validOCIConfig(workspace)
	config.TaskDirectory = taskDirectory
	config.TaskDigest = task.Digest
	invocation, err := config.Invocation()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(invocation.Arguments, "\n")
	for _, required := range []string{
		"--env=VOUCH_TASK_PATH=/vouch/task.json",
		"--env=VOUCH_TASK_DIGEST=" + task.Digest,
		"--mount=type=bind,src=" + taskDirectory + ",dst=/vouch,readonly",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("task-aware invocation missing %q:\n%s", required, joined)
		}
	}
	runtimeDigest, err := config.RuntimeConfigDigest()
	if err != nil {
		t.Fatal(err)
	}
	config.TaskDigest = "sha256:" + strings.Repeat("d", 64)
	if _, err := config.RuntimeConfigDigest(); err == nil {
		t.Fatal("runtime accepted a task digest that did not match the mounted envelope")
	}
	config.TaskDigest = task.Digest
	task.AgentProfile.CommandDigest = "sha256:" + strings.Repeat("e", 64)
	taskData, err = json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(taskPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taskPath, taskData, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(taskPath, 0o444); err != nil {
		t.Fatal(err)
	}
	if changed, err := config.RuntimeConfigDigest(); err == nil || changed == runtimeDigest {
		t.Fatal("runtime accepted a tampered task envelope")
	}
}

func TestOCIInvocationUsesOnlyInternalBrokerNetwork(t *testing.T) {
	config := validOCIConfig(filepath.Join(t.TempDir(), "workspace"))
	token := "transaction-scoped-token-000000000000000000000000"
	config.NetworkName = "vouch-net-0123456789abcdef"
	config.ModelBroker = &ModelBrokerBinding{
		URL: "http://vouch-model-broker:8080/v1", Token: token,
		ImageDigest:  "sha256:" + strings.Repeat("b", 64),
		PolicyDigest: "sha256:" + strings.Repeat("c", 64),
		TokenDigest:  digestValue([]byte(token)),
	}
	invocation, err := config.Invocation()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(invocation.Arguments, "\n")
	for _, required := range []string{
		"--network=" + config.NetworkName,
		"--env=OPENAI_BASE_URL=http://vouch-model-broker:8080/v1",
		"--env=OPENAI_API_KEY=" + token,
		"--env=VOUCH_MODEL_BROKER_URL=http://vouch-model-broker:8080/v1",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("brokered invocation missing %q:\n%s", required, joined)
		}
	}
	if strings.Contains(joined, "--network=none") ||
		strings.Contains(joined, "--network=bridge") {
		t.Fatalf("agent received a bypassable network:\n%s", joined)
	}
	digest, err := config.RuntimeConfigDigest()
	if err != nil {
		t.Fatal(err)
	}
	config.ModelBroker.PolicyDigest = "sha256:" + strings.Repeat("d", 64)
	changed, err := config.RuntimeConfigDigest()
	if err != nil {
		t.Fatal(err)
	}
	if changed == digest {
		t.Fatal("broker policy change did not alter runtime digest")
	}
}

func TestModelBrokerSidecarUsesTwoNetworkBoundaryAndSecretEnvFile(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "engine.log")
	engine := filepath.Join(root, "fake-engine")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$VOUCH_FAKE_ENGINE_LOG"
if [ "$1" = "rm" ]; then
  echo "No such container" >&2
  exit 1
fi
if [ "$1" = "network" ] && [ "$2" = "rm" ]; then
  echo "network not found" >&2
  exit 1
fi
if [ "$1" = "run" ]; then
  for argument in "$@"; do
    case "$argument" in
      --mount=type=bind,src=*,dst=/var/lib/vouch)
        directory=${argument#--mount=type=bind,src=}
        directory=${directory%,dst=/var/lib/vouch}
        printf 'ready\n' > "$directory/ready"
        ;;
    esac
  done
  echo broker-container-id
fi
exit 0
`
	if err := os.WriteFile(engine, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOUCH_FAKE_ENGINE_LOG", logPath)
	policyPath := filepath.Join(root, "policy.json")
	policyData := []byte(`{"version":"test"}`)
	if err := os.WriteFile(policyPath, policyData, 0o600); err != nil {
		t.Fatal(err)
	}
	policySnapshot, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	config := ModelBrokerConfig{
		EnginePath: engine,
		Image:      "broker@sha256:" + strings.Repeat("b", 64),
		PolicyData: policySnapshot, PolicyDigest: digestValue(policySnapshot),
		ReceiptDirectory: filepath.Join(root, "receipts"),
		TransactionID:    "tx:broker-sidecar", RunID: "run:broker-sidecar",
		AgentToken:          "agent-token-000000000000000000000000000000",
		ProviderBearerToken: "provider-secret-never-in-engine-arguments",
		UID:                 1000, GID: 1000, MemoryBytes: 512 << 20,
		CPUMillis: 1000, PIDsLimit: 64, TmpfsBytes: 64 << 20,
	}
	if err := os.WriteFile(
		policyPath,
		[]byte(`{"version":"replaced-after-snapshot"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	session, err := StartModelBroker(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if session.NetworkName != ModelBrokerNetworkName(config.TransactionID, config.RunID) ||
		session.ContainerName != ModelBrokerContainerName(config.TransactionID, config.RunID) {
		t.Fatalf("unexpected broker session: %#v", session)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	logged := string(logData)
	for _, required := range []string{
		"network create --internal",
		"run --detach --rm",
		"--network=bridge",
		"network connect --alias vouch-model-broker",
	} {
		if !strings.Contains(logged, required) {
			t.Errorf("engine log missing %q:\n%s", required, logged)
		}
	}
	if strings.Contains(logged, config.ProviderBearerToken) {
		t.Fatal("provider credential leaked into engine process arguments")
	}
	frozenPolicy, err := os.ReadFile(filepath.Join(
		config.ReceiptDirectory,
		"model-policy.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frozenPolicy, policySnapshot) {
		t.Fatalf(
			"frozen policy=%s, want original snapshot=%s",
			frozenPolicy,
			policySnapshot,
		)
	}
	if _, err := os.Stat(filepath.Join(config.ReceiptDirectory, "broker.env")); !os.IsNotExist(err) {
		t.Fatalf("broker secret environment file was retained: %v", err)
	}
	if err := RemoveModelBroker(context.Background(), config.EnginePath, session); err != nil {
		t.Fatal(err)
	}
}

func TestImageDigestAcceptsOnlyPinnedReferences(t *testing.T) {
	want := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, reference := range []string{want, pinnedImage} {
		got, err := ImageDigest(reference)
		if err != nil {
			t.Fatalf("%s: %v", reference, err)
		}
		if got != want {
			t.Fatalf("digest=%q, want %q", got, want)
		}
	}
	for _, reference := range []string{
		"", "agent", "agent:latest", "-bad@" + want, " agent@" + want,
		"registry.example.invalid/agent@junk@" + want,
	} {
		if _, err := ImageDigest(reference); err == nil {
			t.Fatalf("mutable or unsafe image %q was accepted", reference)
		}
	}
}

func TestPrepareWorkspaceOwnershipRejectsUnsafeInputs(t *testing.T) {
	t.Parallel()
	if err := PrepareWorkspaceOwnership("relative", 1000, 1000); err == nil {
		t.Fatal("relative workspace ownership path was accepted")
	}
	if err := PrepareWorkspaceOwnership(t.TempDir(), 0, 1000); err == nil {
		t.Fatal("root workload ownership was accepted")
	}
}

func TestContainerDoesNotExistDetection(t *testing.T) {
	for _, output := range []string{
		"Error response from daemon: No such container: vouch-demo",
		"no container with name or ID vouch-demo found",
		"container not found",
	} {
		if !containerDoesNotExist([]byte(output)) {
			t.Fatalf("not-found output was not recognized: %q", output)
		}
	}
	if containerDoesNotExist([]byte("permission denied")) {
		t.Fatal("permission failure was treated as an absent container")
	}
}

func validOCIConfig(workspace string) OCIConfig {
	return OCIConfig{
		EnginePath:    "/usr/bin/docker",
		Image:         pinnedImage,
		Workspace:     workspace,
		TransactionID: "tx:runtime",
		RunID:         "run:runtime",
		Command:       []string{"agent", "--task", "upgrade"},
		UID:           1000,
		GID:           1000,
		MemoryBytes:   4 << 30,
		CPUMillis:     2000,
		PIDsLimit:     256,
		TmpfsBytes:    1 << 30,
		ContainerName: "vouch-runtime",
		Role:          "agent",
		WorkspaceMode: "transaction_rw",
	}
}

func stringIndex(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}
	return -1
}
