package gatemole

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

type CollectEvidenceOptions struct {
	RequireSigned bool
	PolicyPath    string
}

func CollectEvidence(repo string, manifestPath string) (Evidence, error) {
	return CollectEvidenceWithOptions(repo, manifestPath, CollectEvidenceOptions{})
}

func CollectEvidenceWithOptions(repo string, manifestPath string, opts CollectEvidenceOptions) (Evidence, error) {
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return Evidence{}, err
	}
	absManifest, err := filepath.Abs(manifestPath)
	if err != nil {
		return Evidence{}, err
	}
	manifest, err := LoadJSON[Manifest](absManifest)
	if err != nil {
		return Evidence{}, err
	}
	specs, err := LoadSpecs(absRepo)
	if err != nil {
		return Evidence{}, err
	}
	config := LoadConfigOrDefault(absRepo)
	pipeline := CompileManifestPipeline(specs, manifest)
	evidence := Evidence{
		Version:                EvidenceSchemaVersion,
		Repo:                   absRepo,
		ManifestPath:           absManifest,
		Compilation:            pipeline.Compilation,
		Manifest:               manifest,
		Specs:                  specs,
		IRs:                    pipeline.IRs,
		VerificationPlans:      pipeline.VerificationPlans,
		Diagnostics:            pipeline.Diagnostics,
		SignedEvidenceRequired: opts.RequireSigned,
		SpecErrors:             pipeline.SpecErrors,
		ManifestErrors:         pipeline.ManifestErrors,
		ArtifactResults:        []ArtifactResult{},
		InvalidEvidence:        []InvalidEvidence{},
		VerifierOutputs:        []VerifierOutput{},
		RequiredObligations:    make(map[string][]Obligation),
		CoveredObligations:     make(map[string][]Obligation),
		MissingObligations:     make(map[string][]Obligation),
		RequiredTests:          make(map[string][]string),
		CoveredTests:           make(map[string][]string),
		MissingTests:           make(map[string][]string),
		RequiredSecurity:       make(map[string][]string),
		CoveredSecurity:        make(map[string][]string),
		MissingSecurity:        make(map[string][]string),
		Findings:               []Finding{},
		Reasons:                []string{},
	}
	obligationIndex := NewObligationIndex(evidence.VerificationPlans)
	evidence.ArtifactResults, evidence.InvalidEvidence = LinkEvidenceArtifacts(absRepo, manifest, manifest.Verification.Artifacts, obligationIndex, ArtifactLinkOptions{
		RequireSigned:  opts.RequireSigned,
		AllowedSigners: config.AllowedSigners,
	})
	buildCoverage(&evidence)
	runVerifiers(&evidence)
	importVerifierFindings(&evidence)
	policy, policyPath, err := LoadReleasePolicy(absRepo, opts.PolicyPath)
	if err != nil {
		return Evidence{}, err
	}
	ApplyReleasePolicy(&evidence, policy, policyPath)
	return evidence, nil
}

func buildCoverage(evidence *Evidence) {
	behaviorCoverage := stringSet(evidence.Manifest.Verification.CoversBehavior)
	testCoverage := stringSet(evidence.Manifest.Verification.CoversTests)
	securityCoverage := stringSet(evidence.Manifest.Verification.CoversSecurity)
	runtimeCoverage := stringSet(evidence.Manifest.Runtime.Metrics)
	artifactCoverage := artifactCoverageByKind(evidence.ArtifactResults)
	useArtifacts := len(evidence.Manifest.Verification.Artifacts) > 0 || len(evidence.ArtifactResults) > 0
	for _, specID := range evidence.Manifest.Change.SpecsTouched {
		plan, ok := evidence.VerificationPlans[specID]
		if !ok {
			continue
		}
		for _, obligation := range plan.Obligations {
			evidence.RequiredObligations[specID] = append(evidence.RequiredObligations[specID], obligation)
			if obligationCovered(obligation, evidence, behaviorCoverage, testCoverage, securityCoverage, runtimeCoverage, artifactCoverage, useArtifacts) {
				evidence.CoveredObligations[specID] = append(evidence.CoveredObligations[specID], obligation)
			} else {
				evidence.MissingObligations[specID] = append(evidence.MissingObligations[specID], obligation)
			}
			switch obligation.Kind {
			case ObligationRequiredTest:
				evidence.RequiredTests[specID] = append(evidence.RequiredTests[specID], obligation.Text)
				if obligationCovered(obligation, evidence, behaviorCoverage, testCoverage, securityCoverage, runtimeCoverage, artifactCoverage, useArtifacts) {
					evidence.CoveredTests[specID] = append(evidence.CoveredTests[specID], obligation.Text)
				} else {
					evidence.MissingTests[specID] = append(evidence.MissingTests[specID], obligation.Text)
				}
			case ObligationSecurity:
				evidence.RequiredSecurity[specID] = append(evidence.RequiredSecurity[specID], obligation.Text)
				if obligationCovered(obligation, evidence, behaviorCoverage, testCoverage, securityCoverage, runtimeCoverage, artifactCoverage, useArtifacts) {
					evidence.CoveredSecurity[specID] = append(evidence.CoveredSecurity[specID], obligation.Text)
				} else {
					evidence.MissingSecurity[specID] = append(evidence.MissingSecurity[specID], obligation.Text)
				}
			}
		}
	}
}

