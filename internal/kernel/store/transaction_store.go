package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/duriantaco/vouch/internal/kernel/model"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

func (s *SQLiteStore) CreateTransaction(ctx context.Context, event model.TransactionEvent) (transactionreducer.Projection, error) {
	if err := verifyTransactionEventDigest(event); err != nil {
		return transactionreducer.Projection{}, err
	}
	projection, err := transactionreducer.Apply(nil, event)
	if err != nil {
		return transactionreducer.Projection{}, err
	}
	if err := projection.Validate(); err != nil {
		return transactionreducer.Projection{}, err
	}
	if projection.Transaction.Admission != nil {
		return transactionreducer.Projection{}, storeError(
			model.ErrorCapabilityDenied,
			"create_transaction",
			event.TransactionID,
			"admission bindings can only be created by atomic task admission",
			nil,
		)
	}
	projectionJSON, err := json.Marshal(projection)
	if err != nil {
		return transactionreducer.Projection{}, storeError(model.ErrorInternal, "create_transaction", event.TransactionID, "encode projection", err)
	}
	eventJSON, err := json.Marshal(event)
	if err != nil {
		return transactionreducer.Projection{}, storeError(model.ErrorInternal, "create_transaction", event.TransactionID, "encode event", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return transactionreducer.Projection{}, storeError(model.ErrorInternal, "create_transaction", event.TransactionID, "begin transaction", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO agent_transactions(
			namespace, transaction_id, event_sequence, last_event_digest,
			projection_json, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		projection.Transaction.Namespace,
		projection.Transaction.ID,
		projection.Transaction.EventSequence,
		projection.LastEventDigest,
		projectionJSON,
		projection.Transaction.CreatedAt.Format(timeFormat),
		projection.Transaction.UpdatedAt.Format(timeFormat),
	)
	if err != nil {
		return transactionreducer.Projection{}, classifyWriteError("create_transaction", event.TransactionID, err)
	}
	if err := insertTransactionEvent(ctx, tx, projection.Transaction.Namespace, event, eventJSON); err != nil {
		return transactionreducer.Projection{}, err
	}
	if err := tx.Commit(); err != nil {
		return transactionreducer.Projection{}, classifyWriteError("create_transaction", event.TransactionID, err)
	}
	return projection, nil
}

func (s *SQLiteStore) AppendTransactionEvents(
	ctx context.Context,
	namespace string,
	expectedSequence int64,
	events []model.TransactionEvent,
) (transactionreducer.Projection, error) {
	if err := validateNamespace(namespace); err != nil {
		return transactionreducer.Projection{}, err
	}
	if len(events) == 0 {
		return transactionreducer.Projection{}, storeError(model.ErrorSchemaInvalid, "append_transaction_events", "", "at least one event is required", nil)
	}
	for _, event := range events {
		if err := verifyTransactionEventDigest(event); err != nil {
			return transactionreducer.Projection{}, err
		}
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return transactionreducer.Projection{}, storeError(model.ErrorInternal, "append_transaction_events", events[0].TransactionID, "begin transaction", err)
	}
	defer tx.Rollback()
	current, err := loadTransactionProjection(ctx, tx, namespace, events[0].TransactionID)
	if err != nil {
		return transactionreducer.Projection{}, err
	}
	if current.Transaction.EventSequence != expectedSequence {
		return transactionreducer.Projection{}, conflict(
			"append_transaction_events",
			events[0].TransactionID,
			fmt.Sprintf("expected transaction sequence %d, current sequence is %d", expectedSequence, current.Transaction.EventSequence),
			nil,
		)
	}
	next := current
	eventJSON := make([][]byte, len(events))
	for i, event := range events {
		if event.TransactionID != current.Transaction.ID {
			return transactionreducer.Projection{}, storeError(model.ErrorEventSequence, "append_transaction_events", event.ID, "batch contains another transaction", nil)
		}
		next, err = transactionreducer.Apply(&next, event)
		if err != nil {
			return transactionreducer.Projection{}, err
		}
		eventJSON[i], err = json.Marshal(event)
		if err != nil {
			return transactionreducer.Projection{}, storeError(model.ErrorInternal, "append_transaction_events", event.ID, "encode event", err)
		}
	}
	if err := next.Validate(); err != nil {
		return transactionreducer.Projection{}, err
	}
	projectionJSON, err := json.Marshal(next)
	if err != nil {
		return transactionreducer.Projection{}, storeError(model.ErrorInternal, "append_transaction_events", current.Transaction.ID, "encode projection", err)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE agent_transactions
		 SET event_sequence = ?, last_event_digest = ?, projection_json = ?, updated_at = ?
		 WHERE namespace = ? AND transaction_id = ? AND event_sequence = ?`,
		next.Transaction.EventSequence,
		next.LastEventDigest,
		projectionJSON,
		next.Transaction.UpdatedAt.Format(timeFormat),
		namespace,
		current.Transaction.ID,
		expectedSequence,
	)
	if err != nil {
		return transactionreducer.Projection{}, classifyWriteError("append_transaction_events", current.Transaction.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return transactionreducer.Projection{}, storeError(model.ErrorInternal, "append_transaction_events", current.Transaction.ID, "read update result", err)
	}
	if rows != 1 {
		return transactionreducer.Projection{}, conflict("append_transaction_events", current.Transaction.ID, "transaction changed during append", nil)
	}
	for i, event := range events {
		if err := insertTransactionEvent(ctx, tx, namespace, event, eventJSON[i]); err != nil {
			return transactionreducer.Projection{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return transactionreducer.Projection{}, classifyWriteError("append_transaction_events", current.Transaction.ID, err)
	}
	return next, nil
}

func (s *SQLiteStore) GetTransaction(ctx context.Context, namespace, transactionID string) (transactionreducer.Projection, error) {
	if err := validateNamespace(namespace); err != nil {
		return transactionreducer.Projection{}, err
	}
	if strings.TrimSpace(transactionID) == "" {
		return transactionreducer.Projection{}, storeError(model.ErrorSchemaInvalid, "get_transaction", transactionID, "transaction ID is required", nil)
	}
	return loadTransactionProjection(ctx, s.db, namespace, transactionID)
}

func (s *SQLiteStore) ListTransactions(ctx context.Context, namespace string) ([]model.AgentTransaction, error) {
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT projection_json FROM agent_transactions WHERE namespace = ? ORDER BY created_at, transaction_id`,
		namespace,
	)
	if err != nil {
		return nil, storeError(model.ErrorInternal, "list_transactions", namespace, "query transactions", err)
	}
	defer rows.Close()
	transactions := []model.AgentTransaction{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, storeError(model.ErrorInternal, "list_transactions", namespace, "scan transaction", err)
		}
		projection, err := model.DecodeStrict[transactionreducer.Projection](data)
		if err != nil {
			return nil, storeError(model.ErrorEventChain, "list_transactions", namespace, "stored transaction projection is invalid", err)
		}
		if err := projection.Validate(); err != nil {
			return nil, storeError(model.ErrorEventChain, "list_transactions", projection.Transaction.ID, "stored transaction projection failed validation", err)
		}
		transactions = append(transactions, projection.Transaction)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError(model.ErrorInternal, "list_transactions", namespace, "iterate transactions", err)
	}
	return transactions, nil
}

func (s *SQLiteStore) ListAllTransactions(ctx context.Context) ([]transactionreducer.Projection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT namespace, transaction_id, projection_json, last_event_digest
		 FROM agent_transactions ORDER BY namespace, created_at, transaction_id`,
	)
	if err != nil {
		return nil, storeError(model.ErrorInternal, "list_all_transactions", "", "query transactions", err)
	}
	defer rows.Close()
	projections := []transactionreducer.Projection{}
	for rows.Next() {
		var namespace, transactionID, lastDigest string
		var data []byte
		if err := rows.Scan(&namespace, &transactionID, &data, &lastDigest); err != nil {
			return nil, storeError(model.ErrorInternal, "list_all_transactions", "", "scan transaction", err)
		}
		projection, err := model.DecodeStrict[transactionreducer.Projection](data)
		if err != nil {
			return nil, storeError(model.ErrorEventChain, "list_all_transactions", transactionID, "stored transaction projection is invalid", err)
		}
		if err := projection.Validate(); err != nil {
			return nil, storeError(model.ErrorEventChain, "list_all_transactions", transactionID, "stored transaction projection failed validation", err)
		}
		if projection.Transaction.Namespace != namespace ||
			projection.Transaction.ID != transactionID ||
			projection.LastEventDigest != lastDigest ||
			!digestPattern.MatchString(lastDigest) {
			return nil, storeError(model.ErrorEventChain, "list_all_transactions", transactionID, "stored transaction projection identity or digest is invalid", nil)
		}
		projections = append(projections, projection)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError(model.ErrorInternal, "list_all_transactions", "", "iterate transactions", err)
	}
	return projections, nil
}

func (s *SQLiteStore) TransactionEvents(
	ctx context.Context,
	namespace, transactionID string,
	afterSequence int64,
) ([]model.TransactionEvent, error) {
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	if strings.TrimSpace(transactionID) == "" || afterSequence < 0 {
		return nil, storeError(model.ErrorSchemaInvalid, "list_transaction_events", transactionID, "transaction ID is required and after must be non-negative", nil)
	}
	if _, err := s.GetTransaction(ctx, namespace, transactionID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_json FROM transaction_events
		 WHERE namespace = ? AND transaction_id = ? AND sequence > ?
		 ORDER BY sequence`,
		namespace,
		transactionID,
		afterSequence,
	)
	if err != nil {
		return nil, storeError(model.ErrorInternal, "list_transaction_events", transactionID, "query events", err)
	}
	defer rows.Close()
	events := []model.TransactionEvent{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, storeError(model.ErrorInternal, "list_transaction_events", transactionID, "scan event", err)
		}
		event, err := model.DecodeStrict[model.TransactionEvent](data)
		if err != nil {
			return nil, storeError(model.ErrorEventChain, "list_transaction_events", transactionID, "stored event is invalid", err)
		}
		if err := event.Validate(); err != nil {
			return nil, storeError(model.ErrorEventChain, "list_transaction_events", event.ID, "stored event failed validation", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError(model.ErrorInternal, "list_transaction_events", transactionID, "iterate events", err)
	}
	return events, nil
}

func (s *SQLiteStore) VerifyTransaction(ctx context.Context, namespace, transactionID string) error {
	events, err := s.TransactionEvents(ctx, namespace, transactionID, 0)
	if err != nil {
		return err
	}
	replayed, err := transactionreducer.Replay(events)
	if err != nil {
		return storeError(model.ErrorEventChain, "verify_transaction", transactionID, "event replay failed", err)
	}
	stored, err := s.GetTransaction(ctx, namespace, transactionID)
	if err != nil {
		return err
	}
	replayedJSON, err := json.Marshal(replayed)
	if err != nil {
		return storeError(model.ErrorInternal, "verify_transaction", transactionID, "encode replayed projection", err)
	}
	storedJSON, err := json.Marshal(stored)
	if err != nil {
		return storeError(model.ErrorInternal, "verify_transaction", transactionID, "encode stored projection", err)
	}
	if !bytes.Equal(replayedJSON, storedJSON) {
		return storeError(model.ErrorEventChain, "verify_transaction", transactionID, "materialized transaction does not match event replay", nil)
	}
	return nil
}

func loadTransactionProjection(
	ctx context.Context,
	query rowQuerier,
	namespace, transactionID string,
) (transactionreducer.Projection, error) {
	var data []byte
	var lastDigest string
	err := query.QueryRowContext(ctx,
		`SELECT projection_json, last_event_digest FROM agent_transactions WHERE namespace = ? AND transaction_id = ?`,
		namespace,
		transactionID,
	).Scan(&data, &lastDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return transactionreducer.Projection{}, storeError(model.ErrorNotFound, "get_transaction", transactionID, "transaction not found in namespace", err)
	}
	if err != nil {
		return transactionreducer.Projection{}, storeError(model.ErrorInternal, "get_transaction", transactionID, "query transaction", err)
	}
	projection, err := model.DecodeStrict[transactionreducer.Projection](data)
	if err != nil {
		return transactionreducer.Projection{}, storeError(model.ErrorEventChain, "get_transaction", transactionID, "stored projection is invalid", err)
	}
	if err := projection.Validate(); err != nil {
		return transactionreducer.Projection{}, storeError(model.ErrorEventChain, "get_transaction", transactionID, "stored projection failed validation", err)
	}
	if projection.Transaction.Namespace != namespace || projection.Transaction.ID != transactionID || projection.LastEventDigest != lastDigest {
		return transactionreducer.Projection{}, storeError(model.ErrorEventChain, "get_transaction", transactionID, "stored projection identity or digest is invalid", nil)
	}
	return projection, nil
}

func insertTransactionEvent(ctx context.Context, tx *sql.Tx, namespace string, event model.TransactionEvent, data []byte) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO transaction_events(
			namespace, transaction_id, sequence, event_id, event_type,
			event_digest, event_json, occurred_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		namespace,
		event.TransactionID,
		event.Sequence,
		event.ID,
		event.Type,
		event.Digest,
		data,
		event.OccurredAt.Format(timeFormat),
	)
	if err != nil {
		return classifyWriteError("append_transaction_events", event.TransactionID, err)
	}
	return nil
}

func verifyTransactionEventDigest(event model.TransactionEvent) error {
	valid, err := model.VerifyTransactionEventDigest(event)
	if err != nil {
		return storeError(model.ErrorInternal, "verify_transaction_event", event.ID, "compute event digest", err)
	}
	if !valid {
		return storeError(model.ErrorEventChain, "verify_transaction_event", event.ID, "event digest does not match its content", nil)
	}
	return nil
}
