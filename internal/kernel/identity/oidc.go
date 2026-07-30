package identity

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
)

const TrustDocumentVersion = "gatemole.oidc_trust.v0"

const maxTrustDocumentBytes = 2 << 20

type TrustDocument struct {
	Version                 string   `json:"version"`
	Issuer                  string   `json:"issuer"`
	Audiences               []string `json:"audiences"`
	ClockSkewSeconds        int64    `json:"clock_skew_seconds"`
	MaxTokenLifetimeSeconds int64    `json:"max_token_lifetime_seconds"`
	PrincipalIDClaim        string   `json:"principal_id_claim"`
	PrincipalKindClaim      string   `json:"principal_kind_claim"`
	NamespaceClaim          string   `json:"namespace_claim"`
	RolesClaim              string   `json:"roles_claim"`
	JWKS                    JWKS     `json:"jwks"`
}

type JWKS struct {
	Keys []JWK `json:"keys"`
}

type JWK struct {
	KTY string `json:"kty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg"`
	KID string `json:"kid"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
	CRV string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
}

type key struct {
	algorithm string
	public    crypto.PublicKey
}

type Verifier struct {
	issuer             string
	audiences          map[string]struct{}
	clockSkew          time.Duration
	maxLifetime        time.Duration
	principalIDClaim   string
	principalKindClaim string
	namespaceClaim     string
	rolesClaim         string
	keys               map[string]key
}

type Identity struct {
	Principal  model.Principal
	Subject    string
	Issuer     string
	Audiences  []string
	Namespaces []string
	Roles      []string
	ExpiresAt  time.Time
	TokenID    string
}

func LoadTrustFile(path string) (*Verifier, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("OIDC trust: open %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("OIDC trust: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxTrustDocumentBytes {
		return nil, errors.New("OIDC trust must be a regular file no larger than 2 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTrustDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("OIDC trust: read %s: %w", path, err)
	}
	if len(data) > maxTrustDocumentBytes {
		return nil, errors.New("OIDC trust must be a regular file no larger than 2 MiB")
	}
	verifier, err := ParseTrustDocument(data)
	if err != nil {
		return nil, fmt.Errorf("OIDC trust: decode %s: %w", path, err)
	}
	return verifier, nil
}

// ParseTrustDocument validates one already-bounded OIDC trust snapshot.
// Callers that also record a digest must hash these exact bytes.
func ParseTrustDocument(data []byte) (*Verifier, error) {
	if len(data) > maxTrustDocumentBytes {
		return nil, errors.New("OIDC trust must be no larger than 2 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document TrustDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON is not allowed")
	}
	return NewVerifier(document)
}

func NewVerifier(document TrustDocument) (*Verifier, error) {
	if document.Version != TrustDocumentVersion {
		return nil, fmt.Errorf("OIDC trust version must be %q", TrustDocumentVersion)
	}
	issuer, err := url.Parse(document.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" ||
		issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return nil, errors.New("OIDC issuer must be an HTTPS URL")
	}
	if len(document.Audiences) == 0 {
		return nil, errors.New("OIDC trust requires at least one audience")
	}
	audiences := make(map[string]struct{}, len(document.Audiences))
	for _, audience := range document.Audiences {
		if strings.TrimSpace(audience) == "" || strings.ContainsAny(audience, "\r\n\x00") {
			return nil, errors.New("OIDC audience is invalid")
		}
		if _, duplicate := audiences[audience]; duplicate {
			return nil, errors.New("OIDC audiences must be unique")
		}
		audiences[audience] = struct{}{}
	}
	if document.ClockSkewSeconds < 0 || document.ClockSkewSeconds > 300 {
		return nil, errors.New("OIDC clock skew must be between 0 and 300 seconds")
	}
	if document.MaxTokenLifetimeSeconds < 60 || document.MaxTokenLifetimeSeconds > 86400 {
		return nil, errors.New("OIDC maximum token lifetime must be between 60 and 86400 seconds")
	}
	claims := []*string{
		&document.PrincipalIDClaim, &document.PrincipalKindClaim,
		&document.NamespaceClaim, &document.RolesClaim,
	}
	defaults := []string{
		"vouch_principal_id", "vouch_principal_kind",
		"vouch_namespaces", "vouch_roles",
	}
	for index, claim := range claims {
		if *claim == "" {
			*claim = defaults[index]
		}
		if !claimName(*claim) {
			return nil, fmt.Errorf("OIDC claim name %q is invalid", *claim)
		}
	}
	keys := make(map[string]key, len(document.JWKS.Keys))
	for _, value := range document.JWKS.Keys {
		parsed, err := parseJWK(value)
		if err != nil {
			return nil, err
		}
		if _, duplicate := keys[value.KID]; duplicate {
			return nil, fmt.Errorf("OIDC JWKS repeats key ID %q", value.KID)
		}
		keys[value.KID] = parsed
	}
	if len(keys) == 0 {
		return nil, errors.New("OIDC JWKS requires at least one signing key")
	}
	return &Verifier{
		issuer: document.Issuer, audiences: audiences,
		clockSkew:          time.Duration(document.ClockSkewSeconds) * time.Second,
		maxLifetime:        time.Duration(document.MaxTokenLifetimeSeconds) * time.Second,
		principalIDClaim:   document.PrincipalIDClaim,
		principalKindClaim: document.PrincipalKindClaim,
		namespaceClaim:     document.NamespaceClaim, rolesClaim: document.RolesClaim,
		keys: keys,
	}, nil
}

func (verifier *Verifier) Verify(token string, now time.Time) (Identity, error) {
	if verifier == nil {
		return Identity{}, errors.New("OIDC verifier is not configured")
	}
	if len(token) < 32 || len(token) > 64<<10 || strings.TrimSpace(token) != token {
		return Identity{}, errors.New("OIDC token has an invalid size or encoding")
	}
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return Identity{}, errors.New("OIDC token must be a compact JWS")
	}
	encoding := base64.RawURLEncoding.Strict()
	headerData, err := encoding.DecodeString(segments[0])
	if err != nil {
		return Identity{}, errors.New("OIDC token header is not base64url")
	}
	payloadData, err := encoding.DecodeString(segments[1])
	if err != nil {
		return Identity{}, errors.New("OIDC token payload is not base64url")
	}
	signature, err := encoding.DecodeString(segments[2])
	if err != nil {
		return Identity{}, errors.New("OIDC token signature is not base64url")
	}
	var header struct {
		Algorithm string          `json:"alg"`
		KeyID     string          `json:"kid"`
		Type      string          `json:"typ,omitempty"`
		Critical  json.RawMessage `json:"crit,omitempty"`
	}
	if err := decodeStrictUniqueObject(headerData, &header); err != nil {
		return Identity{}, fmt.Errorf("OIDC token header: %w", err)
	}
	if header.Algorithm == "" || header.Algorithm == "none" || header.KeyID == "" ||
		len(header.Critical) != 0 ||
		(header.Type != "" && !strings.EqualFold(header.Type, "JWT") &&
			!strings.EqualFold(header.Type, "at+jwt")) {
		return Identity{}, errors.New("OIDC token protected header is not allowed")
	}
	signingKey, exists := verifier.keys[header.KeyID]
	if !exists || signingKey.algorithm != header.Algorithm {
		return Identity{}, errors.New("OIDC token signing key or algorithm is not trusted")
	}
	signingInput := []byte(segments[0] + "." + segments[1])
	if err := verifySignature(signingKey, signingInput, signature); err != nil {
		return Identity{}, err
	}
	claims, err := decodeClaims(payloadData)
	if err != nil {
		return Identity{}, err
	}
	if claims.Issuer != verifier.issuer ||
		!audienceIntersects(claims.Audience, verifier.audiences) {
		return Identity{}, errors.New("OIDC token issuer or audience is not trusted")
	}
	now = now.UTC()
	expiry := time.Unix(claims.ExpiresAt, 0).UTC()
	issuedAt := time.Unix(claims.IssuedAt, 0).UTC()
	if claims.ExpiresAt <= 0 || claims.IssuedAt <= 0 ||
		!expiry.After(now.Add(-verifier.clockSkew)) ||
		issuedAt.After(now.Add(verifier.clockSkew)) ||
		!expiry.After(issuedAt) ||
		expiry.Sub(issuedAt) > verifier.maxLifetime {
		return Identity{}, errors.New("OIDC token lifetime is invalid")
	}
	if claims.NotBefore > 0 {
		notBefore := time.Unix(claims.NotBefore, 0).UTC()
		if notBefore.After(now.Add(verifier.clockSkew)) {
			return Identity{}, errors.New("OIDC token is not active yet")
		}
		if notBefore.After(expiry) {
			return Identity{}, errors.New("OIDC token nbf is after its expiry")
		}
	}
	principalID, err := stringClaim(claims.Extra, verifier.principalIDClaim)
	if err != nil || !model.IsIdentifier(principalID) {
		return Identity{}, errors.New("OIDC token principal ID claim is invalid")
	}
	kindValue, err := stringClaim(claims.Extra, verifier.principalKindClaim)
	if err != nil {
		return Identity{}, errors.New("OIDC token principal kind claim is invalid")
	}
	kind := model.PrincipalKind(kindValue)
	if kind != model.PrincipalHuman && kind != model.PrincipalService &&
		kind != model.PrincipalOperator {
		return Identity{}, errors.New("OIDC token principal kind is not allowed")
	}
	namespaces, err := stringArrayClaim(claims.Extra, verifier.namespaceClaim)
	if err != nil || len(namespaces) == 0 {
		return Identity{}, errors.New("OIDC token namespace claim is invalid")
	}
	for _, namespace := range namespaces {
		if namespace != "*" && !model.IsIdentifier(namespace) {
			return Identity{}, errors.New("OIDC token contains an invalid namespace")
		}
	}
	roles, err := stringArrayClaim(claims.Extra, verifier.rolesClaim)
	if err != nil || len(roles) == 0 {
		return Identity{}, errors.New("OIDC token roles claim is invalid")
	}
	for _, role := range roles {
		if role != "viewer" && role != "operator" && role != "approver" && role != "admin" {
			return Identity{}, errors.New("OIDC token contains an invalid Vouch role")
		}
	}
	payloadDigest := sha256.Sum256(payloadData)
	return Identity{
		Principal: model.Principal{
			ID: principalID, Kind: kind, Issuer: claims.Issuer,
			ClaimsDigest: "sha256:" + hex.EncodeToString(payloadDigest[:]),
		},
		Subject: claims.Subject, Issuer: claims.Issuer,
		Audiences: claims.Audience, Namespaces: namespaces, Roles: roles,
		ExpiresAt: expiry, TokenID: claims.TokenID,
	}, nil
}

func (identity Identity) AllowsNamespace(namespace string) bool {
	for _, allowed := range identity.Namespaces {
		if allowed == "*" || subtle.ConstantTimeCompare([]byte(allowed), []byte(namespace)) == 1 {
			return true
		}
	}
	return false
}

func (identity Identity) HasRole(role string) bool {
	for _, existing := range identity.Roles {
		if existing == "admin" || existing == role {
			return true
		}
	}
	return false
}

type tokenClaims struct {
	Issuer    string                     `json:"iss"`
	Subject   string                     `json:"sub"`
	Audience  []string                   `json:"-"`
	ExpiresAt int64                      `json:"exp"`
	IssuedAt  int64                      `json:"iat"`
	NotBefore int64                      `json:"nbf,omitempty"`
	TokenID   string                     `json:"jti,omitempty"`
	Extra     map[string]json.RawMessage `json:"-"`
}

func decodeClaims(data []byte) (tokenClaims, error) {
	if err := requireUniqueObjectKeys(data); err != nil {
		return tokenClaims{}, fmt.Errorf("OIDC token claims: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return tokenClaims{}, errors.New("OIDC token claims are not a JSON object")
	}
	claims := tokenClaims{Extra: raw}
	if err := json.Unmarshal(raw["iss"], &claims.Issuer); err != nil ||
		json.Unmarshal(raw["sub"], &claims.Subject) != nil ||
		claims.Subject == "" || len(claims.Subject) > 512 {
		return tokenClaims{}, errors.New("OIDC token issuer or subject claim is invalid")
	}
	if err := json.Unmarshal(raw["exp"], &claims.ExpiresAt); err != nil ||
		json.Unmarshal(raw["iat"], &claims.IssuedAt) != nil {
		return tokenClaims{}, errors.New("OIDC token time claims are invalid")
	}
	if value, exists := raw["nbf"]; exists {
		if err := json.Unmarshal(value, &claims.NotBefore); err != nil {
			return tokenClaims{}, errors.New("OIDC token nbf claim is invalid")
		}
	}
	if value, exists := raw["jti"]; exists {
		if err := json.Unmarshal(value, &claims.TokenID); err != nil ||
			len(claims.TokenID) > 512 {
			return tokenClaims{}, errors.New("OIDC token jti claim is invalid")
		}
	}
	if err := json.Unmarshal(raw["aud"], &claims.Audience); err != nil {
		var one string
		if err := json.Unmarshal(raw["aud"], &one); err != nil || one == "" {
			return tokenClaims{}, errors.New("OIDC token audience claim is invalid")
		}
		claims.Audience = []string{one}
	}
	return claims, nil
}

func parseJWK(value JWK) (key, error) {
	if !model.IsIdentifier(value.KID) || value.Use != "" && value.Use != "sig" {
		return key{}, fmt.Errorf("OIDC JWK %q has an invalid ID or use", value.KID)
	}
	encoding := base64.RawURLEncoding.Strict()
	switch {
	case value.KTY == "RSA" && value.Alg == "RS256":
		modulus, err := encoding.DecodeString(value.N)
		if err != nil || len(modulus) < 256 {
			return key{}, fmt.Errorf("OIDC RSA key %q modulus is invalid", value.KID)
		}
		exponentBytes, err := encoding.DecodeString(value.E)
		if err != nil || len(exponentBytes) == 0 || len(exponentBytes) > 4 {
			return key{}, fmt.Errorf("OIDC RSA key %q exponent is invalid", value.KID)
		}
		exponent := new(big.Int).SetBytes(exponentBytes)
		if !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64() > 1<<31-1 {
			return key{}, fmt.Errorf("OIDC RSA key %q exponent is invalid", value.KID)
		}
		modulusInteger := new(big.Int).SetBytes(modulus)
		if modulusInteger.BitLen() < 2048 || exponent.Bit(0) == 0 {
			return key{}, fmt.Errorf("OIDC RSA key %q parameters are unsafe", value.KID)
		}
		return key{
			algorithm: value.Alg,
			public:    &rsa.PublicKey{N: modulusInteger, E: int(exponent.Int64())},
		}, nil
	case value.KTY == "OKP" && value.Alg == "EdDSA" && value.CRV == "Ed25519":
		public, err := encoding.DecodeString(value.X)
		if err != nil || len(public) != ed25519.PublicKeySize {
			return key{}, fmt.Errorf("OIDC Ed25519 key %q is invalid", value.KID)
		}
		return key{algorithm: value.Alg, public: ed25519.PublicKey(public)}, nil
	default:
		return key{}, fmt.Errorf("OIDC JWK %q uses unsupported kty/alg/crv", value.KID)
	}
}

func verifySignature(value key, signingInput, signature []byte) error {
	switch value.algorithm {
	case "RS256":
		digest := sha256.Sum256(signingInput)
		if err := rsa.VerifyPKCS1v15(value.public.(*rsa.PublicKey), crypto.SHA256, digest[:], signature); err != nil {
			return errors.New("OIDC token signature verification failed")
		}
	case "EdDSA":
		if !ed25519.Verify(value.public.(ed25519.PublicKey), signingInput, signature) {
			return errors.New("OIDC token signature verification failed")
		}
	default:
		return errors.New("OIDC token signature algorithm is unsupported")
	}
	return nil
}

func decodeStrictUniqueObject(data []byte, target any) error {
	if err := requireUniqueObjectKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON is not allowed")
	}
	return nil
}

func requireUniqueObjectKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return errors.New("value must be a JSON object")
	}
	seen := map[string]struct{}{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return errors.New("object member name must be a string")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate JSON member %q", name)
		}
		seen[name] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON is not allowed")
		}
		return err
	}
	return nil
}

func stringClaim(claims map[string]json.RawMessage, name string) (string, error) {
	var value string
	if err := json.Unmarshal(claims[name], &value); err != nil {
		return "", err
	}
	return value, nil
}

func stringArrayClaim(claims map[string]json.RawMessage, name string) ([]string, error) {
	var values []string
	if err := json.Unmarshal(claims[name], &values); err != nil {
		return nil, err
	}
	if len(values) > 128 {
		return nil, errors.New("claim contains too many values")
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || len(value) > 256 {
			return nil, errors.New("claim contains an invalid value")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, errors.New("claim values must be unique")
		}
		seen[value] = struct{}{}
	}
	return values, nil
}

func audienceIntersects(values []string, trusted map[string]struct{}) bool {
	for _, value := range values {
		if _, exists := trusted[value]; exists {
			return true
		}
	}
	return false
}

func claimName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for i, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			(i > 0 && (character == '_' || character == '-' || character == '.')) {
			continue
		}
		return false
	}
	return true
}
