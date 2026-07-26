package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
	"github.com/duriantaco/vouch/internal/kernel/store"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
	"github.com/duriantaco/vouch/internal/kernel/transaction/gitstage"
)

func TestServerRunLifecycleAPI(t *testing.T) {
	t.Parallel()
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(kernelStore).Handler()

	created := readCreationEvent(t)
	response := requestJSON(t, handler, http.MethodPost, "/v0/runs", created)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
	}
	var projection reducer.Projection
	decodeResponse(t, response, &projection)

	event := stateEvent(t, projection, model.RunAdmitted, 2)
	appendRequest := appendEventRequest{ExpectedSequence: 1, Event: event}
	response = requestJSON(t, handler, http.MethodPost,
		"/v0/namespaces/fixture/runs/run:demo-001/events", appendRequest)
	if response.Code != http.StatusOK {
		t.Fatalf("append status = %d: %s", response.Code, response.Body.String())
	}
	decodeResponse(t, response, &projection)
	if projection.Run.State != model.RunAdmitted || projection.Run.EventSequence != 2 {
		t.Fatalf("unexpected projection: %#v", projection)
	}

	response = requestJSON(t, handler, http.MethodGet,
		"/v0/namespaces/fixture/runs/run:demo-001/events?after=1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("events status = %d: %s", response.Code, response.Body.String())
	}
	var events []model.RunEvent
	decodeResponse(t, response, &events)
	if len(events) != 1 || events[0].Sequence != 2 {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestProductionPolicyDisablesLegacyHostMutationAPIs(t *testing.T) {
	t.Parallel()
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(
		kernelStore,
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			AllowHost:          false,
			EnforcementProfile: "production",
		}),
	).Handler()

	for _, endpoint := range []string{
		"/v0/runs",
		"/v0/namespaces/fixture/runs/run:demo-001/events",
		"/v0/namespaces/fixture/runs/run:demo-001/capabilities",
		"/v0/namespaces/fixture/runs/run:demo-001/actions",
	} {
		response := requestJSON(
			t,
			handler,
			http.MethodPost,
			endpoint,
			map[string]any{},
		)
		if response.Code != http.StatusForbidden {
			t.Fatalf(
				"legacy host endpoint %s status=%d body=%s",
				endpoint,
				response.Code,
				response.Body.String(),
			)
		}
		var kernelErr model.KernelError
		decodeResponse(t, response, &kernelErr)
		if kernelErr.Code != model.ErrorCapabilityDenied ||
			kernelErr.Operation != "legacy_host_runtime" {
			t.Fatalf(
				"legacy host endpoint %s error=%#v",
				endpoint,
				kernelErr,
			)
		}
	}
}

func TestServerRejectsUnknownRequestFieldsAndPathMismatch(t *testing.T) {
	t.Parallel()
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(kernelStore).Handler()

	response := requestJSON(t, handler, http.MethodPost, "/v0/runs",
		map[string]any{"unexpected": true})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d", response.Code)
	}

	created := readCreationEvent(t)
	response = requestJSON(t, handler, http.MethodPost, "/v0/runs", created)
	var projection reducer.Projection
	decodeResponse(t, response, &projection)
	event := stateEvent(t, projection, model.RunAdmitted, 2)
	response = requestJSON(t, handler, http.MethodPost,
		"/v0/namespaces/fixture/runs/run:different/events",
		appendEventRequest{ExpectedSequence: 1, Event: event})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("path mismatch status = %d", response.Code)
	}

	forged := stateEvent(t, projection, model.RunAdmitted, 2)
	forged.Type = reducer.EventActionAuthorized
	forged.Payload = json.RawMessage(`{"action_id":"action:forged"}`)
	forged.Digest, err = model.ComputeEventDigest(forged)
	if err != nil {
		t.Fatal(err)
	}
	response = requestJSON(t, handler, http.MethodPost,
		"/v0/namespaces/fixture/runs/run:demo-001/events",
		appendEventRequest{ExpectedSequence: 1, Event: forged})
	if response.Code != http.StatusForbidden {
		t.Fatalf("forged privileged event status = %d: %s", response.Code, response.Body.String())
	}
}