func obligationCovered(obligation Obligation, evidence *Evidence, behaviorCoverage map[string]bool, testCoverage map[string]bool, securityCoverage map[string]bool, runtimeCoverage map[string]bool, artifactCoverage map[EvidenceKind]map[string]bool, useArtifacts bool) bool {
	if useArtifacts {
		if obligation.RequiredEvidence == EvidenceRollbackPlan {
			return evidence.Manifest.Rollback.Strategy != "" && artifactCoverage[obligation.RequiredEvidence][obligation.ID]
		}
		return artifactCoverage[obligation.RequiredEvidence][obligation.ID]
	}
	switch obligation.RequiredEvidence {
	case EvidenceBehaviorTrace:
		return behaviorCoverage[obligation.Text]
	case EvidenceTestCoverage:
		return testCoverage[obligation.Text]
	case EvidenceSecurityCheck:
		return securityCoverage[obligation.Text]
	case EvidenceRuntimeMetric:
		return runtimeCoverage[obligation.Text]
	case EvidenceRollbackPlan:
		return evidence.Manifest.Rollback.Strategy != ""
	default:
		return false
	}
}

func artifactCoverageByKind(artifacts []ArtifactResult) map[EvidenceKind]map[string]bool {
	coverage := make(map[EvidenceKind]map[string]bool)
	for _, artifact := range artifacts {
		if artifact.Status != "valid" {
			continue
		}
		if artifact.Kind == EvidenceVerifierOutput {
			continue
		}
		if coverage[artifact.Kind] == nil {
			coverage[artifact.Kind] = make(map[string]bool)
		}
		for _, obligationID := range artifact.CoveredObligations {
			coverage[artifact.Kind][obligationID] = true
		}
	}
	return coverage
}

func importVerifierFindings(evidence *Evidence) {
	for _, result := range evidence.ArtifactResults {
		if result.VerifierOutput != nil {
			evidence.VerifierOutputs = append(evidence.VerifierOutputs, *result.VerifierOutput)
		}
		evidence.Findings = append(evidence.Findings, result.VerifierFindings...)
	}
}

func runVerifiers(evidence *Evidence) {
	verifyCompilation(evidence)
	verifySpecAdherence(evidence)
	verifyArtifactEvidence(evidence)
	verifyBehaviorEvidence(evidence)
	verifyTestAdequacy(evidence)
	verifySecurityEvidence(evidence)
	verifyReleaseReadiness(evidence)
}

func verifyCompilation(evidence *Evidence) {
	if evidence.Compilation.ObligationsBuilt == 0 {
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "compiler",
			Severity:    "high",
			Decision:    "block",
			Claim:       "no obligations were compiled for this change",
			Evidence:    "change.specs_touched did not compile to any obligations",
			RequiredFix: "list touched specs or add an explicit no-contract-impact policy waiver",
		})
	}
}

func verifyArtifactEvidence(evidence *Evidence) {
	hasLinkedArtifacts := len(evidence.Manifest.Verification.Artifacts) > 0 || len(evidence.ArtifactResults) > 0
	if evidence.SignedEvidenceRequired && evidence.Compilation.ObligationsBuilt > 0 && !hasLinkedArtifacts {
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "evidence_linker",
			Severity:    "high",
			Decision:    "block",
			Claim:       "signed evidence artifacts are required",
			Evidence:    "gate --require-signed was used and verification.artifacts is empty",
			RequiredFix: "attach cosign-signed evidence artifacts for compiled obligations",
		})
	}
	if evidence.Compilation.ObligationsBuilt > 0 && !hasLinkedArtifacts && riskRank[evidence.Manifest.Change.Risk] >= riskRank[RiskMedium] {
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "evidence_linker",
			Severity:    "high",
			Decision:    "block",
			Claim:       "artifact-backed evidence is required for medium/high/critical changes",
			Evidence:    fmt.Sprintf("change risk is %s and verification.artifacts is empty", evidence.Manifest.Change.Risk),
			RequiredFix: "attach evidence artifacts that resolve to compiled obligations",
		})
	}
	for _, artifact := range evidence.ArtifactResults {
		if len(artifact.FailedTests) == 0 {
			continue
		}
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "test_adequacy",
			Severity:    "high",
			Decision:    "block",
			Claim:       fmt.Sprintf("test evidence artifact %s reported failing tests", artifact.ID),
			Evidence:    strings.Join(artifact.FailedTests, ", "),
			RequiredFix: "fix failing tests before release",
		})
	}
	for _, invalid := range evidence.InvalidEvidence {
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "evidence_linker",
			Severity:    "high",
			Decision:    "block",
			Claim:       fmt.Sprintf("evidence artifact %s is invalid", invalid.Artifact),
			Evidence:    invalid.Message,
			RequiredFix: "fix the evidence artifact or regenerate the manifest",
		})
	}
}

