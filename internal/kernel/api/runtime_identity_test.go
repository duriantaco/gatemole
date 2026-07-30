package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/runtimeidentity"
	"github.com/duriantaco/vouch/internal/kernel/store"
)

func TestRuntimeHeaderRejectsWrongDaemonBeforeMutation(t *testing.T) {
	t.Parallel()
	runtimeID := "runtime:" + strings.Repeat("a", 64)
	otherRuntimeID := "runtime:" + strings.Repeat("b", 64)
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(
		kernelStore,
		WithRuntimeIdentity(runtimeID),
	).Handler()

	response := requestJSONWithRuntimeHeader(
		t,
		handler,
		http.MethodPost,
		"/v0/transactions",
		map[string]any{"malformed": true},
		otherRuntimeID,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"wrong Runtime header status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var denied model.KernelError
	decodeResponse(t, response, &denied)
	if denied.Code != model.ErrorCapabilityDenied ||
		denied.Operation != "bind_runtime_request" {
		t.Fatalf("wrong Runtime header error=%#v", denied)
	}
	transactions, err := kernelStore.ListTransactions(
		context.Background(),
		"local",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 0 {
		t.Fatalf("wrong Runtime request mutated ledger: %#v", transactions)
	}

	response = requestJSONWithRuntimeHeader(
		t,
		handler,
		http.MethodPost,
		"/v0/transactions",
		map[string]any{"malformed": true},
		"",
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"missing Runtime header status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	decodeResponse(t, response, &denied)
	if denied.Code != model.ErrorCapabilityDenied ||
		denied.Operation != "bind_runtime_request" {
		t.Fatalf("missing Runtime header error=%#v", denied)
	}
	transactions, err = kernelStore.ListTransactions(
		context.Background(),
		"local",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(transactions) != 0 {
		t.Fatalf("missing Runtime request mutated ledger: %#v", transactions)
	}

	response = requestJSONWithRuntimeHeader(
		t,
		handler,
		http.MethodPost,
		"/v0/transactions",
		map[string]any{"malformed": true},
		runtimeID,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"matching Runtime header status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	decodeResponse(t, response, &denied)
	if denied.Code != model.ErrorCapabilityDenied ||
		denied.Operation != "legacy_transaction_creation" {
		t.Fatalf(
			"matching Runtime header did not reach endpoint guard: %#v",
			denied,
		)
	}
}

func TestConfiguredRuntimeRejectsRawRunCreationBeforeMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runtimeID := "runtime:" + strings.Repeat("c", 64)
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	if err := kernelStore.BindRuntimeMetadata(
		ctx,
		runtimeID,
		store.EnforcementProfileDevelopment,
	); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(
		kernelStore,
		WithRuntimeIdentity(runtimeID),
	).Handler()

	response := requestJSONWithRuntimeHeader(
		t,
		handler,
		http.MethodPost,
		"/v0/runs",
		readCreationEvent(t),
		runtimeID,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"raw run creation status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var denied model.KernelError
	decodeResponse(t, response, &denied)
	if denied.Code != model.ErrorCapabilityDenied ||
		denied.Operation != "legacy_run_creation" {
		t.Fatalf("raw run creation error=%#v", denied)
	}
	runs, err := kernelStore.ListRuns(ctx, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("raw run creation changed the ledger: %#v", runs)
	}
}

func TestInvalidConfiguredRuntimeIdentityFailsClosed(t *testing.T) {
	t.Parallel()
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(
		kernelStore,
		WithRuntimeIdentity("runtime:not-canonical"),
	).Handler()

	response := requestJSONWithRuntimeHeader(
		t,
		handler,
		http.MethodGet,
		"/v0/namespaces/local/transactions",
		nil,
		"",
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf(
			"invalid configured Runtime status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var unavailable model.KernelError
	decodeResponse(t, response, &unavailable)
	if unavailable.Code != model.ErrorDriverUnavailable ||
		unavailable.Operation != "bind_runtime_request" {
		t.Fatalf("invalid configured Runtime error=%#v", unavailable)
	}
}

func TestUnconfiguredRuntimeIdentityRetainsEmbeddedCompatibility(t *testing.T) {
	t.Parallel()
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(kernelStore).Handler()

	response := requestJSONWithRuntimeHeader(
		t,
		handler,
		http.MethodGet,
		"/v0/namespaces/local/transactions",
		nil,
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"unconfigured embedded Runtime status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
}

func requestJSONWithRuntimeHeader(
	t *testing.T,
	handler http.Handler,
	method string,
	path string,
	value any,
	runtimeID string,
) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if value != nil {
		if err := json.NewEncoder(&body).Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequestWithContext(
		context.Background(),
		method,
		path,
		&body,
	)
	request.Header.Set("Content-Type", "application/json")
	if runtimeID != "" {
		request.Header.Set(runtimeidentity.HTTPHeader, runtimeID)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
