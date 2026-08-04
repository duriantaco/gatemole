package transaction

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
)

func TestTransactionReplayReachesVerifiedCommitDeterministically(t *testing.T) {
	h := newHarness(t)
	h.state(model.TransactionRunning, "", nil)
	h.addEffect(effectFixture(h.now, 1, nil))
	h.stage()
	h.state(model.TransactionValidating, "", nil)
	h.effectState("effect:tx-demo:1", model.EffectValidated, nil, nil)
	h.verify(model.VerificationPassed)
	h.freezePlan()
	h.freezeApproval(nil)
	h.state(model.TransactionReadyToCommit, "", nil)
	h.effectState("effect:tx-demo:1", model.EffectReleaseReady, nil, nil)
	h.state(model.TransactionCommitting, "", nil)
	h.effectState("effect:tx-demo:1", model.EffectCommitting, nil, nil)
	h.effectState("effect:tx-demo:1", model.EffectCommitted, receipt(h.now), nil)
	h.state(model.TransactionCommitted, "", nil)

	if h.projection.Transaction.State != model.TransactionCommitted {
		t.Fatalf("state = %q, want committed", h.projection.Transaction.State)
	}
	if h.projection.Transaction.CompletedAt == nil {
		t.Fatal("committed transaction requires completed_at")
	}
	replayedA, err := Replay(h.events)
	if err != nil {
		t.Fatalf("replay A: %v", err)
	}
	replayedB, err := Replay(h.events)
	if err != nil {
		t.Fatalf("replay B: %v", err)
	}
	a, err := json.Marshal(replayedA)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(replayedB)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("transaction replay is not byte-equivalent:\n%s\n%s", a, b)
	}
}

func TestRequiredApprovalCannotBeBypassedAndIsSingleUse(t *testing.T) {
	h := validatedHarness(t, 1)
	h.freezeApproval([]string{"security-reviewer"})

	err := h.stateError(model.TransactionReadyToCommit, "", nil)
	assertCode(t, err, model.ErrorApprovalRequired)

	h.state(model.TransactionPendingApproval, "", []string{"approval:security:1"})
	wrong := ApprovalResolvedPayload{Decision: h.approvalDecision(
		"approval:security:1", digest("c"), model.ApprovalApprove, "",
	)}
	assertCode(t, h.emitError(EventApprovalResolved, wrong), model.ErrorApprovalInvalid)

	h.emit(EventApprovalResolved, ApprovalResolvedPayload{Decision: h.approvalDecision(
		"approval:security:1", h.projection.Transaction.ApprovalPackageDigest,
		model.ApprovalApprove, "",
	)})
	if h.projection.Transaction.State != model.TransactionReadyToCommit {
		t.Fatalf("approval left transaction in %q", h.projection.Transaction.State)
	}
	assertCode(t, h.emitError(EventApprovalResolved, ApprovalResolvedPayload{Decision: h.approvalDecision(
		"approval:security:1", h.projection.Transaction.ApprovalPackageDigest,
		model.ApprovalApprove, "",
	)}), model.ErrorTransitionInvalid)
}

