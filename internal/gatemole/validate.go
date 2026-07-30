package gatemole

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

func ValidateSpec(spec Spec) []string {
	var errors []string
	specID := spec.ID
	if specID == "" {
		specID = "<missing id>"
	}
	if spec.ID == "" {
		errors = append(errors, fmt.Sprintf("%s: missing field id", specID))
	}
	if spec.Version != "" && spec.Version != SpecSchemaVersion {
		errors = append(errors, fmt.Sprintf("%s: unsupported spec version %q", specID, spec.Version))
	}
	if spec.Owner == "" {
		errors = append(errors, fmt.Sprintf("%s: missing field owner", specID))
	}
	errors = append(errors, validateList(specID, "owned_paths", spec.OwnedPaths)...)
	errors = append(errors, validateOwnedPaths(specID, "owned_paths", spec.OwnedPaths)...)
	if !validRisk(spec.Risk) {
		errors = append(errors, fmt.Sprintf("%s: invalid risk %q", specID, spec.Risk))
	}
	if len(spec.Behavior) == 0 {
		errors = append(errors, fmt.Sprintf("%s: behavior must be non-empty", specID))
	}
	errors = append(errors, validateList(specID, "behavior", spec.Behavior)...)
	if len(spec.Security) == 0 {
		errors = append(errors, fmt.Sprintf("%s: security must be non-empty", specID))
	}
	errors = append(errors, validateList(specID, "security", spec.Security)...)
	if len(spec.Tests.Required) == 0 {
		errors = append(errors, fmt.Sprintf("%s: tests.required must be non-empty", specID))
	}
	errors = append(errors, validateList(specID, "tests.required", spec.Tests.Required)...)
	if len(spec.Runtime.Metrics) == 0 {
		errors = append(errors, fmt.Sprintf("%s: runtime.metrics must be non-empty", specID))
	}
	errors = append(errors, validateList(specID, "runtime.metrics", spec.Runtime.Metrics)...)
	if spec.Rollback.Strategy == "" {
		errors = append(errors, fmt.Sprintf("%s: rollback.strategy is required", specID))
	}
	return errors
}

func ValidateManifest(manifest Manifest, specs map[string]Spec) []string {
	var errors []string
	if manifest.Version != "" && manifest.Version != ManifestSchemaVersion {
		errors = append(errors, fmt.Sprintf("manifest: unsupported version %q", manifest.Version))
	}
	if manifest.Task.ID == "" || manifest.Task.Summary == "" {
		errors = append(errors, "manifest: task.id and task.summary are required")
	}
	if !validRisk(manifest.Change.Risk) {
		errors = append(errors, "manifest: change.risk must be one of low, medium, high, critical")
	}
	if manifest.Change.BehaviorChanged && len(manifest.Change.SpecsTouched) == 0 {
		errors = append(errors, "manifest: behavior_changed requires change.specs_touched")
	}
	for _, specID := range manifest.Change.SpecsTouched {
		spec, ok := specs[specID]
		if !ok {
			errors = append(errors, fmt.Sprintf("manifest: referenced spec not found: %s", specID))
			continue
		}
		if validRisk(manifest.Change.Risk) && validRisk(spec.Risk) && riskRank[manifest.Change.Risk] < riskRank[spec.Risk] {
			errors = append(errors, fmt.Sprintf("manifest: change.risk %q cannot be lower than touched spec %s risk %q", manifest.Change.Risk, specID, spec.Risk))
		}
	}
	errors = append(errors, validateList("manifest", "change.specs_touched", manifest.Change.SpecsTouched)...)
	errors = append(errors, validateList("manifest", "change.external_effects", manifest.Change.ExternalEffects)...)
	errors = append(errors, validateList("manifest", "verification.covers_behavior", manifest.Verification.CoversBehavior)...)
	errors = append(errors, validateList("manifest", "verification.covers_tests", manifest.Verification.CoversTests)...)
	errors = append(errors, validateList("manifest", "verification.covers_security", manifest.Verification.CoversSecurity)...)
	errors = append(errors, validateList("manifest", "runtime.metrics", manifest.Runtime.Metrics)...)
	if len(manifest.Verification.Commands) == 0 {
		errors = append(errors, "manifest: verification.commands must list the checks that ran")
	}
	errors = append(errors, validateList("manifest", "verification.commands", manifest.Verification.Commands)...)
	errors = append(errors, validateArtifacts(manifest.Verification.Artifacts)...)
	if manifest.Runtime.Canary.Enabled && (manifest.Runtime.Canary.InitialPercent <= 0 || manifest.Runtime.Canary.InitialPercent > 100) {
		errors = append(errors, "manifest: runtime.canary.initial_percent must be between 1 and 100 when canary is enabled")
	}
	return errors
}

