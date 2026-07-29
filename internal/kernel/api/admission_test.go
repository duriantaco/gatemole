package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/admission"
	"github.com/duriantaco/vouch/internal/kernel/identity"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/store"
)

func TestTaskAdmissionCreatesAtomicAuthorityAndReplaysIdempotently(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 8, 30, 0, 0, time.UTC)
	imageDigest := testDigest("b")
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(
		kernelStore,
		WithClock(func() time.Time { return now }),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			AllowedAgentImageDigests: map[string]struct{}{imageDigest: {}},
		}),
	).Handler()
	request := validTaskAdmissionRequest(imageDigest)
	path := "/v0/namespaces/engineering/task-admissions"

	response := requestJSON(t, handler, http.MethodPost, path, request)
	if response.Code != http.StatusCreated {
		t.Fatalf(
			"create status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var created admission.Result
	decodeResponse(t, response, &created)
	if created.Version != admission.ResultVersion ||
		created.Task.Intent != request.Intent ||
		created.Run.Run.State != model.RunAdmitted ||
		created.Run.Run.EventSequence != 3 ||
		created.Transaction.Transaction.Admission == nil {
		t.Fatalf("unexpected admission result: %#v", created)
	}
	if err := kernelStore.VerifyRun(
		context.Background(),
		"engineering",
		created.Run.Run.ID,
	); err != nil {
		t.Fatalf("verify admitted run: %v", err)
	}
	if err := kernelStore.VerifyTransaction(
		context.Background(),
		"engineering",
		created.Transaction.Transaction.ID,
	); err != nil {
		t.Fatalf("verify admitted transaction: %v", err)
	}

	response = requestJSON(t, handler, http.MethodPost, path, request)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"replay status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var replayed admission.Result
	decodeResponse(t, response, &replayed)
	if !reflect.DeepEqual(replayed, created) {
		t.Fatalf("idempotent replay changed the result:\ncreated=%#v\nreplayed=%#v", created, replayed)
	}

	changed := request
	changed.Intent = "A different task must not reuse this admission key."
	response = requestJSON(t, handler, http.MethodPost, path, changed)
	if response.Code != http.StatusConflict {
		t.Fatalf(
			"changed replay status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var conflict model.KernelError
	decodeResponse(t, response, &conflict)
	if conflict.Code != model.ErrorIdempotencyConflict {
		t.Fatalf("conflict code=%q", conflict.Code)
	}
}

func TestTaskAdmissionAppliesRuntimePolicyOnlyToNewAuthority(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 9, 0, 0, 0, time.UTC)
	allowedDigest := testDigest("b")
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	allowedHandler := NewServer(
		kernelStore,
		WithClock(func() time.Time { return now }),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			AllowedAgentImageDigests: map[string]struct{}{allowedDigest: {}},
		}),
	).Handler()
	request := validTaskAdmissionRequest(allowedDigest)
	path := "/v0/namespaces/engineering/task-admissions"
	response := requestJSON(
		t,
		allowedHandler,
		http.MethodPost,
		path,
		request,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf(
			"initial status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}

	deniedHandler := NewServer(
		kernelStore,
		WithClock(func() time.Time { return now.Add(time.Hour) }),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			AllowedAgentImageDigests: map[string]struct{}{testDigest("d"): {}},
		}),
	).Handler()
	response = requestJSON(t, deniedHandler, http.MethodPost, path, request)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"existing admission should replay despite changed policy: status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}

	newRequest := validTaskAdmissionRequest(allowedDigest)
	newRequest.IdempotencyKey = "admission:new-denied-image"
	newRequest.TransactionID = "tx:new-denied-image"
	response = requestJSON(
		t,
		deniedHandler,
		http.MethodPost,
		path,
		newRequest,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"disallowed image status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var denied model.KernelError
	decodeResponse(t, response, &denied)
	if denied.Code != model.ErrorCapabilityDenied {
		t.Fatalf("disallowed image code=%q", denied.Code)
	}

	hostRequest := validTaskAdmissionRequest(allowedDigest)
	hostRequest.IdempotencyKey = "admission:new-host"
	hostRequest.TransactionID = "tx:new-host"
	hostRequest.AgentProfile.RuntimeClass = "host"
	hostRequest.AgentProfile.ImageDigest = ""
	response = requestJSON(
		t,
		deniedHandler,
		http.MethodPost,
		path,
		hostRequest,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"disabled host status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
}

func TestProductionRejectsLegacyTransactionCreation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 9, 15, 0, 0, time.UTC)
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	prepared, err := admission.Prepare(
		"engineering",
		validTaskAdmissionRequest(testDigest("b")),
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewServer(
		kernelStore,
		WithClock(func() time.Time { return now }),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			EnforcementProfile: store.EnforcementProfileProduction,
		}),
	).Handler()
	response := requestJSON(
		t,
		handler,
		http.MethodPost,
		"/v0/transactions",
		prepared.TransactionEvent,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"legacy production creation status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	if _, err := kernelStore.GetTransaction(
		context.Background(),
		"engineering",
		prepared.Result.Transaction.Transaction.ID,
	); !isNotFound(err) {
		t.Fatalf("legacy production creation persisted authority: %v", err)
	}
}

