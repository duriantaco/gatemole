package broker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/capability"
	"github.com/duriantaco/gatemole/internal/kernel/driver"
	"github.com/duriantaco/gatemole/internal/kernel/eventlog"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/reducer"
	"github.com/duriantaco/gatemole/internal/kernel/store"
)

func TestBrokerCommitsAllowedWriteAndAuditsDeniedEscape(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, "workspace"), 0o750); err != nil {
		t.Fatal(err)
	}
	kernelStore, err := store.OpenSQLite(filepath.Join(repo, "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	now := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	projection, grant := createRunningRunWithGrant(t, ctx, kernelStore, now)
	actionBroker, err := New(kernelStore, repo)
	if err != nil {
		t.Fatal(err)
	}
	actionBroker.now = func() time.Time { return now.Add(10 * time.Second) }

	content := []byte("governed write\n")
	write := filesystemAction(t, projection.Run.ID, "action:allowed", "idem:allowed", driver.FilesystemWrite, "workspace/allowed.txt", content)
	outcome, err := actionBroker.Execute(ctx, projection.Run.Namespace, ExecuteRequest{
		ExpectedSequence: projection.Run.EventSequence,
		Action:           write,
		InputBase64:      base64.StdEncoding.EncodeToString(content),
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != "committed" || outcome.CapabilityID != grant.ID || outcome.ResultDigest != driver.DigestBytes(content) {
		t.Fatalf("unexpected outcome: %#v", outcome)
	}
	data, err := os.ReadFile(filepath.Join(repo, "workspace", "allowed.txt"))
	if err != nil || string(data) != string(content) {
		t.Fatalf("committed file data=%q err=%v", data, err)
	}

	escapeContent := []byte("must not escape")
	escape := filesystemAction(t, projection.Run.ID, "action:escape", "idem:escape", driver.FilesystemWrite, "workspace/../outside.txt", escapeContent)
	escapeOutcome, err := actionBroker.Execute(ctx, projection.Run.Namespace, ExecuteRequest{
		ExpectedSequence: outcome.Projection.Run.EventSequence,
		Action:           escape,
		InputBase64:      base64.StdEncoding.EncodeToString(escapeContent),
	})
	assertCode(t, err, model.ErrorCapabilityDenied)
	if escapeOutcome.Status != "denied" {
		t.Fatalf("escape status=%q", escapeOutcome.Status)
	}
	if _, err := os.Stat(filepath.Join(repo, "outside.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied path was modified: %v", err)
	}

	events, err := kernelStore.Events(ctx, projection.Run.Namespace, projection.Run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	wantTail := []string{
		reducer.EventActionRequested,
		reducer.EventActionAuthorized,
		reducer.EventActionExecuting,
		reducer.EventActionCommitted,
		reducer.EventActionRequested,
		reducer.EventActionDenied,
	}
	if len(types) < len(wantTail) {
		t.Fatalf("event history too short: %v", types)
	}
	for i, want := range wantTail {
		if got := types[len(types)-len(wantTail)+i]; got != want {
			t.Fatalf("event tail=%v, want %v", types, wantTail)
		}
	}
	if err := kernelStore.VerifyRun(ctx, projection.Run.Namespace, projection.Run.ID); err != nil {
		t.Fatalf("verify audited run: %v", err)
	}
}

func TestAuthorizeFailsClosedForExpiredRevokedApprovalAndUseLimit(t *testing.T) {
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	request := model.ActionRequest{
		RunID:     "run:test",
		Operation: driver.FilesystemWrite,
		Resource:  model.ResourceSelector{Kind: "filesystem", Pattern: "workspace/file"},
	}
	maxUses := int64(1)
	base := model.CapabilityGrant{
		ID:           "cap:test",
		SubjectRunID: request.RunID,
		Resource:     model.ResourceSelector{Kind: "filesystem", Pattern: "workspace/**"},
		Operations:   []string{driver.FilesystemWrite},
		IssuedAt:     now.Add(-time.Hour),
		ExpiresAt:    now.Add(time.Hour),
		MaxUses:      &maxUses,
	}
	tests := []struct {
		name string
		edit func(*model.CapabilityGrant)
		uses int64
		code model.ErrorCode
	}{
		{"expired", func(grant *model.CapabilityGrant) { grant.ExpiresAt = now }, 0, model.ErrorCapabilityExpired},
		{"revoked", func(grant *model.CapabilityGrant) { revoked := now.Add(-time.Minute); grant.RevokedAt = &revoked }, 0, model.ErrorCapabilityRevoked},
		{"approval", func(grant *model.CapabilityGrant) { grant.Conditions.ApprovalRequired = true }, 0, model.ErrorApprovalRequired},
		{"uses", func(_ *model.CapabilityGrant) {}, 1, model.ErrorCapabilityDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			grant := base
			test.edit(&grant)
			_, err := authorize(request, map[string]model.CapabilityGrant{grant.ID: grant}, map[string]int64{grant.ID: test.uses}, now)
			assertCode(t, err, test.code)
		})
	}
}

func TestBrokerRejectsDuplicateIdempotencyKeyWithoutSecondEffect(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, "workspace"), 0o750); err != nil {
		t.Fatal(err)
	}
	kernelStore, err := store.OpenSQLite(filepath.Join(repo, "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	now := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	projection, _ := createRunningRunWithGrant(t, ctx, kernelStore, now)
	actionBroker, err := New(kernelStore, repo)
	if err != nil {
		t.Fatal(err)
	}
	actionBroker.now = func() time.Time { return now.Add(10 * time.Second) }
	first := filesystemAction(t, projection.Run.ID, "action:first", "idem:same", driver.FilesystemWrite, "workspace/file.txt", []byte("first"))
	outcome, err := actionBroker.Execute(ctx, projection.Run.Namespace, ExecuteRequest{
		ExpectedSequence: projection.Run.EventSequence,
		Action:           first,
		InputBase64:      base64.StdEncoding.EncodeToString([]byte("first")),
	})
	if err != nil {
		t.Fatal(err)
	}
	second := filesystemAction(t, projection.Run.ID, "action:second", "idem:same", driver.FilesystemWrite, "workspace/file.txt", []byte("second"))
	_, err = actionBroker.Execute(ctx, projection.Run.Namespace, ExecuteRequest{
		ExpectedSequence: outcome.Projection.Run.EventSequence,
		Action:           second,
		InputBase64:      base64.StdEncoding.EncodeToString([]byte("second")),
	})
	assertCode(t, err, model.ErrorConflict)
	data, err := os.ReadFile(filepath.Join(repo, "workspace", "file.txt"))
	if err != nil || string(data) != "first" {
		t.Fatalf("duplicate changed file data=%q err=%v", data, err)
	}
}

func TestRecoverMarksAuthorizedInterruptedActionUnknownWithoutRetry(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, "workspace"), 0o750); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(repo, "kernel.db")
	kernelStore, err := store.OpenSQLite(database)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC)
	projection, grant := createRunningRunWithGrant(t, ctx, kernelStore, now)
	action := filesystemAction(t, projection.Run.ID, "action:interrupted", "idem:interrupted", driver.FilesystemWrite, "workspace/ambiguous.txt", []byte("maybe committed"))
	steps := []struct {
		eventType string
		payload   any
	}{
		{reducer.EventActionRequested, model.ActionRequestedPayload{Request: action}},
		{reducer.EventActionAuthorized, model.ActionDecisionPayload{
			ActionID: action.ID, Decision: model.DecisionAllow, CapabilityID: grant.ID, Reason: "authorized before crash",
		}},
		{reducer.EventActionExecuting, model.ActionExecutingPayload{ActionID: action.ID, CapabilityID: grant.ID}},
	}
	for _, step := range steps {
		event, buildErr := eventlog.Next(projection, step.eventType, servicePrincipal(), now.Add(time.Minute), step.payload)
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		projection, err = kernelStore.AppendEvent(ctx, projection.Run.Namespace, projection.Run.EventSequence, event)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Simulate the hard case: the driver effect occurred, but the committed
	// receipt did not make it into the event log before process death.
	ambiguousPath := filepath.Join(repo, "workspace", "ambiguous.txt")
	if err := os.WriteFile(ambiguousPath, []byte("maybe committed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := kernelStore.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.OpenSQLite(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	actionBroker, err := New(reopened, repo, WithClock(func() time.Time { return now.Add(2 * time.Minute) }))
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := actionBroker.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("recovered=%d, want 1", recovered)
	}
	recovered, err = actionBroker.Recover(ctx)
	if err != nil || recovered != 0 {
		t.Fatalf("second recovery=%d err=%v, want idempotent zero", recovered, err)
	}
	events, err := reopened.Events(ctx, projection.Run.Namespace, projection.Run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Type != reducer.EventActionUnknown {
		t.Fatalf("last event=%s, want %s", events[len(events)-1].Type, reducer.EventActionUnknown)
	}
	data, err := os.ReadFile(ambiguousPath)
	if err != nil || string(data) != "maybe committed" {
		t.Fatalf("recovery retried or changed ambiguous effect: data=%q err=%v", data, err)
	}
	if err := reopened.VerifyRun(ctx, projection.Run.Namespace, projection.Run.ID); err != nil {
		t.Fatal(err)
	}
}

func createRunningRunWithGrant(
	t *testing.T,
	ctx context.Context,
	kernelStore store.Store,
	now time.Time,
) (reducer.Projection, model.CapabilityGrant) {
	t.Helper()
	digest := "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	deadline := now.Add(2 * time.Hour)
	maxCalls := int64(10)
	run := model.AgentRun{
		Version:        model.AgentRunVersion,
		ID:             "run:broker-test",
		Namespace:      "test",
		ImageDigest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ContractDigest: digest,
		Principal:      model.Principal{ID: "run:broker-test", Kind: model.PrincipalRun, Issuer: "gatemoled"},
		DelegationChain: []model.Principal{
			{ID: "human:test", Kind: model.PrincipalHuman},
		},
		State:                  model.RunCreated,
		Deadline:               &deadline,
		BudgetLimits:           model.BudgetLimits{MaxToolCalls: &maxCalls},
		Workspace:              "workspace",
		CapabilityIDs:          []string{},
		OutstandingApprovalIDs: []string{},
		EventSequence:          1,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	createdPayload, _ := json.Marshal(model.RunCreatedPayload{Run: run})
	created := model.RunEvent{
		Version: model.RunEventVersion, ID: eventlog.ID(run.ID, 1), RunID: run.ID, Sequence: 1,
		Type: reducer.EventRunCreated, Actor: run.DelegationChain[0], OccurredAt: now, Payload: createdPayload,
	}
	created.Digest, _ = model.ComputeEventDigest(created)
	projection, err := kernelStore.CreateRun(ctx, created)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []model.RunState{model.RunAdmitted, model.RunRunning} {
		event, buildErr := eventlog.Next(projection, reducer.EventRunStateChanged, model.Principal{ID: "operator:test", Kind: model.PrincipalOperator}, now, model.RunStateChangedPayload{From: projection.Run.State, To: state})
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		projection, err = kernelStore.AppendEvent(ctx, run.Namespace, projection.Run.EventSequence, event)
		if err != nil {
			t.Fatal(err)
		}
	}
	contract := model.ExecutionContract{
		Version: model.ExecutionContractVersion,
		ID:      "contract:broker-test",
		Digest:  digest,
		Owner:   run.DelegationChain[0],
		Goal:    "write only inside the run workspace",
		Risk:    "high",
		Resources: []model.ContractResource{
			{
				ID:         "workspace",
				Selector:   model.ResourceSelector{Kind: "filesystem", Pattern: "workspace/**"},
				Operations: []string{driver.FilesystemRead, driver.FilesystemWrite},
				Conditions: model.CapabilityConditions{WorkspaceRoot: "workspace"},
			},
		},
		Budgets:  model.BudgetLimits{MaxToolCalls: &maxCalls},
		Deadline: &deadline,
	}
	grants, err := capability.Compile(contract, projection.Run, now)
	if err != nil {
		t.Fatal(err)
	}
	grantEvent, err := eventlog.Next(projection, reducer.EventCapabilitiesGranted, model.Principal{ID: "service:gatemoled", Kind: model.PrincipalService}, now,
		model.CapabilitiesGrantedPayload{ContractDigest: contract.Digest, Grants: grants})
	if err != nil {
		t.Fatal(err)
	}
	projection, err = kernelStore.AppendEvent(ctx, run.Namespace, projection.Run.EventSequence, grantEvent)
	if err != nil {
		t.Fatal(err)
	}
	return projection, grants[0]
}

func filesystemAction(
	t *testing.T,
	runID, actionID, idempotencyKey, operation, logicalPath string,
	content []byte,
) model.ActionRequest {
	t.Helper()
	var arguments json.RawMessage
	if operation == driver.FilesystemWrite {
		arguments, _ = json.Marshal(driver.FilesystemWriteArguments{Path: logicalPath, ContentDigest: driver.DigestBytes(content)})
	} else {
		arguments, _ = json.Marshal(driver.FilesystemReadArguments{Path: logicalPath})
	}
	_, _, normalized, _ := driver.NormalizeArguments(operation, arguments)
	if normalized == nil {
		// Unsafe paths intentionally fail normalization; the request still uses
		// a structurally valid digest and is denied before driver execution.
		normalized = arguments
	}
	return model.ActionRequest{
		Version:         model.ActionRequestVersion,
		ID:              actionID,
		RunID:           runID,
		Operation:       operation,
		Resource:        model.ResourceSelector{Kind: "filesystem", Pattern: logicalPath},
		Arguments:       arguments,
		ArgumentsDigest: driver.DigestBytes(normalized),
		IdempotencyKey:  idempotencyKey,
		Intent:          "test a governed filesystem action",
		RequestedAt:     time.Date(2026, 7, 23, 0, 0, 5, 0, time.UTC),
		Attempt:         1,
	}
}

func assertCode(t *testing.T, err error, want model.ErrorCode) {
	t.Helper()
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) || kernelErr.Code != want {
		t.Fatalf("error=%v, want %s", err, want)
	}
}
