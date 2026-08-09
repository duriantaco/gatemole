package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/reducer"
	_ "modernc.org/sqlite"
)

const sqliteSchemaVersion = 3

type SQLiteStore struct {
	db                   *sql.DB
	admissionFault       func(string) error
	pairedExecutionFault func(string) error
}

// Health performs a bounded, constant-work database readiness check. It does
// not scan or decode user-controlled run or transaction state.
func (s *SQLiteStore) Health(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return storeError(
			model.ErrorInternal,
			"health_store",
			"",
			"ping SQLite database",
			err,
		)
	}
	var value int
	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&value); err != nil {
		return storeError(
			model.ErrorInternal,
			"health_store",
			"",
			"query SQLite database",
			err,
		)
	}
	if value != 1 {
		return storeError(
			model.ErrorInternal,
			"health_store",
			"",
			"SQLite health query returned an invalid result",
			nil,
		)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storeError(
			model.ErrorInternal,
			"health_store",
			"",
			"begin SQLite write probe",
			err,
		)
	}
	rolledBack := false
	defer func() {
		if !rolledBack {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO kernel_metadata(key, value)
		 VALUES ('readiness_write_probe', 'rollback-only')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
	); err != nil {
		return storeError(
			model.ErrorInternal,
			"health_store",
			"",
			"execute SQLite write probe",
			err,
		)
	}
	if err := tx.Rollback(); err != nil {
		return storeError(
			model.ErrorInternal,
			"health_store",
			"",
			"rollback SQLite write probe",
			err,
		)
	}
	rolledBack = true
	return nil
}

func OpenSQLite(path string) (*SQLiteStore, error) {
	return openSQLite(context.Background(), path, nil)
}

