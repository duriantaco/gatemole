package gatemole

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

func BuildIRFile(specPath string, outPath string) (IR, error) {
	spec, err := LoadJSON[Spec](specPath)
	if err != nil {
		return IR{}, err
	}
	if errors := ValidateSpec(spec); len(errors) > 0 {
		return IR{}, fmt.Errorf("cannot build IR from invalid spec: %v", errors)
	}
	ir := IRFromSpec(spec)
	data, err := json.MarshalIndent(ir, "", "  ")
	if err != nil {
		return IR{}, err
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return IR{}, err
	}
	if err := os.WriteFile(outPath, append(data, '\n'), 0o644); err != nil {
		return IR{}, err
	}
	return ir, nil
}

func IRFromSpec(spec Spec) IR {
	builder := obligationBuilder{
		spec: spec,
		seen: make(map[string]bool),
	}
	var obligations []Obligation
	for i, item := range spec.Behavior {
		obligations = append(obligations, builder.obligation(ObligationBehavior, i+1, item, EvidenceBehaviorTrace))
	}
	for i, item := range spec.Security {
		obligations = append(obligations, builder.obligation(ObligationSecurity, i+1, item, EvidenceSecurityCheck))
	}
	for i, item := range spec.Tests.Required {
		obligations = append(obligations, builder.obligation(ObligationRequiredTest, i+1, item, EvidenceTestCoverage))
	}
	for i, item := range spec.Runtime.Metrics {
		obligations = append(obligations, builder.obligation(ObligationRuntimeSignal, i+1, item, EvidenceRuntimeMetric))
	}
	if spec.Rollback.Strategy != "" {
		text := spec.Rollback.Strategy
		if spec.Rollback.Flag != "" {
			text += ":" + spec.Rollback.Flag
		}
		obligations = append(obligations, builder.obligation(ObligationRollback, 1, text, EvidenceRollbackPlan))
	}
	return IR{
		Version:        IRSchemaVersion,
		Feature:        spec.ID,
		Owner:          spec.Owner,
		Risk:           spec.Risk,
		Obligations:    obligations,
		RequiredChecks: requiredChecks(spec),
		RuntimeSignals: append([]string(nil), spec.Runtime.Metrics...),
		Rollback:       spec.Rollback,
		ReleasePolicy:  releasePolicy(spec),
	}
}

type obligationBuilder struct {
	spec Spec
	seen map[string]bool
}

func (b *obligationBuilder) obligation(kind ObligationKind, index int, text string, evidence EvidenceKind) Obligation {
	return Obligation{
		ID:               b.obligationID(kind, text, index),
		Kind:             kind,
		Text:             text,
		Risk:             b.spec.Risk,
		Severity:         severityFor(b.spec.Risk, kind),
		Source:           fmt.Sprintf("spec:%s:%s[%d]", b.spec.ID, kind, index-1),
		RequiredEvidence: evidence,
	}
}

func (b *obligationBuilder) obligationID(kind ObligationKind, text string, index int) string {
	slug := obligationSlug(text)
	if slug == "" {
		slug = fmt.Sprintf("item_%d", index)
	}
	id := fmt.Sprintf("%s.%s.%s", b.spec.ID, kind, slug)
	if !b.seen[id] {
		b.seen[id] = true
		return id
	}
	fallback := fmt.Sprintf("%s.%d", id, index)
	b.seen[fallback] = true
	return fallback
}

func obligationSlug(text string) string {
	var b strings.Builder
	previousUnderscore := false
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			previousUnderscore = false
			continue
		}
		if !previousUnderscore {
			b.WriteByte('_')
			previousUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func severityFor(risk Risk, kind ObligationKind) string {
	if riskRank[risk] >= riskRank[RiskHigh] {
		return "high"
	}
	if kind == ObligationSecurity || kind == ObligationRollback {
		return "high"
	}
	return "medium"
}

func requiredChecks(spec Spec) []string {
	checks := []string{
		"typecheck",
		"unit_tests",
		"obligation_coverage",
		"evidence_artifact_resolution",
		"behavior_trace_verification",
		"test_evidence_verification",
	}
	if len(spec.Security) > 0 {
		checks = append(checks, "security_invariant_checks", "security_evidence_verification")
	}
	if riskRank[spec.Risk] >= riskRank[RiskHigh] {
		checks = append(checks, "rollback_plan", "runtime_metrics", "canary_required")
	}
	return checks
}

func releasePolicy(spec Spec) []string {
	if riskRank[spec.Risk] >= riskRank[RiskHigh] {
		return []string{
			"block if any required test lacks evidence",
			"block if any security invariant lacks evidence",
			"block if rollback strategy is missing",
			"block if runtime metrics are missing",
			"canary instead of auto-merge when evidence passes",
		}
	}
	return []string{
		"auto-merge allowed when required evidence passes",
		"block on any verifier finding with decision=block",
	}
}
