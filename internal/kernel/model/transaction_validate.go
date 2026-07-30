package model

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
)

func (transaction AgentTransaction) Validate() error {
	const resource = "AgentTransaction"
	if err := validateVersion(resource, transaction.Version, AgentTransactionVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", transaction.ID},
		{"namespace", transaction.Namespace},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	stageIDs := make(map[string]struct{}, len(transaction.StageBindings))
	for i, binding := range transaction.StageBindings {
		field := fmt.Sprintf("stage_bindings[%d]", i)
		if err := validateIdentifier(resource, field+".id", binding.ID); err != nil {
			return err
		}
		if _, exists := stageIDs[binding.ID]; exists {
			return invalid(resource, field+".id", "stage binding IDs must be unique")
		}
		stageIDs[binding.ID] = struct{}{}
		if err := validateIdentifier(resource, field+".kind", binding.Kind); err != nil {
			return err
		}
		if err := validateResourceSelector(resource, field+".resource", binding.Resource); err != nil {
			return err
		}
		if strings.TrimSpace(binding.Location) == "" || strings.ContainsRune(binding.Location, 0) {
			return invalid(resource, field+".location", "stage location is required")
		}
		if strings.TrimSpace(binding.BaseRevision) == "" {
			return invalid(resource, field+".base_revision", "base revision is required")
		}
		if binding.CreatedAt.IsZero() {
			return invalid(resource, field+".created_at", "created_at is required")
		}
	}
	if err := validateDigest(resource, "intent_digest", transaction.IntentDigest); err != nil {
		return err
	}
	if transaction.Task != nil {
		if err := transaction.Task.Validate(); err != nil {
			return err
		}
		if transaction.Task.TransactionID != transaction.ID ||
			transaction.Task.Namespace != transaction.Namespace ||
			transaction.Task.IntentDigest != transaction.IntentDigest ||
			!transaction.Task.CreatedAt.Equal(transaction.CreatedAt) {
			return invalid(resource, "task", "task identity, intent, and creation time must match the transaction")
		}
		if !slices.Contains(transaction.AgentRunIDs, transaction.Task.RunID) {
			return invalid(resource, "task.run_id", "task run must be attached to the transaction")
		}
	}
	if transaction.Admission != nil {
		admission := transaction.Admission
		if admission.RuntimeID != "" &&
			!IsRuntimeID(admission.RuntimeID) {
			return invalid(
				resource,
				"admission.runtime_id",
				"Runtime ID must be canonical",
			)
		}
		if (admission.RuntimeID == "") !=
			(admission.EnforcementProfile == "") ||
			(admission.EnforcementProfile != "" &&
				!IsEnforcementProfile(admission.EnforcementProfile)) {
			return invalid(
				resource,
				"admission.enforcement_profile",
				"Runtime ID and enforcement profile must form one valid binding",
			)
		}
		if err := validateDigest(resource, "admission.task_digest", admission.TaskDigest); err != nil {
			return err
		}
		if err := validateIdentifier(resource, "admission.run_id", admission.RunID); err != nil {
			return err
		}
		if err := validateDigest(resource, "admission.contract_digest", admission.ContractDigest); err != nil {
			return err
		}
		if transaction.Task == nil {
			return invalid(resource, "admission.task_digest", "admission requires the bound task")
		}
		if admission.TaskDigest != transaction.Task.Digest {
			return invalid(resource, "admission.task_digest", "admission task digest does not match the transaction task")
		}
		if admission.RunID != transaction.Task.RunID {
			return invalid(resource, "admission.run_id", "admission run does not match the transaction task")
		}
		if !slices.Contains(transaction.AgentRunIDs, admission.RunID) {
			return invalid(resource, "admission.run_id", "admission run must be attached to the transaction")
		}
	}
	if err := validatePrincipal(resource, "sponsor", transaction.Sponsor); err != nil {
		return err
	}
	if transaction.Sponsor.Kind == PrincipalAgent || transaction.Sponsor.Kind == PrincipalRun {
		return identityInvalid(resource, "sponsor.kind", "an agent or run cannot sponsor its own transaction")
	}
	for _, item := range []struct {
		field  string
		values []string
	}{
		{"agent_run_ids", transaction.AgentRunIDs},
		{"effect_ids", transaction.EffectIDs},
		{"verification_result_ids", transaction.VerificationResultIDs},
		{"outstanding_approval_ids", transaction.OutstandingApprovalIDs},
	} {
		if err := validateUniqueIdentifiers(resource, item.field, item.values); err != nil {
			return err
		}
	}
	if !validTransactionState(transaction.State) {
		return invalid(resource, "state", "unknown transaction state")
	}
	if transactionStateRequiresReason(transaction.State) && strings.TrimSpace(transaction.StateReason) == "" {
		return invalid(resource, "state_reason", "this transaction state requires a reason")
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"staged_state_digest", transaction.StagedStateDigest},
		{"effect_set_digest", transaction.EffectSetDigest},
		{"approval_package_digest", transaction.ApprovalPackageDigest},
		{"commit_plan_digest", transaction.CommitPlanDigest},
	} {
		if item.value != "" {
			if err := validateDigest(resource, item.field, item.value); err != nil {
				return err
			}
		}
	}
	if transactionStateRequiresFrozenEffects(transaction.State) {
		if transaction.StagedStateDigest == "" || transaction.EffectSetDigest == "" {
			return invalid(resource, "effect_set_digest", "state requires staged_state_digest and effect_set_digest")
		}
	}
	if transaction.State == TransactionPendingApproval {
		if transaction.ApprovalPackageDigest == "" || len(transaction.OutstandingApprovalIDs) == 0 {
			return invalid(resource, "approval_package_digest", "pending approval requires a package and outstanding approval")
		}
	}
	if transactionStateRequiresCommitPlan(transaction.State) && transaction.CommitPlanDigest == "" {
		return invalid(resource, "commit_plan_digest", "state requires a frozen commit plan")
	}
	if transaction.EventSequence < 1 {
		return invalid(resource, "event_sequence", "event sequence must start at 1")
	}
	if transaction.CreatedAt.IsZero() || transaction.UpdatedAt.IsZero() {
		return invalid(resource, "created_at", "created_at and updated_at are required")
	}
	if transaction.UpdatedAt.Before(transaction.CreatedAt) {
		return invalid(resource, "updated_at", "updated_at cannot precede created_at")
	}
	if transaction.State.Terminal() {
		if transaction.CompletedAt == nil || transaction.CompletedAt.IsZero() {
			return invalid(resource, "completed_at", "terminal states require completed_at")
		}
		if transaction.CompletedAt.Before(transaction.CreatedAt) {
			return invalid(resource, "completed_at", "completed_at cannot precede created_at")
		}
	} else if transaction.CompletedAt != nil {
		return invalid(resource, "completed_at", "non-terminal states cannot have completed_at")
	}
	return nil
}

