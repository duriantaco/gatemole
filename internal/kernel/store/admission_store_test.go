package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/admission"
	"github.com/duriantaco/gatemole/internal/kernel/eventlog"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/reducer"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
)

func TestTaskAdmissionPersistsAtomicallyAndReplaysIdempotently(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kernel.db")
	kernelStore, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	prepared := prepareStoreAdmission(t, "replay")

	first, created, err := kernelStore.AdmitTask(ctx, prepared)
	if err != nil {
		t.Fatalf("admit task: %v", err)
	}
	if !created {
		t.Fatal("first admission was reported as a replay")
	}
	assertAdmissionRows(t, kernelStore, []int{1, 1, 1, 3, 1, 1, 1})
	if err := kernelStore.VerifyRun(
		ctx,
		prepared.Namespace,
		first.Run.Run.ID,
	); err != nil {
		t.Fatalf("verify admitted run: %v", err)
	}
	if err := kernelStore.VerifyTransaction(
		ctx,
		prepared.Namespace,
		first.Transaction.Transaction.ID,
	); err != nil {
		t.Fatalf("verify admitted transaction: %v", err)
	}

	replayed, created, err := kernelStore.AdmitTask(ctx, prepared)
	if err != nil {
		t.Fatalf("replay task admission: %v", err)
	}
	if created {
		t.Fatal("identical admission replay created another admission")
	}
	assertSameAdmission(t, replayed, first)
	assertAdmissionRows(t, kernelStore, []int{1, 1, 1, 3, 1, 1, 1})

	loaded, requestDigest, err := kernelStore.GetTaskAdmission(
		ctx,
		prepared.Namespace,
		prepared.IdempotencyKey,
	)
	if err != nil {
		t.Fatalf("get task admission: %v", err)
	}
	if requestDigest != prepared.RequestDigest {
		t.Fatalf("request digest = %q, want %q", requestDigest, prepared.RequestDigest)
	}
	assertSameAdmission(t, loaded, first)

	changed := prepared
	changed.RequestDigest = "sha256:" + strings.Repeat("f", 64)
	_, created, err = kernelStore.AdmitTask(ctx, changed)
	if created {
		t.Fatal("changed idempotency retry created authority")
	}
	assertKernelCode(t, err, model.ErrorIdempotencyConflict)
	assertAdmissionRows(t, kernelStore, []int{1, 1, 1, 3, 1, 1, 1})

	if err := kernelStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	afterRestart, created, err := reopened.AdmitTask(ctx, prepared)
	if err != nil {
		t.Fatalf("replay task admission after restart: %v", err)
	}
	if created {
		t.Fatal("restart replay created another admission")
	}
	assertSameAdmission(t, afterRestart, first)
	assertAdmissionRows(t, reopened, []int{1, 1, 1, 3, 1, 1, 1})
}

func TestBoundRuntimeRejectsMissingAndMismatchedAdmissionWithoutWrites(
	t *testing.T,
) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	boundRuntimeID := runtimeMetadataTestID("5")
	if err := kernelStore.BindRuntimeMetadata(
		ctx,
		boundRuntimeID,
		EnforcementProfileDevelopment,
	); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		runtimeID string
	}{
		{name: "missing"},
		{name: "different", runtimeID: runtimeMetadataTestID("6")},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared := prepareStoreAdmissionForRuntime(
				t,
				"runtime-"+test.name,
				test.runtimeID,
			)
			_, _, err := kernelStore.AdmitTask(ctx, prepared)
			assertKernelCode(
				t,
				err,
				model.ErrorCheckpointIncompatible,
			)
			assertAdmissionRows(
				t,
				kernelStore,
				[]int{0, 0, 0, 0, 0, 0, 0},
			)
		})
	}

	prepared := prepareStoreAdmissionForRuntime(
		t,
		"runtime-matching",
		boundRuntimeID,
	)
	if _, created, err := kernelStore.AdmitTask(
		ctx,
		prepared,
	); err != nil || !created {
		t.Fatalf("matching Runtime admission=(created=%t, err=%v)", created, err)
	}
}

