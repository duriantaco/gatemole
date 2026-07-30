package gatemole

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	bootstrap "github.com/duriantaco/gatemole/internal/gatemole/bootstrap"
)

type RepoCompileOutput struct {
	Result RepoCompileResult
	ASTs   IntentASTBundle
	Specs  SpecBundle
	IR     ObligationIRBundle
	Plan   VerificationPlanBundle
}

type RepoCompileResult struct {
	Version          string              `json:"version"`
	Repo             string              `json:"repo"`
	SpecsCompiled    int                 `json:"specs_compiled"`
	ObligationsBuilt int                 `json:"obligations_built"`
	Components       []CompiledComponent `json:"components"`
	Wrote            []string            `json:"wrote"`
	Diagnostics      []Diagnostic        `json:"diagnostics,omitempty"`
}

type CompiledComponent struct {
	Component   string   `json:"component"`
	Risk        Risk     `json:"risk"`
	IntentPath  string   `json:"intent_path"`
	ASTPath     string   `json:"ast_path"`
	SpecPath    string   `json:"spec_path"`
	Obligations []string `json:"obligations"`
}

type IntentASTBundle struct {
	Version string      `json:"version"`
	Repo    string      `json:"repo"`
	ASTs    []IntentAST `json:"asts"`
}

type SpecBundle struct {
	Version string `json:"version"`
	Repo    string `json:"repo"`
	Specs   []Spec `json:"specs"`
}

type ObligationIRBundle struct {
	Version     string       `json:"version"`
	Repo        string       `json:"repo"`
	Components  []IR         `json:"components"`
	Obligations []Obligation `json:"obligations"`
}

type VerificationPlanBundle struct {
	Version string             `json:"version"`
	Repo    string             `json:"repo"`
	Plans   []VerificationPlan `json:"plans"`
}

func CompileRepo(repo string) (RepoCompileOutput, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return RepoCompileOutput{}, err
	}
	output := RepoCompileOutput{
		Result: RepoCompileResult{
			Version:     CompileResultVersion,
			Repo:        absRepo,
			Components:  []CompiledComponent{},
			Wrote:       []string{},
			Diagnostics: []Diagnostic{},
		},
		ASTs:  IntentASTBundle{Version: ASTSchemaVersion, Repo: absRepo, ASTs: []IntentAST{}},
		Specs: SpecBundle{Version: SpecSchemaVersion, Repo: absRepo, Specs: []Spec{}},
		IR:    ObligationIRBundle{Version: ObligationsIRVersion, Repo: absRepo, Components: []IR{}, Obligations: []Obligation{}},
		Plan:  VerificationPlanBundle{Version: PlanBundleVersion, Repo: absRepo, Plans: []VerificationPlan{}},
	}

	if _, _, err := LoadReleasePolicy(absRepo, ""); err != nil {
		output.Result.Diagnostics = append(output.Result.Diagnostics, diagnostic("error", "compile.policy_missing", err.Error(), ".gatemole/policy/release-policy.json", SourceSpan{}))
	}
	intentPaths, err := repoIntentFiles(absRepo)
	if err != nil {
		return output, err
	}
	if len(intentPaths) == 0 {
		output.Result.Diagnostics = append(output.Result.Diagnostics, diagnostic("error", "compile.no_intents", "no intent YAML files found under .gatemole/intents", ".gatemole/intents", SourceSpan{}))
	}
	if HasErrorDiagnostics(output.Result.Diagnostics) {
		return output, DiagnosticError{Diagnostics: output.Result.Diagnostics}
	}

	generated := loadBootstrapGenerated(absRepo)
	for _, intentPath := range intentPaths {
		compiled, diagnostics, err := compileIntentForRepo(absRepo, intentPath, generated)
		output.Result.Diagnostics = append(output.Result.Diagnostics, diagnostics...)
		if err != nil {
			return output, err
		}
		if compiled == nil {
			continue
		}
		output.ASTs.ASTs = append(output.ASTs.ASTs, compiled.AST)
		output.Specs.Specs = append(output.Specs.Specs, compiled.Spec)
		output.IR.Components = append(output.IR.Components, compiled.IR)
		output.IR.Obligations = append(output.IR.Obligations, compiled.IR.Obligations...)
		output.Plan.Plans = append(output.Plan.Plans, compiled.Plan)
		output.Result.Components = append(output.Result.Components, compiled.Component)
		output.Result.SpecsCompiled++
		output.Result.ObligationsBuilt += len(compiled.IR.Obligations)
	}
	if HasErrorDiagnostics(output.Result.Diagnostics) {
		return output, DiagnosticError{Diagnostics: output.Result.Diagnostics}
	}
	if err := writeRepoCompileOutput(absRepo, &output); err != nil {
		return output, err
	}
	return output, nil
}

