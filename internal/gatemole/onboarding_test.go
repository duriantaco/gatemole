package gatemole

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitRepoDetectsPythonProfileAndWritesLayout(t *testing.T) {
	repo := t.TempDir()
	writeText(t, filepath.Join(repo, "pyproject.toml"), `[tool.fyn.tasks]
lint = { cmd = "ruff check src/ tests/" }
typecheck = { cmd = "mypy src/" }
test = { cmd = "pytest" }
`)
	mustMkdir(t, filepath.Join(repo, "tests"))

	result, err := InitRepo(repo, "auto", false)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created {
		t.Fatal("expected config to be created")
	}
	if !contains(result.Profiles, "python") {
		t.Fatalf("expected python profile, got %#v", result.Profiles)
	}
	for _, rel := range []string{".gatemole/intents", ".gatemole/specs", ".gatemole/policy", ".gatemole/manifests", ".gatemole/artifacts", ".gatemole/build"} {
		if !dirExists(filepath.Join(repo, rel)) {
			t.Fatalf("expected directory %s", rel)
		}
	}
	policy := mustLoadPolicy(t, filepath.Join(repo, ".gatemole", "policy", "release-policy.json"))
	if policy.Version != PolicySchemaVersion || len(policy.Rules) == 0 {
		t.Fatalf("expected default release policy, got %#v", policy)
	}
	config := mustLoadConfig(t, filepath.Join(repo, ".gatemole", "config.json"))
	if config.Version != ConfigSchemaVersion {
		t.Fatalf("unexpected config version %s", config.Version)
	}
	if config.AllowedSigners == nil {
		t.Fatalf("expected allowed_signers to render as an array, got nil")
	}
	if !contains(config.Commands, "fyn run pytest --junitxml .gatemole/artifacts/junit.xml") {
		t.Fatalf("expected pytest command, got %#v", config.Commands)
	}

	second, err := InitRepo(repo, "auto", false)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created {
		t.Fatal("expected second init to be idempotent")
	}
}