func TestTaskAdmissionBindsAuthenticatedOperatorToBothLedgers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 9, 30, 0, 0, time.UTC)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwk, err := identity.Ed25519JWK("key:task-admission", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := identity.NewVerifier(identity.TrustDocument{
		Version:                 identity.TrustDocumentVersion,
		Issuer:                  "https://issuer.example.invalid",
		Audiences:               []string{"vouch-api"},
		MaxTokenLifetimeSeconds: 3600,
		JWKS:                    identity.JWKS{Keys: []identity.JWK{jwk}},
	})
	if err != nil {
		t.Fatal(err)
	}
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	imageDigest := testDigest("b")
	handler := NewServer(
		kernelStore,
		WithClock(func() time.Time { return now }),
		WithIdentityVerifier(verifier),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			AllowedAgentImageDigests: map[string]struct{}{imageDigest: {}},
		}),
	).Handler()
	request := validTaskAdmissionRequest(imageDigest)
	request.Actor = model.Principal{
		ID: "operator:alice", Kind: model.PrincipalOperator,
	}
	token := apiIdentityToken(
		t,
		privateKey,
		"key:task-admission",
		now,
		"operator:alice",
		model.PrincipalOperator,
		[]string{"engineering"},
		[]string{"viewer", "operator"},
	)
	response := requestJSONWithToken(
		t,
		handler,
		http.MethodPost,
		"/v0/namespaces/engineering/task-admissions",
		request,
		token,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf(
			"authenticated admission status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var result admission.Result
	decodeResponse(t, response, &result)
	runEvents, err := kernelStore.Events(
		context.Background(),
		"engineering",
		result.Run.Run.ID,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	transactionEvents, err := kernelStore.TransactionEvents(
		context.Background(),
		"engineering",
		result.Transaction.Transaction.ID,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []model.Principal{
		runEvents[0].Actor,
		transactionEvents[0].Actor,
	} {
		if actor.ID != "operator:alice" ||
			actor.Issuer != "https://issuer.example.invalid" ||
			!model.IsSHA256Digest(actor.ClaimsDigest) {
			t.Fatalf("ledger actor was not bound to authenticated claims: %#v", actor)
		}
	}
}

func validTaskAdmissionRequest(imageDigest string) admission.Request {
	maxToolCalls := int64(100)
	maxWallTime := int64(1800)
	return admission.Request{
		Version:        admission.RequestVersion,
		IdempotencyKey: "admission:engineering-task",
		TransactionID:  "tx:engineering-task",
		Intent:         "Update the service inside its isolated workspace.",
		AgentProfile: model.AgentTaskProfileBinding{
			ID:            "agent-profile:engineering",
			Digest:        testDigest("a"),
			RuntimeClass:  "oci",
			ImageDigest:   imageDigest,
			CommandDigest: testDigest("c"),
		},
		Sponsor: model.Principal{
			ID: "human:sponsor", Kind: model.PrincipalHuman,
		},
		Actor: model.Principal{
			ID: "operator:supervisor", Kind: model.PrincipalOperator,
		},
		Contract: admission.ContractSpec{
			Risk: "medium",
			Resources: []model.ContractResource{{
				ID: "transaction-workspace",
				Selector: model.ResourceSelector{
					Kind: "filesystem", Pattern: "workspace/**",
				},
				Operations: []string{
					"filesystem.read",
					"filesystem.write",
				},
				Conditions: model.CapabilityConditions{
					WorkspaceRoot: "workspace",
				},
			}},
			Budgets: model.BudgetLimits{
				MaxToolCalls:       &maxToolCalls,
				MaxWallTimeSeconds: &maxWallTime,
			},
		},
	}
}