func TestDevelopmentRuntimeBindingKeepsLegacyV0AdmissionReadable(
	t *testing.T,
) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	prepared := prepareStoreAdmission(t, "legacy-runtime-adoption")
	if prepared.Result.Version != admission.LegacyResultVersion ||
		prepared.Result.RuntimeID != "" {
		t.Fatalf("fixture is not a legacy v0 admission: %#v", prepared.Result)
	}
	if _, created, err := kernelStore.AdmitTask(
		ctx,
		prepared,
	); err != nil || !created {
		t.Fatalf("legacy admission=(created=%t, err=%v)", created, err)
	}
	if err := kernelStore.BindRuntimeMetadata(
		ctx,
		runtimeMetadataTestID("7"),
		EnforcementProfileDevelopment,
	); err != nil {
		t.Fatalf("development could not adopt legacy ledger: %v", err)
	}
	loaded, requestDigest, err := kernelStore.GetTaskAdmission(
		ctx,
		prepared.Namespace,
		prepared.IdempotencyKey,
	)
	if err != nil {
		t.Fatalf("adopted legacy admission is unreadable: %v", err)
	}
	if loaded.Version != admission.LegacyResultVersion ||
		loaded.RuntimeID != "" ||
		requestDigest != prepared.RequestDigest {
		t.Fatalf("legacy admission changed during adoption: %#v", loaded)
	}
	if _, err := kernelStore.db.ExecContext(
		ctx,
		`UPDATE kernel_metadata SET value = ?
		 WHERE key = ?`,
		EnforcementProfileProduction,
		enforcementProfileMetadataKey,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := kernelStore.GetTaskAdmission(
		ctx,
		prepared.Namespace,
		prepared.IdempotencyKey,
	); err == nil {
		t.Fatal("production-bound ledger replayed legacy v0 admission")
	} else {
		assertKernelCode(t, err, model.ErrorCheckpointIncompatible)
	}
}

func TestTaskAdmissionFaultsRollBackEveryPersistenceStep(t *testing.T) {
	t.Parallel()
	for _, point := range admissionFaultPoints {
		point := point
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			kernelStore := openTestStore(t)
			prepared := prepareStoreAdmission(t, strings.ReplaceAll(point, "_", "-"))
			injected := errors.New("injected admission failure")
			kernelStore.admissionFault = func(candidate string) error {
				if candidate == point {
					return injected
				}
				return nil
			}

			_, created, err := kernelStore.AdmitTask(ctx, prepared)
			if created {
				t.Fatal("faulted admission was reported as created")
			}
			if !errors.Is(err, injected) {
				t.Fatalf("admission error = %v, want injected fault", err)
			}
			assertKernelCode(t, err, model.ErrorInternal)
			assertAdmissionRows(t, kernelStore, []int{0, 0, 0, 0, 0, 0, 0})
			if _, err := kernelStore.GetRun(
				ctx,
				prepared.Namespace,
				prepared.Result.Run.Run.ID,
			); err == nil {
				t.Fatal("fault left an orphan run")
			}
			if _, err := kernelStore.GetTransaction(
				ctx,
				prepared.Namespace,
				prepared.Result.Transaction.Transaction.ID,
			); err == nil {
				t.Fatal("fault left an orphan transaction")
			}
			if _, _, err := kernelStore.GetTaskAdmission(
				ctx,
				prepared.Namespace,
				prepared.IdempotencyKey,
			); err == nil {
				t.Fatal("fault left an orphan admission")
			}

			kernelStore.admissionFault = nil
			if _, created, err := kernelStore.AdmitTask(ctx, prepared); err != nil {
				t.Fatalf("admit after rollback: %v", err)
			} else if !created {
				t.Fatal("admission after rollback was treated as a replay")
			}
			assertAdmissionRows(t, kernelStore, []int{1, 1, 1, 3, 1, 1, 1})
		})
	}
}