func TestPrepareAuthorityFreezesAutomaticCommitBoundary(t *testing.T) {
	h := authorityInputHarness(t, 1)
	policy := BaselinePolicy{}
	policyDigest, err := PolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareAuthority(
		h.projection,
		policy.Evaluate(h.projection.Effects),
		policyDigest,
		releaseBindingFixture(),
		h.now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.TargetState != model.TransactionReadyToCommit ||
		len(prepared.OutstandingApprovalIDs) != 0 ||
		len(prepared.Approval.RequiredApprovalClasses) != 0 {
		t.Fatalf("unexpected automatic authority: %#v", prepared)
	}
	if prepared.Plan.Digest == "" ||
		prepared.Approval.CommitPlanDigest != prepared.Plan.Digest {
		t.Fatal("approval package does not bind the prepared commit plan")
	}
}

func TestPrepareAuthorityRequiresApprovalForControlAndEvidenceCoupling(t *testing.T) {
	h := authorityInputHarness(t, 2)
	h.projection.Effects[0].Resource.Pattern = "internal/auth/middleware.go"
	h.projection.Effects[1].Resource.Pattern = "internal/auth/middleware_test.go"
	effectSetDigest, err := ComputeEffectSetDigest(h.projection.Effects)
	if err != nil {
		t.Fatal(err)
	}
	h.projection.Transaction.EffectSetDigest = effectSetDigest
	for i := range h.projection.Verifications {
		h.projection.Verifications[i].EffectSetDigest = effectSetDigest
	}
	policy := BaselinePolicy{}
	policyDigest, err := PolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareAuthority(
		h.projection,
		policy.Evaluate(h.projection.Effects),
		policyDigest,
		releaseBindingFixture(),
		h.now.Add(time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.TargetState != model.TransactionPendingApproval ||
		len(prepared.OutstandingApprovalIDs) != 1 ||
		len(prepared.Approval.RequiredApprovalClasses) != 1 {
		t.Fatalf("approval requirement was not materialized: %#v", prepared)
	}
}

func TestPrepareAuthorityRejectsExpiredEvidence(t *testing.T) {
	h := authorityInputHarness(t, 1)
	expired := h.now.Add(-time.Second)
	h.projection.Verifications[0].ExpiresAt = &expired
	policy := BaselinePolicy{}
	policyDigest, err := PolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	_, err = PrepareAuthority(
		h.projection,
		policy.Evaluate(h.projection.Effects),
		policyDigest,
		releaseBindingFixture(),
		h.now,
	)
	assertCode(t, err, model.ErrorVerificationFailed)
}

func TestMutationPathInvalidatesFrozenVerificationAndApproval(t *testing.T) {
	h := newHarness(t)
	h.state(model.TransactionRunning, "", nil)
	h.addEffect(effectFixture(h.now, 1, nil))
	h.stage()
	h.state(model.TransactionValidating, "", nil)
	h.verify(model.VerificationPassed)
	h.freezePlan()
	h.freezeApproval(nil)
	h.state(model.TransactionReviseRequired, "verification scope must change", nil)
	h.state(model.TransactionRunning, "", nil)

	tx := h.projection.Transaction
	if tx.StagedStateDigest != "" || tx.EffectSetDigest != "" ||
		tx.CommitPlanDigest != "" || tx.ApprovalPackageDigest != "" ||
		len(tx.VerificationResultIDs) != 0 || h.projection.CommitPlan != nil ||
		h.projection.ApprovalPackage != nil || len(h.projection.Effects) != 0 {
		t.Fatalf("returning to running did not invalidate frozen authority: %#v", h.projection)
	}
	if tx.Attempt != 2 || len(h.projection.SupersededAttempts) != 1 ||
		len(h.projection.SupersededAttempts[0].Effects) != 1 ||
		h.projection.SupersededAttempts[0].Attempt != 1 {
		t.Fatalf("revision did not preserve the superseded attempt: %#v", h.projection)
	}
	h.addEffect(effectFixture(h.now, 1, nil))
	h.stage()
	if h.projection.Effects[0].Attempt != 2 ||
		h.projection.Transaction.State != model.TransactionStaged {
		t.Fatalf("new attempt could not stage an independent effect set: %#v", h.projection)
	}
	if err := h.projection.Validate(); err != nil {
		t.Fatalf("validate revised projection: %v", err)
	}
}

func TestExecutionRetryRequiresLatestSuccessBeforeNoEffectCompletion(t *testing.T) {
	h := newHarness(t)
	h.state(model.TransactionRunning, "", nil)
	h.emit(EventStageBindingCreated, StageBindingCreatedPayload{Binding: model.StageBinding{
		ID: "stage:tx-demo", Kind: "git_worktree",
		Resource: model.ResourceSelector{Kind: "git_repository", Pattern: "repository"},
		Location: "/tmp/gatemole-tx-demo", BaseRevision: strings.Repeat("a", 40),
		CreatedAt: h.now.Add(time.Second),
	}})
	first := h.startExecution("execution:tx-demo:1")
	nonzero := 7
	h.emit(EventAgentExecutionFinished, AgentExecutionFinishedPayload{
		ExecutionID: first.ID, Status: model.AgentExecutionFailed, ExitCode: &nonzero,
		StdoutDigest: digest("a"), StderrDigest: digest("b"),
	})
	assertCode(
		t,
		h.stateError(model.TransactionCompletedNoEffect, "", nil),
		model.ErrorTransitionInvalid,
	)
	second := h.startExecution("execution:tx-demo:2")
	zero := 0
	h.emit(EventAgentExecutionFinished, AgentExecutionFinishedPayload{
		ExecutionID: second.ID, Status: model.AgentExecutionSucceeded, ExitCode: &zero,
		StdoutDigest: digest("c"), StderrDigest: digest("d"),
	})
	h.state(model.TransactionCompletedNoEffect, "", nil)
	if !h.projection.Transaction.State.Terminal() ||
		h.projection.Transaction.CompletedAt == nil || len(h.projection.Executions) != 2 {
		t.Fatalf("no-effect completion did not preserve retry receipts: %#v", h.projection)
	}
}

func TestFailedVerificationCanBeSupersededWithoutChangingFrozenEffects(t *testing.T) {
	h := newHarness(t)
	h.state(model.TransactionRunning, "", nil)
	h.addEffect(effectFixture(h.now, 1, nil))
	h.stage()
	h.state(model.TransactionValidating, "", nil)
	h.verify(model.VerificationFailed)
	failedID := h.projection.Verifications[0].ID
	h.state(model.TransactionValidationFailed, "tests failed", nil)
	h.state(model.TransactionValidating, "", nil)
	h.emit(EventVerificationSuperseded, VerificationSupersededPayload{
		VerificationID: failedID,
		Reason:         "rerun failed verifier",
	})
	result := h.verificationFixture(
		"verification:tx-demo:tests-retry",
		model.VerificationPassed,
	)
	h.emit(EventVerificationRecorded, VerificationRecordedPayload{Result: result})
	if len(h.projection.Verifications) != 1 ||
		h.projection.Verifications[0].ID != result.ID ||
		len(h.projection.SupersededVerifications) != 1 ||
		h.projection.SupersededVerifications[0].ID != failedID ||
		h.projection.Transaction.EffectSetDigest == "" {
		t.Fatalf("verification retry did not preserve immutable history: %#v", h.projection)
	}
}

func TestAuthorityRenewalRevokesPackageAndExpiresEvidence(t *testing.T) {
	h := validatedHarness(t, 1)
	h.freezeApproval([]string{"security-reviewer"})
	h.state(model.TransactionPendingApproval, "", []string{"approval:security:1"})
	h.now = h.now.Add(2 * time.Hour)
	h.emit(EventAuthorityRenewed, AuthorityRenewedPayload{
		Reason: "authority expired before approval",
	})
	if h.projection.Transaction.State != model.TransactionValidating ||
		h.projection.CommitPlan != nil || h.projection.ApprovalPackage != nil ||
		len(h.projection.SupersededAuthorities) != 1 ||
		len(h.projection.Verifications) != 0 ||
		len(h.projection.SupersededVerifications) != 1 {
		t.Fatalf("authority renewal did not revoke stale authority: %#v", h.projection)
	}
	if err := h.projection.Validate(); err != nil {
		t.Fatalf("validate renewed projection: %v", err)
	}
}

func TestPartialCommitCannotMasqueradeAsRollback(t *testing.T) {
	h := validatedHarness(t, 2)
	h.freezeApproval(nil)
	h.state(model.TransactionReadyToCommit, "", nil)
	for _, effect := range h.projection.Effects {
		h.effectState(effect.ID, model.EffectReleaseReady, nil, nil)
	}
	h.state(model.TransactionCommitting, "", nil)
	for _, effect := range h.projection.Effects {
		h.effectState(effect.ID, model.EffectCommitting, nil, nil)
	}
	h.effectState("effect:tx-demo:1", model.EffectCommitted, receipt(h.now), nil)
	h.effectState("effect:tx-demo:2", model.EffectFailed, nil, nil)

	assertCode(t, h.stateError(model.TransactionCommitted, "", nil), model.ErrorTransitionInvalid)
	h.state(model.TransactionCompensating, "", nil)
	assertCode(t, h.stateError(model.TransactionRolledBack, "", nil), model.ErrorPartialCommit)

	h.effectState("effect:tx-demo:1", model.EffectCompensating, nil, nil)
	h.effectState("effect:tx-demo:1", model.EffectCompensated, nil, receipt(h.now))
	h.state(model.TransactionRolledBack, "", nil)
	if h.projection.Transaction.State != model.TransactionRolledBack {
		t.Fatalf("state = %q, want rolled_back", h.projection.Transaction.State)
	}
}

func TestPartialCommitIsTerminalAndRequiresReason(t *testing.T) {
	h := validatedHarness(t, 2)
	h.freezeApproval(nil)
	h.state(model.TransactionReadyToCommit, "", nil)
	for _, effect := range h.projection.Effects {
		h.effectState(effect.ID, model.EffectReleaseReady, nil, nil)
	}
	h.state(model.TransactionCommitting, "", nil)
	for _, effect := range h.projection.Effects {
		h.effectState(effect.ID, model.EffectCommitting, nil, nil)
	}
	h.effectState("effect:tx-demo:1", model.EffectCommitted, receipt(h.now), nil)
	h.effectState("effect:tx-demo:2", model.EffectUnknown, nil, nil)
	assertCode(t, h.stateError(model.TransactionPartiallyCommitted, "", nil), model.ErrorTransitionInvalid)
	h.state(model.TransactionPartiallyCommitted, "second effect outcome is unknown", nil)
	if !h.projection.Transaction.State.Terminal() {
		t.Fatal("partially committed state must be terminal for ordinary transitions")
	}
	assertCode(t, h.stateError(model.TransactionRunning, "", nil), model.ErrorTransitionInvalid)
}

func TestEventContentTamperingBreaksReplay(t *testing.T) {
	h := newHarness(t)
	h.state(model.TransactionRunning, "", nil)
	tampered := append([]model.TransactionEvent(nil), h.events...)
	tampered[1].Payload = json.RawMessage(`{"from":"created","to":"aborted","reason":"tampered"}`)
	_, err := Replay(tampered)
	assertCode(t, err, model.ErrorEventChain)
}

func TestEffectSetDigestExcludesLifecycleReceipts(t *testing.T) {
	effect := effectFixture(time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC), 1, nil)
	before, err := ComputeEffectSetDigest([]model.Effect{effect})
	if err != nil {
		t.Fatal(err)
	}
	effect.Status = model.EffectCommitted
	effect.Receipt = receipt(effect.UpdatedAt)
	effect.UpdatedAt = effect.UpdatedAt.Add(time.Hour)
	after, err := ComputeEffectSetDigest([]model.Effect{effect})
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("lifecycle mutation changed immutable effect digest: %s != %s", before, after)
	}
}

func TestAgentExecutionReceiptReplaysAndMustFinishBeforeStage(t *testing.T) {
	h := newHarness(t)
	h.state(model.TransactionRunning, "", nil)
	bindingTime := h.now.Add(time.Second)
	h.emit(EventStageBindingCreated, StageBindingCreatedPayload{Binding: model.StageBinding{
		ID:           "stage:git-demo",
		Kind:         "git.worktree",
		Resource:     model.ResourceSelector{Kind: "git.repository", Pattern: "repo:demo"},
		Location:     "/tmp/gatemole-demo",
		BaseRevision: strings.Repeat("a", 40),
		CreatedAt:    bindingTime,
	}})
	startedAt := h.now.Add(time.Second)
	execution := model.AgentExecution{
		Version:             model.AgentExecutionVersion,
		ID:                  "execution:demo",
		TransactionID:       h.projection.Transaction.ID,
		Attempt:             h.projection.Transaction.Attempt,
		RunID:               "run:demo",
		StageBindingID:      "stage:git-demo",
		Program:             "agent",
		CommandDigest:       digest("9"),
		RuntimeClass:        "host",
		RuntimeConfigDigest: digest("8"),
		Status:              model.AgentExecutionRunning,
		StartedAt:           startedAt,
	}
	h.emit(EventAgentExecutionStarted, AgentExecutionStartedPayload{Execution: execution})
	h.addEffect(effectFixture(h.now, 1, nil))
	effectSetDigest, err := ComputeEffectSetDigest(h.projection.Effects)
	if err != nil {
		t.Fatal(err)
	}
	assertCode(t, h.emitError(EventTransactionStaged, TransactionStagedPayload{
		StagedStateDigest: digest("5"),
		EffectSetDigest:   effectSetDigest,
	}), model.ErrorTransitionInvalid)

	exitCode := 0
	h.emit(EventAgentExecutionFinished, AgentExecutionFinishedPayload{
		ExecutionID:  execution.ID,
		Status:       model.AgentExecutionSucceeded,
		ExitCode:     &exitCode,
		StdoutDigest: digest("a"),
		StderrDigest: digest("b"),
	})
	h.stage()

	if len(h.projection.Executions) != 1 ||
		h.projection.Executions[0].Status != model.AgentExecutionSucceeded ||
		h.projection.Executions[0].CompletedAt == nil {
		t.Fatalf("unexpected execution projection: %#v", h.projection.Executions)
	}
	replayed, err := Replay(h.events)
	if err != nil {
		t.Fatalf("replay execution receipt: %v", err)
	}
	if len(replayed.Executions) != 1 ||
		replayed.Executions[0].StdoutDigest != digest("a") {
		t.Fatalf("execution receipt did not replay: %#v", replayed.Executions)
	}
}

type harness struct {
	t          *testing.T
	actor      model.Principal
	now        time.Time
	projection Projection
	events     []model.TransactionEvent
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	actor := model.Principal{ID: "human:alice", Kind: model.PrincipalHuman}
	tx := model.AgentTransaction{
		Version:                model.AgentTransactionVersion,
		ID:                     "tx:demo",
		Namespace:              "team-payments",
		Attempt:                1,
		IntentDigest:           digest("1"),
		Sponsor:                model.Principal{ID: "human:alice", Kind: model.PrincipalHuman},
		AgentRunIDs:            []string{"run:demo"},
		StageBindings:          []model.StageBinding{},
		State:                  model.TransactionCreated,
		EffectIDs:              []string{},
		VerificationResultIDs:  []string{},
		OutstandingApprovalIDs: []string{},
		EventSequence:          1,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	event, err := CreationEvent(tx, actor)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := Apply(nil, event)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, actor: actor, now: now, projection: projection, events: []model.TransactionEvent{event}}
}

func validatedHarness(t *testing.T, effectCount int) *harness {
	t.Helper()
	h := newHarness(t)
	h.state(model.TransactionRunning, "", nil)
	for i := 1; i <= effectCount; i++ {
		dependencies := []string(nil)
		if i > 1 {
			dependencies = []string{"effect:tx-demo:" + string(rune('0'+i-1))}
		}
		h.addEffect(effectFixture(h.now, i, dependencies))
	}
	h.stage()
	h.state(model.TransactionValidating, "", nil)
	for _, effect := range h.projection.Effects {
		h.effectState(effect.ID, model.EffectValidated, nil, nil)
	}
	h.verify(model.VerificationPassed)
	h.freezePlan()
	return h
}

func authorityInputHarness(t *testing.T, effectCount int) *harness {
	t.Helper()
	h := newHarness(t)
	h.state(model.TransactionRunning, "", nil)
	for i := 1; i <= effectCount; i++ {
		dependencies := []string(nil)
		if i > 1 {
			dependencies = []string{"effect:tx-demo:" + string(rune('0'+i-1))}
		}
		h.addEffect(effectFixture(h.now, i, dependencies))
	}
	h.stage()
	h.state(model.TransactionValidating, "", nil)
	for _, effect := range h.projection.Effects {
		h.effectState(effect.ID, model.EffectValidated, nil, nil)
	}
	h.verify(model.VerificationPassed)
	return h
}

func (h *harness) emit(eventType string, payload any) {
	h.t.Helper()
	event, err := h.next(eventType, payload)
	if err != nil {
		h.t.Fatal(err)
	}
	next, err := Apply(&h.projection, event)
	if err != nil {
		h.t.Fatalf("apply %s: %v", eventType, err)
	}
	h.projection = next
	h.events = append(h.events, event)
}

func (h *harness) emitError(eventType string, payload any) error {
	h.t.Helper()
	event, err := h.next(eventType, payload)
	if err != nil {
		return err
	}
	_, err = Apply(&h.projection, event)
	return err
}

func (h *harness) next(eventType string, payload any) (model.TransactionEvent, error) {
	h.now = h.now.Add(time.Second)
	return NextEvent(h.projection, eventType, h.actor, h.now, payload)
}

func (h *harness) state(to model.TransactionState, reason string, approvals []string) {
	h.t.Helper()
	h.emit(EventTransactionStateChanged, TransactionStateChangedPayload{
		From:                   h.projection.Transaction.State,
		To:                     to,
		Reason:                 reason,
		OutstandingApprovalIDs: approvals,
	})
}

func (h *harness) stateError(to model.TransactionState, reason string, approvals []string) error {
	h.t.Helper()
	return h.emitError(EventTransactionStateChanged, TransactionStateChangedPayload{
		From:                   h.projection.Transaction.State,
		To:                     to,
		Reason:                 reason,
		OutstandingApprovalIDs: approvals,
	})
}

func (h *harness) addEffect(effect model.Effect) {
	h.t.Helper()
	effect.Attempt = h.projection.Transaction.Attempt
	h.emit(EventEffectAdded, EffectAddedPayload{Effect: effect})
}

func (h *harness) startExecution(id string) model.AgentExecution {
	h.t.Helper()
	execution := model.AgentExecution{
		Version:             model.AgentExecutionVersion,
		ID:                  id,
		TransactionID:       h.projection.Transaction.ID,
		Attempt:             h.projection.Transaction.Attempt,
		RunID:               "run:demo",
		StageBindingID:      "stage:tx-demo",
		Program:             "agent",
		CommandDigest:       digest("1"),
		RuntimeClass:        "host",
		RuntimeConfigDigest: digest("2"),
		Status:              model.AgentExecutionRunning,
		StartedAt:           h.now.Add(time.Second),
	}
	h.emit(EventAgentExecutionStarted, AgentExecutionStartedPayload{
		Execution: execution,
	})
	return execution
}

func (h *harness) stage() {
	h.t.Helper()
	effectSetDigest, err := ComputeEffectSetDigest(h.projection.Effects)
	if err != nil {
		h.t.Fatal(err)
	}
	h.emit(EventTransactionStaged, TransactionStagedPayload{
		StagedStateDigest: digest("5"),
		EffectSetDigest:   effectSetDigest,
	})
}

func (h *harness) effectState(id string, to model.EffectStatus, commitReceipt, compensationReceipt *model.EffectReceipt) {
	h.t.Helper()
	index := effectIndex(h.projection.Effects, id)
	if index < 0 {
		h.t.Fatalf("missing effect %s", id)
	}
	h.emit(EventEffectStateChanged, EffectStateChangedPayload{
		EffectID:            id,
		From:                h.projection.Effects[index].Status,
		To:                  to,
		Receipt:             commitReceipt,
		CompensationReceipt: compensationReceipt,
	})
}

func (h *harness) verify(status model.VerificationStatus) {
	h.t.Helper()
	h.emit(EventVerificationRecorded, VerificationRecordedPayload{Result: h.verificationFixture(
		"verification:tx-demo:tests",
		status,
	)})
}

func (h *harness) verificationFixture(
	id string,
	status model.VerificationStatus,
) model.VerificationResult {
	h.t.Helper()
	evaluatedAt := h.now.Add(time.Second)
	expiresAt := evaluatedAt.Add(time.Hour)
	return model.VerificationResult{
		Version:           model.VerificationResultVersion,
		ID:                id,
		TransactionID:     h.projection.Transaction.ID,
		Attempt:           h.projection.Transaction.Attempt,
		Name:              "authentication-behavior",
		Kind:              model.VerificationInvariant,
		Status:            status,
		EffectSetDigest:   h.projection.Transaction.EffectSetDigest,
		StagedStateDigest: h.projection.Transaction.StagedStateDigest,
		Verifier:          model.Principal{ID: "service:verifier", Kind: model.PrincipalService},
		Independence:      model.VerificationPlatformRun,
		VerifierDigest:    digest("6"),
		InputsDigest:      digest("7"),
		Evidence: []model.ArtifactRef{{
			URI: "evidence://tx-demo/tests", Digest: digest("8"), MediaType: "application/json",
		}},
		Summary:     "verification completed",
		EvaluatedAt: evaluatedAt,
		ExpiresAt:   &expiresAt,
	}
}

func (h *harness) freezePlan() {
	h.t.Helper()
	steps := make([]model.CommitStep, len(h.projection.Effects))
	compensation := make([]model.CompensationStep, 0, len(h.projection.Effects))
	for i, effect := range h.projection.Effects {
		steps[i] = model.CommitStep{
			Sequence:        int64(i + 1),
			EffectID:        effect.ID,
			DependsOn:       append([]string(nil), effect.Dependencies...),
			PreconditionIDs: []string{"verification:tx-demo:tests"},
			IdempotencyKey:  effect.IdempotencyKey,
			TimeoutSeconds:  60,
			RequiresReceipt: true,
		}
	}
	for i := len(h.projection.Effects) - 1; i >= 0; i-- {
		compensation = append(compensation, model.CompensationStep{
			Sequence:        int64(len(compensation) + 1),
			EffectID:        h.projection.Effects[i].ID,
			PreconditionIDs: []string{},
		})
	}
	plan := model.CommitPlan{
		Version:                 model.CommitPlanVersion,
		ID:                      "commit-plan:tx-demo",
		TransactionID:           h.projection.Transaction.ID,
		Attempt:                 h.projection.Transaction.Attempt,
		IntentDigest:            h.projection.Transaction.IntentDigest,
		EffectSetDigest:         h.projection.Transaction.EffectSetDigest,
		StagedStateDigest:       h.projection.Transaction.StagedStateDigest,
		PolicyDigest:            digest("9"),
		Connector:               "git",
		Target:                  model.ResourceSelector{Kind: "git_ref", Pattern: "refs/heads/main"},
		ExpectedResourceVersion: strings.Repeat("a", 40),
		Steps:                   steps,
		CompensationSteps:       compensation,
		IrreversibleEffectIDs:   []string{},
		CreatedAt:               h.now.Add(time.Second),
	}
	var err error
	plan.Digest, err = ComputeCommitPlanDigest(plan)
	if err != nil {
		h.t.Fatal(err)
	}
	h.emit(EventCommitPlanFrozen, CommitPlanFrozenPayload{Plan: plan})
}

func releaseBindingFixture() ReleaseBinding {
	return ReleaseBinding{
		Connector: "git",
		Target: model.ResourceSelector{
			Kind: "git_ref", Pattern: "refs/heads/main",
		},
		ExpectedResourceVersion: strings.Repeat("a", 40),
	}
}

func (h *harness) freezeApproval(classes []string) {
	h.t.Helper()
	if classes == nil {
		classes = []string{}
	}
	approval := model.ApprovalPackage{
		Version:                 model.ApprovalPackageVersion,
		ID:                      "approval-package:tx-demo",
		TransactionID:           h.projection.Transaction.ID,
		Attempt:                 h.projection.Transaction.Attempt,
		IntentDigest:            h.projection.Transaction.IntentDigest,
		EffectSetDigest:         h.projection.Transaction.EffectSetDigest,
		StagedStateDigest:       h.projection.Transaction.StagedStateDigest,
		PolicyDigest:            digest("9"),
		CommitPlanDigest:        h.projection.Transaction.CommitPlanDigest,
		VerificationResultIDs:   append([]string(nil), h.projection.Transaction.VerificationResultIDs...),
		RiskFindings:            []model.RiskFinding{},
		RequiredApprovalClasses: classes,
		Summary:                 "transaction verification and effects",
		CreatedAt:               h.now.Add(time.Second),
		ExpiresAt:               h.now.Add(time.Hour),
	}
	var err error
	approval.Digest, err = ComputeApprovalPackageDigest(approval)
	if err != nil {
		h.t.Fatal(err)
	}
	h.emit(EventApprovalPackageFrozen, ApprovalPackageFrozenPayload{Package: approval})
}

func (h *harness) approvalDecision(
	approvalID string,
	packageDigest string,
	decision model.ApprovalDecisionKind,
	reason string,
) model.ApprovalDecision {
	h.t.Helper()
	value := model.ApprovalDecision{
		Version:         model.ApprovalDecisionVersion,
		ID:              "approval-decision:tx-demo:" + approvalID,
		TransactionID:   h.projection.Transaction.ID,
		ApprovalID:      approvalID,
		PackageDigest:   packageDigest,
		ApprovalClass:   "security-reviewer",
		Decision:        decision,
		Reason:          reason,
		Approver:        h.actor,
		KeyID:           "key:test",
		IssuedAt:        h.now,
		ExpiresAt:       h.now.Add(5 * time.Minute),
		Nonce:           "nonce:test",
		SignatureBase64: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==",
	}
	digest, err := ComputeApprovalDecisionDigest(value)
	if err != nil {
		h.t.Fatal(err)
	}
	value.Digest = digest
	return value
}

func effectFixture(now time.Time, sequence int, dependencies []string) model.Effect {
	bytes := int64(10 + sequence)
	return model.Effect{
		Version:         model.EffectVersion,
		ID:              "effect:tx-demo:" + string(rune('0'+sequence)),
		TransactionID:   "tx:demo",
		Attempt:         1,
		Sequence:        int64(sequence),
		RunID:           "run:demo",
		OriginActionID:  "action:tx-demo:" + string(rune('0'+sequence)),
		System:          "git",
		Resource:        model.ResourceSelector{Kind: "file", Pattern: "internal/service/file.go"},
		Operation:       "modify",
		Arguments:       json.RawMessage(`{"path":"internal/service/file.go"}`),
		ArgumentsDigest: digest("2"),
		Dependencies:    dependencies,
		EstimatedScope:  model.EffectScope{Bytes: &bytes},
		RecoveryClass:   model.RecoveryStageable,
		Status:          model.EffectStaged,
		IdempotencyKey:  "idempotency:tx-demo:" + string(rune('0'+sequence)),
		StageRef: &model.ArtifactRef{
			URI: "worktree://tx-demo/file.go", Digest: digest("3"), MediaType: "text/x-go",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func receipt(now time.Time) *model.EffectReceipt {
	return &model.EffectReceipt{
		Driver: "git", OperationID: "operation:demo", ResultDigest: digest("f"), CommittedAt: now.Add(time.Second),
	}
}

func digest(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}

func assertCode(t *testing.T, err error, want model.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s", want)
	}
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) {
		t.Fatalf("expected KernelError, got %T: %v", err, err)
	}
	if kernelErr.Code != want {
		t.Fatalf("code = %s, want %s: %v", kernelErr.Code, want, err)
	}
}
