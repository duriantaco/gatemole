package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
)

func TestSQLiteStoreImplementsStore(t *testing.T) {
	t.Parallel()
	var _ RunStore = (*SQLiteStore)(nil)
	var _ EventStore = (*SQLiteStore)(nil)
	var _ Store = (*SQLiteStore)(nil)
}

func TestSQLiteStoreHealthIsConstantAndFailsAfterClose(t *testing.T) {
	t.Parallel()
	kernelStore, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := kernelStore.Health(context.Background()); err != nil {
		t.Fatalf("healthy store: %v", err)
	}
	var probes int
	if err := kernelStore.db.QueryRow(
		`SELECT COUNT(*) FROM kernel_metadata
		 WHERE key = 'readiness_write_probe'`,
	).Scan(&probes); err != nil {
		t.Fatal(err)
	}
	if probes != 0 {
		t.Fatal("readiness write probe was committed")
	}
	if err := kernelStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := kernelStore.Health(context.Background()); err == nil {
		t.Fatal("closed store reported healthy")
	}
}

func TestSQLiteStoreCreatesAppendsRestartsAndVerifies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	created := readCreationEvent(t)
	projection, err := store.CreateRun(ctx, created)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	for i, state := range []model.RunState{model.RunAdmitted, model.RunRunning} {
		event := stateEvent(t, projection, state, "", i+2)
		projection, err = store.AppendEvent(ctx, projection.Run.Namespace, projection.Run.EventSequence, event)
		if err != nil {
			t.Fatalf("append %s: %v", state, err)
		}
	}
	if err := store.VerifyRun(ctx, projection.Run.Namespace, projection.Run.ID); err != nil {
		t.Fatalf("verify before restart: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database permissions are too broad: %o", info.Mode().Perm())
	}

	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restored, err := reopened.GetRun(ctx, projection.Run.Namespace, projection.Run.ID)
	if err != nil {
		t.Fatalf("get after restart: %v", err)
	}
	if restored.Run.State != model.RunRunning || restored.Run.EventSequence != 3 {
		t.Fatalf("unexpected restored projection: %#v", restored)
	}
	if restored.LastEventDigest != projection.LastEventDigest {
		t.Fatalf("last digest %q, want %q", restored.LastEventDigest, projection.LastEventDigest)
	}
	events, err := reopened.Events(ctx, projection.Run.Namespace, projection.Run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	if err := reopened.VerifyRun(ctx, projection.Run.Namespace, projection.Run.ID); err != nil {
		t.Fatalf("verify after restart: %v", err)
	}
}

func TestSQLiteStoreRecoversEveryNonTerminalState(t *testing.T) {
	paths := map[model.RunState][]model.RunState{
		model.RunCreated:            {},
		model.RunAdmitted:           {model.RunAdmitted},
		model.RunRunning:            {model.RunAdmitted, model.RunRunning},
		model.RunWaitingForEvent:    {model.RunAdmitted, model.RunRunning, model.RunWaitingForEvent},
		model.RunWaitingForAgent:    {model.RunAdmitted, model.RunRunning, model.RunWaitingForAgent},
		model.RunWaitingForApproval: {model.RunAdmitted, model.RunRunning, model.RunWaitingForApproval},
		model.RunBlocked:            {model.RunAdmitted, model.RunBlocked},
	}
	for target, transitions := range paths {
		t.Run(string(target), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "kernel.db")
			kernelStore, err := OpenSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			projection, err := kernelStore.CreateRun(ctx, readCreationEvent(t))
			if err != nil {
				t.Fatal(err)
			}
			for i, state := range transitions {
				reason := ""
				if state == model.RunBlocked {
					reason = "recovery fixture"
				}
				event := stateEvent(t, projection, state, reason, i+2)
				projection, err = kernelStore.AppendEvent(ctx, projection.Run.Namespace, projection.Run.EventSequence, event)
				if err != nil {
					t.Fatal(err)
				}
			}
			before, err := json.Marshal(projection)
			if err != nil {
				t.Fatal(err)
			}
			if err := kernelStore.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := OpenSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			restored, err := reopened.GetRun(ctx, projection.Run.Namespace, projection.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			after, err := json.Marshal(restored)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("projection changed across restart:\nbefore=%s\nafter=%s", before, after)
			}
			if restored.Run.State != target {
				t.Fatalf("restored state=%s, want %s", restored.Run.State, target)
			}
			if err := reopened.VerifyRun(ctx, restored.Run.Namespace, restored.Run.ID); err != nil {
				t.Fatalf("verify recovered run: %v", err)
			}
		})
	}
}

