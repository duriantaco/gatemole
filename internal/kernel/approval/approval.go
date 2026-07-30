package approval

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	transactionreducer "github.com/duriantaco/gatemole/internal/kernel/transaction"
)

const TrustDocumentVersion = "gatemole.approval_trust.v0"

const maxTrustDocumentBytes = 2 << 20

type TrustedKey struct {
	KeyID           string
	PublicKey       ed25519.PublicKey
	Principal       model.Principal
	ApprovalClasses []string
}

type TrustStore struct {
	keys map[string]TrustedKey
}

type TrustDocument struct {
	Version string             `json:"version"`
	Keys    []TrustDocumentKey `json:"keys"`
}

type TrustDocumentKey struct {
	KeyID           string          `json:"key_id"`
	PublicKeyBase64 string          `json:"public_key_base64"`
	Principal       model.Principal `json:"principal"`
	ApprovalClasses []string        `json:"approval_classes"`
}

func NewTrustStore(keys []TrustedKey) (*TrustStore, error) {
	store := &TrustStore{keys: make(map[string]TrustedKey, len(keys))}
	for _, key := range keys {
		if !model.IsIdentifier(key.KeyID) {
			return nil, fmt.Errorf("approval trust: invalid key ID %q", key.KeyID)
		}
		if len(key.PublicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("approval trust: key %q is not an Ed25519 public key", key.KeyID)
		}
		if !model.IsIdentifier(key.Principal.ID) ||
			key.Principal.Kind != model.PrincipalHuman ||
			(key.Principal.ClaimsDigest != "" && !model.IsSHA256Digest(key.Principal.ClaimsDigest)) {
			return nil, fmt.Errorf("approval trust: key %q must bind a valid human principal", key.KeyID)
		}
		if len(key.ApprovalClasses) == 0 {
			return nil, fmt.Errorf("approval trust: key %q has no approval classes", key.KeyID)
		}
		seenClasses := make(map[string]struct{}, len(key.ApprovalClasses))
		for _, class := range key.ApprovalClasses {
			if !model.IsIdentifier(class) {
				return nil, fmt.Errorf("approval trust: key %q has invalid class %q", key.KeyID, class)
			}
			if _, duplicate := seenClasses[class]; duplicate {
				return nil, fmt.Errorf("approval trust: key %q repeats class %q", key.KeyID, class)
			}
			seenClasses[class] = struct{}{}
		}
		if _, duplicate := store.keys[key.KeyID]; duplicate {
			return nil, fmt.Errorf("approval trust: duplicate key ID %q", key.KeyID)
		}
		copied := key
		copied.PublicKey = append(ed25519.PublicKey(nil), key.PublicKey...)
		copied.ApprovalClasses = append([]string(nil), key.ApprovalClasses...)
		store.keys[key.KeyID] = copied
	}
	return store, nil
}

func EmptyTrustStore() *TrustStore {
	store, _ := NewTrustStore(nil)
	return store
}

func (store *TrustStore) Len() int {
	if store == nil {
		return 0
	}
	return len(store.keys)
}

