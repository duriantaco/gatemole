package vouch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContractsNamespaceForwardsToExistingModuleCommands(t *testing.T) {
	repo := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Main(
		[]string{"--repo", repo, "contracts", "init", "--profile", "generic"},
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("contracts init code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(repo, ".gatemole", "config.json")); err != nil {
		t.Fatalf("contracts namespace did not forward init: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Main(
		[]string{"--repo", repo, "contracts", "compile"},
		&stdout,
		&stderr,
	)
	if code != 1 || !strings.Contains(stderr.String(), "compile.no_intents") {
		t.Fatalf("contracts compile did not reach compiler: code=%d stderr=%s", code, stderr.String())
	}
}

func TestContractsNamespaceRejectsRuntimeCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Main(
		[]string{"contracts", "run"},
		&stdout,
		&stderr,
	)
	if code != 2 || !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("contracts run code=%d stderr=%s", code, stderr.String())
	}
}