func TestContractSuggestSupportsFlatPythonPackageLayout(t *testing.T) {
	repo := t.TempDir()
	writeText(t, filepath.Join(repo, "pyproject.toml"), `[project]
name = "sundae"
`)
	writeText(t, filepath.Join(repo, "sundae", "__init__.py"), "")
	writeText(t, filepath.Join(repo, "sundae", "collector.py"), "")
	writeText(t, filepath.Join(repo, "sundae", "dashboard", "server.py"), "")
	writeText(t, filepath.Join(repo, "tests", "test_dashboard.py"), "")

	suggestions, err := ContractSuggestions(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSuggestion(suggestions, "sundae.core", "sundae/*.py") {
		t.Fatalf("expected flat package core suggestion, got %#v", suggestions)
	}
	if !hasSuggestion(suggestions, "sundae.dashboard", "sundae/dashboard/**") {
		t.Fatalf("expected flat package submodule suggestion, got %#v", suggestions)
	}
}

func TestContractCreateAndManifestCreateMapChangedFilesToOwnedSpec(t *testing.T) {
	repo := initializedRepo(t)
	spec := createSampleContract(t, repo, RiskHigh)

	manifest, err := CreateManifest(repo, ManifestCreateOptions{
		TaskID:           "agent-1",
		Summary:          "change app service",
		Agent:            "codex",
		RunID:            "run-1",
		RunnerIdentity:   "https://github.com/example/repo/.github/workflows/gatemole.yml@refs/heads/main",
		RunnerOIDCIssuer: "https://token.actions.githubusercontent.com",
		ChangedFiles:     []string{"src/app/service.py", "tests/test_app.py"},
		Out:              ".gatemole/manifests/agent-1.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Change.Risk != RiskHigh {
		t.Fatalf("expected risk to inherit high from spec, got %s", manifest.Change.Risk)
	}
	if got := strings.Join(manifest.Change.SpecsTouched, ","); got != spec.ID {
		t.Fatalf("expected touched spec %s, got %s", spec.ID, got)
	}
	if !manifest.Runtime.Canary.Enabled || manifest.Runtime.Canary.InitialPercent != 5 {
		t.Fatalf("expected high-risk manifest to enable canary: %#v", manifest.Runtime.Canary)
	}
	loaded := mustLoadManifest(t, filepath.Join(repo, ".gatemole", "manifests", "agent-1.json"))
	if loaded.Task.ID != "agent-1" {
		t.Fatalf("manifest was not written: %#v", loaded.Task)
	}
	if loaded.Agent.RunnerIdentity == "" || loaded.Agent.RunnerOIDCIssuer == "" {
		t.Fatalf("manifest did not preserve runner identity: %#v", loaded.Agent)
	}
	errors := CompileManifestPipeline(mustLoadSpecs(t, repo), loaded).ManifestErrors
	if len(errors) != 0 {
		t.Fatalf("expected manifest check to pass, got %#v", errors)
	}
}

func TestManifestCreateRejectsRiskDowngrade(t *testing.T) {
	repo := initializedRepo(t)
	createSampleContract(t, repo, RiskHigh)

	_, err := CreateManifest(repo, ManifestCreateOptions{
		TaskID:       "agent-1",
		Summary:      "change app service",
		Agent:        "codex",
		RunID:        "run-1",
		Risk:         RiskLow,
		ChangedFiles: []string{"src/app/service.py"},
		Out:          ".gatemole/manifests/agent-1.json",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot be lower") {
		t.Fatalf("expected risk downgrade error, got %v", err)
	}
}

func TestManifestCreateAllowsUnownedFilesForCompilerTraceabilityBlock(t *testing.T) {
	repo := initializedRepo(t)
	createSampleContract(t, repo, RiskMedium)

	manifest, err := CreateManifest(repo, ManifestCreateOptions{
		TaskID:       "agent-1",
		Summary:      "change unowned file",
		Agent:        "codex",
		RunID:        "run-1",
		ChangedFiles: []string{"docs/README.md"},
		Out:          ".gatemole/manifests/agent-1.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Change.SpecsTouched) != 0 {
		t.Fatalf("expected no matched specs, got %#v", manifest.Change.SpecsTouched)
	}
	errors := CompileManifestPipeline(mustLoadSpecs(t, repo), manifest).ManifestErrors
	if !containsOnboardingSubstring(errors, "is not owned by any spec") {
		t.Fatalf("expected unowned file manifest error, got %#v", errors)
	}
}

func TestManifestCreateUsesConfiguredBaseRef(t *testing.T) {
	repo := initializedRepo(t)
	createSampleContract(t, repo, RiskMedium)
	writeText(t, filepath.Join(repo, "src", "app", "service.py"), "def service():\n    return 'old'\n")
	runGit(t, repo, "init")
	runGit(t, repo, "checkout", "-b", "trunk")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "-c", "user.email=gatemole@example.invalid", "-c", "user.name=Gatemole Test", "commit", "-m", "initial")
	runGit(t, repo, "checkout", "-b", "feature")
	writeText(t, filepath.Join(repo, "src", "app", "service.py"), "def service():\n    return 'new'\n")
	runGit(t, repo, "add", "src/app/service.py")
	runGit(t, repo, "-c", "user.email=gatemole@example.invalid", "-c", "user.name=Gatemole Test", "commit", "-m", "change service")
	t.Setenv("GATEMOLE_BASE_REF", "trunk")

	manifest, err := CreateManifest(repo, ManifestCreateOptions{
		TaskID:  "agent-1",
		Summary: "change app service",
		Agent:   "codex",
		RunID:   "run-1",
		Out:     ".gatemole/manifests/agent-1.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(manifest.Change.ChangedFiles, "src/app/service.py") {
		t.Fatalf("expected changed file from trunk...HEAD, got %#v", manifest.Change.ChangedFiles)
	}
	if got := strings.Join(manifest.Change.SpecsTouched, ","); got != "app.service" {
		t.Fatalf("expected app.service from auto-detected base ref, got %s", got)
	}
}

func TestAttachArtifactInfersCoveredObligations(t *testing.T) {
	repo := initializedRepo(t)
	spec := createSampleContract(t, repo, RiskMedium)
	manifest, err := CreateManifest(repo, ManifestCreateOptions{
		TaskID:       "agent-1",
		Summary:      "change app service",
		Agent:        "codex",
		RunID:        "run-1",
		ChangedFiles: []string{"src/app/service.py"},
		Out:          ".gatemole/manifests/agent-1.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	behaviorID := obligationID(t, spec, ObligationBehavior, "service returns stable JSON")
	writeText(t, filepath.Join(repo, ".gatemole", "artifacts", "behavior.json"), `{"status":"pass","obligations":["`+behaviorID+`"]}`)

	updated, artifact, err := AttachArtifact(repo, AttachArtifactOptions{
		ManifestPath: ".gatemole/manifests/agent-1.json",
		ID:           "behavior",
		Kind:         EvidenceBehaviorTrace,
		Path:         ".gatemole/artifacts/behavior.json",
		Command:      "contract probe",
		ExitCode:     0,
		Out:          ".gatemole/manifests/agent-1.with-artifact.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Producer != manifest.Agent.Name {
		t.Fatalf("expected producer to default to agent, got %q", artifact.Producer)
	}
	if !contains(artifact.Obligations, behaviorID) {
		t.Fatalf("expected inferred obligation %s, got %#v", behaviorID, artifact.Obligations)
	}
	if artifact.SHA256 != sha256Hex([]byte(`{"status":"pass","obligations":["`+behaviorID+`"]}`)) {
		t.Fatalf("expected artifact sha256 to be recorded, got %q", artifact.SHA256)
	}
	if len(updated.Verification.Artifacts) != 1 {
		t.Fatalf("expected one artifact, got %#v", updated.Verification.Artifacts)
	}
}

func TestAttachArtifactInfersSARIFSecurityObligations(t *testing.T) {
	repo := initializedRepo(t)
	spec := createSampleContract(t, repo, RiskMedium)
	_, err := CreateManifest(repo, ManifestCreateOptions{
		TaskID:       "agent-1",
		Summary:      "change app service",
		Agent:        "codex",
		RunID:        "run-1",
		ChangedFiles: []string{"src/app/service.py"},
		Out:          ".gatemole/manifests/agent-1.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	securityID := obligationID(t, spec, ObligationSecurity, "project paths stay inside repo")
	writeSARIFArtifact(t, repo, ".gatemole/artifacts/security.sarif", sarifSecurityLog(securityID, nil))

	_, artifact, err := AttachArtifact(repo, AttachArtifactOptions{
		ManifestPath: ".gatemole/manifests/agent-1.json",
		ID:           "security",
		Kind:         EvidenceSecurityCheck,
		Path:         ".gatemole/artifacts/security.sarif",
		Command:      "semgrep scan --sarif",
		ExitCode:     0,
		Out:          ".gatemole/manifests/agent-1.with-security.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(artifact.Obligations, securityID) {
		t.Fatalf("expected inferred SARIF obligation %s, got %#v", securityID, artifact.Obligations)
	}
}

func TestAttachArtifactRejectsNonZeroExitAndPathEscape(t *testing.T) {
	repo := initializedRepo(t)
	createSampleContract(t, repo, RiskMedium)
	_, err := CreateManifest(repo, ManifestCreateOptions{
		TaskID:       "agent-1",
		Summary:      "change app service",
		Agent:        "codex",
		RunID:        "run-1",
		ChangedFiles: []string{"src/app/service.py"},
		Out:          ".gatemole/manifests/agent-1.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	writeText(t, filepath.Join(repo, ".gatemole", "artifacts", "behavior.json"), `{"status":"pass"}`)
	_, _, err = AttachArtifact(repo, AttachArtifactOptions{
		ManifestPath: ".gatemole/manifests/agent-1.json",
		ID:           "behavior",
		Kind:         EvidenceBehaviorTrace,
		Path:         ".gatemole/artifacts/behavior.json",
		ExitCode:     1,
		Out:          ".gatemole/manifests/agent-1.json",
	})
	if err == nil || !strings.Contains(err.Error(), "exit code") {
		t.Fatalf("expected exit code error, got %v", err)
	}
	_, _, err = AttachArtifact(repo, AttachArtifactOptions{
		ManifestPath: ".gatemole/manifests/agent-1.json",
		ID:           "behavior",
		Kind:         EvidenceBehaviorTrace,
		Path:         "../outside.json",
		ExitCode:     0,
		Out:          ".gatemole/manifests/agent-1.json",
	})
	if err == nil || !strings.Contains(err.Error(), "escapes repo") {
		t.Fatalf("expected path escape error, got %v", err)
	}
}

func TestCLIInitContractManifestAndAttachArtifact(t *testing.T) {
	repo := t.TempDir()
	writeText(t, filepath.Join(repo, "pyproject.toml"), `[tool.pytest.ini_options]
testpaths = ["tests"]
`)
	mustMkdir(t, filepath.Join(repo, "src", "app"))
	var stdout, stderr bytes.Buffer
	code := Main([]string{"--repo", repo, "init", "--profile", "python"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("init failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Main([]string{
		"--repo", repo,
		"contract", "create",
		"--name", "app.service",
		"--owner", "platform",
		"--risk", "medium",
		"--paths", "src/app/**",
		"--behavior", "service returns stable JSON",
		"--required-test", "service json contract is stable",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("contract create failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Main([]string{
		"--repo", repo,
		"manifest", "create",
		"--task-id", "agent-1",
		"--summary", "change app service",
		"--agent", "codex",
		"--run-id", "run-1",
		"--changed-file", "src/app/service.py",
		"--out", ".gatemole/manifests/run.json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("manifest create failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	spec := mustLoadSpecs(t, repo)["app.service"]
	behaviorID := obligationID(t, spec, ObligationBehavior, "service returns stable JSON")
	writeText(t, filepath.Join(repo, ".gatemole", "artifacts", "behavior.json"), `{"status":"pass","obligations":["`+behaviorID+`"]}`)
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{
		"--repo", repo,
		"manifest", "attach-artifact",
		"--manifest", ".gatemole/manifests/run.json",
		"--id", "behavior",
		"--kind", string(EvidenceBehaviorTrace),
		"--path", ".gatemole/artifacts/behavior.json",
		"--exit-code", "0",
		"--out", ".gatemole/manifests/run.with-artifact.json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("attach-artifact failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	manifest := mustLoadManifest(t, filepath.Join(repo, ".gatemole", "manifests", "run.with-artifact.json"))
	if len(manifest.Verification.Artifacts) != 1 {
		t.Fatalf("expected one attached artifact, got %#v", manifest.Verification.Artifacts)
	}
}

func initializedRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	writeText(t, filepath.Join(repo, "pyproject.toml"), `[tool.pytest.ini_options]
testpaths = ["tests"]
`)
	mustMkdir(t, filepath.Join(repo, "src", "app"))
	mustMkdir(t, filepath.Join(repo, "tests"))
	if _, err := InitRepo(repo, "auto", false); err != nil {
		t.Fatal(err)
	}
	return repo
}

func createSampleContract(t *testing.T, repo string, risk Risk) Spec {
	t.Helper()
	spec, _, _, err := CreateContract(repo, Intent{
		Feature:       "app.service",
		Owner:         "platform",
		OwnedPaths:    []string{"src/app/**", "tests/test_app.py"},
		Risk:          risk,
		Behavior:      []string{"service returns stable JSON"},
		Security:      []string{"project paths stay inside repo"},
		RequiredTests: []string{"service json contract is stable"},
		RuntimeMetrics: []string{
			"app.service.requests",
		},
		Rollback: SpecRollback{Strategy: "revert_change"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func writeText(t *testing.T, path string, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(output))
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustLoadConfig(t *testing.T, path string) Config {
	t.Helper()
	config, err := LoadJSON[Config](path)
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func mustLoadPolicy(t *testing.T, path string) ReleasePolicy {
	t.Helper()
	policy, err := LoadJSON[ReleasePolicy](path)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func mustLoadSpecs(t *testing.T, repo string) map[string]Spec {
	t.Helper()
	specs, err := LoadSpecs(repo)
	if err != nil {
		t.Fatal(err)
	}
	return specs
}

func containsOnboardingSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

func hasSuggestion(suggestions []ContractSuggestion, name string, ownedPath string) bool {
	for _, suggestion := range suggestions {
		if suggestion.Name == name && contains(suggestion.OwnedPaths, ownedPath) {
			return true
		}
	}
	return false
}
