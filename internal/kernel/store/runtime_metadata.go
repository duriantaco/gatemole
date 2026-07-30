package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/runtimeidentity"
)

const (
	runtimeIDMetadataKey  = "runtime_id"
	bindRuntimeMetadataOp = "bind_runtime_metadata"
)

// OpenSQLiteForRuntime inspects an existing ledger's Runtime binding on the
// same SQLite connection before schema initialization, then initializes and
// atomically binds the requested Runtime metadata. A mismatched existing
// ledger is therefore rejected before migrations or recovery can change it.
func OpenSQLiteForRuntime(
	ctx context.Context,
	path string,
	runtimeID string,
	profile string,
) (*SQLiteStore, error) {
	if ctx == nil {
		return nil, runtimeMetadataError(
			model.ErrorSchemaInvalid,
			runtimeIDMetadataKey,
			"context is required",
			nil,
		)
	}
	if err := validateRuntimeMetadataInput(runtimeID, profile); err != nil {
		return nil, err
	}
	kernelStore, err := openSQLite(
		ctx,
		path,
		func(ctx context.Context, database *sql.DB) error {
			return checkExistingRuntimeMetadata(
				ctx,
				database,
				runtimeID,
				profile,
			)
		},
	)
	if err != nil {
		return nil, err
	}
	if err := kernelStore.BindRuntimeMetadata(
		ctx,
		runtimeID,
		profile,
	); err != nil {
		_ = kernelStore.Close()
		return nil, err
	}
	return kernelStore, nil
}

// BindRuntimeMetadata durably binds one Runtime instance and enforcement
// profile to a SQLite ledger in a single metadata transaction.
//
// Runtime identity never changes automatically, even while the ledger is
// empty. A missing Runtime binding on a non-empty legacy ledger may be adopted
// only in development. Production refuses to relabel existing history.
func (s *SQLiteStore) BindRuntimeMetadata(
	ctx context.Context,
	runtimeID string,
	profile string,
) error {
	if err := validateRuntimeMetadataInput(runtimeID, profile); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return runtimeMetadataError(
			model.ErrorInternal,
			runtimeIDMetadataKey,
			"begin metadata transaction",
			err,
		)
	}
	defer tx.Rollback()

	boundRuntimeID, runtimeBound, err := readKernelMetadata(
		ctx, tx, runtimeIDMetadataKey,
	)
	if err != nil {
		return runtimeMetadataError(
			model.ErrorInternal,
			runtimeIDMetadataKey,
			"read Runtime identity",
			err,
		)
	}
	if runtimeBound {
		if !runtimeidentity.IsRuntimeID(boundRuntimeID) {
			return runtimeMetadataError(
				model.ErrorCheckpointIncompatible,
				runtimeIDMetadataKey,
				fmt.Sprintf("stored Runtime ID %q is invalid", boundRuntimeID),
				nil,
			)
		}
		if boundRuntimeID != runtimeID {
			return runtimeMetadataError(
				model.ErrorCheckpointIncompatible,
				runtimeIDMetadataKey,
				"SQLite ledger belongs to a different Runtime instance",
				nil,
			)
		}
	}

	boundProfile, profileBound, err := readKernelMetadata(
		ctx, tx, enforcementProfileMetadataKey,
	)
	if err != nil {
		return runtimeMetadataError(
			model.ErrorInternal,
			enforcementProfileMetadataKey,
			"read enforcement profile",
			err,
		)
	}
	if profileBound &&
		boundProfile != EnforcementProfileDevelopment &&
		boundProfile != EnforcementProfileProduction {
		return runtimeMetadataError(
			model.ErrorCheckpointIncompatible,
			enforcementProfileMetadataKey,
			fmt.Sprintf("stored enforcement profile %q is invalid", boundProfile),
			nil,
		)
	}

	needsRuntimeWrite := !runtimeBound
	needsProfileWrite := !profileBound || boundProfile != profile
	nonempty := false
	if needsRuntimeWrite || needsProfileWrite {
		nonempty, err = ledgerHasEntries(ctx, tx)
		if err != nil {
			return err
		}
	}
	if nonempty && needsRuntimeWrite &&
		profile == EnforcementProfileProduction {
		return runtimeMetadataError(
			model.ErrorCheckpointIncompatible,
			runtimeIDMetadataKey,
			"production cannot adopt a nonempty ledger without a Runtime identity",
			nil,
		)
	}
	if nonempty && needsProfileWrite {
		switch {
		case !profileBound && profile == EnforcementProfileProduction:
			return runtimeMetadataError(
				model.ErrorCheckpointIncompatible,
				enforcementProfileMetadataKey,
				"production cannot adopt a nonempty ledger without an enforcement profile",
				nil,
			)
		case profileBound:
			return runtimeMetadataError(
				model.ErrorCheckpointIncompatible,
				enforcementProfileMetadataKey,
				fmt.Sprintf(
					"nonempty ledger is bound to enforcement profile %q",
					boundProfile,
				),
				nil,
			)
		}
	}

	if needsRuntimeWrite {
		if err := insertKernelMetadata(
			ctx, tx, runtimeIDMetadataKey, runtimeID,
		); err != nil {
			return err
		}
	}
	if needsProfileWrite {
		if !profileBound {
			if err := insertKernelMetadata(
				ctx, tx, enforcementProfileMetadataKey, profile,
			); err != nil {
				return err
			}
		} else if err := updateKernelMetadata(
			ctx, tx, enforcementProfileMetadataKey, boundProfile, profile,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return runtimeMetadataError(
			model.ErrorInternal,
			runtimeIDMetadataKey,
			"commit Runtime metadata transaction",
			err,
		)
	}
	return nil
}