func verifySpecAdherence(evidence *Evidence) {
	for _, err := range evidence.SpecErrors {
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "spec_adherence",
			Severity:    "high",
			Decision:    "block",
			Claim:       "spec registry is invalid",
			Evidence:    err,
			RequiredFix: "fix the spec before release gating",
		})
	}
	for _, err := range evidence.ManifestErrors {
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "spec_adherence",
			Severity:    "high",
			Decision:    "block",
			Claim:       "change manifest is invalid",
			Evidence:    err,
			RequiredFix: "fix the manifest before release gating",
		})
	}
}

func verifyBehaviorEvidence(evidence *Evidence) {
	missingBySpec := missingTextsByKind(evidence, ObligationBehavior)
	for _, specID := range sortedStringKeys(missingBySpec) {
		missing := missingBySpec[specID]
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "behavior",
			Severity:    "high",
			Decision:    "block",
			Claim:       fmt.Sprintf("%s has uncovered behavior obligations", specID),
			Evidence:    strings.Join(missing, ", "),
			RequiredFix: "add behavior trace evidence for each changed contract",
		})
	}
}

func verifyTestAdequacy(evidence *Evidence) {
	if evidence.Manifest.Verification.TestResults.Failed > 0 {
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "test_adequacy",
			Severity:    "high",
			Decision:    "block",
			Claim:       "test suite failed",
			Evidence:    fmt.Sprintf("%d failing tests reported", evidence.Manifest.Verification.TestResults.Failed),
			RequiredFix: "fix failing tests before release",
		})
	}
	missingBySpec := missingTextsByKind(evidence, ObligationRequiredTest)
	for _, specID := range sortedStringKeys(missingBySpec) {
		missing := missingBySpec[specID]
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "test_adequacy",
			Severity:    "high",
			Decision:    "block",
			Claim:       fmt.Sprintf("%s has uncovered required tests", specID),
			Evidence:    strings.Join(missing, ", "),
			RequiredFix: "add tests or update verification coverage",
		})
	}
}

func verifySecurityEvidence(evidence *Evidence) {
	missingBySpec := missingTextsByKind(evidence, ObligationSecurity)
	for _, specID := range sortedStringKeys(missingBySpec) {
		missing := missingBySpec[specID]
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "security",
			Severity:    "high",
			Decision:    "block",
			Claim:       fmt.Sprintf("%s has uncovered security invariants", specID),
			Evidence:    strings.Join(missing, ", "),
			RequiredFix: "add security checks or negative tests for each invariant",
		})
	}
}

func verifyReleaseReadiness(evidence *Evidence) {
	missingRollback := missingTextsByKind(evidence, ObligationRollback)
	for _, specID := range sortedStringKeys(missingRollback) {
		missing := missingRollback[specID]
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "rollback",
			Severity:    "high",
			Decision:    "block",
			Claim:       fmt.Sprintf("%s has uncovered rollback obligations", specID),
			Evidence:    strings.Join(missing, ", "),
			RequiredFix: "add a rollback or compensation plan",
		})
	}
	missingRuntime := missingTextsByKind(evidence, ObligationRuntimeSignal)
	for _, specID := range sortedStringKeys(missingRuntime) {
		missing := missingRuntime[specID]
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "observability",
			Severity:    "high",
			Decision:    "block",
			Claim:       fmt.Sprintf("%s has uncovered runtime signal obligations", specID),
			Evidence:    strings.Join(missing, ", "),
			RequiredFix: "add metrics that can detect bad rollout behavior",
		})
	}
	if len(evidence.Manifest.Change.ExternalEffects) > 0 &&
		evidence.Manifest.Rollback.Strategy == "" &&
		len(evidence.Manifest.Rollback.Compensation) == 0 {
		evidence.Findings = append(evidence.Findings, Finding{
			Verifier:    "rollback",
			Severity:    "medium",
			Decision:    "block",
			Claim:       "external side effects need rollback or compensation",
			Evidence:    strings.Join(evidence.Manifest.Change.ExternalEffects, ", "),
			RequiredFix: "add rollback.strategy or rollback.compensation",
		})
	}
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func intersection(required []string, coverage map[string]bool) []string {
	var out []string
	for _, item := range required {
		if coverage[item] {
			out = append(out, item)
		}
	}
	return out
}

func missing(required []string, coverage map[string]bool) []string {
	var out []string
	for _, item := range required {
		if !coverage[item] {
			out = append(out, item)
		}
	}
	return out
}

func missingTextsByKind(evidence *Evidence, kind ObligationKind) map[string][]string {
	out := make(map[string][]string)
	for specID, obligations := range evidence.MissingObligations {
		for _, obligation := range obligations {
			if obligation.Kind == kind {
				out[specID] = append(out[specID], obligation.Text)
			}
		}
	}
	return out
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sortedStringKeys[V any](items map[string]V) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
