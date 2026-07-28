package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/identity"
	"github.com/duriantaco/vouch/internal/kernel/model"
	"github.com/duriantaco/vouch/internal/kernel/store"
)

func TestIdentityMiddlewareEnforcesRoleNamespaceAndActorBinding(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwk, err := identity.Ed25519JWK("key:api-test", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := identity.NewVerifier(identity.TrustDocument{
		Version: identity.TrustDocumentVersion,
		Issuer:  "https://issuer.example.invalid",
		Audiences: []string{
			"vouch-api",
		},
		ClockSkewSeconds:        30,
		MaxTokenLifetimeSeconds: 3600,
		JWKS: identity.JWKS{
			Keys: []identity.JWK{jwk},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	kernelStore, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kernelStore.Close() })
	handler := NewServer(
		kernelStore,
		WithIdentityVerifier(verifier),
		WithClock(func() time.Time { return now }),
	).Handler()
	operatorToken := apiIdentityToken(
		t, privateKey, "key:api-test", now, "operator:alice", model.PrincipalOperator,
		[]string{"fixture"}, []string{"viewer", "operator"},
	)
	viewerToken := apiIdentityToken(
		t, privateKey, "key:api-test", now, "service:auditor", model.PrincipalService,
		[]string{"fixture"}, []string{"viewer"},
	)
	parentNamespaceToken := apiIdentityToken(
		t, privateKey, "key:api-test", now, "service:parent", model.PrincipalService,
		[]string{"team"}, []string{"viewer"},
	)
	nestedNamespaceToken := apiIdentityToken(
		t, privateKey, "key:api-test", now, "service:nested", model.PrincipalService,
		[]string{"team/platform"}, []string{"viewer"},
	)

	response := requestJSONWithToken(
		t, handler, http.MethodGet, "/healthz", nil, "",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("anonymous health status=%d", response.Code)
	}
	response = requestJSONWithToken(
		t, handler, http.MethodGet,
		"/v0/namespaces/fixture/runs", nil, "",
	)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status=%d, want 401", response.Code)
	}

	created := readCreationEvent(t)
	created.Actor = model.Principal{
		ID: "operator:alice", Kind: model.PrincipalOperator,
	}
	created.Digest, err = model.ComputeEventDigest(created)
	if err != nil {
		t.Fatal(err)
	}
	response = requestJSONWithToken(
		t, handler, http.MethodPost, "/v0/runs", created, operatorToken,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d: %s", response.Code, response.Body.String())
	}
	events, err := kernelStore.Events(
		context.Background(), "fixture", created.RunID, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 ||
		events[0].Actor.Issuer != "https://issuer.example.invalid" ||
		!model.IsSHA256Digest(events[0].Actor.ClaimsDigest) {
		t.Fatalf("ledger actor was not bound to verified claims: %#v", events)
	}

	response = requestJSONWithToken(
		t, handler, http.MethodGet,
		"/v0/namespaces/fixture/runs", nil, viewerToken,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("authorized viewer status=%d: %s", response.Code, response.Body.String())
	}
	response = requestJSONWithToken(
		t, handler, http.MethodGet,
		"/v0/namespaces/another-team/runs", nil, viewerToken,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-namespace viewer status=%d, want 403", response.Code)
	}
	escapedNamespacePath := "/v0/namespaces/team%2Fplatform/runs"
	response = requestJSONWithToken(
		t, handler, http.MethodGet,
		escapedNamespacePath, nil, parentNamespaceToken,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf(
			"parent namespace token crossed encoded namespace boundary: status=%d",
			response.Code,
		)
	}
	response = requestJSONWithToken(
		t, handler, http.MethodGet,
		escapedNamespacePath, nil, nestedNamespaceToken,
	)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"encoded namespace token did not reach its namespace: status=%d body=%s",
			response.Code,
			response.Body.String(),
		)
	}

	forged := created
	forged.RunID = "run:forged-actor"
	forged.ID = "event:forged-actor"
	forged.Actor.ID = "operator:bob"
	forged.Digest, err = model.ComputeEventDigest(forged)
	if err != nil {
		t.Fatal(err)
	}
	response = requestJSONWithToken(
		t, handler, http.MethodPost, "/v0/runs", forged, operatorToken,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("forged actor status=%d, want 403", response.Code)
	}
	response = requestJSONWithToken(
		t, handler, http.MethodPost, "/v0/runs", created, viewerToken,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("viewer mutation status=%d, want 403", response.Code)
	}
}

func TestIdentityMiddlewareRequiresApproverRoleAndIssuerBinding(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwk, err := identity.Ed25519JWK("key:approval-api", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := identity.NewVerifier(identity.TrustDocument{
		Version:                 identity.TrustDocumentVersion,
		Issuer:                  "https://issuer.example.invalid",
		Audiences:               []string{"vouch-api"},
		ClockSkewSeconds:        30,
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
	handler := NewServer(
		kernelStore,
		WithIdentityVerifier(verifier),
		WithClock(func() time.Time { return now }),
	).Handler()
	operatorToken := apiIdentityToken(
		t, privateKey, "key:approval-api", now, "human:reviewer", model.PrincipalHuman,
		[]string{"team-a"}, []string{"operator"},
	)
	approverToken := apiIdentityToken(
		t, privateKey, "key:approval-api", now, "human:reviewer", model.PrincipalHuman,
		[]string{"team-a"}, []string{"approver"},
	)
	path := "/v0/namespaces/team-a/transactions/tx:test/approvals"
	request := map[string]any{
		"expected_sequence": 1,
		"decision": map[string]any{
			"approver": map[string]any{
				"id": "human:reviewer", "kind": "human",
				"issuer": "https://issuer.example.invalid",
			},
		},
	}
	response := requestJSONWithToken(
		t, handler, http.MethodPost, path, request, operatorToken,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("operator approved transaction: status=%d", response.Code)
	}
	response = requestJSONWithToken(
		t, handler, http.MethodPost, path, request, approverToken,
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf("approver did not reach handler: status=%d body=%s", response.Code, response.Body.String())
	}
	request["decision"].(map[string]any)["approver"] = map[string]any{
		"id": "human:reviewer", "kind": "human",
	}
	response = requestJSONWithToken(
		t, handler, http.MethodPost, path, request, approverToken,
	)
	if response.Code != http.StatusForbidden {
		t.Fatalf("approval without issuer binding status=%d, want 403", response.Code)
	}
}

func apiIdentityToken(
	t *testing.T,
	privateKey ed25519.PrivateKey,
	keyID string,
	now time.Time,
	principalID string,
	kind model.PrincipalKind,
	namespaces, roles []string,
) string {
	t.Helper()
	token, err := identity.IssueEd25519(identity.IssueRequest{
		KeyID: keyID, Issuer: "https://issuer.example.invalid",
		Audience: "vouch-api", Subject: principalID,
		PrincipalID: principalID, Kind: kind, Namespaces: namespaces, Roles: roles,
		IssuedAt: now, ExpiresAt: now.Add(15 * time.Minute),
		TokenID: "token:" + principalID,
	}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func requestJSONWithToken(
	t *testing.T,
	handler http.Handler,
	method, path string,
	value any,
	token string,
) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if value != nil {
		if err := json.NewEncoder(&body).Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequestWithContext(
		context.Background(), method, path, &body,
	)
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
