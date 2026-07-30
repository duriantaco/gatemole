package transaction

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
)

type PreparedAuthority struct {
	Plan                   model.CommitPlan
	Approval               model.ApprovalPackage
	TargetState            model.TransactionState
	OutstandingApprovalIDs []string
}

type ReleaseBinding struct {
	Connector               string
	Target                  model.ResourceSelector
	ExpectedResourceVersion string
}

func PrepareAuthority(
	projection Projection,
	decision SequenceDecision,
	policyDigest string,
	release ReleaseBinding,
	now time.Time,
) (PreparedAuthority, error) {
	if projection.Transaction.State != model.TransactionValidating {
		return PreparedAuthority{}, transitionError("authority preparation requires a validating transaction")
	}
	if !model.IsSHA256Digest(policyDigest) {
		return PreparedAuthority{}, transactionError(model.ErrorSchemaInvalid, "policy digest is invalid")
	}
	if !model.IsIdentifier(release.Connector) ||
		!model.IsIdentifier(release.Target.Kind) ||
		release.Target.Pattern == "" ||
		release.ExpectedResourceVersion == "" {
		return PreparedAuthority{}, transactionError(model.ErrorSchemaInvalid, "release binding is invalid")
	}
	if err := requirePassingVerification(projection); err != nil {
		return PreparedAuthority{}, err
	}
	now = now.UTC()
	for _, result := range projection.Verifications {
		if result.ExpiresAt != nil && !result.ExpiresAt.After(now) {
			return PreparedAuthority{}, transactionError(model.ErrorVerificationFailed, "verification evidence expired before authority preparation")
		}
	}
	for _, effect := range projection.Effects {
		if effect.RecoveryClass != model.RecoveryReadOnly && effect.Status != model.EffectValidated {
			return PreparedAuthority{}, transitionError("all releasable effects must be validated before authority preparation")
		}
		if effect.RecoveryClass == model.RecoveryIrreversible {
			return PreparedAuthority{}, transactionError(
				model.ErrorTransitionInvalid,
				"irreversible effect requires a connector-provided manual recovery playbook",
			)
		}
	}
	if decision.Outcome != SequenceAllow && decision.Outcome != SequenceRequireApproval {
		return PreparedAuthority{}, transitionError("blocked or revision-required sequence cannot prepare commit authority")
	}

	steps := make([]model.CommitStep, 0, len(projection.Effects))
	compensation := make([]model.CompensationStep, 0, len(projection.Effects))
	for _, effect := range projection.Effects {
		if effect.RecoveryClass == model.RecoveryReadOnly {
			continue
		}
		steps = append(steps, model.CommitStep{
			Sequence:        int64(len(steps) + 1),
			EffectID:        effect.ID,
			DependsOn:       append([]string(nil), effect.Dependencies...),
			PreconditionIDs: append([]string(nil), projection.Transaction.VerificationResultIDs...),
			IdempotencyKey:  effect.IdempotencyKey,
			TimeoutSeconds:  60,
			RequiresReceipt: true,
		})
	}
	for i := len(projection.Effects) - 1; i >= 0; i-- {
		effect := projection.Effects[i]
		step := model.CompensationStep{
			Sequence:        int64(len(compensation) + 1),
			EffectID:        effect.ID,
			PreconditionIDs: append([]string(nil), projection.Transaction.VerificationResultIDs...),
		}
		switch effect.RecoveryClass {
		case model.RecoveryReversible:
			step.PlanRef = effect.BeforeStateRef
			compensation = append(compensation, step)
		case model.RecoveryCompensatable:
			if effect.Compensation == nil {
				return PreparedAuthority{}, transitionError("compensatable effect is missing its compensation plan")
			}
			step.PlanRef = effect.Compensation.PlanRef
			compensation = append(compensation, step)
		}
	}
	plan := model.CommitPlan{
		Version:                 model.CommitPlanVersion,
		ID:                      authorityID("commit-plan", projection.Transaction.ID, now),
		TransactionID:           projection.Transaction.ID,
		IntentDigest:            projection.Transaction.IntentDigest,
		EffectSetDigest:         projection.Transaction.EffectSetDigest,
		StagedStateDigest:       projection.Transaction.StagedStateDigest,
		PolicyDigest:            policyDigest,
		Connector:               release.Connector,
		Target:                  release.Target,
		ExpectedResourceVersion: release.ExpectedResourceVersion,
		Steps:                   steps,
		CompensationSteps:       compensation,
		IrreversibleEffectIDs:   []string{},
		CreatedAt:               now,
	}
	var err error
	plan.Digest, err = ComputeCommitPlanDigest(plan)
	if err != nil {
		return PreparedAuthority{}, err
	}
	if err := plan.Validate(); err != nil {
		return PreparedAuthority{}, err
	}

	findings := make([]model.RiskFinding, len(decision.Findings))
	for i, finding := range decision.Findings {
		severity := "medium"
		if finding.Outcome == SequenceRequireApproval {
			severity = "high"
		}
		findings[i] = model.RiskFinding{
			Code: finding.RuleID, Severity: severity,
			Summary: finding.Summary, EffectIDs: append([]string(nil), finding.EffectIDs...),
		}
	}
	classes := []string{}
	target := model.TransactionReadyToCommit
	outstanding := []string{}
	if decision.Outcome == SequenceRequireApproval {
		classes = []string{"security-reviewer"}
		target = model.TransactionPendingApproval
		outstanding = []string{authorityID("approval", projection.Transaction.ID, now)}
	}
	approval := model.ApprovalPackage{
		Version:                 model.ApprovalPackageVersion,
		ID:                      authorityID("approval-package", projection.Transaction.ID, now),
		TransactionID:           projection.Transaction.ID,
		IntentDigest:            projection.Transaction.IntentDigest,
		EffectSetDigest:         projection.Transaction.EffectSetDigest,
		StagedStateDigest:       projection.Transaction.StagedStateDigest,
		PolicyDigest:            policyDigest,
		CommitPlanDigest:        plan.Digest,
		VerificationResultIDs:   append([]string(nil), projection.Transaction.VerificationResultIDs...),
		RiskFindings:            findings,
		RequiredApprovalClasses: classes,
		Summary: fmt.Sprintf(
			"%d verified effect(s); sequence decision %s; release %s to %s",
			len(projection.Effects), decision.Outcome,
			release.Connector, release.Target.Pattern,
		),
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	approval.Digest, err = ComputeApprovalPackageDigest(approval)
	if err != nil {
		return PreparedAuthority{}, err
	}
	if err := approval.Validate(); err != nil {
		return PreparedAuthority{}, err
	}
	return PreparedAuthority{
		Plan:                   plan,
		Approval:               approval,
		TargetState:            target,
		OutstandingApprovalIDs: outstanding,
	}, nil
}

func authorityID(prefix, transactionID string, now time.Time) string {
	sum := sha256.Sum256([]byte(prefix + "\x00" + transactionID + "\x00" + now.UTC().Format(time.RFC3339Nano)))
	return prefix + ":" + hex.EncodeToString(sum[:16])
}
