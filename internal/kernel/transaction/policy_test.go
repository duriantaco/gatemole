package transaction

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
)

func TestSequencePolicyDetectsControlAndEvidenceCoupling(t *testing.T) {
	policy := BaselinePolicy{}
	control := policyEffect(1, "internal/auth/middleware.go", "modify", nil)
	testEffect := policyEffect(2, "internal/auth/middleware_test.go", "modify", nil)

	if decision := policy.Evaluate([]model.Effect{control}); decision.Outcome != SequenceAllow {
		t.Fatalf("control alone = %q, want allow: %#v", decision.Outcome, decision)
	}
	if decision := policy.Evaluate([]model.Effect{testEffect}); decision.Outcome != SequenceAllow {
		t.Fatalf("test alone = %q, want allow: %#v", decision.Outcome, decision)
	}
	decision := policy.Evaluate([]model.Effect{control, testEffect})
	if decision.Outcome != SequenceRequireApproval {
		t.Fatalf("combined sequence = %q, want approval: %#v", decision.Outcome, decision)
	}
	if !hasFinding(decision, "vouch.sequence.control-and-evidence-coupling") {
		t.Fatalf("missing coupling finding: %#v", decision)
	}
}

func TestSequencePolicyBlocksGrantThenSelfUse(t *testing.T) {
	grant := policyEffect(1, "role/deployer", "permission.grant", nil)
	use := policyEffect(2, "production/service", "deploy", []string{grant.ID})
	decision := (BaselinePolicy{}).Evaluate([]model.Effect{grant, use})
	if decision.Outcome != SequenceBlock || !hasFinding(decision, "vouch.sequence.grant-then-self-use") {
		t.Fatalf("decision = %#v, want self-use block", decision)
	}
}

func TestSequencePolicyRequiresApprovalForIrreversibleAndLargeDelete(t *testing.T) {
	rows := int64(101)
	effect := policyEffect(1, "customer.accounts", "delete", nil)
	effect.System = "postgres"
	effect.RecoveryClass = model.RecoveryIrreversible
	effect.EstimatedScope.Rows = &rows
	decision := (BaselinePolicy{DatabaseDeleteApprovalRows: 100}).Evaluate([]model.Effect{effect})
	if decision.Outcome != SequenceRequireApproval {
		t.Fatalf("outcome = %q, want approval", decision.Outcome)
	}
	if !hasFinding(decision, "vouch.sequence.irreversible-release") || !hasFinding(decision, "vouch.sequence.large-database-delete") {
		t.Fatalf("missing irreversible/delete findings: %#v", decision)
	}
}

func policyEffect(sequence int64, resource, operation string, dependencies []string) model.Effect {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	return model.Effect{
		Version:         model.EffectVersion,
		ID:              "effect:policy:" + string(rune('0'+sequence)),
		TransactionID:   "tx:policy",
		Sequence:        sequence,
		System:          "git",
		Resource:        model.ResourceSelector{Kind: "file", Pattern: resource},
		Operation:       operation,
		Arguments:       json.RawMessage(`{}`),
		ArgumentsDigest: digest("1"),
		Dependencies:    dependencies,
		EstimatedScope:  model.EffectScope{},
		RecoveryClass:   model.RecoveryStageable,
		Status:          model.EffectProposed,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

func hasFinding(decision SequenceDecision, ruleID string) bool {
	for _, finding := range decision.Findings {
		if finding.RuleID == ruleID {
			return true
		}
	}
	return false
}
