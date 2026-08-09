package client

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/admission"
	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/runtimeidentity"
	"github.com/duriantaco/gatemole/internal/kernel/runtimepreflight"
	"github.com/duriantaco/gatemole/internal/kernel/transaction/gitstage"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestClientSendsExplicitBearerToken(t *testing.T) {
	t.Parallel()
	var authorization string
	var runtimeHeader string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		authorization = request.Header.Get("Authorization")
		runtimeHeader = request.Header.Get(runtimeidentity.HTTPHeader)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("[]")),
			Request:    request,
		}, nil
	})
	runtimeID := "runtime:" + strings.Repeat("9", 64)
	client := NewWithTransport(transport).
		WithBearerToken("signed-token").
		WithExpectedRuntimeID(runtimeID)
	if _, err := client.ListRuns(context.Background(), "team-a"); err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer signed-token" {
		t.Fatalf("authorization=%q", authorization)
	}
	if runtimeHeader != runtimeID {
		t.Fatalf("Runtime header=%q, want %q", runtimeHeader, runtimeID)
	}
}

func TestClientGetTransactionDiffValidatesExactBodyAndBinding(t *testing.T) {
	t.Parallel()
	patch := []byte("diff --git a/a.txt b/a.txt\n+reviewed\n")
	sum := sha256.Sum256(patch)
	metadata := gitstage.DiffMetadata{
		Version:           gitstage.DiffMetadataVersion,
		Namespace:         "team/platform",
		TransactionID:     "tx:review",
		Attempt:           2,
		EventSequence:     14,
		StagedStateDigest: "sha256:" + strings.Repeat("a", 64),
		EffectSetDigest:   "sha256:" + strings.Repeat("b", 64),
		PatchDigest:       "sha256:" + hex.EncodeToString(sum[:]),
		BaseRevision:      strings.Repeat("c", 40),
		TreeRevision:      strings.Repeat("d", 40),
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.EscapedPath() !=
			"/v0/namespaces/team%2Fplatform/transactions/tx:review/diff" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.EscapedPath())
		}
		header := make(http.Header)
		header.Set(
			gitstage.DiffMetadataHeader,
			base64.RawURLEncoding.EncodeToString(metadataJSON),
		)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     header,
			Body:       io.NopCloser(strings.NewReader(string(patch))),
			Request:    request,
		}, nil
	})
	result, err := NewWithTransport(transport).GetTransactionDiff(
		context.Background(), "team/platform", "tx:review",
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Patch) != string(patch) || !reflect.DeepEqual(result.Metadata, metadata) {
		t.Fatalf("diff result=%#v", result)
	}
}

func TestClientGetTransactionDiffRejectsBodyOutsideDigest(t *testing.T) {
	t.Parallel()
	metadata := gitstage.DiffMetadata{
		Version:           gitstage.DiffMetadataVersion,
		Namespace:         "local",
		TransactionID:     "tx:review",
		Attempt:           1,
		EventSequence:     2,
		StagedStateDigest: "sha256:" + strings.Repeat("a", 64),
		EffectSetDigest:   "sha256:" + strings.Repeat("b", 64),
		PatchDigest:       "sha256:" + strings.Repeat("c", 64),
		BaseRevision:      strings.Repeat("d", 40),
		TreeRevision:      strings.Repeat("e", 40),
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set(
			gitstage.DiffMetadataHeader,
			base64.RawURLEncoding.EncodeToString(metadataJSON),
		)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     header,
			Body:       io.NopCloser(strings.NewReader("tampered")),
			Request:    request,
		}, nil
	})
	_, err = NewWithTransport(transport).GetTransactionDiff(
		context.Background(), "local", "tx:review",
	)
	if err == nil || !strings.Contains(err.Error(), "patch digest") {
		t.Fatalf("tampered diff body was accepted: %v", err)
	}
}

func TestClientAdmitTaskUsesNamespaceEndpoint(t *testing.T) {
	t.Parallel()
	input := validClientAdmissionRequest()
	prepared, err := admission.Prepare(
		"team/platform",
		input,
		time.Date(2026, 7, 29, 4, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := prepared.Result
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost {
			t.Fatalf("method=%q", request.Method)
		}
		if request.URL.EscapedPath() !=
			"/v0/namespaces/team%2Fplatform/task-admissions" {
			t.Fatalf("path=%q", request.URL.EscapedPath())
		}
		var got admission.Request
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, input) {
			t.Fatalf("request=%#v, want %#v", got, input)
		}
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Status:     "201 Created",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(string(data))),
			Request:    request,
		}, nil
	})
	client := NewWithTransport(transport)
	got, err := client.AdmitTask(
		context.Background(),
		"team/platform",
		input,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result=%#v, want %#v", got, want)
	}
}

