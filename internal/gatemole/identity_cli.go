package gatemole

import (
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
	"strings"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/identity"
	"github.com/duriantaco/gatemole/internal/kernel/model"
)

func identityCommand(repo string, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: gatemole identity keygen|issue")
		return 2
	}
	switch args[0] {
	case "keygen":
		return identityKeygen(repo, args[1:], jsonOut, stdout, stderr)
	case "issue":
		return identityIssue(repo, args[1:], jsonOut, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "identity: unknown command %q\n", args[0])
		return 2
	}
}

func identityKeygen(repo string, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("identity keygen", flag.ContinueOnError)
	flags.SetOutput(stderr)
	keyID := flags.String("key-id", "", "stable OIDC signing key identifier")
	issuer := flags.String("issuer", "", "HTTPS issuer URL")
	audience := flags.String("audience", "", "Gatemole access-token audience")
	privateKeyPath := flags.String("private-key", "", "new private key output path")
	trustFilePath := flags.String("trust-file", "", "new daemon OIDC trust document")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *keyID == "" || *issuer == "" || *audience == "" ||
		*privateKeyPath == "" || *trustFilePath == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "identity keygen requires --key-id, --issuer, --audience, --private-key, and --trust-file")
		return 2
	}
	privatePath := resolveRepoPath(repo, *privateKeyPath)
	trustPath := resolveRepoPath(repo, *trustFilePath)
	if privatePath == trustPath {
		fmt.Fprintln(stderr, "identity keygen requires different private-key and trust-file paths")
		return 2
	}
	for _, target := range []string{privatePath, trustPath} {
		if _, err := os.Lstat(target); err == nil {
			fmt.Fprintf(stderr, "identity keygen refuses to overwrite %s\n", target)
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
	jwk, err := identity.Ed25519JWK(*keyID, publicKey)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	document := identity.TrustDocument{
		Version:                 identity.TrustDocumentVersion,
		Issuer:                  *issuer,
		Audiences:               []string{*audience},
		ClockSkewSeconds:        60,
		MaxTokenLifetimeSeconds: 3600,
		JWKS:                    identity.JWKS{Keys: []identity.JWK{jwk}},
	}
	if _, err := identity.NewVerifier(document); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	trustData, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	trustData = append(trustData, '\n')
	if err := writeNewFile(
		privatePath,
		[]byte(base64.StdEncoding.EncodeToString(privateKey)+"\n"),
		0o600,
	); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := writeNewFile(trustPath, trustData, 0o644); err != nil {
		fmt.Fprintf(stderr, "%v; private key was created at %s and was not removed\n", err, privatePath)
		return 1
	}
	result := struct {
		KeyID          string `json:"key_id"`
		Issuer         string `json:"issuer"`
		Audience       string `json:"audience"`
		PrivateKeyPath string `json:"private_key_path"`
		TrustFilePath  string `json:"trust_file_path"`
	}{
		KeyID: *keyID, Issuer: *issuer, Audience: *audience,
		PrivateKeyPath: privatePath, TrustFilePath: trustPath,
	}
	if jsonOut {
		return renderCommandJSON(result, stdout, stderr)
	}
	fmt.Fprintf(
		stdout,
		"Created local OIDC signing key %s\nPrivate key: %s\nTrust file: %s\n",
		*keyID, privatePath, trustPath,
	)
	return 0
}

func identityIssue(repo string, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("identity issue", flag.ContinueOnError)
	flags.SetOutput(stderr)
	keyPath := flags.String("key", "", "base64 Ed25519 private key file")
	keyID := flags.String("key-id", "", "OIDC signing key identifier")
	issuer := flags.String("issuer", "", "HTTPS token issuer")
	audience := flags.String("audience", "", "token audience")
	subject := flags.String("subject", "", "OIDC subject; defaults to the principal ID")
	principalID := flags.String("principal", "", "Gatemole principal ID")
	kind := flags.String("kind", "", "human, service, or operator")
	namespaceValues := flags.String("namespaces", "", "comma-separated authorized namespaces")
	roleValues := flags.String("roles", "", "comma-separated Gatemole roles")
	ttl := flags.Duration("ttl", 15*time.Minute, "token lifetime, between 1 minute and 1 hour")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *keyPath == "" || *keyID == "" || *issuer == "" || *audience == "" ||
		*principalID == "" || *kind == "" || *namespaceValues == "" ||
		*roleValues == "" || *ttl < time.Minute || *ttl > time.Hour ||
		flags.NArg() != 0 {
		fmt.Fprintln(stderr, "identity issue requires --key, --key-id, --issuer, --audience, --principal, --kind, --namespaces, and --roles; --ttl must be 1m through 1h")
		return 2
	}
	if *subject == "" {
		*subject = *principalID
	}
	privateKey, err := readIdentityPrivateKey(resolveRepoPath(repo, *keyPath))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	now := time.Now().UTC().Truncate(time.Second)
	token, err := identity.IssueEd25519(identity.IssueRequest{
		KeyID:       *keyID,
		Issuer:      *issuer,
		Audience:    *audience,
		Subject:     *subject,
		PrincipalID: *principalID,
		Kind:        model.PrincipalKind(*kind),
		Namespaces:  splitCommaValues(*namespaceValues),
		Roles:       splitCommaValues(*roleValues),
		IssuedAt:    now,
		ExpiresAt:   now.Add(*ttl),
		TokenID:     "token:" + hex.EncodeToString(nonce),
	}, privateKey)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if jsonOut {
		return renderCommandJSON(struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
		}{Token: token, ExpiresAt: now.Add(*ttl)}, stdout, stderr)
	}
	fmt.Fprintln(stdout, token)
	return 0
}

func readIdentityPrivateKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("OIDC private key is not a regular file: %s", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("OIDC private key permissions must be 0600 or stricter: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(decoded) != ed25519.PrivateKeySize {
		return nil, errors.New("OIDC private key is not a base64 Ed25519 private key")
	}
	return ed25519.PrivateKey(decoded), nil
}
