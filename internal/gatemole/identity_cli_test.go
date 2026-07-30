package gatemole

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/identity"
	"github.com/duriantaco/gatemole/internal/kernel/model"
)

func TestIdentityCLIKeygenAndIssue(t *testing.T) {
	repo := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := identityCommand(
		repo,
		[]string{
			"keygen",
			"--key-id", "key:local-issuer",
			"--issuer", "https://issuer.example.invalid",
			"--audience", "gatemole-api",
			"--private-key", "secrets/identity.key",
			"--trust-file", "config/identity-trust.json",
		},
		false,
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("identity keygen code=%d stderr=%s", code, stderr.String())
	}
	keyPath := filepath.Join(repo, "secrets", "identity.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key permissions=%o, want 600", info.Mode().Perm())
	}
	verifier, err := identity.LoadTrustFile(
		filepath.Join(repo, "config", "identity-trust.json"),
	)
	if err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	code = identityCommand(
		repo,
		[]string{
			"issue",
			"--key", keyPath,
			"--key-id", "key:local-issuer",
			"--issuer", "https://issuer.example.invalid",
			"--audience", "gatemole-api",
			"--principal", "operator:alice",
			"--kind", "operator",
			"--namespaces", "team-a,team-b",
			"--roles", "viewer,operator",
			"--ttl", "10m",
		},
		false,
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("identity issue code=%d stderr=%s", code, stderr.String())
	}
	token := strings.TrimSpace(stdout.String())
	verified, err := verifier.Verify(token, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if verified.Principal.ID != "operator:alice" ||
		verified.Principal.Kind != model.PrincipalOperator ||
		!verified.AllowsNamespace("team-b") ||
		!verified.HasRole("operator") {
		t.Fatalf("unexpected issued identity: %#v", verified)
	}

	stderr.Reset()
	code = identityCommand(
		repo,
		[]string{
			"keygen",
			"--key-id", "key:local-issuer",
			"--issuer", "https://issuer.example.invalid",
			"--audience", "gatemole-api",
			"--private-key", "secrets/identity.key",
			"--trust-file", "config/identity-trust.json",
		},
		false,
		&stdout,
		&stderr,
	)
	if code != 1 {
		t.Fatalf("identity keygen overwrote existing material: code=%d", code)
	}
}
