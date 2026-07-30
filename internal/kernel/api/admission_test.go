package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/admission"
	"github.com/duriantaco/vouch/internal/kernel/broker"
	"github.com/duriantaco/vouch/internal/kernel/identity"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/reducer"
	"github.com/duriantaco/vouch/internal/kernel/store"
	transactionreducer "github.com/duriantaco/vouch/internal/kernel/transaction"
	"github.com/duriantaco/vouch/internal/kernel/transaction/gitstage"
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
	if created.Version != admission.LegacyResultVersion ||
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

func TestTaskAdmissionRequiresExactConfiguredRuntimeIdentity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 9, 10, 0, 0, time.UTC)
	runtimeID := apiTestRuntimeID("0")
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	if err := kernelStore.BindRuntimeMetadata(
		context.Background(),
		runtimeID,
		store.EnforcementProfileDevelopment,
	); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(
		kernelStore,
		WithClock(func() time.Time { return now }),
		WithRuntimeIdentity(runtimeID),
	).Handler()
	path := "/v0/namespaces/engineering/task-admissions"
	request := validTaskAdmissionRequest(testDigest("b"))
	request.Version = admission.RequestVersion
	request.ExpectedRuntimeID = runtimeID
	request.ExpectedEnforcementProfile =
		store.EnforcementProfileDevelopment

	response := requestJSON(t, handler, http.MethodPost, path, request)
	if response.Code != http.StatusCreated {
		t.Fatalf(
			"matching Runtime status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var result admission.Result
	decodeResponse(t, response, &result)
	if result.RuntimeID != runtimeID ||
		result.EnforcementProfile !=
			store.EnforcementProfileDevelopment ||
		result.Transaction.Transaction.Admission == nil ||
		result.Transaction.Transaction.Admission.RuntimeID != runtimeID ||
		result.Transaction.Transaction.Admission.EnforcementProfile !=
			store.EnforcementProfileDevelopment {
		t.Fatalf("admission Runtime binding is incomplete: %#v", result)
	}
	capabilityResponse := requestJSONWithRuntimeHeader(
		t,
		handler,
		http.MethodPost,
		"/v0/namespaces/engineering/runs/"+
			result.Run.Run.ID+"/capabilities",
		compileCapabilitiesRequest{
			ExpectedSequence: result.Run.Run.EventSequence,
		},
		runtimeID,
	)
	if capabilityResponse.Code != http.StatusForbidden {
		t.Fatalf(
			"v1 legacy capability route status=%d body=%s",
			capabilityResponse.Code,
			capabilityResponse.Body.String(),
		)
	}
	var capabilityDenied model.KernelError
	decodeResponse(t, capabilityResponse, &capabilityDenied)
	if capabilityDenied.Code != model.ErrorCapabilityDenied ||
		capabilityDenied.Operation != "legacy_capability_compilation" {
		t.Fatalf(
			"v1 legacy capability route error=%#v",
			capabilityDenied,
		)
	}
	unchangedRun, err := kernelStore.GetRun(
		context.Background(),
		"engineering",
		result.Run.Run.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(unchangedRun, result.Run) {
		t.Fatal("legacy capability route changed v1 admitted authority")
	}

	wrong := request
	wrong.ExpectedRuntimeID = apiTestRuntimeID("f")
	response = requestJSON(t, handler, http.MethodPost, path, wrong)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"wrong Runtime replay status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var denied model.KernelError
	decodeResponse(t, response, &denied)
	if denied.Code != model.ErrorCapabilityDenied {
		t.Fatalf("wrong Runtime error=%q", denied.Code)
	}

	wrongProfile := request
	wrongProfile.IdempotencyKey = "admission:wrong-profile"
	wrongProfile.TransactionID = "tx:wrong-profile"
	wrongProfile.ExpectedEnforcementProfile =
		store.EnforcementProfileProduction
	response = requestJSON(
		t,
		handler,
		http.MethodPost,
		path,
		wrongProfile,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"wrong profile status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	if _, _, err := kernelStore.GetTaskAdmission(
		context.Background(),
		"engineering",
		wrongProfile.IdempotencyKey,
	); !isNotFound(err) {
		t.Fatalf("wrong profile admission changed the ledger: %v", err)
	}

	missing := validTaskAdmissionRequest(testDigest("b"))
	missing.Version = admission.RequestVersion
	missing.IdempotencyKey = "admission:missing-runtime"
	missing.TransactionID = "tx:missing-runtime"
	response = requestJSON(t, handler, http.MethodPost, path, missing)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"missing Runtime status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	if _, _, err := kernelStore.GetTaskAdmission(
		context.Background(),
		"engineering",
		missing.IdempotencyKey,
	); !isNotFound(err) {
		t.Fatalf("missing Runtime admission changed the ledger: %v", err)
	}
}

func TestConfiguredDevelopmentRuntimeReplaysButDoesNotCreateLegacyV0Admission(
	t *testing.T,
) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 9, 12, 0, 0, time.UTC)
	runtimeID := apiTestRuntimeID("1")
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })

	request := validTaskAdmissionRequest(testDigest("b"))
	prepared, err := admission.Prepare("engineering", request, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := kernelStore.AdmitTask(
		ctx,
		prepared,
	); err != nil || !created {
		t.Fatalf("seed legacy admission=(created=%t, err=%v)", created, err)
	}
	if err := kernelStore.BindRuntimeMetadata(
		ctx,
		runtimeID,
		store.EnforcementProfileDevelopment,
	); err != nil {
		t.Fatalf("adopt legacy development ledger: %v", err)
	}
	handler := NewServer(
		kernelStore,
		WithClock(func() time.Time { return now.Add(time.Minute) }),
		WithRuntimeIdentity(runtimeID),
	).Handler()
	path := "/v0/namespaces/engineering/task-admissions"

	response := requestJSON(t, handler, http.MethodPost, path, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"headerless legacy replay status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var denied model.KernelError
	decodeResponse(t, response, &denied)
	if denied.Code != model.ErrorCapabilityDenied ||
		denied.Operation != "admit_task" {
		t.Fatalf("headerless legacy replay error=%#v", denied)
	}

	response = requestJSONWithRuntimeHeader(
		t,
		handler,
		http.MethodPost,
		path,
		request,
		runtimeID,
	)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"legacy replay status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	var replayed admission.Result
	decodeResponse(t, response, &replayed)
	if replayed.Version != admission.LegacyResultVersion ||
		replayed.RuntimeID != "" ||
		replayed.RequestDigest != prepared.RequestDigest {
		t.Fatalf("legacy replay changed authority: %#v", replayed)
	}

	newLegacy := request
	newLegacy.IdempotencyKey = "admission:new-legacy"
	newLegacy.TransactionID = "tx:new-legacy"
	response = requestJSONWithRuntimeHeader(
		t,
		handler,
		http.MethodPost,
		path,
		newLegacy,
		runtimeID,
	)
	if response.Code != http.StatusConflict {
		t.Fatalf(
			"new legacy status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}
	if _, _, err := kernelStore.GetTaskAdmission(
		ctx,
		"engineering",
		newLegacy.IdempotencyKey,
	); !isNotFound(err) {
		t.Fatalf("new legacy request created authority: %v", err)
	}
}

func TestConfiguredRuntimeKeepsLegacyAdmissionReadOnlyAcrossMutationPaths(
	t *testing.T,
) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 27, 9, 13, 0, 0, time.UTC)
	runtimeID := apiTestRuntimeID("2")
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
	legacy, created, err := kernelStore.AdmitTask(ctx, prepared)
	if err != nil || !created {
		t.Fatalf("seed legacy admission: created=%t err=%v", created, err)
	}
	if err := kernelStore.BindRuntimeMetadata(
		ctx,
		runtimeID,
		store.EnforcementProfileDevelopment,
	); err != nil {
		t.Fatal(err)
	}
	repository := t.TempDir()
	actionBroker, err := broker.New(kernelStore, repository)
	if err != nil {
		t.Fatal(err)
	}
	stageManager, err := gitstage.New()
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	handler := NewServer(
		kernelStore,
		WithRuntimeIdentity(runtimeID),
		WithBroker(actionBroker),
		WithExecutionRuntimePolicy(ExecutionRuntimePolicy{
			AllowHost:                  true,
			AllowExternalExecution:     true,
			AllowExternalVerification:  true,
			EnginePath:                 "/bin/false",
			MaxAgentTimeoutSecs:        1,
			MaxVerificationTimeoutSecs: 1,
			EnforcementProfile:         store.EnforcementProfileDevelopment,
		}),
		WithTransactionRuntime(
			stageManager,
			repository,
			staging,
			transactionreducer.BaselinePolicy{},
		),
	).Handler()
	runID := legacy.Run.Run.ID
	transactionID := legacy.Transaction.Transaction.ID
	beforeRun := legacy.Run
	beforeTransaction := legacy.Transaction

	rawTransactionResponse := requestJSONWithRuntimeHeader(
		t,
		handler,
		http.MethodPost,
		"/v0/transactions",
		prepared.TransactionEvent,
		runtimeID,
	)
	if rawTransactionResponse.Code != http.StatusForbidden {
		t.Fatalf(
			"raw transaction status=%d body=%s",
			rawTransactionResponse.Code,
			rawTransactionResponse.Body.String(),
		)
	}
	var rawTransactionDenied model.KernelError
	decodeResponse(t, rawTransactionResponse, &rawTransactionDenied)
	if rawTransactionDenied.Code != model.ErrorCapabilityDenied ||
		rawTransactionDenied.Operation != "legacy_transaction_creation" {
		t.Fatalf(
			"raw transaction authority error=%#v",
			rawTransactionDenied,
		)
	}

	runMutations := []struct {
		name string
		path string
		body any
	}{
		{
			name: "run event",
			path: "/v0/namespaces/engineering/runs/" + runID + "/events",
			body: appendEventRequest{
				ExpectedSequence: legacy.Run.Run.EventSequence,
				Event: model.RunEvent{
					RunID: runID,
					Type:  reducer.EventRunStateChanged,
				},
			},
		},
		{
			name: "action",
			path: "/v0/namespaces/engineering/runs/" + runID + "/actions",
			body: broker.ExecuteRequest{
				ExpectedSequence: legacy.Run.Run.EventSequence,
				Action: model.ActionRequest{
					RunID: runID,
				},
			},
		},
	}
	for _, mutation := range runMutations {
		response := requestJSONWithRuntimeHeader(
			t,
			handler,
			http.MethodPost,
			mutation.path,
			mutation.body,
			runtimeID,
		)
		assertLegacyAuthorityDenied(t, response, mutation.name)
	}
	capabilityResponse := requestJSONWithRuntimeHeader(
		t,
		handler,
		http.MethodPost,
		"/v0/namespaces/engineering/runs/"+runID+"/capabilities",
		compileCapabilitiesRequest{
			ExpectedSequence: legacy.Run.Run.EventSequence,
		},
		runtimeID,
	)
	if capabilityResponse.Code != http.StatusForbidden {
		t.Fatalf(
			"legacy capability compilation status=%d body=%s",
			capabilityResponse.Code,
			capabilityResponse.Body.String(),
		)
	}
	var capabilityDenied model.KernelError
	decodeResponse(t, capabilityResponse, &capabilityDenied)
	if capabilityDenied.Code != model.ErrorCapabilityDenied ||
		capabilityDenied.Operation != "legacy_capability_compilation" {
		t.Fatalf(
			"legacy capability compilation error=%#v",
			capabilityDenied,
		)
	}

	actor := model.Principal{
		ID:   "operator:legacy-check",
		Kind: model.PrincipalOperator,
	}
	mutation := transactionMutationRequest{
		ExpectedSequence: legacy.Transaction.Transaction.EventSequence,
		Actor:            actor,
	}
	transactionPath := "/v0/namespaces/engineering/transactions/" +
		transactionID
	transactionMutations := []struct {
		name   string
		suffix string
		body   any
	}{
		{name: "start", suffix: "/start", body: mutation},
		{
			name:   "worktree creation",
			suffix: "/git-worktree",
			body: createWorktreeRequest{
				ExpectedSequence: mutation.ExpectedSequence,
				Actor:            actor,
			},
		},
		{
			name:   "external execution start",
			suffix: "/executions/start",
			body: startAgentExecutionRequest{
				ExpectedSequence: mutation.ExpectedSequence,
			},
		},
		{
			name:   "external execution finish",
			suffix: "/executions/finish",
			body: finishAgentExecutionRequest{
				ExpectedSequence: mutation.ExpectedSequence,
			},
		},
		{
			name:   "daemon execution",
			suffix: "/executions/run",
			body: runAgentExecutionRequest{
				ExpectedSequence: mutation.ExpectedSequence,
				Image:            "image",
				Command:          []string{"noop"},
				TimeoutSeconds:   1,
			},
		},
		{name: "stage", suffix: "/stage", body: mutation},
		{name: "validate", suffix: "/validate", body: mutation},
		{
			name:   "external verification",
			suffix: "/verifications",
			body: recordVerificationRequest{
				ExpectedSequence: mutation.ExpectedSequence,
			},
		},
		{
			name:   "daemon verification",
			suffix: "/verifications/run",
			body: runVerificationRequest{
				ExpectedSequence: mutation.ExpectedSequence,
				Name:             "legacy-check",
				Image:            testDigest("e"),
				Command:          []string{"verify"},
				TimeoutSeconds:   1,
			},
		},
		{name: "prepare", suffix: "/prepare", body: mutation},
		{
			name:   "approval",
			suffix: "/approvals",
			body: resolveApprovalRequest{
				ExpectedSequence: mutation.ExpectedSequence,
			},
		},
		{name: "release", suffix: "/release", body: mutation},
		{name: "abort", suffix: "/abort", body: mutation},
	}
	for _, endpoint := range transactionMutations {
		response := requestJSONWithRuntimeHeader(
			t,
			handler,
			http.MethodPost,
			transactionPath+endpoint.suffix,
			endpoint.body,
			runtimeID,
		)
		assertLegacyAuthorityDenied(t, response, endpoint.name)
	}

	afterRun, err := kernelStore.GetRun(ctx, "engineering", runID)
	if err != nil {
		t.Fatal(err)
	}
	afterTransaction, err := kernelStore.GetTransaction(
		ctx,
		"engineering",
		transactionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterRun, beforeRun) ||
		!reflect.DeepEqual(afterTransaction, beforeTransaction) {
		t.Fatal("legacy authority denial mutated lifecycle projections")
	}
	stagingEntries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(stagingEntries) != 0 {
		t.Fatalf(
			"legacy authority created transaction workspace state: %#v",
			stagingEntries,
		)
	}
}

func assertLegacyAuthorityDenied(
	t *testing.T,
	response *httptest.ResponseRecorder,
	operation string,
) {
	t.Helper()
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"%s legacy authority status=%d body=%s",
			operation,
			response.Code,
			response.Body.String(),
		)
	}
	var denied model.KernelError
	decodeResponse(t, response, &denied)
	if denied.Code != model.ErrorCapabilityDenied ||
		denied.Operation != "get_execution_authority" {
		t.Fatalf("%s legacy authority error=%#v", operation, denied)
	}
}

func apiTestRuntimeID(character string) string {
	return "runtime:" + strings.Repeat(character, 64)
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
		Audiences:               []string{"gatemole-api"},
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
		Version:        admission.LegacyRequestVersion,
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