func TestClientAdmitTaskRejectsInternallyValidUnrelatedAuthority(
	t *testing.T,
) {
	t.Parallel()
	input := validClientAdmissionRequest()
	unrelated := input
	unrelated.Intent = "Authorize unrelated caller-owned work."
	prepared, err := admission.Prepare(
		"team/platform",
		unrelated,
		time.Date(2026, 7, 29, 4, 5, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	result := prepared.Result
	result.RequestDigest, err = admission.ComputeRequestDigest(
		"team/platform",
		input,
	)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusCreated,
			Status:     "201 Created",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(string(data))),
			Request:    request,
		}, nil
	})

	_, err = NewWithTransport(transport).AdmitTask(
		context.Background(),
		"team/platform",
		input,
	)
	if err == nil || !strings.Contains(err.Error(), "caller-owned") {
		t.Fatalf("unrelated admission authority was accepted: %v", err)
	}
}

func validClientAdmissionRequest() admission.Request {
	return admission.Request{
		Version:                    admission.RequestVersion,
		ExpectedRuntimeID:          "runtime:" + strings.Repeat("a", 64),
		ExpectedEnforcementProfile: "development",
		IdempotencyKey:             "admission:client-test",
		TransactionID:              "tx:client-test",
		Intent:                     "Exercise the task admission client.",
		AgentProfile: model.AgentTaskProfileBinding{
			ID:            "agent-profile:client-test",
			Digest:        "sha256:" + strings.Repeat("b", 64),
			RuntimeClass:  "oci",
			ImageDigest:   "sha256:" + strings.Repeat("c", 64),
			Entrypoint:    "/opt/agent",
			CommandDigest: "sha256:" + strings.Repeat("d", 64),
		},
		Sponsor: model.Principal{
			ID:   "human:client-sponsor",
			Kind: model.PrincipalHuman,
		},
		Actor: model.Principal{
			ID:   "operator:client",
			Kind: model.PrincipalOperator,
		},
		Contract: admission.ContractSpec{
			Risk: "low",
			Resources: []model.ContractResource{{
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
			}},
			Budgets: model.BudgetLimits{},
		},
	}
}

func TestClientPreflightRuntimeBindsResponseToRequest(t *testing.T) {
	t.Parallel()
	runtimeID := "runtime:" + strings.Repeat("a", 64)
	request := runtimepreflight.Request{
		Version:           runtimepreflight.RequestVersion,
		ExpectedRuntimeID: runtimeID,
	}
	response := runtimepreflight.Result{
		Version:            runtimepreflight.ResultVersion,
		RuntimeID:          "runtime:" + strings.Repeat("b", 64),
		EnforcementProfile: "development",
		Ready:              true,
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(httpRequest *http.Request) (*http.Response, error) {
		if httpRequest.URL.EscapedPath() !=
			"/v0/namespaces/team%2Fplatform/runtime/preflight" {
			t.Fatalf("path=%q", httpRequest.URL.EscapedPath())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(string(data))),
			Request:    httpRequest,
		}, nil
	})

	_, err = NewWithTransport(transport).PreflightRuntime(
		context.Background(),
		"team/platform",
		request,
	)
	if err == nil || !strings.Contains(err.Error(), "does not match request") {
		t.Fatalf("mismatched Runtime response was accepted: %v", err)
	}
}

func TestClientPreflightRuntimeRejectsInvalidRequestBeforeTransport(t *testing.T) {
	t.Parallel()
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid preflight request reached transport")
		return nil, nil
	})
	_, err := NewWithTransport(transport).PreflightRuntime(
		context.Background(),
		"local",
		runtimepreflight.Request{Version: runtimepreflight.RequestVersion},
	)
	if err == nil || !strings.Contains(err.Error(), "preflight request") {
		t.Fatalf("invalid preflight request was accepted: %v", err)
	}
}

func TestClientPreflightRuntimeRejectsEnforcementProfileMismatch(t *testing.T) {
	t.Parallel()
	runtimeID := "runtime:" + strings.Repeat("a", 64)
	response := runtimepreflight.Result{
		Version:            runtimepreflight.ResultVersion,
		RuntimeID:          runtimeID,
		EnforcementProfile: "development",
		Ready:              true,
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(string(data))),
			Request:    request,
		}, nil
	})

	_, err = NewWithTransport(transport).PreflightRuntime(
		context.Background(),
		"local",
		runtimepreflight.Request{
			Version:                    runtimepreflight.RequestVersion,
			ExpectedRuntimeID:          runtimeID,
			RequiredEnforcementProfile: "production",
		},
	)
	if err == nil || !strings.Contains(err.Error(), "enforcement profile") {
		t.Fatalf("mismatched enforcement profile was accepted: %v", err)
	}
}
