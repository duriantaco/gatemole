package identity

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
)

func TestVerifierAcceptsBoundEd25519Identity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier := testEd25519Verifier(t, publicKey)
	token := testIssueToken(t, privateKey, now, now.Add(10*time.Minute))

	got, err := verifier.Verify(token, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got.Principal.ID != "operator:alice" ||
		got.Principal.Kind != model.PrincipalOperator ||
		got.Principal.Issuer != "https://issuer.example.invalid" ||
		!model.IsSHA256Digest(got.Principal.ClaimsDigest) ||
		got.Subject != "subject:alice" ||
		!got.AllowsNamespace("team-a") ||
		got.AllowsNamespace("team-b") ||
		!got.HasRole("operator") ||
		got.HasRole("approver") {
		t.Fatalf("unexpected verified identity: %#v", got)
	}
}

func TestVerifierRejectsUntrustedOrInvalidTokens(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier := testEd25519Verifier(t, publicKey)
	valid := testIssueToken(t, privateKey, now, now.Add(10*time.Minute))

	parts := strings.Split(valid, ".")
	tamperedSignature := parts[0] + "." + parts[1] + "." +
		base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	expired := testIssueToken(
		t, privateKey, now.Add(-20*time.Minute), now.Add(-10*time.Minute),
	)
	tooLong := signEd25519Claims(t, privateKey, map[string]any{
		"iss": "https://issuer.example.invalid", "sub": "subject:alice",
		"aud": "gatemole", "iat": now.Unix(), "exp": now.Add(2 * time.Hour).Unix(),
		"gatemole_principal_id": "operator:alice", "gatemole_principal_kind": "operator",
		"gatemole_namespaces": []string{"team-a"}, "gatemole_roles": []string{"operator"},
	})
	future := signEd25519Claims(t, privateKey, map[string]any{
		"iss": "https://issuer.example.invalid", "sub": "subject:alice",
		"aud": "gatemole", "iat": now.Unix(), "nbf": now.Add(10 * time.Minute).Unix(),
		"exp":                   now.Add(20 * time.Minute).Unix(),
		"gatemole_principal_id": "operator:alice", "gatemole_principal_kind": "operator",
		"gatemole_namespaces": []string{"team-a"}, "gatemole_roles": []string{"operator"},
	})
	wrongAudience := signEd25519Claims(t, privateKey, map[string]any{
		"iss": "https://issuer.example.invalid", "sub": "subject:alice",
		"aud": "somewhere-else", "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"gatemole_principal_id": "operator:alice", "gatemole_principal_kind": "operator",
		"gatemole_namespaces": []string{"team-a"}, "gatemole_roles": []string{"operator"},
	})
	noneHeader := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"alg":"none","kid":"key:test","typ":"JWT"}`),
	)
	noneToken := noneHeader + "." + parts[1] + ".unsigned"
	duplicateClaims := signRawEd25519(
		t,
		privateKey,
		[]byte(`{"iss":"https://issuer.example.invalid","iss":"https://issuer.example.invalid","sub":"subject:alice","aud":"gatemole","iat":1784800800,"exp":1784801400,"gatemole_principal_id":"operator:alice","gatemole_principal_kind":"operator","gatemole_namespaces":["team-a"],"gatemole_roles":["operator"]}`),
	)

	for name, token := range map[string]string{
		"tampered signature": tamperedSignature,
		"expired":            expired,
		"excessive lifetime": tooLong,
		"future nbf":         future,
		"wrong audience":     wrongAudience,
		"alg none":           noneToken,
		"duplicate claims":   duplicateClaims,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := verifier.Verify(token, now); err == nil {
				t.Fatalf("verifier accepted %s token", name)
			}
		})
	}
}

func TestVerifierAcceptsRS256(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	exponent := big.NewInt(int64(privateKey.PublicKey.E)).Bytes()
	document := testTrustDocument()
	document.JWKS.Keys = []JWK{{
		KTY: "RSA", Use: "sig", Alg: "RS256", KID: "key:rsa",
		N: base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
		E: base64.RawURLEncoding.EncodeToString(exponent),
	}}
	verifier, err := NewVerifier(document)
	if err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]string{
		"alg": "RS256", "kid": "key:rsa", "typ": "JWT",
	})
	claims, _ := json.Marshal(map[string]any{
		"iss": "https://issuer.example.invalid", "sub": "subject:service",
		"aud": "gatemole", "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"gatemole_principal_id": "service:ci", "gatemole_principal_kind": "service",
		"gatemole_namespaces": []string{"team-a"}, "gatemole_roles": []string{"viewer"},
	})
	encoding := base64.RawURLEncoding
	signingInput := encoding.EncodeToString(header) + "." + encoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	token := signingInput + "." + encoding.EncodeToString(signature)
	if _, err := verifier.Verify(token, now); err != nil {
		t.Fatal(err)
	}
}

func TestIssueRejectsUnauthorizedClaims(t *testing.T) {
	t.Parallel()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
	request := IssueRequest{
		KeyID: "key:test", Issuer: "https://issuer.example.invalid",
		Audience: "gatemole", Subject: "subject:alice", PrincipalID: "operator:alice",
		Kind: model.PrincipalOperator, Namespaces: []string{"team-a"},
		Roles: []string{"root"}, IssuedAt: now, ExpiresAt: now.Add(time.Minute),
		TokenID: "token:test",
	}
	if _, err := IssueEd25519(request, privateKey); err == nil {
		t.Fatal("issuer accepted an unknown Gatemole role")
	}
	request.Roles = []string{"operator"}
	request.Namespaces = []string{"not allowed"}
	if _, err := IssueEd25519(request, privateKey); err == nil {
		t.Fatal("issuer accepted an invalid namespace")
	}
}

func testTrustDocument() TrustDocument {
	return TrustDocument{
		Version:                 TrustDocumentVersion,
		Issuer:                  "https://issuer.example.invalid",
		Audiences:               []string{"gatemole"},
		ClockSkewSeconds:        30,
		MaxTokenLifetimeSeconds: 3600,
		JWKS:                    JWKS{},
	}
}

func testEd25519Verifier(t *testing.T, publicKey ed25519.PublicKey) *Verifier {
	t.Helper()
	jwk, err := Ed25519JWK("key:test", publicKey)
	if err != nil {
		t.Fatal(err)
	}
	document := testTrustDocument()
	document.JWKS.Keys = []JWK{jwk}
	verifier, err := NewVerifier(document)
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func testIssueToken(
	t *testing.T,
	privateKey ed25519.PrivateKey,
	issuedAt, expiresAt time.Time,
) string {
	t.Helper()
	token, err := IssueEd25519(IssueRequest{
		KeyID: "key:test", Issuer: "https://issuer.example.invalid",
		Audience: "gatemole", Subject: "subject:alice", PrincipalID: "operator:alice",
		Kind: model.PrincipalOperator, Namespaces: []string{"team-a"},
		Roles: []string{"viewer", "operator"}, IssuedAt: issuedAt, ExpiresAt: expiresAt,
		TokenID: "token:test",
	}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func signEd25519Claims(
	t *testing.T,
	privateKey ed25519.PrivateKey,
	claims map[string]any,
) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return signRawEd25519(t, privateKey, payload)
}

func signRawEd25519(t *testing.T, privateKey ed25519.PrivateKey, payload []byte) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{
		"alg": "EdDSA", "kid": "key:test", "typ": "JWT",
	})
	if err != nil {
		t.Fatal(err)
	}
	encoding := base64.RawURLEncoding
	signingInput := encoding.EncodeToString(header) + "." + encoding.EncodeToString(payload)
	return signingInput + "." + encoding.EncodeToString(
		ed25519.Sign(privateKey, []byte(signingInput)),
	)
}