func validateRuntimeMetadataInput(runtimeID string, profile string) error {
	if !runtimeidentity.IsRuntimeID(runtimeID) {
		return runtimeMetadataError(
			model.ErrorSchemaInvalid,
			runtimeIDMetadataKey,
			"runtime ID is invalid",
			nil,
		)
	}
	if profile != EnforcementProfileDevelopment &&
		profile != EnforcementProfileProduction {
		return runtimeMetadataError(
			model.ErrorSchemaInvalid,
			enforcementProfileMetadataKey,
			"enforcement profile must be development or production",
			nil,
		)
	}
	return nil
}

func checkExistingRuntimeMetadata(
	ctx context.Context,
	database *sql.DB,
	expectedRuntimeID string,
	expectedProfile string,
) error {
	var metadataTableExists bool
	if err := database.QueryRowContext(
		ctx,
		`SELECT EXISTS(
			SELECT 1
			FROM sqlite_master
			WHERE type = 'table' AND name = 'kernel_metadata'
		)`,
	).Scan(&metadataTableExists); err != nil {
		return runtimeMetadataError(
			model.ErrorInternal,
			runtimeIDMetadataKey,
			"inspect existing SQLite metadata schema",
			err,
		)
	}
	nonempty, err := existingLedgerHasEntries(ctx, database)
	if err != nil {
		return err
	}

	storedRuntimeID := ""
	runtimeBound := false
	storedProfile := ""
	profileBound := false
	if metadataTableExists {
		storedRuntimeID, runtimeBound, err = readExistingRuntimeMetadata(
			ctx,
			database,
			runtimeIDMetadataKey,
		)
		if err != nil {
			return runtimeMetadataError(
				model.ErrorInternal,
				runtimeIDMetadataKey,
				"read existing Runtime identity",
				err,
			)
		}
		storedProfile, profileBound, err = readExistingRuntimeMetadata(
			ctx,
			database,
			enforcementProfileMetadataKey,
		)
		if err != nil {
			return runtimeMetadataError(
				model.ErrorInternal,
				enforcementProfileMetadataKey,
				"read existing enforcement profile",
				err,
			)
		}
	}

	switch {
	case runtimeBound && !runtimeidentity.IsRuntimeID(storedRuntimeID):
		return runtimeMetadataError(
			model.ErrorCheckpointIncompatible,
			runtimeIDMetadataKey,
			fmt.Sprintf("stored Runtime ID %q is invalid", storedRuntimeID),
			nil,
		)
	case runtimeBound && storedRuntimeID != expectedRuntimeID:
		return runtimeMetadataError(
			model.ErrorCheckpointIncompatible,
			runtimeIDMetadataKey,
			"SQLite ledger belongs to a different Runtime instance",
			nil,
		)
	case profileBound &&
		storedProfile != EnforcementProfileDevelopment &&
		storedProfile != EnforcementProfileProduction:
		return runtimeMetadataError(
			model.ErrorCheckpointIncompatible,
			enforcementProfileMetadataKey,
			fmt.Sprintf("stored enforcement profile %q is invalid", storedProfile),
			nil,
		)
	}

	needsRuntimeWrite := !runtimeBound
	needsProfileWrite := !profileBound || storedProfile != expectedProfile
	if nonempty && needsRuntimeWrite &&
		expectedProfile == EnforcementProfileProduction {
		return runtimeMetadataError(
			model.ErrorCheckpointIncompatible,
			runtimeIDMetadataKey,
			"production cannot adopt a nonempty ledger without a Runtime identity",
			nil,
		)
	}
	if nonempty && needsProfileWrite {
		switch {
		case !profileBound &&
			expectedProfile == EnforcementProfileProduction:
			return runtimeMetadataError(
				model.ErrorCheckpointIncompatible,
				enforcementProfileMetadataKey,
				"production cannot adopt a nonempty ledger without an enforcement profile",
				nil,
			)
		case profileBound:
			return runtimeMetadataError(
				model.ErrorCheckpointIncompatible,
				enforcementProfileMetadataKey,
				fmt.Sprintf(
					"nonempty ledger is bound to enforcement profile %q",
					storedProfile,
				),
				nil,
			)
		}
	}
	return nil
}