func openSQLite(
	ctx context.Context,
	path string,
	beforeInitialize func(context.Context, *sql.DB) error,
) (*SQLiteStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, storeError(model.ErrorSchemaInvalid, "open_store", path, "database path is required", nil)
	}
	if path != ":memory:" {
		parent := filepath.Dir(path)
		if err := os.MkdirAll(parent, 0o750); err != nil {
			return nil, storeError(model.ErrorInternal, "open_store", path, "create database directory", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, storeError(model.ErrorInternal, "open_store", path, "open SQLite database", err)
	}
	// A single connection makes local transaction ordering explicit. The
	// optimistic sequence check remains authoritative and supports a later
	// multi-process or PostgreSQL implementation.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &SQLiteStore{db: db}
	if beforeInitialize != nil {
		if err := beforeInitialize(ctx, db); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := store.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if path != ":memory:" {
		if err := os.Chmod(path, 0o600); err != nil {
			_ = db.Close()
			return nil, storeError(model.ErrorInternal, "open_store", path, "restrict database permissions", err)
		}
	}
	return store, nil
}

func (s *SQLiteStore) initialize(ctx context.Context) error {
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA busy_timeout = 5000",
	}
	for _, statement := range pragmas {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return storeError(model.ErrorInternal, "initialize_store", "", statement, err)
		}
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS kernel_metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS runs (
			namespace TEXT NOT NULL,
			run_id TEXT NOT NULL,
			event_sequence INTEGER NOT NULL CHECK (event_sequence >= 1),
			last_event_digest TEXT NOT NULL,
			state_json BLOB NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (namespace, run_id)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS events (
			namespace TEXT NOT NULL,
			run_id TEXT NOT NULL,
			sequence INTEGER NOT NULL CHECK (sequence >= 1),
			event_id TEXT NOT NULL UNIQUE,
			event_type TEXT NOT NULL,
			event_digest TEXT NOT NULL,
			event_json BLOB NOT NULL,
			occurred_at TEXT NOT NULL,
			PRIMARY KEY (namespace, run_id, sequence),
			FOREIGN KEY (namespace, run_id) REFERENCES runs(namespace, run_id) ON DELETE RESTRICT
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS events_by_run_type
			ON events(namespace, run_id, event_type, sequence)`,
		`CREATE TABLE IF NOT EXISTS agent_transactions (
			namespace TEXT NOT NULL,
			transaction_id TEXT NOT NULL,
			event_sequence INTEGER NOT NULL CHECK (event_sequence >= 1),
			last_event_digest TEXT NOT NULL,
			projection_json BLOB NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (namespace, transaction_id)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS transaction_events (
			namespace TEXT NOT NULL,
			transaction_id TEXT NOT NULL,
			sequence INTEGER NOT NULL CHECK (sequence >= 1),
			event_id TEXT NOT NULL UNIQUE,
			event_type TEXT NOT NULL,
			event_digest TEXT NOT NULL,
			event_json BLOB NOT NULL,
			occurred_at TEXT NOT NULL,
			PRIMARY KEY (namespace, transaction_id, sequence),
			FOREIGN KEY (namespace, transaction_id) REFERENCES agent_transactions(namespace, transaction_id) ON DELETE RESTRICT
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS transaction_events_by_type
			ON transaction_events(namespace, transaction_id, event_type, sequence)`,
		`CREATE TABLE IF NOT EXISTS execution_contracts (
			namespace TEXT NOT NULL,
			contract_digest TEXT NOT NULL,
			contract_id TEXT NOT NULL,
			contract_json BLOB NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (namespace, contract_digest),
			UNIQUE (namespace, contract_id)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS agent_tasks (
			namespace TEXT NOT NULL,
			task_id TEXT NOT NULL,
			task_digest TEXT NOT NULL,
			transaction_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			task_json BLOB NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (namespace, task_id),
			UNIQUE (namespace, task_digest),
			UNIQUE (namespace, transaction_id),
			UNIQUE (namespace, run_id),
			UNIQUE (namespace, task_id, task_digest)
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS task_admissions (
			namespace TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			request_digest TEXT NOT NULL,
			task_id TEXT NOT NULL,
			task_digest TEXT NOT NULL,
			contract_digest TEXT NOT NULL,
			run_id TEXT NOT NULL,
			transaction_id TEXT NOT NULL,
			result_json BLOB NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (namespace, idempotency_key),
			UNIQUE (namespace, task_id),
			UNIQUE (namespace, run_id),
			UNIQUE (namespace, transaction_id),
			FOREIGN KEY (namespace, task_id, task_digest)
				REFERENCES agent_tasks(namespace, task_id, task_digest)
				ON DELETE RESTRICT,
			FOREIGN KEY (namespace, contract_digest)
				REFERENCES execution_contracts(namespace, contract_digest)
				ON DELETE RESTRICT,
			FOREIGN KEY (namespace, run_id)
				REFERENCES runs(namespace, run_id)
				ON DELETE RESTRICT,
			FOREIGN KEY (namespace, transaction_id)
				REFERENCES agent_transactions(namespace, transaction_id)
				ON DELETE RESTRICT
		) STRICT`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return storeError(model.ErrorInternal, "migrate_store", "", "apply kernel schema", err)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO kernel_metadata(key, value) VALUES('schema_version', ?)
		 ON CONFLICT(key) DO NOTHING`,
		fmt.Sprintf("%d", sqliteSchemaVersion),
	); err != nil {
		return storeError(model.ErrorInternal, "migrate_store", "", "record schema version", err)
	}
	var version string
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM kernel_metadata WHERE key = 'schema_version'`,
	).Scan(&version); err != nil {
		return storeError(model.ErrorInternal, "migrate_store", "", "read schema version", err)
	}
	if version == "1" {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE kernel_metadata SET value = ? WHERE key = 'schema_version' AND value = '1'`,
			"2",
		); err != nil {
			return storeError(model.ErrorInternal, "migrate_store", "", "upgrade schema version", err)
		}
		version = "2"
	}
	if version == "2" {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE kernel_metadata SET value = ? WHERE key = 'schema_version' AND value = '2'`,
			fmt.Sprintf("%d", sqliteSchemaVersion),
		); err != nil {
			return storeError(model.ErrorInternal, "migrate_store", "", "upgrade schema version", err)
		}
		version = fmt.Sprintf("%d", sqliteSchemaVersion)
	}
	if version != fmt.Sprintf("%d", sqliteSchemaVersion) {
		return storeError(
			model.ErrorCheckpointIncompatible,
			"migrate_store",
			"",
			fmt.Sprintf("database schema version %s is incompatible with %d", version, sqliteSchemaVersion),
			nil,
		)
	}
	return nil
}

func (s *SQLiteStore) CreateRun(ctx context.Context, event model.RunEvent) (reducer.Projection, error) {
	if err := verifyEventDigest(event); err != nil {
		return reducer.Projection{}, err
	}
	projection, err := reducer.Apply(nil, event)
	if err != nil {
		return reducer.Projection{}, err
	}
	runJSON, err := json.Marshal(projection.Run)
	if err != nil {
		return reducer.Projection{}, storeError(model.ErrorInternal, "create_run", projection.Run.ID, "encode run", err)
	}
	eventJSON, err := json.Marshal(event)
	if err != nil {
		return reducer.Projection{}, storeError(model.ErrorInternal, "create_run", projection.Run.ID, "encode event", err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return reducer.Projection{}, storeError(model.ErrorInternal, "create_run", projection.Run.ID, "begin transaction", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO runs(
			namespace, run_id, event_sequence, last_event_digest, state_json, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		projection.Run.Namespace,
		projection.Run.ID,
		projection.Run.EventSequence,
		projection.LastEventDigest,
		runJSON,
		projection.Run.CreatedAt.Format(timeFormat),
		projection.Run.UpdatedAt.Format(timeFormat),
	)
	if err != nil {
		return reducer.Projection{}, classifyWriteError("create_run", projection.Run.ID, err)
	}
	if err := insertEvent(ctx, tx, projection.Run.Namespace, event, eventJSON); err != nil {
		return reducer.Projection{}, err
	}
	if err := tx.Commit(); err != nil {
		return reducer.Projection{}, storeError(model.ErrorInternal, "create_run", projection.Run.ID, "commit transaction", err)
	}
	return projection, nil
}

func (s *SQLiteStore) AppendEvent(
	ctx context.Context,
	namespace string,
	expectedSequence int64,
	event model.RunEvent,
) (reducer.Projection, error) {
	if err := validateNamespace(namespace); err != nil {
		return reducer.Projection{}, err
	}
	if err := verifyEventDigest(event); err != nil {
		return reducer.Projection{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return reducer.Projection{}, storeError(model.ErrorInternal, "append_event", event.RunID, "begin transaction", err)
	}
	defer tx.Rollback()
	current, err := loadProjection(ctx, tx, namespace, event.RunID)
	if err != nil {
		return reducer.Projection{}, err
	}
	if current.Run.EventSequence != expectedSequence {
		return reducer.Projection{}, conflict(
			"append_event",
			event.RunID,
			fmt.Sprintf("expected run sequence %d, current sequence is %d", expectedSequence, current.Run.EventSequence),
			nil,
		)
	}
	next, err := reducer.Apply(&current, event)
	if err != nil {
		return reducer.Projection{}, err
	}
	runJSON, err := json.Marshal(next.Run)
	if err != nil {
		return reducer.Projection{}, storeError(model.ErrorInternal, "append_event", event.RunID, "encode run", err)
	}
	eventJSON, err := json.Marshal(event)
	if err != nil {
		return reducer.Projection{}, storeError(model.ErrorInternal, "append_event", event.RunID, "encode event", err)
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE runs
		 SET event_sequence = ?, last_event_digest = ?, state_json = ?, updated_at = ?
		 WHERE namespace = ? AND run_id = ? AND event_sequence = ?`,
		next.Run.EventSequence,
		next.LastEventDigest,
		runJSON,
		next.Run.UpdatedAt.Format(timeFormat),
		namespace,
		event.RunID,
		expectedSequence,
	)
	if err != nil {
		return reducer.Projection{}, classifyWriteError("append_event", event.RunID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return reducer.Projection{}, storeError(model.ErrorInternal, "append_event", event.RunID, "read update result", err)
	}
	if rows != 1 {
		return reducer.Projection{}, conflict("append_event", event.RunID, "run changed during append", nil)
	}
	if err := insertEvent(ctx, tx, namespace, event, eventJSON); err != nil {
		return reducer.Projection{}, err
	}
	if err := tx.Commit(); err != nil {
		return reducer.Projection{}, classifyWriteError("append_event", event.RunID, err)
	}
	return next, nil
}

func (s *SQLiteStore) GetRun(ctx context.Context, namespace, runID string) (reducer.Projection, error) {
	if err := validateNamespace(namespace); err != nil {
		return reducer.Projection{}, err
	}
	if strings.TrimSpace(runID) == "" {
		return reducer.Projection{}, storeError(model.ErrorSchemaInvalid, "get_run", runID, "run ID is required", nil)
	}
	return loadProjection(ctx, s.db, namespace, runID)
}

func (s *SQLiteStore) ListRuns(ctx context.Context, namespace string) ([]model.AgentRun, error) {
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT state_json FROM runs WHERE namespace = ? ORDER BY created_at, run_id`,
		namespace,
	)
	if err != nil {
		return nil, storeError(model.ErrorInternal, "list_runs", namespace, "query runs", err)
	}
	defer rows.Close()
	runs := []model.AgentRun{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, storeError(model.ErrorInternal, "list_runs", namespace, "scan run", err)
		}
		run, err := model.DecodeStrict[model.AgentRun](data)
		if err != nil {
			return nil, storeError(model.ErrorEventChain, "list_runs", namespace, "stored run is invalid", err)
		}
		if err := run.Validate(); err != nil {
			return nil, storeError(model.ErrorEventChain, "list_runs", run.ID, "stored run failed validation", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError(model.ErrorInternal, "list_runs", namespace, "iterate runs", err)
	}
	return runs, nil
}

func (s *SQLiteStore) ListAllRuns(ctx context.Context) ([]reducer.Projection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT namespace, run_id, state_json, last_event_digest
		 FROM runs ORDER BY namespace, created_at, run_id`,
	)
	if err != nil {
		return nil, storeError(model.ErrorInternal, "list_all_runs", "", "query runs", err)
	}
	defer rows.Close()
	projections := []reducer.Projection{}
	for rows.Next() {
		var namespace, runID, lastDigest string
		var data []byte
		if err := rows.Scan(&namespace, &runID, &data, &lastDigest); err != nil {
			return nil, storeError(model.ErrorInternal, "list_all_runs", "", "scan run", err)
		}
		run, err := model.DecodeStrict[model.AgentRun](data)
		if err != nil {
			return nil, storeError(model.ErrorEventChain, "list_all_runs", runID, "stored run is invalid", err)
		}
		if err := run.Validate(); err != nil {
			return nil, storeError(model.ErrorEventChain, "list_all_runs", runID, "stored run failed validation", err)
		}
		if run.Namespace != namespace || run.ID != runID || !digestPattern.MatchString(lastDigest) {
			return nil, storeError(model.ErrorEventChain, "list_all_runs", runID, "stored projection identity or digest is invalid", nil)
		}
		projections = append(projections, reducer.Projection{Run: run, LastEventDigest: lastDigest})
	}
	if err := rows.Err(); err != nil {
		return nil, storeError(model.ErrorInternal, "list_all_runs", "", "iterate runs", err)
	}
	return projections, nil
}

func (s *SQLiteStore) Events(
	ctx context.Context,
	namespace, runID string,
	afterSequence int64,
) ([]model.RunEvent, error) {
	if err := validateNamespace(namespace); err != nil {
		return nil, err
	}
	if strings.TrimSpace(runID) == "" {
		return nil, storeError(model.ErrorSchemaInvalid, "list_events", runID, "run ID is required", nil)
	}
	if afterSequence < 0 {
		return nil, storeError(model.ErrorSchemaInvalid, "list_events", runID, "after sequence cannot be negative", nil)
	}
	if _, err := s.GetRun(ctx, namespace, runID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_json FROM events
		 WHERE namespace = ? AND run_id = ? AND sequence > ?
		 ORDER BY sequence`,
		namespace,
		runID,
		afterSequence,
	)
	if err != nil {
		return nil, storeError(model.ErrorInternal, "list_events", runID, "query events", err)
	}
	defer rows.Close()
	events := []model.RunEvent{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, storeError(model.ErrorInternal, "list_events", runID, "scan event", err)
		}
		event, err := model.DecodeStrict[model.RunEvent](data)
		if err != nil {
			return nil, storeError(model.ErrorEventChain, "list_events", runID, "stored event is invalid", err)
		}
		if err := event.Validate(); err != nil {
			return nil, storeError(model.ErrorEventChain, "list_events", event.ID, "stored event failed validation", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, storeError(model.ErrorInternal, "list_events", runID, "iterate events", err)
	}
	return events, nil
}

func (s *SQLiteStore) VerifyRun(ctx context.Context, namespace, runID string) error {
	events, err := s.Events(ctx, namespace, runID, 0)
	if err != nil {
		return err
	}
	replayed, err := reducer.Replay(events)
	if err != nil {
		return storeError(model.ErrorEventChain, "verify_run", runID, "event replay failed", err)
	}
	stored, err := s.GetRun(ctx, namespace, runID)
	if err != nil {
		return err
	}
	replayedJSON, err := json.Marshal(replayed.Run)
	if err != nil {
		return storeError(model.ErrorInternal, "verify_run", runID, "encode replayed run", err)
	}
	storedJSON, err := json.Marshal(stored.Run)
	if err != nil {
		return storeError(model.ErrorInternal, "verify_run", runID, "encode stored run", err)
	}
	if !bytes.Equal(replayedJSON, storedJSON) || replayed.LastEventDigest != stored.LastEventDigest {
		return storeError(model.ErrorEventChain, "verify_run", runID, "materialized run does not match event replay", nil)
	}
	return nil
}

func (s *SQLiteStore) Close() error {
	if err := s.db.Close(); err != nil {
		return storeError(model.ErrorInternal, "close_store", "", "close SQLite database", err)
	}
	return nil
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadProjection(ctx context.Context, query rowQuerier, namespace, runID string) (reducer.Projection, error) {
	var data []byte
	var sequence int64
	var lastDigest string
	err := query.QueryRowContext(ctx,
		`SELECT state_json, event_sequence, last_event_digest
		 FROM runs WHERE namespace = ? AND run_id = ?`,
		namespace,
		runID,
	).Scan(&data, &sequence, &lastDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return reducer.Projection{}, storeError(model.ErrorNotFound, "get_run", runID, "run not found in namespace", err)
	}
	if err != nil {
		return reducer.Projection{}, storeError(model.ErrorInternal, "get_run", runID, "query run", err)
	}
	run, err := model.DecodeStrict[model.AgentRun](data)
	if err != nil {
		return reducer.Projection{}, storeError(model.ErrorEventChain, "get_run", runID, "stored run is invalid", err)
	}
	if err := run.Validate(); err != nil {
		return reducer.Projection{}, storeError(model.ErrorEventChain, "get_run", runID, "stored run failed validation", err)
	}
	if run.Namespace != namespace ||
		run.ID != runID ||
		run.EventSequence != sequence {
		return reducer.Projection{}, storeError(model.ErrorEventChain, "get_run", runID, "stored run identity or sequence does not match its key", nil)
	}
	if !digestPattern.MatchString(lastDigest) {
		return reducer.Projection{}, storeError(model.ErrorEventChain, "get_run", runID, "stored event digest is invalid", nil)
	}
	return reducer.Projection{Run: run, LastEventDigest: lastDigest}, nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, namespace string, event model.RunEvent, data []byte) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO events(
			namespace, run_id, sequence, event_id, event_type, event_digest, event_json, occurred_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		namespace,
		event.RunID,
		event.Sequence,
		event.ID,
		event.Type,
		event.Digest,
		data,
		event.OccurredAt.Format(timeFormat),
	)
	if err != nil {
		return classifyWriteError("append_event", event.RunID, err)
	}
	return nil
}

func validateNamespace(namespace string) error {
	if !identifierPattern.MatchString(namespace) {
		return storeError(model.ErrorSchemaInvalid, "validate_namespace", namespace, "invalid namespace", nil)
	}
	return nil
}

func classifyWriteError(operation, resource string, err error) error {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unique constraint") ||
		strings.Contains(message, "constraint failed") ||
		strings.Contains(message, "database is locked") {
		return conflict(operation, resource, "concurrent or duplicate store write", err)
	}
	return storeError(model.ErrorInternal, operation, resource, "write kernel store", err)
}

func verifyEventDigest(event model.RunEvent) error {
	valid, err := model.VerifyEventDigest(event)
	if err != nil {
		return storeError(model.ErrorInternal, "verify_event", event.ID, "compute event digest", err)
	}
	if !valid {
		return storeError(model.ErrorEventChain, "verify_event", event.ID, "event digest does not match its content", nil)
	}
	return nil
}

func conflict(operation, resource, message string, cause error) *model.KernelError {
	return storeError(model.ErrorConflict, operation, resource, message, cause)
}

func storeError(code model.ErrorCode, operation, resource, message string, cause error) *model.KernelError {
	return &model.KernelError{
		Code:      code,
		Operation: operation,
		Resource:  resource,
		Message:   message,
		Cause:     cause,
	}
}

const timeFormat = "2006-01-02T15:04:05.999999999Z07:00"

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	digestPattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)
