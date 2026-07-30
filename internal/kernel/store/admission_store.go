package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/duriantaco/gatemole/internal/kernel/admission"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/reducer"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
)

const (
	admitTaskOperation = "admit_task"

	admissionFaultAfterContractInsert           = "after_contract_insert"
	admissionFaultAfterTaskInsert               = "after_task_insert"
	admissionFaultAfterRunProjectionInsert      = "after_run_projection_insert"
	admissionFaultAfterRunCreatedEvent          = "after_run_created_event"
	admissionFaultAfterCapabilitiesGrantedEvent = "after_capabilities_granted_event"
	admissionFaultAfterRunAdmittedEvent         = "after_run_admitted_event"
	admissionFaultAfterTransactionInsert        = "after_transaction_projection_insert"
	admissionFaultAfterTransactionCreatedEvent  = "after_transaction_created_event"
	admissionFaultAfterAdmissionInsert          = "after_admission_insert"
	admissionFaultBeforeCommit                  = "before_commit"
)

var admissionFaultPoints = []string{
	admissionFaultAfterContractInsert,
	admissionFaultAfterTaskInsert,
	admissionFaultAfterRunProjectionInsert,
	admissionFaultAfterRunCreatedEvent,
	admissionFaultAfterCapabilitiesGrantedEvent,
	admissionFaultAfterRunAdmittedEvent,
	admissionFaultAfterTransactionInsert,
	admissionFaultAfterTransactionCreatedEvent,
	admissionFaultAfterAdmissionInsert,
	admissionFaultBeforeCommit,
}

type encodedAdmission struct {
	resultJSON            []byte
	taskJSON              []byte
	contractJSON          []byte
	runJSON               []byte
	runEventJSON          [][]byte
	transactionJSON       []byte
	transactionEventJSON  []byte
	runProjection         reducer.Projection
	transactionProjection transactionreducer.Projection
}