func readExistingRuntimeMetadata(
	ctx context.Context,
	database *sql.DB,
	key string,
) (string, bool, error) {
	var value string
	err := database.QueryRowContext(
		ctx,
		`SELECT value FROM kernel_metadata WHERE key = ?`,
		key,
	).Scan(&value)
	switch {
	case err == nil:
		return value, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	default:
		return "", false, err
	}
}

func existingLedgerHasEntries(
	ctx context.Context,
	database *sql.DB,
) (bool, error) {
	for _, table := range []string{
		"runs",
		"events",
		"agent_transactions",
		"transaction_events",
		"execution_contracts",
		"agent_tasks",
		"task_admissions",
	} {
		var exists bool
		if err := database.QueryRowContext(
			ctx,
			`SELECT EXISTS(
				SELECT 1
				FROM sqlite_master
				WHERE type = 'table' AND name = ?
			)`,
			table,
		).Scan(&exists); err != nil {
			return false, runtimeMetadataError(
				model.ErrorInternal,
				runtimeIDMetadataKey,
				"inspect existing SQLite ledger schema",
				err,
			)
		}
		if !exists {
			continue
		}
		var hasRows bool
		query := `SELECT EXISTS(SELECT 1 FROM ` + table + `)`
		if err := database.QueryRowContext(ctx, query).Scan(&hasRows); err != nil {
			return false, runtimeMetadataError(
				model.ErrorInternal,
				runtimeIDMetadataKey,
				"inspect existing SQLite ledger contents",
				err,
			)
		}
		if hasRows {
			return true, nil
		}
	}
	return false, nil
}

