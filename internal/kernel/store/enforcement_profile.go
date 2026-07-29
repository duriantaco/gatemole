package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/duriantaco/vouch/internal/kernel/model"
)

const (
	EnforcementProfileDevelopment = "development"
	EnforcementProfileProduction  = "production"

	enforcementProfileMetadataKey = "enforcement_profile"
	bindEnforcementProfileOp      = "bind_enforcement_profile"
)

// BindEnforcementProfile durably associates a SQLite ledger with its
// enforcement profile. Empty ledgers may be rebound, but a ledger containing
// run or transaction history cannot move between profiles. An existing
// unmarked ledger may only be adopted by the development profile.
func (s *SQLiteStore) BindEnforcementProfile(ctx context.Context, profile string) error {
	if profile != EnforcementProfileDevelopment &&
		profile != EnforcementProfileProduction {
		return enforcementProfileError(
			model.ErrorSchemaInvalid,
			"enforcement profile must be development or production",
			nil,
		)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return enforcementProfileError(model.ErrorInternal, "begin metadata transaction", err)
	}
	defer tx.Rollback()

	var boundProfile string
	err = tx.QueryRowContext(
		ctx,
		`SELECT value FROM kernel_metadata WHERE key = ?`,
		enforcementProfileMetadataKey,
	).Scan(&boundProfile)
	switch {
	case err == nil:
		if boundProfile != EnforcementProfileDevelopment &&
			boundProfile != EnforcementProfileProduction {
			return enforcementProfileError(
				model.ErrorCheckpointIncompatible,
				fmt.Sprintf("stored enforcement profile %q is invalid", boundProfile),
				nil,
			)
		}
		if boundProfile == profile {
			return commitEnforcementProfileBinding(tx)
		}
	case errors.Is(err, sql.ErrNoRows):
		boundProfile = ""
	default:
		return enforcementProfileError(model.ErrorInternal, "read enforcement profile", err)
	}

	nonempty, err := ledgerHasEntries(ctx, tx)
	if err != nil {
		return err
	}
	if nonempty {
		if boundProfile == "" && profile == EnforcementProfileProduction {
			return enforcementProfileError(
				model.ErrorCheckpointIncompatible,
				"production cannot adopt a nonempty ledger without an enforcement profile",
				nil,
			)
		}
		if boundProfile != "" {
			return enforcementProfileError(
				model.ErrorCheckpointIncompatible,
				fmt.Sprintf("nonempty ledger is bound to enforcement profile %q", boundProfile),
				nil,
			)
		}
	}

	var result sql.Result
	if boundProfile == "" {
		result, err = tx.ExecContext(
			ctx,
			`INSERT INTO kernel_metadata(key, value) VALUES(?, ?)`,
			enforcementProfileMetadataKey,
			profile,
		)
	} else {
		result, err = tx.ExecContext(
			ctx,
			`UPDATE kernel_metadata SET value = ? WHERE key = ? AND value = ?`,
			profile,
			enforcementProfileMetadataKey,
			boundProfile,
		)
	}
	if err != nil {
		return classifyWriteError(bindEnforcementProfileOp, enforcementProfileMetadataKey, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return enforcementProfileError(model.ErrorInternal, "inspect metadata write", err)
	}
	if rows != 1 {
		return enforcementProfileError(
			model.ErrorConflict,
			"enforcement profile changed during binding",
			nil,
		)
	}
	return commitEnforcementProfileBinding(tx)
}

func ledgerHasEntries(ctx context.Context, tx *sql.Tx) (bool, error) {
	var nonempty bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM runs
		UNION ALL SELECT 1 FROM events
		UNION ALL SELECT 1 FROM agent_transactions
		UNION ALL SELECT 1 FROM transaction_events
		UNION ALL SELECT 1 FROM execution_contracts
		UNION ALL SELECT 1 FROM agent_tasks
		UNION ALL SELECT 1 FROM task_admissions
	)`).Scan(&nonempty)
	if err != nil {
		return false, enforcementProfileError(model.ErrorInternal, "inspect ledger contents", err)
	}
	return nonempty, nil
}

func commitEnforcementProfileBinding(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return enforcementProfileError(model.ErrorInternal, "commit metadata transaction", err)
	}
	return nil
}

func enforcementProfileError(code model.ErrorCode, message string, cause error) *model.KernelError {
	return storeError(
		code,
		bindEnforcementProfileOp,
		enforcementProfileMetadataKey,
		message,
		cause,
	)
}
