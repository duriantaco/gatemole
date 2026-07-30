package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/admission"
	"github.com/duriantaco/vouch/internal/kernel/eventlog"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

func TestGetExecutionAuthorityTracksLifecycleAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kernel.db")
	runtimeID := runtimeMetadataTestID("8")
	kernelStore, err := OpenSQLiteForRuntime(
		ctx,
		path,
		runtimeID,
		EnforcementProfileDevelopment,
	)
	if err != nil {
		t.Fatal(err)
	}
	prepared := prepareStoreAdmissionForRuntime(
		t,
		"execution-authority",
		runtimeID,
	)
	initial, created, err := kernelStore.AdmitTask(ctx, prepared)
	if err != nil || !created {
		t.Fatalf("admit task: created=%v err=%v", created, err)
	}

	fresh, err := kernelStore.GetExecutionAuthority(
		ctx,
		prepared.Namespace,
		initial.Transaction.Transaction.ID,
	)
	if err != nil {
		t.Fatalf("get fresh execution authority: %v", err)
	}
	assertAuthorityImmutableAdmission(t, fresh, initial)
	if !reflect.DeepEqual(fresh.Run, initial.Run) ||
		!reflect.DeepEqual(fresh.Transaction, initial.Transaction) {
		t.Fatal("fresh authority did not return admitted lifecycle projections")
	}
	freshByRun, err := kernelStore.GetExecutionAuthorityForRun(
		ctx,
		prepared.Namespace,
		initial.Run.Run.ID,
	)
	if err != nil {
		t.Fatalf("get fresh execution authority by run: %v", err)
	}
	if !reflect.DeepEqual(freshByRun, fresh) {
		t.Fatal("run admission index resolved different execution authority")
	}

	currentRun, currentTransaction := advanceAuthorityLifecycle(
		t,
		ctx,
		kernelStore,
		prepared.Namespace,
		initial,
	)
	advanced, err := kernelStore.GetExecutionAuthority(
		ctx,
		prepared.Namespace,
		initial.Transaction.Transaction.ID,
	)
	if err != nil {
		t.Fatalf("get advanced execution authority: %v", err)
	}
	assertAuthorityImmutableAdmission(t, advanced, initial)
	if !reflect.DeepEqual(advanced.Run, currentRun) ||
		!reflect.DeepEqual(advanced.Transaction, currentTransaction) {
		t.Fatal("advanced authority did not return current lifecycle projections")
	}

	if err := kernelStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteForRuntime(
		ctx,
		path,
		runtimeID,
		EnforcementProfileDevelopment,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	afterRestart, err := reopened.GetExecutionAuthority(
		ctx,
		prepared.Namespace,
		initial.Transaction.Transaction.ID,
	)
	if err != nil {
		t.Fatalf("get execution authority after restart: %v", err)
	}
	if !reflect.DeepEqual(afterRestart, advanced) {
		t.Fatal("execution authority changed across store restart")
	}
}

