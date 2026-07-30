package verification

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/sandbox"
)

func TestRunnerReportsInterruptedContainerCleanupFailure(t *testing.T) {
	engine := filepath.Join(t.TempDir(), "fake-oci")
	if err := os.WriteFile(engine, []byte(`#!/bin/sh
if [ "$1" = "rm" ]; then
  echo "permission denied" >&2
  exit 1
fi
exec sleep 5
`), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	config := sandbox.OCIConfig{
		EnginePath:    engine,
		Image:         "agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Workspace:     workspace,
		TransactionID: "tx:cleanup",
		RunID:         "run:cleanup",
		Command:       []string{"agent"},
		UID:           1000,
		GID:           1000,
		MemoryBytes:   512 << 20,
		CPUMillis:     1000,
		PIDsLimit:     64,
		TmpfsBytes:    64 << 20,
		ContainerName: "gatemole-cleanup",
		Role:          "agent",
		WorkspaceMode: "transaction_rw",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	outcome, err := (Runner{EvidenceRoot: t.TempDir()}).Run(ctx, config, "execution:cleanup")
	if !IsCleanupError(err) {
		t.Fatalf("error=%v, want CleanupError", err)
	}
	if outcome.Receipt.Status != model.AgentExecutionInterrupted {
		t.Fatalf("status=%s, want interrupted", outcome.Receipt.Status)
	}
	if outcome.EvidenceDirectory == "" || len(outcome.Evidence) != 3 {
		t.Fatalf("interrupted evidence was not preserved: %#v", outcome)
	}
}

func TestRunnerTerminatesWorkloadAtOutputLimit(t *testing.T) {
	engine := filepath.Join(t.TempDir(), "fake-oci")
	if err := os.WriteFile(engine, []byte(`#!/bin/sh
if [ "$1" = "rm" ]; then
  exit 0
fi
while :; do
  printf 'unbounded verifier output\n'
done
`), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	config := sandbox.OCIConfig{
		EnginePath:    engine,
		Image:         "agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Workspace:     workspace,
		TransactionID: "tx:output-limit",
		RunID:         "run:output-limit",
		Command:       []string{"verify"},
		UID:           1000,
		GID:           1000,
		MemoryBytes:   512 << 20,
		CPUMillis:     1000,
		PIDsLimit:     64,
		TmpfsBytes:    64 << 20,
		ContainerName: "gatemole-output-limit",
		Role:          "verifier",
		WorkspaceMode: "staged_ro",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	outcome, err := (Runner{
		EvidenceRoot: t.TempDir(),
		CaptureLimit: 1024,
	}).Run(ctx, config, "verification:output-limit")
	if err != nil {
		t.Fatalf("output-limit cleanup failed: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("workload reached the test timeout instead of the output limit")
	}
	if outcome.Receipt.Status != model.AgentExecutionFailed ||
		!outcome.Receipt.StdoutTruncated {
		t.Fatalf("output limit was not enforced: %#v", outcome.Receipt)
	}
	data, err := os.ReadFile(
		filepath.Join(outcome.EvidenceDirectory, "stdout.log"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != 1024 {
		t.Fatalf("captured stdout=%d bytes, want 1024", len(data))
	}
}

func TestRunnerReconcilesNamedContainerAfterFailedWorkload(t *testing.T) {
	root := t.TempDir()
	cleanupMarker := filepath.Join(root, "cleanup")
	engine := filepath.Join(root, "fake-oci")
	if err := os.WriteFile(engine, []byte(`#!/bin/sh
if [ "$1" = "rm" ]; then
  : > "$GATEMOLE_CLEANUP_MARKER"
  exit 0
fi
exit 42
`), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GATEMOLE_CLEANUP_MARKER", cleanupMarker)
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	config := sandbox.OCIConfig{
		EnginePath:    engine,
		Image:         "agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Workspace:     workspace,
		TransactionID: "tx:failed-cleanup",
		RunID:         "run:failed-cleanup",
		Command:       []string{"agent"},
		UID:           1000,
		GID:           1000,
		MemoryBytes:   512 << 20,
		CPUMillis:     1000,
		PIDsLimit:     64,
		TmpfsBytes:    64 << 20,
		ContainerName: "gatemole-failed-cleanup",
		Role:          "agent",
		WorkspaceMode: "transaction_rw",
	}
	outcome, err := (Runner{EvidenceRoot: t.TempDir()}).Run(
		context.Background(),
		config,
		"execution:failed-cleanup",
	)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Receipt.Status != model.AgentExecutionFailed {
		t.Fatalf("status=%s, want failed", outcome.Receipt.Status)
	}
	if _, err := os.Stat(cleanupMarker); err != nil {
		t.Fatalf("named container was not reconciled: %v", err)
	}
}
