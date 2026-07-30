package gatemole

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompileCommandBuildsRepoCompilerPipeline(t *testing.T) {
	repo := bootstrapFixture(t)
	var stdout, stderr bytes.Buffer

	code := Main([]string{"--repo", repo, "bootstrap"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("bootstrap failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Main([]string{"--repo", repo, "compile"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("compile failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"Compiled 1 contract drafts into",
		"Pipeline: repo signals -> contracts -> obligation IR -> verification plan",
		"HIGH auth.password_reset",
		"required_test.token_expiry",
		".gatemole/specs/auth.password_reset.spec.json",
		".gatemole/build/obligations.ir.json",
		".gatemole/build/verification-plan.json",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected compile output to contain %q, got:\n%s", want, out)
		}
	}
	if !fileExists(filepath.Join(repo, ".gatemole", "build", "ast", "auth.password_reset.ast.json")) {
		t.Fatal("compile did not write AST artifact")
	}
	spec := mustLoadSpecFile(t, filepath.Join(repo, ".gatemole", "specs", "auth.password_reset.spec.json"))
	if spec.ID != "auth.password_reset" || spec.Risk != RiskHigh {
		t.Fatalf("unexpected compiled spec: %#v", spec)
	}
	ir := mustLoadIRBundle(t, filepath.Join(repo, ".gatemole", "build", "obligations.ir.json"))
	requiredTest := findBundleObligation(ir, "auth.password_reset.required_test.token_expiry")
	if requiredTest == nil {
		t.Fatalf("missing compiled required-test obligation: %#v", ir.Obligations)
	}
	if requiredTest.Generated == nil || requiredTest.Generated.By != "gatemole.bootstrap" {
		t.Fatalf("generated provenance was not preserved: %#v", requiredTest)
	}
	plan := mustLoadPlanBundle(t, filepath.Join(repo, ".gatemole", "build", "verification-plan.json"))
	if len(plan.Plans) != 1 || plan.Plans[0].Feature != "auth.password_reset" {
		t.Fatalf("unexpected verification plan bundle: %#v", plan)
	}
}

func TestCompileEmitIRPrintsAggregateIR(t *testing.T) {
	repo := bootstrapFixture(t)
	var stdout, stderr bytes.Buffer

	if code := Main([]string{"--repo", repo, "bootstrap"}, &stdout, &stderr); code != 0 {
		t.Fatalf("bootstrap failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"--repo", repo, "compile", "--emit", "ir"}, &stdout, &stderr); code != 0 {
		t.Fatalf("compile --emit ir failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	ir, err := decodeJSON[ObligationIRBundle](stdout.Bytes())
	if err != nil {
		t.Fatalf("compile --emit ir did not print IR JSON: %v\n%s", err, stdout.String())
	}
	if ir.Version != ObligationsIRVersion || len(ir.Obligations) == 0 {
		t.Fatalf("unexpected emitted IR: %#v", ir)
	}
}

func TestCompileRequiresVersionedIntents(t *testing.T) {
	repo := initializedRepo(t)
	writeText(t, filepath.Join(repo, ".gatemole", "intents", "legacy.yaml"), `feature: legacy.service
owner: platform
owned_paths:
  - src/legacy/**
risk: medium
behavior:
  - legacy behavior
security:
  - legacy invariant
required_tests:
  - legacy test
runtime_metrics:
  - legacy.metric
rollback:
  strategy: revert_change
`)
	var stdout, stderr bytes.Buffer

	code := Main([]string{"--repo", repo, "compile"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected compile to reject missing version: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "compile.required_version") {
		t.Fatalf("expected missing version diagnostic, got stderr:\n%s", stderr.String())
	}
	if fileExists(filepath.Join(repo, ".gatemole", "specs", "legacy.service.spec.json")) {
		t.Fatal("compile wrote spec despite missing version")
	}
}

func mustLoadSpecFile(t *testing.T, path string) Spec {
	t.Helper()
	spec, err := LoadJSON[Spec](path)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func mustLoadIRBundle(t *testing.T, path string) ObligationIRBundle {
	t.Helper()
	ir, err := LoadJSON[ObligationIRBundle](path)
	if err != nil {
		t.Fatal(err)
	}
	return ir
}

func mustLoadPlanBundle(t *testing.T, path string) VerificationPlanBundle {
	t.Helper()
	plan, err := LoadJSON[VerificationPlanBundle](path)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func findBundleObligation(bundle ObligationIRBundle, id string) *Obligation {
	for i := range bundle.Obligations {
		if bundle.Obligations[i].ID == id {
			return &bundle.Obligations[i]
		}
	}
	return nil
}

func decodeJSON[T any](data []byte) (T, error) {
	var value T
	err := json.Unmarshal(data, &value)
	return value, err
}
