package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duriantaco/gatemole/internal/kernel/model"
)

func TestBindRuntimeMetadataPersistsAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kernel.db")
	runtimeID := runtimeMetadataTestID("0")

	kernelStore, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := kernelStore.BindRuntimeMetadata(
		ctx, runtimeID, EnforcementProfileDevelopment,
	); err != nil {
		t.Fatal(err)
	}
	if err := kernelStore.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.BindRuntimeMetadata(
		ctx, runtimeID, EnforcementProfileDevelopment,
	); err != nil {
		t.Fatalf("idempotent bind after restart: %v", err)
	}
	assertStoredKernelMetadata(t, reopened, runtimeIDMetadataKey, runtimeID)
	assertStoredKernelMetadata(
		t, reopened,
		enforcementProfileMetadataKey,
		EnforcementProfileDevelopment,
	)
}

func TestOpenSQLiteForRuntimeRejectsMismatchBeforeInitialization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kernel.db")
	first := runtimeMetadataTestID("1")
	second := runtimeMetadataTestID("2")

	kernelStore, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := kernelStore.BindRuntimeMetadata(
		ctx,
		first,
		EnforcementProfileDevelopment,
	); err != nil {
		t.Fatal(err)
	}
	if err := kernelStore.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	mismatched, err := OpenSQLiteForRuntime(
		ctx,
		path,
		second,
		EnforcementProfileDevelopment,
	)
	if mismatched != nil {
		_ = mismatched.Close()
		t.Fatal("mismatched Runtime returned an open store")
	}
	assertRuntimeMetadataError(
		t,
		err,
		model.ErrorCheckpointIncompatible,
		runtimeIDMetadataKey,
	)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("mismatched Runtime changed the SQLite database before rejection")
	}

	reopened, err := OpenSQLiteForRuntime(
		ctx,
		path,
		first,
		EnforcementProfileDevelopment,
	)
	if err != nil {
		t.Fatalf("matching Runtime could not reopen ledger: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertStoredKernelMetadata(t, reopened, runtimeIDMetadataKey, first)
}

func TestOpenSQLiteForRuntimeValidatesBeforeCreatingDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kernel.db")
	kernelStore, err := OpenSQLiteForRuntime(
		context.Background(),
		path,
		"invalid Runtime",
		EnforcementProfileDevelopment,
	)
	if kernelStore != nil {
		_ = kernelStore.Close()
		t.Fatal("invalid Runtime returned an open store")
	}
	assertRuntimeMetadataError(
		t,
		err,
		model.ErrorSchemaInvalid,
		runtimeIDMetadataKey,
	)
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("invalid Runtime created a database: %v", statErr)
	}
}

func TestOpenSQLiteForRuntimeRejectsProfileConflictBeforeInitialization(
	t *testing.T,
) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "kernel.db")
	runtimeID := runtimeMetadataTestID("3")
	kernelStore, err := OpenSQLiteForRuntime(
		ctx,
		path,
		runtimeID,
		EnforcementProfileDevelopment,
	)
	if err != nil {
		t.Fatal(err)
	}
	createEnforcementProfileTransaction(t, kernelStore)
	if err := kernelStore.Close(); err != nil {
		t.Fatal(err)
	}
	before := runtimeDatabaseSnapshot(t, directory)

	reopened, err := OpenSQLiteForRuntime(
		ctx,
		path,
		runtimeID,
		EnforcementProfileProduction,
	)
	if reopened != nil {
		_ = reopened.Close()
		t.Fatal("conflicting profile returned an open store")
	}
	assertRuntimeMetadataError(
		t,
		err,
		model.ErrorCheckpointIncompatible,
		enforcementProfileMetadataKey,
	)
	after := runtimeDatabaseSnapshot(t, directory)
	if !equalRuntimeDatabaseSnapshots(before, after) {
		t.Fatalf(
			"profile conflict changed database files:\nbefore=%v\nafter=%v",
			before,
			after,
		)
	}
}

func TestOpenSQLiteForRuntimeRejectsLegacyProductionBeforeInitialization(
	t *testing.T,
) {
	t.Parallel()
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "kernel.db")
	kernelStore, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	createEnforcementProfileTransaction(t, kernelStore)
	if err := kernelStore.Close(); err != nil {
		t.Fatal(err)
	}
	before := runtimeDatabaseSnapshot(t, directory)

	reopened, err := OpenSQLiteForRuntime(
		ctx,
		path,
		runtimeMetadataTestID("4"),
		EnforcementProfileProduction,
	)
	if reopened != nil {
		_ = reopened.Close()
		t.Fatal("legacy production ledger returned an open store")
	}
	assertRuntimeMetadataError(
		t,
		err,
		model.ErrorCheckpointIncompatible,
		runtimeIDMetadataKey,
	)
	after := runtimeDatabaseSnapshot(t, directory)
	if !equalRuntimeDatabaseSnapshots(before, after) {
		t.Fatalf(
			"legacy production refusal changed database files:\nbefore=%v\nafter=%v",
			before,
			after,
		)
	}
}

func TestBindRuntimeMetadataNeverRebindsRuntimeID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	first := runtimeMetadataTestID("a")
	second := runtimeMetadataTestID("b")

	if err := kernelStore.BindRuntimeMetadata(
		ctx, first, EnforcementProfileDevelopment,
	); err != nil {
		t.Fatal(err)
	}
	err := kernelStore.BindRuntimeMetadata(
		ctx, second, EnforcementProfileProduction,
	)
	assertRuntimeMetadataError(
		t, err, model.ErrorCheckpointIncompatible, runtimeIDMetadataKey,
	)
	assertStoredKernelMetadata(t, kernelStore, runtimeIDMetadataKey, first)
	assertStoredKernelMetadata(
		t, kernelStore,
		enforcementProfileMetadataKey,
		EnforcementProfileDevelopment,
	)
}