func LoadTrustFile(path string) (*TrustStore, error) {
	if strings.TrimSpace(path) == "" {
		return EmptyTrustStore(), nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("approval trust: open %s: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("approval trust: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxTrustDocumentBytes {
		return nil, errors.New("approval trust must be a regular file no larger than 2 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTrustDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("approval trust: read %s: %w", path, err)
	}
	if len(data) > maxTrustDocumentBytes {
		return nil, errors.New("approval trust must be a regular file no larger than 2 MiB")
	}
	store, err := ParseTrustDocument(data)
	if err != nil {
		return nil, fmt.Errorf("approval trust: decode %s: %w", path, err)
	}
	return store, nil
}

// ParseTrustDocument validates one already-bounded approval trust snapshot.
// Callers that also record a digest must hash these exact bytes.
func ParseTrustDocument(data []byte) (*TrustStore, error) {
	if len(data) > maxTrustDocumentBytes {
		return nil, errors.New("approval trust must be no larger than 2 MiB")
	}
	var document TrustDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	if document.Version != TrustDocumentVersion {
		return nil, fmt.Errorf("expected version %q", TrustDocumentVersion)
	}
	keys := make([]TrustedKey, 0, len(document.Keys))
	for _, entry := range document.Keys {
		publicKey, err := base64.StdEncoding.DecodeString(entry.PublicKeyBase64)
		if err != nil {
			return nil, fmt.Errorf("key %q public key is not base64: %w", entry.KeyID, err)
		}
		keys = append(keys, TrustedKey{
			KeyID:           entry.KeyID,
			PublicKey:       ed25519.PublicKey(publicKey),
			Principal:       entry.Principal,
			ApprovalClasses: entry.ApprovalClasses,
		})
	}
	return NewTrustStore(keys)
}

func Sign(decision *model.ApprovalDecision, privateKey ed25519.PrivateKey) error {
	if decision == nil {
		return errors.New("approval sign: decision is required")
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("approval sign: invalid Ed25519 private key")
	}
	digest, err := transactionreducer.ComputeApprovalDecisionDigest(*decision)
	if err != nil {
		return fmt.Errorf("approval sign: %w", err)
	}
	decision.Digest = digest
	decision.SignatureBase64 = base64.StdEncoding.EncodeToString(
		ed25519.Sign(privateKey, []byte(digest)),
	)
	if err := decision.Validate(); err != nil {
		return fmt.Errorf("approval sign: %w", err)
	}
	return nil
}

func (store *TrustStore) Verify(
	decision model.ApprovalDecision,
	pkg model.ApprovalPackage,
	now time.Time,
) error {
	if err := decision.Validate(); err != nil {
		return approvalError(err.Error(), err)
	}
	if err := pkg.Validate(); err != nil {
		return approvalError("approval package is invalid", err)
	}
	expectedDigest, err := transactionreducer.ComputeApprovalDecisionDigest(decision)
	if err != nil {
		return approvalError("cannot compute approval decision digest", err)
	}
	if decision.Digest != expectedDigest {
		return approvalError("approval decision digest does not match its content", nil)
	}
	if decision.TransactionID != pkg.TransactionID ||
		decision.PackageDigest != pkg.Digest {
		return approvalError("approval decision does not bind to the pending package", nil)
	}
	if !contains(pkg.RequiredApprovalClasses, decision.ApprovalClass) {
		return approvalError("approval class is not required by the pending package", nil)
	}
	now = now.UTC()
	if now.Before(decision.IssuedAt.Add(-2 * time.Minute)) {
		return approvalError("approval decision issuance is too far in the future", nil)
	}
	if !decision.ExpiresAt.After(now) {
		return approvalError("approval decision expired", nil)
	}
	if decision.ExpiresAt.After(decision.IssuedAt.Add(15 * time.Minute)) {
		return approvalError("approval decision lifetime exceeds 15 minutes", nil)
	}
	if decision.IssuedAt.Before(pkg.CreatedAt) ||
		decision.ExpiresAt.After(pkg.ExpiresAt) ||
		!pkg.ExpiresAt.After(now) {
		return approvalError("approval decision is outside the package validity window", nil)
	}
	if store == nil {
		return approvalError("no approval trust store is configured", nil)
	}
	key, trusted := store.keys[decision.KeyID]
	if !trusted {
		return approvalError("approval signing key is not trusted", nil)
	}
	if key.Principal != decision.Approver {
		return approvalError("approval signer does not match the trusted principal", nil)
	}
	if !contains(key.ApprovalClasses, decision.ApprovalClass) {
		return approvalError("approval signing key is not trusted for this class", nil)
	}
	signature, err := base64.StdEncoding.DecodeString(decision.SignatureBase64)
	if err != nil || !ed25519.Verify(key.PublicKey, []byte(decision.Digest), signature) {
		return approvalError("approval signature verification failed", err)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("trailing JSON value")
	}
	return err
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func approvalError(message string, cause error) *model.KernelError {
	return &model.KernelError{
		Code: model.ErrorApprovalInvalid, Operation: "verify_approval",
		Message: message, Cause: cause,
	}
}
