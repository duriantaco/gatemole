package vouch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const builtinPolicyPath = "builtin:default-release-policy"

func DefaultPolicyPath(repo string) string {
	return filepath.Join(repo, ".gatemole", "policy", "release-policy.json")
}

func DefaultReleasePolicy() ReleasePolicy {
	return ReleasePolicy{
		Version: PolicySchemaVersion,
		Rules: []PolicyRule{
			{
				ID:          "block_invalid_spec_or_manifest",
				Description: "Block when specs or the manifest are invalid.",
				When: PolicyCondition{
					Fact: "has_invalid_spec_or_manifest",
				},
				Decision: "block",
				Reasons:  []string{"invalid specs or manifest"},
				Stop:     true,
			},
			{
				ID:          "block_verifier_findings",
				Description: "Block on verifier findings that request a block decision.",
				When: PolicyCondition{
					Fact: "has_blocking_findings",
				},
				Decision:     "block",
				ReasonSource: "blocking_finding_claims",
				Stop:         true,
			},
			{
				ID:          "high_risk_canary",
				Description: "Allow high and critical risk changes only as canaries when evidence passes.",
				When: PolicyCondition{
					All: []PolicyCondition{
						{Fact: "no_policy_blockers"},
						{RiskAtLeast: RiskHigh},
						{Fact: "canary_enabled"},
					},
				},
				Decision: "canary",
				Reasons:  []string{"high-risk change has passing evidence and canary enabled"},
				Stop:     true,
			},
			{
				ID:          "high_risk_without_canary",
				Description: "Escalate high and critical risk changes when evidence passes but canary is absent.",
				When: PolicyCondition{
					All: []PolicyCondition{
						{Fact: "no_policy_blockers"},
						{RiskAtLeast: RiskHigh},
						{Not: &PolicyCondition{Fact: "canary_enabled"}},
					},
				},
				Decision: "human_escalation",
				Reasons:  []string{"high-risk change passed checks but has no canary"},
				Stop:     true,
			},
			{
				ID:          "low_medium_auto_merge",
				Description: "Allow low and medium risk changes when required evidence passes.",
				When: PolicyCondition{
					All: []PolicyCondition{
						{Fact: "no_policy_blockers"},
						{RiskBelow: RiskHigh},
					},
				},
				Decision: "auto_merge",
				Reasons:  []string{"low/medium risk change passed required evidence"},
				Stop:     true,
			},
		},
	}
}

func LoadReleasePolicy(repo string, policyPath string) (ReleasePolicy, string, error) {
	path := policyPath
	if path == "" {
		path = DefaultPolicyPath(repo)
	}
	resolved := path
	if path != "" && path != builtinPolicyPath && !filepath.IsAbs(path) {
		resolved = filepath.Join(repo, path)
	}
	if path == builtinPolicyPath {
		policy := DefaultReleasePolicy()
		return policy, builtinPolicyPath, ValidateReleasePolicy(policy)
	}
	policy, err := LoadJSON[ReleasePolicy](resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && policyPath == "" {
			return ReleasePolicy{}, "", fmt.Errorf("release policy not found at %s; run vouch init or pass --policy", resolved)
		}
		return ReleasePolicy{}, "", err
	}
	if err := ValidateReleasePolicy(policy); err != nil {
		return ReleasePolicy{}, "", err
	}
	return policy, resolved, nil
}

func ValidateReleasePolicy(policy ReleasePolicy) error {
	if policy.Version != PolicySchemaVersion {
		return fmt.Errorf("policy version must be %s", PolicySchemaVersion)
	}
	if len(policy.Rules) == 0 {
		return errors.New("policy requires at least one rule")
	}
	seen := make(map[string]bool)
	for _, rule := range policy.Rules {
		if strings.TrimSpace(rule.ID) == "" {
			return errors.New("policy rule id is required")
		}
		if seen[rule.ID] {
			return fmt.Errorf("duplicate policy rule id %q", rule.ID)
		}
		seen[rule.ID] = true
		if !validPolicyDecision(rule.Decision) {
			return fmt.Errorf("policy rule %s has invalid decision %q", rule.ID, rule.Decision)
		}
		if rule.ReasonSource != "" && !validReasonSource(rule.ReasonSource) {
			return fmt.Errorf("policy rule %s has invalid reason_source %q", rule.ID, rule.ReasonSource)
		}
		if err := validatePolicyCondition(rule.When); err != nil {
			return fmt.Errorf("policy rule %s: %w", rule.ID, err)
		}
	}
	return nil
}