func TestSQLiteStoreUsesOptimisticSequenceConflicts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	projection, err := store.CreateRun(ctx, readCreationEvent(t))
	if err != nil {
		t.Fatal(err)
	}

	events := []model.RunEvent{
		stateEvent(t, projection, model.RunAdmitted, "", 2),
		stateEvent(t, projection, model.RunFailed, "competing admission failure", 2),
	}
	errorsOut := make(chan error, len(events))
	var wg sync.WaitGroup
	for _, event := range events {
		event := event
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, appendErr := store.AppendEvent(ctx, projection.Run.Namespace, projection.Run.EventSequence, event)
			errorsOut <- appendErr
		}()
	}
	wg.Wait()
	close(errorsOut)

	successes := 0
	conflicts := 0
	for appendErr := range errorsOut {
		if appendErr == nil {
			successes++
			continue
		}
		if hasKernelCode(appendErr, model.ErrorConflict) {
			conflicts++
			continue
		}
		t.Fatalf("unexpected append error: %v", appendErr)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want 1 each", successes, conflicts)
	}
	eventsAfter, err := store.Events(ctx, projection.Run.Namespace, projection.Run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != 2 {
		t.Fatalf("stored events = %d, want 2", len(eventsAfter))
	}
}

func TestSQLiteStoreRollsBackRejectedTransition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	projection, err := store.CreateRun(ctx, readCreationEvent(t))
	if err != nil {
		t.Fatal(err)
	}
	illegal := stateEvent(t, projection, model.RunCompleted, "", 2)
	_, err = store.AppendEvent(ctx, projection.Run.Namespace, projection.Run.EventSequence, illegal)
	assertKernelCode(t, err, model.ErrorTransitionInvalid)

	stored, err := store.GetRun(ctx, projection.Run.Namespace, projection.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Run.State != model.RunCreated || stored.Run.EventSequence != 1 {
		t.Fatalf("rejected event mutated projection: %#v", stored)
	}
	events, err := store.Events(ctx, projection.Run.Namespace, projection.Run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want only creation", len(events))
	}
}

func TestSQLiteStoreRejectsDuplicateRunAndIsolatesNamespaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	created := readCreationEvent(t)
	projection, err := store.CreateRun(ctx, created)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateRun(ctx, created)
	assertKernelCode(t, err, model.ErrorConflict)

	_, err = store.GetRun(ctx, "another-namespace", projection.Run.ID)
	assertKernelCode(t, err, model.ErrorNotFound)
	listed, err := store.ListRuns(ctx, "another-namespace")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("cross-namespace list returned %d runs", len(listed))
	}
}

func TestSQLiteStoreDetectsMaterializedStateTampering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	projection, err := store.CreateRun(ctx, readCreationEvent(t))
	if err != nil {
		t.Fatal(err)
	}
	tampered := projection.Run
	tampered.State = model.RunAdmitted
	data, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.ExecContext(ctx,
		`UPDATE runs SET state_json = ? WHERE namespace = ? AND run_id = ?`,
		data,
		projection.Run.Namespace,
		projection.Run.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	err = store.VerifyRun(ctx, projection.Run.Namespace, projection.Run.ID)
	assertKernelCode(t, err, model.ErrorEventChain)
}

func TestSQLiteStoreFiltersEventsAfterCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openTestStore(t)
	projection, err := store.CreateRun(ctx, readCreationEvent(t))
	if err != nil {
		t.Fatal(err)
	}
	for i, state := range []model.RunState{model.RunAdmitted, model.RunRunning} {
		event := stateEvent(t, projection, state, "", i+2)
		projection, err = store.AppendEvent(ctx, projection.Run.Namespace, projection.Run.EventSequence, event)
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.Events(ctx, projection.Run.Namespace, projection.Run.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Sequence != 2 || events[1].Sequence != 3 {
		t.Fatalf("unexpected cursor result: %#v", events)
	}
}

func openTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func readCreationEvent(t *testing.T) model.RunEvent {
	t.Helper()
	path := filepath.Join(schemaRoot(t), "fixtures", "kernel", "valid", "run_event.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	event, err := model.DecodeStrict[model.RunEvent](data)
	if err != nil {
		t.Fatal(err)
	}
	event.Digest, err = model.ComputeEventDigest(event)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func stateEvent(t *testing.T, projection reducer.Projection, to model.RunState, reason string, sequence int) model.RunEvent {
	t.Helper()
	payload, err := json.Marshal(model.RunStateChangedPayload{
		From:   projection.Run.State,
		To:     to,
		Reason: reason,
	})
	if err != nil {
		t.Fatal(err)
	}
	event := model.RunEvent{
		Version:        model.RunEventVersion,
		ID:             fmt.Sprintf("event:store-demo:%d:%s", sequence, strings.ReplaceAll(string(to), "_", "-")),
		RunID:          projection.Run.ID,
		Sequence:       int64(sequence),
		Type:           reducer.EventRunStateChanged,
		Actor:          model.Principal{ID: "service:gatemoled", Kind: model.PrincipalService},
		OccurredAt:     projection.Run.UpdatedAt.Add(time.Duration(sequence) * time.Second),
		Payload:        payload,
		PreviousDigest: projection.LastEventDigest,
	}
	event.Digest, err = model.ComputeEventDigest(event)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func schemaRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "schemas"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func assertKernelCode(t *testing.T, err error, want model.ErrorCode) {
	t.Helper()
	if !hasKernelCode(err, want) {
		t.Fatalf("expected kernel error %s, got %T: %v", want, err, err)
	}
}

func hasKernelCode(err error, want model.ErrorCode) bool {
	var kernelErr *model.KernelError
	return errors.As(err, &kernelErr) && kernelErr.Code == want
}
