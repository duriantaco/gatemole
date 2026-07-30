package transaction

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/duriantaco/vouch/internal/kernel/model"
)

type SequenceOutcome string

const (
	SequenceAllow           SequenceOutcome = "allow"
	SequenceRequireApproval SequenceOutcome = "require_approval"
	SequenceRevise          SequenceOutcome = "revise"
	SequenceBlock           SequenceOutcome = "block"
)

type SequenceFinding struct {
	RuleID    string          `json:"rule_id"`
	Outcome   SequenceOutcome `json:"outcome"`
	Summary   string          `json:"summary"`
	EffectIDs []string        `json:"effect_ids"`
}

type SequenceDecision struct {
	Outcome  SequenceOutcome   `json:"outcome"`
	Findings []SequenceFinding `json:"findings"`
}

// SequencePolicy evaluates an ordered transaction prefix. It is deterministic:
// the same ordered effects and policy implementation produce the same result.
type SequencePolicy interface {
	Evaluate([]model.Effect) SequenceDecision
}

// BaselinePolicy contains the first domain-independent composition controls.
// Connector policy packs will replace path/name heuristics with typed semantic
// facts, but these rules establish the enforcement shape now.
type BaselinePolicy struct {
	DatabaseDeleteApprovalRows int64
}

func (policy BaselinePolicy) Digest() (string, error) {
	threshold := policy.DatabaseDeleteApprovalRows
	if threshold <= 0 {
		threshold = 100
	}
	return digestJSON(struct {
		Version                    string `json:"version"`
		DatabaseDeleteApprovalRows int64  `json:"database_delete_approval_rows"`
	}{
		Version:                    "gatemole.sequence_policy.baseline.v0",
		DatabaseDeleteApprovalRows: threshold,
	})
}

func PolicyDigest(policy SequencePolicy) (string, error) {
	digestible, ok := policy.(interface {
		Digest() (string, error)
	})
	if !ok {
		return "", fmt.Errorf("sequence policy does not expose an immutable digest")
	}
	return digestible.Digest()
}

func (policy BaselinePolicy) Evaluate(effects []model.Effect) SequenceDecision {
	threshold := policy.DatabaseDeleteApprovalRows
	if threshold == 0 {
		threshold = 100
	}
	findings := make([]SequenceFinding, 0)

	controlEffects := make([]string, 0)
	evidenceEffects := make([]string, 0)
	for _, effect := range effects {
		if isProtectedControl(effect) {
			if isVerificationResource(effect) {
				evidenceEffects = append(evidenceEffects, effect.ID)
			} else {
				controlEffects = append(controlEffects, effect.ID)
			}
		}
		if effect.RecoveryClass == model.RecoveryIrreversible {
			findings = append(findings, SequenceFinding{
				RuleID:    "gatemole.sequence.irreversible-release",
				Outcome:   SequenceRequireApproval,
				Summary:   "Irreversible effects require approval before release.",
				EffectIDs: []string{effect.ID},
			})
		}
		if isLargeDatabaseDelete(effect, threshold) {
			findings = append(findings, SequenceFinding{
				RuleID:    "gatemole.sequence.large-database-delete",
				Outcome:   SequenceRequireApproval,
				Summary:   fmt.Sprintf("Database deletion exceeds %d estimated rows.", threshold),
				EffectIDs: []string{effect.ID},
			})
		}
	}
	if len(controlEffects) > 0 && len(evidenceEffects) > 0 {
		findings = append(findings, SequenceFinding{
			RuleID:    "gatemole.sequence.control-and-evidence-coupling",
			Outcome:   SequenceRequireApproval,
			Summary:   "A protected control and its verification changed in the same transaction.",
			EffectIDs: append(append([]string(nil), controlEffects...), evidenceEffects...),
		})
	}
	if finding, found := delegatedAuthoritySelfUse(effects); found {
		findings = append(findings, finding)
	}

	outcome := SequenceAllow
	for _, finding := range findings {
		if sequenceOutcomeRank(finding.Outcome) > sequenceOutcomeRank(outcome) {
			outcome = finding.Outcome
		}
	}
	return SequenceDecision{Outcome: outcome, Findings: findings}
}

func isProtectedControl(effect model.Effect) bool {
	value := strings.ToLower(effect.Resource.Pattern + " " + effect.Operation)
	for _, marker := range []string{"auth", "permission", "security", "access-control", "policy"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func isVerificationResource(effect model.Effect) bool {
	path := strings.ToLower(filepath.ToSlash(effect.Resource.Pattern))
	base := filepath.Base(path)
	return strings.Contains(path, "/test/") || strings.Contains(path, "/tests/") ||
		strings.Contains(base, "_test.") || strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") || strings.Contains(path, "/fixtures/")
}

func isLargeDatabaseDelete(effect model.Effect, threshold int64) bool {
	if !strings.Contains(strings.ToLower(effect.System), "postgres") && !strings.Contains(strings.ToLower(effect.System), "database") {
		return false
	}
	if !strings.Contains(strings.ToLower(effect.Operation), "delete") || effect.EstimatedScope.Rows == nil {
		return false
	}
	return *effect.EstimatedScope.Rows > threshold
}

func delegatedAuthoritySelfUse(effects []model.Effect) (SequenceFinding, bool) {
	grants := make(map[string]struct{})
	for _, effect := range effects {
		operation := strings.ToLower(effect.Operation)
		if strings.Contains(operation, "grant") || strings.Contains(operation, "permission.create") {
			grants[effect.ID] = struct{}{}
			continue
		}
		for _, dependency := range effect.Dependencies {
			if _, dependsOnGrant := grants[dependency]; dependsOnGrant {
				return SequenceFinding{
					RuleID:    "gatemole.sequence.grant-then-self-use",
					Outcome:   SequenceBlock,
					Summary:   "The transaction attempts to consume authority it granted to itself.",
					EffectIDs: []string{dependency, effect.ID},
				}, true
			}
		}
	}
	return SequenceFinding{}, false
}

func sequenceOutcomeRank(outcome SequenceOutcome) int {
	switch outcome {
	case SequenceAllow:
		return 0
	case SequenceRequireApproval:
		return 1
	case SequenceRevise:
		return 2
	case SequenceBlock:
		return 3
	default:
		return 4
	}
}
