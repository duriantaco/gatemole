package vouch

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/approval"
	"github.com/duriantaco/vouch/internal/kernel/model"
)

func approvalCommand(repo string, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "keygen" {
		fmt.Fprintln(stderr, "usage: vouch approval keygen --key-id ID --principal ID --class CLASS --private-key FILE --trust-file FILE")
		return 2
	}
	return approvalKeygen(repo, args[1:], jsonOut, stdout, stderr)
}

func approvalKeygen(repo string, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("approval keygen", flag.ContinueOnError)
	flags.SetOutput(stderr)
	keyID := flags.String("key-id", "", "stable public key identifier")
	principalID := flags.String("principal", "", "human approver principal ID")
	issuer := flags.String("issuer", "", "identity issuer bound to the approver")
	class := flags.String("class", "", "approval class trusted for this key")
	privateKeyPath := flags.String("private-key", "", "new private key output path")
	trustFilePath := flags.String("trust-file", "", "new daemon trust document output path")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *keyID == "" || *principalID == "" || *class == "" ||
		*privateKeyPath == "" || *trustFilePath == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "approval keygen requires --key-id, --principal, --class, --private-key, and --trust-file")
		return 2
	}
	privatePath := resolveRepoPath(repo, *privateKeyPath)
	trustPath := resolveRepoPath(repo, *trustFilePath)
	if privatePath == trustPath {
		fmt.Fprintln(stderr, "approval keygen requires different private-key and trust-file paths")
		return 2
	}
	for _, path := range []string{privatePath, trustPath} {
		if _, err := os.Lstat(path); err == nil {
			fmt.Fprintf(stderr, "approval keygen refuses to overwrite %s\n", path)
			return 1
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	document := approval.TrustDocument{
		Version: approval.TrustDocumentVersion,
		Keys: []approval.TrustDocumentKey{{
			KeyID:           *keyID,
			PublicKeyBase64: base64.StdEncoding.EncodeToString(publicKey),
			Principal: model.Principal{
				ID: *principalID, Kind: model.PrincipalHuman, Issuer: *issuer,
			},
			ApprovalClasses: []string{*class},
		}},
	}
	if _, err := approval.NewTrustStore([]approval.TrustedKey{{
		KeyID: *keyID, PublicKey: publicKey, Principal: document.Keys[0].Principal,
		ApprovalClasses: []string{*class},
	}}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	trustData, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	trustData = append(trustData, '\n')
	if err := writeNewFile(privatePath, []byte(base64.StdEncoding.EncodeToString(privateKey)+"\n"), 0o600); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeNewFile(trustPath, trustData, 0o644); err != nil {
		fmt.Fprintf(stderr, "%v; private key was created at %s and was not removed\n", err, privatePath)
		return 1
	}
	result := struct {
		KeyID          string `json:"key_id"`
		PrivateKeyPath string `json:"private_key_path"`
		TrustFilePath  string `json:"trust_file_path"`
	}{
		KeyID: *keyID, PrivateKeyPath: privatePath, TrustFilePath: trustPath,
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Created approval key %s\nPrivate key: %s\nTrust file: %s\n", *keyID, privatePath, trustPath)
	return 0
}

func transactionApprove(
	repo string,
	args []string,
	jsonOut bool,
	stdout, stderr io.Writer,
	newClient transactionClientFactory,
) int {
	flags := flag.NewFlagSet("tx approve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	namespace := flags.String("namespace", "", "transaction namespace")
	id := flags.String("id", "", "transaction ID")
	keyPath := flags.String("key", "", "base64 Ed25519 private key file")
	keyID := flags.String("key-id", "", "trusted approval key ID")
	approverID := flags.String("approver", "", "human approver principal ID")
	issuer := flags.String("issuer", "", "identity issuer bound to the trusted principal")
	class := flags.String("class", "", "approval class")
	approvalID := flags.String("approval-id", "", "outstanding approval ID; inferred when only one exists")
	decisionValue := flags.String("decision", string(model.ApprovalApprove), "approve, reject, or revise")
	reason := flags.String("reason", "", "required for reject or revise")
	ttl := flags.Duration("ttl", 5*time.Minute, "signed decision lifetime, maximum 15 minutes")
	socket := flags.String("socket", defaultKernelSocket(repo), "gatemoled Unix socket")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" || *id == "" || *keyPath == "" || *keyID == "" ||
		*approverID == "" || *class == "" || flags.NArg() != 0 ||
		*ttl <= 0 || *ttl > 15*time.Minute {
		fmt.Fprintln(stderr, "tx approve requires --namespace, --id, --key, --key-id, --approver, and --class; --ttl must be within 15 minutes")
		return 2
	}
	kind := model.ApprovalDecisionKind(*decisionValue)
	if kind != model.ApprovalApprove && kind != model.ApprovalReject && kind != model.ApprovalRevise {
		fmt.Fprintln(stderr, "tx approve --decision must be approve, reject, or revise")
		return 2
	}
	if kind == model.ApprovalApprove && strings.TrimSpace(*reason) != "" {
		fmt.Fprintln(stderr, "tx approve --reason is only valid for reject or revise")
		return 2
	}
	if kind != model.ApprovalApprove && strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(stderr, "tx approve reject and revise decisions require --reason")
		return 2
	}
	privateKey, err := readApprovalPrivateKey(resolveRepoPath(repo, *keyPath))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	client := newClient(*socket)
	projection, err := client.GetTransaction(context.Background(), *namespace, *id)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if projection.ApprovalPackage == nil ||
		projection.Transaction.State != model.TransactionPendingApproval {
		fmt.Fprintln(stderr, "transaction does not have a pending immutable approval package")
		return 1
	}
	if *approvalID == "" {
		if len(projection.Transaction.OutstandingApprovalIDs) != 1 {
			fmt.Fprintln(stderr, "tx approve requires --approval-id when more than one approval is outstanding")
			return 2
		}
		*approvalID = projection.Transaction.OutstandingApprovalIDs[0]
	}
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	now := time.Now().UTC()
	expiresAt := now.Add(*ttl)
	if projection.ApprovalPackage.ExpiresAt.Before(expiresAt) {
		expiresAt = projection.ApprovalPackage.ExpiresAt
	}
	nonce := hex.EncodeToString(nonceBytes)
	decision := model.ApprovalDecision{
		Version:       model.ApprovalDecisionVersion,
		ID:            "approval-decision:" + nonce,
		TransactionID: *id,
		ApprovalID:    *approvalID,
		PackageDigest: projection.Transaction.ApprovalPackageDigest,
		ApprovalClass: *class,
		Decision:      kind,
		Reason:        strings.TrimSpace(*reason),
		Approver: model.Principal{
			ID: *approverID, Kind: model.PrincipalHuman, Issuer: *issuer,
		},
		KeyID:     *keyID,
		IssuedAt:  now,
		ExpiresAt: expiresAt,
		Nonce:     nonce,
	}
	if err := approval.Sign(&decision, privateKey); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	next, err := client.ResolveTransactionApproval(
		context.Background(), *namespace, *id,
		projection.Transaction.EventSequence, decision,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(next, stdout, stderr)
	}
	fmt.Fprintf(stdout, "Recorded signed %s decision for %s: state=%s\n", kind, *id, next.Transaction.State)
	return 0
}

func readApprovalPrivateKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("approval private key is not a regular file: %s", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("approval private key permissions must be 0600 or stricter: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("approval private key is not a base64 Ed25519 private key")
	}
	return ed25519.PrivateKey(decoded), nil
}

func resolveRepoPath(repo, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(repo, path)
}

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}