func TestTaskAdmissionReplaySurvivesLifecycleAdvancement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	prepared := prepareStoreAdmission(t, "advanced")
	initial, created, err := kernelStore.AdmitTask(ctx, prepared)
	if err != nil || !created {
		t.Fatalf("admit task: created=%v err=%v", created, err)
	}
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
	if _, err := kernelStore.AppendEvent(
		ctx,
		prepared.Namespace,
		initial.Run.Run.EventSequence,
		runEvent,
	); err != nil {
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
	if _, err := kernelStore.AppendTransactionEvents(
		ctx,
		prepared.Namespace,
		initial.Transaction.Transaction.EventSequence,
		[]model.TransactionEvent{transactionEvent},
	); err != nil {
		t.Fatalf("advance admitted transaction: %v", err)
	}

	loaded, _, err := kernelStore.GetTaskAdmission(
		ctx,
		prepared.Namespace,
		prepared.IdempotencyKey,
	)
	if err != nil {
		t.Fatalf("get admission after lifecycle advancement: %v", err)
	}
	assertSameAdmission(t, loaded, initial)
	replayed, created, err := kernelStore.AdmitTask(ctx, prepared)
	if err != nil {
		t.Fatalf("idempotent retry after lifecycle advancement: %v", err)
	}
	if created {
		t.Fatal("advanced admission retry created new authority")
	}
	assertSameAdmission(t, replayed, initial)
	assertAdmissionRows(t, kernelStore, []int{1, 1, 1, 4, 1, 2, 1})
}

func TestGetTaskAdmissionRejectsUnknownKey(t *testing.T) {
	t.Parallel()
	kernelStore := openTestStore(t)
	_, _, err := kernelStore.GetTaskAdmission(
		context.Background(),
		"store-test",
		"admission:missing",
	)
	assertKernelCode(t, err, model.ErrorNotFound)
}

func TestLegacyTransactionCreationCannotForgeAdmissionBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	prepared := prepareStoreAdmission(t, "forged-binding")
	_, err := kernelStore.CreateTransaction(
		ctx,
		prepared.TransactionEvent,
	)
	assertKernelCode(t, err, model.ErrorCapabilityDenied)
	if _, err := kernelStore.GetTransaction(
		ctx,
		prepared.Namespace,
		prepared.Result.Transaction.Transaction.ID,
	); !hasStoreCode(err, model.ErrorNotFound) {
		t.Fatalf("forged admission binding reached the transaction ledger: %v", err)
	}
}

