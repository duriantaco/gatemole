package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

const getExecutionAuthorityOperation = "get_execution_authority"

// GetExecutionAuthority returns one consistent, fully verified view of the
// authority admitted for a transaction and its current lifecycle projections.
// Legacy transactions are intentionally not upgraded into execution authority.
func (s *SQLiteStore) GetExecutionAuthority(
	ctx context.Context,
	namespace, transactionID string,
) (ExecutionAuthoritySnapshot, error) {
	if err := validateNamespace(namespace); err != nil {
		return ExecutionAuthoritySnapshot{}, err
	}
	if !identifierPattern.MatchString(transactionID) {
		return ExecutionAuthoritySnapshot{}, storeError(
			model.ErrorSchemaInvalid,
			getExecutionAuthorityOperation,
			transactionID,
			"invalid transaction ID",
			nil,
		)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
		ReadOnly:  true,
	})
	if err != nil {
		return ExecutionAuthoritySnapshot{}, storeError(
			model.ErrorInternal,
			getExecutionAuthorityOperation,
			transactionID,
			"begin authority snapshot",
			err,
		)
	}
	defer tx.Rollback()

	var idempotencyKey string
	err = tx.QueryRowContext(
		ctx,
		`SELECT idempotency_key
		 FROM task_admissions
		 WHERE namespace = ? AND transaction_id = ?`,
		namespace,
		transactionID,
	).Scan(&idempotencyKey)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ExecutionAuthoritySnapshot{}, storeError(
			model.ErrorNotFound,
			getExecutionAuthorityOperation,
			transactionID,
			"transaction has no task admission in namespace",
			err,
		)
	case err != nil:
		return ExecutionAuthoritySnapshot{}, storeError(
			model.ErrorInternal,
			getExecutionAuthorityOperation,
			transactionID,
			"query task admission",
			err,
		)
	}

	initial, _, err := loadTaskAdmission(ctx, tx, namespace, idempotencyKey)
	if err != nil {
		return ExecutionAuthoritySnapshot{}, authoritySnapshotError(
			transactionID,
			"load admitted authority",
			err,
		)
	}
	if initial.Transaction.Transaction.ID != transactionID {
		return ExecutionAuthoritySnapshot{}, storeError(
			model.ErrorEventChain,
			getExecutionAuthorityOperation,
			transactionID,
			"task admission transaction binding does not match its index",
			nil,
		)
	}

	currentRun, err := loadProjection(
		ctx,
		tx,
		namespace,
		initial.Run.Run.ID,
	)
	if err != nil {
		return ExecutionAuthoritySnapshot{}, err
	}
	currentTransaction, err := loadTransactionProjection(
		ctx,
		tx,
		namespace,
		transactionID,
	)
	if err != nil {
		return ExecutionAuthoritySnapshot{}, err
	}
	if err := validateCurrentAdmissionBindings(
		initial,
		currentRun.Run,
		currentTransaction,
	); err != nil {
		return ExecutionAuthoritySnapshot{}, authoritySnapshotError(
			transactionID,
			"current authority no longer matches its admission",
			err,
		)
	}
	if err := verifyLiveRunProjection(
		ctx,
		tx,
		namespace,
		currentRun,
	); err != nil {
		return ExecutionAuthoritySnapshot{}, err
	}
	if err := verifyLiveTransactionProjection(
		ctx,
		tx,
		namespace,
		currentTransaction,
	); err != nil {
		return ExecutionAuthoritySnapshot{}, err
	}

	if err := tx.Commit(); err != nil {
		return ExecutionAuthoritySnapshot{}, storeError(
			model.ErrorInternal,
			getExecutionAuthorityOperation,
			transactionID,
			"commit authority snapshot",
			err,
		)
	}
	return ExecutionAuthoritySnapshot{
		Task:        initial.Task,
		Contract:    initial.Contract,
		Grants:      append([]model.CapabilityGrant(nil), initial.Grants...),
		Run:         currentRun,
		Transaction: currentTransaction,
	}, nil
}

type authorityRowsQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func verifyLiveRunProjection(
	ctx context.Context,
	query authorityRowsQuerier,
	namespace string,
	current reducer.Projection,
) error {
	rows, err := query.QueryContext(
		ctx,
		`SELECT sequence, event_id, event_type, event_digest, event_json
		 FROM events
		 WHERE namespace = ? AND run_id = ?
		 ORDER BY sequence`,
		namespace,
		current.Run.ID,
	)
	if err != nil {
		return storeError(
			model.ErrorInternal,
			getExecutionAuthorityOperation,
			current.Run.ID,
			"query authoritative run events",
			err,
		)
	}
	defer rows.Close()

	events := make([]model.RunEvent, 0, current.Run.EventSequence)
	for rows.Next() {
		var sequence int64
		var eventID, eventType, eventDigest string
		var data []byte
		if err := rows.Scan(
			&sequence,
			&eventID,
			&eventType,
			&eventDigest,
			&data,
		); err != nil {
			return storeError(
				model.ErrorInternal,
				getExecutionAuthorityOperation,
				current.Run.ID,
				"scan authoritative run event",
				err,
			)
		}
		event, err := model.DecodeStrict[model.RunEvent](data)
		if err != nil {
			return storeError(
				model.ErrorEventChain,
				getExecutionAuthorityOperation,
				current.Run.ID,
				"stored run event is invalid",
				err,
			)
		}
		if event.RunID != current.Run.ID ||
			event.Sequence != sequence ||
			event.ID != eventID ||
			event.Type != eventType ||
			event.Digest != eventDigest {
			return storeError(
				model.ErrorEventChain,
				getExecutionAuthorityOperation,
				current.Run.ID,
				"stored run event columns do not match their envelope",
				nil,
			)
		}
		if err := verifyEventDigest(event); err != nil {
			return authoritySnapshotError(
				current.Run.ID,
				"stored run event digest is invalid",
				err,
			)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return storeError(
			model.ErrorInternal,
			getExecutionAuthorityOperation,
			current.Run.ID,
			"iterate authoritative run events",
			err,
		)
	}
	replayed, err := reducer.Replay(events)
	if err != nil {
		return storeError(
			model.ErrorEventChain,
			getExecutionAuthorityOperation,
			current.Run.ID,
			"authoritative run event replay failed",
			err,
		)
	}
	if !reflect.DeepEqual(replayed, current) {
		return storeError(
			model.ErrorEventChain,
			getExecutionAuthorityOperation,
			current.Run.ID,
			"current run projection does not match authoritative replay",
			nil,
		)
	}
	return nil
}

func verifyLiveTransactionProjection(
	ctx context.Context,
	query authorityRowsQuerier,
	namespace string,
	current transactionreducer.Projection,
) error {
	transactionID := current.Transaction.ID
	rows, err := query.QueryContext(
		ctx,
		`SELECT sequence, event_id, event_type, event_digest, event_json
		 FROM transaction_events
		 WHERE namespace = ? AND transaction_id = ?
		 ORDER BY sequence`,
		namespace,
		transactionID,
	)
	if err != nil {
		return storeError(
			model.ErrorInternal,
			getExecutionAuthorityOperation,
			transactionID,
			"query authoritative transaction events",
			err,
		)
	}
	defer rows.Close()

	events := make([]model.TransactionEvent, 0, current.Transaction.EventSequence)
	for rows.Next() {
		var sequence int64
		var eventID, eventType, eventDigest string
		var data []byte
		if err := rows.Scan(
			&sequence,
			&eventID,
			&eventType,
			&eventDigest,
			&data,
		); err != nil {
			return storeError(
				model.ErrorInternal,
				getExecutionAuthorityOperation,
				transactionID,
				"scan authoritative transaction event",
				err,
			)
		}
		event, err := model.DecodeStrict[model.TransactionEvent](data)
		if err != nil {
			return storeError(
				model.ErrorEventChain,
				getExecutionAuthorityOperation,
				transactionID,
				"stored transaction event is invalid",
				err,
			)
		}
		if event.TransactionID != transactionID ||
			event.Sequence != sequence ||
			event.ID != eventID ||
			event.Type != eventType ||
			event.Digest != eventDigest {
			return storeError(
				model.ErrorEventChain,
				getExecutionAuthorityOperation,
				transactionID,
				"stored transaction event columns do not match their envelope",
				nil,
			)
		}
		if err := verifyTransactionEventDigest(event); err != nil {
			return authoritySnapshotError(
				transactionID,
				"stored transaction event digest is invalid",
				err,
			)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return storeError(
			model.ErrorInternal,
			getExecutionAuthorityOperation,
			transactionID,
			"iterate authoritative transaction events",
			err,
		)
	}
	replayed, err := transactionreducer.Replay(events)
	if err != nil {
		return storeError(
			model.ErrorEventChain,
			getExecutionAuthorityOperation,
			transactionID,
			"authoritative transaction event replay failed",
			err,
		)
	}
	if !reflect.DeepEqual(replayed, current) {
		return storeError(
			model.ErrorEventChain,
			getExecutionAuthorityOperation,
			transactionID,
			"current transaction projection does not match authoritative replay",
			nil,
		)
	}
	return nil
}

func authoritySnapshotError(resource, message string, cause error) error {
	var kernelError *model.KernelError
	code := model.ErrorEventChain
	if errors.As(cause, &kernelError) &&
		kernelError.Code == model.ErrorInternal {
		code = kernelError.Code
	}
	return storeError(
		code,
		getExecutionAuthorityOperation,
		resource,
		message,
		cause,
	)
}