func ApplyReleasePolicy(evidence *Evidence, policy ReleasePolicy, policyPath string) {
	input := PolicyInputFromEvidence(*evidence)
	result := EvaluateReleasePolicy(policy, policyPath, input)
	enforcePolicyFloor(input, &result)
	evidence.Decision = result.Decision
	evidence.Reasons = cloneStrings(result.Reasons)
	evidence.PolicyPath = result.PolicyPath
	evidence.PolicyResult = result
}

func EvaluateReleasePolicy(policy ReleasePolicy, policyPath string, input PolicyInput) PolicyResult {
	result := PolicyResult{
		Version:    PolicyResultVersion,
		PolicyPath: policyPath,
		Reasons:    []string{},
		RulesFired: []string{},
	}
	for _, rule := range policy.Rules {
		if !policyConditionMatches(rule.When, input) {
			continue
		}
		if result.Decision == "block" && rule.Decision != "block" {
			continue
		}
		result.Decision = rule.Decision
		result.RulesFired = append(result.RulesFired, rule.ID)
		result.Reasons = append(result.Reasons, policyRuleReasons(rule, input)...)
		if rule.Stop {
			break
		}
	}
	if result.Decision == "" {
		result.Decision = "block"
		result.RulesFired = append(result.RulesFired, "policy_no_match")
		result.Reasons = append(result.Reasons, "release policy produced no decision")
	}
	if len(result.Reasons) == 0 {
		result.Reasons = append(result.Reasons, fmt.Sprintf("policy rule %s selected %s", result.RulesFired[len(result.RulesFired)-1], result.Decision))
	}
	result.FiredPolicyRule = result.RulesFired[0]
	return result
}

func enforcePolicyFloor(input PolicyInput, result *PolicyResult) {
	if !input.HasInvalidSpecOrManifest && !input.HasBlockingFindings {
		return
	}
	if result.Decision == "block" {
		return
	}
	result.Decision = "block"
	result.RulesFired = append(result.RulesFired, "policy_floor_block")
	result.FiredPolicyRule = "policy_floor_block"
	result.Reasons = append(result.Reasons, policyFloorReasons(input)...)
	result.Reasons = uniqueStrings(result.Reasons)
}

func policyFloorReasons(input PolicyInput) []string {
	var reasons []string
	if input.HasInvalidSpecOrManifest {
		reasons = append(reasons, "invalid specs or manifest")
	}
	reasons = append(reasons, input.BlockingFindingClaims...)
	if len(reasons) == 0 {
		reasons = append(reasons, "policy floor blocked release")
	}
	return reasons
}

func PolicyInputFromEvidence(evidence Evidence) PolicyInput {
	blocking := []Finding{}
	claims := []string{}
	for _, finding := range evidence.Findings {
		if finding.Blocks() {
			blocking = append(blocking, finding)
			claims = append(claims, finding.Claim)
		}
	}
	return PolicyInput{
		Version:                  PolicyInputVersion,
		Manifest:                 evidence.Manifest,
		Compilation:              evidence.Compilation,
		Risk:                     evidence.Manifest.Change.Risk,
		RiskRank:                 riskRank[evidence.Manifest.Change.Risk],
		CanaryEnabled:            evidence.Manifest.Runtime.Canary.Enabled,
		SignedEvidenceRequired:   evidence.SignedEvidenceRequired,
		SpecErrors:               cloneStrings(evidence.SpecErrors),
		ManifestErrors:           cloneStrings(evidence.ManifestErrors),
		ArtifactResults:          cloneArtifactResults(evidence.ArtifactResults),
		InvalidEvidence:          cloneInvalidEvidence(evidence.InvalidEvidence),
		VerifierOutputs:          cloneVerifierOutputs(evidence.VerifierOutputs),
		MissingObligations:       obligationTextMap(evidence.MissingObligations),
		CoveredObligations:       obligationTextMap(evidence.CoveredObligations),
		Findings:                 cloneFindings(evidence.Findings),
		BlockingFindings:         blocking,
		BlockingFindingClaims:    claims,
		HasInvalidSpecOrManifest: len(evidence.SpecErrors) > 0 || len(evidence.ManifestErrors) > 0,
		HasInvalidEvidence:       len(evidence.InvalidEvidence) > 0,
		HasMissingObligations:    len(evidence.MissingObligations) > 0,
		HasBlockingFindings:      len(blocking) > 0,
	}
}

