package transaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/duriantaco/gatemole/internal/kernel/model"
)

// ComputeEffectSetDigest hashes the immutable proposal fields in sequence
// order. Lifecycle status, receipts, and timestamps are deliberately excluded
// so commit and compensation do not mutate the approved proposal digest.
func ComputeEffectSetDigest(effects []model.Effect) (string, error) {
	type immutableEffect struct {
		ID                 string                    `json:"id"`
		TransactionID      string                    `json:"transaction_id"`
		Attempt            int64                     `json:"attempt,omitempty"`
		Sequence           int64                     `json:"sequence"`
		RunID              string                    `json:"run_id,omitempty"`
		OriginExecutionID  string                    `json:"origin_execution_id,omitempty"`
		OriginActionID     string                    `json:"origin_action_id,omitempty"`
		System             string                    `json:"system"`
		Resource           model.ResourceSelector    `json:"resource"`
		Operation          string                    `json:"operation"`
		ArgumentsDigest    string                    `json:"arguments_digest"`
		Dependencies       []string                  `json:"dependencies"`
		EstimatedScope     model.EffectScope         `json:"estimated_scope"`
		DataClassification string                    `json:"data_classification,omitempty"`
		RecoveryClass      model.EffectRecoveryClass `json:"recovery_class"`
		IdempotencyKey     string                    `json:"idempotency_key,omitempty"`
		StageDigest        string                    `json:"stage_digest,omitempty"`
		BeforeStateDigest  string                    `json:"before_state_digest,omitempty"`
		Compensation       *model.CompensationSpec   `json:"compensation,omitempty"`
	}

	values := make([]immutableEffect, len(effects))
	for i, effect := range effects {
		if effect.Sequence != int64(i+1) {
			return "", fmt.Errorf("effect %q has sequence %d at index %d", effect.ID, effect.Sequence, i)
		}
		value := immutableEffect{
			ID:                 effect.ID,
			TransactionID:      effect.TransactionID,
			Attempt:            effect.Attempt,
			Sequence:           effect.Sequence,
			RunID:              effect.RunID,
			OriginExecutionID:  effect.OriginExecutionID,
			OriginActionID:     effect.OriginActionID,
			System:             effect.System,
			Resource:           effect.Resource,
			Operation:          effect.Operation,
			ArgumentsDigest:    effect.ArgumentsDigest,
			Dependencies:       effect.Dependencies,
			EstimatedScope:     effect.EstimatedScope,
			DataClassification: effect.DataClassification,
			RecoveryClass:      effect.RecoveryClass,
			IdempotencyKey:     effect.IdempotencyKey,
			Compensation:       effect.Compensation,
		}
		if effect.StageRef != nil {
			value.StageDigest = effect.StageRef.Digest
		}
		if effect.BeforeStateRef != nil {
			value.BeforeStateDigest = effect.BeforeStateRef.Digest
		}
		values[i] = value
	}
	data, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode effect set: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func ComputeVerifierDigest(commandDigest, runtimeConfigDigest, imageDigest string) (string, error) {
	return digestJSON(struct {
		Version             string `json:"version"`
		CommandDigest       string `json:"command_digest"`
		RuntimeConfigDigest string `json:"runtime_config_digest"`
		ImageDigest         string `json:"image_digest"`
	}{
		Version:             "gatemole.verifier_identity.v0",
		CommandDigest:       commandDigest,
		RuntimeConfigDigest: runtimeConfigDigest,
		ImageDigest:         imageDigest,
	})
}

func ComputeCommandDigest(command []string) (string, error) {
	if len(command) == 0 {
		return "", fmt.Errorf("command is required")
	}
	return digestJSON(command)
}

func ComputeVerificationInputsDigest(effectSetDigest, stagedStateDigest string) (string, error) {
	return digestJSON(struct {
		Version           string `json:"version"`
		EffectSetDigest   string `json:"effect_set_digest"`
		StagedStateDigest string `json:"staged_state_digest"`
	}{
		Version:           "gatemole.verification_inputs.v0",
		EffectSetDigest:   effectSetDigest,
		StagedStateDigest: stagedStateDigest,
	})
}

func ComputeCommitPlanDigest(plan model.CommitPlan) (string, error) {
	plan.Digest = ""
	return digestJSON(plan)
}

func ComputeApprovalPackageDigest(approval model.ApprovalPackage) (string, error) {
	approval.Digest = ""
	return digestJSON(approval)
}

func ComputeApprovalDecisionDigest(decision model.ApprovalDecision) (string, error) {
	decision.Digest = ""
	decision.SignatureBase64 = ""
	return digestJSON(decision)
}

func digestJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode digest input: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