func TestReadinessChecksStoreAndProductionVerifierConfiguration(t *testing.T) {
	t.Parallel()
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })

	readyHandler := NewServer(kernelStore).Handler()
	response := requestJSON(t, readyHandler, http.MethodGet, "/readyz", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("ready status=%d body=%s", response.Code, response.Body.String())
	}

	notReadyHandler := NewServer(
		kernelStore,
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			RequireVerifierProfiles: true,
		}),
	).Handler()
	response = requestJSON(t, notReadyHandler, http.MethodGet, "/readyz", nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf(
			"missing production verifier profiles readiness=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
}

func TestReadinessRejectsClosedStoreAndUnavailableRuntime(t *testing.T) {
	t.Parallel()

	closedStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	closedHandler := NewServer(closedStore).Handler()
	if err := closedStore.Close(); err != nil {
		t.Fatal(err)
	}
	response := requestJSON(
		t,
		closedHandler,
		http.MethodGet,
		"/readyz",
		nil,
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf(
			"closed store readiness=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}

	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	runtimeHandler := NewServer(
		kernelStore,
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			EnginePath: "/path/that/does/not/exist",
		}),
	).Handler()
	response = requestJSON(
		t,
		runtimeHandler,
		http.MethodGet,
		"/readyz",
		nil,
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf(
			"unavailable runtime readiness=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
}

func TestReadinessRetainsFullPinnedAgentImageReferences(t *testing.T) {
	t.Parallel()
	full := "registry.example.invalid/team/agent@sha256:" +
		strings.Repeat("a", 64)
	server := NewServer(
		nil,
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			AllowedAgentImages: []string{full},
			AllowedAgentImageDigests: map[string]struct{}{
				"sha256:" + strings.Repeat("a", 64): {},
			},
		}),
	)
	images := server.requiredRuntimeImages()
	if len(images) != 1 || images[0] != full {
		t.Fatalf("readiness images=%q, want full pinned reference %q", images, full)
	}
}

func TestProductionExecutionPolicyRejectsClientAssertedReceipts(t *testing.T) {
	t.Parallel()
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	manager, err := gitstage.New()
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer(
		kernelStore,
		WithTransactionRuntime(
			manager,
			t.TempDir(),
			t.TempDir(),
			transactionreducer.BaselinePolicy{},
		),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			AllowExternalExecution:    false,
			AllowExternalVerification: false,
		}),
	).Handler()
	base := "/v0/namespaces/team/transactions/tx:receipt"
	for _, endpoint := range []string{
		base + "/executions/start",
		base + "/executions/finish",
		base + "/verifications",
	} {
		response := requestJSON(t, handler, http.MethodPost, endpoint, map[string]any{})
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s status=%d, want %d: %s", endpoint, response.Code, http.StatusForbidden, response.Body.String())
		}
		var kernelError model.KernelError
		decodeResponse(t, response, &kernelError)
		if kernelError.Code != model.ErrorCapabilityDenied {
			t.Fatalf("%s error=%s, want %s", endpoint, kernelError.Code, model.ErrorCapabilityDenied)
		}
	}
}

func TestOCIWorkloadAdmissionIsGloballyBounded(t *testing.T) {
	t.Parallel()
	server := NewServer(
		nil,
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			MaxConcurrentWorkloads: 1,
		}),
	)
	release, err := server.acquireWorkloadSlot("tx:first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.acquireWorkloadSlot("tx:second"); err == nil {
		t.Fatal("concurrent OCI workload exceeded global admission limit")
	} else {
		var kernelError *model.KernelError
		if !errors.As(err, &kernelError) ||
			kernelError.Code != model.ErrorBudgetExceeded {
			t.Fatalf("admission error=%v", err)
		}
	}
	release()
	reacquired, err := server.acquireWorkloadSlot("tx:third")
	if err != nil {
		t.Fatalf("released OCI slot was not reusable: %v", err)
	}
	reacquired()
}

func TestResourceLocksSerializeAndReleaseMapEntries(t *testing.T) {
	t.Parallel()
	server := NewServer(nil)
	first := server.runLock("team", "tx:one")
	second := server.runLock("team", "tx:one")
	first.Lock()
	acquired := make(chan struct{})
	done := make(chan struct{})
	go func() {
		second.Lock()
		close(acquired)
		second.Unlock()
		close(done)
	}()
	select {
	case <-acquired:
		t.Fatal("same-resource lock did not serialize callers")
	case <-time.After(25 * time.Millisecond):
	}
	first.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiting resource lock did not complete")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.locks) != 0 {
		t.Fatalf("released resource locks retained %d map entries", len(server.locks))
	}
}

func readCreationEvent(t *testing.T) model.RunEvent {
	t.Helper()
	path := filepath.Join("..", "..", "..", "schemas", "fixtures", "kernel", "valid", "run_event.json")
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

func stateEvent(t *testing.T, projection reducer.Projection, to model.RunState, sequence int64) model.RunEvent {
	t.Helper()
	payload, err := json.Marshal(model.RunStateChangedPayload{From: projection.Run.State, To: to})
	if err != nil {
		t.Fatal(err)
	}
	event := model.RunEvent{
		Version:        model.RunEventVersion,
		ID:             fmt.Sprintf("event:api:%d", sequence),
		RunID:          projection.Run.ID,
		Sequence:       sequence,
		Type:           reducer.EventRunStateChanged,
		Actor:          model.Principal{ID: "service:vouchd", Kind: model.PrincipalService},
		OccurredAt:     projection.Run.UpdatedAt.Add(time.Second),
		Payload:        payload,
		PreviousDigest: projection.LastEventDigest,
	}
	event.Digest, err = model.ComputeEventDigest(event)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func requestJSON(t *testing.T, handler http.Handler, method, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if value != nil {
		if err := json.NewEncoder(&body).Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequestWithContext(context.Background(), method, path, &body)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}