func WriteDefaultReleasePolicy(path string) error {
	return writeJSONFile(path, DefaultReleasePolicy())
}

func cloneFindings(values []Finding) []Finding {
	out := make([]Finding, 0, len(values))
	return append(out, values...)
}

func cloneVerifierOutputs(values []VerifierOutput) []VerifierOutput {
	out := make([]VerifierOutput, 0, len(values))
	return append(out, values...)
}

func policyRuleReasons(rule PolicyRule, input PolicyInput) []string {
	reasons := cloneStrings(rule.Reasons)
	switch rule.ReasonSource {
	case "":
	case "blocking_finding_claims":
		reasons = append(reasons, input.BlockingFindingClaims...)
	case "spec_errors":
		reasons = append(reasons, input.SpecErrors...)
	case "manifest_errors":
		reasons = append(reasons, input.ManifestErrors...)
	default:
		reasons = append(reasons, "unsupported reason source: "+rule.ReasonSource)
	}
	return uniqueStrings(reasons)
}

func policyConditionMatches(condition PolicyCondition, input PolicyInput) bool {
	if condition.Fact != "" && !policyFact(condition.Fact, input) {
		return false
	}
	if condition.RiskAtLeast != "" && riskRank[input.Risk] < riskRank[condition.RiskAtLeast] {
		return false
	}
	if condition.RiskBelow != "" && riskRank[input.Risk] >= riskRank[condition.RiskBelow] {
		return false
	}
	for _, child := range condition.All {
		if !policyConditionMatches(child, input) {
			return false
		}
	}
	if len(condition.Any) > 0 {
		matched := false
		for _, child := range condition.Any {
			if policyConditionMatches(child, input) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if condition.Not != nil && policyConditionMatches(*condition.Not, input) {
		return false
	}
	return true
}

func policyFact(name string, input PolicyInput) bool {
	switch name {
	case "always":
		return true
	case "has_invalid_spec_or_manifest":
		return input.HasInvalidSpecOrManifest
	case "has_blocking_findings":
		return input.HasBlockingFindings
	case "no_policy_blockers":
		return !input.HasInvalidSpecOrManifest && !input.HasBlockingFindings
	case "canary_enabled":
		return input.CanaryEnabled
	case "signed_evidence_required":
		return input.SignedEvidenceRequired
	case "has_invalid_evidence":
		return len(input.InvalidEvidence) > 0
	case "has_missing_obligations":
		return len(input.MissingObligations) > 0
	default:
		return false
	}
}

func validatePolicyCondition(condition PolicyCondition) error {
	if condition.Fact != "" && !validPolicyFact(condition.Fact) {
		return fmt.Errorf("invalid fact %q", condition.Fact)
	}
	if condition.RiskAtLeast != "" && !validRisk(condition.RiskAtLeast) {
		return fmt.Errorf("invalid risk_at_least %q", condition.RiskAtLeast)
	}
	if condition.RiskBelow != "" && !validRisk(condition.RiskBelow) {
		return fmt.Errorf("invalid risk_below %q", condition.RiskBelow)
	}
	for _, child := range condition.All {
		if err := validatePolicyCondition(child); err != nil {
			return err
		}
	}
	for _, child := range condition.Any {
		if err := validatePolicyCondition(child); err != nil {
			return err
		}
	}
	if condition.Not != nil {
		return validatePolicyCondition(*condition.Not)
	}
	return nil
}

func validPolicyDecision(decision string) bool {
	switch decision {
	case "block", "human_escalation", "canary", "auto_merge":
		return true
	default:
		return false
	}
}

func validReasonSource(source string) bool {
	switch source {
	case "blocking_finding_claims", "spec_errors", "manifest_errors":
		return true
	default:
		return false
	}
}

func validPolicyFact(fact string) bool {
	switch fact {
	case "always", "has_invalid_spec_or_manifest", "has_blocking_findings", "no_policy_blockers", "canary_enabled", "signed_evidence_required", "has_invalid_evidence", "has_missing_obligations":
		return true
	default:
		return false
	}
}
