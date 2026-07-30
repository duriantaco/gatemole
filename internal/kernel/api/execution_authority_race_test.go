package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/eventlog"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/reducer"
	"github.com/duriantaco/gatemole/internal/kernel/store"
)

func TestRunAgentExecutionRejectsAuthorityExpiringDuringPrelaunch(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	clock := &prelaunchExpiryClock{
		valid:   base,
		expired: base.Add(11 * time.Second),
	}
	fixture := newExecutionAuthorityRaceAPIFixture(
		t,
		"deadline-prelaunch",
		clock.Now,
		nil,
	)

	clock.Arm()
	assertExecutionAuthorityRejectedBeforeWorkload(
		t,
		fixture,
		model.ErrorCapabilityExpired,
	)
	if calls := clock.ArmedCalls(); calls < 2 {
		t.Fatalf("prelaunch clock calls=%d want at least 2", calls)
	}
}

func TestRunAgentExecutionRejectsRunHeadChangingBeforeLaunchClaim(t *testing.T) {
	t.Parallel()
	base := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	var racingStore *runHeadChangingStore
	fixture := newExecutionAuthorityRaceAPIFixture(
		t,
		"run-head-claim",
		func() time.Time { return base },
		func(kernelStore store.Store) store.Store {
			racingStore = &runHeadChangingStore{
				Store: kernelStore,
				actor: model.Principal{
					ID:   "service:authority-race",
					Kind: model.PrincipalService,
				},
			}
			return racingStore
		},
	)

	response := requestJSONWithRuntimeHeader(
		t,
		fixture.handler,
		http.MethodPost,
		fmt.Sprintf(
			"/v0/namespaces/%s/transactions/%s/executions/run",
			fixture.namespace,
			fixture.transactionID,
		),
		runAgentExecutionRequest{
			ExpectedSequence: fixture.transactionHead,
			Actor:            fixture.actor,
			Image:            fixture.image,
			Command:          append([]string(nil), fixture.command...),
			TimeoutSeconds:   1,
		},
		fixture.runtimeID,
	)
	if mutationErr := racingStore.MutationError(); mutationErr != nil {
		t.Fatalf("change admitted run head: %v", mutationErr)
	}
	if !racingStore.Mutated() {
		t.Fatal("authority snapshot did not trigger the run-head race")
	}
	if response.Code != http.StatusConflict {
		t.Fatalf(
			"run-head claim status=%d want=%d body=%s",
			response.Code,
			http.StatusConflict,
			response.Body.String(),
		)
	}
	var kernelErr model.KernelError
	decodeResponse(t, response, &kernelErr)
	if kernelErr.Code != model.ErrorConflict {
		t.Fatalf(
			"run-head claim code=%q want=%q body=%s",
			kernelErr.Code,
			model.ErrorConflict,
			response.Body.String(),
		)
	}
	if _, err := os.Stat(fixture.engineMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale run authority invoked OCI engine: %v", err)
	}

	ctx := context.Background()
	transaction, err := fixture.kernelStore.GetTransaction(
		ctx,
		fixture.namespace,
		fixture.transactionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	transactionEvents, err := fixture.kernelStore.TransactionEvents(
		ctx,
		fixture.namespace,
		fixture.transactionID,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if transaction.Transaction.EventSequence != fixture.transactionHead ||
		len(transactionEvents) != fixture.transactionSize ||
		len(transaction.Executions) != 0 {
		t.Fatalf(
			"failed run-head claim changed transaction: sequence=%d events=%d executions=%d",
			transaction.Transaction.EventSequence,
			len(transactionEvents),
			len(transaction.Executions),
		)
	}

	run, err := fixture.kernelStore.GetRun(
		ctx,
		fixture.namespace,
		fixture.runID,
	)
	if err != nil {
		t.Fatal(err)
	}
	runEvents, err := fixture.kernelStore.Events(
		ctx,
		fixture.namespace,
		fixture.runID,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if run.Run.State != model.RunCancelled ||
		run.Run.EventSequence != fixture.runHead+1 ||
		len(runEvents) != fixture.runSize+1 {
		t.Fatalf(
			"racing run mutation missing: state=%q sequence=%d events=%d",
			run.Run.State,
			run.Run.EventSequence,
			len(runEvents),
		)
	}
}

type prelaunchExpiryClock struct {
	mu      sync.Mutex
	valid   time.Time
	expired time.Time
	armed   bool
	calls   int
}

func (clock *prelaunchExpiryClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if !clock.armed {
		return clock.valid
	}
	clock.calls++
	if clock.calls == 1 {
		return clock.valid
	}
	return clock.expired
}

func (clock *prelaunchExpiryClock) Arm() {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.armed = true
	clock.calls = 0
}

func (clock *prelaunchExpiryClock) ArmedCalls() int {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.calls
}

type runHeadChangingStore struct {
	store.Store
	actor model.Principal

	once        sync.Once
	mu          sync.Mutex
	armed       bool
	mutated     bool
	mutationErr error
}

func (kernelStore *runHeadChangingStore) GetExecutionAuthority(
	ctx context.Context,
	namespace, transactionID string,
) (store.ExecutionAuthoritySnapshot, error) {
	snapshot, err := kernelStore.Store.GetExecutionAuthority(
		ctx,
		namespace,
		transactionID,
	)
	if err != nil {
		return store.ExecutionAuthoritySnapshot{}, err
	}
	kernelStore.mu.Lock()
	armed := kernelStore.armed
	kernelStore.mu.Unlock()
	if !armed {
		return snapshot, nil
	}
	kernelStore.once.Do(func() {
		event, eventErr := eventlog.Next(
			snapshot.Run,
			reducer.EventRunStateChanged,
			kernelStore.actor,
			snapshot.Run.Run.UpdatedAt.Add(time.Second),
			model.RunStateChangedPayload{
				From:   snapshot.Run.Run.State,
				To:     model.RunCancelled,
				Reason: "simulate a concurrent lifecycle decision",
			},
		)
		if eventErr == nil {
			_, eventErr = kernelStore.Store.AppendEvent(
				ctx,
				namespace,
				snapshot.Run.Run.EventSequence,
				event,
			)
		}
		kernelStore.mu.Lock()
		defer kernelStore.mu.Unlock()
		kernelStore.mutated = eventErr == nil
		kernelStore.mutationErr = eventErr
	})
	return snapshot, nil
}

func (kernelStore *runHeadChangingStore) Arm() {
	kernelStore.mu.Lock()
	defer kernelStore.mu.Unlock()
	kernelStore.armed = true
}

func (kernelStore *runHeadChangingStore) Mutated() bool {
	kernelStore.mu.Lock()
	defer kernelStore.mu.Unlock()
	return kernelStore.mutated
}

func (kernelStore *runHeadChangingStore) MutationError() error {
	kernelStore.mu.Lock()
	defer kernelStore.mu.Unlock()
	return kernelStore.mutationErr
}

func newExecutionAuthorityRaceAPIFixture(
	t *testing.T,
	suffix string,
	clock func() time.Time,
	decorateStore func(store.Store) store.Store,
) *executionAuthorityAPIFixture {
	t.Helper()
	fixture := newExecutionAuthorityServerFixtureWithOptions(
		t,
		suffix,
		clock().UTC(),
		clock,
		decorateStore,
	)
	admitted := admitExecutionAuthorityTask(
		t,
		fixture,
		suffix,
		model.ContractResource{
			ID: "workspace",
			Selector: model.ResourceSelector{
				Kind:    "filesystem",
				Pattern: "workspace/**",
			},
			Operations: []string{
				"filesystem.read",
				"filesystem.write",
			},
			Conditions: model.CapabilityConditions{
				WorkspaceRoot: "workspace",
			},
		},
	)
	fixture.prepareRunningTransaction(t, admitted.Transaction)
	if fixture.armAuthorityRace != nil {
		fixture.armAuthorityRace()
	}
	fixture.captureEventHeads(t)
	return fixture
}
