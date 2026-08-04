package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/admission"
	"github.com/duriantaco/gatemole/internal/kernel/eventlog"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/reducer"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
)

func TestAppendTransactionEventsIfRunCurrentClaimsLaunch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	fixture := prepareLaunchClaim(t, ctx, kernelStore, "claim-success")

	claimed, err := kernelStore.AppendTransactionEventsIfRunCurrent(
		ctx,
		fixture.admission.Task.Namespace,
		fixture.transaction.Transaction.EventSequence,
		fixture.admission.Run.Run.EventSequence,
		fixture.admission.Run.LastEventDigest,
		[]model.TransactionEvent{fixture.startEvent},
	)
	if err != nil {
		t.Fatalf("claim launch: %v", err)
	}
	if claimed.Transaction.EventSequence !=
		fixture.transaction.Transaction.EventSequence+1 ||
		len(claimed.Executions) != 1 ||
		claimed.Executions[0].Status != model.AgentExecutionRunning ||
		claimed.Executions[0].RunID != fixture.admission.Run.Run.ID {
		t.Fatalf("unexpected claimed projection: %#v", claimed)
	}
	currentRun, err := kernelStore.GetRun(
		ctx,
		fixture.admission.Task.Namespace,
		fixture.admission.Run.Run.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(currentRun, fixture.admission.Run) {
		t.Fatal("launch claim mutated the admitted run")
	}
	events, err := kernelStore.TransactionEvents(
		ctx,
		fixture.admission.Task.Namespace,
		fixture.admission.Transaction.Transaction.ID,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != int(claimed.Transaction.EventSequence) ||
		events[len(events)-1].Type !=
			transactionreducer.EventAgentExecutionStarted {
		t.Fatalf("launch event was not durably appended: %#v", events)
	}
}

func TestAppendTransactionEventsIfRunCurrentConflictsWithoutAppend(
	t *testing.T,
) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	fixture := prepareLaunchClaim(t, ctx, kernelStore, "claim-run-conflict")
	beforeEvents, err := kernelStore.TransactionEvents(
		ctx,
		fixture.admission.Task.Namespace,
		fixture.admission.Transaction.Transaction.ID,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}

	actor := model.Principal{
		ID: "service:gatemoled", Kind: model.PrincipalService, Issuer: "gatemoled",
	}
	runEvent, err := eventlog.Next(
		fixture.admission.Run,
		reducer.EventRunStateChanged,
		actor,
		fixture.admission.Task.CreatedAt.Add(3*time.Second),
		model.RunStateChangedPayload{
			From: model.RunAdmitted,
			To:   model.RunRunning,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kernelStore.AppendEvent(
		ctx,
		fixture.admission.Task.Namespace,
		fixture.admission.Run.Run.EventSequence,
		runEvent,
	); err != nil {
		t.Fatalf("advance run before launch claim: %v", err)
	}

	_, err = kernelStore.AppendTransactionEventsIfRunCurrent(
		ctx,
		fixture.admission.Task.Namespace,
		fixture.transaction.Transaction.EventSequence,
		fixture.admission.Run.Run.EventSequence,
		fixture.admission.Run.LastEventDigest,
		[]model.TransactionEvent{fixture.startEvent},
	)
	assertKernelCode(t, err, model.ErrorConflict)

	after, err := kernelStore.GetTransaction(
		ctx,
		fixture.admission.Task.Namespace,
		fixture.admission.Transaction.Transaction.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, fixture.transaction) {
		t.Fatal("run-head conflict changed the transaction projection")
	}
	afterEvents, err := kernelStore.TransactionEvents(
		ctx,
		fixture.admission.Task.Namespace,
		fixture.admission.Transaction.Transaction.ID,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterEvents, beforeEvents) {
		t.Fatal("run-head conflict appended a transaction event")
	}
}

type launchClaimFixture struct {
	admission   admission.Result
	transaction transactionreducer.Projection
	startEvent  model.TransactionEvent
}

func prepareLaunchClaim(
	t *testing.T,
	ctx context.Context,
	kernelStore *SQLiteStore,
	suffix string,
) launchClaimFixture {
	t.Helper()
	prepared := prepareStoreAdmission(t, suffix)
	admitted, created, err := kernelStore.AdmitTask(ctx, prepared)
	if err != nil || !created {
		t.Fatalf("admit task: created=%v err=%v", created, err)
	}
	actor := model.Principal{
		ID: "service:gatemoled", Kind: model.PrincipalService, Issuer: "gatemoled",
	}
	startedAt := admitted.Task.CreatedAt.Add(time.Second)
	started, err := transactionreducer.NextEvent(
		admitted.Transaction,
		transactionreducer.EventTransactionStateChanged,
		actor,
		startedAt,
		transactionreducer.TransactionStateChangedPayload{
			From: model.TransactionCreated,
			To:   model.TransactionRunning,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := kernelStore.AppendTransactionEvents(
		ctx,
		prepared.Namespace,
		admitted.Transaction.Transaction.EventSequence,
		[]model.TransactionEvent{started},
	)
	if err != nil {
		t.Fatal(err)
	}

	boundAt := startedAt.Add(time.Second)
	binding := model.StageBinding{
		ID:   "stage:" + suffix,
		Kind: "git_worktree",
		Resource: model.ResourceSelector{
			Kind: "git_repository", Pattern: "repo:" + suffix,
		},
		Location:     "/staging/" + suffix,
		BaseRevision: strings.Repeat("a", 40),
		CreatedAt:    boundAt,
	}
	bound, err := transactionreducer.NextEvent(
		transaction,
		transactionreducer.EventStageBindingCreated,
		actor,
		boundAt,
		transactionreducer.StageBindingCreatedPayload{Binding: binding},
	)
	if err != nil {
		t.Fatal(err)
	}
	transaction, err = kernelStore.AppendTransactionEvents(
		ctx,
		prepared.Namespace,
		transaction.Transaction.EventSequence,
		[]model.TransactionEvent{bound},
	)
	if err != nil {
		t.Fatal(err)
	}

	executionAt := boundAt.Add(time.Second)
	execution := model.AgentExecution{
		Version: model.AgentExecutionVersion,
		ID: transactionreducer.ExecutionID(
			transaction.Transaction.ID,
			transaction.Transaction.EventSequence+1,
		),
		TransactionID:       transaction.Transaction.ID,
		Attempt:             transaction.Transaction.Attempt,
		RunID:               admitted.Run.Run.ID,
		StageBindingID:      binding.ID,
		Program:             "agent",
		CommandDigest:       admitted.Task.AgentProfile.CommandDigest,
		RuntimeClass:        "oci",
		RuntimeConfigDigest: "sha256:" + strings.Repeat("d", 64),
		ImageDigest:         admitted.Task.AgentProfile.ImageDigest,
		TaskDigest:          admitted.Task.Digest,
		Status:              model.AgentExecutionRunning,
		StartedAt:           executionAt,
	}
	startEvent, err := transactionreducer.NextEvent(
		transaction,
		transactionreducer.EventAgentExecutionStarted,
		actor,
		executionAt,
		transactionreducer.AgentExecutionStartedPayload{
			Execution: execution,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return launchClaimFixture{
		admission:   admitted,
		transaction: transaction,
		startEvent:  startEvent,
	}
}
