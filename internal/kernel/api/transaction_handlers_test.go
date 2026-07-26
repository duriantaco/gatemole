package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
	"github.com/duriantaco/vouch/internal/kernel/transaction/gitstage"
)

func TestFrozenStageFreshnessRequiresStateAndEffectDigests(t *testing.T) {
	t.Parallel()
	stateDigest := testDigest("1")
	effects := []model.Effect{{
		ID: "effect:one", TransactionID: "tx:one", Sequence: 1,
		System: "git", Operation: "modify",
	}}
	rawDigest, err := transactionreducer.ComputeEffectSetDigest(effects)
	if err != nil {
		t.Fatal(err)
	}
	boundEffects := append([]model.Effect(nil), effects...)
	boundEffects[0].RunID = "run:one"
	effectDigest, err := transactionreducer.ComputeEffectSetDigest(boundEffects)
	if err != nil {
		t.Fatal(err)
	}
	projection := transactionreducer.Projection{
		Transaction: model.AgentTransaction{
			StagedStateDigest: stateDigest,
			EffectSetDigest:   effectDigest,
			AgentRunIDs:       []string{"run:one"},
		},
	}
	if !matchesFrozenStage(gitstage.Snapshot{
		StagedStateDigest: stateDigest,
		EffectSetDigest:   rawDigest,
		Effects:           effects,
	}, projection) {
		t.Fatal("matching frozen stage was rejected")
	}
	if matchesFrozenStage(gitstage.Snapshot{
		StagedStateDigest: testDigest("3"),
		EffectSetDigest:   rawDigest,
		Effects:           effects,
	}, projection) {
		t.Fatal("staged-state mismatch was accepted")
	}
	if matchesFrozenStage(gitstage.Snapshot{
		StagedStateDigest: stateDigest,
		EffectSetDigest:   testDigest("4"),
		Effects:           effects,
	}, projection) {
		t.Fatal("effect-set mismatch was accepted")
	}
	mutatedEffects := append([]model.Effect(nil), effects...)
	mutatedEffects[0].Operation = "delete"
	mutatedRawDigest, err := transactionreducer.ComputeEffectSetDigest(
		mutatedEffects,
	)
	if err != nil {
		t.Fatal(err)
	}
	if matchesFrozenStage(gitstage.Snapshot{
		StagedStateDigest: stateDigest,
		EffectSetDigest:   mutatedRawDigest,
		Effects:           mutatedEffects,
	}, projection) {
		t.Fatal("normalized effect-set mutation was accepted")
	}
}

func TestPersistedTaskAuthorizesExactAgentProfileAndMaterializesReadOnly(t *testing.T) {
	t.Parallel()
	command := []string{"agent", "--task-file", "/vouch/task.json"}
	commandDigest, err := transactionreducer.ComputeCommandDigest(command)
	if err != nil {
		t.Fatal(err)
	}
	imageDigest := "sha256:" + strings.Repeat("b", 64)
	task, err := model.NewAgentTask(
		"task:api", "tx:api", "engineering", "run:api",
		"Implement the requested API without publishing unrelated changes.",
		model.AgentTaskProfileBinding{
			ID: "agent-profile:api", Digest: testDigest("a"),
			RuntimeClass: "oci", ImageDigest: imageDigest,
			CommandDigest: commandDigest,
		},
		time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := taskAuthorizesExecution(
		&task, task.RunID, "oci", imageDigest, commandDigest,
	); err != nil {
		t.Fatal(err)
	}
	if err := taskAuthorizesDaemonExecution(
		&task, task.RunID, "oci", imageDigest, commandDigest,
	); err != nil {
		t.Fatal(err)
	}
	err = taskAuthorizesDaemonExecution(
		nil, task.RunID, "oci", imageDigest, commandDigest,
	)
	kernelErr, ok := err.(*model.KernelError)
	if !ok || kernelErr.Code != model.ErrorCapabilityDenied {
		t.Fatalf("taskless daemon execution was not denied: %v", err)
	}
	if err := taskAuthorizesExecution(
		&task, task.RunID, "oci", imageDigest, testDigest("f"),
	); err == nil {
		t.Fatal("different agent command was authorized by the persisted task")
	}

	directory, cleanup, err := materializeAgentTask(t.TempDir(), task)
	if err != nil {
		t.Fatal(err)
	}
	taskPath := filepath.Join(directory, "task.json")
	data, err := os.ReadFile(taskPath)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := model.DecodeStrict[model.AgentTask](data)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Intent != task.Intent || persisted.Digest != task.Digest {
		t.Fatalf("materialized task changed: %#v", persisted)
	}
	info, err := os.Stat(taskPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("task envelope is writable: %o", info.Mode().Perm())
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("task materialization was retained after cleanup: %v", err)
	}

	encoded, err := json.Marshal(task)
	if err != nil || len(encoded) == 0 {
		t.Fatalf("task does not encode as a resource: %v", err)
	}
}
