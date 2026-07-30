package gatemole

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/duriantaco/gatemole/internal/kernel/approval"
)

func TestApprovalKeygenCreatesRestrictedKeyAndLoadableTrust(t *testing.T) {
	repo := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := approvalCommand(
		repo,
		[]string{
			"keygen",
			"--key-id", "key:alice",
			"--principal", "human:alice",
			"--class", "security-reviewer",
			"--private-key", "secrets/approval.key",
			"--trust-file", "config/approval-trust.json",
		},
		false,
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("approval keygen code=%d stderr=%s", code, stderr.String())
	}
	keyPath := filepath.Join(repo, "secrets", "approval.key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key permissions=%o, want 600", info.Mode().Perm())
	}
	trust, err := approval.LoadTrustFile(filepath.Join(repo, "config", "approval-trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	if trust.Len() != 1 {
		t.Fatalf("trusted keys=%d, want 1", trust.Len())
	}

	stderr.Reset()
	code = approvalCommand(
		repo,
		[]string{
			"keygen",
			"--key-id", "key:alice",
			"--principal", "human:alice",
			"--class", "security-reviewer",
			"--private-key", "secrets/approval.key",
			"--trust-file", "config/approval-trust.json",
		},
		false,
		&stdout,
		&stderr,
	)
	if code != 1 {
		t.Fatalf("approval keygen overwrote existing key: code=%d", code)
	}
}
