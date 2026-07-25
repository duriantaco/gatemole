package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
)

func TestBindEnforcementProfileEmptyLedgerMayBindAndSwitch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)

	for _, profile := range []string{
		EnforcementProfileDevelopment,
		EnforcementProfileProduction,
		EnforcementProfileProduction,
	} {
		if err := store.BindEnforcementProfile(ctx, profile); err != nil {
			t.Fatalf("bind %s: %v", profile, err)
		}
		assertStoredEnforcementProfile(t, store, profile)
	}
}

func TestBindEnforcementProfilePersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindEnforcementProfile(ctx, EnforcementProfileProduction); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertStoredEnforcementProfile(t, reopened, EnforcementProfileProduction)
	if err := reopened.BindEnforcementProfile(ctx, EnforcementProfileProduction); err != nil {
		t.Fatalf("idempotent binding after restart: %v", err)
	}
}

func TestBindEnforcementProfileDevelopmentLedgerRefusesProduction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)

	if err := store.BindEnforcementProfile(ctx, EnforcementProfileDevelopment); err != nil {
		t.Fatal(err)
	}
	createEnforcementProfileTransaction(t, store)

	err := store.BindEnforcementProfile(ctx, EnforcementProfileProduction)
	assertEnforcementProfileError(t, err, model.ErrorCheckpointIncompatible)
	assertStoredEnforcementProfile(t, store, EnforcementProfileDevelopment)
}

func TestBindEnforcementProfileProductionLedgerRefusesDevelopment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)

	if err := store.BindEnforcementProfile(ctx, EnforcementProfileProduction); err != nil {
		t.Fatal(err)
	}
	createEnforcementProfileTransaction(t, store)

	err := store.BindEnforcementProfile(ctx, EnforcementProfileDevelopment)
	assertEnforcementProfileError(t, err, model.ErrorCheckpointIncompatible)
	assertStoredEnforcementProfile(t, store, EnforcementProfileProduction)
}

func TestBindEnforcementProfileProductionRefusesUnmarkedLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	createEnforcementProfileTransaction(t, store)

	err := store.BindEnforcementProfile(ctx, EnforcementProfileProduction)
	assertEnforcementProfileError(t, err, model.ErrorCheckpointIncompatible)

	var profile string
	err = store.db.QueryRowContext(
		ctx,
		`SELECT value FROM kernel_metadata WHERE key = ?`,
		enforcementProfileMetadataKey,
	).Scan(&profile)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("production refusal recorded profile %q: %v", profile, err)
	}
}

func TestBindEnforcementProfileDevelopmentAdoptsUnmarkedLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	createEnforcementProfileTransaction(t, store)

	if err := store.BindEnforcementProfile(ctx, EnforcementProfileDevelopment); err != nil {
		t.Fatalf("development adoption: %v", err)
	}
	assertStoredEnforcementProfile(t, store, EnforcementProfileDevelopment)
}

func TestBindEnforcementProfileRejectsInvalidProfile(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)

	for _, profile := range []string{"", "Development", " production ", "staging"} {
		err := store.BindEnforcementProfile(context.Background(), profile)
		assertEnforcementProfileError(t, err, model.ErrorSchemaInvalid)
	}
}

func TestBindEnforcementProfileRejectsCorruptStoredProfile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.db.ExecContext(
		ctx,
		`INSERT INTO kernel_metadata(key, value) VALUES(?, ?)`,
		enforcementProfileMetadataKey,
		"unexpected",
	); err != nil {
		t.Fatal(err)
	}

	err := store.BindEnforcementProfile(ctx, EnforcementProfileDevelopment)
	assertEnforcementProfileError(t, err, model.ErrorCheckpointIncompatible)
	assertStoredEnforcementProfile(t, store, "unexpected")
}

func createEnforcementProfileTransaction(t *testing.T, store *SQLiteStore) {
	t.Helper()
	now := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	sponsor := model.Principal{ID: "human:profile-test", Kind: model.PrincipalHuman}
	transaction := model.AgentTransaction{
		Version:                model.AgentTransactionVersion,
		ID:                     "tx:enforcement-profile",
		Namespace:              "profile-test",
		IntentDigest:           "sha256:" + strings.Repeat("a", 64),
		Sponsor:                sponsor,
		AgentRunIDs:            []string{"run:enforcement-profile"},
		StageBindings:          []model.StageBinding{},
		State:                  model.TransactionCreated,
		EffectIDs:              []string{},
		VerificationResultIDs:  []string{},
		OutstandingApprovalIDs: []string{},
		EventSequence:          1,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	event, err := transactionreducer.CreationEvent(transaction, sponsor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTransaction(context.Background(), event); err != nil {
		t.Fatalf("create transaction: %v", err)
	}
}

func assertStoredEnforcementProfile(t *testing.T, store *SQLiteStore, want string) {
	t.Helper()
	var got string
	if err := store.db.QueryRow(
		`SELECT value FROM kernel_metadata WHERE key = ?`,
		enforcementProfileMetadataKey,
	).Scan(&got); err != nil {
		t.Fatalf("read stored enforcement profile: %v", err)
	}
	if got != want {
		t.Fatalf("stored enforcement profile = %q, want %q", got, want)
	}
}

func assertEnforcementProfileError(t *testing.T, err error, want model.ErrorCode) {
	t.Helper()
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) {
		t.Fatalf("error = %T %v, want *model.KernelError", err, err)
	}
	if kernelErr.Code != want ||
		kernelErr.Operation != bindEnforcementProfileOp ||
		kernelErr.Resource != enforcementProfileMetadataKey {
		t.Fatalf("unexpected kernel error: %#v", kernelErr)
	}
}