// AdmitTask persists one prepared admission as a single SQLite transaction.
// The bool result is true only when this call created the admission. An
// identical idempotency retry returns the immutable first result with false;
// reuse of the key with different request authority conflicts.
func (s *SQLiteStore) AdmitTask(
	ctx context.Context,
	prepared admission.Prepared,
) (admission.Result, bool, error) {
	if err := validateAdmissionKey(
		prepared.Namespace,
		prepared.IdempotencyKey,
		prepared.RequestDigest,
	); err != nil {
		return admission.Result{}, false, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return admission.Result{}, false, storeError(
			model.ErrorInternal,
			admitTaskOperation,
			prepared.IdempotencyKey,
			"begin admission transaction",
			err,
		)
	}
	defer tx.Rollback()
	if err := requireStoredRuntimeBinding(
		ctx,
		tx,
		prepared.Result.RuntimeID,
		prepared.Result.EnforcementProfile,
		false,
		admitTaskOperation,
		prepared.IdempotencyKey,
	); err != nil {
		return admission.Result{}, false, err
	}

	existing, requestDigest, err := loadTaskAdmission(
		ctx,
		tx,
		prepared.Namespace,
		prepared.IdempotencyKey,
	)
	switch {
	case err == nil:
		if requestDigest != prepared.RequestDigest {
			return admission.Result{}, false, storeError(
				model.ErrorIdempotencyConflict,
				admitTaskOperation,
				prepared.IdempotencyKey,
				"idempotency key is already bound to different admission authority",
				nil,
			)
		}
		return existing, false, nil
	case hasStoreCode(err, model.ErrorNotFound):
		// This is the only branch allowed to create authority.
	default:
		return admission.Result{}, false, err
	}

	encoded, err := prepareAdmission(prepared)
	if err != nil {
		return admission.Result{}, false, err
	}

	if err := ensureExecutionContract(
		ctx,
		tx,
		prepared,
		encoded.contractJSON,
	); err != nil {
		return admission.Result{}, false, err
	}
	if err := s.runAdmissionFault(
		admissionFaultAfterContractInsert,
		prepared.IdempotencyKey,
	); err != nil {
		return admission.Result{}, false, err
	}

	if err := s.execAdmissionStep(
		ctx,
		tx,
		admissionFaultAfterTaskInsert,
		prepared.IdempotencyKey,
		prepared.Result.Task.ID,
		`INSERT INTO agent_tasks(
			namespace, task_id, task_digest, transaction_id, run_id,
			task_json, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		prepared.Namespace,
		prepared.Result.Task.ID,
		prepared.Result.Task.Digest,
		prepared.Result.Task.TransactionID,
		prepared.Result.Task.RunID,
		encoded.taskJSON,
		prepared.Result.Task.CreatedAt.Format(timeFormat),
	); err != nil {
		return admission.Result{}, false, err
	}

	run := encoded.runProjection.Run
	if err := s.execAdmissionStep(
		ctx,
		tx,
		admissionFaultAfterRunProjectionInsert,
		prepared.IdempotencyKey,
		run.ID,
		`INSERT INTO runs(
			namespace, run_id, event_sequence, last_event_digest,
			state_json, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		run.Namespace,
		run.ID,
		run.EventSequence,
		encoded.runProjection.LastEventDigest,
		encoded.runJSON,
		run.CreatedAt.Format(timeFormat),
		run.UpdatedAt.Format(timeFormat),
	); err != nil {
		return admission.Result{}, false, err
	}

	runEventFaults := []string{
		admissionFaultAfterRunCreatedEvent,
		admissionFaultAfterCapabilitiesGrantedEvent,
		admissionFaultAfterRunAdmittedEvent,
	}
	for index, event := range prepared.RunEvents {
		if err := insertEvent(
			ctx,
			tx,
			prepared.Namespace,
			event,
			encoded.runEventJSON[index],
		); err != nil {
			return admission.Result{}, false, err
		}
		if err := s.runAdmissionFault(
			runEventFaults[index],
			prepared.IdempotencyKey,
		); err != nil {
			return admission.Result{}, false, err
		}
	}

	transaction := encoded.transactionProjection.Transaction
	if err := s.execAdmissionStep(
		ctx,
		tx,
		admissionFaultAfterTransactionInsert,
		prepared.IdempotencyKey,
		transaction.ID,
		`INSERT INTO agent_transactions(
			namespace, transaction_id, event_sequence, last_event_digest,
			projection_json, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		transaction.Namespace,
		transaction.ID,
		transaction.EventSequence,
		encoded.transactionProjection.LastEventDigest,
		encoded.transactionJSON,
		transaction.CreatedAt.Format(timeFormat),
		transaction.UpdatedAt.Format(timeFormat),
	); err != nil {
		return admission.Result{}, false, err
	}

	if err := insertTransactionEvent(
		ctx,
		tx,
		prepared.Namespace,
		prepared.TransactionEvent,
		encoded.transactionEventJSON,
	); err != nil {
		return admission.Result{}, false, err
	}
	if err := s.runAdmissionFault(
		admissionFaultAfterTransactionCreatedEvent,
		prepared.IdempotencyKey,
	); err != nil {
		return admission.Result{}, false, err
	}

	if err := s.execAdmissionStep(
		ctx,
		tx,
		admissionFaultAfterAdmissionInsert,
		prepared.IdempotencyKey,
		prepared.IdempotencyKey,
		`INSERT INTO task_admissions(
			namespace, idempotency_key, request_digest,
			task_id, task_digest, contract_digest, run_id, transaction_id,
			result_json, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		prepared.Namespace,
		prepared.IdempotencyKey,
		prepared.RequestDigest,
		prepared.Result.Task.ID,
		prepared.Result.Task.Digest,
		prepared.Result.Contract.Digest,
		run.ID,
		transaction.ID,
		encoded.resultJSON,
		prepared.Result.Task.CreatedAt.Format(timeFormat),
	); err != nil {
		return admission.Result{}, false, err
	}
	if err := s.runAdmissionFault(
		admissionFaultBeforeCommit,
		prepared.IdempotencyKey,
	); err != nil {
		return admission.Result{}, false, err
	}

	if err := tx.Commit(); err != nil {
		return admission.Result{}, false, classifyWriteError(
			admitTaskOperation,
			prepared.IdempotencyKey,
			err,
		)
	}
	return prepared.Result, true, nil
}

func (s *SQLiteStore) GetTaskAdmission(
	ctx context.Context,
	namespace, idempotencyKey string,
) (admission.Result, string, error) {
	if err := validateAdmissionLookupKey(namespace, idempotencyKey); err != nil {
		return admission.Result{}, "", err
	}
	return loadTaskAdmission(ctx, s.db, namespace, idempotencyKey)
}

func prepareAdmission(prepared admission.Prepared) (encodedAdmission, error) {
	if err := prepared.Validate(); err != nil {
		return encodedAdmission{}, err
	}
	encoded := encodedAdmission{
		runProjection:         prepared.Result.Run,
		transactionProjection: prepared.Result.Transaction,
		runEventJSON:          make([][]byte, len(prepared.RunEvents)),
	}
	for _, target := range []struct {
		name  string
		value any
		out   *[]byte
	}{
		{"admission result", prepared.Result, &encoded.resultJSON},
		{"task", prepared.Result.Task, &encoded.taskJSON},
		{"contract", prepared.Result.Contract, &encoded.contractJSON},
		{"run projection", prepared.Result.Run.Run, &encoded.runJSON},
		{"transaction projection", prepared.Result.Transaction, &encoded.transactionJSON},
		{"transaction event", prepared.TransactionEvent, &encoded.transactionEventJSON},
	} {
		data, marshalErr := json.Marshal(target.value)
		if marshalErr != nil {
			return encodedAdmission{}, storeError(
				model.ErrorInternal,
				admitTaskOperation,
				prepared.IdempotencyKey,
				"encode "+target.name,
				marshalErr,
			)
		}
		*target.out = data
	}
	for index, event := range prepared.RunEvents {
		data, err := json.Marshal(event)
		if err != nil {
			return encodedAdmission{}, storeError(
				model.ErrorInternal,
				admitTaskOperation,
				event.ID,
				"encode run event",
				err,
			)
		}
		encoded.runEventJSON[index] = data
	}
	return encoded, nil
}

func ensureExecutionContract(
	ctx context.Context,
	tx *sql.Tx,
	prepared admission.Prepared,
	contractJSON []byte,
) error {
	contract := prepared.Result.Contract
	var existingID string
	var existingJSON []byte
	err := tx.QueryRowContext(
		ctx,
		`SELECT contract_id, contract_json
		 FROM execution_contracts
		 WHERE namespace = ? AND contract_digest = ?`,
		prepared.Namespace,
		contract.Digest,
	).Scan(&existingID, &existingJSON)
	switch {
	case err == nil:
		if existingID != contract.ID || !bytes.Equal(existingJSON, contractJSON) {
			return conflict(
				admitTaskOperation,
				contract.Digest,
				"contract digest is already bound to different content",
				nil,
			)
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return storeError(
			model.ErrorInternal,
			admitTaskOperation,
			contract.Digest,
			"query execution contract",
			err,
		)
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO execution_contracts(
			namespace, contract_digest, contract_id, contract_json, created_at
		) VALUES(?, ?, ?, ?, ?)`,
		prepared.Namespace,
		contract.Digest,
		contract.ID,
		contractJSON,
		prepared.Result.Task.CreatedAt.Format(timeFormat),
	); err != nil {
		return classifyWriteError(admitTaskOperation, contract.Digest, err)
	}
	return nil
}

func loadTaskAdmission(
	ctx context.Context,
	query rowQuerier,
	namespace, idempotencyKey string,
) (admission.Result, string, error) {
	var requestDigest, taskID, taskDigest, contractDigest, runID, transactionID string
	var resultJSON, taskJSON, contractJSON, runJSON, transactionJSON []byte
	var runLastDigest, transactionLastDigest string
	err := query.QueryRowContext(
		ctx,
		`SELECT
			a.request_digest, a.task_id, a.task_digest, a.contract_digest,
			a.run_id, a.transaction_id, a.result_json,
			t.task_json, c.contract_json,
			r.state_json, r.last_event_digest,
			x.projection_json, x.last_event_digest
		 FROM task_admissions AS a
		 JOIN agent_tasks AS t
		   ON t.namespace = a.namespace
		  AND t.task_id = a.task_id
		  AND t.task_digest = a.task_digest
		 JOIN execution_contracts AS c
		   ON c.namespace = a.namespace
		  AND c.contract_digest = a.contract_digest
		 JOIN runs AS r
		   ON r.namespace = a.namespace
		  AND r.run_id = a.run_id
		 JOIN agent_transactions AS x
		   ON x.namespace = a.namespace
		  AND x.transaction_id = a.transaction_id
		 WHERE a.namespace = ? AND a.idempotency_key = ?`,
		namespace,
		idempotencyKey,
	).Scan(
		&requestDigest,
		&taskID,
		&taskDigest,
		&contractDigest,
		&runID,
		&transactionID,
		&resultJSON,
		&taskJSON,
		&contractJSON,
		&runJSON,
		&runLastDigest,
		&transactionJSON,
		&transactionLastDigest,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return admission.Result{}, "", storeError(
			model.ErrorNotFound,
			"get_task_admission",
			idempotencyKey,
			"task admission not found in namespace",
			err,
		)
	}
	if err != nil {
		return admission.Result{}, "", storeError(
			model.ErrorInternal,
			"get_task_admission",
			idempotencyKey,
			"query task admission",
			err,
		)
	}
	result, err := decodeAdmissionJSON[admission.Result](
		resultJSON, idempotencyKey, "admission result",
	)
	if err != nil {
		return admission.Result{}, "", err
	}
	if err := requireStoredRuntimeBinding(
		ctx,
		query,
		result.RuntimeID,
		result.EnforcementProfile,
		result.Version == admission.LegacyResultVersion,
		"get_task_admission",
		idempotencyKey,
	); err != nil {
		return admission.Result{}, "", err
	}
	task, err := decodeAdmissionJSON[model.AgentTask](taskJSON, taskID, "task")
	if err != nil {
		return admission.Result{}, "", err
	}
	contract, err := decodeAdmissionJSON[model.ExecutionContract](
		contractJSON, contractDigest, "contract",
	)
	if err != nil {
		return admission.Result{}, "", err
	}
	run, err := decodeAdmissionJSON[model.AgentRun](runJSON, runID, "run projection")
	if err != nil {
		return admission.Result{}, "", err
	}
	if err := run.Validate(); err != nil {
		return admission.Result{}, "", storeError(
			model.ErrorEventChain,
			"get_task_admission",
			runID,
			"stored authoritative run failed validation",
			err,
		)
	}
	transaction, err := decodeAdmissionJSON[transactionreducer.Projection](
		transactionJSON, transactionID, "transaction projection",
	)
	if err != nil {
		return admission.Result{}, "", err
	}
	if err := transaction.Validate(); err != nil {
		return admission.Result{}, "", storeError(
			model.ErrorEventChain,
			"get_task_admission",
			transactionID,
			"stored authoritative transaction failed validation",
			err,
		)
	}
	if !digestPattern.MatchString(requestDigest) ||
		result.IdempotencyKey != idempotencyKey ||
		result.RequestDigest != requestDigest ||
		result.Task.ID != taskID ||
		result.Task.Digest != taskDigest ||
		result.Contract.Digest != contractDigest ||
		result.Run.Run.ID != runID ||
		result.Transaction.Transaction.ID != transactionID {
		return admission.Result{}, "", storeError(
			model.ErrorEventChain,
			"get_task_admission",
			idempotencyKey,
			"stored admission identity or digest is invalid",
			nil,
		)
	}
	if err := validateLoadedAdmissionResult(
		result,
		namespace,
		idempotencyKey,
		requestDigest,
	); err != nil {
		return admission.Result{}, "", err
	}
	if err := equalJSON(
		task,
		result.Task,
		"stored task does not match its admission result",
		taskID,
	); err != nil {
		return admission.Result{}, "", err
	}
	if err := equalJSON(
		contract,
		result.Contract,
		"stored contract does not match its admission result",
		contractDigest,
	); err != nil {
		return admission.Result{}, "", err
	}
	if !digestPattern.MatchString(runLastDigest) ||
		!digestPattern.MatchString(transactionLastDigest) ||
		run.EventSequence < result.Run.Run.EventSequence ||
		transaction.Transaction.EventSequence <
			result.Transaction.Transaction.EventSequence {
		return admission.Result{}, "", storeError(
			model.ErrorEventChain,
			"get_task_admission",
			idempotencyKey,
			"stored authority projection precedes its admission",
			nil,
		)
	}
	if (run.EventSequence == result.Run.Run.EventSequence &&
		runLastDigest != result.Run.LastEventDigest) ||
		(transaction.Transaction.EventSequence ==
			result.Transaction.Transaction.EventSequence &&
			transactionLastDigest != result.Transaction.LastEventDigest) {
		return admission.Result{}, "", storeError(
			model.ErrorEventChain,
			"get_task_admission",
			idempotencyKey,
			"stored authority event digest changed without lifecycle advancement",
			nil,
		)
	}
	if err := validateCurrentAdmissionBindings(result, run, transaction); err != nil {
		return admission.Result{}, "", err
	}
	if err := verifyAdmissionEventPrefixes(
		ctx,
		query,
		namespace,
		result,
	); err != nil {
		return admission.Result{}, "", err
	}
	return result, requestDigest, nil
}

func validateCurrentAdmissionBindings(
	initial admission.Result,
	currentRun model.AgentRun,
	currentTransaction transactionreducer.Projection,
) error {
	initialRun := initial.Run.Run
	current := currentTransaction.Transaction
	admittedTransaction := initial.Transaction.Transaction
	for _, comparison := range []struct {
		left, right any
		message     string
		resource    string
	}{
		{currentRun.ImageDigest, initialRun.ImageDigest, "run image changed after admission", currentRun.ID},
		{currentRun.ContractDigest, initialRun.ContractDigest, "run contract changed after admission", currentRun.ID},
		{currentRun.Principal, initialRun.Principal, "run principal changed after admission", currentRun.ID},
		{currentRun.DelegationChain, initialRun.DelegationChain, "run delegation changed after admission", currentRun.ID},
		{currentRun.ParentRunID, initialRun.ParentRunID, "run parent changed after admission", currentRun.ID},
		{currentRun.Deadline, initialRun.Deadline, "run deadline changed after admission", currentRun.ID},
		{currentRun.BudgetLimits, initialRun.BudgetLimits, "run budget limits changed after admission", currentRun.ID},
		{current.IntentDigest, admittedTransaction.IntentDigest, "transaction intent changed after admission", current.ID},
		{current.Task, admittedTransaction.Task, "transaction task changed after admission", current.ID},
		{current.Admission, admittedTransaction.Admission, "transaction admission binding changed", current.ID},
		{current.Sponsor, admittedTransaction.Sponsor, "transaction sponsor changed after admission", current.ID},
		{current.AgentRunIDs, admittedTransaction.AgentRunIDs, "transaction run binding changed after admission", current.ID},
	} {
		if err := equalJSON(
			comparison.left,
			comparison.right,
			comparison.message,
			comparison.resource,
		); err != nil {
			return err
		}
	}
	if len(currentRun.CapabilityIDs) < len(initialRun.CapabilityIDs) ||
		!slices.Equal(
			currentRun.CapabilityIDs[:len(initialRun.CapabilityIDs)],
			initialRun.CapabilityIDs,
		) {
		return storeError(
			model.ErrorEventChain, "get_task_admission", currentRun.ID,
			"run lost or reordered capabilities installed at admission", nil,
		)
	}
	return nil
}

func verifyAdmissionEventPrefixes(
	ctx context.Context,
	query rowQuerier,
	namespace string,
	result admission.Result,
) error {
	runEvents := make([]model.RunEvent, result.Run.Run.EventSequence)
	for index := range runEvents {
		var data []byte
		if err := query.QueryRowContext(
			ctx,
			`SELECT event_json FROM events
			 WHERE namespace = ? AND run_id = ? AND sequence = ?`,
			namespace,
			result.Run.Run.ID,
			index+1,
		).Scan(&data); err != nil {
			return storeError(
				model.ErrorEventChain,
				"get_task_admission",
				result.Run.Run.ID,
				"authoritative run event prefix is incomplete",
				err,
			)
		}
		event, err := decodeAdmissionJSON[model.RunEvent](
			data, result.Run.Run.ID, "run event prefix",
		)
		if err != nil {
			return err
		}
		runEvents[index] = event
	}

	var transactionEventJSON []byte
	if err := query.QueryRowContext(
		ctx,
		`SELECT event_json FROM transaction_events
		 WHERE namespace = ? AND transaction_id = ? AND sequence = 1`,
		namespace,
		result.Transaction.Transaction.ID,
	).Scan(&transactionEventJSON); err != nil {
		return storeError(
			model.ErrorEventChain,
			"get_task_admission",
			result.Transaction.Transaction.ID,
			"authoritative transaction event prefix is incomplete",
			err,
		)
	}
	transactionEvent, err := decodeAdmissionJSON[model.TransactionEvent](
		transactionEventJSON,
		result.Transaction.Transaction.ID,
		"transaction event prefix",
	)
	if err != nil {
		return err
	}
	prepared := admission.Prepared{
		Namespace:        namespace,
		IdempotencyKey:   result.IdempotencyKey,
		RequestDigest:    result.RequestDigest,
		Result:           result,
		RunEvents:        runEvents,
		TransactionEvent: transactionEvent,
	}
	if err := prepared.Validate(); err != nil {
		return storeError(
			model.ErrorEventChain,
			"get_task_admission",
			result.IdempotencyKey,
			"stored admission does not match its authoritative event prefixes",
			err,
		)
	}
	return nil
}

func validateLoadedAdmissionResult(
	result admission.Result,
	namespace, idempotencyKey, requestDigest string,
) error {
	if result.IdempotencyKey != idempotencyKey ||
		result.RequestDigest != requestDigest {
		return storeError(
			model.ErrorEventChain,
			"get_task_admission",
			idempotencyKey,
			"stored admission result envelope is invalid",
			nil,
		)
	}
	if err := result.Validate(namespace); err != nil {
		return storeError(
			model.ErrorEventChain,
			"get_task_admission",
			idempotencyKey,
			"stored admission result failed validation",
			err,
		)
	}
	return nil
}

func validateAdmissionKey(namespace, idempotencyKey, requestDigest string) error {
	if err := validateAdmissionLookupKey(namespace, idempotencyKey); err != nil {
		return err
	}
	if !digestPattern.MatchString(requestDigest) {
		return storeError(
			model.ErrorSchemaInvalid,
			admitTaskOperation,
			idempotencyKey,
			"request digest must be a SHA-256 digest",
			nil,
		)
	}
	return nil
}

func validateAdmissionLookupKey(namespace, idempotencyKey string) error {
	if err := validateNamespace(namespace); err != nil {
		return err
	}
	if !identifierPattern.MatchString(idempotencyKey) {
		return storeError(
			model.ErrorSchemaInvalid,
			"get_task_admission",
			idempotencyKey,
			"invalid idempotency key",
			nil,
		)
	}
	return nil
}

func (s *SQLiteStore) execAdmissionStep(
	ctx context.Context,
	tx *sql.Tx,
	point, admissionKey, resource, statement string,
	args ...any,
) error {
	if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
		return classifyWriteError(admitTaskOperation, resource, err)
	}
	return s.runAdmissionFault(point, admissionKey)
}

func (s *SQLiteStore) runAdmissionFault(point, resource string) error {
	if s.admissionFault == nil {
		return nil
	}
	if err := s.admissionFault(point); err != nil {
		return storeError(
			model.ErrorInternal,
			admitTaskOperation,
			resource,
			fmt.Sprintf("admission persistence fault at %s", point),
			err,
		)
	}
	return nil
}

func decodeAdmissionJSON[T any](data []byte, resource, name string) (T, error) {
	value, err := model.DecodeStrict[T](data)
	if err != nil {
		var zero T
		return zero, storeError(
			model.ErrorEventChain,
			"get_task_admission",
			resource,
			"stored "+name+" is invalid",
			err,
		)
	}
	return value, nil
}

func equalJSON(left, right any, message, resource string) error {
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return storeError(
			model.ErrorInternal,
			admitTaskOperation,
			resource,
			"encode comparison input",
			err,
		)
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		return storeError(
			model.ErrorInternal,
			admitTaskOperation,
			resource,
			"encode comparison input",
			err,
		)
	}
	if !bytes.Equal(leftJSON, rightJSON) {
		return storeError(
			model.ErrorEventChain,
			admitTaskOperation,
			resource,
			message,
			nil,
		)
	}
	return nil
}

func hasStoreCode(err error, code model.ErrorCode) bool {
	var kernelError *model.KernelError
	return errors.As(err, &kernelError) && kernelError.Code == code
}