func TestBindRuntimeMetadataDevelopmentAdoptsLegacyLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	runtimeID := runtimeMetadataTestID("c")
	createEnforcementProfileTransaction(t, kernelStore)

	if err := kernelStore.BindRuntimeMetadata(
		ctx, runtimeID, EnforcementProfileDevelopment,
	); err != nil {
		t.Fatalf("development adoption: %v", err)
	}
	assertStoredKernelMetadata(t, kernelStore, runtimeIDMetadataKey, runtimeID)
	assertStoredKernelMetadata(
		t, kernelStore,
		enforcementProfileMetadataKey,
		EnforcementProfileDevelopment,
	)
}

func TestBindRuntimeMetadataProductionRefusesLegacyRuntimeAdoption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	if err := kernelStore.BindEnforcementProfile(
		ctx, EnforcementProfileProduction,
	); err != nil {
		t.Fatal(err)
	}
	createEnforcementProfileTransaction(t, kernelStore)

	err := kernelStore.BindRuntimeMetadata(
		ctx,
		runtimeMetadataTestID("d"),
		EnforcementProfileProduction,
	)
	assertRuntimeMetadataError(
		t, err, model.ErrorCheckpointIncompatible, runtimeIDMetadataKey,
	)
	assertMissingKernelMetadata(t, kernelStore, runtimeIDMetadataKey)
	assertStoredKernelMetadata(
		t, kernelStore,
		enforcementProfileMetadataKey,
		EnforcementProfileProduction,
	)
}

func TestBindRuntimeMetadataRejectsInvalidInputWithoutPartialWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)

	err := kernelStore.BindRuntimeMetadata(
		ctx,
		runtimeMetadataTestID("e"),
		"Production",
	)
	assertRuntimeMetadataError(
		t, err, model.ErrorSchemaInvalid, enforcementProfileMetadataKey,
	)
	assertMissingKernelMetadata(t, kernelStore, runtimeIDMetadataKey)
	assertMissingKernelMetadata(t, kernelStore, enforcementProfileMetadataKey)

	err = kernelStore.BindRuntimeMetadata(
		ctx, "not valid!", EnforcementProfileDevelopment,
	)
	assertRuntimeMetadataError(
		t, err, model.ErrorSchemaInvalid, runtimeIDMetadataKey,
	)
	assertMissingKernelMetadata(t, kernelStore, runtimeIDMetadataKey)
}

func TestBindRuntimeMetadataRejectsCorruptStoredRuntimeID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	kernelStore := openTestStore(t)
	if _, err := kernelStore.db.ExecContext(
		ctx,
		`INSERT INTO kernel_metadata(key, value) VALUES(?, ?)`,
		runtimeIDMetadataKey,
		"not valid!",
	); err != nil {
		t.Fatal(err)
	}

	err := kernelStore.BindRuntimeMetadata(
		ctx,
		runtimeMetadataTestID("f"),
		EnforcementProfileDevelopment,
	)
	assertRuntimeMetadataError(
		t, err, model.ErrorCheckpointIncompatible, runtimeIDMetadataKey,
	)
	assertStoredKernelMetadata(t, kernelStore, runtimeIDMetadataKey, "not valid!")
	assertMissingKernelMetadata(t, kernelStore, enforcementProfileMetadataKey)
}

func assertStoredKernelMetadata(
	t *testing.T,
	kernelStore *SQLiteStore,
	key string,
	want string,
) {
	t.Helper()
	var got string
	if err := kernelStore.db.QueryRow(
		`SELECT value FROM kernel_metadata WHERE key = ?`,
		key,
	).Scan(&got); err != nil {
		t.Fatalf("read metadata %q: %v", key, err)
	}
	if got != want {
		t.Fatalf("metadata %q = %q, want %q", key, got, want)
	}
}

func assertMissingKernelMetadata(
	t *testing.T,
	kernelStore *SQLiteStore,
	key string,
) {
	t.Helper()
	var value string
	err := kernelStore.db.QueryRow(
		`SELECT value FROM kernel_metadata WHERE key = ?`,
		key,
	).Scan(&value)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("metadata %q unexpectedly exists as %q: %v", key, value, err)
	}
}

func assertRuntimeMetadataError(
	t *testing.T,
	err error,
	code model.ErrorCode,
	resource string,
) {
	t.Helper()
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) {
		t.Fatalf("error = %T %v, want *model.KernelError", err, err)
	}
	if kernelErr.Code != code ||
		kernelErr.Operation != bindRuntimeMetadataOp ||
		kernelErr.Resource != resource {
		t.Fatalf("unexpected kernel error: %#v", kernelErr)
	}
}

func runtimeMetadataTestID(character string) string {
	return "runtime:" + strings.Repeat(character, 64)
}

func runtimeDatabaseSnapshot(t *testing.T, directory string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := make(map[string][]byte, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		snapshot[entry.Name()] = data
	}
	return snapshot
}

func equalRuntimeDatabaseSnapshots(
	left map[string][]byte,
	right map[string][]byte,
) bool {
	if len(left) != len(right) {
		return false
	}
	for name, leftData := range left {
		rightData, exists := right[name]
		if !exists || !bytes.Equal(leftData, rightData) {
			return false
		}
	}
	return true
}