func TestGetExecutionAuthorityRejectsDevelopmentAdoptedLegacyAdmission(
	t *testing.T,
) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	prepared := prepareStoreAdmission(t, "legacy-execution-authority")
	initial, created, err := kernelStore.AdmitTask(ctx, prepared)
	if err != nil || !created {
		t.Fatalf("admit legacy task: created=%v err=%v", created, err)
	}
	if initial.Version != admission.LegacyResultVersion ||
		initial.RuntimeID != "" ||
		initial.EnforcementProfile != "" {
		t.Fatalf("fixture is not an unbound v0 admission: %#v", initial)
	}

	runtimeID := runtimeMetadataTestID("9")
	if err := kernelStore.BindRuntimeMetadata(
		ctx,
		runtimeID,
		EnforcementProfileDevelopment,
	); err != nil {
		t.Fatalf("adopt legacy ledger in development: %v", err)
	}
	readable, requestDigest, err := kernelStore.GetTaskAdmission(
		ctx,
		prepared.Namespace,
		prepared.IdempotencyKey,
	)
	if err != nil {
		t.Fatalf("read adopted legacy admission: %v", err)
	}
	if requestDigest != prepared.RequestDigest {
		t.Fatalf(
			"legacy request digest=%q want=%q",
			requestDigest,
			prepared.RequestDigest,
		)
	}
	assertSameAdmission(t, readable, initial)

	beforeRun, err := kernelStore.GetRun(
		ctx,
		prepared.Namespace,
		initial.Run.Run.ID,
	)
	if err != nil {
		t.Fatalf("get run before authority denial: %v", err)
	}
	beforeTransaction, err := kernelStore.GetTransaction(
		ctx,
		prepared.Namespace,
		initial.Transaction.Transaction.ID,
	)
	if err != nil {
		t.Fatalf("get transaction before authority denial: %v", err)
	}

	snapshot, err := kernelStore.GetExecutionAuthority(
		ctx,
		prepared.Namespace,
		initial.Transaction.Transaction.ID,
	)
	if !reflect.DeepEqual(snapshot, ExecutionAuthoritySnapshot{}) {
		t.Fatalf("denied legacy authority returned a snapshot: %#v", snapshot)
	}
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) {
		t.Fatalf("authority error=%T %v, want *model.KernelError", err, err)
	}
	if kernelErr.Code != model.ErrorCapabilityDenied ||
		kernelErr.Operation != getExecutionAuthorityOperation ||
		kernelErr.Resource != initial.Transaction.Transaction.ID {
		t.Fatalf("unexpected authority denial: %#v", kernelErr)
	}
	snapshot, err = kernelStore.GetExecutionAuthorityForRun(
		ctx,
		prepared.Namespace,
		initial.Run.Run.ID,
	)
	if !reflect.DeepEqual(snapshot, ExecutionAuthoritySnapshot{}) {
		t.Fatalf(
			"denied legacy run authority returned a snapshot: %#v",
			snapshot,
		)
	}
	if !errors.As(err, &kernelErr) ||
		kernelErr.Code != model.ErrorCapabilityDenied {
		t.Fatalf("unexpected legacy run authority denial: %#v", err)
	}

	afterRun, err := kernelStore.GetRun(
		ctx,
		prepared.Namespace,
		initial.Run.Run.ID,
	)
	if err != nil {
		t.Fatalf("get run after authority denial: %v", err)
	}
	afterTransaction, err := kernelStore.GetTransaction(
		ctx,
		prepared.Namespace,
		initial.Transaction.Transaction.ID,
	)
	if err != nil {
		t.Fatalf("get transaction after authority denial: %v", err)
	}
	if !reflect.DeepEqual(afterRun, beforeRun) ||
		!reflect.DeepEqual(afterTransaction, beforeTransaction) {
		t.Fatal("legacy authority denial mutated lifecycle projections")
	}
	replayed, replayDigest, err := kernelStore.GetTaskAdmission(
		ctx,
		prepared.Namespace,
		prepared.IdempotencyKey,
	)
	if err != nil {
		t.Fatalf("re-read adopted legacy admission: %v", err)
	}
	if replayDigest != requestDigest {
		t.Fatalf("replayed request digest=%q want=%q", replayDigest, requestDigest)
	}
	assertSameAdmission(t, replayed, initial)
	if err := kernelStore.VerifyRun(
		ctx,
		prepared.Namespace,
		initial.Run.Run.ID,
	); err != nil {
		t.Fatalf("verify run after authority denial: %v", err)
	}
	if err := kernelStore.VerifyTransaction(
		ctx,
		prepared.Namespace,
		initial.Transaction.Transaction.ID,
	); err != nil {
		t.Fatalf("verify transaction after authority denial: %v", err)
	}
}

