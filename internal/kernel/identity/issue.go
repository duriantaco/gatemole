package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
)

type IssueRequest struct {
	KeyID       string
	Issuer      string
	Audience    string
	Subject     string
	PrincipalID string
	Kind        model.PrincipalKind
	Namespaces  []string
	Roles       []string
	IssuedAt    time.Time
	ExpiresAt   time.Time
	TokenID     string
}

// Ed25519JWK returns the public signing-key representation accepted by the
// verifier. It is intended for local bootstrap and acceptance testing; an
// enterprise deployment should normally use the JWKS managed by its IdP.
func Ed25519JWK(keyID string, publicKey ed25519.PublicKey) (JWK, error) {
	if !model.IsIdentifier(keyID) {
		return JWK{}, errors.New("OIDC signing key ID is invalid")
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return JWK{}, errors.New("OIDC signing key is not an Ed25519 public key")
	}
	return JWK{
		KTY: "OKP",
		Use: "sig",
		Alg: "EdDSA",
		KID: keyID,
		CRV: "Ed25519",
		X:   base64.RawURLEncoding.EncodeToString(publicKey),
	}, nil
}

// IssueEd25519 signs a compact JWT suitable for the static OIDC trust
// document. Keeping issuance here makes local bootstrap deterministic without
// making the daemon an identity provider.
func IssueEd25519(request IssueRequest, privateKey ed25519.PrivateKey) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", errors.New("OIDC signing key is not an Ed25519 private key")
	}
	if !model.IsIdentifier(request.KeyID) ||
		!model.IsIdentifier(request.PrincipalID) ||
		request.Subject == "" || len(request.Subject) > 512 ||
		request.Audience == "" ||
		request.TokenID == "" || len(request.TokenID) > 512 {
		return "", errors.New("OIDC issue request identifiers are invalid")
	}
	if request.Kind != model.PrincipalHuman &&
		request.Kind != model.PrincipalService &&
		request.Kind != model.PrincipalOperator {
		return "", errors.New("OIDC issue request principal kind is not allowed")
	}
	if len(request.Namespaces) == 0 || len(request.Roles) == 0 {
		return "", errors.New("OIDC issue request requires namespaces and roles")
	}
	issuedAt := request.IssuedAt.UTC()
	expiresAt := request.ExpiresAt.UTC()
	if issuedAt.IsZero() || !expiresAt.After(issuedAt) {
		return "", errors.New("OIDC issue request lifetime is invalid")
	}
	jwk, err := Ed25519JWK(request.KeyID, privateKey.Public().(ed25519.PublicKey))
	if err != nil {
		return "", err
	}
	document := TrustDocument{
		Version:                 TrustDocumentVersion,
		Issuer:                  request.Issuer,
		Audiences:               []string{request.Audience},
		ClockSkewSeconds:        0,
		MaxTokenLifetimeSeconds: int64(expiresAt.Sub(issuedAt) / time.Second),
		JWKS:                    JWKS{Keys: []JWK{jwk}},
	}
	verifier, err := NewVerifier(document)
	if err != nil {
		return "", fmt.Errorf("OIDC issue request: %w", err)
	}
	headerData, err := json.Marshal(map[string]string{
		"alg": "EdDSA",
		"kid": request.KeyID,
		"typ": "JWT",
	})
	if err != nil {
		return "", err
	}
	claimsData, err := json.Marshal(map[string]any{
		"iss":                     request.Issuer,
		"sub":                     request.Subject,
		"aud":                     request.Audience,
		"exp":                     expiresAt.Unix(),
		"iat":                     issuedAt.Unix(),
		"nbf":                     issuedAt.Unix(),
		"jti":                     request.TokenID,
		"gatemole_principal_id":   request.PrincipalID,
		"gatemole_principal_kind": request.Kind,
		"gatemole_namespaces":     request.Namespaces,
		"gatemole_roles":          request.Roles,
	})
	if err != nil {
		return "", err
	}
	encoding := base64.RawURLEncoding
	signingInput := encoding.EncodeToString(headerData) + "." +
		encoding.EncodeToString(claimsData)
	token := signingInput + "." + encoding.EncodeToString(
		ed25519.Sign(privateKey, []byte(signingInput)),
	)
	if _, err := verifier.Verify(token, issuedAt); err != nil {
		return "", fmt.Errorf("OIDC issued token failed self-verification: %w", err)
	}
	return token, nil
}
