package store

import (
	"context"
	"errors"
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

func TestAppendPairedExecutionEventClaimsBothLedgers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	fixture := prepareLaunchClaim(t, ctx, kernelStore, "claim-success")

	paired, err := kernelStore.AppendPairedExecutionEvent(
		ctx,
		fixture.admission.Task.Namespace,
		ExecutionHeads{
			TransactionSequence: fixture.transaction.Transaction.EventSequence,
			TransactionDigest:   fixture.transaction.LastEventDigest,
			RunSequence:         fixture.admission.Run.Run.EventSequence,
			RunDigest:           fixture.admission.Run.LastEventDigest,
		},
		fixture.startEvent,
	)
	if err != nil {
		t.Fatalf("claim launch: %v", err)
	}
	claimed := paired.Transaction
	if claimed.Transaction.EventSequence !=
		fixture.transaction.Transaction.EventSequence+1 ||
		len(claimed.Executions) != 1 ||
		claimed.Executions[0].Status != model.AgentExecutionRunning ||
		claimed.Executions[0].RunID != fixture.admission.Run.Run.ID {
		t.Fatalf("unexpected claimed projection: %#v", claimed)
	}
	if paired.Run.Run.State != model.RunRunning ||
		paired.Run.Run.ActiveExecutionID != claimed.Executions[0].ID ||
		paired.Run.Run.EventSequence != fixture.admission.Run.Run.EventSequence+1 {
		t.Fatalf("run was not paired with launch: %#v", paired.Run)
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
	runEvents, err := kernelStore.Events(
		ctx,
		fixture.admission.Task.Namespace,
		fixture.admission.Run.Run.ID,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(runEvents) != int(paired.Run.Run.EventSequence) ||
		runEvents[len(runEvents)-1].Type != reducer.EventRunExecutionStarted {
		t.Fatalf("paired run event was not durably appended: %#v", runEvents)
	}
}

func TestAppendPairedExecutionEventConflictsWithoutPartialAppend(
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

	_, err = kernelStore.AppendPairedExecutionEvent(
		ctx,
		fixture.admission.Task.Namespace,
		ExecutionHeads{
			TransactionSequence: fixture.transaction.Transaction.EventSequence,
			TransactionDigest:   fixture.transaction.LastEventDigest,
			RunSequence:         fixture.admission.Run.Run.EventSequence,
			RunDigest:           fixture.admission.Run.LastEventDigest,
		},
		fixture.startEvent,
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

func TestAppendPairedExecutionEventSettlesRunAndChargesWallTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	fixture := prepareLaunchClaim(t, ctx, kernelStore, "settle")
	paired, err := kernelStore.AppendPairedExecutionEvent(
		ctx,
		fixture.admission.Task.Namespace,
		ExecutionHeads{
			TransactionSequence: fixture.transaction.Transaction.EventSequence,
			TransactionDigest:   fixture.transaction.LastEventDigest,
			RunSequence:         fixture.admission.Run.Run.EventSequence,
			RunDigest:           fixture.admission.Run.LastEventDigest,
		},
		fixture.startEvent,
	)
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 17
	completedAt := paired.Transaction.Executions[0].StartedAt.Add(2500 * time.Millisecond)
	finished, err := transactionreducer.NextEvent(
		paired.Transaction,
		transactionreducer.EventAgentExecutionFinished,
		model.Principal{ID: "service:gatemoled-runtime", Kind: model.PrincipalService},
		completedAt,
		transactionreducer.AgentExecutionFinishedPayload{
			ExecutionID:  paired.Transaction.Executions[0].ID,
			Status:       model.AgentExecutionFailed,
			ExitCode:     &exitCode,
			StdoutDigest: "sha256:" + strings.Repeat("e", 64),
			StderrDigest: "sha256:" + strings.Repeat("f", 64),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	paired, err = kernelStore.AppendPairedExecutionEvent(
		ctx,
		fixture.admission.Task.Namespace,
		ExecutionHeads{
			TransactionSequence: paired.Transaction.Transaction.EventSequence,
			TransactionDigest:   paired.Transaction.LastEventDigest,
			RunSequence:         paired.Run.Run.EventSequence,
			RunDigest:           paired.Run.LastEventDigest,
		},
		finished,
	)
	if err != nil {
		t.Fatal(err)
	}
	if paired.Transaction.Executions[0].Status != model.AgentExecutionFailed ||
		paired.Run.Run.State != model.RunWaitingForAgent ||
		paired.Run.Run.ActiveExecutionID != "" ||
		paired.Run.Run.BudgetUsage.WallTimeSeconds != 3 {
		t.Fatalf("execution did not settle both ledgers: %#v", paired)
	}
	if err := kernelStore.VerifyRun(
		ctx,
		fixture.admission.Task.Namespace,
		paired.Run.Run.ID,
	); err != nil {
		t.Fatalf("verify settled run: %v", err)
	}
	if err := kernelStore.VerifyTransaction(
		ctx,
		fixture.admission.Task.Namespace,
		paired.Transaction.Transaction.ID,
	); err != nil {
		t.Fatalf("verify settled transaction: %v", err)
	}
}

func TestPairedSettlementReconcilesLegacyUnpairedActiveExecution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	fixture := prepareLaunchClaim(t, ctx, kernelStore, "legacy-active")
	legacyTransaction, err := kernelStore.AppendTransactionEvents(
		ctx,
		fixture.admission.Task.Namespace,
		fixture.transaction.Transaction.EventSequence,
		[]model.TransactionEvent{fixture.startEvent},
	)
	if err != nil {
		t.Fatal(err)
	}
	exitCode := 9
	finished, err := transactionreducer.NextEvent(
		legacyTransaction,
		transactionreducer.EventAgentExecutionFinished,
		model.Principal{ID: "service:gatemoled-recovery", Kind: model.PrincipalService},
		legacyTransaction.Executions[0].StartedAt.Add(2*time.Second),
		transactionreducer.AgentExecutionFinishedPayload{
			ExecutionID:  legacyTransaction.Executions[0].ID,
			Status:       model.AgentExecutionFailed,
			ExitCode:     &exitCode,
			StdoutDigest: "sha256:" + strings.Repeat("e", 64),
			StderrDigest: "sha256:" + strings.Repeat("f", 64),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	paired, err := kernelStore.AppendPairedExecutionEvent(
		ctx,
		fixture.admission.Task.Namespace,
		ExecutionHeads{
			TransactionSequence: legacyTransaction.Transaction.EventSequence,
			TransactionDigest:   legacyTransaction.LastEventDigest,
			RunSequence:         fixture.admission.Run.Run.EventSequence,
			RunDigest:           fixture.admission.Run.LastEventDigest,
		},
		finished,
	)
	if err != nil {
		t.Fatal(err)
	}
	if paired.Run.Run.State != model.RunWaitingForAgent ||
		paired.Run.Run.ActiveExecutionID != "" ||
		paired.Run.Run.EventSequence != fixture.admission.Run.Run.EventSequence+2 ||
		paired.Run.Run.BudgetUsage.WallTimeSeconds != 2 {
		t.Fatalf("legacy execution was not reconciled: %#v", paired.Run)
	}
	events, err := kernelStore.Events(
		ctx,
		fixture.admission.Task.Namespace,
		fixture.admission.Run.Run.ID,
		fixture.admission.Run.Run.EventSequence,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != reducer.EventRunExecutionStarted ||
		events[1].Type != reducer.EventRunExecutionFinished {
		t.Fatalf("legacy reconciliation events=%#v", events)
	}
}

func TestPairedExecutionFaultsRollBackBothLedgers(t *testing.T) {
	t.Parallel()
	for _, point := range pairedExecutionFaultPoints {
		point := point
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			kernelStore := openTestStore(t)
			fixture := prepareLaunchClaim(
				t,
				ctx,
				kernelStore,
				"fault-"+strings.ReplaceAll(point, "_", "-"),
			)
			beforeRunEvents, err := kernelStore.Events(
				ctx,
				fixture.admission.Task.Namespace,
				fixture.admission.Run.Run.ID,
				0,
			)
			if err != nil {
				t.Fatal(err)
			}
			beforeTransactionEvents, err := kernelStore.TransactionEvents(
				ctx,
				fixture.admission.Task.Namespace,
				fixture.transaction.Transaction.ID,
				0,
			)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("injected paired execution failure")
			kernelStore.pairedExecutionFault = func(candidate string) error {
				if candidate == point {
					return injected
				}
				return nil
			}
			_, err = kernelStore.AppendPairedExecutionEvent(
				ctx,
				fixture.admission.Task.Namespace,
				ExecutionHeads{
					TransactionSequence: fixture.transaction.Transaction.EventSequence,
					TransactionDigest:   fixture.transaction.LastEventDigest,
					RunSequence:         fixture.admission.Run.Run.EventSequence,
					RunDigest:           fixture.admission.Run.LastEventDigest,
				},
				fixture.startEvent,
			)
			if !errors.Is(err, injected) {
				t.Fatalf("paired append error=%v, want injected fault", err)
			}
			afterRun, getErr := kernelStore.GetRun(
				ctx,
				fixture.admission.Task.Namespace,
				fixture.admission.Run.Run.ID,
			)
			if getErr != nil {
				t.Fatal(getErr)
			}
			afterTransaction, getErr := kernelStore.GetTransaction(
				ctx,
				fixture.admission.Task.Namespace,
				fixture.transaction.Transaction.ID,
			)
			if getErr != nil {
				t.Fatal(getErr)
			}
			afterRunEvents, getErr := kernelStore.Events(
				ctx,
				fixture.admission.Task.Namespace,
				fixture.admission.Run.Run.ID,
				0,
			)
			if getErr != nil {
				t.Fatal(getErr)
			}
			afterTransactionEvents, getErr := kernelStore.TransactionEvents(
				ctx,
				fixture.admission.Task.Namespace,
				fixture.transaction.Transaction.ID,
				0,
			)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if !reflect.DeepEqual(afterRun, fixture.admission.Run) ||
				!reflect.DeepEqual(afterTransaction, fixture.transaction) ||
				!reflect.DeepEqual(afterRunEvents, beforeRunEvents) ||
				!reflect.DeepEqual(afterTransactionEvents, beforeTransactionEvents) {
				t.Fatal("fault left a partial paired execution mutation")
			}
		})
	}
}

func TestExecutionBudgetUsageIncludesModelReceiptAndRoundsWallTimeUp(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(time.Second + time.Nanosecond)
	usage := executionBudgetUsage(model.AgentExecution{
		StartedAt:   startedAt,
		CompletedAt: &completedAt,
		ModelBroker: &model.ModelBrokerExecution{
			Calls: 4, InputTokens: 120, OutputTokens: 45,
		},
	}, model.AgentRun{})
	if usage.WallTimeSeconds != 2 || usage.ModelCalls != 4 ||
		usage.InputTokens != 120 || usage.OutputTokens != 45 {
		t.Fatalf("unexpected execution budget charge: %#v", usage)
	}
}

func TestExecutionBudgetUsageSaturatesAtRemainingHardLimits(t *testing.T) {
	t.Parallel()
	startedAt := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(10 * time.Second)
	maxCalls, maxInput, maxOutput, maxWall := int64(5), int64(100), int64(80), int64(20)
	usage := executionBudgetUsage(model.AgentExecution{
		StartedAt:   startedAt,
		CompletedAt: &completedAt,
		ModelBroker: &model.ModelBrokerExecution{
			Calls: 4, InputTokens: 120, OutputTokens: 45,
		},
	}, model.AgentRun{
		BudgetLimits: model.BudgetLimits{
			MaxModelCalls: &maxCalls, MaxInputTokens: &maxInput,
			MaxOutputTokens: &maxOutput, MaxWallTimeSeconds: &maxWall,
		},
		BudgetUsage: model.BudgetUsage{
			ModelCalls: 3, InputTokens: 90, OutputTokens: 70, WallTimeSeconds: 18,
		},
	})
	if usage.ModelCalls != 2 || usage.InputTokens != 10 ||
		usage.OutputTokens != 10 || usage.WallTimeSeconds != 2 {
		t.Fatalf("execution charge did not saturate at hard limits: %#v", usage)
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