func (execution AgentExecution) Validate() error {
	const resource = "AgentExecution"
	if err := validateVersion(resource, execution.Version, AgentExecutionVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", execution.ID},
		{"transaction_id", execution.TransactionID},
		{"run_id", execution.RunID},
		{"stage_binding_id", execution.StageBindingID},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if strings.TrimSpace(execution.Program) == "" || strings.ContainsRune(execution.Program, 0) {
		return invalid(resource, "program", "program is required and cannot contain NUL")
	}
	if len(execution.Program) > 4096 {
		return invalid(resource, "program", "program cannot exceed 4096 bytes")
	}
	if err := validateDigest(resource, "command_digest", execution.CommandDigest); err != nil {
		return err
	}
	if execution.RuntimeClass != "oci" && execution.RuntimeClass != "host" {
		return invalid(resource, "runtime_class", "runtime class must be oci or host")
	}
	if err := validateDigest(resource, "runtime_config_digest", execution.RuntimeConfigDigest); err != nil {
		return err
	}
	if execution.RuntimeClass == "oci" {
		if err := validateDigest(resource, "image_digest", execution.ImageDigest); err != nil {
			return err
		}
	} else if execution.ImageDigest != "" {
		return invalid(resource, "image_digest", "host execution cannot claim a container image digest")
	}
	if execution.TaskDigest != "" {
		if err := validateDigest(resource, "task_digest", execution.TaskDigest); err != nil {
			return err
		}
	}
	if execution.ModelBroker != nil {
		broker := execution.ModelBroker
		if execution.RuntimeClass != "oci" || !IsIdentifier(broker.Provider) {
			return invalid(resource, "model_broker", "model broker requires an OCI execution and valid provider")
		}
		for _, item := range []struct {
			field string
			value string
		}{
			{"model_broker.image_digest", broker.ImageDigest},
			{"model_broker.policy_digest", broker.PolicyDigest},
		} {
			if err := validateDigest(resource, item.field, item.value); err != nil {
				return err
			}
		}
		if broker.Calls < 0 || broker.CompletedCalls < 0 ||
			broker.FailedCalls < 0 || broker.UnknownCalls < 0 ||
			broker.InputTokens < 0 || broker.OutputTokens < 0 {
			return invalid(resource, "model_broker", "model broker counters cannot be negative")
		}
		if broker.CompletedCalls+broker.FailedCalls+broker.UnknownCalls != broker.Calls {
			return invalid(resource, "model_broker", "model broker call outcomes must sum to calls")
		}
		if broker.ReceiptLedgerDigest != "" {
			if err := validateDigest(resource, "model_broker.receipt_ledger_digest", broker.ReceiptLedgerDigest); err != nil {
				return err
			}
		}
	}
	if !validAgentExecutionStatus(execution.Status) {
		return invalid(resource, "status", "unknown agent execution status")
	}
	if execution.StartedAt.IsZero() {
		return invalid(resource, "started_at", "started_at is required")
	}
	if execution.Status == AgentExecutionRunning {
		if execution.ExitCode != nil || execution.CompletedAt != nil ||
			execution.StdoutDigest != "" || execution.StderrDigest != "" {
			return invalid(resource, "status", "running execution cannot contain completion fields")
		}
		if execution.ModelBroker != nil &&
			(execution.ModelBroker.ReceiptLedgerDigest != "" ||
				execution.ModelBroker.Calls != 0 ||
				execution.ModelBroker.CompletedCalls != 0 ||
				execution.ModelBroker.FailedCalls != 0 ||
				execution.ModelBroker.UnknownCalls != 0 ||
				execution.ModelBroker.InputTokens != 0 ||
				execution.ModelBroker.OutputTokens != 0) {
			return invalid(resource, "model_broker", "running execution cannot contain final model receipts")
		}
		return nil
	}
	if execution.CompletedAt == nil || execution.CompletedAt.IsZero() {
		return invalid(resource, "completed_at", "terminal execution requires completed_at")
	}
	if execution.CompletedAt.Before(execution.StartedAt) {
		return invalid(resource, "completed_at", "completed_at cannot precede started_at")
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"stdout_digest", execution.StdoutDigest},
		{"stderr_digest", execution.StderrDigest},
	} {
		if err := validateDigest(resource, item.field, item.value); err != nil {
			return err
		}
	}
	switch execution.Status {
	case AgentExecutionSucceeded:
		if execution.ExitCode == nil || *execution.ExitCode != 0 {
			return invalid(resource, "exit_code", "successful execution requires exit code 0")
		}
	case AgentExecutionFailed:
		if execution.ExitCode == nil || *execution.ExitCode == 0 {
			return invalid(resource, "exit_code", "failed execution requires a non-zero exit code")
		}
	case AgentExecutionInterrupted, AgentExecutionStartFailed:
		if execution.ExitCode != nil {
			return invalid(resource, "exit_code", "interrupted and start-failed executions cannot claim an exit code")
		}
	}
	if execution.ModelBroker != nil &&
		execution.Status != AgentExecutionInterrupted &&
		execution.ModelBroker.ReceiptLedgerDigest == "" {
		return invalid(resource, "model_broker.receipt_ledger_digest", "terminal brokered execution requires a receipt ledger digest")
	}
	return nil
}

