package model

import (
	"strings"
	"testing"
	"time"
)

func TestAgentTaskRetainsAndBindsExactIntentAndProfile(t *testing.T) {
	t.Parallel()
	task := validAgentTask(t)
	if task.Intent != "Fix authentication without changing public behavior.\nRun all tests." {
		t.Fatalf("intent was not retained exactly: %q", task.Intent)
	}
	if task.IntentDigest != ComputeAgentTaskIntentDigest(task.Intent) {
		t.Fatal("intent digest does not bind retained intent")
	}
	if err := task.Validate(); err != nil {
		t.Fatal(err)
	}

	mutated := task
	mutated.Intent += "\nPublish it."
	if err := mutated.Validate(); err == nil {
		t.Fatal("task accepted intent that did not match its persisted digest")
	}
	mutated = task
	mutated.AgentProfile.CommandDigest = "sha256:" + strings.Repeat("f", 64)
	if err := mutated.Validate(); err == nil {
		t.Fatal("task accepted a profile mutation that did not match its task digest")
	}
	mutated = task
	mutated.AgentProfile.Entrypoint = "/opt/different-agent"
	if err := mutated.Validate(); err == nil {
		t.Fatal("task accepted an entrypoint mutation that did not match its task digest")
	}
}

func TestAgentTransactionTaskCrossBindingIsOptionalButStrict(t *testing.T) {
	t.Parallel()
	task := validAgentTask(t)
	transaction := AgentTransaction{
		Version: AgentTransactionVersion, ID: task.TransactionID,
		Namespace: task.Namespace, IntentDigest: task.IntentDigest, Task: &task,
		Sponsor:     Principal{ID: "human:sponsor", Kind: PrincipalHuman},
		AgentRunIDs: []string{task.RunID}, StageBindings: []StageBinding{},
		State: TransactionCreated, EffectIDs: []string{},
		VerificationResultIDs: []string{}, OutstandingApprovalIDs: []string{},
		EventSequence: 1, CreatedAt: task.CreatedAt, UpdatedAt: task.CreatedAt,
	}
	if err := transaction.Validate(); err != nil {
		t.Fatal(err)
	}

	legacy := transaction
	legacy.Task = nil
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy v0 transaction without a task was broken: %v", err)
	}

	mismatched := transaction
	mismatched.IntentDigest = "sha256:" + strings.Repeat("0", 64)
	if err := mismatched.Validate(); err == nil {
		t.Fatal("transaction accepted a task bound to a different intent")
	}
}

func validAgentTask(t *testing.T) AgentTask {
	t.Helper()
	task, err := NewAgentTask(
		"task:auth-fix",
		"tx:auth-fix",
		"engineering",
		"run:auth-fix",
		"Fix authentication without changing public behavior.\nRun all tests.",
		AgentTaskProfileBinding{
			ID:            "agent-profile:secure-coder",
			Digest:        "sha256:" + strings.Repeat("a", 64),
			RuntimeClass:  "oci",
			ImageDigest:   "sha256:" + strings.Repeat("b", 64),
			Entrypoint:    "/opt/vouch-agent",
			CommandDigest: "sha256:" + strings.Repeat("c", 64),
		},
		time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	return task
}