func (output RepoCompileOutput) EmitArtifact(name string) any {
	switch name {
	case "ast":
		return output.ASTs
	case "spec":
		return output.Specs
	case "ir":
		return output.IR
	case "plan":
		return output.Plan
	default:
		return output.Result
	}
}

type compiledIntent struct {
	AST       IntentAST
	Spec      Spec
	IR        IR
	Plan      VerificationPlan
	Component CompiledComponent
}

func compileIntentForRepo(repo string, intentPath string, generated generatedIndex) (*compiledIntent, []Diagnostic, error) {
	var diagnostics []Diagnostic
	ast, parseDiagnostics, err := ParseIntentASTFile(intentPath)
	if err != nil {
		return nil, diagnostics, err
	}
	diagnostics = append(diagnostics, parseDiagnostics...)
	if HasErrorDiagnostics(parseDiagnostics) {
		return nil, diagnostics, nil
	}
	typed, typedDiagnostics := AnalyzeIntentAST(ast)
	diagnostics = append(diagnostics, typedDiagnostics...)
	diagnostics = append(diagnostics, validateCompileIntent(ast, typed)...)
	if HasErrorDiagnostics(diagnostics) {
		return nil, diagnostics, nil
	}
	spec := SpecFromIntent(typed.Intent())
	specDiagnostics := stringDiagnostics("spec", ValidateSpec(spec))
	diagnostics = append(diagnostics, specDiagnostics...)
	if HasErrorDiagnostics(specDiagnostics) {
		return nil, diagnostics, nil
	}
	ir := IRFromSpec(spec)
	attachGeneratedInfo(&ir, generated)
	irDiagnostics := validateCompiledIR(ir)
	diagnostics = append(diagnostics, irDiagnostics...)
	if HasErrorDiagnostics(irDiagnostics) {
		return nil, diagnostics, nil
	}
	plan := VerificationPlanFromIR(ir, Manifest{
		Change: Change{
			Risk:         spec.Risk,
			SpecsTouched: []string{spec.ID},
		},
	})
	relIntent := repoRelativePath(repo, intentPath)
	astPath := filepath.ToSlash(filepath.Join(".gatemole", "build", "ast", spec.ID+".ast.json"))
	specPath := filepath.ToSlash(filepath.Join(".gatemole", "specs", spec.ID+".spec.json"))
	return &compiledIntent{
		AST:  ast,
		Spec: spec,
		IR:   ir,
		Plan: plan,
		Component: CompiledComponent{
			Component:   spec.ID,
			Risk:        spec.Risk,
			IntentPath:  relIntent,
			ASTPath:     astPath,
			SpecPath:    specPath,
			Obligations: obligationIDs(ir.Obligations),
		},
	}, diagnostics, nil
}