func TestSQLiteV3MigrationPreservesLegacyRunWithoutInventingAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kernel.db")
	kernelStore, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := kernelStore.CreateRun(ctx, readCreationEvent(t))
	if err != nil {
		t.Fatalf("create legacy run: %v", err)
	}
	if err := kernelStore.Close(); err != nil {
		t.Fatal(err)
	}

	legacyDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"PRAGMA foreign_keys = OFF",
		"DROP TABLE task_admissions",
		"DROP TABLE agent_tasks",
		"DROP TABLE execution_contracts",
		"UPDATE kernel_metadata SET value = '2' WHERE key = 'schema_version'",
	} {
		if _, err := legacyDB.ExecContext(ctx, statement); err != nil {
			_ = legacyDB.Close()
			t.Fatalf("prepare v2 fixture with %q: %v", statement, err)
		}
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("migrate v2 store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	var version string
	if err := reopened.db.QueryRowContext(
		ctx,
		`SELECT value FROM kernel_metadata WHERE key = 'schema_version'`,
	).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "3" {
		t.Fatalf("schema version = %q, want 3", version)
	}
	restored, err := reopened.GetRun(ctx, legacy.Run.Namespace, legacy.Run.ID)
	if err != nil {
		t.Fatalf("get legacy run after migration: %v", err)
	}
	if restored.LastEventDigest != legacy.LastEventDigest {
		t.Fatal("v3 migration changed legacy run history")
	}
	if _, _, err := reopened.GetTaskAdmission(
		ctx,
		legacy.Run.Namespace,
		"admission:legacy",
	); err == nil {
		t.Fatal("v3 migration invented an admission for a legacy run")
	} else {
		assertKernelCode(t, err, model.ErrorNotFound)
	}
	for _, table := range []string{
		"execution_contracts", "agent_tasks", "task_admissions",
	} {
		var name string
		if err := reopened.db.QueryRowContext(
			ctx,
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`,
			table,
		).Scan(&name); err != nil {
			t.Fatalf("v3 table %s was not created: %v", table, err)
		}
	}
}

func TestGetTaskAdmissionDetectsAuthoritativeProjectionMismatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	prepared := prepareStoreAdmission(t, "projection-mismatch")
	if _, _, err := kernelStore.AdmitTask(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := kernelStore.db.ExecContext(
		ctx,
		`UPDATE runs
		 SET last_event_digest = ?
		 WHERE namespace = ? AND run_id = ?`,
		"sha256:"+strings.Repeat("f", 64),
		prepared.Namespace,
		prepared.Result.Run.Run.ID,
	); err != nil {
		t.Fatal(err)
	}
	_, _, err := kernelStore.GetTaskAdmission(
		ctx,
		prepared.Namespace,
		prepared.IdempotencyKey,
	)
	assertKernelCode(t, err, model.ErrorEventChain)
}

func prepareStoreAdmission(t *testing.T, suffix string) admission.Prepared {
	t.Helper()
	return prepareStoreAdmissionForRuntime(t, suffix, "")
}

func prepareStoreAdmissionForRuntime(
	t *testing.T,
	suffix string,
	runtimeID string,
) admission.Prepared {
	t.Helper()
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	maxTools := int64(8)
	deadline := now.Add(2 * time.Hour)
	version := admission.RequestVersion
	if runtimeID == "" {
		version = admission.LegacyRequestVersion
	}
	enforcementProfile := ""
	if runtimeID != "" {
		enforcementProfile = EnforcementProfileDevelopment
	}
	prepared, err := admission.Prepare(
		"store-test",
		admission.Request{
			Version:                    version,
			ExpectedRuntimeID:          runtimeID,
			ExpectedEnforcementProfile: enforcementProfile,
			IdempotencyKey:             "admission:" + suffix,
			TransactionID:              "tx:" + suffix,
			RunID:                      "run:" + suffix,
			Intent:                     "Make the exact bounded fixture change.",
			AgentProfile: model.AgentTaskProfileBinding{
				ID:            "agent-profile:store-test",
				Digest:        "sha256:" + strings.Repeat("a", 64),
				RuntimeClass:  "oci",
				ImageDigest:   "sha256:" + strings.Repeat("b", 64),
				CommandDigest: "sha256:" + strings.Repeat("c", 64),
			},
			Sponsor: model.Principal{
				ID: "human:store-sponsor", Kind: model.PrincipalHuman,
			},
			Actor: model.Principal{
				ID: "operator:store-test", Kind: model.PrincipalOperator,
			},
			Contract: admission.ContractSpec{
				Risk: "low",
				Resources: []model.ContractResource{
					{
						ID: "workspace",
						Selector: model.ResourceSelector{
							Kind: "filesystem", Pattern: "workspace/**",
						},
						Operations: []string{"filesystem.read", "filesystem.write"},
						Conditions: model.CapabilityConditions{
							WorkspaceRoot: "workspace",
						},
					},
				},
				Budgets: model.BudgetLimits{
					MaxToolCalls: &maxTools,
				},
				Deadline: &deadline,
			},
		},
		now,
	)
	if err != nil {
		t.Fatalf("prepare admission: %v", err)
	}
	return prepared
}

func assertAdmissionRows(t *testing.T, kernelStore *SQLiteStore, want []int) {
	t.Helper()
	tables := []string{
		"execution_contracts",
		"agent_tasks",
		"runs",
		"events",
		"agent_transactions",
		"transaction_events",
		"task_admissions",
	}
	if len(want) != len(tables) {
		t.Fatalf("invalid row-count fixture: %d values for %d tables", len(want), len(tables))
	}
	for index, table := range tables {
		var count int
		query := "SELECT COUNT(*) FROM " + table + " WHERE namespace = ?"
		if err := kernelStore.db.QueryRow(query, "store-test").Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != want[index] {
			t.Fatalf("%s rows = %d, want %d", table, count, want[index])
		}
	}
}

func assertSameAdmission(t *testing.T, got, want admission.Result) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("admission changed:\ngot  %s\nwant %s", gotJSON, wantJSON)
	}
}
