package gatemole

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTryUsesSnapshotByDefault(t *testing.T) {
	repo := bootstrapFixture(t)
	var stdout, stderr bytes.Buffer

	code := Main([]string{"--repo", repo, "try"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("try failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"Gatemole Try",
		"Mode: temp snapshot",
		"contracts drafted: 1",
		"obligations compiled: 5",
		"HIGH auth.password_reset",
		"write drafts with: gatemole try --repo",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected try output to contain %q, got:\n%s", want, out)
		}
	}
	if fileExists(filepath.Join(repo, ".gatemole", "intents", "auth.password_reset.yaml")) {
		t.Fatal("try wrote generated intents into source repo without --write")
	}
	if fileExists(filepath.Join(repo, ".gatemole", "build", "obligations.ir.json")) {
		t.Fatal("try wrote compiler output into source repo without --write")
	}
}

func TestTryWriteModeWritesGatemoleFiles(t *testing.T) {
	repo := bootstrapFixture(t)
	var stdout, stderr bytes.Buffer

	code := Main([]string{"--repo", repo, "try", "--write"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("try --write failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Mode: write to source repo") || !strings.Contains(out, "wrote: .gatemole/") {
		t.Fatalf("expected write-mode output, got:\n%s", out)
	}
	if !fileExists(filepath.Join(repo, ".gatemole", "intents", "auth.password_reset.yaml")) {
		t.Fatal("try --write did not write generated intent")
	}
	if !fileExists(filepath.Join(repo, ".gatemole", "build", "obligations.ir.json")) {
		t.Fatal("try --write did not compile obligations")
	}
}

func TestTryJSONIsStableAndNonDestructive(t *testing.T) {
	repo := bootstrapFixture(t)
	var stdout, stderr bytes.Buffer

	code := Main([]string{"--repo", repo, "--json", "try"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("try --json failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	var result TryResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("try --json emitted invalid JSON: %v\n%s", err, stdout.String())
	}
	if result.Version != tryResultVersion || result.Mode != "snapshot" || result.Drafts != 1 || result.CompiledObligations != 5 {
		t.Fatalf("unexpected try JSON result: %#v", result)
	}
	if len(result.TopDrafts) != 1 || result.TopDrafts[0].Edit != ".gatemole/intents/auth.password_reset.yaml" {
		t.Fatalf("expected top draft edit path in JSON, got %#v", result.TopDrafts)
	}
	if fileExists(filepath.Join(repo, ".gatemole", "intents", "auth.password_reset.yaml")) {
		t.Fatal("try --json wrote generated intent into source repo")
	}
}

func TestTryImportsJUnitAndShowsGateDecision(t *testing.T) {
	repo := bootstrapFixture(t)
	writeText(t, filepath.Join(repo, ".gatemole", "artifacts", "pytest.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="pytest" tests="1" failures="0" errors="0" skipped="0">
  <testcase classname="tests.auth.test_password_reset" name="test_token_expiry" file="tests/auth/test_password_reset.py"></testcase>
</testsuite>
`)
	var stdout, stderr bytes.Buffer

	code := Main([]string{"--repo", repo, "try", "--junit", ".gatemole/artifacts/pytest.xml"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("try --junit failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"Evidence:",
		"JUnit linked: 1 required-test obligations",
		"Gate:",
		"decision: block",
		"covered: 1/5 obligations",
		"Why blocked:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected try --junit output to contain %q, got:\n%s", want, out)
		}
	}
	if fileExists(filepath.Join(repo, ".gatemole", "evidence", "manifest.json")) {
		t.Fatal("try --junit wrote evidence manifest into source repo without --write")
	}
}

func TestTryImportsIgnoredJUnitArtifactFromGitWorktree(t *testing.T) {
	repo := bootstrapFixture(t)
	writeText(t, filepath.Join(repo, ".gitignore"), ".gatemole/\n")
	writeText(t, filepath.Join(repo, ".gatemole", "artifacts", "pytest.xml"), `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="pytest" tests="1" failures="0" errors="0" skipped="0">
  <testcase classname="tests.auth.test_password_reset" name="test_token_expiry" file="tests/auth/test_password_reset.py"></testcase>
</testsuite>
`)
	cmd := exec.Command("git", "init")
	cmd.Dir = repo
	if err := cmd.Run(); err != nil {
		t.Skipf("git init failed: %v", err)
	}
	var stdout, stderr bytes.Buffer

	code := Main([]string{"--repo", repo, "try", "--junit", ".gatemole/artifacts/pytest.xml"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("try --junit failed for ignored JUnit artifact: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "JUnit linked: 1 required-test obligations") {
		t.Fatalf("expected ignored JUnit artifact to be copied into snapshot, got:\n%s", stdout.String())
	}
	if fileExists(filepath.Join(repo, ".gatemole", "evidence", "manifest.json")) {
		t.Fatal("try --junit wrote evidence manifest into source repo without --write")
	}
}