func validateCompileIntent(ast IntentAST, typed TypedIntent) []Diagnostic {
	var diagnostics []Diagnostic
	version := findCompileASTNode(ast, "version")
	if version == nil || strings.TrimSpace(version.Value) == "" {
		diagnostics = append(diagnostics, diagnostic("error", "compile.required_version", "version is required for repo-level compile", "version", SourceSpan{File: ast.File}))
	} else if version.Value != IntentSchemaVersion {
		diagnostics = append(diagnostics, diagnostic("error", "compile.invalid_version", fmt.Sprintf("version must be %s", IntentSchemaVersion), "version", version.Span))
	}
	if typed.Feature.Value != "" {
		if err := validateContractName(typed.Feature.Value); err != nil {
			diagnostics = append(diagnostics, diagnostic("error", "compile.invalid_name", err.Error(), "feature", typed.Feature.Span))
		}
	}
	if len(typed.OwnedPaths) == 0 {
		diagnostics = append(diagnostics, diagnostic("warning", "compile.missing_paths", "owned_paths is empty; generated plans may not map changed files", "owned_paths", typed.span("owned_paths")))
	}
	if typed.Owner.Value == "unowned" {
		diagnostics = append(diagnostics, diagnostic("warning", "compile.unowned_contract", "owner is unowned; assign a real owner before relying on this contract", "owner", typed.Owner.Span))
	}
	return diagnostics
}

func validateCompiledIR(ir IR) []Diagnostic {
	var diagnostics []Diagnostic
	for _, obligation := range ir.Obligations {
		if obligation.ID == "" {
			diagnostics = append(diagnostics, diagnostic("error", "compile.empty_obligation_id", "obligation id must be non-empty", "obligations", SourceSpan{}))
		}
		if strings.ContainsAny(obligation.ID, " \t\r\n") {
			diagnostics = append(diagnostics, diagnostic("error", "compile.unstable_obligation_id", fmt.Sprintf("obligation id %q contains whitespace", obligation.ID), "obligations", SourceSpan{}))
		}
		prefix := fmt.Sprintf("%s.%s.", ir.Feature, obligation.Kind)
		if !strings.HasPrefix(obligation.ID, prefix) {
			diagnostics = append(diagnostics, diagnostic("error", "compile.unstable_obligation_id", fmt.Sprintf("obligation id %q must start with %q", obligation.ID, prefix), "obligations", SourceSpan{}))
		}
		if !validObligationKind(obligation.Kind) {
			diagnostics = append(diagnostics, diagnostic("error", "compile.unknown_obligation_kind", fmt.Sprintf("unknown obligation kind %q", obligation.Kind), "obligations", SourceSpan{}))
		}
		if !validEvidenceKind(obligation.RequiredEvidence) {
			diagnostics = append(diagnostics, diagnostic("error", "compile.unknown_required_evidence", fmt.Sprintf("unknown required evidence %q", obligation.RequiredEvidence), "obligations", SourceSpan{}))
		}
	}
	return diagnostics
}

func validObligationKind(kind ObligationKind) bool {
	switch kind {
	case ObligationBehavior, ObligationSecurity, ObligationRequiredTest, ObligationRuntimeSignal, ObligationRollback:
		return true
	default:
		return false
	}
}

func writeRepoCompileOutput(repo string, output *RepoCompileOutput) error {
	for i, component := range output.Result.Components {
		if err := writeJSONFile(filepath.Join(repo, filepath.FromSlash(component.ASTPath)), output.ASTs.ASTs[i]); err != nil {
			return err
		}
		if err := writeJSONFile(filepath.Join(repo, filepath.FromSlash(component.SpecPath)), output.Specs.Specs[i]); err != nil {
			return err
		}
		output.Result.Wrote = append(output.Result.Wrote, component.ASTPath, component.SpecPath)
	}
	irPath := filepath.ToSlash(filepath.Join(".gatemole", "build", "obligations.ir.json"))
	planPath := filepath.ToSlash(filepath.Join(".gatemole", "build", "verification-plan.json"))
	if err := writeJSONFile(filepath.Join(repo, filepath.FromSlash(irPath)), output.IR); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(repo, filepath.FromSlash(planPath)), output.Plan); err != nil {
		return err
	}
	output.Result.Wrote = append(output.Result.Wrote, irPath, planPath)
	return nil
}