func TestGetExecutionAuthorityRejectsTransactionWithoutAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	legacy := model.AgentTransaction{
		Version:                model.AgentTransactionVersion,
		ID:                     "tx:legacy-authority",
		Namespace:              "store-test",
		IntentDigest:           "sha256:" + strings.Repeat("a", 64),
		Sponsor:                model.Principal{ID: "human:legacy", Kind: model.PrincipalHuman},
		AgentRunIDs:            []string{"run:legacy-authority"},
		StageBindings:          []model.StageBinding{},
		State:                  model.TransactionCreated,
		EffectIDs:              []string{},
		VerificationResultIDs:  []string{},
		OutstandingApprovalIDs: []string{},
		EventSequence:          1,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	event, err := transactionreducer.CreationEvent(legacy, legacy.Sponsor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kernelStore.CreateTransaction(ctx, event); err != nil {
		t.Fatalf("create legacy transaction: %v", err)
	}

	_, err = kernelStore.GetExecutionAuthority(
		ctx,
		legacy.Namespace,
		legacy.ID,
	)
	assertKernelCode(t, err, model.ErrorNotFound)

	_, err = kernelStore.GetExecutionAuthority(
		ctx,
		legacy.Namespace,
		"tx:missing-authority",
	)
	assertKernelCode(t, err, model.ErrorNotFound)
}

func TestGetExecutionAuthorityRejectsTamperedAuthority(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		tamper func(*testing.T, context.Context, *SQLiteStore, string)
	}{
		{
			name: "immutable task",
			tamper: func(
				t *testing.T,
				ctx context.Context,
				kernelStore *SQLiteStore,
				transactionID string,
			) {
				t.Helper()
				if _, err := kernelStore.db.ExecContext(
					ctx,
					`UPDATE agent_tasks SET task_json = ?
					 WHERE namespace = ? AND transaction_id = ?`,
					[]byte("{}"),
					"store-test",
					transactionID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "current run binding",
			tamper: func(
				t *testing.T,
				ctx context.Context,
				kernelStore *SQLiteStore,
				transactionID string,
			) {
				t.Helper()
				var runID string
				if err := kernelStore.db.QueryRowContext(
					ctx,
					`SELECT run_id FROM task_admissions
					 WHERE namespace = ? AND transaction_id = ?`,
					"store-test",
					transactionID,
				).Scan(&runID); err != nil {
					t.Fatal(err)
				}
				current, err := kernelStore.GetRun(ctx, "store-test", runID)
				if err != nil {
					t.Fatal(err)
				}
				current.Run.ContractDigest = "sha256:" + strings.Repeat("f", 64)
				data, err := json.Marshal(current.Run)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := kernelStore.db.ExecContext(
					ctx,
					`UPDATE runs SET state_json = ?
					 WHERE namespace = ? AND run_id = ?`,
					data,
					"store-test",
					runID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "advanced run event",
			tamper: func(
				t *testing.T,
				ctx context.Context,
				kernelStore *SQLiteStore,
				transactionID string,
			) {
				t.Helper()
				var runID string
				if err := kernelStore.db.QueryRowContext(
					ctx,
					`SELECT run_id FROM task_admissions
					 WHERE namespace = ? AND transaction_id = ?`,
					"store-test",
					transactionID,
				).Scan(&runID); err != nil {
					t.Fatal(err)
				}
				var data []byte
				if err := kernelStore.db.QueryRowContext(
					ctx,
					`SELECT event_json FROM events
					 WHERE namespace = ? AND run_id = ? AND sequence = 4`,
					"store-test",
					runID,
				).Scan(&data); err != nil {
					t.Fatal(err)
				}
				event, err := model.DecodeStrict[model.RunEvent](data)
				if err != nil {
					t.Fatal(err)
				}
				event.Actor.ID = "service:tampered"
				data, err = json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := kernelStore.db.ExecContext(
					ctx,
					`UPDATE events SET event_json = ?
					 WHERE namespace = ? AND run_id = ? AND sequence = 4`,
					data,
					"store-test",
					runID,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			kernelStore := openTestStore(t)
			suffix := "tamper-" + strings.ReplaceAll(test.name, " ", "-")
			runtimeID := runtimeMetadataTestID("a")
			if err := kernelStore.BindRuntimeMetadata(
				ctx,
				runtimeID,
				EnforcementProfileDevelopment,
			); err != nil {
				t.Fatal(err)
			}
			prepared := prepareStoreAdmissionForRuntime(
				t,
				suffix,
				runtimeID,
			)
			initial, created, err := kernelStore.AdmitTask(ctx, prepared)
			if err != nil || !created {
				t.Fatalf("admit task: created=%v err=%v", created, err)
			}
			if test.name == "advanced run event" {
				advanceAuthorityLifecycle(
					t,
					ctx,
					kernelStore,
					prepared.Namespace,
					initial,
				)
			}
			test.tamper(
				t,
				ctx,
				kernelStore,
				initial.Transaction.Transaction.ID,
			)

			_, err = kernelStore.GetExecutionAuthority(
				ctx,
				prepared.Namespace,
				initial.Transaction.Transaction.ID,
			)
			assertKernelCode(t, err, model.ErrorEventChain)
		})
	}
}

func advanceAuthorityLifecycle(
	t *testing.T,
	ctx context.Context,
	kernelStore *SQLiteStore,
	namespace string,
	initial admission.Result,
) (reducer.Projection, transactionreducer.Projection) {
	t.Helper()
	actor := model.Principal{
		ID: "service:gatemoled", Kind: model.PrincipalService, Issuer: "gatemoled",
	}
	advancedAt := initial.Task.CreatedAt.Add(time.Second)
	runEvent, err := eventlog.Next(
		initial.Run,
		reducer.EventRunStateChanged,
		actor,
		advancedAt,
		model.RunStateChangedPayload{
			From: model.RunAdmitted,
			To:   model.RunRunning,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	currentRun, err := kernelStore.AppendEvent(
		ctx,
		namespace,
		initial.Run.Run.EventSequence,
		runEvent,
	)
	if err != nil {
		t.Fatalf("advance admitted run: %v", err)
	}
	transactionEvent, err := transactionreducer.NextEvent(
		initial.Transaction,
		transactionreducer.EventTransactionStateChanged,
		actor,
		advancedAt,
		transactionreducer.TransactionStateChangedPayload{
			From: model.TransactionCreated,
			To:   model.TransactionRunning,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	currentTransaction, err := kernelStore.AppendTransactionEvents(
		ctx,
		namespace,
		initial.Transaction.Transaction.EventSequence,
		[]model.TransactionEvent{transactionEvent},
	)
	if err != nil {
		t.Fatalf("advance admitted transaction: %v", err)
	}
	return currentRun, currentTransaction
}

func assertAuthorityImmutableAdmission(
	t *testing.T,
	got ExecutionAuthoritySnapshot,
	want admission.Result,
) {
	t.Helper()
	if !reflect.DeepEqual(got.Task, want.Task) ||
		!reflect.DeepEqual(got.Contract, want.Contract) ||
		!reflect.DeepEqual(got.Grants, want.Grants) {
		t.Fatal("execution authority changed immutable admission resources")
	}
}