func (effect Effect) Validate() error {
	const resource = "Effect"
	if err := validateVersion(resource, effect.Version, EffectVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", effect.ID},
		{"transaction_id", effect.TransactionID},
		{"system", effect.System},
		{"operation", effect.Operation},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"run_id", effect.RunID},
		{"origin_action_id", effect.OriginActionID},
		{"idempotency_key", effect.IdempotencyKey},
	} {
		if item.value != "" {
			if err := validateIdentifier(resource, item.field, item.value); err != nil {
				return err
			}
		}
	}
	if effect.Sequence < 1 {
		return newError(ErrorEffectSequence, "validate", resource, "sequence", "effect sequence must start at 1", nil)
	}
	if err := validateResourceSelector(resource, "resource", effect.Resource); err != nil {
		return err
	}
	if err := validateJSONObject(resource, "arguments", effect.Arguments); err != nil {
		return err
	}
	if err := validateDigest(resource, "arguments_digest", effect.ArgumentsDigest); err != nil {
		return err
	}
	if err := validateUniqueIdentifiers(resource, "dependencies", effect.Dependencies); err != nil {
		return err
	}
	for _, dependency := range effect.Dependencies {
		if dependency == effect.ID {
			return invalid(resource, "dependencies", "an effect cannot depend on itself")
		}
	}
	for _, item := range []struct {
		field string
		value *int64
	}{
		{"estimated_scope.rows", effect.EstimatedScope.Rows},
		{"estimated_scope.bytes", effect.EstimatedScope.Bytes},
		{"estimated_scope.objects", effect.EstimatedScope.Objects},
	} {
		if err := validateOptionalNonnegative(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if !validRecoveryClass(effect.RecoveryClass) {
		return invalid(resource, "recovery_class", "unknown effect recovery class")
	}
	if !validEffectStatus(effect.Status) {
		return invalid(resource, "status", "unknown effect status")
	}
	for _, item := range []struct {
		field string
		value *ArtifactRef
	}{
		{"stage_ref", effect.StageRef},
		{"before_state_ref", effect.BeforeStateRef},
	} {
		if item.value != nil {
			if err := validateArtifactRef(resource, item.field, *item.value); err != nil {
				return err
			}
		}
	}
	if effectStatusRequiresStage(effect.Status) && effect.RecoveryClass != RecoveryReadOnly && effect.StageRef == nil {
		return invalid(resource, "stage_ref", "staged and releasable effects require a staged artifact")
	}
	if effectStatusRequiresReleaseMetadata(effect.Status) && effect.RecoveryClass != RecoveryReadOnly && effect.IdempotencyKey == "" {
		return invalid(resource, "idempotency_key", "releasable effects require an idempotency key")
	}
	if effect.RecoveryClass == RecoveryReversible && effectStatusRequiresReleaseMetadata(effect.Status) && effect.BeforeStateRef == nil {
		return invalid(resource, "before_state_ref", "reversible effects require captured before-state before release")
	}
	if effect.Compensation != nil {
		if effect.RecoveryClass != RecoveryCompensatable {
			return invalid(resource, "compensation", "only compensatable effects accept a semantic compensation plan")
		}
		if err := validateIdentifier(resource, "compensation.operation", effect.Compensation.Operation); err != nil {
			return err
		}
		if err := validateJSONObject(resource, "compensation.arguments", effect.Compensation.Arguments); err != nil {
			return err
		}
		if err := validateDigest(resource, "compensation.arguments_digest", effect.Compensation.ArgumentsDigest); err != nil {
			return err
		}
		if effect.Compensation.PlanRef != nil {
			if err := validateArtifactRef(resource, "compensation.plan_ref", *effect.Compensation.PlanRef); err != nil {
				return err
			}
		}
	} else if effect.RecoveryClass == RecoveryCompensatable && effectStatusRequiresReleaseMetadata(effect.Status) {
		return invalid(resource, "compensation", "compensatable effects require a plan before release")
	}
	if effect.RecoveryClass == RecoveryIrreversible && effect.Compensation != nil {
		return invalid(resource, "compensation", "irreversible effects cannot claim compensation")
	}
	if effect.Receipt != nil {
		if err := validateEffectReceipt(resource, "receipt", *effect.Receipt); err != nil {
			return err
		}
	}
	if effect.CompensationReceipt != nil {
		if err := validateEffectReceipt(resource, "compensation_receipt", *effect.CompensationReceipt); err != nil {
			return err
		}
	}
	if effectStatusRequiresReceipt(effect.Status) && effect.Receipt == nil {
		return invalid(resource, "receipt", "committed or compensating effects require a commit receipt")
	}
	if effect.Status == EffectCompensated && effect.CompensationReceipt == nil {
		return invalid(resource, "compensation_receipt", "compensated effects require a compensation receipt")
	}
	if effect.CreatedAt.IsZero() || effect.UpdatedAt.IsZero() {
		return invalid(resource, "created_at", "created_at and updated_at are required")
	}
	if effect.UpdatedAt.Before(effect.CreatedAt) {
		return invalid(resource, "updated_at", "updated_at cannot precede created_at")
	}
	return nil
}

func (result VerificationResult) Validate() error {
	const resource = "VerificationResult"
	if err := validateVersion(resource, result.Version, VerificationResultVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", result.ID},
		{"transaction_id", result.TransactionID},
		{"name", result.Name},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if !validVerificationKind(result.Kind) {
		return invalid(resource, "kind", "unknown verification kind")
	}
	if !validVerificationStatus(result.Status) {
		return invalid(resource, "status", "unknown verification status")
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"effect_set_digest", result.EffectSetDigest},
		{"staged_state_digest", result.StagedStateDigest},
		{"verifier_digest", result.VerifierDigest},
		{"inputs_digest", result.InputsDigest},
	} {
		if err := validateDigest(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if err := validatePrincipal(resource, "verifier", result.Verifier); err != nil {
		return err
	}
	if !validVerificationIndependence(result.Independence) {
		return invalid(resource, "independence", "unknown verifier independence class")
	}
	if len(result.Evidence) == 0 {
		return invalid(resource, "evidence", "at least one evidence artifact is required")
	}
	for i, evidence := range result.Evidence {
		if err := validateArtifactRef(resource, fmt.Sprintf("evidence[%d]", i), evidence); err != nil {
			return err
		}
	}
	if strings.TrimSpace(result.Summary) == "" {
		return invalid(resource, "summary", "summary is required")
	}
	if result.EvaluatedAt.IsZero() {
		return invalid(resource, "evaluated_at", "evaluated_at is required")
	}
	if result.ExpiresAt != nil && !result.ExpiresAt.After(result.EvaluatedAt) {
		return invalid(resource, "expires_at", "expires_at must be after evaluated_at")
	}
	return nil
}

func (approval ApprovalPackage) Validate() error {
	const resource = "ApprovalPackage"
	if err := validateVersion(resource, approval.Version, ApprovalPackageVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", approval.ID},
		{"transaction_id", approval.TransactionID},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"intent_digest", approval.IntentDigest},
		{"effect_set_digest", approval.EffectSetDigest},
		{"staged_state_digest", approval.StagedStateDigest},
		{"policy_digest", approval.PolicyDigest},
		{"commit_plan_digest", approval.CommitPlanDigest},
		{"digest", approval.Digest},
	} {
		if err := validateDigest(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if len(approval.VerificationResultIDs) == 0 {
		return invalid(resource, "verification_result_ids", "at least one verification result is required")
	}
	if err := validateUniqueIdentifiers(resource, "verification_result_ids", approval.VerificationResultIDs); err != nil {
		return err
	}
	if err := validateUniqueIdentifiers(resource, "required_approval_classes", approval.RequiredApprovalClasses); err != nil {
		return err
	}
	for i, finding := range approval.RiskFindings {
		field := fmt.Sprintf("risk_findings[%d]", i)
		if err := validateIdentifier(resource, field+".code", finding.Code); err != nil {
			return err
		}
		if !validRiskSeverity(finding.Severity) {
			return invalid(resource, field+".severity", "unknown risk severity")
		}
		if strings.TrimSpace(finding.Summary) == "" {
			return invalid(resource, field+".summary", "summary is required")
		}
		if err := validateUniqueIdentifiers(resource, field+".effect_ids", finding.EffectIDs); err != nil {
			return err
		}
	}
	if strings.TrimSpace(approval.Summary) == "" {
		return invalid(resource, "summary", "summary is required")
	}
	if approval.CreatedAt.IsZero() || approval.ExpiresAt.IsZero() || !approval.ExpiresAt.After(approval.CreatedAt) {
		return invalid(resource, "expires_at", "approval expiry must be after creation")
	}
	return nil
}

func (decision ApprovalDecision) Validate() error {
	const resource = "ApprovalDecision"
	if err := validateVersion(resource, decision.Version, ApprovalDecisionVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", decision.ID},
		{"transaction_id", decision.TransactionID},
		{"approval_id", decision.ApprovalID},
		{"approval_class", decision.ApprovalClass},
		{"key_id", decision.KeyID},
		{"nonce", decision.Nonce},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"package_digest", decision.PackageDigest},
		{"digest", decision.Digest},
	} {
		if err := validateDigest(resource, item.field, item.value); err != nil {
			return err
		}
	}
	switch decision.Decision {
	case ApprovalApprove:
		if strings.TrimSpace(decision.Reason) != "" {
			return invalid(resource, "reason", "approval must not include a rejection or revision reason")
		}
	case ApprovalReject, ApprovalRevise:
		if strings.TrimSpace(decision.Reason) == "" {
			return invalid(resource, "reason", "rejection and revision decisions require a reason")
		}
	default:
		return invalid(resource, "decision", "decision must be approve, reject, or revise")
	}
	if err := validatePrincipal(resource, "approver", decision.Approver); err != nil {
		return err
	}
	if decision.Approver.Kind != PrincipalHuman {
		return identityInvalid(resource, "approver.kind", "transaction approval requires a human principal")
	}
	if decision.IssuedAt.IsZero() || decision.ExpiresAt.IsZero() ||
		!decision.ExpiresAt.After(decision.IssuedAt) {
		return invalid(resource, "expires_at", "approval decision expiry must be after issuance")
	}
	signature, err := base64.StdEncoding.DecodeString(decision.SignatureBase64)
	if err != nil || len(signature) != 64 {
		return invalid(resource, "signature_base64", "must be a base64-encoded Ed25519 signature")
	}
	return nil
}

func (plan CommitPlan) Validate() error {
	const resource = "CommitPlan"
	if err := validateVersion(resource, plan.Version, CommitPlanVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", plan.ID},
		{"transaction_id", plan.TransactionID},
		{"connector", plan.Connector},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if err := validateResourceSelector(resource, "target", plan.Target); err != nil {
		return err
	}
	if strings.TrimSpace(plan.ExpectedResourceVersion) == "" ||
		strings.ContainsRune(plan.ExpectedResourceVersion, 0) {
		return invalid(resource, "expected_resource_version", "expected resource version is required")
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"intent_digest", plan.IntentDigest},
		{"effect_set_digest", plan.EffectSetDigest},
		{"staged_state_digest", plan.StagedStateDigest},
		{"policy_digest", plan.PolicyDigest},
		{"digest", plan.Digest},
	} {
		if err := validateDigest(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if len(plan.Steps) == 0 {
		return invalid(resource, "steps", "at least one release step is required")
	}
	stepIndex := make(map[string]int, len(plan.Steps))
	stepDependencies := make(map[string][]string, len(plan.Steps))
	for i, step := range plan.Steps {
		field := fmt.Sprintf("steps[%d]", i)
		if step.Sequence != int64(i+1) {
			return newError(ErrorEffectSequence, "validate", resource, field+".sequence", "commit steps must be contiguous and start at 1", nil)
		}
		if err := validateIdentifier(resource, field+".effect_id", step.EffectID); err != nil {
			return err
		}
		if _, exists := stepIndex[step.EffectID]; exists {
			return invalid(resource, field+".effect_id", "an effect may appear only once in a commit plan")
		}
		if err := validateUniqueIdentifiers(resource, field+".depends_on", step.DependsOn); err != nil {
			return err
		}
		for _, dependency := range step.DependsOn {
			if _, exists := stepIndex[dependency]; !exists {
				return newError(ErrorEffectSequence, "validate", resource, field+".depends_on", "dependencies must be earlier commit steps", nil)
			}
		}
		if err := validateUniqueIdentifiers(resource, field+".precondition_ids", step.PreconditionIDs); err != nil {
			return err
		}
		if step.IdempotencyKey != "" {
			if err := validateIdentifier(resource, field+".idempotency_key", step.IdempotencyKey); err != nil {
				return err
			}
		}
		if step.TimeoutSeconds < 1 {
			return invalid(resource, field+".timeout_seconds", "timeout must be at least one second")
		}
		if !step.RequiresReceipt {
			return invalid(resource, field+".requires_receipt", "every release step must require a receipt")
		}
		stepIndex[step.EffectID] = i
		stepDependencies[step.EffectID] = step.DependsOn
	}
	compensationIndex := make(map[string]int, len(plan.CompensationSteps))
	for i, step := range plan.CompensationSteps {
		field := fmt.Sprintf("compensation_steps[%d]", i)
		if step.Sequence != int64(i+1) {
			return newError(ErrorEffectSequence, "validate", resource, field+".sequence", "compensation steps must be contiguous and start at 1", nil)
		}
		if _, exists := stepIndex[step.EffectID]; !exists {
			return invalid(resource, field+".effect_id", "compensation references an effect outside the commit plan")
		}
		if _, exists := compensationIndex[step.EffectID]; exists {
			return invalid(resource, field+".effect_id", "an effect may appear only once in a compensation plan")
		}
		if err := validateUniqueIdentifiers(resource, field+".precondition_ids", step.PreconditionIDs); err != nil {
			return err
		}
		if step.PlanRef != nil {
			if err := validateArtifactRef(resource, field+".plan_ref", *step.PlanRef); err != nil {
				return err
			}
		}
		compensationIndex[step.EffectID] = i
	}
	for effectID, dependencies := range stepDependencies {
		effectPosition, effectCompensated := compensationIndex[effectID]
		for _, dependency := range dependencies {
			dependencyPosition, dependencyCompensated := compensationIndex[dependency]
			if effectCompensated && dependencyCompensated && effectPosition >= dependencyPosition {
				return newError(ErrorEffectSequence, "validate", resource, "compensation_steps", "compensation must reverse commit dependency order", nil)
			}
		}
	}
	if err := validateUniqueIdentifiers(resource, "irreversible_effect_ids", plan.IrreversibleEffectIDs); err != nil {
		return err
	}
	for _, effectID := range plan.IrreversibleEffectIDs {
		if _, exists := stepIndex[effectID]; !exists {
			return invalid(resource, "irreversible_effect_ids", "irreversible effect is not present in commit steps")
		}
		if _, compensated := compensationIndex[effectID]; compensated {
			return invalid(resource, "compensation_steps", "an irreversible effect cannot have a compensation step")
		}
	}
	if plan.ManualRecoveryRef != nil {
		if err := validateArtifactRef(resource, "manual_recovery_ref", *plan.ManualRecoveryRef); err != nil {
			return err
		}
	} else if len(plan.IrreversibleEffectIDs) > 0 {
		return invalid(resource, "manual_recovery_ref", "plans with irreversible effects require a manual recovery playbook")
	}
	if plan.CreatedAt.IsZero() {
		return invalid(resource, "created_at", "created_at is required")
	}
	return nil
}

func (event TransactionEvent) Validate() error {
	const resource = "TransactionEvent"
	if err := validateVersion(resource, event.Version, TransactionEventVersion); err != nil {
		return err
	}
	for _, item := range []struct {
		field string
		value string
	}{
		{"id", event.ID},
		{"transaction_id", event.TransactionID},
		{"type", event.Type},
	} {
		if err := validateIdentifier(resource, item.field, item.value); err != nil {
			return err
		}
	}
	if event.Sequence < 1 {
		return newError(ErrorEventSequence, "validate", resource, "sequence", "event sequence must start at 1", nil)
	}
	if err := validatePrincipal(resource, "actor", event.Actor); err != nil {
		return err
	}
	if event.OccurredAt.IsZero() {
		return invalid(resource, "occurred_at", "occurred_at is required")
	}
	if err := validateJSONObject(resource, "payload", event.Payload); err != nil {
		return err
	}
	for i, ref := range event.Artifacts {
		if err := validateArtifactRef(resource, fmt.Sprintf("artifacts[%d]", i), ref); err != nil {
			return err
		}
	}
	if event.Sequence == 1 && event.PreviousDigest != "" {
		return newError(ErrorEventChain, "validate", resource, "previous_digest", "the first event cannot have a previous digest", nil)
	}
	if event.Sequence > 1 {
		if event.PreviousDigest == "" {
			return newError(ErrorEventChain, "validate", resource, "previous_digest", "events after sequence 1 require a previous digest", nil)
		}
		if err := validateDigest(resource, "previous_digest", event.PreviousDigest); err != nil {
			return err
		}
	}
	return validateDigest(resource, "digest", event.Digest)
}

func validateEffectReceipt(resource, field string, receipt EffectReceipt) error {
	if err := validateIdentifier(resource, field+".driver", receipt.Driver); err != nil {
		return err
	}
	if err := validateIdentifier(resource, field+".operation_id", receipt.OperationID); err != nil {
		return err
	}
	if err := validateDigest(resource, field+".result_digest", receipt.ResultDigest); err != nil {
		return err
	}
	if strings.TrimSpace(receipt.ResourceVersion) == "" && receipt.ResourceVersion != "" {
		return invalid(resource, field+".resource_version", "resource version cannot be blank")
	}
	if receipt.CommittedAt.IsZero() {
		return invalid(resource, field+".committed_at", "committed_at is required")
	}
	return nil
}

func validTransactionState(state TransactionState) bool {
	switch state {
	case TransactionCreated, TransactionRunning, TransactionStaged,
		TransactionValidating, TransactionValidationFailed,
		TransactionReviseRequired, TransactionBlocked,
		TransactionPendingApproval, TransactionReadyToCommit,
		TransactionCommitting, TransactionCommitted,
		TransactionCompensating, TransactionRolledBack,
		TransactionReleaseFailed,
		TransactionPartiallyCommitted, TransactionManualRecoveryRequired,
		TransactionAborted:
		return true
	default:
		return false
	}
}

func validAgentExecutionStatus(status AgentExecutionStatus) bool {
	switch status {
	case AgentExecutionRunning, AgentExecutionSucceeded, AgentExecutionFailed,
		AgentExecutionInterrupted, AgentExecutionStartFailed:
		return true
	default:
		return false
	}
}

func transactionStateRequiresReason(state TransactionState) bool {
	switch state {
	case TransactionValidationFailed, TransactionReviseRequired,
		TransactionBlocked, TransactionReleaseFailed, TransactionPartiallyCommitted,
		TransactionManualRecoveryRequired, TransactionAborted:
		return true
	default:
		return false
	}
}

func transactionStateRequiresFrozenEffects(state TransactionState) bool {
	switch state {
	case TransactionStaged, TransactionValidating, TransactionValidationFailed,
		TransactionReviseRequired, TransactionPendingApproval,
		TransactionReadyToCommit, TransactionCommitting, TransactionCommitted,
		TransactionCompensating, TransactionRolledBack,
		TransactionReleaseFailed,
		TransactionPartiallyCommitted, TransactionManualRecoveryRequired:
		return true
	default:
		return false
	}
}

func transactionStateRequiresCommitPlan(state TransactionState) bool {
	switch state {
	case TransactionReadyToCommit, TransactionCommitting,
		TransactionCommitted, TransactionCompensating,
		TransactionRolledBack, TransactionReleaseFailed, TransactionPartiallyCommitted,
		TransactionManualRecoveryRequired:
		return true
	default:
		return false
	}
}

func validRecoveryClass(class EffectRecoveryClass) bool {
	switch class {
	case RecoveryReadOnly, RecoveryStageable, RecoveryReversible,
		RecoveryCompensatable, RecoveryIrreversible:
		return true
	default:
		return false
	}
}

func validEffectStatus(status EffectStatus) bool {
	switch status {
	case EffectProposed, EffectStaged, EffectValidated, EffectReleaseReady,
		EffectCommitting, EffectCommitted, EffectBlocked, EffectRejected,
		EffectFailed, EffectUnknown, EffectCompensating, EffectCompensated,
		EffectManualRecoveryRequired:
		return true
	default:
		return false
	}
}

func effectStatusRequiresStage(status EffectStatus) bool {
	switch status {
	case EffectStaged, EffectValidated, EffectReleaseReady, EffectCommitting,
		EffectCommitted, EffectCompensating, EffectCompensated:
		return true
	default:
		return false
	}
}

func effectStatusRequiresReleaseMetadata(status EffectStatus) bool {
	switch status {
	case EffectReleaseReady, EffectCommitting, EffectCommitted,
		EffectCompensating, EffectCompensated:
		return true
	default:
		return false
	}
}

func effectStatusRequiresReceipt(status EffectStatus) bool {
	switch status {
	case EffectCommitted, EffectCompensating, EffectCompensated:
		return true
	default:
		return false
	}
}

func validVerificationKind(kind VerificationKind) bool {
	switch kind {
	case VerificationPrecondition, VerificationInvariant, VerificationPostcondition:
		return true
	default:
		return false
	}
}

func validVerificationStatus(status VerificationStatus) bool {
	switch status {
	case VerificationPassed, VerificationFailed, VerificationIndeterminate:
		return true
	default:
		return false
	}
}

func validVerificationIndependence(value VerificationIndependence) bool {
	switch value {
	case VerificationAgentSupplied, VerificationPlatformRun,
		VerificationExternalAttestation:
		return true
	default:
		return false
	}
}

func validRiskSeverity(value string) bool {
	switch value {
	case "info", "low", "medium", "high", "critical":
		return true
	default:
		return false
	}
}