func RenderCompileResult(result RepoCompileResult) string {
	var b strings.Builder
	if len(result.Components) == 0 {
		b.WriteString("Compiled 0 contract drafts into 0 obligations.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "Compiled %d contract drafts into %d obligations.\n\n", result.SpecsCompiled, result.ObligationsBuilt)
	b.WriteString("Pipeline: repo signals -> contracts -> obligation IR -> verification plan\n\n")
	for _, component := range result.Components {
		fmt.Fprintf(&b, "%s %s\n", compileRiskLabel(component.Risk), component.Component)
		for _, obligationID := range component.Obligations {
			shortID := strings.TrimPrefix(obligationID, component.Component+".")
			fmt.Fprintf(&b, "  - %s\n", shortID)
		}
		b.WriteByte('\n')
	}
	if len(result.Wrote) > 0 {
		b.WriteString("Wrote:\n")
		for _, path := range result.Wrote {
			fmt.Fprintf(&b, "  %s\n", path)
		}
	}
	warnings := warningDiagnostics(result.Diagnostics)
	if len(warnings) > 0 {
		b.WriteString("Warnings:\n")
		for _, warning := range warnings {
			fmt.Fprintf(&b, "  - %s\n", FormatDiagnostic(warning))
		}
	}
	return b.String()
}

func repoIntentFiles(repo string) ([]string, error) {
	var paths []string
	for _, pattern := range []string{"*.yaml", "*.yml"} {
		matches, err := filepath.Glob(filepath.Join(repo, ".gatemole", "intents", pattern))
		if err != nil {
			return nil, err
		}
		paths = append(paths, matches...)
	}
	sort.Strings(paths)
	return paths, nil
}

func repoRelativePath(repo string, path string) string {
	rel, err := filepath.Rel(repo, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func obligationIDs(obligations []Obligation) []string {
	out := make([]string, 0, len(obligations))
	for _, obligation := range obligations {
		out = append(out, obligation.ID)
	}
	return out
}

func findCompileASTNode(ast IntentAST, key string) *ASTNode {
	for i := range ast.Nodes {
		if ast.Nodes[i].Key == key {
			return &ast.Nodes[i]
		}
	}
	return nil
}

func warningDiagnostics(diagnostics []Diagnostic) []Diagnostic {
	var out []Diagnostic
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "warning" {
			out = append(out, diagnostic)
		}
	}
	return out
}

func compileRiskLabel(risk Risk) string {
	switch risk {
	case RiskLow:
		return "LOW"
	case RiskMedium:
		return "MED"
	case RiskHigh:
		return "HIGH"
	case RiskCritical:
		return "CRIT"
	default:
		return strings.ToUpper(string(risk))
	}
}

type generatedIndex struct {
	byID       map[string]GeneratedInfo
	byKindText map[string]GeneratedInfo
}

func loadBootstrapGenerated(repo string) generatedIndex {
	index := generatedIndex{
		byID:       map[string]GeneratedInfo{},
		byKindText: map[string]GeneratedInfo{},
	}
	report, err := LoadJSON[bootstrap.Result](filepath.Join(repo, ".gatemole", "build", "bootstrap-report.json"))
	if err != nil {
		return index
	}
	for _, draft := range report.Drafts {
		for _, obligation := range draft.Obligations {
			generated := GeneratedInfo{
				By:         obligation.Generated.By,
				Mode:       obligation.Generated.Mode,
				Confidence: obligation.Generated.Confidence,
				Source: GeneratedSource{
					Type:   obligation.Generated.Source.Type,
					File:   obligation.Generated.Source.File,
					Symbol: obligation.Generated.Source.Symbol,
					Detail: obligation.Generated.Source.Detail,
				},
			}
			index.byID[obligation.ID] = generated
			index.byKindText[generatedKey(obligation.Kind, obligation.Description)] = generated
		}
	}
	return index
}

func attachGeneratedInfo(ir *IR, index generatedIndex) {
	for i := range ir.Obligations {
		generated, ok := index.byID[ir.Obligations[i].ID]
		if !ok {
			generated, ok = index.byKindText[generatedKey(string(ir.Obligations[i].Kind), ir.Obligations[i].Text)]
		}
		if ok {
			copy := generated
			ir.Obligations[i].Generated = &copy
		}
	}
}

func generatedKey(kind string, text string) string {
	return kind + "\x00" + text
}
