package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
)

func TestAgentTaskIntentSurvivesDurableTransactionReload(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kernel.db")
	kernelStore, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	task, err := model.NewAgentTask(
		"task:persisted", "tx:persisted", "engineering", "run:persisted",
		"Retain this exact human-owned task across daemon restarts.",
		model.AgentTaskProfileBinding{
			ID: "agent-profile:fixture", Digest: "sha256:" + strings.Repeat("a", 64),
			RuntimeClass: "oci", ImageDigest: "sha256:" + strings.Repeat("b", 64),
			CommandDigest: "sha256:" + strings.Repeat("c", 64),
		},
		time.Date(2026, 7, 25, 11, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	transaction := model.AgentTransaction{
		Version: model.AgentTransactionVersion, ID: task.TransactionID,
		Namespace: task.Namespace, IntentDigest: task.IntentDigest, Task: &task,
		Sponsor:     model.Principal{ID: "human:sponsor", Kind: model.PrincipalHuman},
		AgentRunIDs: []string{task.RunID}, StageBindings: []model.StageBinding{},
		State: model.TransactionCreated, EffectIDs: []string{},
		VerificationResultIDs: []string{}, OutstandingApprovalIDs: []string{},
		EventSequence: 1, CreatedAt: task.CreatedAt, UpdatedAt: task.CreatedAt,
	}
	event, err := transactionreducer.CreationEvent(
		transaction,
		model.Principal{ID: "operator:creator", Kind: model.PrincipalOperator},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kernelStore.CreateTransaction(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if err := kernelStore.Close(); err != nil {
		t.Fatal(err)
	}

	kernelStore, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	projection, err := kernelStore.GetTransaction(
		context.Background(), task.Namespace, task.TransactionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Transaction.Task == nil ||
		projection.Transaction.Task.Intent != task.Intent ||
		projection.Transaction.Task.Digest != task.Digest {
		t.Fatalf("durable task changed after reload: %#v", projection.Transaction.Task)
	}
}
