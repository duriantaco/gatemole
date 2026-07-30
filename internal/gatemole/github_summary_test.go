package gatemole

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGateGitHubSummaryWritesStepSummary(t *testing.T) {
	repo, manifestPath, _ := writeFullyCoveredUIScenario(t, nil)
	summaryPath := filepath.Join(t.TempDir(), "summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summaryPath)
	var stdout, stderr bytes.Buffer

	code := Main([]string{"--repo", repo, "--manifest", manifestPath, "gate", "--github-summary"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gate --github-summary failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Release decision: auto_merge") {
		t.Fatalf("expected normal gate output, got %s", stdout.String())
	}
	data, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatal(err)
	}
	summary := string(data)
	for _, want := range []string{
		"# Gatemole Gate",
		"| Decision | `auto_merge` |",
		"| Obligations | `5/5 covered` |",
		"### `ui.copy`",
		"| Covered | `required_test.button_label_renders` | `test_coverage` |",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("expected summary to contain %q, got:\n%s", want, summary)
		}
	}
}

func TestGateGitHubSummaryRequiresEnvironment(t *testing.T) {
	repo, manifestPath, _ := writeFullyCoveredUIScenario(t, nil)
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	var stdout, stderr bytes.Buffer

	code := Main([]string{"--repo", repo, "--manifest", manifestPath, "gate", "--github-summary"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected missing summary env to fail: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "GITHUB_STEP_SUMMARY") {
		t.Fatalf("expected missing GITHUB_STEP_SUMMARY error, got %s", stderr.String())
	}
}