func validRisk(risk Risk) bool {
	_, ok := riskRank[risk]
	return ok
}

func validEvidenceKind(kind EvidenceKind) bool {
	switch kind {
	case EvidenceBehaviorTrace, EvidenceSecurityCheck, EvidenceTestCoverage, EvidenceRuntimeMetric, EvidenceRollbackPlan, EvidenceVerifierOutput:
		return true
	default:
		return false
	}
}

func validateArtifacts(artifacts []EvidenceArtifact) []string {
	var errors []string
	seenIDs := make(map[string]bool, len(artifacts))
	for i, artifact := range artifacts {
		owner := fmt.Sprintf("manifest: verification.artifacts[%d]", i)
		if artifact.ID == "" {
			errors = append(errors, owner+" missing id")
		} else if seenIDs[artifact.ID] {
			errors = append(errors, fmt.Sprintf("manifest: verification.artifacts contains duplicate id %q", artifact.ID))
		}
		seenIDs[artifact.ID] = true
		if !validEvidenceKind(artifact.Kind) {
			errors = append(errors, fmt.Sprintf("%s has unsupported kind %q", owner, artifact.Kind))
		}
		if artifact.Producer == "" {
			errors = append(errors, owner+" missing producer")
		}
		if artifact.Path == "" {
			errors = append(errors, owner+" requires path")
		}
		if artifact.ExitCode == nil {
			errors = append(errors, owner+" missing exit_code")
		}
		if artifact.SHA256 != "" {
			decoded, err := hex.DecodeString(artifact.SHA256)
			if err != nil || len(decoded) != 32 {
				errors = append(errors, owner+" sha256 must be 64 hex characters")
			}
		}
		hasSignedField := artifact.EvidenceBundle != "" || artifact.SignatureBundle != "" || artifact.SignerIdentity != "" || artifact.SignerOIDCIssuer != ""
		if hasSignedField && (artifact.EvidenceBundle == "" || artifact.SignatureBundle == "" || artifact.SignerIdentity == "" || artifact.SignerOIDCIssuer == "") {
			errors = append(errors, owner+" signed artifacts require evidence_bundle, signature_bundle, signer_identity, and signer_oidc_issuer together")
		}
		if len(artifact.Obligations) == 0 {
			errors = append(errors, owner+" must reference at least one obligation")
		}
		errors = append(errors, validateList("manifest", fmt.Sprintf("verification.artifacts[%d].obligations", i), artifact.Obligations)...)
	}
	return errors
}

func validateList(owner string, field string, values []string) []string {
	var errors []string
	seen := make(map[string]bool, len(values))
	for i, value := range values {
		if value == "" {
			errors = append(errors, fmt.Sprintf("%s: %s[%d] must be non-empty", owner, field, i))
			continue
		}
		if seen[value] {
			errors = append(errors, fmt.Sprintf("%s: %s contains duplicate value %q", owner, field, value))
		}
		seen[value] = true
	}
	return errors
}

func validateOwnedPaths(owner string, field string, values []string) []string {
	var errors []string
	for i, value := range values {
		if value == "" {
			continue
		}
		if filepath.IsAbs(value) {
			errors = append(errors, fmt.Sprintf("%s: %s[%d] must be repo-relative", owner, field, i))
			continue
		}
		normalized := filepath.ToSlash(filepath.Clean(value))
		if normalized == "." || normalized == ".." || strings.HasPrefix(normalized, "../") || strings.Contains(normalized, "/../") {
			errors = append(errors, fmt.Sprintf("%s: %s[%d] must not escape the repo", owner, field, i))
		}
	}
	return errors
}

func validateOwnedPathValues(owner string, field string, values []string, span SourceSpan) []Diagnostic {
	var diagnostics []Diagnostic
	for _, message := range validateOwnedPaths(owner, field, values) {
		diagnostics = append(diagnostics, diagnostic("error", "intent.invalid_owned_path", message, field, span))
	}
	return diagnostics
}
