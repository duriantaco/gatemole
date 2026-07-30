package vouch

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not resolve caller path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func TestBlockedManifestBlocksForMissingEvidence(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	evidence, err := CollectEvidence(demo, filepath.Join(demo, ".gatemole", "manifests", "blocked.json"))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !contains(evidence.MissingTests["auth.password_reset"], "token cannot be reused") {
		t.Fatalf("missing token replay test was not reported: %#v", evidence.MissingTests["auth.password_reset"])
	}
	if !contains(evidence.MissingSecurity["auth.password_reset"], "reset token is never logged") {
		t.Fatalf("missing token logging invariant was not reported: %#v", evidence.MissingSecurity["auth.password_reset"])
	}
	if !missingObligation(evidence, "auth.password_reset", ObligationRuntimeSignal, "password_reset.requested") {
		t.Fatalf("missing runtime obligation was not reported: %#v", evidence.MissingObligations["auth.password_reset"])
	}
	if missingObligation(evidence, "auth.password_reset", ObligationBehavior, "reset token is single-use") {
		t.Fatalf("behavior obligations should be covered in blocked fixture")
	}
}

func TestPassingManifestCanaries(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	evidence, err := CollectEvidence(demo, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "canary" {
		t.Fatalf("expected canary, got %s", evidence.Decision)
	}
	if got := len(evidence.MissingTests["auth.password_reset"]); got != 0 {
		t.Fatalf("expected no missing tests, got %d", got)
	}
	if got := len(evidence.MissingSecurity["auth.password_reset"]); got != 0 {
		t.Fatalf("expected no missing security checks, got %d", got)
	}
	if got := len(evidence.InvalidEvidence); got != 0 {
		t.Fatalf("expected no invalid evidence, got %#v", evidence.InvalidEvidence)
	}
	if !artifactCovered(evidence, "test-results", "auth.password_reset.required_test.token_cannot_be_reused") {
		t.Fatalf("expected JUnit artifact to cover token replay obligation: %#v", evidence.ArtifactResults)
	}
	first := RenderEvidence(evidence)
	second := RenderEvidence(evidence)
	if first != second {
		t.Fatal("evidence report render should be deterministic")
	}
}

func TestIntentCompilesToSpecAndIR(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	tmp := t.TempDir()
	specOut := filepath.Join(tmp, "auth.password_reset.json")
	irOut := filepath.Join(tmp, "auth.password_reset.ir.json")
	spec, err := CompileIntentFile(filepath.Join(demo, ".gatemole", "intents", "auth.password_reset.yaml"), specOut)
	if err != nil {
		t.Fatal(err)
	}
	if spec.ID != "auth.password_reset" {
		t.Fatalf("expected auth.password_reset spec, got %s", spec.ID)
	}
	ir, err := BuildIRFile(specOut, irOut)
	if err != nil {
		t.Fatal(err)
	}
	if ir.Version != "gatemole.ir.v0" {
		t.Fatalf("unexpected IR version %s", ir.Version)
	}
	if !hasObligation(ir, ObligationSecurity, EvidenceSecurityCheck, "reset token is never logged") {
		t.Fatalf("expected IR security obligation for token logging: %#v", ir.Obligations)
	}
	if !hasObligationID(ir, "auth.password_reset.security.reset_token_is_never_logged") {
		t.Fatalf("expected stable semantic obligation id for token logging: %#v", ir.Obligations)
	}
	if !hasObligation(ir, ObligationRollback, EvidenceRollbackPlan, "feature_flag:password_reset_v2") {
		t.Fatalf("expected IR rollback obligation: %#v", ir.Obligations)
	}
	if !contains(ir.RequiredChecks, "canary_required") {
		t.Fatalf("expected high-risk IR to require canary: %#v", ir.RequiredChecks)
	}
}

func TestIntentParsesToStableASTWithSourceSpans(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	ast, diagnostics, err := ParseIntentASTFile(filepath.Join(demo, ".gatemole", "intents", "auth.password_reset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if HasErrorDiagnostics(diagnostics) {
		t.Fatalf("unexpected diagnostics: %#v", diagnostics)
	}
	if ast.Version != ASTSchemaVersion {
		t.Fatalf("unexpected AST version %s", ast.Version)
	}
	feature := findASTNode(ast, "feature")
	if feature == nil || feature.Value != "auth.password_reset" {
		t.Fatalf("unexpected feature AST node: %#v", ast.Nodes)
	}
	behavior := findASTNode(ast, "behavior")
	if behavior == nil {
		t.Fatal("missing behavior AST node")
	}
	ownedPaths := findASTNode(ast, "owned_paths")
	if ownedPaths == nil || len(ownedPaths.Values) != 2 {
		t.Fatalf("expected owned_paths AST node with two entries, got %#v", ownedPaths)
	}
	if len(behavior.Values) != 4 {
		t.Fatalf("expected 4 behavior values, got %d", len(behavior.Values))
	}
	if behavior.Span.Line == 0 || behavior.Values[0].Span.Line == 0 {
		t.Fatalf("expected source spans on section and values: %#v", behavior)
	}
}

func TestIntentParserReportsDuplicateKeysWithSourceLine(t *testing.T) {
	tmp := t.TempDir()
	intentPath := filepath.Join(tmp, "duplicate.yaml")
	if err := os.WriteFile(intentPath, []byte("feature: one\nfeature: two\nowner: platform\nrisk: low\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, diagnostics, err := ParseIntentASTFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := findDiagnostic(diagnostics, "intent.duplicate_key")
	if diagnostic == nil {
		t.Fatalf("expected duplicate key diagnostic, got %#v", diagnostics)
	}
	if diagnostic.Span.Line != 2 {
		t.Fatalf("expected duplicate key on line 2, got %#v", diagnostic)
	}
}

func TestIntentParserReportsWrongNodeTypesWithSourceLine(t *testing.T) {
	tmp := t.TempDir()
	intentPath := filepath.Join(tmp, "wrong-types.yaml")
	if err := os.WriteFile(intentPath, []byte(`feature:
  - not scalar
owner: platform
risk: medium
behavior: not a list
security:
  - invariant
required_tests:
  - test
runtime_metrics:
  - metric
rollback:
  strategy: feature_flag
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, diagnostics, err := ParseIntentASTFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	scalar := findDiagnostic(diagnostics, "intent.expected_scalar")
	if scalar == nil || scalar.Span.Line != 2 {
		t.Fatalf("expected scalar type diagnostic on line 2, got %#v in %#v", scalar, diagnostics)
	}
	list := findDiagnostic(diagnostics, "intent.expected_list")
	if list == nil || list.Span.Line != 5 {
		t.Fatalf("expected list type diagnostic on line 5, got %#v in %#v", list, diagnostics)
	}
}

func TestIntentParserReportsUnsupportedNestedRollbackKey(t *testing.T) {
	tmp := t.TempDir()
	intentPath := filepath.Join(tmp, "rollback.yaml")
	if err := os.WriteFile(intentPath, []byte(`feature: demo
owner: platform
risk: medium
behavior:
  - behavior
security:
  - invariant
required_tests:
  - test
runtime_metrics:
  - metric
rollback:
  strategy: feature_flag
  ttl: 10m
`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, diagnostics, err := ParseIntentASTFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := findDiagnostic(diagnostics, "intent.unsupported_rollback_key")
	if diagnostic == nil {
		t.Fatalf("expected unsupported rollback key diagnostic, got %#v", diagnostics)
	}
	if diagnostic.Span.Line != 14 {
		t.Fatalf("expected rollback key diagnostic on line 14, got %#v", diagnostic)
	}
}

func TestIntentParserSupportsMultilineScalarsAndComments(t *testing.T) {
	tmp := t.TempDir()
	intentPath := filepath.Join(tmp, "multiline.yaml")
	if err := os.WriteFile(intentPath, []byte(`feature: docs.demo
owner: docs
risk: low
# comment should not affect the AST
goal: >
  one line
  two line
behavior:
  - behavior
security:
  - invariant
required_tests:
  - test
runtime_metrics:
  - metric
rollback:
  strategy: revert_commit
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ast, diagnostics, err := ParseIntentASTFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	if HasErrorDiagnostics(diagnostics) {
		t.Fatalf("unexpected diagnostics: %#v", diagnostics)
	}
	goal := findASTNode(ast, "goal")
	if goal == nil {
		t.Fatal("missing goal node")
	}
	if !strings.Contains(goal.Value, "one line two line") {
		t.Fatalf("expected folded multiline goal, got %q", goal.Value)
	}
	if len(ast.Nodes) != 9 {
		t.Fatalf("comment should not add AST nodes: %#v", ast.Nodes)
	}
}

func TestIntentSemanticAnalyzerProducesTypedValuesWithSpans(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	ast, diagnostics, err := ParseIntentASTFile(filepath.Join(demo, ".gatemole", "intents", "auth.password_reset.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if HasErrorDiagnostics(diagnostics) {
		t.Fatalf("unexpected parser diagnostics: %#v", diagnostics)
	}
	typed, diagnostics := AnalyzeIntentAST(ast)
	if HasErrorDiagnostics(diagnostics) {
		t.Fatalf("unexpected semantic diagnostics: %#v", diagnostics)
	}
	if typed.Feature.Value != "auth.password_reset" || typed.Feature.Span.Line == 0 {
		t.Fatalf("expected typed feature with source span, got %#v", typed.Feature)
	}
	if got := len(typed.OwnedPaths); got != 2 || typed.OwnedPaths[0].Value != "src/auth/**" {
		t.Fatalf("expected typed owned_paths, got %#v", typed.OwnedPaths)
	}
	if got := len(typed.Behavior); got != 4 || typed.Behavior[0].Span.Line == 0 {
		t.Fatalf("expected typed behavior values with spans, got %#v", typed.Behavior)
	}
}

func TestInvalidIntentReturnsCompilerDiagnostics(t *testing.T) {
	tmp := t.TempDir()
	intentPath := filepath.Join(tmp, "bad.yaml")
	outPath := filepath.Join(tmp, "bad.json")
	if err := os.WriteFile(intentPath, []byte("feature: bad\nrisk: nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := CompileIntentFile(intentPath, outPath)
	if err == nil {
		t.Fatal("expected invalid intent to fail")
	}
	if !strings.Contains(err.Error(), "intent.invalid_risk") {
		t.Fatalf("expected compiler diagnostics, got %v", err)
	}
	var diagnosticErr DiagnosticError
	if !strings.Contains(err.Error(), ":2:") {
		t.Fatalf("expected line-numbered diagnostic, got %v", err)
	}
	if !errorAs(err, &diagnosticErr) {
		t.Fatalf("expected DiagnosticError, got %T", err)
	}
}

func TestVerificationPlanBuildsFromSpecAndManifest(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	tmp := t.TempDir()
	planOut := filepath.Join(tmp, "plan.json")
	plan, err := BuildVerificationPlanFile(
		filepath.Join(demo, ".gatemole", "specs", "auth.password_reset.json"),
		filepath.Join(demo, ".gatemole", "manifests", "pass.json"),
		planOut,
	)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Version != PlanSchemaVersion {
		t.Fatalf("unexpected plan version %s", plan.Version)
	}
	if !contains(plan.VerifierRoles, "security") || !contains(plan.VerifierRoles, "rollback") {
		t.Fatalf("expected security and rollback verifier roles: %#v", plan.VerifierRoles)
	}
	if len(plan.Obligations) != 16 {
		t.Fatalf("expected 16 obligations, got %d", len(plan.Obligations))
	}
	loaded, err := LoadJSON[VerificationPlan](planOut)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Feature != "auth.password_reset" {
		t.Fatalf("unexpected loaded plan feature %s", loaded.Feature)
	}
}

func TestArtifactsBuildDeterministically(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	out := filepath.Join(t.TempDir(), "artifacts")
	specPath := filepath.Join(demo, ".gatemole", "specs", "auth.password_reset.json")
	if err := BuildArtifacts(specPath, out); err != nil {
		t.Fatal(err)
	}
	first := mustReadFile(t, filepath.Join(out, "verifier-packets.json"))
	if err := BuildArtifacts(specPath, out); err != nil {
		t.Fatal(err)
	}
	second := mustReadFile(t, filepath.Join(out, "verifier-packets.json"))
	if !bytes.Equal(first, second) {
		t.Fatal("artifact generation should be deterministic")
	}
	var packets []VerifierPacket
	if err := json.Unmarshal(first, &packets); err != nil {
		t.Fatal(err)
	}
	if len(packets) == 0 {
		t.Fatal("expected verifier packets")
	}
	for _, packet := range packets {
		if packet.PromptVersion != VerifierPromptVersion || packet.OutputSchema != VerifierOutputVersion {
			t.Fatalf("expected verifier packet schema pins, got %#v", packet)
		}
		if !strings.Contains(packet.RequiredOutput, VerifierOutputVersion) {
			t.Fatalf("expected required output to name verifier output schema, got %q", packet.RequiredOutput)
		}
	}
	for _, name := range []string{"verification-plan.json", "verifier-packets.json", "test-obligations.json", "release-policy.json"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("missing artifact %s: %v", name, err)
		}
	}
}

func TestCLISmokeForNewCommands(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	tmp := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Main([]string{"intent", "parse", "--intent", filepath.Join(demo, ".gatemole", "intents", "auth.password_reset.yaml"), "--out", filepath.Join(tmp, "ast.json")}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("intent parse failed: %d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"plan", "build", "--spec", filepath.Join(demo, ".gatemole", "specs", "auth.password_reset.json"), "--manifest", filepath.Join(demo, ".gatemole", "manifests", "pass.json"), "--out", filepath.Join(tmp, "plan.json")}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("plan build failed: %d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"artifacts", "build", "--spec", filepath.Join(demo, ".gatemole", "specs", "auth.password_reset.json"), "--out", filepath.Join(tmp, "artifacts")}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("artifacts build failed: %d stderr=%s", code, stderr.String())
	}
}

func TestLowRiskCompleteManifestAutoMerges(t *testing.T) {
	repo, manifestPath := writeScenario(t, Spec{
		ID:       "ui.copy",
		Owner:    "product",
		Risk:     RiskLow,
		Behavior: []string{"button label changes to Save"},
		Security: []string{"no secrets introduced"},
		Tests:    SpecTests{Required: []string{"button label renders"}},
		Runtime:  SpecRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: SpecRollback{Strategy: "revert_commit"},
	}, Manifest{
		Task: Task{ID: "issue-1", Summary: "update copy"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"ui.copy"},
			BehaviorChanged: true,
		},
		Verification: Verification{
			CoversBehavior: []string{"button label changes to Save"},
			Commands:       []string{"go test ./..."},
			CoversTests:    []string{"button label renders"},
			CoversSecurity: []string{"no secrets introduced"},
			TestResults:    TestResults{Passed: 3},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "auto_merge" {
		t.Fatalf("expected auto_merge, got %s: %#v", evidence.Decision, evidence.Reasons)
	}
}

func TestHighRiskCompleteManifestWithoutCanaryEscalates(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	manifest := mustLoadManifest(t, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	manifest.Runtime.Canary.Enabled = false
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "no-canary.json")
	writeJSON(t, manifestPath, manifest)
	evidence, err := CollectEvidence(demo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "human_escalation" {
		t.Fatalf("expected human_escalation, got %s", evidence.Decision)
	}
}

func TestExternalEffectsWithoutRollbackBlock(t *testing.T) {
	repo, manifestPath := writeScenario(t, Spec{
		ID:       "email.invite",
		Owner:    "growth",
		Risk:     RiskMedium,
		Behavior: []string{"user can send invite"},
		Security: []string{"invite token is scoped"},
		Tests:    SpecTests{Required: []string{"invite sends"}},
		Runtime:  SpecRuntime{Metrics: []string{"invite.sent"}},
		Rollback: SpecRollback{Strategy: "disable_job"},
	}, Manifest{
		Task: Task{ID: "issue-2", Summary: "send invite"},
		Change: Change{
			Risk:            RiskMedium,
			SpecsTouched:    []string{"email.invite"},
			BehaviorChanged: true,
			ExternalEffects: []string{"sends_email"},
		},
		Verification: Verification{
			CoversBehavior: []string{"user can send invite"},
			Commands:       []string{"go test ./..."},
			CoversTests:    []string{"invite sends"},
			CoversSecurity: []string{"invite token is scoped"},
			TestResults:    TestResults{Passed: 3},
		},
		Runtime: ManifestRuntime{Metrics: []string{"invite.sent"}},
	})
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
}

func TestMissingBehaviorTraceEvidenceBlocks(t *testing.T) {
	repo, manifestPath := writeScenario(t, Spec{
		ID:       "billing.invoice",
		Owner:    "finance",
		Risk:     RiskMedium,
		Behavior: []string{"invoice total includes tax"},
		Security: []string{"invoice is visible only to owner"},
		Tests:    SpecTests{Required: []string{"invoice total includes tax"}},
		Runtime:  SpecRuntime{Metrics: []string{"invoice.created"}},
		Rollback: SpecRollback{Strategy: "revert_commit"},
	}, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-3", Summary: "invoice tax"},
		Change: Change{
			Risk:            RiskMedium,
			SpecsTouched:    []string{"billing.invoice"},
			BehaviorChanged: true,
		},
		Verification: Verification{
			Commands:       []string{"go test ./..."},
			CoversTests:    []string{"invoice total includes tax"},
			CoversSecurity: []string{"invoice is visible only to owner"},
			TestResults:    TestResults{Passed: 3},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"invoice.created"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !missingObligation(evidence, "billing.invoice", ObligationBehavior, "invoice total includes tax") {
		t.Fatalf("missing behavior obligation was not reported: %#v", evidence.MissingObligations["billing.invoice"])
	}
}

func TestManifestCannotDowngradeSpecRisk(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	manifest := mustLoadManifest(t, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	manifest.Change.Risk = RiskLow
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "downgrade.json")
	writeJSON(t, manifestPath, manifest)
	evidence, err := CollectEvidence(demo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if len(evidence.ManifestErrors) == 0 {
		t.Fatalf("expected manifest risk validation error")
	}
}

func TestInvalidCanaryPercentBlocks(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	manifest := mustLoadManifest(t, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	manifest.Runtime.Canary.InitialPercent = 101
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "bad-canary.json")
	writeJSON(t, manifestPath, manifest)
	evidence, err := CollectEvidence(demo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
}

func TestUnknownArtifactObligationBlocks(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	manifest := mustLoadManifest(t, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	manifest.Verification.Artifacts[0].Obligations[0] = "auth.password_reset.behavior.not_declared"
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "bad-artifact-ref.json")
	writeJSON(t, manifestPath, manifest)
	evidence, err := CollectEvidence(demo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !containsSubstring(evidence.ManifestErrors, "unknown obligation") {
		t.Fatalf("expected unknown obligation manifest error: %#v", evidence.ManifestErrors)
	}
}

func TestMissingArtifactPathBlocks(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	manifest := mustLoadManifest(t, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	setArtifactPath(t, &manifest, "test-results", "artifacts/does-not-exist.xml")
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "missing-artifact.json")
	writeJSON(t, manifestPath, manifest)
	evidence, err := CollectEvidence(demo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "test-results", "artifact_missing") {
		t.Fatalf("expected missing artifact invalid evidence: %#v", evidence.InvalidEvidence)
	}
}

func TestArtifactSHA256MismatchBlocks(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	manifest := mustLoadManifest(t, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	setArtifactSHA(t, &manifest, "test-results", strings.Repeat("0", 64))
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "bad-hash.json")
	writeJSON(t, manifestPath, manifest)
	evidence, err := CollectEvidence(demo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "test-results", "sha256_mismatch") {
		t.Fatalf("expected sha mismatch invalid evidence: %#v", evidence.InvalidEvidence)
	}
}

func TestNonZeroArtifactExitBlocks(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	manifest := mustLoadManifest(t, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	setArtifactExitCode(t, &manifest, "test-results", 1)
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "non-zero-artifact.json")
	writeJSON(t, manifestPath, manifest)
	evidence, err := CollectEvidence(demo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "test-results", "non_zero_exit") {
		t.Fatalf("expected non-zero exit invalid evidence: %#v", evidence.InvalidEvidence)
	}
}

func TestMissingArtifactExitCodeBlocks(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	manifest := mustLoadManifest(t, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	clearArtifactExitCode(t, &manifest, "test-results")
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "missing-exit-code.json")
	writeJSON(t, manifestPath, manifest)
	evidence, err := CollectEvidence(demo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "test-results", "missing_exit_code") {
		t.Fatalf("expected missing exit-code invalid evidence: %#v", evidence.InvalidEvidence)
	}
}

func TestArtifactPathEscapeBlocks(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	manifest := mustLoadManifest(t, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	setArtifactPath(t, &manifest, "test-results", "../README.md")
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "escaping-artifact.json")
	writeJSON(t, manifestPath, manifest)
	evidence, err := CollectEvidence(demo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "test-results", "artifact_path_escape") {
		t.Fatalf("expected path escape invalid evidence: %#v", evidence.InvalidEvidence)
	}
}

func TestArtifactAbsolutePathBlocks(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	manifest := mustLoadManifest(t, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	setArtifactPath(t, &manifest, "test-results", filepath.Join(t.TempDir(), "junit.xml"))
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "absolute-artifact.json")
	writeJSON(t, manifestPath, manifest)
	evidence, err := CollectEvidence(demo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "test-results", "artifact_path_escape") {
		t.Fatalf("expected absolute path invalid evidence: %#v", evidence.InvalidEvidence)
	}
}

func TestArtifactSymlinkEscapeBlocks(t *testing.T) {
	spec := Spec{
		ID:       "account.profile",
		Owner:    "accounts",
		Risk:     RiskLow,
		Behavior: []string{"user can update profile"},
		Security: []string{"profile update requires owner"},
		Tests:    SpecTests{Required: []string{"profile update saves"}},
		Runtime:  SpecRuntime{Metrics: []string{"profile.updated"}},
		Rollback: SpecRollback{Strategy: "revert_commit"},
	}
	securityID := obligationID(t, spec, ObligationSecurity, "profile update requires owner")
	repo, manifestPath := writeScenario(t, spec, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-symlink", Summary: "profile update"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"account.profile"},
			BehaviorChanged: true,
		},
		Verification: Verification{
			Commands:    []string{"go test ./..."},
			TestResults: TestResults{Passed: 2},
			Artifacts: []EvidenceArtifact{{
				ID:          "security-results",
				Kind:        EvidenceSecurityCheck,
				Producer:    "ci",
				Path:        "artifacts/security.json",
				ExitCode:    exitCode(0),
				Obligations: []string{securityID},
			}},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"profile.updated"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	outsidePath := filepath.Join(t.TempDir(), "security.json")
	if err := os.WriteFile(outsidePath, []byte(`{"status":"pass","obligations":["`+securityID+`"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(repo, "artifacts", "security.json")
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "security-results", "artifact_path_escape") {
		t.Fatalf("expected symlink path escape invalid evidence: %#v", evidence.InvalidEvidence)
	}
}

func TestFailedJUnitObligationBlocks(t *testing.T) {
	spec := Spec{
		ID:       "auth.password_reset",
		Owner:    "security",
		Risk:     RiskMedium,
		Behavior: []string{"reset token is single-use"},
		Security: []string{"reset token is never logged"},
		Tests:    SpecTests{Required: []string{"token cannot be reused"}},
		Runtime:  SpecRuntime{Metrics: []string{"password_reset.requested"}},
		Rollback: SpecRollback{Strategy: "disable_feature_flag"},
	}
	testID := obligationID(t, spec, ObligationRequiredTest, "token cannot be reused")
	repo, manifestPath := writeScenario(t, spec, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-junit", Summary: "password reset"},
		Change: Change{
			Risk:            RiskMedium,
			SpecsTouched:    []string{"auth.password_reset"},
			BehaviorChanged: true,
		},
		Verification: Verification{
			Commands:    []string{"go test ./..."},
			TestResults: TestResults{Passed: 3},
			Artifacts: []EvidenceArtifact{{
				ID:          "test-results",
				Kind:        EvidenceTestCoverage,
				Producer:    "ci",
				Path:        "artifacts/junit-failing.xml",
				ExitCode:    exitCode(0),
				Obligations: []string{testID},
			}},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"password_reset.requested"}},
		Rollback: ManifestRollback{Strategy: "disable_feature_flag"},
	})
	writeArtifact(t, repo, "artifacts/junit-failing.xml", `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="auth.password_reset" tests="1" failures="1" errors="0" skipped="0">
  <testcase classname="`+testID+`" name="token cannot be reused">
    <failure message="expected token reuse to fail">token was accepted twice</failure>
  </testcase>
</testsuite>
`)
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "test-results", "junit_import") {
		t.Fatalf("expected junit import invalid evidence: %#v", evidence.InvalidEvidence)
	}
	if !contains(evidence.MissingTests["auth.password_reset"], "token cannot be reused") {
		t.Fatalf("expected failing JUnit obligation to be missing: %#v", evidence.MissingTests["auth.password_reset"])
	}
}

func TestUnrelatedJUnitFailureBlocksArtifact(t *testing.T) {
	spec := Spec{
		ID:       "auth.password_reset",
		Owner:    "security",
		Risk:     RiskMedium,
		Behavior: []string{"reset token expires after 30 minutes"},
		Security: []string{"reset token is never logged"},
		Tests:    SpecTests{Required: []string{"token expires"}},
		Runtime:  SpecRuntime{Metrics: []string{"password_reset.requested"}},
		Rollback: SpecRollback{Strategy: "disable_feature_flag"},
	}
	testID := obligationID(t, spec, ObligationRequiredTest, "token expires")
	repo, manifestPath := writeScenario(t, spec, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-junit-unrelated", Summary: "password reset"},
		Change: Change{
			Risk:            RiskMedium,
			SpecsTouched:    []string{"auth.password_reset"},
			BehaviorChanged: true,
		},
		Verification: Verification{
			Commands:    []string{"go test ./..."},
			TestResults: TestResults{Passed: 3},
			Artifacts: []EvidenceArtifact{{
				ID:          "test-results",
				Kind:        EvidenceTestCoverage,
				Producer:    "ci",
				Path:        "artifacts/junit-unrelated-failing.xml",
				ExitCode:    exitCode(0),
				Obligations: []string{testID},
			}},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"password_reset.requested"}},
		Rollback: ManifestRollback{Strategy: "disable_feature_flag"},
	})
	writeArtifact(t, repo, "artifacts/junit-unrelated-failing.xml", `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="auth.password_reset" tests="2" failures="1" errors="0" skipped="0">
  <testcase classname="`+testID+`" name="token expires" />
  <testcase classname="unrelated.package" name="unrelated failed test">
    <failure message="unrelated failed">boom</failure>
  </testcase>
</testsuite>
`)
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "test-results", "junit_import") {
		t.Fatalf("expected unrelated JUnit failure to invalidate artifact: %#v", evidence.InvalidEvidence)
	}
	if !contains(evidence.MissingTests["auth.password_reset"], "token expires") {
		t.Fatalf("expected invalid JUnit artifact to leave test obligation missing: %#v", evidence.MissingTests["auth.password_reset"])
	}
}

func TestJUnitObligationMatchRequiresExactID(t *testing.T) {
	spec := Spec{
		ID:       "auth.password_reset",
		Owner:    "security",
		Risk:     RiskMedium,
		Behavior: []string{"reset token expires after 30 minutes"},
		Security: []string{"reset token is never logged"},
		Tests:    SpecTests{Required: []string{"token expires"}},
		Runtime:  SpecRuntime{Metrics: []string{"password_reset.requested"}},
		Rollback: SpecRollback{Strategy: "disable_feature_flag"},
	}
	testID := obligationID(t, spec, ObligationRequiredTest, "token expires")
	repo, manifestPath := writeScenario(t, spec, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-junit-exact", Summary: "password reset"},
		Change: Change{
			Risk:            RiskMedium,
			SpecsTouched:    []string{"auth.password_reset"},
			BehaviorChanged: true,
		},
		Verification: Verification{
			Commands:    []string{"go test ./..."},
			TestResults: TestResults{Passed: 3},
			Artifacts: []EvidenceArtifact{{
				ID:          "test-results",
				Kind:        EvidenceTestCoverage,
				Producer:    "ci",
				Path:        "artifacts/junit-near-match.xml",
				ExitCode:    exitCode(0),
				Obligations: []string{testID},
			}},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"password_reset.requested"}},
		Rollback: ManifestRollback{Strategy: "disable_feature_flag"},
	})
	writeArtifact(t, repo, "artifacts/junit-near-match.xml", `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="auth.password_reset" tests="1" failures="0" errors="0" skipped="0">
  <testcase classname="`+testID+`_extra" name="token expires" />
</testsuite>
`)
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "test-results", "junit_import") {
		t.Fatalf("expected near-match JUnit artifact to be invalid: %#v", evidence.InvalidEvidence)
	}
	if !contains(evidence.MissingTests["auth.password_reset"], "token expires") {
		t.Fatalf("expected exact obligation to remain missing: %#v", evidence.MissingTests["auth.password_reset"])
	}
}

func TestGenericArtifactFailStatusBlocks(t *testing.T) {
	spec := Spec{
		ID:       "account.profile",
		Owner:    "accounts",
		Risk:     RiskLow,
		Behavior: []string{"user can update profile"},
		Security: []string{"profile update requires owner"},
		Tests:    SpecTests{Required: []string{"profile update saves"}},
		Runtime:  SpecRuntime{Metrics: []string{"profile.updated"}},
		Rollback: SpecRollback{Strategy: "revert_commit"},
	}
	securityID := obligationID(t, spec, ObligationSecurity, "profile update requires owner")
	repo, manifestPath := writeScenario(t, spec, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-security-artifact", Summary: "profile update"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"account.profile"},
			BehaviorChanged: true,
		},
		Verification: Verification{
			Commands:    []string{"go test ./..."},
			TestResults: TestResults{Passed: 2},
			Artifacts: []EvidenceArtifact{{
				ID:          "security-results",
				Kind:        EvidenceSecurityCheck,
				Producer:    "ci",
				Path:        "artifacts/security.json",
				ExitCode:    exitCode(0),
				Obligations: []string{securityID},
			}},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"profile.updated"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	writeArtifact(t, repo, "artifacts/security.json", `{"status":"fail","obligations":["`+securityID+`"]}`)
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "security-results", "artifact_import") {
		t.Fatalf("expected failing generic artifact to be invalid: %#v", evidence.InvalidEvidence)
	}
}

func TestGenericArtifactStatusMustBePassingWhenPresent(t *testing.T) {
	for _, status := range []string{"fail", "failed", "error", "canceled"} {
		covered, issues := importGenericEvidence([]byte(`{"status":"`+status+`","obligations":["obligation.one"]}`), []string{"obligation.one"})
		if !contains(covered, "obligation.one") {
			t.Fatalf("expected obligation token to be detected for status %s: %#v", status, covered)
		}
		if !containsSubstring(issues, "not a passing status") {
			t.Fatalf("expected non-passing status issue for %s: %#v", status, issues)
		}
	}
	covered, issues := importGenericEvidence([]byte(`{"status":"success","obligations":["obligation.one"]}`), []string{"obligation.one"})
	if !contains(covered, "obligation.one") || len(issues) != 0 {
		t.Fatalf("expected successful status to cover cleanly, covered=%#v issues=%#v", covered, issues)
	}
	covered, issues = importGenericEvidence([]byte(`{"obligations":["obligation.one"]}`), []string{"obligation.one"})
	if !contains(covered, "obligation.one") || len(issues) != 0 {
		t.Fatalf("expected status-less plan artifact to cover by exact ID, covered=%#v issues=%#v", covered, issues)
	}
}

func TestGenericArtifactRequiresExactObligationTokens(t *testing.T) {
	covered, issues := importGenericEvidence([]byte(`{"status":"pass","obligations":["obligation.one_extra"]}`), []string{"obligation.one"})
	if len(covered) != 0 {
		t.Fatalf("near-match obligation should not cover exact obligation: %#v", covered)
	}
	if !containsSubstring(issues, "does not reference obligation obligation.one") {
		t.Fatalf("expected missing exact obligation issue: %#v", issues)
	}
	covered, issues = importGenericEvidence([]byte(`{"status":"pass","obligations":[]}`), []string{"obligation.one"})
	if len(covered) != 0 {
		t.Fatalf("missing obligation should not cover: %#v", covered)
	}
	if !containsSubstring(issues, "does not reference obligation obligation.one") {
		t.Fatalf("expected missing obligation issue: %#v", issues)
	}
}

func TestSARIFSecurityEvidenceCoversReferencedObligation(t *testing.T) {
	repo, manifestPath, ids := writeFullyCoveredUIScenario(t, nil)
	securityID := ids[ObligationSecurity]
	manifest := mustLoadManifest(t, manifestPath)
	setArtifactPath(t, &manifest, "security", "artifacts/security.sarif")
	writeJSON(t, manifestPath, manifest)
	writeSARIFArtifact(t, repo, "artifacts/security.sarif", sarifSecurityLog(securityID, nil))

	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "auto_merge" {
		t.Fatalf("expected SARIF security evidence to pass, got %s: findings=%#v invalid=%#v", evidence.Decision, evidence.Findings, evidence.InvalidEvidence)
	}
	if !artifactCovered(evidence, "security", securityID) {
		t.Fatalf("expected SARIF artifact to cover security obligation: %#v", evidence.ArtifactResults)
	}
	if hasInvalidEvidence(evidence, "security", "sarif_import") {
		t.Fatalf("expected SARIF artifact to import cleanly: %#v", evidence.InvalidEvidence)
	}
}

func TestSARIFHighSecurityFindingBlocks(t *testing.T) {
	repo, manifestPath, ids := writeFullyCoveredUIScenario(t, nil)
	securityID := ids[ObligationSecurity]
	manifest := mustLoadManifest(t, manifestPath)
	setArtifactPath(t, &manifest, "security", "artifacts/security.sarif")
	writeJSON(t, manifestPath, manifest)
	writeSARIFArtifact(t, repo, "artifacts/security.sarif", sarifSecurityLog(
		"rules.no-hardcoded-secret",
		map[string]any{
			"severity":          "warning",
			"tags":              []string{securityID},
			"security-severity": "8.2",
		},
		sarifFindingResult("rules.no-hardcoded-secret", "warning", "hardcoded secret in changed file"),
	))

	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected high SARIF finding to block, got %s", evidence.Decision)
	}
	if artifactCovered(evidence, "security", securityID) {
		t.Fatalf("expected blocked SARIF obligation to remain uncovered: %#v", evidence.ArtifactResults)
	}
	if !hasFinding(evidence, "sarif", "semgrep reported high-severity finding rules.no-hardcoded-secret") {
		t.Fatalf("expected imported SARIF finding: %#v", evidence.Findings)
	}
	if !contains(evidence.PolicyResult.RulesFired, "block_verifier_findings") {
		t.Fatalf("expected policy to block on SARIF finding: %#v", evidence.PolicyResult)
	}
}

func TestSARIFRequiresExactObligationIDs(t *testing.T) {
	data, err := json.Marshal(sarifSecurityLog("rules.near-match", map[string]any{
		"tags": []string{"obligation.one_extra"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	covered, findings, issues := importSARIFEvidence(data, []string{"obligation.one"}, ObligationIndex{
		ByID: map[string]Obligation{
			"obligation.one": {ID: "obligation.one"},
		},
	})
	if len(covered) != 0 {
		t.Fatalf("expected near-match SARIF reference not to cover, got %#v", covered)
	}
	if len(findings) != 0 {
		t.Fatalf("expected no finding for unmapped SARIF result, got %#v", findings)
	}
	if !containsSubstring(issues, "SARIF does not reference obligation obligation.one") {
		t.Fatalf("expected exact-ID issue, got %#v", issues)
	}
}

func TestJUnitErrorsAndSkipsInvalidateArtifact(t *testing.T) {
	covered, failed, issues := importJUnitEvidence([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="suite" tests="3" failures="0" errors="1" skipped="1">
  <testcase classname="obligation.one" name="obligation.one" />
  <testcase classname="unrelated" name="error">
    <error message="boom">boom</error>
  </testcase>
  <testcase classname="unrelated" name="skip">
    <skipped message="not run" />
  </testcase>
</testsuite>
`), []string{"obligation.one"})
	if !contains(covered, "obligation.one") {
		t.Fatalf("expected passing obligation testcase to be detected: %#v", covered)
	}
	if len(failed) != 2 {
		t.Fatalf("expected error and skipped testcase labels, got %#v", failed)
	}
	if !containsSubstring(issues, "errors=1 skipped=1") {
		t.Fatalf("expected suite problem count issue: %#v", issues)
	}
	if !containsSubstring(issues, "failing/error/skipped") {
		t.Fatalf("expected testcase problem issue: %#v", issues)
	}
}

func TestNoCompiledObligationsBlocks(t *testing.T) {
	repo, manifestPath := writeScenario(t, Spec{
		ID:       "ui.copy",
		Owner:    "product",
		Risk:     RiskLow,
		Behavior: []string{"button label changes to Save"},
		Security: []string{"no secrets introduced"},
		Tests:    SpecTests{Required: []string{"button label renders"}},
		Runtime:  SpecRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: SpecRollback{Strategy: "revert_commit"},
	}, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-no-specs", Summary: "copy update"},
		Change: Change{
			Risk:         RiskLow,
			ChangedFiles: []string{"src/copy.ts"},
		},
		Verification: Verification{
			Commands:    []string{"go test ./..."},
			TestResults: TestResults{Passed: 1},
		},
	})
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if evidence.Compilation.ObligationsBuilt != 0 {
		t.Fatalf("expected zero compiled obligations, got %d", evidence.Compilation.ObligationsBuilt)
	}
	if !hasFinding(evidence, "compiler", "no obligations were compiled") {
		t.Fatalf("expected compiler finding for zero obligations: %#v", evidence.Findings)
	}
}

func TestChangedFileOwnedByTouchedSpecPassesTraceability(t *testing.T) {
	repo, manifestPath := writeScenario(t, Spec{
		ID:         "ui.copy",
		Owner:      "product",
		OwnedPaths: []string{"src/ui/**"},
		Risk:       RiskLow,
		Behavior:   []string{"button label changes to Save"},
		Security:   []string{"no secrets introduced"},
		Tests:      SpecTests{Required: []string{"button label renders"}},
		Runtime:    SpecRuntime{Metrics: []string{"ui.rendered"}},
		Rollback:   SpecRollback{Strategy: "revert_commit"},
	}, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-trace-pass", Summary: "copy update"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"ui.copy"},
			BehaviorChanged: true,
			ChangedFiles:    []string{"src/ui/copy.ts"},
		},
		Verification: Verification{
			CoversBehavior: []string{"button label changes to Save"},
			Commands:       []string{"go test ./..."},
			CoversTests:    []string{"button label renders"},
			CoversSecurity: []string{"no secrets introduced"},
			TestResults:    TestResults{Passed: 1},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "auto_merge" {
		t.Fatalf("expected auto_merge, got %s: %#v", evidence.Decision, evidence.ManifestErrors)
	}
}

func TestChangedFileOwnedByUntouchedSpecBlocksTraceability(t *testing.T) {
	repo, manifestPath := writeScenario(t, Spec{
		ID:         "ui.copy",
		Owner:      "product",
		OwnedPaths: []string{"src/ui/**"},
		Risk:       RiskLow,
		Behavior:   []string{"button label changes to Save"},
		Security:   []string{"no secrets introduced"},
		Tests:      SpecTests{Required: []string{"button label renders"}},
		Runtime:    SpecRuntime{Metrics: []string{"ui.rendered"}},
		Rollback:   SpecRollback{Strategy: "revert_commit"},
	}, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-trace-wrong-spec", Summary: "copy update"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"ui.copy"},
			BehaviorChanged: true,
			ChangedFiles:    []string{"src/billing/invoice.ts"},
		},
		Verification: Verification{
			CoversBehavior: []string{"button label changes to Save"},
			Commands:       []string{"go test ./..."},
			CoversTests:    []string{"button label renders"},
			CoversSecurity: []string{"no secrets introduced"},
			TestResults:    TestResults{Passed: 1},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	writeJSON(t, filepath.Join(repo, ".gatemole", "specs", "billing.invoice.json"), Spec{
		Version:    SpecSchemaVersion,
		ID:         "billing.invoice",
		Owner:      "finance",
		OwnedPaths: []string{"src/billing/**"},
		Risk:       RiskLow,
		Behavior:   []string{"invoice total includes tax"},
		Security:   []string{"invoice is visible only to owner"},
		Tests:      SpecTests{Required: []string{"invoice total includes tax"}},
		Runtime:    SpecRuntime{Metrics: []string{"invoice.created"}},
		Rollback:   SpecRollback{Strategy: "revert_commit"},
	})
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !containsSubstring(evidence.ManifestErrors, "owned by spec billing.invoice") {
		t.Fatalf("expected traceability error for untouched owner: %#v", evidence.ManifestErrors)
	}
}

func TestChangedFileWithoutOwnerBlocksTraceability(t *testing.T) {
	repo, manifestPath := writeScenario(t, Spec{
		ID:         "ui.copy",
		Owner:      "product",
		OwnedPaths: []string{"src/ui/**"},
		Risk:       RiskLow,
		Behavior:   []string{"button label changes to Save"},
		Security:   []string{"no secrets introduced"},
		Tests:      SpecTests{Required: []string{"button label renders"}},
		Runtime:    SpecRuntime{Metrics: []string{"ui.rendered"}},
		Rollback:   SpecRollback{Strategy: "revert_commit"},
	}, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-trace-unowned", Summary: "copy update"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"ui.copy"},
			BehaviorChanged: true,
			ChangedFiles:    []string{"src/unknown/file.ts"},
		},
		Verification: Verification{
			CoversBehavior: []string{"button label changes to Save"},
			Commands:       []string{"go test ./..."},
			CoversTests:    []string{"button label renders"},
			CoversSecurity: []string{"no secrets introduced"},
			TestResults:    TestResults{Passed: 1},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if !containsSubstring(evidence.ManifestErrors, "not owned by any spec") {
		t.Fatalf("expected unowned file traceability error: %#v", evidence.ManifestErrors)
	}
}

func TestMediumRiskWithoutArtifactsBlocksEvenWithTextCoverage(t *testing.T) {
	repo, manifestPath := writeScenario(t, Spec{
		ID:       "billing.invoice",
		Owner:    "finance",
		Risk:     RiskMedium,
		Behavior: []string{"invoice total includes tax"},
		Security: []string{"invoice is visible only to owner"},
		Tests:    SpecTests{Required: []string{"invoice total includes tax"}},
		Runtime:  SpecRuntime{Metrics: []string{"invoice.created"}},
		Rollback: SpecRollback{Strategy: "revert_commit"},
	}, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-no-artifacts", Summary: "invoice tax"},
		Change: Change{
			Risk:            RiskMedium,
			SpecsTouched:    []string{"billing.invoice"},
			BehaviorChanged: true,
		},
		Verification: Verification{
			CoversBehavior: []string{"invoice total includes tax"},
			Commands:       []string{"go test ./..."},
			CoversTests:    []string{"invoice total includes tax"},
			CoversSecurity: []string{"invoice is visible only to owner"},
			TestResults:    TestResults{Passed: 3},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"invoice.created"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected block, got %s", evidence.Decision)
	}
	if len(evidence.MissingObligations["billing.invoice"]) != 0 {
		t.Fatalf("text coverage should satisfy obligations when artifact mode is absent: %#v", evidence.MissingObligations["billing.invoice"])
	}
	if !hasFinding(evidence, "evidence_linker", "artifact-backed evidence is required") {
		t.Fatalf("expected evidence linker finding for missing artifacts: %#v", evidence.Findings)
	}
}

func TestUnknownJSONFieldBlocksCompilation(t *testing.T) {
	repo, manifestPath := writeScenario(t, Spec{
		ID:       "ui.copy",
		Owner:    "product",
		Risk:     RiskLow,
		Behavior: []string{"button label changes to Save"},
		Security: []string{"no secrets introduced"},
		Tests:    SpecTests{Required: []string{"button label renders"}},
		Runtime:  SpecRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: SpecRollback{Strategy: "revert_commit"},
	}, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-unknown-json", Summary: "copy update"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"ui.copy"},
			BehaviorChanged: true,
		},
		Verification: Verification{
			CoversBehavior: []string{"button label changes to Save"},
			Commands:       []string{"go test ./..."},
			CoversTests:    []string{"button label renders"},
			CoversSecurity: []string{"no secrets introduced"},
			TestResults:    TestResults{Passed: 1},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	data := mustReadFile(t, manifestPath)
	data = bytes.Replace(data, []byte("\n}"), []byte(",\n  \"unexpected\": true\n}"), 1)
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := CollectEvidence(repo, manifestPath)
	if err == nil {
		t.Fatal("expected unknown JSON field to fail compilation")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestTrailingJSONBlocksCompilation(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "manifest.json")
	writeJSON(t, path, Manifest{Version: ManifestSchemaVersion})
	data := mustReadFile(t, path)
	if err := os.WriteFile(path, append(data, []byte("{}")...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadJSON[Manifest](path)
	if err == nil {
		t.Fatal("expected trailing JSON content to fail compilation")
	}
	if !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("expected trailing JSON error, got %v", err)
	}
}

func TestGateResultJSONIsCompactAndDeterministic(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	evidence, err := CollectEvidence(demo, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	if err != nil {
		t.Fatal(err)
	}
	output, err := RenderGateResultJSON(evidence)
	if err != nil {
		t.Fatal(err)
	}
	var result GateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Decision != "canary" {
		t.Fatalf("expected canary gate result, got %s", result.Decision)
	}
	if result.SpecErrors == nil || result.ManifestErrors == nil || result.InvalidEvidence == nil {
		t.Fatalf("expected empty result collections to render as arrays, got spec=%#v manifest=%#v invalid=%#v", result.SpecErrors, result.ManifestErrors, result.InvalidEvidence)
	}
	if result.FiredPolicyRule != "high_risk_canary" {
		t.Fatalf("unexpected fired policy rule %s", result.FiredPolicyRule)
	}
	if !contains(result.CoveredObligations["auth.password_reset"], "auth.password_reset.required_test.token_cannot_be_reused") {
		t.Fatalf("expected compact covered obligation IDs: %#v", result.CoveredObligations)
	}
}

func TestGateCommandWritesGateResultFile(t *testing.T) {
	repo, manifestPath := writeScenario(t, Spec{
		Version:    SpecSchemaVersion,
		ID:         "ui.copy",
		Owner:      "product",
		OwnedPaths: []string{"src/ui/**"},
		Risk:       RiskLow,
		Behavior:   []string{"button label changes to Save"},
		Security:   []string{"no secrets introduced"},
		Tests:      SpecTests{Required: []string{"button label renders"}},
		Runtime:    SpecRuntime{Metrics: []string{"ui.rendered"}},
		Rollback:   SpecRollback{Strategy: "revert_commit"},
	}, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-gate-out", Summary: "copy update"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"ui.copy"},
			BehaviorChanged: true,
			ChangedFiles:    []string{"src/ui/copy.ts"},
		},
		Verification: Verification{
			CoversBehavior: []string{"button label changes to Save"},
			Commands:       []string{"go test ./..."},
			CoversTests:    []string{"button label renders"},
			CoversSecurity: []string{"no secrets introduced"},
			TestResults:    TestResults{Passed: 1},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Main([]string{"--repo", repo, "--manifest", manifestPath, "gate", "--out", ".gatemole/build/gate-result.json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gate failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Release decision: auto_merge") {
		t.Fatalf("expected human gate output, got %s", stdout.String())
	}
	result, err := LoadJSON[GateResult](filepath.Join(repo, ".gatemole", "build", "gate-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != "gatemole.gate_result.v0" || result.Decision != "auto_merge" {
		t.Fatalf("unexpected gate result: %#v", result)
	}
}

func TestCustomPolicyOverridesReleaseDecision(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	tmp := t.TempDir()
	policyPath := filepath.Join(tmp, "freeze-policy.json")
	writeJSON(t, policyPath, ReleasePolicy{
		Version: PolicySchemaVersion,
		Rules: []PolicyRule{{
			ID:       "release_freeze",
			When:     PolicyCondition{Fact: "always"},
			Decision: "human_escalation",
			Reasons:  []string{"release freeze requires human approval"},
			Stop:     true,
		}},
	})
	evidence, err := CollectEvidenceWithOptions(demo, filepath.Join(demo, ".gatemole", "manifests", "pass.json"), CollectEvidenceOptions{PolicyPath: policyPath})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "human_escalation" {
		t.Fatalf("expected custom policy decision, got %s", evidence.Decision)
	}
	if evidence.PolicyResult.FiredPolicyRule != "release_freeze" {
		t.Fatalf("expected custom policy rule, got %#v", evidence.PolicyResult)
	}
	if !contains(evidence.Reasons, "release freeze requires human approval") {
		t.Fatalf("expected custom policy reason, got %#v", evidence.Reasons)
	}
}

func TestCustomPolicyCannotBypassSignedEvidenceFailures(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	tmp := t.TempDir()
	policyPath := filepath.Join(tmp, "unsafe-policy.json")
	writeJSON(t, policyPath, ReleasePolicy{
		Version: PolicySchemaVersion,
		Rules: []PolicyRule{{
			ID:       "unsafe_auto_merge",
			When:     PolicyCondition{Fact: "always"},
			Decision: "auto_merge",
			Reasons:  []string{"unsafe policy tried to auto merge"},
			Stop:     true,
		}},
	})
	evidence, err := CollectEvidenceWithOptions(demo, filepath.Join(demo, ".gatemole", "manifests", "pass.json"), CollectEvidenceOptions{
		RequireSigned: true,
		PolicyPath:    policyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected policy floor to block signed evidence failures, got %s", evidence.Decision)
	}
	if !contains(evidence.PolicyResult.RulesFired, "policy_floor_block") {
		t.Fatalf("expected policy floor rule to fire: %#v", evidence.PolicyResult)
	}
	if !containsSubstring(evidence.Reasons, "evidence artifact behavior-trace is invalid") {
		t.Fatalf("expected signed evidence failure reason, got %#v", evidence.Reasons)
	}
}

func TestPolicyInputIncludesArtifactVerificationState(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	evidence, err := CollectEvidence(demo, filepath.Join(demo, ".gatemole", "manifests", "pass.json"))
	if err != nil {
		t.Fatal(err)
	}
	input := PolicyInputFromEvidence(evidence)
	if len(input.ArtifactResults) == 0 {
		t.Fatal("expected policy input to include artifact results")
	}
	if input.HasInvalidEvidence {
		t.Fatalf("expected passing demo to have no invalid evidence: %#v", input.InvalidEvidence)
	}
}

func TestVerifierOutputFindingBlocksReleasePolicy(t *testing.T) {
	var behaviorID string
	repo, manifestPath, _ := writeFullyCoveredUIScenario(t, func(ids map[ObligationKind]string) []EvidenceArtifact {
		behaviorID = ids[ObligationBehavior]
		return []EvidenceArtifact{
			unsignedArtifact("ai-review", EvidenceVerifierOutput, "artifacts/verifier-block.json", behaviorID),
		}
	})
	writeVerifierOutputArtifact(t, repo, "artifacts/verifier-block.json", VerifierOutput{
		Version:       VerifierOutputVersion,
		Verifier:      "spec_adherence",
		PromptVersion: VerifierPromptVersion,
		Model:         "gpt-5.2",
		Obligations:   []string{behaviorID},
		Confidence:    0.82,
		Findings: []Finding{{
			Verifier:    "spec_adherence",
			Severity:    "high",
			Decision:    "block",
			Claim:       "behavior artifact does not prove the saved label state",
			Evidence:    "artifact references the obligation ID but does not include a state trace",
			RequiredFix: "attach a behavior trace that exercises the saved label state",
			Obligations: []string{behaviorID},
		}},
	})
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected verifier output to block, got %s", evidence.Decision)
	}
	if !hasFinding(evidence, "spec_adherence", "behavior artifact does not prove") {
		t.Fatalf("expected imported verifier finding: %#v", evidence.Findings)
	}
	if len(evidence.VerifierOutputs) != 1 {
		t.Fatalf("expected verifier output to be imported: %#v", evidence.VerifierOutputs)
	}
	if !contains(evidence.PolicyResult.RulesFired, "block_verifier_findings") {
		t.Fatalf("expected policy to block on verifier finding: %#v", evidence.PolicyResult)
	}
	input := PolicyInputFromEvidence(evidence)
	if len(input.VerifierOutputs) != 1 || !input.HasBlockingFindings {
		t.Fatalf("expected policy input to include verifier output and blocking finding: %#v", input)
	}
}

func TestMalformedVerifierOutputInvalidatesEvidence(t *testing.T) {
	var behaviorID string
	repo, manifestPath, _ := writeFullyCoveredUIScenario(t, func(ids map[ObligationKind]string) []EvidenceArtifact {
		behaviorID = ids[ObligationBehavior]
		return []EvidenceArtifact{
			unsignedArtifact("ai-review", EvidenceVerifierOutput, "artifacts/verifier-bad.json", behaviorID),
		}
	})
	writeArtifact(t, repo, "artifacts/verifier-bad.json", `{
  "version": "gatemole.verifier_output.old",
  "verifier": "spec_adherence",
  "prompt_version": "gatemole.verifier_prompt.v0",
  "obligations": ["`+behaviorID+`"],
  "findings": []
}`)
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected malformed verifier output to block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "ai-review", "verifier_output_import") {
		t.Fatalf("expected verifier output import failure: %#v", evidence.InvalidEvidence)
	}
	if !hasFinding(evidence, "evidence_linker", "evidence artifact ai-review is invalid") {
		t.Fatalf("expected invalid verifier output finding: %#v", evidence.Findings)
	}
}

func TestVerifierOutputDoesNotSatisfyRequiredEvidenceCoverage(t *testing.T) {
	spec := sampleUISpec()
	behaviorID := obligationID(t, spec, ObligationBehavior, "button label changes to Save")
	securityID := obligationID(t, spec, ObligationSecurity, "no secrets introduced")
	testID := obligationID(t, spec, ObligationRequiredTest, "button label renders")
	runtimeID := obligationID(t, spec, ObligationRuntimeSignal, "ui.rendered")
	rollbackID := obligationID(t, spec, ObligationRollback, "revert_commit")
	allIDs := []string{behaviorID, securityID, testID, runtimeID, rollbackID}
	repo, manifestPath := writeScenario(t, spec, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-verifier-only", Summary: "verifier only"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"ui.copy"},
			BehaviorChanged: true,
			ChangedFiles:    []string{"src/ui/copy.ts"},
		},
		Verification: Verification{
			Commands:    []string{"ai verifier"},
			TestResults: TestResults{Passed: 1},
			Artifacts: []EvidenceArtifact{
				unsignedArtifact("ai-review", EvidenceVerifierOutput, "artifacts/verifier-pass.json", allIDs...),
			},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	writeVerifierOutputArtifact(t, repo, "artifacts/verifier-pass.json", VerifierOutput{
		Version:       VerifierOutputVersion,
		Verifier:      "spec_adherence",
		PromptVersion: VerifierPromptVersion,
		Model:         "gpt-5.2",
		Obligations:   allIDs,
		Confidence:    0.74,
		Findings: []Finding{{
			Verifier:    "spec_adherence",
			Severity:    "low",
			Decision:    "pass",
			Claim:       "verifier found no contradiction in the submitted evidence",
			Evidence:    "no non-verifier evidence was attached",
			Obligations: allIDs,
		}},
	})
	evidence, err := CollectEvidence(repo, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected verifier-only evidence to block, got %s", evidence.Decision)
	}
	if !missingObligation(evidence, "ui.copy", ObligationBehavior, "button label changes to Save") {
		t.Fatalf("verifier output should not satisfy behavior coverage: %#v", evidence.CoveredObligations["ui.copy"])
	}
	if len(evidence.CoveredObligations["ui.copy"]) != 0 {
		t.Fatalf("verifier output should not count as required evidence coverage: %#v", evidence.CoveredObligations["ui.copy"])
	}
}

func TestMissingDefaultPolicyFailsClosed(t *testing.T) {
	repo := t.TempDir()
	specDir := filepath.Join(repo, ".gatemole", "specs")
	manifestDir := filepath.Join(repo, ".gatemole", "manifests")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(specDir, "ui.copy.json"), Spec{
		Version:    SpecSchemaVersion,
		ID:         "ui.copy",
		Owner:      "product",
		OwnedPaths: []string{"src/ui/**"},
		Risk:       RiskLow,
		Behavior:   []string{"button label changes to Save"},
		Security:   []string{"no secrets introduced"},
		Tests:      SpecTests{Required: []string{"button label renders"}},
		Runtime:    SpecRuntime{Metrics: []string{"ui.rendered"}},
		Rollback:   SpecRollback{Strategy: "revert_commit"},
	})
	manifestPath := filepath.Join(manifestDir, "change.json")
	writeJSON(t, manifestPath, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-missing-policy", Summary: "copy update"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"ui.copy"},
			BehaviorChanged: true,
			ChangedFiles:    []string{"src/ui/copy.ts"},
		},
		Verification: Verification{
			CoversBehavior: []string{"button label changes to Save"},
			Commands:       []string{"go test ./..."},
			CoversTests:    []string{"button label renders"},
			CoversSecurity: []string{"no secrets introduced"},
			TestResults:    TestResults{Passed: 1},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	_, err := CollectEvidence(repo, manifestPath)
	if err == nil || !strings.Contains(err.Error(), "release policy not found") {
		t.Fatalf("expected missing policy error, got %v", err)
	}
}

func TestPolicySimulateCommandRendersResult(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Main([]string{
		"--repo", demo,
		"--manifest", filepath.Join(demo, ".gatemole", "manifests", "pass.json"),
		"policy", "simulate",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("policy simulate failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Decision: canary") || !strings.Contains(stdout.String(), "Rules fired: high_risk_canary") {
		t.Fatalf("unexpected policy simulate output: %s", stdout.String())
	}
}

func TestPolicySimulateRejectsUnexpectedArgs(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Main([]string{
		"--repo", demo,
		"--manifest", filepath.Join(demo, ".gatemole", "manifests", "pass.json"),
		"policy", "simulate", "unexpected",
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("expected usage error, got code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "unexpected argument") {
		t.Fatalf("expected unexpected arg error, got %s", stderr.String())
	}
}

func TestPolicySimulateJSONIncludesInputAndResult(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := Main([]string{
		"--json",
		"--repo", demo,
		"--manifest", filepath.Join(demo, ".gatemole", "manifests", "pass.json"),
		"policy", "simulate",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("policy simulate json failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var simulation PolicySimulation
	if err := json.Unmarshal(stdout.Bytes(), &simulation); err != nil {
		t.Fatal(err)
	}
	if simulation.Result.Decision != "canary" || simulation.Input.Risk != RiskHigh {
		t.Fatalf("unexpected simulation: %#v", simulation)
	}
}

func TestRequireSignedBlocksUnsignedArtifacts(t *testing.T) {
	root := repoRoot(t)
	demo := filepath.Join(root, "demo_repo")
	evidence, err := CollectEvidenceWithOptions(demo, filepath.Join(demo, ".gatemole", "manifests", "pass.json"), CollectEvidenceOptions{RequireSigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected signed-evidence gate to block unsigned artifacts, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "behavior-trace", "missing_evidence_bundle") {
		t.Fatalf("expected unsigned behavior artifact to be invalid: %#v", evidence.InvalidEvidence)
	}
}

func TestRequireSignedRequiresArtifactBackedEvidence(t *testing.T) {
	repo, manifestPath := writeScenario(t, Spec{
		Version:    SpecSchemaVersion,
		ID:         "ui.copy",
		Owner:      "product",
		OwnedPaths: []string{"src/ui/**"},
		Risk:       RiskLow,
		Behavior:   []string{"button label changes to Save"},
		Security:   []string{"no secrets introduced"},
		Tests:      SpecTests{Required: []string{"button label renders"}},
		Runtime:    SpecRuntime{Metrics: []string{"ui.rendered"}},
		Rollback:   SpecRollback{Strategy: "revert_commit"},
	}, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-signed-text", Summary: "text evidence"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"ui.copy"},
			BehaviorChanged: true,
			ChangedFiles:    []string{"src/ui/copy.ts"},
		},
		Verification: Verification{
			CoversBehavior: []string{"button label changes to Save"},
			Commands:       []string{"go test ./..."},
			CoversTests:    []string{"button label renders"},
			CoversSecurity: []string{"no secrets introduced"},
			TestResults:    TestResults{Passed: 1},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	evidence, err := CollectEvidenceWithOptions(repo, manifestPath, CollectEvidenceOptions{RequireSigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected require-signed to block text-only evidence, got %s", evidence.Decision)
	}
	if !hasFinding(evidence, "evidence_linker", "signed evidence artifacts are required") {
		t.Fatalf("expected signed artifact finding: %#v", evidence.Findings)
	}
}

func TestRequireSignedAcceptsCosignVerifiedArtifacts(t *testing.T) {
	installFakeCosign(t, 0)
	signerIdentity := "https://github.com/example/repo/.github/workflows/vouch.yml@refs/heads/main"
	signerOIDCIssuer := "https://token.actions.githubusercontent.com"
	spec := Spec{
		Version:    SpecSchemaVersion,
		ID:         "ui.copy",
		Owner:      "product",
		OwnedPaths: []string{"src/ui/**"},
		Risk:       RiskLow,
		Behavior:   []string{"button label changes to Save"},
		Security:   []string{"no secrets introduced"},
		Tests:      SpecTests{Required: []string{"button label renders"}},
		Runtime:    SpecRuntime{Metrics: []string{"ui.rendered"}},
		Rollback:   SpecRollback{Strategy: "revert_commit"},
	}
	behaviorID := obligationID(t, spec, ObligationBehavior, "button label changes to Save")
	securityID := obligationID(t, spec, ObligationSecurity, "no secrets introduced")
	testID := obligationID(t, spec, ObligationRequiredTest, "button label renders")
	runtimeID := obligationID(t, spec, ObligationRuntimeSignal, "ui.rendered")
	rollbackID := obligationID(t, spec, ObligationRollback, "revert_commit")
	signed := func(id string, kind EvidenceKind, path string, obligations ...string) EvidenceArtifact {
		return EvidenceArtifact{
			ID:               id,
			Kind:             kind,
			Producer:         "ci",
			Path:             path,
			EvidenceBundle:   "artifacts/" + id + ".gatemole-bundle.json",
			SignatureBundle:  "artifacts/" + id + ".sigstore.json",
			SignerIdentity:   signerIdentity,
			SignerOIDCIssuer: signerOIDCIssuer,
			ExitCode:         exitCode(0),
			Obligations:      obligations,
		}
	}
	repo, manifestPath := writeScenario(t, spec, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-signed", Summary: "signed evidence"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"ui.copy"},
			BehaviorChanged: true,
			ChangedFiles:    []string{"src/ui/copy.ts"},
		},
		Agent: Agent{
			Name:             "codex",
			RunID:            "run-signed",
			Model:            "gpt-5.2",
			RunnerIdentity:   signerIdentity,
			RunnerOIDCIssuer: signerOIDCIssuer,
		},
		Verification: Verification{
			Commands:    []string{"go test ./..."},
			TestResults: TestResults{Passed: 1},
			Artifacts: []EvidenceArtifact{
				signed("behavior", EvidenceBehaviorTrace, "artifacts/behavior.json", behaviorID),
				signed("security", EvidenceSecurityCheck, "artifacts/security.json", securityID),
				signed("tests", EvidenceTestCoverage, "artifacts/junit.xml", testID),
				signed("runtime", EvidenceRuntimeMetric, "artifacts/runtime.json", runtimeID),
				signed("rollback", EvidenceRollbackPlan, "artifacts/rollback.json", rollbackID),
			},
		},
		Runtime:  ManifestRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	writeAllowedSigners(t, repo, signerIdentity, signerOIDCIssuer)
	writeArtifact(t, repo, "artifacts/behavior.json", `{"status":"pass","obligations":["`+behaviorID+`"]}`)
	writeArtifact(t, repo, "artifacts/security.json", `{"status":"pass","obligations":["`+securityID+`"]}`)
	writeArtifact(t, repo, "artifacts/runtime.json", `{"status":"pass","obligations":["`+runtimeID+`"]}`)
	writeArtifact(t, repo, "artifacts/rollback.json", `{"status":"pass","obligations":["`+rollbackID+`"]}`)
	writeArtifact(t, repo, "artifacts/junit.xml", `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="ui.copy" tests="1" failures="0" errors="0" skipped="0">
  <testcase classname="`+testID+`" name="button label renders" />
</testsuite>
`)
	for _, artifact := range []string{"behavior", "security", "tests", "runtime", "rollback"} {
		writeArtifact(t, repo, "artifacts/"+artifact+".sigstore.json", `{"mediaType":"application/vnd.dev.sigstore.bundle+json"}`)
	}
	manifest := mustLoadManifest(t, manifestPath)
	for _, artifact := range manifest.Verification.Artifacts {
		writeEvidenceBundle(t, repo, artifact.EvidenceBundle, manifest, artifact, nil)
	}
	evidence, err := CollectEvidenceWithOptions(repo, manifestPath, CollectEvidenceOptions{RequireSigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "auto_merge" {
		t.Fatalf("expected signed artifacts to pass, got %s: invalid=%#v findings=%#v", evidence.Decision, evidence.InvalidEvidence, evidence.Findings)
	}
	for _, result := range evidence.ArtifactResults {
		if !result.SignatureVerified {
			t.Fatalf("expected signature verification for artifact %s: %#v", result.ID, evidence.ArtifactResults)
		}
		if !result.BundleVerified || !result.HashVerified {
			t.Fatalf("expected bundle/hash verification for artifact %s: %#v", result.ID, evidence.ArtifactResults)
		}
	}
}

func TestRequireSignedRejectsEvidenceBundleHashMismatch(t *testing.T) {
	installFakeCosign(t, 0)
	repo, manifestPath, _ := writeFullyCoveredUIScenario(t, nil)
	attachSignedBundles(t, repo, manifestPath, func(artifact EvidenceArtifact, bundle *EvidenceBundle) {
		if artifact.ID == "behavior" {
			bundle.Artifact.SHA256 = strings.Repeat("0", 64)
		}
	})
	evidence, err := CollectEvidenceWithOptions(repo, manifestPath, CollectEvidenceOptions{RequireSigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected tampered bundle hash to block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "behavior", "evidence_bundle") {
		t.Fatalf("expected behavior bundle to be invalid: %#v", evidence.InvalidEvidence)
	}
}

func TestRequireSignedRejectsRunnerIdentityMismatch(t *testing.T) {
	installFakeCosign(t, 0)
	repo, manifestPath, _ := writeFullyCoveredUIScenario(t, nil)
	attachSignedBundles(t, repo, manifestPath, func(artifact EvidenceArtifact, bundle *EvidenceBundle) {
		if artifact.ID == "behavior" {
			bundle.Runner.Identity = "https://github.com/example/repo/.github/workflows/other.yml@refs/heads/main"
		}
	})
	evidence, err := CollectEvidenceWithOptions(repo, manifestPath, CollectEvidenceOptions{RequireSigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected runner identity mismatch to block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "behavior", "evidence_bundle") {
		t.Fatalf("expected behavior bundle to be invalid: %#v", evidence.InvalidEvidence)
	}
}

func TestRequireSignedRejectsSignerOutsideRepoAllowlist(t *testing.T) {
	installFakeCosign(t, 0)
	repo, manifestPath, _ := writeFullyCoveredUIScenario(t, nil)
	attachSignedBundles(t, repo, manifestPath, nil)
	writeAllowedSigners(t, repo, "https://github.com/example/repo/.github/workflows/other.yml@refs/heads/main", "https://token.actions.githubusercontent.com")
	evidence, err := CollectEvidenceWithOptions(repo, manifestPath, CollectEvidenceOptions{RequireSigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected signer outside allowlist to block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "behavior", "signer_not_allowed") {
		t.Fatalf("expected behavior signer to be rejected: %#v", evidence.InvalidEvidence)
	}
}

func TestRequireSignedRejectsEmptySignerAllowlist(t *testing.T) {
	installFakeCosign(t, 0)
	repo, manifestPath, _ := writeFullyCoveredUIScenario(t, nil)
	attachSignedBundles(t, repo, manifestPath, nil)
	writeAllowedSignerList(t, repo, nil)
	evidence, err := CollectEvidenceWithOptions(repo, manifestPath, CollectEvidenceOptions{RequireSigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected empty signer allowlist to block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "behavior", "missing_allowed_signers") {
		t.Fatalf("expected missing allowed signers failure: %#v", evidence.InvalidEvidence)
	}
}

func TestRequireSignedRejectsAllowedIdentityWithWrongIssuer(t *testing.T) {
	installFakeCosign(t, 0)
	repo, manifestPath, _ := writeFullyCoveredUIScenario(t, nil)
	attachSignedBundles(t, repo, manifestPath, nil)
	writeAllowedSigners(t, repo, "https://github.com/example/repo/.github/workflows/vouch.yml@refs/heads/main", "https://issuer.example.invalid")
	evidence, err := CollectEvidenceWithOptions(repo, manifestPath, CollectEvidenceOptions{RequireSigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "block" {
		t.Fatalf("expected wrong signer issuer to block, got %s", evidence.Decision)
	}
	if !hasInvalidEvidence(evidence, "behavior", "signer_not_allowed") {
		t.Fatalf("expected signer issuer to be rejected: %#v", evidence.InvalidEvidence)
	}
}

func TestRequireSignedAcceptsMultiEntryAllowlist(t *testing.T) {
	installFakeCosign(t, 0)
	repo, manifestPath, _ := writeFullyCoveredUIScenario(t, nil)
	attachSignedBundles(t, repo, manifestPath, nil)
	writeAllowedSignerList(t, repo, []AllowedSigner{
		{
			Identity:   "https://github.com/example/repo/.github/workflows/other.yml@refs/heads/main",
			OIDCIssuer: "https://token.actions.githubusercontent.com",
		},
		{
			Identity:   "https://github.com/example/repo/.github/workflows/vouch.yml@refs/heads/main",
			OIDCIssuer: "https://token.actions.githubusercontent.com",
		},
	})
	evidence, err := CollectEvidenceWithOptions(repo, manifestPath, CollectEvidenceOptions{RequireSigned: true})
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Decision != "auto_merge" {
		t.Fatalf("expected matching signer in multi-entry allowlist to pass, got %s: invalid=%#v", evidence.Decision, evidence.InvalidEvidence)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasObligation(ir IR, kind ObligationKind, evidence EvidenceKind, text string) bool {
	for _, obligation := range ir.Obligations {
		if obligation.Kind == kind && obligation.RequiredEvidence == evidence && obligation.Text == text {
			return true
		}
	}
	return false
}

func hasObligationID(ir IR, id string) bool {
	for _, obligation := range ir.Obligations {
		if obligation.ID == id {
			return true
		}
	}
	return false
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

func findDiagnostic(diagnostics []Diagnostic, code string) *Diagnostic {
	for i := range diagnostics {
		if diagnostics[i].Code == code {
			return &diagnostics[i]
		}
	}
	return nil
}

func missingObligation(evidence Evidence, specID string, kind ObligationKind, text string) bool {
	for _, obligation := range evidence.MissingObligations[specID] {
		if obligation.Kind == kind && obligation.Text == text {
			return true
		}
	}
	return false
}

func artifactCovered(evidence Evidence, artifactID string, obligationID string) bool {
	for _, result := range evidence.ArtifactResults {
		if result.ID != artifactID {
			continue
		}
		return contains(result.CoveredObligations, obligationID)
	}
	return false
}

func hasInvalidEvidence(evidence Evidence, artifactID string, code string) bool {
	for _, invalid := range evidence.InvalidEvidence {
		if invalid.Artifact == artifactID && invalid.Code == code {
			return true
		}
	}
	return false
}

func hasFinding(evidence Evidence, verifier string, claim string) bool {
	for _, finding := range evidence.Findings {
		if finding.Verifier == verifier && strings.Contains(finding.Claim, claim) {
			return true
		}
	}
	return false
}

func setArtifactPath(t *testing.T, manifest *Manifest, artifactID string, path string) {
	t.Helper()
	for i := range manifest.Verification.Artifacts {
		if manifest.Verification.Artifacts[i].ID == artifactID {
			manifest.Verification.Artifacts[i].Path = path
			return
		}
	}
	t.Fatalf("artifact %s not found", artifactID)
}

func setArtifactSHA(t *testing.T, manifest *Manifest, artifactID string, sha string) {
	t.Helper()
	for i := range manifest.Verification.Artifacts {
		if manifest.Verification.Artifacts[i].ID == artifactID {
			manifest.Verification.Artifacts[i].SHA256 = sha
			return
		}
	}
	t.Fatalf("artifact %s not found", artifactID)
}

func setArtifactExitCode(t *testing.T, manifest *Manifest, artifactID string, exitCode int) {
	t.Helper()
	for i := range manifest.Verification.Artifacts {
		if manifest.Verification.Artifacts[i].ID == artifactID {
			manifest.Verification.Artifacts[i].ExitCode = &exitCode
			return
		}
	}
	t.Fatalf("artifact %s not found", artifactID)
}

func clearArtifactExitCode(t *testing.T, manifest *Manifest, artifactID string) {
	t.Helper()
	for i := range manifest.Verification.Artifacts {
		if manifest.Verification.Artifacts[i].ID == artifactID {
			manifest.Verification.Artifacts[i].ExitCode = nil
			return
		}
	}
	t.Fatalf("artifact %s not found", artifactID)
}

func exitCode(code int) *int {
	return &code
}

func sampleUISpec() Spec {
	return Spec{
		Version:    SpecSchemaVersion,
		ID:         "ui.copy",
		Owner:      "product",
		OwnedPaths: []string{"src/ui/**"},
		Risk:       RiskLow,
		Behavior:   []string{"button label changes to Save"},
		Security:   []string{"no secrets introduced"},
		Tests:      SpecTests{Required: []string{"button label renders"}},
		Runtime:    SpecRuntime{Metrics: []string{"ui.rendered"}},
		Rollback:   SpecRollback{Strategy: "revert_commit"},
	}
}

func unsignedArtifact(id string, kind EvidenceKind, path string, obligations ...string) EvidenceArtifact {
	return EvidenceArtifact{
		ID:          id,
		Kind:        kind,
		Producer:    "ci",
		Path:        path,
		ExitCode:    exitCode(0),
		Obligations: obligations,
	}
}

func writeFullyCoveredUIScenario(t *testing.T, extra func(map[ObligationKind]string) []EvidenceArtifact) (string, string, map[ObligationKind]string) {
	t.Helper()
	spec := sampleUISpec()
	ids := map[ObligationKind]string{
		ObligationBehavior:      obligationID(t, spec, ObligationBehavior, "button label changes to Save"),
		ObligationSecurity:      obligationID(t, spec, ObligationSecurity, "no secrets introduced"),
		ObligationRequiredTest:  obligationID(t, spec, ObligationRequiredTest, "button label renders"),
		ObligationRuntimeSignal: obligationID(t, spec, ObligationRuntimeSignal, "ui.rendered"),
		ObligationRollback:      obligationID(t, spec, ObligationRollback, "revert_commit"),
	}
	artifacts := []EvidenceArtifact{
		unsignedArtifact("behavior", EvidenceBehaviorTrace, "artifacts/behavior.json", ids[ObligationBehavior]),
		unsignedArtifact("security", EvidenceSecurityCheck, "artifacts/security.json", ids[ObligationSecurity]),
		unsignedArtifact("tests", EvidenceTestCoverage, "artifacts/junit.xml", ids[ObligationRequiredTest]),
		unsignedArtifact("runtime", EvidenceRuntimeMetric, "artifacts/runtime.json", ids[ObligationRuntimeSignal]),
		unsignedArtifact("rollback", EvidenceRollbackPlan, "artifacts/rollback.json", ids[ObligationRollback]),
	}
	if extra != nil {
		artifacts = append(artifacts, extra(ids)...)
	}
	repo, manifestPath := writeScenario(t, spec, Manifest{
		Version: ManifestSchemaVersion,
		Task:    Task{ID: "issue-covered-ui", Summary: "copy update"},
		Change: Change{
			Risk:            RiskLow,
			SpecsTouched:    []string{"ui.copy"},
			BehaviorChanged: true,
			ChangedFiles:    []string{"src/ui/copy.ts"},
		},
		Verification: Verification{
			Commands:    []string{"go test ./..."},
			TestResults: TestResults{Passed: 1},
			Artifacts:   artifacts,
		},
		Runtime:  ManifestRuntime{Metrics: []string{"ui.rendered"}},
		Rollback: ManifestRollback{Strategy: "revert_commit"},
	})
	writeArtifact(t, repo, "artifacts/behavior.json", `{"status":"pass","obligations":["`+ids[ObligationBehavior]+`"]}`)
	writeArtifact(t, repo, "artifacts/security.json", `{"status":"pass","obligations":["`+ids[ObligationSecurity]+`"]}`)
	writeArtifact(t, repo, "artifacts/runtime.json", `{"status":"pass","obligations":["`+ids[ObligationRuntimeSignal]+`"]}`)
	writeArtifact(t, repo, "artifacts/rollback.json", `{"status":"pass","obligations":["`+ids[ObligationRollback]+`"]}`)
	writeArtifact(t, repo, "artifacts/junit.xml", `<?xml version="1.0" encoding="UTF-8"?>
<testsuite name="ui.copy" tests="1" failures="0" errors="0" skipped="0">
  <testcase classname="`+ids[ObligationRequiredTest]+`" name="button label renders" />
</testsuite>
`)
	return repo, manifestPath, ids
}

func attachSignedBundles(t *testing.T, repo string, manifestPath string, mutate func(EvidenceArtifact, *EvidenceBundle)) {
	t.Helper()
	signerIdentity := "https://github.com/example/repo/.github/workflows/vouch.yml@refs/heads/main"
	signerOIDCIssuer := "https://token.actions.githubusercontent.com"
	writeAllowedSigners(t, repo, signerIdentity, signerOIDCIssuer)
	manifest := mustLoadManifest(t, manifestPath)
	manifest.Agent = Agent{
		Name:             "codex",
		RunID:            "run-signed",
		Model:            "gpt-5.2",
		RunnerIdentity:   signerIdentity,
		RunnerOIDCIssuer: signerOIDCIssuer,
	}
	for i := range manifest.Verification.Artifacts {
		id := manifest.Verification.Artifacts[i].ID
		manifest.Verification.Artifacts[i].EvidenceBundle = "artifacts/" + id + ".gatemole-bundle.json"
		manifest.Verification.Artifacts[i].SignatureBundle = "artifacts/" + id + ".sigstore.json"
		manifest.Verification.Artifacts[i].SignerIdentity = signerIdentity
		manifest.Verification.Artifacts[i].SignerOIDCIssuer = signerOIDCIssuer
	}
	writeJSON(t, manifestPath, manifest)
	for _, artifact := range manifest.Verification.Artifacts {
		writeArtifact(t, repo, artifact.SignatureBundle, `{"mediaType":"application/vnd.dev.sigstore.bundle+json"}`)
		writeEvidenceBundle(t, repo, artifact.EvidenceBundle, manifest, artifact, mutate)
	}
}

func writeAllowedSigners(t *testing.T, repo string, identity string, issuer string) {
	t.Helper()
	writeAllowedSignerList(t, repo, []AllowedSigner{{
		Identity:   identity,
		OIDCIssuer: issuer,
	}})
}

func writeAllowedSignerList(t *testing.T, repo string, signers []AllowedSigner) {
	t.Helper()
	config := LoadConfigOrDefault(repo)
	config.AllowedSigners = append([]AllowedSigner(nil), signers...)
	configPath := filepath.Join(repo, ".gatemole", "config.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, configPath, config)
}

func writeScenario(t *testing.T, spec Spec, manifest Manifest) (string, string) {
	t.Helper()
	repo := t.TempDir()
	specDir := filepath.Join(repo, ".gatemole", "specs")
	policyDir := filepath.Join(repo, ".gatemole", "policy")
	manifestDir := filepath.Join(repo, ".gatemole", "manifests")
	if err := os.MkdirAll(specDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(policyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteDefaultReleasePolicy(filepath.Join(policyDir, "release-policy.json")); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(specDir, spec.ID+".json"), spec)
	manifestPath := filepath.Join(manifestDir, "change.json")
	writeJSON(t, manifestPath, manifest)
	return repo, manifestPath
}

func writeArtifact(t *testing.T, repo string, relPath string, data string) {
	t.Helper()
	path := filepath.Join(repo, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeVerifierOutputArtifact(t *testing.T, repo string, relPath string, output VerifierOutput) {
	t.Helper()
	data, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeArtifact(t, repo, relPath, string(append(data, '\n')))
}

func writeSARIFArtifact(t *testing.T, repo string, relPath string, log map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeArtifact(t, repo, relPath, string(append(data, '\n')))
}

func sarifSecurityLog(ruleID string, properties map[string]any, results ...map[string]any) map[string]any {
	rule := map[string]any{"id": ruleID}
	if len(properties) > 0 {
		rule["properties"] = properties
	}
	return map[string]any{
		"version": sarifVersion,
		"runs": []any{map[string]any{
			"tool": map[string]any{
				"driver": map[string]any{
					"name":  "semgrep",
					"rules": []any{rule},
				},
			},
			"results": results,
		}},
	}
}

func sarifFindingResult(ruleID string, level string, message string) map[string]any {
	return map[string]any{
		"ruleId": ruleID,
		"level":  level,
		"message": map[string]any{
			"text": message,
		},
	}
}

func writeEvidenceBundle(t *testing.T, repo string, relPath string, manifest Manifest, artifact EvidenceArtifact, mutate func(EvidenceArtifact, *EvidenceBundle)) {
	t.Helper()
	artifactData := mustReadFile(t, filepath.Join(repo, artifact.Path))
	exitCode := 0
	if artifact.ExitCode != nil {
		exitCode = *artifact.ExitCode
	}
	bundle := EvidenceBundle{
		Version:      EvidenceBundleVersion,
		ManifestID:   manifest.Task.ID,
		SpecsTouched: append([]string(nil), manifest.Change.SpecsTouched...),
		ChangeRisk:   manifest.Change.Risk,
		Artifact: EvidenceBundleArtifact{
			ID:          artifact.ID,
			Kind:        artifact.Kind,
			Path:        artifact.Path,
			SHA256:      sha256Hex(artifactData),
			Producer:    artifact.Producer,
			Command:     artifact.Command,
			ExitCode:    exitCode,
			Obligations: append([]string(nil), artifact.Obligations...),
		},
		Runner: EvidenceBundleRunner{
			Identity:   artifact.SignerIdentity,
			OIDCIssuer: artifact.SignerOIDCIssuer,
			AgentName:  manifest.Agent.Name,
			AgentRunID: manifest.Agent.RunID,
			AgentModel: manifest.Agent.Model,
		},
		Timestamp: "2026-05-07T00:00:00Z",
	}
	if mutate != nil {
		mutate(artifact, &bundle)
	}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeArtifact(t, repo, relPath, string(append(data, '\n')))
}

func installFakeCosign(t *testing.T, exitCode int) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cosign")
	script := "#!/bin/sh\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func obligationID(t *testing.T, spec Spec, kind ObligationKind, text string) string {
	t.Helper()
	ir := IRFromSpec(spec)
	for _, obligation := range ir.Obligations {
		if obligation.Kind == kind && obligation.Text == text {
			return obligation.ID
		}
	}
	t.Fatalf("obligation %s/%q not found in %#v", kind, text, ir.Obligations)
	return ""
}

func mustLoadManifest(t *testing.T, path string) Manifest {
	t.Helper()
	manifest, err := LoadJSON[Manifest](path)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findASTNode(ast IntentAST, key string) *ASTNode {
	for i := range ast.Nodes {
		if ast.Nodes[i].Key == key {
			return &ast.Nodes[i]
		}
	}
	return nil
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func errorAs(err error, target any) bool {
	switch typed := target.(type) {
	case *DiagnosticError:
		value, ok := err.(DiagnosticError)
		if ok {
			*typed = value
		}
		return ok
	default:
		return false
	}
}