func requireStoredRuntimeBinding(
	ctx context.Context,
	query rowQuerier,
	expectedRuntimeID string,
	expectedProfile string,
	allowLegacy bool,
	operation string,
	resource string,
) error {
	var storedRuntimeID string
	legacyAdmission := false
	err := query.QueryRowContext(
		ctx,
		`SELECT value FROM kernel_metadata WHERE key = ?`,
		runtimeIDMetadataKey,
	).Scan(&storedRuntimeID)
	switch {
	case errors.Is(err, sql.ErrNoRows) && expectedRuntimeID == "":
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return storeError(
			model.ErrorCheckpointIncompatible,
			operation,
			resource,
			"SQLite ledger has no Runtime identity binding",
			nil,
		)
	case err != nil:
		return storeError(
			model.ErrorInternal,
			operation,
			resource,
			"read SQLite Runtime identity binding",
			err,
		)
	case !runtimeidentity.IsRuntimeID(storedRuntimeID):
		return storeError(
			model.ErrorCheckpointIncompatible,
			operation,
			resource,
			fmt.Sprintf("stored Runtime ID %q is invalid", storedRuntimeID),
			nil,
		)
	case expectedRuntimeID == "" && allowLegacy:
		legacyAdmission = true
	case expectedRuntimeID == "":
		return storeError(
			model.ErrorCheckpointIncompatible,
			operation,
			resource,
			"admission has no Runtime identity binding",
			nil,
		)
	case !model.IsRuntimeID(expectedRuntimeID):
		return storeError(
			model.ErrorSchemaInvalid,
			operation,
			resource,
			"admission Runtime identity is invalid",
			nil,
		)
	case storedRuntimeID != expectedRuntimeID:
		return storeError(
			model.ErrorCheckpointIncompatible,
			operation,
			resource,
			"admission belongs to a different Runtime instance",
			nil,
		)
	}

	if legacyAdmission {
		var storedProfile string
		err = query.QueryRowContext(
			ctx,
			`SELECT value FROM kernel_metadata WHERE key = ?`,
			enforcementProfileMetadataKey,
		).Scan(&storedProfile)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return storeError(
				model.ErrorCheckpointIncompatible,
				operation,
				resource,
				"SQLite ledger has no enforcement profile binding",
				nil,
			)
		case err != nil:
			return storeError(
				model.ErrorInternal,
				operation,
				resource,
				"read SQLite enforcement profile binding",
				err,
			)
		case storedProfile != EnforcementProfileDevelopment:
			return storeError(
				model.ErrorCheckpointIncompatible,
				operation,
				resource,
				"legacy v0 admission may be replayed only by a development Runtime",
				nil,
			)
		default:
			return nil
		}
	}

	if !model.IsEnforcementProfile(expectedProfile) {
		return storeError(
			model.ErrorSchemaInvalid,
			operation,
			resource,
			"admission enforcement profile is invalid",
			nil,
		)
	}
	var storedProfile string
	err = query.QueryRowContext(
		ctx,
		`SELECT value FROM kernel_metadata WHERE key = ?`,
		enforcementProfileMetadataKey,
	).Scan(&storedProfile)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return storeError(
			model.ErrorCheckpointIncompatible,
			operation,
			resource,
			"SQLite ledger has no enforcement profile binding",
			nil,
		)
	case err != nil:
		return storeError(
			model.ErrorInternal,
			operation,
			resource,
			"read SQLite enforcement profile binding",
			err,
		)
	case storedProfile != expectedProfile:
		return storeError(
			model.ErrorCheckpointIncompatible,
			operation,
			resource,
			"admission belongs to a different enforcement profile",
			nil,
		)
	default:
		return nil
	}
}

func readKernelMetadata(
	ctx context.Context,
	tx *sql.Tx,
	key string,
) (string, bool, error) {
	var value string
	err := tx.QueryRowContext(
		ctx,
		`SELECT value FROM kernel_metadata WHERE key = ?`,
		key,
	).Scan(&value)
	switch {
	case err == nil:
		return value, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	default:
		return "", false, err
	}
}

func insertKernelMetadata(
	ctx context.Context,
	tx *sql.Tx,
	key string,
	value string,
) error {
	result, err := tx.ExecContext(
		ctx,
		`INSERT INTO kernel_metadata(key, value) VALUES(?, ?)`,
		key,
		value,
	)
	if err != nil {
		return classifyWriteError(bindRuntimeMetadataOp, key, err)
	}
	return requireOneMetadataRow(result, key)
}

func updateKernelMetadata(
	ctx context.Context,
	tx *sql.Tx,
	key string,
	from string,
	to string,
) error {
	result, err := tx.ExecContext(
		ctx,
		`UPDATE kernel_metadata
		 SET value = ?
		 WHERE key = ? AND value = ?`,
		to,
		key,
		from,
	)
	if err != nil {
		return classifyWriteError(bindRuntimeMetadataOp, key, err)
	}
	return requireOneMetadataRow(result, key)
}

func requireOneMetadataRow(result sql.Result, key string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return runtimeMetadataError(
			model.ErrorInternal,
			key,
			"inspect Runtime metadata write",
			err,
		)
	}
	if rows != 1 {
		return runtimeMetadataError(
			model.ErrorConflict,
			key,
			"Runtime metadata changed during binding",
			nil,
		)
	}
	return nil
}

func runtimeMetadataError(
	code model.ErrorCode,
	resource string,
	message string,
	cause error,
) *model.KernelError {
	return storeError(
		code,
		bindRuntimeMetadataOp,
		resource,
		message,
		cause,
	)
}
