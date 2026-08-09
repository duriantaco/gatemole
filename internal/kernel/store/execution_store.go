package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/eventlog"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/reducer"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
)

const (
	pairedExecutionOperation                   = "append_paired_execution_event"
	pairedExecutionFaultAfterRunUpdate         = "after_run_update"
	pairedExecutionFaultAfterRunEvent          = "after_run_event"
	pairedExecutionFaultAfterTransactionUpdate = "after_transaction_update"
	pairedExecutionFaultAfterTransactionEvent  = "after_transaction_event"
	pairedExecutionFaultBeforeCommit           = "before_commit"
)

var pairedExecutionFaultPoints = []string{
	pairedExecutionFaultAfterRunUpdate,
	pairedExecutionFaultAfterRunEvent,
	pairedExecutionFaultAfterTransactionUpdate,
	pairedExecutionFaultAfterTransactionEvent,
	pairedExecutionFaultBeforeCommit,
}

// AppendPairedExecutionEvent advances the admitted run and its transaction in
// one SQLite commit. The transaction event is caller-authored because it
// contains the supervised receipt; the matching run event and budget charge
// are derived inside this boundary. Charges saturate at remaining hard limits
// so an overrun cannot make the terminal receipt impossible to persist.
func (s *SQLiteStore) AppendPairedExecutionEvent(
	ctx context.Context,
	namespace string,
	heads ExecutionHeads,
	transactionEvent model.TransactionEvent,
) (ExecutionProjection, error) {
	if err := validateNamespace(namespace); err != nil {
		return ExecutionProjection{}, err
	}
	if heads.TransactionSequence < 1 || heads.RunSequence < 1 ||
		!digestPattern.MatchString(heads.TransactionDigest) ||
		!digestPattern.MatchString(heads.RunDigest) {
		return ExecutionProjection{}, storeError(
			model.ErrorSchemaInvalid,
			pairedExecutionOperation,
			transactionEvent.TransactionID,
			"both execution ledger heads must be valid",
			nil,
		)
	}
	if transactionEvent.Type != transactionreducer.EventAgentExecutionStarted &&
		transactionEvent.Type != transactionreducer.EventAgentExecutionFinished {
		return ExecutionProjection{}, storeError(
			model.ErrorSchemaInvalid,
			pairedExecutionOperation,
			transactionEvent.ID,
			"only agent execution start and finish events can be paired",
			nil,
		)
	}
	if err := verifyTransactionEventDigest(transactionEvent); err != nil {
		return ExecutionProjection{}, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return ExecutionProjection{}, storeError(
			model.ErrorInternal,
			pairedExecutionOperation,
			transactionEvent.TransactionID,
			"begin paired execution transaction",
			err,
		)
	}
	defer tx.Rollback()

	currentTransaction, err := loadTransactionProjection(
		ctx,
		tx,
		namespace,
		transactionEvent.TransactionID,
	)
	if err != nil {
		return ExecutionProjection{}, err
	}
	if currentTransaction.Transaction.EventSequence != heads.TransactionSequence ||
		currentTransaction.LastEventDigest != heads.TransactionDigest {
		return ExecutionProjection{}, conflict(
			pairedExecutionOperation,
			transactionEvent.TransactionID,
			"transaction changed after execution authority was loaded",
			nil,
		)
	}
	currentRun, err := loadAdmittedExecutionRun(
		ctx,
		tx,
		namespace,
		currentTransaction,
	)
	if err != nil {
		return ExecutionProjection{}, err
	}
	if currentRun.Run.EventSequence != heads.RunSequence ||
		currentRun.LastEventDigest != heads.RunDigest {
		return ExecutionProjection{}, conflict(
			pairedExecutionOperation,
			currentRun.Run.ID,
			"admitted run changed after execution authority was loaded",
			nil,
		)
	}

	nextTransaction, err := transactionreducer.Apply(
		&currentTransaction,
		transactionEvent,
	)
	if err != nil {
		return ExecutionProjection{}, err
	}
	if err := nextTransaction.Validate(); err != nil {
		return ExecutionProjection{}, err
	}
	execution, err := pairedExecutionReceipt(
		transactionEvent,
		nextTransaction,
		currentRun.Run.ID,
	)
	if err != nil {
		return ExecutionProjection{}, err
	}
	runEvents, nextRun, err := pairedRunEvents(
		currentRun,
		transactionEvent,
		execution,
	)
	if err != nil {
		return ExecutionProjection{}, err
	}
	for _, runEvent := range runEvents {
		if err := verifyEventDigest(runEvent); err != nil {
			return ExecutionProjection{}, err
		}
	}

	runJSON, err := json.Marshal(nextRun.Run)
	if err != nil {
		return ExecutionProjection{}, storeError(
			model.ErrorInternal,
			pairedExecutionOperation,
			currentRun.Run.ID,
			"encode paired run projection",
			err,
		)
	}
	runEventJSON := make([][]byte, len(runEvents))
	for index, runEvent := range runEvents {
		runEventJSON[index], err = json.Marshal(runEvent)
		if err != nil {
			return ExecutionProjection{}, storeError(
				model.ErrorInternal,
				pairedExecutionOperation,
				runEvent.ID,
				"encode paired run event",
				err,
			)
		}
	}
	transactionJSON, err := json.Marshal(nextTransaction)
	if err != nil {
		return ExecutionProjection{}, storeError(
			model.ErrorInternal,
			pairedExecutionOperation,
			currentTransaction.Transaction.ID,
			"encode paired transaction projection",
			err,
		)
	}
	transactionEventJSON, err := json.Marshal(transactionEvent)
	if err != nil {
		return ExecutionProjection{}, storeError(
			model.ErrorInternal,
			pairedExecutionOperation,
			transactionEvent.ID,
			"encode paired transaction event",
			err,
		)
	}

	if err := updatePairedRun(
		ctx,
		tx,
		namespace,
		currentRun,
		nextRun,
		runJSON,
	); err != nil {
		return ExecutionProjection{}, err
	}
	if err := s.runPairedExecutionFault(
		pairedExecutionFaultAfterRunUpdate,
		execution.ID,
	); err != nil {
		return ExecutionProjection{}, err
	}
	for index, runEvent := range runEvents {
		if err := insertEvent(
			ctx,
			tx,
			namespace,
			runEvent,
			runEventJSON[index],
		); err != nil {
			return ExecutionProjection{}, err
		}
		if err := s.runPairedExecutionFault(
			pairedExecutionFaultAfterRunEvent,
			execution.ID,
		); err != nil {
			return ExecutionProjection{}, err
		}
	}
	if err := updatePairedTransaction(
		ctx,
		tx,
		namespace,
		currentTransaction,
		nextTransaction,
		transactionJSON,
	); err != nil {
		return ExecutionProjection{}, err
	}
	if err := s.runPairedExecutionFault(
		pairedExecutionFaultAfterTransactionUpdate,
		execution.ID,
	); err != nil {
		return ExecutionProjection{}, err
	}
	if err := insertTransactionEvent(
		ctx,
		tx,
		namespace,
		transactionEvent,
		transactionEventJSON,
	); err != nil {
		return ExecutionProjection{}, err
	}
	if err := s.runPairedExecutionFault(
		pairedExecutionFaultAfterTransactionEvent,
		execution.ID,
	); err != nil {
		return ExecutionProjection{}, err
	}
	if err := s.runPairedExecutionFault(
		pairedExecutionFaultBeforeCommit,
		execution.ID,
	); err != nil {
		return ExecutionProjection{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExecutionProjection{}, classifyWriteError(
			pairedExecutionOperation,
			execution.ID,
			err,
		)
	}
	return ExecutionProjection{Run: nextRun, Transaction: nextTransaction}, nil
}

func loadAdmittedExecutionRun(
	ctx context.Context,
	tx *sql.Tx,
	namespace string,
	transaction transactionreducer.Projection,
) (reducer.Projection, error) {
	binding := transaction.Transaction.Admission
	task := transaction.Transaction.Task
	if binding == nil || task == nil {
		return reducer.Projection{}, storeError(
			model.ErrorCapabilityDenied,
			pairedExecutionOperation,
			transaction.Transaction.ID,
			"paired execution requires atomic task admission",
			nil,
		)
	}
	var runID, taskDigest, contractDigest string
	err := tx.QueryRowContext(
		ctx,
		`SELECT run_id, task_digest, contract_digest
		 FROM task_admissions
		 WHERE namespace = ? AND transaction_id = ?`,
		namespace,
		transaction.Transaction.ID,
	).Scan(&runID, &taskDigest, &contractDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return reducer.Projection{}, storeError(
			model.ErrorNotFound,
			pairedExecutionOperation,
			transaction.Transaction.ID,
			"transaction has no admitted run in namespace",
			err,
		)
	}
	if err != nil {
		return reducer.Projection{}, storeError(
			model.ErrorInternal,
			pairedExecutionOperation,
			transaction.Transaction.ID,
			"query admitted run binding",
			err,
		)
	}
	if runID != binding.RunID || runID != task.RunID ||
		taskDigest != binding.TaskDigest || taskDigest != task.Digest ||
		contractDigest != binding.ContractDigest {
		return reducer.Projection{}, storeError(
			model.ErrorEventChain,
			pairedExecutionOperation,
			transaction.Transaction.ID,
			"admission and transaction execution bindings disagree",
			nil,
		)
	}
	return loadProjection(ctx, tx, namespace, runID)
}

func pairedExecutionReceipt(
	event model.TransactionEvent,
	projection transactionreducer.Projection,
	runID string,
) (model.AgentExecution, error) {
	var execution model.AgentExecution
	switch event.Type {
	case transactionreducer.EventAgentExecutionStarted:
		if len(projection.Executions) == 0 {
			return model.AgentExecution{}, storeError(
				model.ErrorEventChain,
				pairedExecutionOperation,
				event.ID,
				"execution start produced no receipt",
				nil,
			)
		}
		execution = projection.Executions[len(projection.Executions)-1]
	case transactionreducer.EventAgentExecutionFinished:
		payload, err := model.DecodeStrict[transactionreducer.AgentExecutionFinishedPayload](event.Payload)
		if err != nil {
			return model.AgentExecution{}, err
		}
		for index := range projection.Executions {
			if projection.Executions[index].ID == payload.ExecutionID {
				execution = projection.Executions[index]
				break
			}
		}
	}
	if execution.ID == "" || execution.RunID != runID || execution.Attempt < 1 {
		return model.AgentExecution{}, storeError(
			model.ErrorIdentityInvalid,
			pairedExecutionOperation,
			event.ID,
			"transaction execution does not match the admitted run",
			nil,
		)
	}
	return execution, nil
}

func pairedRunEvents(
	projection reducer.Projection,
	transactionEvent model.TransactionEvent,
	execution model.AgentExecution,
) ([]model.RunEvent, reducer.Projection, error) {
	events := []model.RunEvent{}
	current := projection
	appendEvent := func(eventType string, occurredAt time.Time, payload any) error {
		event, err := eventlog.Next(
			current,
			eventType,
			transactionEvent.Actor,
			occurredAt,
			payload,
		)
		if err != nil {
			return err
		}
		next, err := reducer.Apply(&current, event)
		if err != nil {
			return err
		}
		events = append(events, event)
		current = next
		return nil
	}
	startPayload := model.RunExecutionStartedPayload{
		TransactionID: execution.TransactionID,
		ExecutionID:   execution.ID,
		Attempt:       execution.Attempt,
	}
	switch transactionEvent.Type {
	case transactionreducer.EventAgentExecutionStarted:
		if err := appendEvent(
			reducer.EventRunExecutionStarted,
			transactionEvent.OccurredAt,
			startPayload,
		); err != nil {
			return nil, reducer.Projection{}, err
		}
	case transactionreducer.EventAgentExecutionFinished:
		// Before paired lifecycle existed, a crash could leave an admitted run
		// beside an already-running transaction execution. Reconstruct that
		// missing run-side start in the same commit as recovery settlement.
		if current.Run.ActiveExecutionID == "" && current.Run.State == model.RunAdmitted {
			if err := appendEvent(
				reducer.EventRunExecutionStarted,
				execution.StartedAt,
				startPayload,
			); err != nil {
				return nil, reducer.Projection{}, err
			}
		}
		if err := appendEvent(
			reducer.EventRunExecutionFinished,
			transactionEvent.OccurredAt,
			model.RunExecutionFinishedPayload{
				TransactionID: execution.TransactionID,
				ExecutionID:   execution.ID,
				Attempt:       execution.Attempt,
				Status:        execution.Status,
				Usage:         executionBudgetUsage(execution, current.Run),
			},
		); err != nil {
			return nil, reducer.Projection{}, err
		}
	default:
		return nil, reducer.Projection{}, fmt.Errorf("unsupported paired event %q", transactionEvent.Type)
	}
	return events, current, nil
}

func executionBudgetUsage(
	execution model.AgentExecution,
	run model.AgentRun,
) model.BudgetUsage {
	usage := model.BudgetUsage{}
	if execution.CompletedAt != nil {
		duration := execution.CompletedAt.Sub(execution.StartedAt)
		usage.WallTimeSeconds = int64(duration / time.Second)
		if duration%time.Second != 0 {
			usage.WallTimeSeconds++
		}
	}
	if execution.ModelBroker != nil {
		usage.ModelCalls = execution.ModelBroker.Calls
		usage.InputTokens = execution.ModelBroker.InputTokens
		usage.OutputTokens = execution.ModelBroker.OutputTokens
	}
	usage.InputTokens = boundedBudgetCharge(
		usage.InputTokens, run.BudgetUsage.InputTokens, run.BudgetLimits.MaxInputTokens,
	)
	usage.OutputTokens = boundedBudgetCharge(
		usage.OutputTokens, run.BudgetUsage.OutputTokens, run.BudgetLimits.MaxOutputTokens,
	)
	usage.ModelCalls = boundedBudgetCharge(
		usage.ModelCalls, run.BudgetUsage.ModelCalls, run.BudgetLimits.MaxModelCalls,
	)
	usage.ToolCalls = boundedBudgetCharge(
		usage.ToolCalls, run.BudgetUsage.ToolCalls, run.BudgetLimits.MaxToolCalls,
	)
	usage.CostMicros = boundedBudgetCharge(
		usage.CostMicros, run.BudgetUsage.CostMicros, run.BudgetLimits.MaxCostMicros,
	)
	usage.WallTimeSeconds = boundedBudgetCharge(
		usage.WallTimeSeconds,
		run.BudgetUsage.WallTimeSeconds,
		run.BudgetLimits.MaxWallTimeSeconds,
	)
	return usage
}

func boundedBudgetCharge(actual, current int64, limit *int64) int64 {
	if actual <= 0 {
		return 0
	}
	remaining := int64(math.MaxInt64)
	if current >= 0 {
		remaining -= current
	} else {
		return 0
	}
	if limit != nil {
		limitedRemaining := *limit - current
		if limitedRemaining < remaining {
			remaining = limitedRemaining
		}
	}
	if remaining <= 0 {
		return 0
	}
	if actual > remaining {
		return remaining
	}
	return actual
}

func updatePairedRun(
	ctx context.Context,
	tx *sql.Tx,
	namespace string,
	current, next reducer.Projection,
	projectionJSON []byte,
) error {
	result, err := tx.ExecContext(
		ctx,
		`UPDATE runs
		 SET event_sequence = ?, last_event_digest = ?, state_json = ?, updated_at = ?
		 WHERE namespace = ? AND run_id = ?
		   AND event_sequence = ? AND last_event_digest = ?`,
		next.Run.EventSequence,
		next.LastEventDigest,
		projectionJSON,
		next.Run.UpdatedAt.Format(timeFormat),
		namespace,
		current.Run.ID,
		current.Run.EventSequence,
		current.LastEventDigest,
	)
	if err != nil {
		return classifyWriteError(pairedExecutionOperation, current.Run.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return storeError(model.ErrorInternal, pairedExecutionOperation, current.Run.ID, "read run update result", err)
	}
	if rows != 1 {
		return conflict(pairedExecutionOperation, current.Run.ID, "run changed during paired append", nil)
	}
	return nil
}

func updatePairedTransaction(
	ctx context.Context,
	tx *sql.Tx,
	namespace string,
	current, next transactionreducer.Projection,
	projectionJSON []byte,
) error {
	result, err := tx.ExecContext(
		ctx,
		`UPDATE agent_transactions
		 SET event_sequence = ?, last_event_digest = ?, projection_json = ?, updated_at = ?
		 WHERE namespace = ? AND transaction_id = ?
		   AND event_sequence = ? AND last_event_digest = ?`,
		next.Transaction.EventSequence,
		next.LastEventDigest,
		projectionJSON,
		next.Transaction.UpdatedAt.Format(timeFormat),
		namespace,
		current.Transaction.ID,
		current.Transaction.EventSequence,
		current.LastEventDigest,
	)
	if err != nil {
		return classifyWriteError(pairedExecutionOperation, current.Transaction.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return storeError(model.ErrorInternal, pairedExecutionOperation, current.Transaction.ID, "read transaction update result", err)
	}
	if rows != 1 {
		return conflict(pairedExecutionOperation, current.Transaction.ID, "transaction changed during paired append", nil)
	}
	return nil
}

func (s *SQLiteStore) runPairedExecutionFault(point, executionID string) error {
	if s.pairedExecutionFault == nil {
		return nil
	}
	if err := s.pairedExecutionFault(point); err != nil {
		return storeError(
			model.ErrorInternal,
			pairedExecutionOperation,
			executionID,
			fmt.Sprintf("paired execution persistence fault at %s", point),
			err,
		)
	}
	return nil
}
